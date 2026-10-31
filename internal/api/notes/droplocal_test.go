package notes

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	coretimeline "github.com/elythia-network/elythia/internal/core/timeline"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTimelineJSONCache_DropLocal(t *testing.T) {
	c := newTimelineJSONCache(time.Minute)
	now := time.Unix(1000, 0)
	c.set(c.generation(), "a", []byte("1"), now)
	c.set(c.generation(), "b", []byte("2"), now)

	c.DropLocal()
	for _, k := range []string{"a", "b"} {
		_, ok := c.get(k, now)
		assert.False(t, ok, "DropLocal drops %q", k)
	}

	// 捨てる前に読んだ世代の番号では積まない。捨てた後に読んだ番号なら積む。
	stale := c.generation()
	c.DropLocal()
	c.set(stale, "a", []byte("stale"), now)
	_, ok := c.get("a", now)
	assert.False(t, ok, "a response built before the drop is not stored")
	c.set(c.generation(), "a", []byte("fresh"), now)
	body, ok := c.get("a", now)
	require.True(t, ok)
	assert.Equal(t, "fresh", string(body))
}

// serveTimeline reads the generation before building the response, so a drop
// while the notes are being read keeps the response out of the cache.
func TestTimeline_Cache_DropDuringBuildIsNotStored(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	note1 := seedTimelineNote(noteRepo)
	tl, fanout := newRealTimelineService(t, noteRepo)
	require.NoError(t, fanout.Push(context.Background(), coretimeline.LocalTimeline, note1, 100))
	h := newTimelineHandler(t, noteRepo, tl)
	h.EnableTimelineJSONCache(time.Minute)
	cache := h.TimelineJSONCache()
	require.NotNil(t, cache)

	dropping := func(viewer *model.User, req TimelineRequest) ([]*model.Note, error) {
		cache.DropLocal()
		return []*model.Note{noteRepo.Notes[note1]}, nil
	}
	c1, rec1 := newJSONRequest(t, "/api/notes/local-timeline", `{}`)
	require.NoError(t, h.serveTimeline(c1, "local", id.NormalizeCursor, dropping, false))
	require.Equal(t, http.StatusOK, rec1.Code)

	// 1 回目は捨てる前に組み立て始めたので積まれていない。2 回目は読み直す。
	calls := 0
	counting := func(viewer *model.User, req TimelineRequest) ([]*model.Note, error) {
		calls++
		return []*model.Note{}, nil
	}
	c2, rec2 := newJSONRequest(t, "/api/notes/local-timeline", `{}`)
	require.NoError(t, h.serveTimeline(c2, "local", id.NormalizeCursor, counting, false))
	assert.Equal(t, 1, calls, "the response built across the drop was not cached")
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp))
	assert.Empty(t, resp)
}

func TestTimelineJSONCache_NilWhenDisabled(t *testing.T) {
	h := newTimelineHandler(t, testutil.NewMockNoteRepository(), nil)
	assert.Nil(t, h.TimelineJSONCache())
}

func TestFeaturedRankingCache_DropLocal(t *testing.T) {
	h, _, _ := newExtraHandler(t)
	fake := &fakeFeaturedReader{global: []string{"a"}}
	h.SetFeaturedRanking(fake)
	_, err := h.cachedGlobalRanking(context.Background())
	require.NoError(t, err)

	h.FeaturedRankingCache().DropLocal()
	fake.global = []string{"b"}
	ids, err := h.cachedGlobalRanking(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, fake.globalCalls, "the ranking is read again after DropLocal")
	assert.Equal(t, []string{"b"}, ids)
}
