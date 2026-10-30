package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/elythia-network/elythia/internal/activitypub"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	"github.com/elythia-network/elythia/internal/repository"
)

// handleFollow processes an inbound Follow activity.
func (p *Processor) handleFollow(act genericActivity) error {
	follower, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	followeeURI, err := readObjectString(act.Object)
	if err != nil {
		return err
	}
	// 本家 follow() は getUserFromApId で引く (自ホストかは host で判定し、
	// `/users/{id}` の id だけを読む。削除済みの利用者は引かない)。
	// 見つからなければ本家は 'skip: followee not found' で ack する。以前は
	// error を返して inbox の retry を使い切っていた (#3330)。DB 障害は retry
	// させるために伝播する (#3115 と同じ扱い)。
	followee, err := p.userFromAPID(followeeURI, inboundActorHost(act.Actor))
	if err != nil {
		if repository.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("follow: lookup followee: %w", err)
	}
	// upstream follow() は followee が remote の場合 'skip: フォローしようと
	// しているユーザーはローカルユーザーではありません' で明示的に拒否する
	// (ApInboxService.ts)。これが無いと relay / 悪意ある remote actor が
	// Follow(actor=remoteA, object=既知 remoteB) を送るだけで local DB に
	// remoteA->remoteB の Following 行が作られ、フォロワー数や timeline fanout を
	// 汚染できる。skip は error にせず ack して終わる (inbox retry を防ぐ) (#1826)。
	if !followee.IsLocal() {
		slog.Info("federation: skipping inbound Follow targeting a remote followee",
			"follower", act.Actor, "followee", followeeURI)
		return nil
	}
	// フォローしてきた人のアカウントの作成日時を、分からなければ埋める
	// (#3465)。フォローの判定 (#3466) より前に置く。best-effort で、失敗しても
	// フォローは続ける。待つのは accountCreatedAtFillTimeout まで。
	if p.accountCreatedAt != nil {
		p.accountCreatedAt(context.Background(), follower)
	}
	// inbound AP Follow は AP protocol で withReplies を運ばないので
	// FollowOptions{} (default false) で作成する (#1056)。remote follower の
	// per-followee withReplies preference は AP protocol 範囲外。
	result, err := p.followingService.Follow(follower.ID, followee.ID, corefollowing.FollowOptions{})
	// block 起因の失敗は upstream UserFollowingService.follow と同じ AP 流儀で
	// 処理する (#1631)。エラーのまま返すと inbox が retry → dead-letter になり
	// 相手サーバーの pending follow が永遠に解消されない。
	if errors.Is(err, corefollowing.ErrBlocking) && !follower.IsLocal() && followee.IsLocal() && p.blockingService != nil {
		// ErrBlocking = remote follower が local followee を block している。
		// upstream は相互 block なら blocked (Reject) を優先し、blocking 単独
		// なら自動で block 解除して follow を続行する。
		blocked, berr := p.blockingService.IsBlocked(followee.ID, follower.ID)
		if berr != nil {
			return berr
		}
		if blocked {
			err = corefollowing.ErrBlocked
		} else {
			if uerr := p.blockingService.Unblock(follower.ID, followee.ID); uerr != nil {
				return fmt.Errorf("inbound follow: auto-unblock %s->%s: %w", follower.ID, followee.ID, uerr)
			}
			slog.Info("inbound follow: auto-unblocked stale remote block",
				"follower", follower.ID, "followee", followee.ID)
			result, err = p.followingService.Follow(follower.ID, followee.ID, corefollowing.FollowOptions{})
		}
	}
	if errors.Is(err, corefollowing.ErrBlocked) && !follower.IsLocal() && followee.IsLocal() {
		// local followee が remote follower を block している場合は Reject を
		// 送り返して正常終了する (upstream「エラーにするのではなく Reject を
		// 送り返しておしまい」)。配送失敗は Accept 側と同じく warn のみ。
		if p.inboundFollowAcceptor != nil {
			if rerr := p.inboundFollowAcceptor.SendRejectForInboundFollow(follower, followee, act.raw); rerr != nil {
				slog.Warn("inbound follow reject delivery failed",
					"follower", follower.ID, "followee", followee.ID, "err", rerr)
			}
		}
		return nil
	}
	alreadyFollowing := errors.Is(err, corefollowing.ErrAlreadyFollowing)
	// ErrAlreadyRequested はlocked followeeへの再送。既に FollowRequest が
	// 存在するので何もしなくて良い (Accept は承認時に送られる)。エラーとして
	// 上に返すと inbox が 4xx を返して相手が retry を続けるので swallow する。
	alreadyRequested := errors.Is(err, corefollowing.ErrAlreadyRequested)
	if err != nil && !alreadyFollowing && !alreadyRequested {
		return err
	}
	// Accept を返送するのは Following が成立した (新規 or 既存) 場合のみ。
	// 既にフォローされている (ErrAlreadyFollowing) 場合も Accept を再送する:
	// 相手サーバーが Accept を見逃していた or retry で新規 Follow を
	// 送り直した状況で、我々が何も返さないと相手は永遠に pending のままに
	// なる (idempotent な Accept 再送で解消する)。original Follow の raw を
	// そのまま object に包むことでリモート側が送った Follow と matching
	// できる。FollowRequest 止まり (locked followee) の場合は acceptor を
	// 呼ばない (Accept は明示承認後)。
	shouldAccept := alreadyFollowing || (result != nil && result.Following != nil)
	if shouldAccept && p.inboundFollowAcceptor != nil {
		if err := p.inboundFollowAcceptor.SendAcceptForInboundFollow(follower, followee, act.raw); err != nil {
			slog.Warn("inbound follow accept delivery failed",
				"follower", follower.ID, "followee", followee.ID, "err", err)
		}
	}
	return nil
}

// handleUndo processes an Undo activity wrapping a Follow / Like / Announce.
func (p *Processor) handleUndo(act genericActivity) error {
	var inner genericActivity
	// 型エラーを握らないと、直後の normalizeActor (#999) が救うはずの
	// `inner.actor` が embedded object のケースに**到達できない** (#2662)。
	if err := unmarshalIgnoringTypeErrors(act.Object, &inner); err != nil {
		// object の形が壊れている Undo は送り直しても直らない。再試行を使い切らせ
		// ないために受け取って捨てる (下の reject / flag も同じ)。
		slog.Info("federation: dropping Undo with an unreadable object", "actor", act.Actor, "err", err)
		return nil
	}
	// inner.actor が embedded object のケースも救済する (#999 / upstream #17340)。
	//
	// **現状の Undo 配下はどれも `inner.Actor` を読まない** (権威は外側の
	// `act.Actor`、対象は `inner.Object`)。Accept / Reject と形を揃えるための
	// 予防で、ここだけは挙動で観測できない = テストで固定できない。消しても
	// 今は壊れないが、inner.actor を読む sub-handler を足したときに object
	// 形式が黙って落ちるので残す (#2665)。
	inner.normalizeActor(act.Object)
	switch strings.ToLower(inner.Type) {
	case "follow":
		return p.handleUndoFollow(act, inner)
	case "like":
		return p.handleUndoLike(act, inner)
	case "announce":
		return p.handleUndoAnnounce(act, inner)
	case "block":
		return p.handleUndoBlock(act, inner)
	case "accept":
		return p.handleUndoAccept(act, inner)
	case "emojireaction", "emojireact":
		return p.handleUndoLike(act, inner)
	case "invite":
		// CherryPick が pre-start reversi 招待を取り消す際に Undo(Invite) を
		// 送ってくる (#417 P4)。inner.Object が reversi Game なら reversi
		// 側で処理、それ以外 (例: Group Invite) は未対応扱いにして inbox
		// が 202 を返すようにする (400 にすると peer 側の配送 retry を
		// 招いてしまう)。
		if err := p.handleReversiUndoInvite(act, inner); err != nil {
			if errors.Is(err, ErrNotReversiGame) {
				return ErrUnsupportedActivity
			}
			return err
		}
		return nil
	}
	return ErrUnsupportedActivity
}

// handleUndoAccept processes an inbound Undo(Accept): a remote followee revokes
// a previously-accepted follow, which means we should drop our local follower's
// following relationship to them (upstream ApInboxService.undoAccept、#1560)。
//
// 二段ネスト: act=Undo, inner=Accept, inner.Object=元の Follow。
//   - act.Actor       = remote followee (accept を撤回した側)
//   - Follow.actor     = local follower (フォローしていた側)
//
// follower→followee の Following があれば Unfollow する。
func (p *Processor) handleUndoAccept(act genericActivity, inner genericActivity) error {
	followee, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	// inner.Object は元の Follow。その actor が local follower。
	//
	// 型エラーを握らないと、直後の normalizeActor (#999) が救うはずの
	// `"actor": {"id": ...}` 形式で早期 return してしまい、Undo(Accept(Follow))
	// が unfollow されない (#2662)。string URI は握っても ID / Actor が空のまま
	// なので従来どおり skip に落ちる。
	var follow genericActivity
	if uerr := unmarshalIgnoringTypeErrors(inner.Object, &follow); uerr != nil {
		// object が Follow の URI 文字列だけのケースは follower を特定できない。
		// upstream は getUserFromApId(activity.object) で解決するが、mk-go では
		// Follow URI から follower を逆引きする経路が無いので skip (ack)。
		return nil
	}
	follow.normalizeActor(inner.Object)
	followerURI, err := readActorString(follow)
	if err != nil || followerURI == "" {
		return nil
	}
	follower, err := p.userFromAPID(followerURI, inboundActorHost(act.Actor))
	if err != nil {
		// **#3115 が Accept / Reject で確立した原則の裏返し** (#3116)。ack すると
		// job が retry されず、**accept を撤回された相手をフォローし続ける**。
		if repository.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("undo accept: lookup follower: %w", err)
	}
	if follower == nil {
		return nil
	}
	if err := p.followingService.Unfollow(follower.ID, followee.ID); err != nil {
		// フォローしていなければ no-op (upstream の 'skip: フォローされていない')。
		if errors.Is(err, corefollowing.ErrNotFollowing) {
			return nil
		}
		return err
	}
	return nil
}

// handleUndoFollow undoes a previously created Follow.
func (p *Processor) handleUndoFollow(act genericActivity, inner genericActivity) error {
	follower, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	followeeURI, err := readObjectString(inner.Object)
	if err != nil {
		return err
	}
	// 本家 undoFollow() も getUserFromApId で引き、見つからなければ skip (#3330)。
	followee, err := p.userFromAPID(followeeURI, inboundActorHost(act.Actor))
	if err != nil {
		if repository.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("undo follow: lookup followee: %w", err)
	}
	// #2106 L30: upstream undoFollow は followee が remote (host != null) なら skip する
	// (handleFollow の gate と対称)。remote→remote の Undo を DB 走査 / counter 経路に
	// 入れない防御線を Undo 側にも揃える。
	if followee.Host != nil {
		return nil
	}
	if err := p.followingService.Unfollow(follower.ID, followee.ID); err != nil &&
		!errors.Is(err, corefollowing.ErrNotFollowing) {
		return err
	}
	// locked local followee 宛の pending follow request を取り消す。upstream
	// undoFollow は requestExist を見て cancelFollowRequest を呼ぶ。これが無いと
	// remote が accept 前に Undo(Follow) を送った場合、phantom な follow request 行と
	// receiveFollowRequest 通知が残る (handleReject と同じ pattern、#1948-21)。
	if err := p.followingService.CancelRequest(follower.ID, followee.ID); err != nil &&
		!errors.Is(err, corefollowing.ErrRequestNotFound) {
		return err
	}
	return nil
}

// handleUndoLike removes a reaction previously added via Like.
//
// Note: reversi game session URI を対象とした Undo(EmojiReaction) は
// handleLike と対称に 202 ack で握り潰す (#417 P5)。dispatch 順も handleLike
// と揃えており、ResolveActor を先頭で実行することで actor cache の populate
// が両 path で同様に行われるようにする (#417 P5 Devin review)。
// reactionService nil の本番 config は無いので unconditional な ResolveActor
// で実害なし。
func (p *Processor) handleUndoLike(act genericActivity, inner genericActivity) error {
	reactor, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		// permanent な actor resolve 失敗は ack して skip (#1183)。
		if isPermanentSkipError(err) {
			slog.Info("federation: skipping Undo(Like) (actor unresolvable)",
				"actor", act.Actor, "err", err)
			return nil
		}
		return err
	}
	targetURI, err := readObjectString(inner.Object)
	if err != nil {
		// inner.object 欠如した malformed Undo(Like) は handleLike と同じく
		// 202 ack 扱い (旧実装は reactionService nil 経路で ErrUnsupportedActivity
		// を返していた)。
		return ErrUnsupportedActivity
	}
	if corereversi.IsReversiGameSessionURI(targetURI) {
		return nil
	}
	if p.reactionService == nil {
		return ErrUnsupportedActivity
	}
	target, err := p.resolver.ResolveNoteForInbox(targetURI, inboundActorHost(act.Actor))
	if err != nil {
		// permanent な target resolve 失敗は ack。Undo の対象 reaction record
		// が存在しないだけでも idempotent (= 削除済みは ErrReactionNotFound で
		// nil 返却) なので、resolve できない先の reaction は元から無い扱いで
		// 整合する (#1183)。
		if isPermanentSkipError(err) {
			slog.Info("federation: skipping Undo(Like) (target unresolvable)",
				"object", targetURI, "actor", act.Actor, "err", err)
			return nil
		}
		return err
	}
	if err := p.reactionService.Delete(reactor, target.ID); err != nil {
		// 既に削除済みは許容
		if errors.Is(err, corereaction.ErrReactionNotFound) {
			return nil
		}
		return err
	}
	return nil
}

// handleUndoAnnounce removes a previously created Announce (renote).
// inner.Object には元ノートの URI を持つことを想定する。
func (p *Processor) handleUndoAnnounce(act genericActivity, inner genericActivity) error {
	announcer, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		// permanent な actor resolve 失敗は ack (#1183)。announcer が居な
		// ければ Announce record も作っていないはずなので削除対象も無く
		// idempotent と整合する。
		if isPermanentSkipError(err) {
			slog.Info("federation: skipping Undo(Announce) (actor unresolvable)",
				"actor", act.Actor, "err", err)
			return nil
		}
		return err
	}
	targetURI, err := readObjectString(inner.Object)
	if err != nil {
		return err
	}
	target, err := p.resolver.ResolveNoteForInbox(targetURI, inboundActorHost(act.Actor))
	if err != nil {
		// permanent な target resolve 失敗は ack。target が解決できなければ
		// renote record も無いはずなので、削除対象が無いまま activity を ack
		// する形で整合する (#1183)。
		if isPermanentSkipError(err) {
			slog.Info("federation: skipping Undo(Announce) (target unresolvable)",
				"object", targetURI, "actor", act.Actor, "err", err)
			return nil
		}
		return err
	}
	// announcer が pure renote を 1 件でも持っていれば削除する。複数あった場合
	// は最新の 1 件のみで十分 (本家 misskey の挙動と同じ)。
	// viewerID には announcer.ID を渡す: push-down 後 (#1500) も self-branch で
	// announcer 自身の renote は visibility を問わず全件見えるため、下の
	// `n.UserID != announcer.ID` で残す対象行は従来どおり取得できる。
	renotes, err := p.noteRepo.ListRenotesOf(target.ID, announcer.ID, "", "", 50)
	if err != nil {
		return err
	}
	for _, n := range renotes {
		if n.UserID != announcer.ID {
			continue
		}
		if !corenote.IsPureRenote(n) {
			continue
		}
		if err := p.noteRepo.Delete(n); err != nil {
			return err
		}
		// 取り消されたブーストの通知を消す (#3201)。**この経路は DeleteService を
		// 通らない** (noteRepo.Delete を直接呼ぶ) ので、DeleteService 側の hook
		// だけではリモートからのリノート取り消しが丸ごと漏れる。
		if p.notificationHook != nil {
			p.notificationHook.OnNoteDeleted(n)
		}
		// handleAnnounce の increment と同条件でのみ減算する。条件がずれると
		// 加算しなかった boost の undo で count が負に振れる (#2283)。
		//
		// なお upstream は renoteCount を減算しない (incRenoteCount しか無く、
		// note 削除時も据え置き) ため、これは mk-go の意図的な divergence。
		// unrenote 後もカウントが残り続ける方が不自然なので維持する
		// (docs/divergence.md 参照)。
		if target.UserID != announcer.ID && !announcer.IsBot {
			_ = p.noteRepo.IncrementCount(target.ID, "renoteCount", -1)
		}
		return nil
	}
	return nil
}

// lookup の失敗を ack するか伝播させるかの規則 (#3115 / #3116 / #3121)。
//
//  1. **inbox job / queue job から到達する経路は分ける。** not-found は retry しても
//     結果が変わらないので ack、それ以外は伝播させて queue に retry させる。
//     `repository.IsNotFound` で判定する。ack すると job が成功扱いになるので、
//     **一時的な接続断だけで activity が恒久的に失われる** — 相手の再送は
//     当てにできない (inbox は enqueue した時点で 202 を返す)。
//  2. **fire-and-forget な hook は ack のまま。** 配送 hook は `safeGo` で
//     投げっぱなしに呼ばれ、戻り値も retry の仕組みも無いので、error を返しても
//     行き先が無い。**理由を書いて残す** (`note_delivery_hook.go` の
//     `findNoteAuthor` に書いてある。他はそこを参照する)。
//  3. **#534 の idempotency invariant と混同しない。** あれは「retry しても結果が
//     変わらないもの」の話で、`ErrRequestNotFound` / `ErrBlocking` / `ErrBlocked` が
//     それに該当する。**retry すれば成功しうる障害はそこに入らない。**
//  4. **「確かめられなかった」は「無かった」ではない (#3121)。** 署名検証の失敗は
//     既定で ack する — 署名が合わない body を retry しても結果は変わらないため。
//     しかし検証は DB も読むので、そのまま書くと **DB 障害のあいだに届いた
//     activity まで同じ ack に落ちる**。actor / 公開鍵の lookup は
//     `ErrLookupUnavailable` で種別を残し、`inbox.go` がそれだけ retry に倒す。
//     **落とす側は 1 箇所ではない** — HTTP 署名 (`verifyPayload`)、転送 activity の
//     認可 (`authorizeActor` → LD-Signature の creator 解決と鍵引き)、Headers 無しの
//     legacy 経路 (`ldVerifier.VerifyAndCompact`) の 3 つが同じ sentinel を見る。
//
// この規則で数えれば対象は導ける (件数を別に持たない)。
//
// **#3121 時点で ack のまま残っているもの** (どれも規則 1-4 で説明が付く):
//   - `resolver_actor_refresh.go` の `fetchActor` / `fetchNote` の失敗 — リモートが
//     取れないという相手側の事情で、**こちらの retry で取り戻せるとは限らない**。
//     分けるなら HTTP status から一時的か恒久的かを決める別の設計が要るので、
//     ここでは触っていない。
//   - `resolver_actor.go` / `resolver_note_ingest.go` の `userRepo.Create` /
//     `noteRepo.Create` の失敗 — 読み側を先に分けたので、DB が落ちていれば
//     手前の lookup で止まる。ここに来るのは列に入らない値のように
//     retry しても変わらないものが主だが、**書き込みだけが落ちている状態は
//     取りこぼす**。
//   - `post_scheduled_note.go` の publish 失敗 — 二重 publish / 二重通知を避ける
//     ための意図的な設計 (#2106 L61)。予約投稿の job は attempts を積むように
//     なったが (#3121)、retry されるのは **publish に到達する前の失敗だけ**。
//   - 配送 hook 一式 — 規則 2。

// handleAccept processes an inbound Accept activity. リモートfolloweeがローカル
// followerからのフォローリクエストを承認した場合に、フォロー関係を確立する。
// inner.id が自ホストの follow-relay URI で、かつ送信元 actor がその relay
// 自身であれば relay の Accept とみなし、RelayStatusMarker.MarkAccepted を
// 呼び出す。actor の inbox / sharedInbox と relay の inbox の完全一致は本家
// 2026.10.0 と同じで、id が自ホストの URI であることと actor の host の照合は
// mk-go 側の追加。status が requesting の relay だけが動く (MarkAccepted 側)。
func (p *Processor) handleAccept(act genericActivity) error {
	// こちらが送った QuoteRequest への承認 (FEP-044f、#3234)。object が id だけの
	// 文字列でも来るので、下の Follow 用の解釈より先に見る。
	if p.quoteAnswers != nil {
		if reqURI := quoteAnswerRequestURI(act.Object, p.localBaseURL); reqURI != "" {
			return p.quoteAnswers.HandleAnswer(act.Actor, reqURI, true, quoteAnswerResult(act.raw))
		}
	}
	var inner genericActivity
	if f, ok := p.localFollowFromID(act.Object, act.Actor); ok {
		// object が Follow の id だけの文字列 (#3491)。本家 ApInboxService.accept は
		// resolver で id を解決し、自インスタンスの `/follows/` は follow request から
		// Follow を組み直す。ここで組み直さないと、id だけの Accept を返す相手への
		// フォローがずっと申請中のまま残る。
		inner = f
	} else if err := unmarshalIgnoringTypeErrors(act.Object, &inner); err != nil {
		// 型エラーを握らないと、直後の normalizeActor (#999) が救うはずの
		// `inner.actor` が embedded object のケースに**到達できない** (#2662)。
		return nil
	} else {
		// inner.actor が embedded object のケースも救済する (#999 / upstream #17340)。
		inner.normalizeActor(act.Object)
	}
	// chat room invitation の Accept (remote が我々の room 招待を承認) は
	// membership 化する (#1203)。
	if strings.EqualFold(inner.Type, "invite") {
		return p.handleChatRoomAccept(act, inner)
	}
	if !strings.EqualFold(inner.Type, "follow") {
		return nil
	}
	if relayID := matchFollowRelayID(inner.ID, p.localBaseURL); relayID != "" && p.relayMarker != nil {
		// 送信元がその relay 自身でなければ状態を変えずに drop する。
		// 通さないと、署名が通る任意のリモート actor が未承認 / 拒否済みの
		// relay を accepted に倒せ、全公開ノートがその inbox へ流れ出す。
		allowed, err := p.relayStatusChangeAllowed(act.Actor, relayID, "Accept")
		if err != nil {
			return err
		}
		if !allowed {
			return nil
		}
		return p.relayMarker.MarkAccepted(context.Background(), relayID)
	}
	// actorはリモートfollowee（承認した側）
	followee, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	// inner.actorはローカルfollower（フォローを申請した側）
	followerURI, err := readActorString(inner)
	if err != nil {
		return nil
	}
	follower, err := p.userFromAPID(followerURI, inboundActorHost(act.Actor))
	if err != nil {
		// **not-found だけ ack する** (#3115)。その利用者が居ないことは retry
		// しても変わらないので、7 回 retry (試行は計 8 回、
		// `defaultInboxJobMaxAttempts`) してから dead letter に積む意味が無い。
		//
		// **DB 障害は伝播させる。** ack すると**job が成功扱いになって queue が
		// retry しない**ので、一時的な接続断だけで Accept が恒久的に失われ、
		// ローカル利用者のフォローリクエストは「リクエスト中」のまま永久に残る。
		// **相手の再送は当てにできない** — inbox は enqueue した時点で 202 を
		// 返すので、worker が ack しようが error を返そうが相手が見る応答は
		// 変わらない。#534 の idempotency invariant は「retry しても結果が
		// 変わらないもの」の話で、**retry すれば成功しうる障害はそこに入らない**
		// (#2792 と同じ判断)。
		if repository.IsNotFound(err) {
			return nil
		}
		return err
	}
	// フォローリクエストを承認してフォロー関係を確立
	if err := p.followingService.AcceptRequest(followee.ID, follower.ID); err != nil {
		// フォローリクエストが存在しない場合は無視（既にフォロー済み等）
		if errors.Is(err, corefollowing.ErrRequestNotFound) {
			return nil
		}
		// block 関係による拒否 (AcceptRequest の多層防御、#3111 レビュー) も
		// retry しても結果は変わらないので ack する。生で返すと inbox job が
		// 8 回 retry してから dead letter に積まれ、その間 inbox health の
		// 統計にも失敗として乗る (#534 の idempotency invariant)。
		if errors.Is(err, corefollowing.ErrBlocking) || errors.Is(err, corefollowing.ErrBlocked) {
			return nil
		}
		return err
	}
	return nil
}

// handleReject undoes a Follow that was rejected by the followee.
//
// 想定: 自分 (ローカル follower) が remote followee に対して Follow を送ったが
// 拒否された場合に呼ばれる。inner.Follow.actor がローカル follower、object が
// remote followee。inner.id が自ホストの follow-relay URI で、かつ送信元
// actor がその relay 自身なら relay 関連として RelayStatusMarker.MarkRejected
// を呼ぶ。
func (p *Processor) handleReject(act genericActivity) error {
	if p.quoteAnswers != nil {
		if reqURI := quoteAnswerRequestURI(act.Object, p.localBaseURL); reqURI != "" {
			return p.quoteAnswers.HandleAnswer(act.Actor, reqURI, false, "")
		}
	}
	var inner genericActivity
	if f, ok := p.localFollowFromID(act.Object, act.Actor); ok {
		// object が Follow の id だけの文字列 (#3491、handleAccept と同じ)。
		inner = f
	} else if err := unmarshalIgnoringTypeErrors(act.Object, &inner); err != nil {
		// 型エラーを握らないと、直後の normalizeActor (#999) が救うはずの
		// `inner.actor` が embedded object のケースに**到達できない** (#2662)。
		slog.Info("federation: dropping Reject with an unreadable object", "actor", act.Actor, "err", err)
		return nil
	} else {
		// inner.actor が embedded object のケースも救済する (#999 / upstream #17340)。
		inner.normalizeActor(act.Object)
	}
	// chat room invitation の Reject (remote が我々の room 招待を辞退) は
	// pending invitation を削除する (#1203)。
	if strings.EqualFold(inner.Type, "invite") {
		return p.handleChatRoomReject(act, inner)
	}
	if !strings.EqualFold(inner.Type, "follow") {
		return ErrUnsupportedActivity
	}
	if relayID := matchFollowRelayID(inner.ID, p.localBaseURL); relayID != "" && p.relayMarker != nil {
		// Accept 側と同じ理由で送信元を検証する。こちらを通すと稼働中の
		// relay 配送を任意の actor が無言で止められる。
		allowed, err := p.relayStatusChangeAllowed(act.Actor, relayID, "Reject")
		if err != nil {
			return err
		}
		if !allowed {
			return nil
		}
		return p.relayMarker.MarkRejected(context.Background(), relayID)
	}
	followee, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	followerURI, err := readActorString(inner)
	if err != nil {
		return err
	}
	// ローカルユーザーは user.uri が NULL なので、URI から ID を読んで引く
	// (userFromAPID)。これをやらないと FindByURI が fail して reject が silent
	// drop されてしまい、ローカル側の FollowRequest が消えずに永遠に pending の
	// ままになる。
	follower, err := p.userFromAPID(followerURI, inboundActorHost(act.Actor))
	if err != nil {
		// **Accept と同じ扱い** (#3115)。not-found は retry しても変わらないので
		// ack するが、**DB 障害を ack すると job が成功扱いになって retry されず**、
		// 直上のコメントが書いている「FollowRequest が永遠に pending」がそのまま
		// 起きる。Accept と Reject は同じ決定の裏表なので、片方だけ直さない。
		if repository.IsNotFound(err) {
			return nil
		}
		return err
	}
	// Reject されるのはこちらが送った Follow なので、follower はローカルの利用者の
	// はず。本家 rejectFollow と同じく、ローカルでなければ何もしない
	// (`skip: follower is not a local user`)。
	if !follower.IsLocal() {
		return nil
	}
	// 既存のフォローがあれば解除する。pending な follow request も同様。
	// #2106 N11: upstream remoteReject は AP 配送を伴わない内部削除なので、rejecter へ
	// 余計な Undo(Follow) を逆配送しない (Following 行が残るエッジケースでの連合ノイズ /
	// 相手の重複処理を防ぐ)。申請だけが残っていた場合も同じで、unfollow の通知
	// (main stream と Webhook) は本家と同じく 1 回だけ出す (#3330)。
	return p.followingService.RemoteReject(follower.ID, followee.ID)
}

// handleMove processes an inbound Move activity. アカウント移行通知を受けて
// リモートactorのプロフィールを強制再取得する。TTLキャッシュをバイパスして
// movedTo/alsoKnownAsの最新値を反映する。
func (p *Processor) handleMove(act genericActivity) error {
	// upstream move() は activity.target を getApHrefNullable で読み、無い/不正なら
	// 'skip: invalid activity target' で早期 return する。target が無い Move で
	// ForceResolveActor (TTL-bypass の強制 refetch) を発火させない (#1948-21)。
	// getApHrefNullable は string か object.href を読む (id ではない) ので、同じ
	// 受理集合になるよう readApHref を使う。
	var t struct {
		Target json.RawMessage `json:"target"`
	}
	if len(act.raw) > 0 {
		_ = json.Unmarshal(act.raw, &t)
	}
	if readApHref(t.Target) == "" {
		return nil // skip: invalid activity target
	}
	if _, err := p.resolver.ForceResolveActor(act.Actor); err != nil {
		return err
	}
	return nil
}

// localFollowFromID rebuilds the Follow an Accept / Reject refers to when its
// object is only the id of a Follow this instance sent: a relay follow
// (`/activities/follow-relay/<id>`) or a user follow (`/follows/<followerID>/
// <hash of the followee URI>`, URLBuilder.FollowURI). For a user follow the id
// must be the one this instance would have sent to actorURI, so a remote actor
// can only answer follows addressed to itself.
//
// 本家 ApResolverService.resolveLocal の `follows` と同じく、id から Follow を
// 組み直す。follower は id のパスから取り、その利用者がローカルに居るかは
// 呼び出し側の userFromAPID が確かめる。本家の形の id (`/follows/<申請の id>`、
// TS 版から移行した時点で申請中だったもの) は引けないので扱わない
// (docs/divergence/federation.md)。relay の id だけの Accept / Reject は本家では
// 解決に失敗するが、こちらは relayStatusChangeAllowed を通したうえで受ける。
func (p *Processor) localFollowFromID(object json.RawMessage, actorURI string) (genericActivity, bool) {
	if p.localBaseURL == "" {
		return genericActivity{}, false
	}
	var id string
	if json.Unmarshal(object, &id) != nil || id == "" {
		return genericActivity{}, false
	}
	if matchFollowRelayID(id, p.localBaseURL) != "" {
		return genericActivity{Type: "Follow", ID: id}, true
	}
	rest, ok := strings.CutPrefix(id, p.localBaseURL+"/follows/")
	if !ok {
		return genericActivity{}, false
	}
	followerID, _, ok := strings.Cut(rest, "/")
	if !ok || followerID == "" {
		return genericActivity{}, false
	}
	urls := activitypub.NewURLBuilder(p.localBaseURL)
	if urls.FollowURI(followerID, actorURI) != id {
		return genericActivity{}, false
	}
	return genericActivity{Type: "Follow", ID: id, Actor: urls.UserURI(followerID)}, true
}
