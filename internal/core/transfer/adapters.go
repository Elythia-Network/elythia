package transfer

import (
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
)

// FollowingServiceAdapter wraps core/following.Service so that its Follow
// method matches the Importer's FollowingService interface (which returns
// `any` to avoid depending on the concrete FollowResult type).
type FollowingServiceAdapter struct {
	svc *corefollowing.Service
}

// NewFollowingServiceAdapter constructs a FollowingServiceAdapter.
func NewFollowingServiceAdapter(svc *corefollowing.Service) *FollowingServiceAdapter {
	return &FollowingServiceAdapter{svc: svc}
}

// Follow delegates to core/following.Service.Follow, bridging transfer's
// FollowOptions to corefollowing.FollowOptions. CSV import 側で parse された
// withReplies (#1058) を Service まで threading する。
func (a *FollowingServiceAdapter) Follow(followerID, followeeID string, opts FollowOptions) (any, error) {
	if a == nil || a.svc == nil {
		return nil, nil
	}
	return a.svc.Follow(followerID, followeeID, corefollowing.FollowOptions{
		WithReplies: opts.WithReplies,
		Silent:      opts.Silent,
	})
}
