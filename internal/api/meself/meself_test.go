package meself

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func newUser(id string) *model.User {
	return &model.User{ID: id, Username: id, AvatarDecorations: datatypes.JSON([]byte("[]"))}
}

func packed(u *model.User) entity.UserDetailed {
	return entity.PackUserDetailed(u, nil)
}

type recordingEnricher struct {
	called  int
	lastID  string
	inject  map[string]any
	profile *model.UserProfile
}

func (e *recordingEnricher) EnrichSelf(_ context.Context, u *model.User, profile *model.UserProfile, resp map[string]any) {
	e.called++
	e.lastID = u.ID
	e.profile = profile
	for k, v := range e.inject {
		resp[k] = v
	}
}

func TestIsSelf(t *testing.T) {
	me := newUser("u1")
	assert.True(t, IsSelf(me, "u1"))
	assert.False(t, IsSelf(me, "u2"))
	assert.False(t, IsSelf(nil, "u1"), "anonymous viewer is never self")
}

func TestPack_NotSelfReturnsUserDetailedAsIs(t *testing.T) {
	t.Cleanup(func() { SetEnricher(nil) })
	enricher := &recordingEnricher{}
	SetEnricher(enricher)

	target := newUser("u2")
	got := Pack(context.Background(), packed(target), target, nil, newUser("u1"))

	d, ok := got.(entity.UserDetailed)
	require.True(t, ok, "non-self must stay a UserDetailed, got %T", got)
	assert.Equal(t, "u2", d.ID)
	assert.Zero(t, enricher.called, "enricher must not run for other users")
}

func TestPack_AnonymousViewer(t *testing.T) {
	target := newUser("u2")
	_, ok := Pack(context.Background(), packed(target), target, nil, nil).(entity.UserDetailed)
	assert.True(t, ok, "anonymous viewer gets the plain UserDetailed")
}

func TestPack_SelfPromotesToMeDetailed(t *testing.T) {
	t.Cleanup(func() { SetEnricher(nil) })
	SetEnricher(nil)

	me := newUser("u1")
	got := Pack(context.Background(), packed(me), me, nil, me)

	resp, ok := got.(map[string]any)
	require.True(t, ok, "self must be promoted to a MeDetailed map, got %T", got)
	assert.Equal(t, "u1", resp["id"])
	// MeDetailed only fields.
	assert.Contains(t, resp, "isExplorable")
	assert.Contains(t, resp, "hasUnreadNotification")
	assert.Contains(t, resp, "avatarId")
}

// Enricher 未配線でも policies は非 null で出す (misskey_dart の
// MeDetailed.fromJson が Map を要求する、#1240)。
func TestPackMe_PoliciesDefaultedWithoutEnricher(t *testing.T) {
	t.Cleanup(func() { SetEnricher(nil) })
	SetEnricher(nil)

	me := newUser("u1")
	resp, ok := PackMe(context.Background(), packed(me), me, nil).(map[string]any)
	require.True(t, ok)
	policies, ok := resp["policies"].(map[string]any)
	require.True(t, ok, "policies must be a non-null object")
	assert.NotEmpty(t, policies)
}

func TestPackMe_EnricherOverridesDefaults(t *testing.T) {
	t.Cleanup(func() { SetEnricher(nil) })
	enricher := &recordingEnricher{inject: map[string]any{
		"policies":                 map[string]any{"driveCapacityMb": 999},
		"isAdmin":                  true,
		"unreadNotificationsCount": 3,
	}}
	SetEnricher(enricher)

	me := newUser("u1")
	profile := &model.UserProfile{UserID: "u1"}
	resp, ok := PackMe(context.Background(), packed(me), me, profile).(map[string]any)
	require.True(t, ok)

	assert.Equal(t, 1, enricher.called)
	assert.Equal(t, "u1", enricher.lastID)
	assert.Same(t, profile, enricher.profile, "profile is handed through untouched")
	assert.Equal(t, map[string]any{"driveCapacityMb": 999}, resp["policies"])
	assert.Equal(t, true, resp["isAdmin"])
	assert.Equal(t, 3, resp["unreadNotificationsCount"])
}

func TestSetEnricher_NilDisablesEnrichment(t *testing.T) {
	t.Cleanup(func() { SetEnricher(nil) })
	enricher := &recordingEnricher{}
	SetEnricher(enricher)
	SetEnricher(nil)

	me := newUser("u1")
	_ = PackMe(context.Background(), packed(me), me, nil)
	assert.Zero(t, enricher.called)
}
