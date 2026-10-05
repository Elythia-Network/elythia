package charts

import (
	"github.com/elythia-network/elythia/internal/core/chart"
	"github.com/elythia-network/elythia/internal/model"
)

// SchemaPerUserFollowing returns the schema for the per-user
// "perUserFollowing" chart. Each user has 12 columns spread across the
// local/remote × followings/followers × total/inc/dec axes.
func SchemaPerUserFollowing() chart.Schema {
	return chart.Schema{
		Name:    "perUserFollowing",
		Grouped: true,
		Columns: []chart.ColumnDef{
			{Name: "local.followings.total", Accumulate: true},
			{Name: "local.followings.inc", Range: chart.RangeSmall},
			{Name: "local.followings.dec", Range: chart.RangeSmall},
			{Name: "local.followers.total", Accumulate: true},
			{Name: "local.followers.inc", Range: chart.RangeSmall},
			{Name: "local.followers.dec", Range: chart.RangeSmall},
			{Name: "remote.followings.total", Accumulate: true},
			{Name: "remote.followings.inc", Range: chart.RangeSmall},
			{Name: "remote.followings.dec", Range: chart.RangeSmall},
			{Name: "remote.followers.total", Accumulate: true},
			{Name: "remote.followers.inc", Range: chart.RangeSmall},
			{Name: "remote.followers.dec", Range: chart.RangeSmall},
		},
	}
}

// PerUserFollowingChart aggregates per-user follow / unfollow events.
// Each Update commits two diffs: one against the follower's bucket and
// one against the followee's bucket. The local/remote prefix on each
// side is determined by the *other* user's host (mirroring upstream).
type PerUserFollowingChart struct {
	c *chart.Chart
}

// NewPerUserFollowingChart wraps an engine instance built with
// SchemaPerUserFollowing.
func NewPerUserFollowingChart(c *chart.Chart) *PerUserFollowingChart {
	return &PerUserFollowingChart{c: c}
}

// Chart returns the underlying engine pointer.
func (p *PerUserFollowingChart) Chart() *chart.Chart { return p.c }

// Update commits one follow / unfollow event. Both follower and
// followee receive a diff under their own user id. The follower's
// "followings.*" columns and the followee's "followers.*" columns are
// updated; the local/remote prefix is chosen by each side's own
// `host` value, matching `UserEntityService.isLocalUser` upstream.
//
// 本家の `PerUserFollowingChart.update` は `prefixFollower` を follower
// 自身の host で決め、`prefixFollowee` も followee 自身の host で決める。
// (TS版コメントとは逆で、ロジックは self-host 基準) — 実コードに合わせる。
func (p *PerUserFollowingChart) Update(follower, followee *model.User, isFollow bool) error {
	sign := int64(1)
	if !isFollow {
		sign = -1
	}
	incVal := int64(0)
	decVal := int64(0)
	if isFollow {
		incVal = 1
	} else {
		decVal = 1
	}
	prefixFollower := userPrefix(follower)
	prefixFollowee := userPrefix(followee)
	if err := p.c.Commit(chart.Diff{
		prefixFollower + ".followings.total": sign,
		prefixFollower + ".followings.inc":   incVal,
		prefixFollower + ".followings.dec":   decVal,
	}, follower.ID); err != nil {
		return err
	}
	return p.c.Commit(chart.Diff{
		prefixFollowee + ".followers.total": sign,
		prefixFollowee + ".followers.inc":   incVal,
		prefixFollowee + ".followers.dec":   decVal,
	}, followee.ID)
}

// userPrefix returns "local" or "remote" depending on whether the
// user's host field is empty. Used by per-user-following and other
// charts that need to partition columns on locality.
func userPrefix(u *model.User) string {
	if u == nil || u.Host == nil || *u.Host == "" {
		return "local"
	}
	return "remote"
}
