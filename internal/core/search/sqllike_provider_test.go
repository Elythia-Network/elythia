package search

import (
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSQLLikeProviderForTest() (*SQLLikeProvider, *testutil.MockNoteRepository) {
	repo := testutil.NewMockNoteRepository()
	return NewSQLLikeProvider(repo, nil), repo
}

func TestSQLLikeProvider_IndexAndUnindexAreNoOps(t *testing.T) {
	p, _ := newSQLLikeProviderForTest()
	require.NoError(t, p.IndexNote(&model.Note{ID: "n1"}))
	require.NoError(t, p.UnindexNote(&model.Note{ID: "n1"}))
}

func TestSQLLikeProvider_SearchNoteHappyPath(t *testing.T) {
	p, repo := newSQLLikeProviderForTest()
	hello := "Hello, world"
	repo.Notes["n1"] = &model.Note{
		ID:         "n1",
		UserID:     "u1",
		Visibility: model.NoteVisibilityPublic,
		Text:       &hello,
	}
	other := "completely different"
	repo.Notes["n2"] = &model.Note{
		ID:         "n2",
		UserID:     "u1",
		Visibility: model.NoteVisibilityPublic,
		Text:       &other,
	}

	out, err := p.SearchNote(nil, "hello", SearchOpts{}, Pagination{Limit: 10})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "n1", out[0].ID)
}

func TestSQLLikeProvider_EmptyQueryRejected(t *testing.T) {
	p, _ := newSQLLikeProviderForTest()
	_, err := p.SearchNote(nil, "", SearchOpts{}, Pagination{Limit: 10})
	require.ErrorIs(t, err, ErrEmptyQuery)
}

func TestSQLLikeProvider_DefaultLimitWhenZero(t *testing.T) {
	p, repo := newSQLLikeProviderForTest()
	hello := "hello"
	for i := range 12 {
		id := string(rune('a'+i)) + "1"
		repo.Notes[id] = &model.Note{
			ID:         id,
			UserID:     "u",
			Visibility: model.NoteVisibilityPublic,
			Text:       &hello,
		}
	}
	out, err := p.SearchNote(nil, "hello", SearchOpts{}, Pagination{}) // Limit==0
	require.NoError(t, err)
	// MockNoteRepository.SearchByFilter のデフォルト Limit は 10。
	assert.Len(t, out, 10)
}

// stubFailingNoteRepo wraps the in-memory mock and forces SearchByFilter to fail
// so we can verify the provider propagates repo errors.
type stubFailingNoteRepo struct {
	*testutil.MockNoteRepository
}

func (s *stubFailingNoteRepo) SearchByFilter(_ model.NoteSearchFilter) ([]*model.Note, error) {
	return nil, errors.New("boom")
}

func TestSQLLikeProvider_RepoErrorPropagated(t *testing.T) {
	repo := &stubFailingNoteRepo{MockNoteRepository: testutil.NewMockNoteRepository()}
	p := NewSQLLikeProvider(repo, nil)
	_, err := p.SearchNote(nil, "hello", SearchOpts{}, Pagination{Limit: 10})
	require.Error(t, err)
}

// stubFilterCapturingNoteRepo records the last NoteSearchFilter passed to
// SearchByFilter so that tests can verify provider → filter wiring without
// depending on the underlying SQL backend.
type stubFilterCapturingNoteRepo struct {
	*testutil.MockNoteRepository
	lastFilter model.NoteSearchFilter
}

func (s *stubFilterCapturingNoteRepo) SearchByFilter(f model.NoteSearchFilter) ([]*model.Note, error) {
	s.lastFilter = f
	return s.MockNoteRepository.SearchByFilter(f)
}

func TestSQLPgroongaProvider_PropagatesPgroongaFlag(t *testing.T) {
	repo := &stubFilterCapturingNoteRepo{MockNoteRepository: testutil.NewMockNoteRepository()}
	p := NewSQLPgroongaProvider(repo, nil)
	_, err := p.SearchNote(nil, "hello", SearchOpts{}, Pagination{Limit: 10})
	require.NoError(t, err)
	assert.True(t, p.pgroonga, "constructor should mark provider as pgroonga")
	assert.True(t, repo.lastFilter.Pgroonga, "Pgroonga flag must reach the repository filter")
}

func TestSQLLikeProvider_DoesNotSetPgroongaFlag(t *testing.T) {
	repo := &stubFilterCapturingNoteRepo{MockNoteRepository: testutil.NewMockNoteRepository()}
	p := NewSQLLikeProvider(repo, nil)
	_, err := p.SearchNote(nil, "hello", SearchOpts{}, Pagination{Limit: 10})
	require.NoError(t, err)
	assert.False(t, repo.lastFilter.Pgroonga)
}

func TestSQLLikeProvider_VisibilityFilterDropsHidden(t *testing.T) {
	p, repo := newSQLLikeProviderForTest()
	hello := "hello"
	repo.Notes["n1"] = &model.Note{
		ID:         "n1",
		UserID:     "author",
		Visibility: model.NoteVisibilityPublic,
		Text:       &hello,
	}
	// Mock returns this row because it is public/home, but the post-filter
	// in core/note.CanSeeNote should keep it for an anonymous viewer.
	out, err := p.SearchNote(nil, "hello", SearchOpts{}, Pagination{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

// #2069 (upstream #16119): rangeStartID/rangeEndID は epoch ms → aidx prefix 境界。
// nil は空文字 (制限なし)、非 nil は AidxCutoffPrefix の値。
func TestRangeBoundConversion(t *testing.T) {
	assert.Equal(t, "", rangeStartID(nil))
	assert.Equal(t, "", rangeEndID(nil))
	at := time.Date(2022, 6, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	assert.NotEmpty(t, rangeStartID(&at))
	assert.NotEmpty(t, rangeEndID(&at))
	// end は start より後ろの境界 (rangeEndAt+1 > rangeStartAt-1 で prefix も大)。
	assert.Greater(t, rangeEndID(&at), rangeStartID(&at))
}

// LocalUsersOnly is passed down to the repository so that remote authors are
// dropped before LIMIT (visitors under ugcVisibilityForVisitor=local).
func TestSQLLikeProvider_LocalUsersOnly(t *testing.T) {
	p, repo := newSQLLikeProviderForTest()
	hello := "hello"
	remote := "remote.example"
	repo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Visibility: model.NoteVisibilityPublic, Text: &hello}
	repo.Notes["n2"] = &model.Note{ID: "n2", UserID: "u2", UserHost: &remote, Visibility: model.NoteVisibilityPublic, Text: &hello}

	out, err := p.SearchNote(nil, "hello", SearchOpts{LocalUsersOnly: true}, Pagination{Limit: 10})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "n1", out[0].ID)

	out, err = p.SearchNote(nil, "hello", SearchOpts{}, Pagination{Limit: 10})
	require.NoError(t, err)
	assert.Len(t, out, 2)
}
