package moderatoractivity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	coreannouncement "github.com/elythia-network/elythia/internal/core/announcement"
	"github.com/elythia-network/elythia/internal/model"
)

type capturedMainEvent struct {
	userID    string
	eventType string
}

type fakeMainPublisher struct{ events []capturedMainEvent }

func (f *fakeMainPublisher) PublishMainEvent(userID, eventType string, _ any) {
	f.events = append(f.events, capturedMainEvent{userID, eventType})
}

// TestCheck_AllInactive_PublishesAnnouncementCreatedPerModerator wires the
// real core/announcement.Creator (the router.go wiring of #1606) and verifies
// the invitation-only switch emits one `announcementCreated` main-stream
// event per moderator, mirroring upstream AnnouncementService.create.
func TestCheck_AllInactive_PublishesAnnouncementCreatedPerModerator(t *testing.T) {
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-10 * 24 * time.Hour)
	meta := &fakeMeta{meta: &model.Meta{DisableRegistration: false}}
	mods := &fakeModerators{users: []*model.User{
		userWithActive("a", &old),
		userWithActive("b", &old),
	}}

	repo := &fakeAnnounce{}
	pub := &fakeMainPublisher{}
	// idGen=nil でも Creator は publish する (packed createdAt が空になるだけ)。
	creator := coreannouncement.NewCreator(repo, nil, pub, nil)
	s := NewService(mods, meta, &fakeProfiles{}, creator, &fakeWebhook{}, &fakeIDGen{}, nil)
	s.now = func() time.Time { return now }

	require.NoError(t, s.Check())
	require.Len(t, repo.created, 2, "moderator ごとに announcement を永続化する")
	require.Len(t, pub.events, 2, "moderator ごとに announcementCreated を publish する")
	got := map[string]string{}
	for _, ev := range pub.events {
		got[ev.userID] = ev.eventType
	}
	assert.Equal(t, map[string]string{
		"a": "announcementCreated",
		"b": "announcementCreated",
	}, got)
}
