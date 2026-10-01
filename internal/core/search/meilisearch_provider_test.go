package search

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMeilisearchIndex is an in-memory MeilisearchIndex used by tests.
type fakeMeilisearchIndex struct {
	docs           map[string]NoteDocument
	settings       IndexSettings
	addErr         error
	deleteErr      error
	searchErr      error
	settingsErr    error
	lastSearchReq  IndexSearchRequest
	lastSearchTerm string
	hits           []IndexSearchHit
}

func newFakeIndex() *fakeMeilisearchIndex {
	return &fakeMeilisearchIndex{docs: make(map[string]NoteDocument)}
}

func (f *fakeMeilisearchIndex) AddDocuments(docs []NoteDocument) error {
	if f.addErr != nil {
		return f.addErr
	}
	for _, d := range docs {
		f.docs[d.ID] = d
	}
	return nil
}

func (f *fakeMeilisearchIndex) DeleteDocument(id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.docs, id)
	return nil
}

func (f *fakeMeilisearchIndex) Search(query string, req IndexSearchRequest) (*IndexSearchResponse, error) {
	f.lastSearchReq = req
	f.lastSearchTerm = query
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return &IndexSearchResponse{Hits: f.hits}, nil
}

func (f *fakeMeilisearchIndex) UpdateSettings(s IndexSettings) error {
	if f.settingsErr != nil {
		return f.settingsErr
	}
	f.settings = s
	return nil
}

func newProviderWithFake(t *testing.T, scope IndexScope) (*MeilisearchProvider, *fakeMeilisearchIndex, *testutil.MockNoteRepository, id.Generator) {
	t.Helper()
	idx := newFakeIndex()
	repo := testutil.NewMockNoteRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	p := NewMeilisearchProvider(idx, repo, nil, idGen, scope)
	return p, idx, repo, idGen
}

func TestMeilisearchProvider_DefaultScopeIsLocal(t *testing.T) {
	p := NewMeilisearchProvider(newFakeIndex(), testutil.NewMockNoteRepository(), nil, nil, "")
	assert.Equal(t, IndexScopeLocal, p.scope)
}

func TestMeilisearchProvider_ApplyDefaultSettings(t *testing.T) {
	p, idx, _, _ := newProviderWithFake(t, IndexScopeGlobal)
	require.NoError(t, p.ApplyDefaultSettings())
	assert.Contains(t, idx.settings.SearchableAttributes, "text")
	assert.Equal(t, int64(10000), idx.settings.PaginationMaxHits)
	assert.False(t, idx.settings.TypoToleranceEnabled)
}

func TestMeilisearchProvider_ApplyDefaultSettingsErrorPropagated(t *testing.T) {
	p, idx, _, _ := newProviderWithFake(t, IndexScopeGlobal)
	idx.settingsErr = errors.New("boom")
	require.Error(t, p.ApplyDefaultSettings())
}

func TestMeilisearchProvider_IndexNoteHappyPath(t *testing.T) {
	p, idx, _, idGen := newProviderWithFake(t, IndexScopeLocal)
	text := "hello"
	noteID := idGen.Generate(time.Now())
	require.NoError(t, p.IndexNote(&model.Note{
		ID:         noteID,
		UserID:     "u1",
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
	}))
	require.Len(t, idx.docs, 1)
	doc := idx.docs[noteID]
	assert.Equal(t, "u1", doc.UserID)
	assert.Equal(t, "hello", *doc.Text)
}

func TestMeilisearchProvider_IndexNoteSkipsNilNote(t *testing.T) {
	p, idx, _, _ := newProviderWithFake(t, IndexScopeLocal)
	require.NoError(t, p.IndexNote(nil))
	assert.Empty(t, idx.docs)
}

func TestMeilisearchProvider_IndexNoteSkipsEmptyContent(t *testing.T) {
	p, idx, _, idGen := newProviderWithFake(t, IndexScopeLocal)
	noteID := idGen.Generate(time.Now())
	require.NoError(t, p.IndexNote(&model.Note{ID: noteID, Visibility: model.NoteVisibilityPublic}))
	assert.Empty(t, idx.docs)
}

func TestMeilisearchProvider_IndexNoteSkipsPrivateVisibility(t *testing.T) {
	p, idx, _, idGen := newProviderWithFake(t, IndexScopeGlobal)
	text := "secret"
	noteID := idGen.Generate(time.Now())
	require.NoError(t, p.IndexNote(&model.Note{
		ID:         noteID,
		UserID:     "u1",
		Text:       &text,
		Visibility: model.NoteVisibilityFollowers,
	}))
	assert.Empty(t, idx.docs)
}

func TestMeilisearchProvider_IndexNoteScopeLocalSkipsRemote(t *testing.T) {
	p, idx, _, idGen := newProviderWithFake(t, IndexScopeLocal)
	text := "remote post"
	host := "remote.example"
	noteID := idGen.Generate(time.Now())
	require.NoError(t, p.IndexNote(&model.Note{
		ID:         noteID,
		UserID:     "u1",
		UserHost:   &host,
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
	}))
	assert.Empty(t, idx.docs)
}

func TestMeilisearchProvider_IndexNoteScopeGlobalIncludesRemote(t *testing.T) {
	p, idx, _, idGen := newProviderWithFake(t, IndexScopeGlobal)
	text := "remote post"
	host := "remote.example"
	noteID := idGen.Generate(time.Now())
	require.NoError(t, p.IndexNote(&model.Note{
		ID:         noteID,
		UserID:     "u1",
		UserHost:   &host,
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
	}))
	assert.Len(t, idx.docs, 1)
}

func TestMeilisearchProvider_IndexNoteUnknownScopeFallsBackToLocal(t *testing.T) {
	p, idx, _, idGen := newProviderWithFake(t, IndexScope("weird"))
	text := "hello"
	host := "remote.example"
	noteID := idGen.Generate(time.Now())
	require.NoError(t, p.IndexNote(&model.Note{
		ID:         noteID,
		UserID:     "u1",
		UserHost:   &host, // remote → unknown scope は local 同等で skip
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
	}))
	assert.Empty(t, idx.docs)
}

func TestMeilisearchProvider_IndexNoteAddDocumentsErrorPropagated(t *testing.T) {
	p, idx, _, idGen := newProviderWithFake(t, IndexScopeLocal)
	idx.addErr = errors.New("boom")
	text := "hello"
	require.Error(t, p.IndexNote(&model.Note{
		ID:         idGen.Generate(time.Now()),
		UserID:     "u1",
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
	}))
}

func TestMeilisearchProvider_UnindexNoteHappyPath(t *testing.T) {
	p, idx, _, idGen := newProviderWithFake(t, IndexScopeLocal)
	noteID := idGen.Generate(time.Now())
	idx.docs[noteID] = NoteDocument{ID: noteID}
	require.NoError(t, p.UnindexNote(&model.Note{ID: noteID, Visibility: model.NoteVisibilityPublic}))
	assert.Empty(t, idx.docs)
}

func TestMeilisearchProvider_UnindexNoteSkipsNil(t *testing.T) {
	p, _, _, _ := newProviderWithFake(t, IndexScopeLocal)
	require.NoError(t, p.UnindexNote(nil))
}

func TestMeilisearchProvider_UnindexNoteSkipsHiddenVisibility(t *testing.T) {
	p, idx, _, idGen := newProviderWithFake(t, IndexScopeLocal)
	noteID := idGen.Generate(time.Now())
	idx.docs[noteID] = NoteDocument{ID: noteID}
	require.NoError(t, p.UnindexNote(&model.Note{ID: noteID, Visibility: model.NoteVisibilityFollowers}))
	// hidden visibility は no-op なので削除されない
	assert.Len(t, idx.docs, 1)
}

func TestMeilisearchProvider_UnindexNoteDeleteErrorPropagated(t *testing.T) {
	p, idx, _, idGen := newProviderWithFake(t, IndexScopeLocal)
	idx.deleteErr = errors.New("boom")
	noteID := idGen.Generate(time.Now())
	require.Error(t, p.UnindexNote(&model.Note{ID: noteID, Visibility: model.NoteVisibilityPublic}))
}

func TestMeilisearchProvider_SearchNoteHappyPath(t *testing.T) {
	p, idx, repo, idGen := newProviderWithFake(t, IndexScopeLocal)
	text := "hello"
	noteID := idGen.Generate(time.Now())
	repo.Notes[noteID] = &model.Note{
		ID:         noteID,
		UserID:     "u1",
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
	}
	idx.hits = []IndexSearchHit{{ID: noteID}}

	out, err := p.SearchNote(nil, "hello", SearchOpts{
		UserID:    "u1",
		ChannelID: "ch1",
		Host:      ".",
	}, Pagination{
		Limit: 5,
	})
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, noteID, out[0].ID)

	// IndexSearchRequest assertions
	assert.Equal(t, int64(5), idx.lastSearchReq.Limit)
	assert.Contains(t, idx.lastSearchReq.AttributesToRetrieve, "id")
	assert.Equal(t, []string{"createdAt:desc"}, idx.lastSearchReq.Sort)
	assert.Contains(t, idx.lastSearchReq.Filter, "userId = 'u1'")
	assert.Contains(t, idx.lastSearchReq.Filter, "channelId = 'ch1'")
	assert.Contains(t, idx.lastSearchReq.Filter, meiliLocalUserClause)
}

func TestMeilisearchProvider_SearchNoteEmptyQueryRejected(t *testing.T) {
	p, _, _, _ := newProviderWithFake(t, IndexScopeLocal)
	_, err := p.SearchNote(nil, "", SearchOpts{}, Pagination{Limit: 5})
	require.ErrorIs(t, err, ErrEmptyQuery)
}

func TestMeilisearchProvider_SearchNoteDefaultLimit(t *testing.T) {
	p, idx, _, _ := newProviderWithFake(t, IndexScopeLocal)
	idx.hits = nil
	_, err := p.SearchNote(nil, "x", SearchOpts{}, Pagination{Limit: 0})
	require.NoError(t, err)
	assert.Equal(t, int64(10), idx.lastSearchReq.Limit)
}

func TestMeilisearchProvider_SearchNoteIndexErrorPropagated(t *testing.T) {
	p, idx, _, _ := newProviderWithFake(t, IndexScopeLocal)
	idx.searchErr = errors.New("boom")
	_, err := p.SearchNote(nil, "x", SearchOpts{}, Pagination{Limit: 5})
	require.Error(t, err)
}

func TestMeilisearchProvider_SearchNoteSortsByIDDesc(t *testing.T) {
	p, idx, repo, idGen := newProviderWithFake(t, IndexScopeLocal)
	text := "hello"
	earlierID := idGen.Generate(time.Now().Add(-time.Hour))
	laterID := idGen.Generate(time.Now())
	for _, nid := range []string{earlierID, laterID} {
		repo.Notes[nid] = &model.Note{
			ID:         nid,
			UserID:     "u1",
			Text:       &text,
			Visibility: model.NoteVisibilityPublic,
		}
	}
	// Hit を昇順で渡すと、SearchNote 内の sort.SliceStable で id 降順に
	// 並び替えられることを確認する。
	idx.hits = []IndexSearchHit{{ID: earlierID}, {ID: laterID}}
	out, err := p.SearchNote(nil, "hello", SearchOpts{}, Pagination{Limit: 5})
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, laterID, out[0].ID)
	assert.Equal(t, earlierID, out[1].ID)
}

func TestMeilisearchProvider_SearchNoteEmptyHitsReturnsNil(t *testing.T) {
	p, idx, _, _ := newProviderWithFake(t, IndexScopeLocal)
	idx.hits = nil
	out, err := p.SearchNote(nil, "x", SearchOpts{}, Pagination{Limit: 5})
	require.NoError(t, err)
	assert.Nil(t, out)
}

// failingFindManyRepo lets us inject a FindManyByIDsWithUser failure for the
// hit-resolution path.
type failingFindManyRepo struct {
	*testutil.MockNoteRepository
}

func (f *failingFindManyRepo) FindManyByIDsWithUser(_ []string) ([]*model.Note, error) {
	return nil, errors.New("boom")
}

func TestMeilisearchProvider_SearchNoteRepoErrorPropagated(t *testing.T) {
	idx := newFakeIndex()
	repo := &failingFindManyRepo{MockNoteRepository: testutil.NewMockNoteRepository()}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	p := NewMeilisearchProvider(idx, repo, nil, idGen, IndexScopeLocal)
	idx.hits = []IndexSearchHit{{ID: "n1"}}
	_, err = p.SearchNote(nil, "hello", SearchOpts{}, Pagination{Limit: 5})
	require.Error(t, err)
}

func TestMeilisearchProvider_SearchNoteHostFilterRemote(t *testing.T) {
	p, idx, _, _ := newProviderWithFake(t, IndexScopeGlobal)
	idx.hits = nil
	_, err := p.SearchNote(nil, "x", SearchOpts{Host: "remote.example"}, Pagination{Limit: 5})
	require.NoError(t, err)
	assert.Contains(t, idx.lastSearchReq.Filter, "userHost = 'remote.example'")
}

func TestMeilisearchProvider_BuildFilterWithDateCursors(t *testing.T) {
	p, _, _, idGen := newProviderWithFake(t, IndexScopeLocal)
	until := idGen.Generate(time.UnixMilli(1700000000000))
	since := idGen.Generate(time.UnixMilli(1690000000000))
	got := p.buildFilter(SearchOpts{}, Pagination{UntilID: until, SinceID: since})
	assert.Contains(t, got, "createdAt < ")
	assert.Contains(t, got, "createdAt > ")
}

func TestMeilisearchProvider_BuildFilterIgnoresInvalidIDs(t *testing.T) {
	p, _, _, _ := newProviderWithFake(t, IndexScopeLocal)
	// AIDX requires at least 8 characters; "bad" は parse failure 経路を踏む。
	got := p.buildFilter(SearchOpts{}, Pagination{UntilID: "bad", SinceID: "bad"})
	assert.Empty(t, got)
}

func TestMeilisearchProvider_BuildFilterEmpty(t *testing.T) {
	p, _, _, _ := newProviderWithFake(t, IndexScopeLocal)
	got := p.buildFilter(SearchOpts{}, Pagination{})
	assert.Equal(t, "", got)
}

func TestMeilisearchProvider_TimestampOfNilGenerator(t *testing.T) {
	p := NewMeilisearchProvider(newFakeIndex(), testutil.NewMockNoteRepository(), nil, nil, IndexScopeLocal)
	got := p.timestampOf(&model.Note{ID: "x"})
	assert.Equal(t, int64(0), got)
}

func TestMeilisearchProvider_TimestampOfBadID(t *testing.T) {
	p, _, _, _ := newProviderWithFake(t, IndexScopeLocal)
	got := p.timestampOf(&model.Note{ID: "x"}) // < 8 chars → ParseTime error
	assert.Equal(t, int64(0), got)
}

func TestQuoteValueEscapesLikeUpstream(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "plain", want: `'plain'`},
		{name: "single quote", in: "a'b", want: `'a\'b'`},
		{name: "backslash", in: `a\b`, want: `'a\\b'`},
		{name: "backslash before quote", in: `a\'b`, want: `'a\\\'b'`},
		{name: "trailing backslash", in: `a\`, want: `'a\\'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, quoteValue(tc.in))
		})
	}
	assert.True(t, strings.HasPrefix(quoteValue("x"), "'"))
}

func TestMeilisearchProvider_BuildFilterEscapesValues(t *testing.T) {
	p, _, _, _ := newProviderWithFake(t, IndexScopeLocal)
	got := p.buildFilter(SearchOpts{
		UserID:    "u'1",
		ChannelID: `c\`,
		Host:      `h\'x`,
	}, Pagination{})
	assert.Equal(t, `(userId = 'u\'1') AND (channelId = 'c\\') AND (userHost = 'h\\\'x')`, got)
}

// LocalUsersOnly adds the local-author clause (upstream searchNoteByMeilisearch
// for visitors under ugcVisibilityForVisitor=local), and leaves the filter
// alone when unset.
//
// 句は `IS NULL` だけでは足りない。属性の無い文書 (omitempty で索引した既存の
// ローカルの文書) に一致しないので、未ログインの検索が常に空になる。
func TestMeilisearchProvider_BuildFilterLocalUsersOnly(t *testing.T) {
	p, _, _, _ := newProviderWithFake(t, IndexScopeGlobal)
	assert.Equal(t, "(userHost NOT EXISTS OR userHost IS NULL)", p.buildFilter(SearchOpts{LocalUsersOnly: true}, Pagination{}))
	assert.Equal(t, "(userHost NOT EXISTS OR userHost IS NULL)", p.buildFilter(SearchOpts{Host: "."}, Pagination{}))
	assert.Equal(t, "", p.buildFilter(SearchOpts{}, Pagination{}))
	assert.Equal(t, `(userHost = 'remote.example') AND (userHost NOT EXISTS OR userHost IS NULL)`,
		p.buildFilter(SearchOpts{Host: "remote.example", LocalUsersOnly: true}, Pagination{}))
}

// Local authors are indexed with an explicit null userHost, like upstream,
// so that `userHost IS NULL` filters written against upstream indexes match.
func TestNoteDocument_LocalUserHostIsNull(t *testing.T) {
	b, err := json.Marshal(NoteDocument{ID: "n1"})
	require.NoError(t, err)
	assert.Contains(t, string(b), `"userHost":null`)
}
