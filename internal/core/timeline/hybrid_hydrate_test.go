package timeline

import (
	"context"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unhydratedNoteRepo lists bare rows and records what HydrateNotes received.
type unhydratedNoteRepo struct {
	*testutil.MockNoteRepository
	home, local  []*model.Note
	hydrated     [][]string
	hydrateErr   error
	hydratedList int
}

func (r *unhydratedNoteRepo) ListHomeTimeline(string, int, string, string, model.TimelineDBFilter) ([]*model.Note, error) {
	r.hydratedList++
	return nil, errors.New("the hydrated list must not be used")
}

func (r *unhydratedNoteRepo) ListLocalTimeline(int, string, string, model.TimelineDBFilter) ([]*model.Note, error) {
	r.hydratedList++
	return nil, errors.New("the hydrated list must not be used")
}

func (r *unhydratedNoteRepo) ListHomeTimelineUnhydrated(_ string, limit int, _, _ string, _ model.TimelineDBFilter) ([]*model.Note, error) {
	return copyRows(r.home, limit), nil
}

func (r *unhydratedNoteRepo) ListLocalTimelineUnhydrated(limit int, _, _ string, _ model.TimelineDBFilter) ([]*model.Note, error) {
	return copyRows(r.local, limit), nil
}

func (r *unhydratedNoteRepo) HydrateNotes(notes []*model.Note) error {
	ids := make([]string, 0, len(notes))
	for _, n := range notes {
		ids = append(ids, n.ID)
		n.User = &model.User{ID: n.UserID}
	}
	r.hydrated = append(r.hydrated, ids)
	return r.hydrateErr
}

func copyRows(rows []*model.Note, limit int) []*model.Note {
	out := make([]*model.Note, 0, limit)
	for _, n := range rows {
		if len(out) == limit {
			break
		}
		c := *n
		out = append(out, &c)
	}
	return out
}

func hybridRow(id string) *model.Note {
	return &model.Note{ID: id, UserID: "u_" + id, Visibility: model.NoteVisibilityPublic}
}

// The hybrid DB fallback merges the bare home / local rows, cuts the page to
// limit and hydrates only that page, once (#3611; it used to hydrate up to
// 2*limit rows and drop half).
func TestHybridDBFallback_HydratesOnlyTheMergedPage(t *testing.T) {
	repo := &unhydratedNoteRepo{
		MockNoteRepository: testutil.NewMockNoteRepository(),
		home:               []*model.Note{hybridRow("e"), hybridRow("c"), hybridRow("a")},
		local:              []*model.Note{hybridRow("d"), hybridRow("c"), hybridRow("b")},
	}
	svc := NewService(NewFanoutTimelineService(closedClient(t), idGen, ""), repo, testutil.NewMockFollowingRepository())
	svc.SetFanoutToggle(&fakeFanoutToggle{enabled: false})

	got, err := svc.HybridTimeline(context.Background(), &model.User{ID: "viewer"}, "", "", 3, TimelineFilter{})
	require.NoError(t, err)
	ids := make([]string, 0, len(got))
	for _, n := range got {
		ids = append(ids, n.ID)
		require.NotNil(t, n.User, "returned notes are hydrated")
	}
	assert.Equal(t, []string{"e", "d", "c"}, ids)
	assert.Equal(t, [][]string{{"e", "d", "c"}}, repo.hydrated)
	assert.Zero(t, repo.hydratedList)

	repo.hydrated = nil
	repo.hydrateErr = errors.New("boom")
	_, err = svc.HybridTimeline(context.Background(), &model.User{ID: "viewer"}, "", "", 3, TimelineFilter{})
	assert.Error(t, err, "a hydration failure is not served as bare rows")
}
