package federation

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/activitypub/mfm"
	"github.com/elythia-network/elythia/internal/core/fedrule"
	"github.com/elythia-network/elythia/internal/misc/hashtag"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// UpdateRemoteQuestion applies an inbound Update(Question) to an existing remote
// poll by refreshing its per-choice vote counts from the Question's oneOf/anyOf
// replies.totalItems (upstream ApQuestionService.updateQuestion、#1779)。
//
// 動作:
//   - object を Question として parse できなければ no-op
//   - URI で note を引いて見つからなければ no-op (反映先が無い)
//   - note の著者がローカルなら no-op (ローカル poll は remote Update で変更しない)
//   - poll が無ければ no-op
//   - Update activity の actor が poll 著者と一致しなければ拒否 (他 user の poll を
//     更新させない、upstream の attribution check 相当)
//   - 既存 choice 名で apChoice を照合し、新しい totalItems で votes を上書きする
//
// votes 以外 (choices / expiresAt) は更新しない (upstream も votes のみ更新)。
func (r *Resolver) UpdateRemoteQuestion(object json.RawMessage, actorURI string) error {
	if r.noteRepo == nil || r.pollRepo == nil {
		return nil
	}
	var apNote activitypub.Note
	if err := json.Unmarshal(object, &apNote); err != nil {
		return nil
	}
	// 書き込み側 (ingestNoteWithCreated) が正規化した値で保存しているので、
	// lookup も同じ正規化を通す (#2662)。
	apNote.ID = trimWHATWGURL(apNote.ID)
	if apNote.ID == "" {
		return nil
	}
	note, err := r.noteRepo.FindByURI(apNote.ID)
	if err != nil {
		// 取り込んでいない note の Update は何もしない。**障害は伝播させる**
		// (#3116) — ack すると job が retry されず、投票数が古いまま固定される。
		if repository.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("update remote question: lookup note: %w", err)
	}
	if note.UserHost == nil {
		// ローカル著者の poll は remote からの Update で書き換えない。
		return nil
	}
	poll, err := r.pollRepo.FindByNoteID(note.ID)
	if err != nil && !repository.IsNotFound(err) {
		return fmt.Errorf("update remote question: lookup poll: %w", err)
	}
	if err != nil || poll == nil {
		return nil
	}
	// attribution: Update の actor は poll 著者 (note author) と一致必須。別 user の
	// poll URI を指定した更新を拒否する。
	//
	// **一致を確認できなければ更新しない (#3037)。** 以前は
	// 「`FindByID` が成功し、`author.URI` が非 nil で、値が違うとき」だけ
	// 拒否していたので、**DB 障害 / 行の消失 / URI が NULL のどれでも検査が
	// 丸ごと消える** = fail-open。ここは「他人の poll を書き換えられるか」を
	// 決める唯一の検査なので、判定できないなら何もしない側に倒す。
	//
	// リモート著者は必ず URI を持つ (上で `note.UserHost == nil` を除いて
	// いる) ので、非 nil を要求しても正当な更新は落ちない。
	if actorURI != "" {
		if r.userRepo == nil {
			return nil
		}
		author, aerr := r.userRepo.FindByID(note.UserID)
		if aerr != nil {
			// **障害は黙って捨てない (#3037 レビュー)。** 「攻撃者による
			// 不一致」と「判定できなかった」を同じ無言の nil に潰すと、
			// 瞬断で落ちた更新が誰にも見えない (#2792 / #2725)。
			slog.Warn("federation: cannot verify poll update attribution",
				"id", apNote.ID, "actor", actorURI, "err", aerr)
			// **「適用しない」と「ack する」は分ける** (#3116)。判定できない以上
			// 適用しないのは正しい (fail-closed) が、ack すると job が retry
			// されず、**note / poll は引けて author だけ落ちた部分障害**で
			// 投票数が古いまま固定される — この関数が直したはずの結末そのもの。
			// not-found (= author 行が消えている) は retry しても変わらないので ack。
			if !repository.IsNotFound(aerr) {
				return fmt.Errorf("update remote question: verify attribution: %w", aerr)
			}
			return nil
		}
		if author == nil || author.URI == nil || *author.URI != actorURI {
			return nil
		}
	}
	apChoices := apNote.OneOf
	if len(apChoices) == 0 {
		apChoices = apNote.AnyOf
	}
	if len(apChoices) == 0 {
		return nil
	}
	// choice 名 -> totalItems。upstream は name 一致で照合する (位置ではない)。
	counts := make(map[string]int64, len(apChoices))
	for _, c := range apChoices {
		if c.Replies != nil && c.Replies.TotalItems >= 0 {
			// 保存側と同じ正規化を通す (#2726)。切った選択肢は生値では引けない。
			counts[remotePollChoice(c.Name)] = int64(c.Replies.TotalItems)
		}
	}
	newVotes := make([]int64, len(poll.Choices))
	copy(newVotes, poll.Votes)
	changed := false
	for i, name := range poll.Choices {
		if v, ok := counts[name]; ok && v != newVotes[i] {
			newVotes[i] = v
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return r.pollRepo.UpdateVotes(note.ID, newVotes)
}

// UpdateRemoteNote applies an inbound Update Note activity body to an existing
// remote-authored note row. Misskey 本家にはノート編集機能が無いためローカル
// note は不変だが、Mastodon 等から push されてくる Update activity は受信
// するだけ受信して反映する。
//
// 動作:
//   - URI でノートを引いて見つからなければ何もせず nil
//   - 見つかったが著者がローカルなら何もしない (ローカルノートは変更不可)
//   - Update activity の actor (actorURI) が note 著者と一致しなければ無視
//     (別 remote 著者の note URI を狙った改ざんを拒否、UpdateRemoteQuestion と対称、#1819)
//   - 著者がリモートで actor が一致すれば note の text / mentions / cw / url /
//     emojis / tags / fileIds / attachedFileTypes を更新する。**`sensitive` は
//     note の列としては書かない**が、cw の決定と**添付の NSFW 判定**に読む
//     (`upsertAttachments` が**新規に作る** `drive_file` の `isSensitive` /
//     `maybeSensitive` に書く。既知 URL の添付は URI で dedup して `continue`
//     する (関数から抜けるのではなく次の添付へ進む) ので、**保存済みの添付の
//     NSFW は送信者の sensitive では変わらない**。例外は連合のルール (#3090) の
//     「センシティブにする」で、投稿者自身の保存済みの行にも立てる)
//   - **書き込みは note の列だけではない。** 同じ呼び出しで `drive_file` /
//     `emoji` / hashtag の行が作られ、**`emoji` と hashtag は既存行も書き換わる**
//     — `upsertEmojis` は同名 + 同 host の行の `originalUrl` / `publicUrl` /
//     `uri` / `license` を上書きし、`hashtagHook.OnNoteCreated` は
//     `mentionedUserIds` への append と count++ を撃つ。`drive_file` だけは
//     URI で dedup するので新規作成のみ
func (r *Resolver) UpdateRemoteNote(body []byte, actorURI string, inboundSourceHost ...string) (*model.Note, error) {
	if r.noteRepo == nil {
		return nil, ErrInvalidNote
	}
	var apNote activitypub.Note
	if err := json.Unmarshal(body, &apNote); err != nil {
		return nil, ErrInvalidNote
	}
	// lookup も書き込み側と同じ正規化を通す (#2662)。
	apNote.ID = trimWHATWGURL(apNote.ID)
	if apNote.ID == "" {
		return nil, ErrInvalidNote
	}
	existing, err := r.noteRepo.FindByURI(apNote.ID)
	if err != nil {
		// **障害は伝播させる** (#3116)。ack すると job が retry されず、
		// **リモートのノート編集 (text / cw / 添付) が恒久的に落ちる**。
		// 同じ handler の Question 側と揃える — 片方だけ直すと、頻度の高い
		// ほうが取りこぼされたままになる。
		if !repository.IsNotFound(err) {
			return nil, fmt.Errorf("update remote note: lookup note: %w", err)
		}
		// 未取得のリモート Note は無視 (こちらに該当データが無いものを編集する
		// 通知が来ても反映先が無いため)。
		return nil, nil
	}
	if existing.UserHost == nil {
		// ローカルノートに対する Update は無視 (Misskey は編集機能を持たない)。
		return existing, nil
	}
	// attribution: Update の actor は note 著者と一致必須。別 remote 著者の note URI を
	// 指定した改ざん (**上の GoDoc が挙げる書き込み**。一覧をここにも書くと片方だけ
	// 古くなるので繰り返さない — 実際 `url` を足すまで両方が
	// `text/cw/sensitive/mentions` のまま陳腐化していた。`url` が入ったことで
	// クライアントが開く permalink も差し替え対象になった) を拒否する。**note の
	// 列だけの話ではない** — `upsertEmojis` に渡す host は**対象 note の著者**の
	// ものなので、この検査が無ければ他人の host の同名カスタム絵文字の画像 URL を
	// 攻撃者の URL に差し替えられる。inbox 層の
	// authorizeActor は signer==activity.actor と activity.id host の整合しか保証
	// しないため、ここで対象 note の著者まで照合する (#1819、UpdateRemoteQuestion と対称)。
	//
	// **一致を確認できなければ更新しない (#3037、`UpdateRemoteQuestion` と
	// 同じ理由)。** 以前は fail-open で、DB 障害 / 行の消失 / URI が NULL の
	// どれでも検査が丸ごと消えていた。
	if actorURI != "" {
		if r.userRepo == nil {
			return existing, nil
		}
		author, aerr := r.userRepo.FindByID(existing.UserID)
		if aerr != nil {
			// Question 側と同じ理由 (#3037 レビュー)。
			slog.Warn("federation: cannot verify note update attribution",
				"id", apNote.ID, "actor", actorURI, "err", aerr)
			return existing, nil
		}
		if author == nil || author.URI == nil || *author.URI != actorURI {
			return existing, nil
		}
	}
	fields := map[string]any{}
	// IngestNoteと同じ3段フォールバックでtext抽出
	var newText string
	if apNote.Source != nil &&
		apNote.Source.MediaType == "text/x.misskeymarkdown" &&
		apNote.Source.Content != "" {
		// create 経路と同じく NUL を落とす (#2723)。
		newText = remoteText(apNote.Source.Content, 0)
	} else if apNote.MisskeyContent != "" {
		newText = remoteText(apNote.MisskeyContent.String(), 0)
	} else if apNote.Content != "" {
		newText = mfm.FromHTML(apNote.Content)
	}
	// 保存するのは切った本文、判定と hashtag は全文 (create 経路と同じ)。
	storedText := clipRemoteNoteText(newText)
	// CW は**下の fields 反映より前に**決める。禁止語の判定を「更新後の値」で
	// 行うために先に要るが、判定は `existing` を書き換える前に済ませたい
	// (弾く場合は in-memory の note も無傷で返す)。
	var newCW *string
	if summary := remoteText(apNote.Summary.String(), noteCWMaxRunes); summary != "" {
		newCW = &summary
	} else if apNote.Sensitive {
		// Summary が空でも sensitive なら空 CW を保つ (IngestNote と対称)。
		empty := ""
		newCW = &empty
	}
	// 禁止語 (meta.prohibitedWords) は編集経路にも掛ける。
	//
	// **upstream には対応物が無い** — 本家 ApInboxService.update は object が
	// Actor でも Question でもなければ `skip: Unknown type` で捨てるので、Note の
	// 編集を取り込むのは mk-go 固有の拡張。取り込む以上、create 側と同じ検査を
	// 通さないと「禁止語を含まない note を投げてから Update で差し替える」で
	// 判定を素通りできる。
	//
	// **書き込みより前に判定する。** 以降の `upsertEmojis` / `upsertAttachments` は
	// note の列とは別に `emoji` / `drive_file` の行を書くので、後ろに置くと
	// 「本文は更新しないが絵文字だけ差し替わる」形になる。poll は
	// UpdateRemoteQuestion 側の担当なのでここでは渡さない (選択肢の文言自体は
	// Update(Question) でも変わらない — 変わるのは投票数だけ)。
	effectiveText := existing.Text
	if newText != "" {
		effectiveText = &newText
	}
	// **CW は受け取った Update を正とする** (#3090)。Update は投稿全体を送り直す
	// ものなので、summary が無ければ CW は外れている。以前は「変えない」として
	// 保存済みの値を使っていたが、保存済みの CW には連合のルールが付けた文言も
	// 入るので、それが送信者の CW として tag に拾われ、パターンや禁止語の判定にも
	// 混ざって以後の編集が全部弾かれた (#3090 の敵対的レビューで実測)。ルールの
	// CW は、当たれば下で付け直す。
	effectiveCW := newCW
	if r.containsProhibitedWords(effectiveText, effectiveCW, nil) {
		slog.Info("federation: dropping inbound note update containing prohibited words",
			"uri", truncateRunes(apNote.ID, noteURIMaxRunes), "noteId", existing.ID)
		return existing, nil
	}
	// text が変わらなくても tag 配列の Mention は AP Update で変化しうる (#397)。
	// IngestNote と同じく tag の Mention だけを user ID 配列にする (本家
	// ApNoteService.createNote の apMentions と同じ。本文からは取らない、#3330)。
	tagHrefs := extractMentionTags(apNote.Tag)
	var inboundChain *resolveChain
	if len(inboundSourceHost) > 0 {
		inboundChain = &resolveChain{inboundLocalReferences: true, inboundSourceHost: inboundSourceHost[0]}
	}
	tagMentions, merr := r.resolveMentionedUserIDs(tagHrefs, inboundChain)
	if merr != nil {
		return nil, fmt.Errorf("update remote note: %w", merr)
	}
	// 作成時と同じく返信先の作者と specified の宛先も足す。足さないと、編集のたびに
	// 作成時に入れたそれらが mentions から消える。
	mentions := withImplicitMentions(existing, mergeMentionIDs(nil, tagMentions))
	// メンション数の上限も編集経路に掛ける (#3330)。禁止語と同じく upstream には
	// 対応物が無い (ノートの Update を取り込まない) が、掛けないと「上限内の note を
	// 投げてから Update で Mention を増やす」で取り込み時の判定 (#17167 / #17576) を
	// 素通りできる。数え方は IngestNote と同じ。弾くときは禁止語と同じく更新を
	// 捨てて ack する (書き込みより前に判定する理由も同じ)。
	if r.exceedsMentionLimit(existing, mentions, tagHrefs) {
		slog.Info("federation: dropping inbound note update with too many mentions",
			"uri", truncateRunes(apNote.ID, noteURIMaxRunes), "noteId", existing.ID)
		return existing, nil
	}
	// 連合のルール (#3090) も編集に掛ける。掛けないと「条件に当たらない投稿を
	// 作ってから Update で差し替える」で素通りできる。判定は更新後の値で行う。
	var ruleDecision fedrule.Decision
	if r.rules != nil && r.rules.HasNoteRules() {
		author, aerr := r.noteAuthorForRules(existing.UserID)
		if aerr != nil {
			// 投稿者を引けないと bot / 新規の条件を判定できない。取り込みを
			// やり直させる (黙って素通しにしない)。
			return nil, fmt.Errorf("update remote note: rule author: %w", aerr)
		}
		hasAttachment := len(extractAttachments(apNote.Attachment, apNote.Sensitive.Bool())) > 0
		in := noteRuleFacts(author, effectiveText, effectiveCW, &apNote, hasAttachment)
		if in.Host == "" && existing.UserHost != nil {
			in.Host = strings.ToLower(*existing.UserHost)
		}
		in.Actor = r.rules.ActorFacts(author)
		in.Update = true
		// 「初めて見てから N 時間以内」は投稿した時刻で測る (fedrule.NoteInput.At)。
		if t, err := r.idGen.ParseTime(existing.ID); err == nil {
			in.At = t
		}
		ruleDecision = r.rules.EvaluateNote(in)
		if ruleDecision.Reject {
			slog.Info("federation: dropping inbound note update rejected by a federation rule",
				"uri", truncateRunes(apNote.ID, noteURIMaxRunes), "noteId", existing.ID)
			return existing, nil
		}
		if ruleDecision.Unlist && existing.Visibility == model.NoteVisibilityPublic {
			fields["visibility"] = model.NoteVisibilityHome
			existing.Visibility = model.NoteVisibilityHome
		}
	}
	// 編集した本文 / CW がセンシティブワードに当たれば、public の投稿を home にする
	// (取り込みと同じ判定)。編集の取り込みは Elythia の拡張なので本家に対応物は
	// 無いが、掛けないと当たらない投稿を作ってから差し替えるだけで降格を
	// 素通りできる。**狭める方向だけ** — 当たらなくなっても public には戻さない
	// (降格の理由がセンシティブワードかどうかを記録していないため)。
	if existing.Visibility == model.NoteVisibilityPublic && r.containsSensitiveWords(effectiveText, effectiveCW) {
		fields["visibility"] = model.NoteVisibilityHome
		existing.Visibility = model.NoteVisibilityHome
	}
	if newText != "" {
		fields["text"] = &storedText
		existing.Text = &storedText
	}
	if !slices.Equal([]string(existing.Mentions), []string(mentions)) {
		fields["mentions"] = mentions
		existing.Mentions = mentions
	}
	if !pointerStringsEqual(existing.CW, newCW) {
		fields["cw"] = newCW
		existing.CW = newCW
	}
	// permalink も追従する (#2729)。**捨てられた値では上書きしない** — 読めない
	// `url` が来ただけで、取り込み時に保存した正しい permalink を消してしまう。
	if u := remoteNoteURL(apNote.ID, apNote.URL.String()); u != "" && (existing.URL == nil || *existing.URL != u) {
		fields["url"] = &u
		existing.URL = &u
	}
	// AP Note Tag配列からカスタム絵文字を抽出してDBにupsert
	// 既存値と比較して変化があった場合のみfieldsに含める
	if existing.UserHost != nil {
		emojis := r.noteEmojisFor(r.upsertEmojis(extractEmojiTags(apNote.Tag), *existing.UserHost), existing.UserHost)
		if !slices.Equal([]string(existing.Emojis), []string(emojis)) {
			fields["emojis"] = emojis
			existing.Emojis = emojis
		}
	}
	// hashtag は AP `tag` Hashtag entry + 編集後の本文 / CW から再抽出して
	// 差分があれば更新 (#679)。IngestNote と同じ規則。
	//
	// 本文がある Update は全文 (`newText`、保存するのは切った `storedText`) から、
	// 本文の無い Update は保存済みの `existing.Text` (切った後) から拾う。後者は
	// 8192 単位を超えていた投稿では末尾のタグを拾わない (本文の無い Update は
	// まれなので、保存済みの値で足りるとした。禁止語と連合のルールの判定も同じ)。
	// CW は更新後の `existing.CW` を読むので、CW の更新より後ろに置く。将来 text 更新ロジックを別関数に切り出す等の refactor が
	// 入ったら、この順序依存も追従させること。
	hashtagSources := extractHashtagTagNames(apNote.Tag)
	if newText != "" {
		hashtagSources = append(hashtagSources, newText)
	} else if existing.Text != nil {
		hashtagSources = append(hashtagSources, *existing.Text)
	}
	if existing.CW != nil {
		hashtagSources = append(hashtagSources, *existing.CW)
	}
	newTags := hashtag.ExtractNoteTags(hashtagSources...) // 正規化して格納 (#1948-18)
	tagsChanged := !slices.Equal([]string(existing.Tags), newTags)
	if tagsChanged {
		// Extract は hashtag が無いと nil を返すが、nil の model.StringArray は
		// Updates() map 経由で SQL NULL になり note.tags (NOT NULL) 制約に違反する。
		// tags が非空→空に変わるケースで踏むため空配列に倒して '{}' を書く。
		noteTags := model.StringArray(newTags)
		if noteTags == nil {
			noteTags = model.StringArray{}
		}
		fields["tags"] = noteTags
		existing.Tags = noteTags
	}
	// ルールの CW は tag を抜いた後に付ける (取り込み側と同じ理由)。
	if cw := ruleCW(ruleDecision, existing.CW); cw != nil {
		fields["cw"] = cw
		existing.CW = cw
	}
	// AP `attachment` 配列の差分を反映する (#378)。driveFileRepo 未設定時は
	// upsertAttachments が空 slice を返すので何もしない (= 既存 fileIDs を
	// 誤って空に上書きしない、Devin #400 #1)。
	if r.driveFileRepo != nil {
		fileIDs := r.upsertAttachments(applyRulesToAttachments(ruleDecision,
			extractAttachments(apNote.Attachment, apNote.Sensitive.Bool())), &existing.UserID, existing.UserHost)
		// ルールを先に当てる (取り込み側と同じ理由)。
		folded := ruleDecision.Sensitive && !r.markFilesSensitive(fileIDs, existing.UserID)
		if !r.markMediaSilencedFiles(fileIDs, existing.UserID, existing.UserHost) {
			folded = true
		}
		if folded {
			if cw := foldCW(existing.CW); cw != nil {
				fields["cw"] = cw
				existing.CW = cw
			}
		}
		if !slices.Equal([]string(existing.FileIDs), []string(fileIDs)) {
			fields["fileIds"] = model.StringArray(fileIDs)
			existing.FileIDs = model.StringArray(fileIDs)
			types := r.collectAttachedFileTypes(fileIDs)
			fields["attachedFileTypes"] = model.StringArray(types)
			existing.AttachedFileTypes = model.StringArray(types)
		}
	}
	if len(fields) == 0 {
		return existing, nil
	}
	if err := r.noteRepo.UpdateFields(existing.ID, fields); err != nil {
		return nil, err
	}
	// hashtag table の更新は「tags が変化したかつ新規 tag がある」場合のみ呼ぶ
	// (#680)。RecordMention は冪等なので二度叩いても害は無いが、UPDATE を毎回
	// 走らせるのは無駄。tag が増えた / 入れ替わった場合に新規 tag 行が確保され
	// 著者カウントが反映される。逆に tag が「減った」場合の数値減算は upstream
	// Misskey TS も実装していない (新規 mention の積み上げ専用) ので追従しない。
	if tagsChanged && len(newTags) > 0 && r.hashtagHook != nil {
		// existing は user 情報を持たないので、UserID + UserHost から最小限の
		// User を組む。HashtagHook は IsLocal() / ID しか参照しないので十分。
		author := &model.User{ID: existing.UserID, Host: existing.UserHost}
		r.hashtagHook.OnNoteCreated(existing, author)
	}
	return existing, nil
}
