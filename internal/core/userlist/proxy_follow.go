// Package userlist provides side-effect helpers for user list membership
// changes (#1704).
package userlist

import (
	"log/slog"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
)

// SystemAccountFetcher fetches a system account by kind (e.g. "proxy").
// 実装は core/systemaccount.Service。
type SystemAccountFetcher interface {
	Fetch(kind string) (*model.User, error)
}

// FollowEnqueuer enqueues per-pair follow jobs. 実装は queue.Client。
type FollowEnqueuer interface {
	EnqueueFollowBulk(payloads []queue.FollowPayload) error
}

// ProxyFollower makes the instance's "proxy" system account follow remote users
// added to a user list, so their federated posts reach this instance even when
// no local user follows them (upstream UserListService.addMember の
// isRemoteUser → createFollowJob、#1704)。
type ProxyFollower struct {
	systemAcct SystemAccountFetcher
	enqueuer   FollowEnqueuer
}

// NewProxyFollower constructs a ProxyFollower.
func NewProxyFollower(systemAcct SystemAccountFetcher, enqueuer FollowEnqueuer) *ProxyFollower {
	return &ProxyFollower{systemAcct: systemAcct, enqueuer: enqueuer}
}

// EnqueueProxyFollow makes the proxy account follow each remote user id. Best-
// effort: membership は既に永続化済みなので、失敗してもログのみで request は
// 失敗させない (upstream も createFollowJob を await せず投げっぱなし)。空 slice
// は no-op。
func (p *ProxyFollower) EnqueueProxyFollow(remoteUserIDs []string) {
	if len(remoteUserIDs) == 0 {
		return
	}
	proxy, err := p.systemAcct.Fetch("proxy")
	if err != nil {
		slog.Warn("userlist: fetch proxy account for remote follow failed", "err", err)
		return
	}
	payloads := make([]queue.FollowPayload, 0, len(remoteUserIDs))
	for _, id := range remoteUserIDs {
		payloads = append(payloads, queue.FollowPayload{FollowerID: proxy.ID, FolloweeID: id})
	}
	if err := p.enqueuer.EnqueueFollowBulk(payloads); err != nil {
		slog.Warn("userlist: enqueue proxy follow failed", "err", err)
	}
}
