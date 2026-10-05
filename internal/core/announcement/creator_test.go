package announcement

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

type fakeRepo struct {
	created []*model.Announcement
	err     error
}

func (f *fakeRepo) Create(a *model.Announcement) error {
	if f.err != nil {
		return f.err
	}
	f.created = append(f.created, a)
	return nil
}

type mainEvent struct {
	userID    string
	eventType string
	body      any
}

type fakePublisher struct{ events []mainEvent }

func (f *fakePublisher) PublishMainEvent(userID, eventType string, body any) {
	f.events = append(f.events, mainEvent{userID, eventType, body})
}

type broadcastEvent struct {
	eventType string
	body      any
}

type fakeBroadcast struct{ events []broadcastEvent }

func (f *fakeBroadcast) PublishBroadcast(eventType string, body any) {
	f.events = append(f.events, broadcastEvent{eventType, body})
}

func ptr[T any](v T) *T { return &v }

func newAnnouncement(t *testing.T, userID *string) (*model.Announcement, id.Generator) {
	t.Helper()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return &model.Announcement{
		ID:                     idGen.Generate(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)),
		Title:                  "Change to Invitation-Only",
		Text:                   "body",
		Icon:                   "info",
		Display:                "normal",
		IsActive:               true,
		ForExistingUsers:       true,
		NeedConfirmationToRead: true,
		UserID:                 userID,
	}, idGen
}

func TestCreate_PerUserPublishesAnnouncementCreated(t *testing.T) {
	a, idGen := newAnnouncement(t, ptr("mod1"))
	repo := &fakeRepo{}
	pub := &fakePublisher{}
	c := NewCreator(repo, idGen, pub, nil)

	require.NoError(t, c.Create(a))
	require.Len(t, repo.created, 1)
	require.Len(t, pub.events, 1)
	ev := pub.events[0]
	assert.Equal(t, "mod1", ev.userID)
	assert.Equal(t, "announcementCreated", ev.eventType)
	// upstream AnnouncementService.create の event body shape
	// ({"announcement": packed}) と一致させる。
	body, ok := ev.body.(map[string]any)
	require.True(t, ok)
	packed, ok := body["announcement"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, a.ID, packed["id"])
	assert.Equal(t, "Change to Invitation-Only", packed["title"])
	// #2106 L24/L52: create event は upstream 同様 me-less pack なので forYou=false。
	assert.Equal(t, false, packed["forYou"])
	assert.Equal(t, false, packed["isRead"])
	assert.Equal(t, "2026-06-01T12:00:00.000Z", packed["createdAt"])
}

// #2056: global (UserID nil) は main へ出さず broadcast stream へ announcementCreated
// を流す (upstream AnnouncementService.create の publishBroadcastStream)。
func TestCreate_GlobalAnnouncementBroadcasts(t *testing.T) {
	a, idGen := newAnnouncement(t, nil)
	repo := &fakeRepo{}
	pub := &fakePublisher{}
	bc := &fakeBroadcast{}
	c := NewCreator(repo, idGen, pub, bc)

	require.NoError(t, c.Create(a))
	assert.Len(t, repo.created, 1)
	assert.Empty(t, pub.events, "global は main へ出さない")
	require.Len(t, bc.events, 1, "global は broadcast へ出す (#2056)")
	assert.Equal(t, "announcementCreated", bc.events[0].eventType)
	body, ok := bc.events[0].body.(map[string]any)
	require.True(t, ok)
	packed, ok := body["announcement"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, a.ID, packed["id"])
}

// #2056: per-user announcement は broadcast へは出さない (main のみ)。
func TestCreate_PerUserDoesNotBroadcast(t *testing.T) {
	a, idGen := newAnnouncement(t, ptr("mod1"))
	repo := &fakeRepo{}
	pub := &fakePublisher{}
	bc := &fakeBroadcast{}
	c := NewCreator(repo, idGen, pub, bc)

	require.NoError(t, c.Create(a))
	assert.Len(t, pub.events, 1, "per-user は main へ出す")
	assert.Empty(t, bc.events, "per-user は broadcast へ出さない")
}

func TestCreate_NilPublisherStillPersists(t *testing.T) {
	a, idGen := newAnnouncement(t, ptr("mod1"))
	repo := &fakeRepo{}
	c := NewCreator(repo, idGen, nil, nil)

	require.NoError(t, c.Create(a))
	assert.Len(t, repo.created, 1)
}

func TestCreate_RepoErrorPropagatesWithoutPublish(t *testing.T) {
	a, idGen := newAnnouncement(t, ptr("mod1"))
	repo := &fakeRepo{err: errors.New("db down")}
	pub := &fakePublisher{}
	c := NewCreator(repo, idGen, pub, nil)

	err := c.Create(a)
	assert.Error(t, err)
	assert.Empty(t, pub.events, "永続化に失敗した announcement の event は出さない")
}
