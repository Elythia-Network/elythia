package entity

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubRolesLookup implements UserRolesLookup for tests.
type stubRolesLookup struct {
	roles map[string][]*model.Role
}

func (s *stubRolesLookup) LookupUserRoles(userID string) []*model.Role {
	return s.roles[userID]
}

// resolveUserRolesSorted unwired → nil (= existing pre-#1103 behaviour).
func TestResolveUserRolesSorted_LookupUnwired(t *testing.T) {
	t.Cleanup(func() { SetUserRolesLookup(nil) })
	SetUserRolesLookup(nil)
	got := resolveUserRolesSorted("u1")
	assert.Nil(t, got)
}

// resolveUserRolesSorted sorts by displayOrder descending (= upstream
// `b.displayOrder - a.displayOrder`)。共有 cache を mutate しないこと。
func TestResolveUserRolesSorted_SortDescending(t *testing.T) {
	t.Cleanup(func() { SetUserRolesLookup(nil) })
	src := []*model.Role{
		{ID: "low", DisplayOrder: 1},
		{ID: "high", DisplayOrder: 10},
		{ID: "mid", DisplayOrder: 5},
	}
	SetUserRolesLookup(&stubRolesLookup{roles: map[string][]*model.Role{"u1": src}})
	got := resolveUserRolesSorted("u1")
	require.Len(t, got, 3)
	assert.Equal(t, "high", got[0].ID)
	assert.Equal(t, "mid", got[1].ID)
	assert.Equal(t, "low", got[2].ID)
	// 元 slice 順は保たれる (= 共有 cache が mutate されていない)。
	assert.Equal(t, "low", src[0].ID)
	assert.Equal(t, "high", src[1].ID)
	assert.Equal(t, "mid", src[2].ID)
}

// packPublicRoles: isPublic=true のみが出力に含まれる。
func TestPackPublicRoles_FiltersIsPublic(t *testing.T) {
	t.Cleanup(func() { SetUserRolesLookup(nil) })
	color := "#abc"
	icon := "https://example.com/i.png"
	SetUserRolesLookup(&stubRolesLookup{roles: map[string][]*model.Role{
		"u1": {
			{ID: "rA", Name: "Public", IsPublic: true, DisplayOrder: 2, Color: &color, IconURL: &icon, Description: "d", IsModerator: false, IsAdministrator: true},
			{ID: "rB", Name: "Hidden", IsPublic: false, DisplayOrder: 1},
			{ID: "rC", Name: "AlsoPublic", IsPublic: true, DisplayOrder: 5},
		},
	}})
	got := packPublicRoles(&model.User{ID: "u1"})
	require.Len(t, got, 2)
	// displayOrder desc で並ぶ: rC (5) → rA (2)
	got0 := got[0].(map[string]any)
	assert.Equal(t, "rC", got0["id"])
	got1 := got[1].(map[string]any)
	assert.Equal(t, "rA", got1["id"])
	assert.Equal(t, "Public", got1["name"])
	assert.Equal(t, &color, got1["color"])
	assert.Equal(t, &icon, got1["iconUrl"])
	assert.Equal(t, "d", got1["description"])
	assert.Equal(t, false, got1["isModerator"])
	assert.Equal(t, true, got1["isAdministrator"])
	assert.Equal(t, 2, got1["displayOrder"])
}

// packPublicRoles: ロールがゼロなら空配列を返す (nil ではなく []any{})。
func TestPackPublicRoles_EmptyReturnsEmptySlice(t *testing.T) {
	t.Cleanup(func() { SetUserRolesLookup(nil) })
	SetUserRolesLookup(nil) // unwired
	got := packPublicRoles(&model.User{ID: "u1"})
	assert.NotNil(t, got, "must return [] not nil so JSON output is `[]`")
	assert.Empty(t, got)
}

// packBadgeRoles: asBadge && isPublic のみ含まれ、displayOrder desc 順。
func TestPackBadgeRoles_FiltersAsBadgeAndIsPublic(t *testing.T) {
	t.Cleanup(func() { SetUserRolesLookup(nil) })
	icon := "https://example.com/badge.png"
	SetUserRolesLookup(&stubRolesLookup{roles: map[string][]*model.Role{
		"u1": {
			{ID: "r1", Name: "Badge1", AsBadge: true, IsPublic: true, DisplayOrder: 3, IconURL: &icon},
			{ID: "r2", Name: "NotBadge", AsBadge: false, IsPublic: true, DisplayOrder: 5},
			{ID: "r3", Name: "BadgePrivate", AsBadge: true, IsPublic: false, DisplayOrder: 4},
			{ID: "r4", Name: "Badge2", AsBadge: true, IsPublic: true, DisplayOrder: 10},
		},
	}})
	gotPtr := packBadgeRoles(&model.User{ID: "u1"})
	require.NotNil(t, gotPtr)
	got := *gotPtr
	require.Len(t, got, 2)
	// displayOrder desc: r4 (10) → r1 (3)
	got0 := got[0].(map[string]any)
	assert.Equal(t, "Badge2", got0["name"])
	got1 := got[1].(map[string]any)
	assert.Equal(t, "Badge1", got1["name"])
	assert.Equal(t, &icon, got1["iconUrl"])
	assert.Equal(t, 3, got1["displayOrder"])
}

// packBadgeRoles: remote user は meta.showRoleBadgesOfRemoteUsers が off なら
// nil を返し (= upstream undefined / JSON 省略)、on なら badge を pack する (#1653)。
func TestPackBadgeRoles_RemoteUserGatedByMetaFlag(t *testing.T) {
	t.Cleanup(func() { SetUserRolesLookup(nil); SetShowRemoteBadgesLookup(nil) })
	SetUserRolesLookup(&stubRolesLookup{roles: map[string][]*model.Role{
		"u1": {{ID: "r1", Name: "Badge", AsBadge: true, IsPublic: true}},
	}})
	host := "remote.example"
	remote := &model.User{ID: "u1", Host: &host}

	// flag 未配線 (= off) → nil (omit)
	assert.Nil(t, packBadgeRoles(remote))

	// flag off → nil (omit)
	SetShowRemoteBadgesLookup(func() bool { return false })
	assert.Nil(t, packBadgeRoles(remote))

	// flag on → badge を pack
	SetShowRemoteBadgesLookup(func() bool { return true })
	got := packBadgeRoles(remote)
	require.NotNil(t, got)
	require.Len(t, *got, 1)
	assert.Equal(t, "Badge", (*got)[0].(map[string]any)["name"])
}

// packBadgeRoles: local user はロールがゼロでも空配列 present (&[]any{}) を返す。
func TestPackBadgeRoles_LocalEmptyReturnsPresentEmpty(t *testing.T) {
	t.Cleanup(func() { SetUserRolesLookup(nil) })
	SetUserRolesLookup(nil)
	got := packBadgeRoles(&model.User{ID: "u1"})
	require.NotNil(t, got, "local user は badge が無くても present (空配列)")
	assert.Empty(t, *got)
}

// packBadgeRoles: nil user は nil (omit)、packPublicRoles は空配列。
func TestPackRoles_NilUserReturnsEmpty(t *testing.T) {
	t.Cleanup(func() { SetUserRolesLookup(nil) })
	assert.Nil(t, packBadgeRoles(nil))
	assert.Empty(t, packPublicRoles(nil))
}

// 報告経路 (#1103) の end-to-end guard: PackUserDetailed が public role を
// `roles` field に含めることを assert。旧 mk-go は空配列固定だったため
// admin が公開ロールを assign しても profile に表示されなかった。
func TestPackUserDetailed_IncludesPublicRoles(t *testing.T) {
	t.Cleanup(func() { SetUserRolesLookup(nil) })
	SetUserRolesLookup(&stubRolesLookup{roles: map[string][]*model.Role{
		"u1": {
			{ID: "rPub", Name: "Verified", IsPublic: true, DisplayOrder: 10},
			{ID: "rPriv", Name: "Hidden", IsPublic: false, DisplayOrder: 5},
		},
	}})
	d := PackUserDetailed(&model.User{ID: "u1", Username: "alice"}, nil)
	require.Len(t, d.Roles, 1, "public role should appear, private role should not")
	r := d.Roles[0].(map[string]any)
	assert.Equal(t, "rPub", r["id"])
	assert.Equal(t, "Verified", r["name"])
}

// 報告経路の end-to-end guard (badge 側): PackUserLite が asBadge && isPublic
// な role を `badgeRoles` field に含めること。
func TestPackUserLite_IncludesBadgeRoles(t *testing.T) {
	t.Cleanup(func() { SetUserRolesLookup(nil) })
	icon := "https://example.com/badge.png"
	SetUserRolesLookup(&stubRolesLookup{roles: map[string][]*model.Role{
		"u1": {
			{ID: "r1", Name: "Cool", AsBadge: true, IsPublic: true, DisplayOrder: 7, IconURL: &icon},
		},
	}})
	lite := PackUserLite(&model.User{ID: "u1", Username: "alice"})
	require.NotNil(t, lite.BadgeRoles)
	require.Len(t, *lite.BadgeRoles, 1)
	br := (*lite.BadgeRoles)[0].(map[string]any)
	assert.Equal(t, "Cool", br["name"])
	assert.Equal(t, &icon, br["iconUrl"])
}

// PackUserDetailed: lookup 未配線時は `roles: []` で出力する (= 旧 mk-go
// と同じ shape で JSON serialise 時に `[]` になる)。報告経路の end-to-end
// fallback として、本 PR の wire が万一 router から外れても frontend が
// `null` で壊れず空配列で安全に描画できることを担保する。
func TestPackUserDetailed_RolesEmptyWhenLookupUnwired(t *testing.T) {
	t.Cleanup(func() { SetUserRolesLookup(nil) })
	SetUserRolesLookup(nil)
	d := PackUserDetailed(&model.User{ID: "u1", Username: "alice"}, nil)
	assert.NotNil(t, d.Roles, "Roles must be [] not nil so JSON output is `[]`")
	assert.Empty(t, d.Roles)
}

// PackUserLite: 同上、lookup 未配線時の `badgeRoles: []` fallback を guard。
func TestPackUserLite_BadgeRolesEmptyWhenLookupUnwired(t *testing.T) {
	t.Cleanup(func() { SetUserRolesLookup(nil) })
	SetUserRolesLookup(nil)
	lite := PackUserLite(&model.User{ID: "u1", Username: "alice"})
	assert.NotNil(t, lite.BadgeRoles)
	assert.Empty(t, lite.BadgeRoles)
}
