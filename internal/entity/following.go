// Package entity: Following response shape (Misskey TS FollowingEntityService
// 互換)。/api/following/list (#17385 + #17416 / upstream 2026.5.2) や
// /api/users/{followers,following} で list element として使う。
package entity

import (
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// Following mirrors upstream Misskey TS FollowingEntityService.pack output.
// Both `followee` and `follower` are optional and only emitted when caller
// requests them via populate flags (= upstream `populateFollowee` /
// `populateFollower` options).
type Following struct {
	ID         string        `json:"id"`
	CreatedAt  string        `json:"createdAt"`
	FolloweeID string        `json:"followeeId"`
	FollowerID string        `json:"followerId"`
	Followee   *UserDetailed `json:"followee,omitempty"`
	Follower   *UserDetailed `json:"follower,omitempty"`
}

// UserProfileLookup resolves user_id → (User, UserProfile) for caller-supplied
// batch fetching. Returns (nil, nil) when the user is not found so the
// corresponding Followee / Follower field stays nil.
type UserProfileLookup func(userID string) (*model.User, *model.UserProfile)

// PackFollowing converts a model.Following row to entity.Following.
// `populateFollowee` / `populateFollower` flags determine whether the
// corresponding user object is embedded (packed via PackUserDetailed,
// matching upstream `UserDetailedNotMe` schema for opts.populateFollowee=true
// in /api/following/list).
//
// `lookup` is called only when a populate flag is true and lets the caller
// pre-batch user fetches to avoid N+1. Passing nil makes the function emit
// followee / follower as nil.
//
// `idGen` is used to derive `createdAt` from the time-based aidx ID. When nil
// or parse fails the field stays empty (= caller can still ship the rest).
func PackFollowing(
	f *model.Following,
	populateFollowee, populateFollower bool,
	lookup UserProfileLookup,
	idGen id.Generator,
) Following {
	out := Following{
		ID:         f.ID,
		FolloweeID: f.FolloweeID,
		FollowerID: f.FollowerID,
	}
	if idGen != nil {
		if t, err := idGen.ParseTime(f.ID); err == nil {
			// createdAt は他 packer と同じ固定 ms 精度 ISO-8601 で出す
			// (upstream JS toISOString 互換、#1556)。RFC3339Nano は精度が
			// 可変 (末尾ゼロ trim / 端数無し) になり drift するため使わない。
			out.CreatedAt = t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}
	if populateFollowee && lookup != nil {
		if u, p := lookup(f.FolloweeID); u != nil {
			d := PackUserDetailed(u, p, idGen)
			out.Followee = &d
		}
	}
	if populateFollower && lookup != nil {
		if u, p := lookup(f.FollowerID); u != nil {
			d := PackUserDetailed(u, p, idGen)
			out.Follower = &d
		}
	}
	return out
}
