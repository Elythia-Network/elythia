package achievement

import (
	"context"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

type stubProfiles struct {
	profiles  map[string]*model.UserProfile
	updates   []map[string]any
	updateErr error
}

func (s *stubProfiles) GetProfile(userID string) *model.UserProfile {
	return s.profiles[userID]
}

func (s *stubProfiles) UpdateProfileFields(userID string, fields map[string]any) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	s.updates = append(s.updates, fields)
	// 永続化を模倣して in-memory profile にも反映する (後続 Grant の冪等性確認用)。
	if p := s.profiles[userID]; p != nil {
		if v, ok := fields["achievements"].(string); ok {
			p.Achievements = datatypes.JSON(v)
		}
	}
	return nil
}

type stubNotifier struct {
	calls []notification.CreateInput
	err   error
}

func (s *stubNotifier) Create(_ context.Context, in notification.CreateInput) (*notification.Notification, error) {
	s.calls = append(s.calls, in)
	if s.err != nil {
		return nil, s.err
	}
	return &notification.Notification{}, nil
}

func TestGrant_New(t *testing.T) {
	profiles := &stubProfiles{profiles: map[string]*model.UserProfile{"u1": {UserID: "u1"}}}
	notifier := &stubNotifier{}
	svc := NewService(profiles, notifier)

	granted, err := svc.Grant(context.Background(), "u1", MyNoteFavorited1)
	require.NoError(t, err)
	assert.True(t, granted)
	require.Len(t, profiles.updates, 1)
	assert.Contains(t, profiles.updates[0]["achievements"], MyNoteFavorited1)
	// notification は achievementEarned + Extra.achievement = type で 1 回。
	require.Len(t, notifier.calls, 1)
	assert.Equal(t, "u1", notifier.calls[0].NotifieeID)
	assert.Equal(t, notification.TypeAchievementEarned, notifier.calls[0].Type)
	assert.Equal(t, MyNoteFavorited1, notifier.calls[0].Extra["achievement"])
}

func TestGrant_Duplicate(t *testing.T) {
	profiles := &stubProfiles{profiles: map[string]*model.UserProfile{
		"u1": {UserID: "u1", Achievements: datatypes.JSON(`[{"name":"myNoteFavorited1","unlockedAt":1000}]`)},
	}}
	notifier := &stubNotifier{}
	svc := NewService(profiles, notifier)

	granted, err := svc.Grant(context.Background(), "u1", MyNoteFavorited1)
	require.NoError(t, err)
	assert.False(t, granted, "既獲得なら no-op")
	assert.Empty(t, profiles.updates)
	assert.Empty(t, notifier.calls)
}

func TestGrant_Idempotent_SecondCallNoOp(t *testing.T) {
	profiles := &stubProfiles{profiles: map[string]*model.UserProfile{"u1": {UserID: "u1"}}}
	svc := NewService(profiles, &stubNotifier{})

	g1, _ := svc.Grant(context.Background(), "u1", MyNoteFavorited1)
	g2, _ := svc.Grant(context.Background(), "u1", MyNoteFavorited1)
	assert.True(t, g1)
	assert.False(t, g2, "2 回目は永続化済みなので no-op")
	assert.Len(t, profiles.updates, 1)
}

func TestGrant_UnknownType(t *testing.T) {
	profiles := &stubProfiles{profiles: map[string]*model.UserProfile{"u1": {UserID: "u1"}}}
	notifier := &stubNotifier{}
	svc := NewService(profiles, notifier)

	granted, err := svc.Grant(context.Background(), "u1", "bogusAchievement")
	require.NoError(t, err)
	assert.False(t, granted)
	assert.Empty(t, profiles.updates)
	assert.Empty(t, notifier.calls)
}

func TestGrant_NoProfile(t *testing.T) {
	profiles := &stubProfiles{profiles: map[string]*model.UserProfile{}}
	svc := NewService(profiles, &stubNotifier{})

	granted, err := svc.Grant(context.Background(), "ghost", MyNoteFavorited1)
	require.NoError(t, err)
	assert.False(t, granted)
	assert.Empty(t, profiles.updates)
}

func TestGrant_UpdateError(t *testing.T) {
	profiles := &stubProfiles{
		profiles:  map[string]*model.UserProfile{"u1": {UserID: "u1"}},
		updateErr: errors.New("db down"),
	}
	notifier := &stubNotifier{}
	svc := NewService(profiles, notifier)

	granted, err := svc.Grant(context.Background(), "u1", MyNoteFavorited1)
	assert.Error(t, err)
	assert.False(t, granted)
	assert.Empty(t, notifier.calls, "永続化失敗時は通知しない")
}

func TestGrant_NilNotifier(t *testing.T) {
	profiles := &stubProfiles{profiles: map[string]*model.UserProfile{"u1": {UserID: "u1"}}}
	svc := NewService(profiles, nil)

	granted, err := svc.Grant(context.Background(), "u1", MyNoteFavorited1)
	require.NoError(t, err)
	assert.True(t, granted, "notifier nil でも付与は永続化される")
	assert.Len(t, profiles.updates, 1)
}

func TestGrant_NotificationError_StillGrants(t *testing.T) {
	profiles := &stubProfiles{profiles: map[string]*model.UserProfile{"u1": {UserID: "u1"}}}
	notifier := &stubNotifier{err: errors.New("notify fail")}
	svc := NewService(profiles, notifier)

	granted, err := svc.Grant(context.Background(), "u1", MyNoteFavorited1)
	require.NoError(t, err, "通知失敗は grant を巻き戻さない")
	assert.True(t, granted)
	assert.Len(t, profiles.updates, 1)
	assert.Len(t, notifier.calls, 1)
}
