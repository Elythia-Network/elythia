package reversi

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/elythia-network/elythia/internal/model"
)

// Elythia: 相手を指定した招待が受けられたことを、招待した人に知らせる (#3946)。
//
// 本家は招待を Redis にだけ置き、受けた時点で対局の行を作って、招待した人の
// reversi stream に `matched` を流す (ReversiService.matched)。招待した人の
// reversi/match も、それ以降は作られた対局を返す。Elythia は連合のために招待の
// 時点で行を作るので、行の有無では受けられたかが分からない。受けた印を対局ごとに
// 持ち、`matched` と reversi/match の応答をそれで決める。

// MatchedNotifier tells a local inviter that their invitation was accepted:
// the `matched` event of the reversi stream. 対局を本家の packDetail の形で
// 包むのは API 側 (api/reversi.Handler) なので、core からは interface で呼ぶ。
type MatchedNotifier interface {
	ReversiMatched(inviterID string, game *model.ReversiGame)
}

// SetMatchedNotifier sets who tells inviters about accepted invitations (#3946).
func (s *Service) SetMatchedNotifier(n MatchedNotifier) {
	s.matched = n
}

// acceptedTTL is how long the accepted mark lives.
//
// 未開始の対局は clean のジョブ (1 日 1 回、作られてから 10 分を過ぎたもの) が
// 消すまで残る。印はその間ずっと持つ。途中で切れると、招待された側の画面が
// 10 秒ごとに呼ぶ reversi/show-game で印が付き直り、招待した人に知らせ直してしまう。
const acceptedTTL = 25 * time.Hour

// inviterWaitingTTL is how long the inviter counts as waiting for the game after
// their last reversi/match. 本家の招待の期限 (INVITATION_TIMEOUT_MS、20 秒) と同じ。
// 画面は 5 秒ごとに呼び直す。
const inviterWaitingTTL = 20 * time.Second

func acceptedKey(gameID string) string {
	return "reversi:game:accepted:" + gameID
}

func inviterWaitingKey(gameID string) string {
	return "reversi:game:inviterWaiting:" + gameID
}

// MarkInviterWaiting records that the inviter is waiting for the game right now
// (their reversi/match for the invitee).
//
// 本家の招待は、招待した人の heartbeat が途切れると 20 秒で消え、受けられなくなる。
// Elythia の招待は対局の行なので、招待した人が去った後も (cancel-match が届かな
// かったときなど) 受けられる。待っていない人に matched を流すと、別の相手を待って
// いる画面がその対局へ移ってしまうので、待っている間だけ知らせる。
func (s *Service) MarkInviterWaiting(ctx context.Context, gameID string) {
	if s.redis == nil {
		return
	}
	if err := s.redis.Set(ctx, inviterWaitingKey(gameID), "1", inviterWaitingTTL).Err(); err != nil {
		slog.Warn("reversi: mark inviter waiting failed", "gameId", gameID, "err", err)
	}
}

// AcceptInvitation records that the invitee accepted the pending game and, the
// first time, tells the inviter (User1) if they are waiting for it.
//
// 受ける経路 (reversi/match、招待された側の reversi/show-game、連合の Join) は
// 何度も通りうるので、知らせるのは印を初めて付けたときだけにする。Redis が
// 使えないときは知らせない (印が無いので、招待された側の画面が呼ぶたびに知らせて
// しまう)。招待した人は従来どおり「自分の対局」から開ける。
func (s *Service) AcceptInvitation(ctx context.Context, game *model.ReversiGame) {
	if game == nil || game.IsStarted || game.IsEnded || s.redis == nil {
		return
	}
	// 値は受けた時刻 (InvitationAcceptedRecently が読む)
	first, err := s.redis.SetNX(ctx, acceptedKey(game.ID), time.Now().UnixMilli(), acceptedTTL).Result()
	if err != nil {
		slog.Warn("reversi: mark invitation accepted failed", "gameId", game.ID, "err", err)
		return
	}
	if !first || s.matched == nil {
		return
	}
	waiting, err := s.redis.Exists(ctx, inviterWaitingKey(game.ID)).Result()
	if err != nil {
		slog.Warn("reversi: read inviter waiting failed", "gameId", game.ID, "err", err)
		return
	}
	if waiting > 0 {
		s.matched.ReversiMatched(game.User1ID, game)
	}
}

// acceptedReturnWindow is how long after the acceptance the inviter's
// reversi/match returns the accepted game. 本家は 3 分以内に作られた対局を返し
// (ReversiService.ts:103)、対局は受けた時点で作られるので、受けてから 3 分になる。
// 長くすると、受けられたまま放置された対局があるときに同じ相手を招待し直しても、
// 古い対局が返って相手に招待が届かない (レビューで実測)。
const acceptedReturnWindow = 3 * time.Minute

// InvitationAcceptedRecently reports whether the invitee accepted the pending
// game within acceptedReturnWindow. Redis が使えないときは受けられていない扱いに
// する (従来どおり 204 を返す)。
func (s *Service) InvitationAcceptedRecently(ctx context.Context, gameID string) bool {
	if s.redis == nil {
		return false
	}
	v, err := s.redis.Get(ctx, acceptedKey(gameID)).Int64()
	if errors.Is(err, redis.Nil) {
		return false
	}
	if err != nil {
		slog.Warn("reversi: read invitation accepted failed", "gameId", gameID, "err", err)
		return false
	}
	return time.Since(time.UnixMilli(v)) < acceptedReturnWindow
}

// WasInvitationAccepted reports whether the invitee has accepted the pending
// game at any time (#3949): the accepted mark, or the invitee already being
// ready.
//
// 招待した人の reversi/cancel-match は、受けられた対局を取り消さない。本家の
// matchSpecificUserCancel は Redis の招待を消すだけで、受けた時点で作られた対局には
// 触らないため。準備の状態は DB にあるので、Redis が使えないときや受けた印が無い
// ときの手がかりにする。どちらも無ければ、受けられていない招待として扱う。
func (s *Service) WasInvitationAccepted(ctx context.Context, game *model.ReversiGame) bool {
	if game.User2Ready {
		return true
	}
	if s.redis == nil {
		return false
	}
	n, err := s.redis.Exists(ctx, acceptedKey(game.ID)).Result()
	if err != nil {
		slog.Warn("reversi: read invitation accepted failed", "gameId", game.ID, "err", err)
		return false
	}
	return n > 0
}

// AcceptedLongAgo reports whether the game was accepted more than
// acceptedReturnWindow ago, so it no longer counts as an invitation (#3949).
//
// 受けた後に招待した人が去っても対局は残す (cancel-match で消さない)。本家は受けた
// 時点で招待を消し、作られてから 3 分の対局だけを返すので、それを過ぎた対局を
// 招待として拾うと、招待された側の逆向きの招待やランダムマッチが、相手のいない
// 古い対局へ飛ばされる。連合で同じ相手から届いた招待も、この対局を使い回さずに
// 新しい対局にする。受けてから 3 分の間は、本家と同じく対局を返す
//
// 受けた印 (受けた時刻) があるときだけ真にする。準備の状態 (User2Ready) だけでは
// 受けた時刻が分からないので、取り消さない判定 (WasInvitationAccepted) にだけ使う
func (s *Service) AcceptedLongAgo(ctx context.Context, game *model.ReversiGame) bool {
	if s.redis == nil {
		return false
	}
	v, err := s.redis.Get(ctx, acceptedKey(game.ID)).Int64()
	if err != nil {
		return false
	}
	return time.Since(time.UnixMilli(v)) >= acceptedReturnWindow
}
