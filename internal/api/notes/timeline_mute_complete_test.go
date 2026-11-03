package notes

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/core/timeline"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// filterCapturingNoteRepo records the TimelineDBFilter each DB timeline query
// receives.
type filterCapturingNoteRepo struct {
	*testutil.MockNoteRepository
	got []model.TimelineDBFilter
}

func (r *filterCapturingNoteRepo) ListHomeTimeline(userID string, limit int, sinceID, untilID string, f model.TimelineDBFilter) ([]*model.Note, error) {
	r.got = append(r.got, f)
	return r.MockNoteRepository.ListHomeTimeline(userID, limit, sinceID, untilID, f)
}

func (r *filterCapturingNoteRepo) ListLocalTimeline(limit int, sinceID, untilID string, f model.TimelineDBFilter) ([]*model.Note, error) {
	r.got = append(r.got, f)
	return r.MockNoteRepository.ListLocalTimeline(limit, sinceID, untilID, f)
}

func (r *filterCapturingNoteRepo) ListGlobalTimeline(limit int, sinceID, untilID string, f model.TimelineDBFilter) ([]*model.Note, error) {
	r.got = append(r.got, f)
	return r.MockNoteRepository.ListGlobalTimeline(limit, sinceID, untilID, f)
}

type failingMuteeLister struct{ repository.MutingRepository }

func (failingMuteeLister) ListMuteeIDs(string) ([]string, error) {
	return nil, errors.New("muting unavailable")
}

type failingRenoteMuteeLister struct {
	repository.RenoteMutingRepository
}

func (failingRenoteMuteeLister) ListMuteeIDs(string) ([]string, error) {
	return nil, errors.New("renote muting unavailable")
}

// TestTimelines_MuteListsCompleteOnlyWhenRead pins that the four timelines
// mark the mute lists complete only when they were read, so the DB fallback
// swaps the muting subqueries for the lists only then (#3987). A list that
// failed to load must keep the subqueries; otherwise the mutes would silently
// stop applying to the SQL.
func TestTimelines_MuteListsCompleteOnlyWhenRead(t *testing.T) {
	endpoints := map[string]func(h *Handler) func(echo.Context) error{
		"home":   func(h *Handler) func(echo.Context) error { return h.Timeline },
		"local":  func(h *Handler) func(echo.Context) error { return h.LocalTimeline },
		"global": func(h *Handler) func(echo.Context) error { return h.GlobalTimeline },
		"hybrid": func(h *Handler) func(echo.Context) error { return h.HybridTimeline },
	}
	viewer := &model.User{ID: "viewer"}
	for name, endpoint := range endpoints {
		t.Run(name, func(t *testing.T) {
			newEnv := func(failing bool) *filterCapturingNoteRepo {
				noteRepo := &filterCapturingNoteRepo{MockNoteRepository: testutil.NewMockNoteRepository()}
				idGen, err := id.NewGenerator("aidx")
				require.NoError(t, err)
				tl := timeline.NewService(nil, noteRepo, testutil.NewMockFollowingRepository())
				tl.SetFanoutToggle(relationFanoutOff{})
				h := newHandlerNilDeps(noteRepo, nil, nil, nil, tl, nil, nil, nil, idGen)
				muting := testutil.NewMockMutingRepository()
				require.NoError(t, muting.Create(&model.Muting{ID: "m1", MuterID: viewer.ID, MuteeID: "muted"}))
				renoteMuting := testutil.NewMockRenoteMutingRepository()
				require.NoError(t, renoteMuting.Create(&model.RenoteMuting{ID: "r1", MuterID: viewer.ID, MuteeID: "renoter"}))
				h.mutingRepo, h.renoteMutingRepo = muting, renoteMuting
				if failing {
					h.mutingRepo = failingMuteeLister{muting}
					h.renoteMutingRepo = failingRenoteMuteeLister{renoteMuting}
				}
				postExtra(endpoint(h), `{}`, viewer)
				require.NotEmpty(t, noteRepo.got, "the DB timeline must be queried")
				return noteRepo
			}

			read := newEnv(false)
			for _, f := range read.got {
				assert.True(t, f.MutedUserIDsComplete)
				assert.Equal(t, []string{"muted"}, f.MutedUserIDs)
				assert.True(t, f.RenoteMutedUserIDsComplete)
				assert.Equal(t, []string{"renoter"}, f.RenoteMutedUserIDs)
				assert.True(t, f.UseMutingSubquery)
				assert.True(t, f.UseRenoteMutingSubquery)
			}

			failed := newEnv(true)
			for _, f := range failed.got {
				assert.False(t, f.MutedUserIDsComplete, "a mute list that failed to load must keep the subquery")
				assert.False(t, f.RenoteMutedUserIDsComplete, "a renote-mute list that failed to load must keep the subquery")
				assert.True(t, f.UseMutingSubquery)
				assert.True(t, f.UseRenoteMutingSubquery)
			}
		})
	}
}

// TestRequestRelations_MuteListsComplete pins the conditions under which the
// mute lists count as complete (#3987).
func TestRequestRelations_MuteListsComplete(t *testing.T) {
	h, _ := newTestHandler(t)
	h.mutingRepo = testutil.NewMockMutingRepository()
	h.renoteMutingRepo = testutil.NewMockRenoteMutingRepository()

	rel := h.newRequestRelations(&model.User{ID: "viewer"})
	assert.True(t, rel.timelineMutedUserIDsComplete(), "an empty list read without error is complete")
	assert.True(t, rel.timelineRenoteMutedUserIDsComplete())

	anon := h.newRequestRelations(nil)
	assert.False(t, anon.timelineMutedUserIDsComplete(), "an anonymous request has no list")
	assert.False(t, anon.timelineRenoteMutedUserIDsComplete())

	h.mutingRepo, h.renoteMutingRepo = nil, nil
	unwired := h.newRequestRelations(&model.User{ID: "viewer"})
	assert.False(t, unwired.timelineMutedUserIDsComplete(), "an unwired repository yields no list")
	assert.False(t, unwired.timelineRenoteMutedUserIDsComplete())
}
