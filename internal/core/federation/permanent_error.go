package federation

import (
	"errors"

	"github.com/elythia-network/elythia/internal/activitypub"
	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
)

// isPermanentSkipError reports whether err represents a permanent failure
// that the inbox processor should silently ack rather than feed back into
// the retry queue. callers (= Like / Undo / Announce handler 群) use this
// to choose between "best-effort skip" (= log info and return nil,
// swallowing the activity) and "transient failure" (= return err to put
// the activity back on the retry queue).
//
// 「resolve 経路の失敗」と「activity 処理の policy 違反」の両方を含むため
// 命名は resolve 限定にしていない (= reactionService.Create が返す
// `ErrNoteNotVisible` も同 helper で吸収する想定)。
//
// permanent と分類する error:
//
//   - HTTP 4xx (401 / 403 / 404 / 410) — `*activitypub.StatusError`
//     mk-go が follower で無い follower-only note / 削除済 / authorized
//     fetch 拒否 のいずれも、retry しても解消しない構造的失敗。
//   - `ErrInvalidActor` / `ErrInvalidNote` — 取得は成功したが対側が返した
//     JSON が parse 不能。schema 不正で retry でも直らない。
//   - `corereaction.ErrNoteNotVisible` — note 解決後の visibility 違反。
//     mk-go の policy 上 reaction を作れないので、activity 全体は ack
//     して終わる方が筋。
//   - `ErrHostNotAllowed` — federation policy (none / specified /
//     blockedHosts) で許可されない host からの fetch / ingest。policy は
//     retry では解消しないので ack して drop する (#1419 review)。
//
// transient と分類する (false 返す) error: 5xx / network error / timeout
// 等、対側の一過性障害。retry サイクルに乗せる。`nil` も false を返す
// (= 「成功」を「permanent fail」と誤判定しないため。
func isPermanentSkipError(err error) bool {
	if err == nil {
		return false
	}
	var statusErr *activitypub.StatusError
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case 401, 403, 404, 410:
			return true
		}
		return false
	}
	if errors.Is(err, ErrInvalidActor) || errors.Is(err, ErrInvalidNote) {
		return true
	}
	if errors.Is(err, corereaction.ErrNoteNotVisible) {
		return true
	}
	if errors.Is(err, ErrHostNotAllowed) {
		return true
	}
	return false
}
