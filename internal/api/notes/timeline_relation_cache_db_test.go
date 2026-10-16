package notes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/timeline"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var (
	relationDBOnce sync.Once
	relationDB     *gorm.DB
)

func openRelationDB(t *testing.T) *gorm.DB {
	t.Helper()
	relationDBOnce.Do(func() {
		relationDB = testutil.MustOpenTestDB()
		testutil.ApplyMigrations(relationDB)
	})
	return relationDB
}

type relationFanoutOff struct{}

func (relationFanoutOff) FanoutTimelineEnabled() bool { return false }

// relationTimelineEnv is a notes handler on the real DB whose relation
// repositories share one RelationCache, as the router wires them (#3607).
type relationTimelineEnv struct {
	db                      *gorm.DB
	h                       *Handler
	rc                      *repository.RelationCache
	muting                  repository.MutingRepository
	blocking                repository.BlockingRepository
	following               repository.FollowingRepository
	viewer                  *model.User
	authorID, noteID        string
	channelID, chanNoteID   string
	channelFollowing        repository.ChannelFollowingRepository
	channelMuting           repository.ChannelMutingRepository
	renoteMuting            repository.RenoteMutingRepository
	renoterID, renoteNoteID string
}

func newRelationTimelineEnv(t *testing.T, prefix string, fanout bool) *relationTimelineEnv {
	t.Helper()
	db := openRelationDB(t)
	cleanup := func() {
		db.Exec(`DELETE FROM "note" WHERE "userId" LIKE ?`, prefix+"%")
		db.Exec(`DELETE FROM "channel" WHERE id LIKE ?`, prefix+"%")
		db.Exec(`DELETE FROM "user" WHERE id LIKE ?`, prefix+"%")
	}
	cleanup()
	t.Cleanup(cleanup)

	mkUser := func(name string) string {
		uid := prefix + name
		require.NoError(t, db.Create(&model.User{ID: uid, Username: uid, UsernameLower: uid, AvatarDecorations: datatypes.JSON([]byte("[]"))}).Error)
		return uid
	}
	viewerID, authorID, renoterID := mkUser("v"), mkUser("a"), mkUser("r")
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	e := &relationTimelineEnv{db: db, authorID: authorID, renoterID: renoterID}
	e.channelID = prefix + "ch"
	require.NoError(t, db.Exec(`INSERT INTO "channel" (id, name, "userId") VALUES (?, ?, ?)`, e.channelID, e.channelID, authorID).Error)
	e.noteID = idGen.Generate(time.Now().Add(-3 * time.Second))
	require.NoError(t, db.Create(&model.Note{ID: e.noteID, UserID: authorID, Text: ptr("hello"), Visibility: "public"}).Error)
	e.chanNoteID = idGen.Generate(time.Now().Add(-2 * time.Second))
	require.NoError(t, db.Create(&model.Note{ID: e.chanNoteID, UserID: authorID, Text: ptr("in channel"), Visibility: "public", ChannelID: &e.channelID}).Error)
	e.renoteNoteID = idGen.Generate(time.Now().Add(-time.Second))
	require.NoError(t, db.Create(&model.Note{ID: e.renoteNoteID, UserID: renoterID, Visibility: "public", RenoteID: &e.noteID, RenoteUserID: &authorID}).Error)

	e.rc = repository.NewRelationCacheWithTTL(db, time.Hour)
	noteRepo := repository.NewNoteRepository(db)
	e.following = repository.NewFollowingRepositoryWithRelationCache(db, e.rc)
	e.muting = repository.NewMutingRepositoryWithRelationCache(db, e.rc)
	e.blocking = repository.NewBlockingRepositoryWithRelationCache(db, e.rc)
	e.renoteMuting = repository.NewRenoteMutingRepositoryWithRelationCache(db, e.rc)
	e.channelMuting = repository.NewChannelMutingRepositoryWithRelationCache(db, e.rc)
	e.channelFollowing = repository.NewChannelFollowingRepositoryWithRelationCache(db, e.rc)
	userRepo := repository.NewUserRepositoryWithRelationCache(db, e.rc)

	var tl *timeline.Service
	if fanout {
		// Redis の HTL から読み、DB には落ちない経路。ここではミュートも
		// キャッシュした一覧だけで判定される (DB の経路は SQL が muting を直接引く)。
		if testRedis == nil {
			t.Skip("redis unavailable")
		}
		ftl := timeline.NewFanoutTimelineService(testRedis.Client, idGen, prefix)
		for _, nid := range []string{e.noteID, e.renoteNoteID} {
			require.NoError(t, ftl.Push(context.Background(), timeline.HomeTimelineName(viewerID), nid, 100))
		}
		tl = timeline.NewService(ftl, noteRepo, e.following)
		tl.SetDBFallbackToggle(dbFallbackOff{})
	} else {
		tl = timeline.NewService(nil, noteRepo, e.following)
		tl.SetFanoutToggle(relationFanoutOff{})
	}
	e.h = NewHandler(noteRepo, nil, nil, nil, tl, nil, nil, nil, idGen)
	e.h.SetUserRepo(userRepo)
	e.h.SetMutingRepo(e.muting)
	e.h.SetRenoteMutingRepo(e.renoteMuting)
	e.h.SetBlockingRepo(e.blocking)
	e.h.SetUserFollowingRepo(e.following)
	e.h.SetChannelMutingRepo(e.channelMuting)
	e.h.SetChannelFollowingRepo(e.channelFollowing)
	e.h.SetRelationCache(e.rc)

	require.NoError(t, e.following.Create(&model.Following{ID: prefix + "f1", FollowerID: viewerID, FolloweeID: authorID}))
	require.NoError(t, e.following.Create(&model.Following{ID: prefix + "f2", FollowerID: viewerID, FolloweeID: renoterID}))
	e.viewer, err = userRepo.FindByID(viewerID)
	require.NoError(t, err)
	return e
}

func ptr(s string) *string { return &s }

// home returns the note ids of the viewer's home timeline.
func (e *relationTimelineEnv) home(t *testing.T) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/notes/timeline", strings.NewReader(`{"limit":10}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), e.viewer)
	require.NoError(t, e.h.Timeline(c))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var notes []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &notes))
	ids := make([]string, 0, len(notes))
	for _, n := range notes {
		ids = append(ids, n["id"].(string))
	}
	return ids
}

// TestTimeline_RelationCache_ChangesApplyToTheNextRequest pins that a mute,
// block, renote mute, channel follow / mute or unfollow made through the
// repositories is used by the very next timeline request even though the
// lists are cached between requests (#3607). It runs on both the database
// path and the Redis (fanout) path.
func TestTimeline_RelationCache_ChangesApplyToTheNextRequest(t *testing.T) {
	for _, fanout := range []bool{false, true} {
		t.Run(fmt.Sprintf("fanout=%v", fanout), func(t *testing.T) {
			testRelationCacheSteps(t, fanout)
		})
	}
}

func testRelationCacheSteps(t *testing.T, fanout bool) {
	prefix := "trcd3607_"
	if fanout {
		prefix = "trcf3607_"
	}
	e := newRelationTimelineEnv(t, prefix, fanout)
	viewer := e.viewer.ID

	require.ElementsMatch(t, []string{e.noteID, e.renoteNoteID}, e.home(t))

	// キャッシュから返していることを確かめる: repository を通らずに足した
	// ブロックは、次の要求でも効かない。ミュートは DB の経路の SQL が muting を
	// 直接引くので、この確認には使えない (ブロックは一覧をそのまま SQL に渡す)。
	require.NoError(t, e.db.Create(&model.Blocking{ID: prefix + "b0", BlockerID: e.renoterID, BlockeeID: viewer}).Error)
	assert.ElementsMatch(t, []string{e.noteID, e.renoteNoteID}, e.home(t), "served from the cache")
	e.db.Exec(`DELETE FROM "blocking" WHERE id = ?`, prefix+"b0")
	e.rc.Invalidate(repository.RelationBlockedBy, viewer)

	steps := []struct {
		name       string
		write      func() error
		want       []string
		dbPathOnly bool
	}{
		{name: "mute the author", write: func() error {
			return e.muting.Create(&model.Muting{ID: prefix + "m1", MuterID: viewer, MuteeID: e.authorID})
		}, want: []string{}},
		{name: "unmute", write: func() error { return e.muting.Delete(&model.Muting{ID: prefix + "m1"}) }, want: []string{e.noteID, e.renoteNoteID}},
		{name: "the author blocks the viewer", write: func() error {
			return e.blocking.Create(&model.Blocking{ID: prefix + "b1", BlockerID: e.authorID, BlockeeID: viewer})
		}, want: []string{}},
		{name: "unblock", write: func() error { return e.blocking.Delete(&model.Blocking{ID: prefix + "b1"}) }, want: []string{e.noteID, e.renoteNoteID}},
		{name: "renote-mute the renoter", write: func() error {
			return e.renoteMuting.Create(&model.RenoteMuting{ID: prefix + "r1", MuterID: viewer, MuteeID: e.renoterID})
		}, want: []string{e.noteID}},
		// チャンネルとフォローの一覧で取得範囲が変わるのは DB の経路だけ
		// (Redis の HTL は配った時点のフォローで決まっている)。
		{name: "follow the channel", write: func() error {
			return e.channelFollowing.Create(&model.ChannelFollowing{ID: prefix + "cf", FollowerID: viewer, FolloweeID: e.channelID})
		}, want: []string{e.noteID, e.chanNoteID}, dbPathOnly: true},
		{name: "mute the channel", write: func() error {
			return e.channelMuting.Create(&model.ChannelMuting{ID: prefix + "cm", UserID: viewer, ChannelID: e.channelID})
		}, want: []string{e.noteID}, dbPathOnly: true},
		{name: "unfollow the author", write: func() error { return e.following.Delete(&model.Following{ID: prefix + "f1"}) }, want: []string{}, dbPathOnly: true},
	}
	for _, st := range steps {
		if fanout && st.dbPathOnly {
			continue
		}
		require.NoError(t, st.write(), st.name)
		assert.ElementsMatch(t, st.want, e.home(t), st.name)
	}
}

// TestTimeline_RelationCache_UnfollowAppliesToFollowersOnlyReplyGuard pins the
// following list: on the Redis path a reply to a followers-only note is shown
// only while the viewer follows that note's author, so an unfollow must reach
// the cached following list before the next request (#3607).
func TestTimeline_RelationCache_UnfollowAppliesToFollowersOnlyReplyGuard(t *testing.T) {
	prefix := "trcg3607_"
	e := newRelationTimelineEnv(t, prefix, true)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	fo := idGen.Generate(time.Now().Add(-time.Second))
	require.NoError(t, e.db.Create(&model.Note{ID: fo, UserID: e.authorID, Text: ptr("followers only"), Visibility: model.NoteVisibilityFollowers}).Error)
	reply := idGen.Generate(time.Now())
	require.NoError(t, e.db.Create(&model.Note{ID: reply, UserID: e.renoterID, Text: ptr("reply"), Visibility: "public", ReplyID: &fo, ReplyUserID: &e.authorID}).Error)
	ftl := timeline.NewFanoutTimelineService(testRedis.Client, idGen, prefix)
	require.NoError(t, ftl.Push(context.Background(), timeline.HomeTimelineName(e.viewer.ID), reply, 100))

	require.Contains(t, e.home(t), reply, "visible while following the replied-to author")
	require.NoError(t, e.following.Delete(&model.Following{ID: prefix + "f1"}))
	assert.NotContains(t, e.home(t), reply, "hidden right after the unfollow")
}
