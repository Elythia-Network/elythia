package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/activitypub/mfm"
	"github.com/elythia-network/elythia/internal/core/fedrule"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/misc/hashtag"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// ingestNoteWithCreated is the body of IngestNoteWithCreated carrying the
// current note/quote-chain recursion depth (#1828)。
func (r *Resolver) ingestNoteWithCreated(body []byte, deliveringActorURI string, depth int, ephemeral bool, chain *resolveChain) (*model.Note, bool, error) {
	if r.noteRepo == nil {
		return nil, false, ErrInvalidNote
	}
	var apNote activitypub.Note
	if err := json.Unmarshal(body, &apNote); err != nil {
		return nil, false, ErrInvalidNote
	}
	// actor と同じ理由で URL 系を正規化する。`note.uri` にそのまま入るので、
	// 生値だと後続の `FindByURI` / host 検証 / 配送先解決が全部ずれる (#2662)。
	apNote.ID = trimWHATWGURL(apNote.ID)
	apNote.AttributedTo = activitypub.APLenientID(trimWHATWGURL(apNote.AttributedTo.String()))
	if apNote.ID == "" || apNote.AttributedTo == "" {
		return nil, false, ErrInvalidNote
	}
	// `note.uri` は varchar(512)。**身元そのものなので切らない** — 切ると別の
	// note を指す URI になり、dedup (`FindByURI`) の鍵も壊れる。収まらなければ
	// document ごと拒否する (#2723)。NUL も同じ扱い (22021 で落ちる)。
	//
	// **DB を引く前に見る。** NUL 入りの値は `FindByURI` の SELECT 自体が落ちる。
	//
	// **この error が上でどう扱われるかは呼び出し元で決まる。** 一般化しないこと
	// (「permanent 分類だから常に ack」「常に dead」はどちらも誤り。#2723 では 5 周に
	// わたってこの記述を間違えた)。実際に起きる結末は少なくとも 4 種類ある:
	//
	//   - **ack して drop** — `isPermanentSkipError` を通すハンドラ (Like / Announce /
	//     Undo(Like) / Undo(Announce)) が対象 note を解決して踏んだ場合
	//   - **inbox job が retry を使い切って dead** — `handleCreate` の object や
	//     `handleAdd` のピン留め対象のように、error をそのまま返すハンドラ
	//   - **その値だけ黙って落ちて activity は成功** — 引用先 (`resolveQuoteTarget`) や
	//     featured のピン (`resolveNoteBestEffort`) のように best-effort な解決
	//   - **inbox job ですらない** — REST の `ap/show` は取り込みに失敗すると生の AP
	//     JSON を 200 で返す
	//   - **上のどれであっても、Collection に包まれて配送されたら ack** —
	//     `handleCollection` が item の error をログに出して握る
	//
	// 数え直すなら `ResolveNote` / `IngestNote*` の呼び出し元を全部辿ること。**ただし
	// 最後の 1 つは呼び出し元一覧には出ない** — Collection 化は `dispatchActivity` 側の
	// 性質なので、`handleCollection` を別途見ること。**このリストを他所にコピー
	// しないこと** — 4 箇所に散らした結果、毎回どこかがずれた。
	//
	// いずれにせよこの document は保存できないので結末としては正しい。gate の利得は
	// 原因が 22001 ではなく明示的な拒否として残ること。
	//
	// **ephemeral (リレー) 経路にも効かせる。** Redis に置く分には列幅は無関係だが、
	// ephemeral note は後で materialize されうる (`ephemeral.Materializer.EnsureNote`
	// → `NoteWriter.Create`) ので、そのとき同じ 22001 で落ちる。先に弾くほうが
	// 原因に近い。
	if !fitsColumn(apNote.ID, noteURIMaxRunes) {
		slog.Warn("federation: rejecting note whose id does not fit note.uri",
			"id", truncateRunes(apNote.ID, noteURIMaxRunes))
		return nil, false, ErrInvalidNote
	}
	switch existing, err := r.noteRepo.FindByURI(apNote.ID); {
	case err == nil:
		return existing, false, nil
	case !repository.IsNotFound(err):
		// **障害を「まだ無い」に倒さない** (#3121)。倒すと再 fetch して INSERT へ
		// 進む。救っているのは `note.uri` の UNIQUE 制約と下の fallback だけで、
		// その手前でリモートへの再取得が走る。retry させるほうが安い。
		return nil, false, fmt.Errorf("ingest note: dedup lookup: %w", err)
	}
	// ephemeral 経路では DB の miss が「未取り込み」を意味しないので URI 逆引きも
	// 引く (#2397)。非 ephemeral では引かない: 直接配送で DB 行を作る側は
	// dropSupersededEphemeral で ephemeral 側を落として上書きするのが正しい。
	if ephemeral {
		if existing := r.ephemeralNoteByURI(apNote.ID); existing != nil {
			return existing, false, nil
		}
	}
	// upstream ApNoteService.validateNote 相当の attribution 検証 (#1839):
	//   1. note の id host と著者 (attributedTo) host が一致すること
	//      (別 host の著者を詐称する cross-host forge を弾く)。
	//   2. inbound Create では note の著者が配送 actor 本人であること
	//      (alice@a が bob@b になりすました note を作る forge を弾く)。
	idHost, idErr := hostFromURI(apNote.ID)
	attrHost, attrErr := hostFromURI(apNote.AttributedTo.String())
	// idna 正規化して Unicode/punycode mixed-form の同一 host を誤 reject しない (#1850)。
	if idErr != nil || attrErr != nil || punyHost(idHost) != punyHost(attrHost) {
		slog.Warn("federation: note id/attributedTo host mismatch", "id", apNote.ID, "attributedTo", apNote.AttributedTo)
		return nil, false, ErrNoteAttributionMismatch
	}
	if deliveringActorURI != "" && apNote.AttributedTo.String() != deliveringActorURI {
		slog.Warn("federation: note attribution does not match delivering actor", "attributedTo", apNote.AttributedTo, "actor", deliveringActorURI)
		return nil, false, ErrNoteAttributionMismatch
	}
	// quote cycle 遮断と featured の自己参照遮断は resolveChain が担う (#2685)。
	// ここで chain へ足しておくと、**inbox 直送 (resolveNoteOnce を通らない経路)
	// でも立つ** (#2686)。resolveNoteOnce から来た場合は既に入っているので
	// 二重には増えない。
	chain = chain.with(apNote.ID, apNote.ID)
	// federation policy gate: 直接 handleCreate から受け取った body や、
	// 中継経由で attributedTo が allowlist 外の host を指している payload を
	// DB 永続化させない。既存 row hit (上の FindByURI) は素通しで legacy
	// 互換を維持する。
	if !r.hostAllowedForURI(apNote.AttributedTo.String()) {
		return nil, false, ErrHostNotAllowed
	}
	actor, err := r.resolveNoteAuthor(apNote.AttributedTo.String(), ephemeral, depth, chain)
	if err != nil {
		return nil, false, err
	}
	// 遅延配送 / origin downtime 後の bulk push などで note が遅れて届くケース
	// では AP の `published` を採用しないと timeline 上の並びが受信時刻順に
	// なってしまい remote とずれる (#940)。time-based ID (aidx 等) なので
	// idGen.Generate に published を渡せば自動的に tl 並びも remote 一致する。
	now := parseAPPublishedTime(apNote.Published.String(), time.Now())
	noteURI := apNote.ID
	note := &model.Note{
		ID:         r.idGen.Generate(now),
		UserID:     actor.ID,
		UserHost:   actor.Host,
		URI:        &noteURI,
		Visibility: deriveVisibility(actor, apNote.To, apNote.CC),
	}
	// HTML 版の permalink。Mastodon 系では `id` (AP object) と `url` (Web ページ)
	// が別なので、保存しないとクライアントが原文ページへ辿れない (#2729)。
	if u := remoteNoteURL(noteURI, apNote.URL.String()); u != "" {
		note.URL = &u
	}
	// #2106 N14: silenced instance (meta.silencedHosts) の remote public note は home に
	// 降格する (upstream NoteCreateService.ts:491-495 の inSilencedInstance 降格)。public
	// timeline / 連合 broadcast から外す admin moderation 機能。silencedChecker 未配線時は
	// 降格しない (legacy 挙動)。
	if note.Visibility == model.NoteVisibilityPublic && actor.Host != nil && *actor.Host != "" &&
		r.silencedChecker != nil && r.silencedChecker.IsSilenced(*actor.Host) {
		note.Visibility = model.NoteVisibilityHome
	}
	// TS本家ApNoteService.tsと同じ3段フォールバックでtext抽出:
	// 1. source.content (MFM) → 2. _misskey_content → 3. content (HTML→MFM変換)
	var fullText *string
	if apNote.Source != nil &&
		apNote.Source.MediaType == "text/x.misskeymarkdown" &&
		apNote.Source.Content != "" {
		// 長さは本家と同じく 8192 (UTF-16 の単位) で切る (clipRemoteNoteText)。
		// **NUL も落とす**。
		// `content` 経路は mfm.FromHTML が落とすが、source / _misskey_content は
		// FromHTML を通らない (#2723)。
		// **空になったら nil のままにする** (NUL だけの content が来ると空になる)。
		// `content` 経路が `if text != ""` で nil を保つので、揃えないと REST の
		// `text` が同じ入力で `null` と `""` に分かれる。
		if text := remoteText(apNote.Source.Content, 0); text != "" {
			fullText = &text
		}
	} else if apNote.MisskeyContent != "" {
		if text := remoteText(apNote.MisskeyContent.String(), 0); text != "" {
			fullText = &text
		}
	} else if apNote.Content != "" {
		text := mfm.FromHTML(apNote.Content)
		if text != "" {
			fullText = &text
		}
	}
	// 保存するのは切った本文、禁止語・連合のルール・本文からの hashtag の判定は
	// 切る前の全文で行う (本家 ApNoteService は checkProhibitedWordsContain を
	// 全文で見てから NoteCreateService.create で切る)。
	// 切った後に前後の空白を削り、空なら本文無しにする (本家と同じ、#3369)。
	if fullText != nil {
		if stored := storedRemoteNoteText(*fullText); stored != "" {
			note.Text = &stored
		}
	}
	if apNote.Summary != "" {
		// **列の上限で切り、NUL を落とす。** CW は**相手が自由に決められる値**で、
		// 長さの制限は送信側の実装次第 (upstream Misskey 自身は投稿時に 100 で
		// 弾くが、AP でそれを強制する仕組みは無い)。溢れると Create ごと落ちて
		// ingest が error を返す。生の DB error なので `isPermanentSkipError` に
		// 当たらず、**トップレベル配送ならどのハンドラでも inbox job が dead になる**
		// (既定 8 回) (#2723)。
		// **空になったら CW を付けない。** NUL だけの summary をそのまま入れると
		// `cw = ""` になり、Misskey は「ラベル無しの CW」として本文を折りたたむ。
		// `text` 側と同じ書き分け (#2723)。
		if summary := remoteText(apNote.Summary.String(), noteCWMaxRunes); summary != "" {
			note.CW = &summary
		}
	}
	if apNote.Sensitive && note.CW == nil {
		// Sensitive かつ Summary が無いケース: 空文字 CW を設定して NSFW を表現
		empty := ""
		note.CW = &empty
	}
	// 禁止語 (meta.prohibitedWords) は inbound にも掛ける。upstream の
	// ApNoteService.ts:203 が `checkProhibitedWordsContain({cw, text, pollChoices})`
	// を**添付とユーザーを登録する前**に呼ぶのと同じ位置づけで、ここより後ろの
	// `upsertEmojis` / `upsertAttachments` / `noteRepo.Create` はどれも行を作る。
	//
	// **error ではなく (nil, false, nil) で落とす。** upstream は
	// IdentifiableError('689ee33f-…') を投げるが、InboxProcessorService がそれを
	// catch して `'blocked notes with prohibited words'` を返す = **ack して
	// retry しない**。mk-go の handleCreate は未知の error をそのまま retry キューへ
	// 戻すので、error にすると同じ note が 8 回配送し直されて dead letter に積まれる。
	// note を作らずに抜ける形は AP vote 経路が既に使っており、呼び出し側は
	// nil note を扱える (ResolveNoteWithCreated だけは ErrInvalidNote に倒す)。
	if r.containsProhibitedWords(fullText, note.CW, apPollChoices(&apNote)) {
		slog.Info("federation: dropping inbound note containing prohibited words",
			"uri", truncateRunes(apNote.ID, noteURIMaxRunes), "actor", actor.ID)
		return nil, false, nil
	}
	// センシティブワード (meta.sensitiveWords) に当たる public の投稿は home にする。
	// 本家はリモートの投稿も NoteCreateService.create を通すので、ローカル投稿と
	// 同じ降格が掛かる。判定は禁止語と同じく切る前の全文で行う (本家も切る前に
	// 判定する)。編集 (Update) の取り込みでも同じ判定で降格する (UpdateRemoteNote)。
	// 当たらなければ、本家と同じく投稿者のロールの canPublicNote でも降格する (#3369)。
	if note.Visibility == model.NoteVisibilityPublic &&
		(r.containsSensitiveWords(fullText, note.CW) || r.authorCannotPublicNote(actor.ID)) {
		note.Visibility = model.NoteVisibilityHome
	}
	// 連合のルール (#3090)。禁止語と同じく**行を作る前**に評価する。ここより後ろは
	// 返信先・引用先の取得や添付の登録で行を書くので、拒否するものはここで落とす。
	// 取り込みの全経路 (inbox の Create、Announce 先・返信先・引用先の取得、
	// リレー) がこの関数を通るので、ブースト経由で入ってくる投稿にも効く。
	var ruleDecision fedrule.Decision
	if r.rules != nil && r.rules.HasNoteRules() {
		hasAttachment := len(extractAttachments(apNote.Attachment, apNote.Sensitive.Bool())) > 0
		in := noteRuleFacts(actor, fullText, note.CW, &apNote, hasAttachment)
		in.Actor = r.rules.ActorFacts(actor)
		ruleDecision = r.rules.EvaluateNote(in)
		if ruleDecision.Reject {
			slog.Info("federation: dropping inbound note rejected by a federation rule",
				"uri", truncateRunes(apNote.ID, noteURIMaxRunes), "actor", actor.ID)
			return nil, false, nil
		}
		// silence と同じ形でタイムラインから外す (public → home)。
		if ruleDecision.Unlist && note.Visibility == model.NoteVisibilityPublic {
			note.Visibility = model.NoteVisibilityHome
		}
	}
	// 返信先がローカルに存在すれば紐付ける。リモート返信先の解決は後続 phase で
	// 対応するため、現状では nil のままにする。
	var replyTarget *model.Note
	if apNote.InReplyTo != "" {
		// inReplyTo も同じ (#2662)。生値だと返信チェーンが繋がらない。
		apNote.InReplyTo = activitypub.APLenientID(trimWHATWGURL(apNote.InReplyTo.String()))
		// **障害を「返信先が無い」に倒さない** (#3121)。倒すと `ReplyID = nil` の
		// まま note を保存してしまい、**URI は DB に入るので retry は dedup に
		// ヒットする** = スレッド接続が恒久的に失われる。not-found (こちらが
		// 取り込んでいない返信先) は従来どおり nil のまま進む。
		var replyErr error
		localReplyID := r.extractLocalNoteID(apNote.InReplyTo.String())
		if localReplyID != "" {
			reply, err := r.noteRepo.FindByID(localReplyID)
			replyTarget, replyErr = reply, err
		} else {
			reply, err := r.noteRepo.FindByURI(apNote.InReplyTo.String())
			replyTarget, replyErr = reply, err
		}
		if replyErr != nil {
			if !repository.IsNotFound(replyErr) {
				if localReplyID != "" && chain != nil && chain.inboundLocalReferences {
					return nil, false, fmt.Errorf("%w: ingest note: lookup local reply target: %w", ErrInboundLocalReferenceFailure, replyErr)
				}
				return nil, false, fmt.Errorf("ingest note: lookup reply target: %w", replyErr)
			}
			replyTarget = nil
		}
		if replyTarget != nil {
			if chain != nil && chain.inboundLocalReferences {
				if err := r.RequireInboundLocalNote(replyTarget, chain.inboundSourceHost); err != nil {
					return nil, false, fmt.Errorf("ingest note: validate reply target: %w", err)
				}
			}
			note.ReplyID = &replyTarget.ID
			note.ReplyUserID = &replyTarget.UserID
			note.ReplyUserHost = replyTarget.UserHost
			// thread-muting が深い thread でも効くよう threadId を thread root に
			// 揃える (local create path #1928 と同 logic、upstream NoteCreateService:
			// 623-627: threadId = data.reply.threadId ?? data.reply.id)。AP 経由で
			// 取り込んだ remote reply にも適用しないと remote thread の deep muting が
			// 効かない (#1931)。
			threadID := replyTarget.ID
			if replyTarget.ThreadID != nil && *replyTarget.ThreadID != "" {
				threadID = *replyTarget.ThreadID
			}
			note.ThreadID = &threadID
			// reply 先より広い可視性にはできない (upstream 2026.7.0 #17747)。
			// upstream の ApNoteService は NoteCreateService.create を経由する
			// ため、リモート発の reply にも同じクランプが掛かる。これが無いと
			// 「ローカルの followers 限定 note へリモートが to:[Public] で返信」
			// でローカル TL / 再配送に本文文脈が漏れる。
			note.Visibility = corenote.ClampVisibilityForReply(replyTarget.Visibility, note.Visibility)
		}
	}
	// AP vote 判定: reply target が poll を持ち apNote.Name (choice 名) が
	// 入っていれば「投票」として処理し、note は作らずに早期 return する
	// (#690)。Misskey TS の ApNoteService.create と同等の wire format。
	// pollRepo / pollVoter 未配線環境では従来通り reply note として fall
	// through する (legacy 互換)。
	if replyTarget != nil && replyTarget.HasPoll && apNote.Name != "" && r.pollRepo != nil && r.pollVoter != nil {
		poll, perr := r.pollRepo.FindByNoteID(replyTarget.ID)
		if perr != nil && !repository.IsNotFound(perr) {
			// **障害を「poll ではない」に倒さない** (#3121)。倒すと投票が普通の
			// 返信ノートとして確定し、dedup があるので retry でも直らない。
			return nil, false, fmt.Errorf("ingest note: lookup poll: %w", perr)
		}
		if perr == nil && poll != nil {
			// 保存側と同じ正規化を通して照合する。切った選択肢に対する投票は
			// 生値では一致しない (#2726)。
			want := remotePollChoice(apNote.Name.String())
			idx := -1
			for i, c := range poll.Choices {
				if c == want {
					idx = i
					break
				}
			}
			if idx >= 0 {
				if err := r.pollVoter.Vote(actor, replyTarget.ID, idx); err != nil {
					slog.Warn("federation: AP poll vote rejected",
						"actor", actor.ID, "noteId", replyTarget.ID, "choice", apNote.Name, "err", err)
				}
				// vote として処理した場合、reply note は作成せず終了
				// (note=nil で caller の fanout / notification も skip される)。
				return nil, false, nil
			}
			slog.Warn("federation: AP poll vote choice not found",
				"actor", actor.ID, "noteId", replyTarget.ID, "choice", apNote.Name)
		}
	}
	// メンションは AP `tag` 配列の Mention だけから取り、user ID へ解決して保存する
	// (mentions 列はローカル create 経路と同じ user ID の配列にする)。
	//
	// 本家 ApNoteService.createNote は apMentions (= tag の Mention) を
	// NoteCreateService に渡し、本文からはメンションを取らない。以前は本文の
	// `@user` も解決して tag の分と合わせていたが、それだと tag に無い
	// `@alice@<自ホスト>` を本文に書くだけで、alice に通知が届き、フォロワー限定の
	// 投稿も見えるようになっていた (#3330)。
	tagHrefs := extractMentionTags(apNote.Tag)
	var audience []string
	if note.Visibility == model.NoteVisibilitySpecified {
		audience = specifiedAudience(actor, apNote.To, apNote.CC)
	}
	// DB に無いリモートの actor は先に取りに行く (本家 ApMentionService.
	// extractApMentions / ApAudienceService.parseAudience の resolvePerson)。
	// 取れたものは下の DB 照合で引ける。範囲と上限は fetchUnknownMentionActors。
	// 投稿者の mentionLimit は、取得の上限と下の上限判定で同じ値を 1 回だけ引く。
	authorMentionLimit := r.lazyMentionLimit(note.UserID)
	if !ephemeral && depth == 0 {
		r.fetchUnknownMentionActors(authorMentionLimit, tagHrefs, audience, chain)
	}
	tagMentions, merr := r.resolveMentionedUserIDs(tagHrefs, chain)
	if merr != nil {
		return nil, false, fmt.Errorf("ingest note: %w", merr)
	}
	note.Mentions = mergeMentionIDs(nil, tagMentions)
	// specified visibility では AP `to` / `cc` の宛先 actor URI 列を ID へ解決して
	// 埋める。CanView の VisibleUserIDs チェック (core/note/visibility.go) で受信者が
	// note を参照できるようにするため (#397)。
	//
	// 本家 ApAudienceService.parseAudience は `to` と `cc` の両方から public と
	// 投稿者の followers を除いた残りを宛先にする。以前は `to` だけを見ていたので、
	// `cc` に置かれた宛先が DM を読めなかった (#3330)。
	if note.Visibility == model.NoteVisibilitySpecified {
		visible, verr := r.resolveMentionedUserIDs(audience, chain)
		if verr != nil {
			return nil, false, fmt.Errorf("ingest note: %w", verr)
		}
		// reply target を必ず含める (upstream NoteCreateService.ts:603-605、
		// ローカル create 経路の #2106 N13 と同型)。特に #17747 の clamp で
		// `to:[Public]` の reply を specified へ降格させた場合、apNote.To から
		// は宛先が 1 件も解決できず VisibleUserIDs が空になり、reply 先の当人
		// (= specified note の作者) すら本文を参照できなくなる。
		if replyTarget != nil && replyTarget.UserID != note.UserID {
			alreadyVisible := false
			for _, id := range visible {
				if id == replyTarget.UserID {
					alreadyVisible = true
					break
				}
			}
			if !alreadyVisible {
				visible = append(visible, replyTarget.UserID)
			}
		}
		note.VisibleUserIDs = model.StringArray(visible)
	}
	// 返信先の作者と specified の宛先も mentions に入れる (本家
	// NoteCreateService.create が mentionedUsers に足してから insert.mentions に
	// 書く。ローカルの作成経路と同じ形)。入れないと、Mention tag の無い DM の宛先に
	// mention 通知が届かず、notes/mentions にも出ない (#3330)。
	note.Mentions = withImplicitMentions(note, note.Mentions)
	// upstream Misskey #17167 (= 2026.5.0 fix / triage #1004): mentionLimit を
	// 超える note は無効と扱い、保存せずに ErrContainsTooManyMentions を返す。
	// caller (processor.handleCreate) が当該 sentinel を catch して queue retry
	// 経路から除外することで、罠の inbox job が永続蓄積するのを防ぐ。
	// upstream #17576: 制限判定は「解決できたユーザー数」でなく、remote が宣言した
	// raw mention 数 (AP tag の Mention href ユニーク数) との max で行う。一部しか
	// 解決できなくても大量 mention をすり抜けさせない。limit 値は投稿者の role
	// policy の mentionLimit。本家は投稿者がリモートでも roleService.getUserPolicies
	// を引くので、base policy の変更や、リモートの利用者に割り当てた / 条件で当たる
	// ロールが効く (#3330。以前は既定値 20 固定だった)。
	//
	// 本家の「解決できたユーザー」(mentionedUsers) は mention に加えて、返信先の
	// 作者 (自分への返信を除く) と specified の宛先を含む (NoteCreateService.create)。
	// 宛先を数えないと、Mention tag を持たない DM で宛先を何人でも並べられる
	// (#3330)。そのため宛先の解決を判定より前に行う。
	if exceedsRemoteMentionLimit(note, note.Mentions, tagHrefs, authorMentionLimit) {
		return nil, false, corenote.ErrContainsTooManyMentions
	}
	// 引用先のpolicy確認を、絵文字や添付の行を書き込む前に済ませる。
	quoted, quoteErr := r.resolveQuoteTarget(apNote.MisskeyQuote.String(), apNote.QuoteURL.String(), depth, ephemeral, chain)
	if quoteErr != nil {
		return nil, false, fmt.Errorf("ingest note: validate quote target: %w", quoteErr)
	}
	if quoted != nil &&
		(quoted.Visibility == model.NoteVisibilityFollowers ||
			quoted.Visibility == model.NoteVisibilitySpecified) {
		quoted = nil
	}
	if quoted != nil && quoted.ID != note.ID {
		note.RenoteID = &quoted.ID
		note.RenoteUserID = &quoted.UserID
		note.RenoteUserHost = quoted.UserHost
	}
	// AP Note Tag配列からカスタム絵文字を抽出してDBにupsert
	if actor.Host != nil {
		note.Emojis = r.noteEmojisFor(r.upsertEmojis(extractEmojiTags(apNote.Tag), *actor.Host), actor.Host)
	}
	// hashtag は AP `tag` 配列の Hashtag entry と本文 / CW の両方から拾い、
	// hashtag.ExtractNoteTags で case-insensitive dedup + 件数 cap + 長さ判定を
	// 一括処理する (#679)。長すぎる tag は truncate ではなく **drop** する。`tag` 配列が空 / Hashtag を含まない実装 (古い Mastodon
	// 等) でも本文 fallback で trends 集計に乗るようにする。
	hashtagSources := extractHashtagTagNames(apNote.Tag)
	if fullText != nil {
		hashtagSources = append(hashtagSources, *fullText)
	}
	if note.CW != nil {
		hashtagSources = append(hashtagSources, *note.CW)
	}
	// note.tags は upstream と同じく normalizeForSearch (NFKC+lowercase) + 128drop
	// + 32cap で正規化して格納する (#1948-18)。
	if tags := hashtag.ExtractNoteTags(hashtagSources...); len(tags) > 0 {
		note.Tags = model.StringArray(tags)
	}
	// ルールの CW は tag を抜いた**後**に付ける。前に付けると、管理者の書いた
	// 文言の hashtag がその投稿の tag として集計される。
	if cw := ruleCW(ruleDecision, note.CW); cw != nil {
		note.CW = cw
	}
	// AP `attachment` 配列を drive_file 行に upsert (#378)。link 形式のみで
	// 実 fetch はせず、frontend が drive_file.url 経由で remote 取得する。
	note.FileIDs = r.upsertAttachments(applyRulesToAttachments(ruleDecision,
		extractAttachments(apNote.Attachment, apNote.Sensitive.Bool())), &actor.ID, actor.Host)
	// ルールを先に当てる (後に回すと、メディアサイレンスで isSensitive だけ立った
	// 行を「済み」として飛ばし、ルールの maybeSensitive が付かない)。
	folded := ruleDecision.Sensitive && !r.markFilesSensitive(note.FileIDs, actor.ID)
	if !r.markMediaSilencedFiles(note.FileIDs, actor.ID, actor.Host) {
		folded = true
	}
	if folded {
		if cw := foldCW(note.CW); cw != nil {
			note.CW = cw
		}
	}
	if len(note.FileIDs) > 0 {
		// AttachedFileTypes は MIME type の配列 (TS との互換性)。
		note.AttachedFileTypes = r.collectAttachedFileTypes(note.FileIDs)
	}
	// Question（投票）の場合はhasPollフラグをCreate前に設定
	if len(apNote.OneOf) > 0 || len(apNote.AnyOf) > 0 {
		note.HasPoll = true
	}
	if ephemeral {
		// リレー由来の投稿は DB に入れず Redis へ置く (#2332)。ここまでの
		// 検証 (host binding / attribution / 可視性 / mention 上限) は
		// 通常経路と完全に同じものを通っている。
		if err := r.ephemeralSink.PutNote(context.Background(), note, actor); err != nil {
			return nil, false, err
		}
		// repliesCount / renoteCount の増分と poll 行の作成は行わない。前者は
		// 対象が DB に無い可能性があり、後者は poll.noteId が note への FK を
		// 持つため行を作れない。hashtag 集計も DB 書き込みなので行わない。
		//
		// chart は数える。以前 processor が呼んでいたときも ephemeral の投稿を
		// 数えていたので、その挙動を変えない。
		r.fireNoteCreated(note)
		return note, true, nil
	}
	if err := r.noteRepo.Create(note); err != nil {
		// dedup race: FindByURI (上) と Create の間に別の ingest が同 URI を先に
		// 作ると note.uri の UNIQUE 制約違反になる (#1527 review #2)。その場合は
		// 既存行を引いて dedup hit (created=false) として返し、重複 INSERT と
		// renoteCount / 各 hook の二重発火を防ぐ。FindByURI が引けない (= UNIQUE
		// 違反以外の真の失敗) なら元の err を返す。
		if existing, ferr := r.noteRepo.FindByURI(apNote.ID); ferr == nil && existing != nil {
			return existing, false, nil
		}
		return nil, false, err
	}
	// 本家はリモートの投稿も NoteCreateService を通すので、投稿者の notesCount を
	// 1 増やす (#3928)。ephemeral に入れた時点では数えず (投稿者の行が DB に無い
	// ことがある)、DB に入れたここで数える。失敗はベストエフォートで無視。
	r.incrementAuthorNotesCount(actor.ID)
	// ローカルノートへの返信の場合、repliesCount を増やす。
	// これにより timeline や API 上の「返信数」表示が federated reply も
	// 含むようになる。失敗はベストエフォートで無視。
	if note.ReplyID != nil {
		_ = r.noteRepo.IncrementCount(*note.ReplyID, "repliesCount", 1)
	}
	// 引用 renote は本家 NoteCreateService と同様、対象 note の renoteCount を増分する
	// (line 786: `data.renote && data.renote.userId !== user.id && !user.isBot`)。
	// 自分の note の自己引用と bot 投稿は除外する (#1527)。pure renote (Announce) は
	// handleAnnounce 側で別途 increment 済み。
	if note.RenoteID != nil && quoted != nil && quoted.UserID != actor.ID && !actor.IsBot {
		_ = r.noteRepo.IncrementCount(*note.RenoteID, "renoteCount", 1)
	}
	// Question (投票) の処理: oneOf/anyOf から Poll レコードを作成
	if r.pollRepo != nil && note.HasPoll {
		r.createPollFromQuestion(note, &apNote)
	}
	// 同じ投稿が先に relay 経由で ephemeral に入っていたら落とす。放置すると
	// timeline の合成で DB 行と ephemeral の両方が出て二重表示になる (#2332)。
	supersededEphemeral := r.dropSupersededEphemeral(apNote.ID, string(note.Visibility), actor)
	// hashtag table の mentionedUsersCount / userIds 更新 (#680 / #719)。
	// hook 実装 (core/hashtag.Service) が内部で goroutine を起こす
	// fire-and-forget 設計なので、IngestNote から見ると即時 return する。
	// inbox processor の drain time が tag 数に比例して伸びる退行を回避。
	if r.hashtagHook != nil && len(note.Tags) > 0 {
		r.hashtagHook.OnNoteCreated(note, actor)
	}
	// ephemeral に入っていた投稿は、そこで既に数えてある (#3330)。
	if !supersededEphemeral {
		r.fireNoteCreated(note)
	}
	return note, true, nil
}

// createPollFromQuestion creates a Poll record from an AP Question's oneOf/anyOf choices.
func (r *Resolver) createPollFromQuestion(note *model.Note, apNote *activitypub.Note) {
	choices := apNote.OneOf
	multiple := false
	if len(apNote.AnyOf) > 0 {
		choices = apNote.AnyOf
		multiple = true
	}
	choiceNames := make([]string, len(choices))
	votes := make([]int64, len(choices))
	for i, c := range choices {
		choiceNames[i] = remotePollChoice(c.Name)
		if c.Replies != nil {
			votes[i] = int64(c.Replies.TotalItems)
		}
	}
	poll := &model.Poll{
		NoteID:         note.ID,
		Multiple:       multiple,
		Choices:        choiceNames,
		Votes:          votes,
		NoteVisibility: note.Visibility,
		UserID:         note.UserID,
		UserHost:       note.UserHost,
	}
	// endTime/closedから有効期限を設定
	if apNote.EndTime != "" {
		if t, err := time.Parse(time.RFC3339, apNote.EndTime.String()); err == nil {
			poll.ExpiresAt = &t
		}
	} else if apNote.Closed != "" {
		if t, err := time.Parse(time.RFC3339, apNote.Closed.String()); err == nil {
			poll.ExpiresAt = &t
		}
	}
	// **エラーを捨てない。** 捨てると note が hasPoll=true のまま poll 行だけ
	// 無い状態になり、REST では選択肢の無い投票として出る。ここで error を上へ
	// 返しても直らない (note は既に Create 済みで、retry は FindByURI の dedup
	// hit で早期 return するため poll 作成へ再到達しない) ので、結末は
	// 「ログに残す」が正しい (#2726)。
	if err := r.pollRepo.Create(poll); err != nil {
		slog.Warn("federation: failed to persist remote poll",
			"noteId", note.ID, "uri", truncateRunes(apNote.ID, noteURIMaxRunes), "err", err)
	}
}

// remotePollChoice prepares an AP Question choice for `poll.choices`
// (`varchar(256)[]`).
//
// 選択肢は表示される本文なので切って NUL を落とす。1 つでも溢れると
// `poll` の INSERT ごと落ちて、**note は hasPoll=true のまま poll 行が無い**
// 状態になる (#2726)。
//
// **upstream は切らない** (`ApQuestionService.extractPollFromQuestion` は
// `x.name` をそのまま渡す)。upstream は poll を note と同じ transaction で
// 入れるので、同じ入力では note ごと落ちる。mk-go は note を残して選択肢を
// 切る = 安全側 (docs/divergence.md)。
func remotePollChoice(name string) string {
	return remoteText(name, pollChoiceMaxRunes)
}
