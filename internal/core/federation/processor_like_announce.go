package federation

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// handleLike attaches a reaction to a local note based on a Like activity.
//
// Note: P5 (#417) で reversi 早期分岐を入れた都合上、reactionService nil
// チェックを reversi 分岐の後に移動している。副作用として:
//   - ResolveActor が無条件で呼ばれる (旧実装は reactionService nil 時に
//     早期 return していた)。reversi 分岐側でも actor 解決は必要なので
//     妥当な変更。reactionService が wire されていない config は production
//     には無いので実害なし (#417 P5 Devin review)。
//   - object が malformed (string でも nested object でもない) Like は
//     reactionService の有無に関わらず ErrUnsupportedActivity (= 202 ack)
//     を返す。旧実装は reactionService 配線時に readObjectString error を
//     400 として返していたが、404 / 400 で peer の retry を誘発するより
//     202 で握り潰すほうが連合衛生的に望ましい (#417 P5 Devin review)。
func (p *Processor) handleLike(act genericActivity) error {
	reactor, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		// permanent な actor resolve 失敗 (削除済 / visibility 制限 /
		// authorized fetch 拒否 / malformed JSON) は activity を ack して
		// failed bucket 行きを避ける (#1183)。reactor が居なければ reaction
		// 自体作れないので skip = noop。transient (5xx / network) は従来通り
		// retry サイクルに乗せる。
		if isPermanentSkipError(err) {
			slog.Info("federation: skipping Like (actor unresolvable)",
				"actor", act.Actor, "err", err)
			return nil
		}
		return err
	}
	// アクティビティ全体を Like として解釈し content/_misskey_reaction を取得する。
	// object は URI 文字列の場合とネストされた Like オブジェクトの場合がある。
	var like activitypub.Like
	_ = json.Unmarshal(act.raw, &like)
	if like.Object == "" {
		// object が文字列の場合: Activity.Object は json:"-" でスキップされるため
		// act.Object (RawMessage) から直接読む
		uri, rerr := readObjectString(act.Object)
		if rerr != nil {
			return ErrUnsupportedActivity
		}
		like.Object = uri
	}
	reaction := like.Content
	if like.MisskeyReaction != "" {
		reaction = like.MisskeyReaction
	}
	// upstream like() は reaction を `_misskey_reaction ?? content ?? name` で解決
	// する。一部の Mastodon-fork / Pleroma 系 EmojiReact は shortcode を `name` に
	// 乗せるため、content/_misskey_reaction が無ければ name を fallback に使う
	// (#1948-21)。like.Name は埋め込み Object の name (AS activity の name)。
	if reaction == "" {
		reaction = like.Name.String()
	}
	// reversi game session URI (`/games/{UUID}/{sessionID}`) は CherryPick
	// 拡張の reaction 連合。純正 Misskey フロントは `reacted` を表示する UI を
	// 持たないので mk-go では state 変化させず 202 ack だけ返す (#417 P5)。
	// resolver.ResolveNote が 404 で失敗するのを避けるため Note Like より先に
	// 弾く必要がある。
	if corereversi.IsReversiGameSessionURI(like.Object) {
		// reactor / reaction は note Like 経路で使われるので blank assign 不要。
		// actor 解決は handleLike 冒頭で済ませているのでキャッシュ取込の
		// 副作用はこの早期 return でも残る。
		return nil
	}
	if p.reactionService == nil {
		return ErrUnsupportedActivity
	}
	target, err := p.resolver.ResolveNoteForInbox(like.Object, inboundActorHost(act.Actor))
	if err != nil {
		// permanent な target resolve 失敗 (followers-only / 削除済 等) は
		// reaction record を作らず ack。retry しても永久に解消しない (#1183)。
		if isPermanentSkipError(err) {
			slog.Info("federation: skipping Like (target unresolvable)",
				"object", like.Object, "actor", act.Actor, "err", err)
			return nil
		}
		return err
	}
	// Like activity に乗ってきたカスタム絵文字メタデータを emoji table に
	// upsert する。Misskey TS の apNoteService.extractEmojis(...) 相当
	// (#459)。reactionService.Create 自体は emoji 不在でも reaction 文字列
	// を保存できるので、upsert 失敗で Create を止める必要はなく、UI 表示
	// 側の reactionEmojis 解決が FallbackReaction (heart) に落ちる程度。
	if reactor.Host != nil && len(like.Tag) > 0 {
		p.resolver.upsertEmojis(extractEmojiTags(like.Tag), *reactor.Host)
	}
	if _, err := p.reactionService.Create(reactor, target.ID, reaction); err != nil {
		if errors.Is(err, corereaction.ErrAlreadyReacted) {
			return nil
		}
		// reaction service が visibility 違反 (`ErrNoteNotVisible`) 等を
		// 返すケースも permanent 扱いで skip (#1183)。
		if isPermanentSkipError(err) {
			slog.Info("federation: skipping Like (reaction policy violation)",
				"object", like.Object, "actor", act.Actor, "err", err)
			return nil
		}
		return err
	}
	return nil
}

// handleAnnounce creates a renote pointing at the announced note.
// signer は antenna hook の relay 判定にのみ使う。既存の viaRelay
// (announcer が relay actor 本人か) は renote 抑止の判定で、そちらの意味は
// 変えない — Mastodon 系リレーが転送した Announce は従来どおり renote を作る。
func (p *Processor) handleAnnounce(act genericActivity, signer *model.User) error {
	// bearcaps (bear:) object URI は未対応として skip (#1560、upstream
	// ApInboxService.announce の 'skip: bearcaps url not supported')。
	if isBearcapURI(act.Object) {
		slog.Info("federation: skipping Announce with bearcaps object url", "actor", act.Actor)
		return nil
	}
	announcer, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		// permanent な actor resolve 失敗は ack して skip (#1183、handleLike
		// と同じ理由)。
		if isPermanentSkipError(err) {
			slog.Info("federation: skipping Announce (actor unresolvable)",
				"actor", act.Actor, "err", err)
			return nil
		}
		return err
	}
	targetURI, err := readObjectString(act.Object)
	if err != nil {
		return err
	}
	// relay 由来かどうかは **note を解決する前に** 判定する。ResolveNote は
	// 解決と同時に DB へ永続化するので、後から判定したのでは「リレー投稿を
	// DB に入れない」(#2332) が成立しない。
	//
	// announcer.Inbox / SharedInbox が registered relay の Inbox と一致した
	// 場合のみ relay 由来と判定。
	viaRelay := p.relayActorChecker != nil && p.relayActorChecker.IsRelayActor(announcer)

	var target *model.Note
	targetCreated := false
	if viaRelay {
		// リレー経由でしか観測しない投稿は Redis に置く。ephemeral store が
		// 未配線 / 機能無効なら ResolveNoteEphemeral が通常経路に倒れる。
		target, err = p.resolver.ResolveNoteEphemeralForInbox(targetURI, inboundActorHost(act.Actor))
	} else {
		// **新規取り込みかどうかは resolver から受け取る** (#2751)。取得 URI で
		// 既存行を引く方式では、Announce の object が document id と別名の URL
		// (`https://h/@user/1` に対し id は `https://h/notes/1`) のときに何度
		// 取り込んでも「新規」に見え、既知の note を繰り返し antenna へ押し込める。
		// instance actor に見せなかった対象 (鍵アカウントが自分のフォロワー限定の
		// 投稿をリノートしたときなど) は、announcer をフォローしているローカルの
		// 利用者の鍵で取り直す (#3916)。
		target, targetCreated, err = p.resolver.resolveNoteWithCreatedForInboxRetrying(targetURI, inboundActorHost(act.Actor), p.hiddenNoteRetry(announcer, signer))
	}
	if err != nil {
		// permanent な target resolve 失敗 (削除済 note / followers-only) は
		// Announce 記録を skip して ack (#1183)。
		if isPermanentSkipError(err) {
			slog.Info("federation: skipping Announce (target unresolvable)",
				"object", targetURI, "actor", act.Actor, "err", err)
			return nil
		}
		return err
	}
	// upstream Misskey #17308 (= 2026.5.0 fix / triage #1002): relay 由来の
	// Announce は renote として保存せず、target note 自体を timeline stream に
	// publish する。renote 表示にしてしまうと「relay actor がリノートした」と
	// いう偽装表示になり、原投稿者からの直接配送と区別がつかなくなるため。
	if viaRelay {
		return p.publishRelayDeliveredNote(target, targetURI, act.Actor)
	}
	// upstream announceNote の 2 つの skip guard (#1560、ApInboxService.ts:350-361):
	//   1. !isVisibleForMe(renote, actor): 見えない note の boost は drop する。
	//      本家は isVisibleForMe を通った後も NoteCreateService が他人の followers
	//      note と specified note の renote を拒否するので、取り込めるのは public /
	//      home と作者本人の followers note だけになる。ここではその結果を直接
	//      判定する (remote-remote の follow graph は要らない)。本家は拒否を throw
	//      で job のエラーにするが、ここでは ack して捨てる。
	//   2. activity.published < 対象 note の createdAt: announce が対象 note より
	//      前に published されたと主張する malformed timestamp は drop する。
	//
	// **作者本人の followers note は通す** (#3520)。本家の isVisibleForMe は作者
	// 本人なら true で、NoteCreateService も自分の followers note の renote を許す
	// (公開範囲は下で followers に絞る)。近似がこの形まで捨てていたので、鍵
	// アカウントが自分の followers note をリノートしてもフォロワーに出なかった。
	targetPublicOrHome := target.Visibility == model.NoteVisibilityPublic || target.Visibility == model.NoteVisibilityHome
	selfFollowersRenote := target.Visibility == model.NoteVisibilityFollowers && target.UserID == announcer.ID
	if !targetPublicOrHome && !selfFollowersRenote {
		slog.Info("federation: skipping Announce of non-public note", "object", targetURI, "actor", act.Actor, "visibility", target.Visibility)
		return nil
	}
	// **ブースト対象そのものを antenna に載せる** (#2751)。hook に渡る renote 行は
	// Text / CW を持たないので、キーワード指定または withFile の antenna は
	// inbound Announce では原理的に 1 件もマッチしない (upstream は
	// ApInboxService.ts:329 の resolveNote(target) 経由で target 自体を
	// NoteCreateService へ通すので載る)。
	//
	// **upstream より後ろ、可視性 gate の直後に置く。** upstream は resolveNote の
	// 時点で載せるが、mk-go の antenna 可視性判定 (corenote.CanSeeNote) は
	// upstream の checkHitAntenna より緩く、followers note でも「viewer が
	// mention されている / reply 先が viewer」なら通す (#2106 N27 の意図的緩和)。
	// resolveNote の位置に置くと、crafted Announce を投げるだけで**配送されて
	// いない followers-only note を対象ユーザーの antenna に載せられる**入口が
	// できる。public/home に絞った後なら、その入口は開かない。作者本人の
	// followers note の renote (#3520) も、同じ理由でここには載せない。
	//
	// **新規に取り込んだときだけ発火させる。** 無条件だと再ブーストのたびに古い
	// note が antenna に湧く。upstream も既知の note では createNote を通らない
	// ので載らない。
	//
	// **ローカル note は対象外。** 作成時に既に antenna を通っている。
	// **canonical なローカル URI は `targetCreated` が既に弾く** —
	// `extractLocalNoteID` (urls.NoteURI の前方一致) → `FindByID` hit で
	// created=false になるため。その範囲では UserHost の条件は二重で、片方を
	// 外してもテストは落ちない (等価変異)。ただし前方一致から外れるローカル URL
	// は remote 扱いで fetch されうるので、条件としては残す。
	//
	// **リレー経由は #2743 の方針どおり外す。** 取りこぼしの範囲は
	// docs/divergence.md に記載。`!viaRelay` はここに到達する時点で常に真
	// (上の early return を通る) で、renote 側の hook と同じく将来 early return
	// を消したときの保険。実際に止めているのは signer 側の判定。
	if p.antennaHook != nil && targetCreated && targetPublicOrHome && !viaRelay && !p.isRelayDelivery(signer) &&
		target.UserHost != nil && *target.UserHost != "" {
		hydratedTarget, author := p.hydrateWithAuthor(target)
		if author == nil {
			// replica 遅延などで直前に入れた行の author を引けないことがある。
			// 黙って落とすと「antenna に載らない」だけが残るのでログに出す。
			slog.Warn("federation: announced target has no resolvable author, skipping antenna",
				"noteURI", targetURI, "userId", target.UserID)
		} else {
			p.goHook(func() { p.antennaHook.OnNoteCreated(hydratedTarget, author) })
		}
	}
	if act.Published != "" {
		if published, perr := time.Parse(time.RFC3339, act.Published); perr == nil {
			if targetCreatedAt, terr := p.resolver.idGen.ParseTime(target.ID); terr == nil && published.Before(targetCreatedAt) {
				slog.Info("federation: skipping Announce with malformed createdAt (published before target)",
					"object", targetURI, "actor", act.Actor)
				return nil
			}
		}
	}
	// announce 自身に id があれば URI として保存し、重複検出にも使う。
	// upstream Misskey #17356 (= 2026.5.1 fix) は同 activity の重複処理 race を
	// Redis 分散 lock で防ぐが、mk-go は single-instance 前提で DB unique 制約 +
	// 本 FindByURI 経由の dedup によって race を防いでいる。multi-instance 構成
	// (mk-go pod 複数台 load balance) を採用する場合は別途 Redis lock 化を検討
	// (= triage #1009 / upstream #17356 close)。
	if act.ID != "" {
		// `note.uri` は varchar(512)。**Announce の id は renote の身元**で、
		// 下の dedup (`FindByURI`) の鍵でもあるので切らずに拒否する (#2723)。
		// 切ると別の行と衝突する。
		//
		// **DB を引く前に見る。** NUL 入りの値は `FindByURI` の SELECT 自体が
		// 22021 で落ちる。
		//
		// **この error は上位に surface する** — `dispatchActivity` がそのまま返すので
		// inbox job は retry を使い切って dead になる (既定 8 回)。そもそも保存
		// できない activity なので結末としては正しい。
		//
		// **ここでの利得は「早く落とす」ことではない。** gate は `ResolveNote` の後
		// なので、対象 note の fetch と永続化は既に済んでいる。得られるのは、原因が
		// 22001 ではなく明示的な拒否として残ること。
		//
		// **NUL も届く。** `url.Parse` は制御文字を弾くが、**fragment だけは例外**で
		// `https://h/a#<NUL>` は parse を通る (実測)。`authorizeActor` の host 検査も
		// `Hostname()` を見るだけなので抜ける。「NUL は来ないから長さだけ見ればいい」に
		// 縮めないこと。
		if !fitsColumn(act.ID, noteURIMaxRunes) {
			slog.Warn("federation: rejecting Announce whose id does not fit note.uri",
				"id", truncateRunes(act.ID, noteURIMaxRunes))
			return ErrInvalidNote
		}
		switch _, err := p.noteRepo.FindByURI(act.ID); {
		case err == nil:
			// 既に取り込み済み。
			return nil
		case !repository.IsNotFound(err):
			// **障害を「重複でない」に倒さない** (#3116)。倒すと DB 障害のあいだ
			// 同じ Announce が二重の renote になる。
			return fmt.Errorf("announce dedup: %w", err)
		}
	}
	// 遅延配送 Announce では activity.published を採用して timeline 並びを
	// origin と揃える (#940)。空 / parse 不可なら nowFn にフォールバック。
	now := parseAPPublishedTime(act.Published, nowFn())
	// upstream announceNote は activity の to/cc から renote の visibility を決める。
	// 旧実装は public 固定で、home renote 由来の Announce (Misskey renderAnnounce は
	// home を to=[followers], cc=[Public] で送る) が public 化され public TL / RSS /
	// 連合配送に乗っていた (#1826)。inbound Note と同じ deriveVisibility で揃える
	// (Misskey renderAnnounce の public=to[Public] / home=to[followers]+cc[Public] /
	// followers=to[followers] shape を正しく判定する)。実装が送らない exotic shape
	// (cc のみ Public 等) での upstream parseAudience との差、および specified renote の
	// visibleUsers / 通知の扱いは #1864 で別途対応する。
	renoteVisibility := deriveVisibility(announcer, decodeAudience(act.To), decodeAudience(act.CC))
	// upstream NoteCreateService は renote の visibility を対象 note 以下に clamp する
	// (home note は public で renote できず home に落ちる)。home target に対する
	// public boost を home に落とす。これが無いと crafted Announce(to=[Public],
	// object=home note) で home note を global timeline に leak させられる。
	// followers target (作者本人のもの) は下で followers に絞る。
	if target.Visibility == model.NoteVisibilityHome && renoteVisibility == model.NoteVisibilityPublic {
		renoteVisibility = model.NoteVisibilityHome
	}
	// followers target (作者本人のものだけが上の gate を通る) は followers 以下に
	// 絞る。本家 NoteCreateService の `case 'followers'` と同じく、public / home を
	// followers に落とし、それより狭い公開範囲 (specified) は維持する。
	if target.Visibility == model.NoteVisibilityFollowers &&
		(renoteVisibility == model.NoteVisibilityPublic || renoteVisibility == model.NoteVisibilityHome) {
		renoteVisibility = model.NoteVisibilityFollowers
	}
	renote := &model.Note{
		ID:         p.resolver.idGen.Generate(now),
		UserID:     announcer.ID,
		UserHost:   announcer.Host,
		RenoteID:   &target.ID,
		Visibility: renoteVisibility,
	}
	if act.ID != "" {
		// 上の gate で列に入ることを確かめてある。
		uri := act.ID
		renote.URI = &uri
	}
	renote.RenoteUserID = &target.UserID
	renote.RenoteUserHost = target.UserHost
	if err := p.noteRepo.Create(renote); err != nil {
		return err
	}
	// 本家の announceNote も NoteCreateService を通るので、リノートした人の
	// notesCount を 1 増やす (#3928)。
	p.resolver.incrementAuthorNotesCount(announcer.ID)
	// upstream NoteCreateService の incRenoteCount 条件
	// (`data.renote.userId !== user.id && !user.isBot`) を inbound Announce
	// にも適用する。自己 boost と bot の boost は加算しない (#2283)。
	// Undo(Announce) 側の減算も同条件にして対称性を保つこと。
	if target.UserID != announcer.ID && !announcer.IsBot {
		_ = p.noteRepo.IncrementCount(target.ID, "renoteCount", 1)
	}
	hydrated := hydrateNoteForFanout(p.noteRepo, renote)
	// hook はベストエフォートで safeGoFedHook 経由で非同期発火する (#1158)。
	// handleCreate (#569) と同じく Redis LPUSH / publish 待ちで inbox worker
	// drain を block しないことが目的。順序保証は handleCreate と同様に各 hook
	// 側の冪等性で吸収する。closure capture する `hydrated` / `announcer` /
	// `target` は handleAnnounce 内 local 変数で関数 return 後も生存するため
	// goroutine 上の参照は安全 (race なし)。
	if p.fanoutHook != nil {
		p.goHook(func() { p.fanoutHook.OnNoteCreated(hydrated, announcer) })
	}
	// **リレー経由では発火させない (#2743)。** Misskey 系リレー (announcer が
	// relay actor 本人) は上の `if viaRelay` で publishRelayDeliveredNote へ
	// 抜けるので、ここの `!viaRelay` は実質常に true — early return を将来
	// 消したときの保険として残してある。実際にここで止めているのは Mastodon
	// 系リレーが他人の Announce を転送する形で、それは signer 側で判定する。
	//
	// **signer が nil のときは素通しする** (isRelayDelivery の fail-safe)。
	// production の inbox は必ず signer を積むので到達しない。
	//
	// **ここで渡すのは renote 行。** renote は text を持たないので、キーワード
	// 指定のある antenna はこの行では当たらない。ブースト対象そのものは上の
	// #2751 の block が別途渡している (新規取り込み時のみ)。
	if p.antennaHook != nil && !viaRelay && !p.isRelayDelivery(signer) {
		p.goHook(func() { p.antennaHook.OnNoteCreated(hydrated, announcer) })
	}
	if p.notificationHook != nil {
		// remote user が local note を renote した時に元投稿者へ通知を出す
		// (#415)。target が renoteTarget。reply 通知は Announce では発生
		// しないので nil を渡す。
		p.goHook(func() {
			p.notificationHook.OnNoteCreated(hydrated, announcer, nil, target)
		})
	}
	// chart hook 発火 (#1156)。dedup チェック (上部の `act.ID != ""` ゲート付き
	// FindByURI) を通り抜けて noteRepo.Create(renote) も成功した時点で「この
	// 経路では」新規作成と確定するので、created flag は持たない。
	//
	// 例外として `act.ID` が空 (= activity id を持たない Announce) の場合は
	// 上の dedup 自体が skip されるため、同じ Boost が複数回届くと renote 行も
	// 毎回新規に生成され chart も毎回 fire する。これは本 PR で導入した挙動
	// ではなく既存の handleAnnounce 設計に従ったもの。activity id 無しの
	// Announce を出す実装は実運用ではほぼ無いので影響軽微だが、`act.ID` の
	// dedup 強化は別 issue でフォロー (= renote 重複そのものの解消が主題)。
	if p.noteChartHook != nil {
		p.goHook(func() { p.noteChartHook.OnNoteCreated(hydrated) })
	}
	return nil
}

// publishRelayDeliveredNote handles a relay-delivered Announce (= no renote
// creation, just publish the target note to timeline streams). 仕様詳細は
// handleAnnounce 内の relay-delivered コメント参照 (upstream #17308 / triage
// #1002)。
//
// target の author を最も信頼できる relation 経由で解決する: まず
// hydrateNoteForFanout で User relation を pre-load し、それが空なら
// userRepo.FindByID(target.UserID) に fallback する。author が解決できない
// 場合 (= target.UserID も無い等の broken state) は fanoutHook を呼ばずに
// 戻る (safe degrade)。
//
// renoteCount は increment しない (= target は新規 renote 対象ではない)、
// notificationHook も呼ばない (= 原投稿者には relay 経由配送の通知を出さない、
// 上流 / 他 instance から直接届く Create で通知済みの可能性が高いため二重通知
// 回避)。
func (p *Processor) publishRelayDeliveredNote(target *model.Note, targetURI, relayActor string) error {
	hydrated, author := p.hydrateWithAuthor(target)
	if author == nil {
		slog.Warn("federation: relay-delivered note has no resolvable author, skipping fanout",
			"relay", relayActor, "noteURI", targetURI, "userId", target.UserID)
		return nil
	}
	if p.fanoutHook != nil {
		p.goHook(func() { p.fanoutHook.OnNoteCreated(hydrated, author) })
	}
	// **antennaHook はここでは発火させない (#2743)。** 理由は AntennaHook の
	// doc を参照。handleCreate / handleAnnounce との差はこの 1 点だけなので、
	// 「揃っていない」と見て足さないこと。
	slog.Debug("federation: relay-delivered note published",
		"relay", relayActor, "noteURI", targetURI, "authorId", author.ID)
	return nil
}

// hydrateWithAuthor pre-loads a note's relations for hook delivery and resolves
// its author, falling back to userRepo when the User relation is empty.
// author が解決できない (broken state) 場合は nil を返すので、呼び出し側は
// hook を呼ばずに戻ること。
func (p *Processor) hydrateWithAuthor(n *model.Note) (*model.Note, *model.User) {
	hydrated := hydrateNoteForFanout(p.noteRepo, n)
	author := hydrated.User
	if author == nil && p.userRepo != nil && n.UserID != "" {
		if u, err := p.userRepo.FindByID(n.UserID); err == nil {
			author = u
		}
	}
	return hydrated, author
}

// isRelayDelivery reports whether the activity was forwarded by a subscribed
// relay, based on the verified HTTP signer (#2338).
//
// signer が nil (署名なし経路 / verifier 未配線) なら false。判定できない
// ときはリレー扱いしない = 従来どおり DB に保存する fail-safe。
func (p *Processor) isRelayDelivery(signer *model.User) bool {
	return signer != nil && p.relayActorChecker != nil && p.relayActorChecker.IsRelayActor(signer)
}

// resolveCreateActor resolves the author of an inbound Create, keeping
// relay-forwarded authors out of the database (#2338).
func (p *Processor) resolveCreateActor(uri string, viaRelay bool) (*model.User, error) {
	if viaRelay {
		return p.resolver.ResolveActorEphemeral(uri)
	}
	return p.resolver.ResolveActor(uri)
}

// ingestCreateNote persists an inbound Create's note, diverting relay-forwarded
// ones to the ephemeral store (#2338).
//
// 引用先が instance actor に見えなくても取り直さない (#3916)。取り直して取り
// 込んでも、フォロワー限定・指名の投稿は引用先につながず (ingestNoteWithCreated)、
// 引用のたびにフォロワーの actor を相手に知らせるだけになる。
func (p *Processor) ingestCreateNote(object []byte, deliveringActorURI string, viaRelay bool, sourceHost string) (*model.Note, bool, error) {
	if viaRelay {
		return p.resolver.IngestNoteEphemeralForInbox(object, deliveringActorURI, sourceHost)
	}
	return p.resolver.IngestNoteWithCreatedForInbox(object, deliveringActorURI, sourceHost)
}

// FollowerFetchKeys picks the key of a local follower of a remote actor
// (#3916). Implemented by LocalFollowerKeys.
type FollowerFetchKeys interface {
	KeyForFollowerOf(actor *model.User) *activitypub.PrivateKey
}

// SetFollowerFetchKeys wires the picker used to retry, with a local follower's
// key, an Announce target the instance actor cannot see (#3916). Without it
// nothing is retried.
func (p *Processor) SetFollowerFetchKeys(k FollowerFetchKeys) {
	p.followerKeys = k
}

// hiddenNoteRetry returns the lazy key source for the target of actor's
// Announce, or nil when it should not be retried: nothing wired, a local or
// hostless actor, or a relay delivery (signer is a relay).
//
// **取り直すのは、その actor をフォローしている人の鍵だけ。** フォロワー限定の
// 投稿は、相手のサーバーがフォロワーにしか見せない。フォローしていない人の鍵では
// 取れないうえ、こちらの誰かを無関係な相手に知らせることになる。取り直すのは
// actor と同じホストの投稿だけで (フォロワーがフォローしているのはその actor)、
// 鍵は取り直しが要るときに初めて引く (fetchKeySource)。
func (p *Processor) hiddenNoteRetry(actor, signer *model.User) *fetchKeySource {
	if p.followerKeys == nil || actor == nil || actor.IsLocal() || actor.Host == nil || p.isRelayDelivery(signer) {
		return nil
	}
	keys := p.followerKeys
	return lazyFetchKeySource("followers-of\x00"+actor.ID, *actor.Host, func() *activitypub.PrivateKey {
		return keys.KeyForFollowerOf(actor)
	})
}
