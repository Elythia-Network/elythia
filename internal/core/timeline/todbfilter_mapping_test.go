package timeline

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// toDBFilter の field pass-through を網羅的に固定する regression guard。
//
// 既存の TestToDBFilter_UseMutingSubquery は subquery 切替 / MutedUserIDs の
// 意図的 drop / MutedChannelIDs / WithReplies を検証しているが、残りの
// pass-through field (WithFiles / WithRenotes / IncludeMyRenotes /
// IncludeRenotedMyNotes / IncludeLocalRenotes) と UseRenoteMutingSubquery は
// 未検証だった。これらは TimelineFilter から model.TimelineDBFilter へ素通しで
// 渡って SQL 経路の WHERE 条件を決めるため、conversion で取り落とすと timeline
// の filter が静かに無効化される。ここで全 field の伝搬を lock する。
func TestToDBFilter_AllFieldsMapped(t *testing.T) {
	withRenotes := false
	withReplies := true
	includeMyRenotes := false
	includeRenotedMyNotes := false
	includeLocalRenotes := false

	in := TimelineFilter{
		WithFiles:             true,
		WithRenotes:           &withRenotes,
		WithReplies:           &withReplies,
		IncludeMyRenotes:      &includeMyRenotes,
		IncludeRenotedMyNotes: &includeRenotedMyNotes,
		IncludeLocalRenotes:   &includeLocalRenotes,
		MutedChannelIDs:       []string{"ch1", "ch2"},
		FollowedChannelIDs:    []string{"chf1", "chf2"},
		LocalUsersOnly:        true,
	}

	out := toDBFilter(in, "viewer1")
	assert.True(t, out.LocalUsersOnly)

	assert.True(t, out.WithFiles)
	if assert.NotNil(t, out.WithRenotes) {
		assert.False(t, *out.WithRenotes)
	}
	if assert.NotNil(t, out.WithReplies) {
		assert.True(t, *out.WithReplies)
	}
	if assert.NotNil(t, out.IncludeMyRenotes) {
		assert.False(t, *out.IncludeMyRenotes)
	}
	if assert.NotNil(t, out.IncludeRenotedMyNotes) {
		assert.False(t, *out.IncludeRenotedMyNotes)
	}
	if assert.NotNil(t, out.IncludeLocalRenotes) {
		assert.False(t, *out.IncludeLocalRenotes)
	}
	assert.Equal(t, []string{"ch1", "ch2"}, out.MutedChannelIDs)
	assert.Equal(t, []string{"chf1", "chf2"}, out.FollowedChannelIDs)

	// viewerID 有なら mute / renote-mute の両 subquery が有効化される。
	assert.True(t, out.UseMutingSubquery)
	assert.True(t, out.UseRenoteMutingSubquery)
}

// anonymous viewer (viewerID == "") では renote-mute subquery も off になる。
// 既存テストは UseMutingSubquery のみ確認していたので renote 側を補完する。
func TestToDBFilter_AnonymousDisablesRenoteMutingSubquery(t *testing.T) {
	out := toDBFilter(TimelineFilter{}, "")
	assert.False(t, out.UseMutingSubquery)
	assert.False(t, out.UseRenoteMutingSubquery)
}

// nil pointer の field は nil のまま伝搬する (= repository 側で default 解釈に
// 委ねる)。zero-value の TimelineFilter を渡したときの shape を固定する。
func TestToDBFilter_NilPointersPreserved(t *testing.T) {
	out := toDBFilter(TimelineFilter{}, "viewer1")
	assert.False(t, out.WithFiles)
	assert.Nil(t, out.WithRenotes)
	assert.Nil(t, out.WithReplies)
	assert.Nil(t, out.IncludeMyRenotes)
	assert.Nil(t, out.IncludeRenotedMyNotes)
	assert.Nil(t, out.IncludeLocalRenotes)
	assert.Empty(t, out.MutedChannelIDs)
}
