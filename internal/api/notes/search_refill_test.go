package notes

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/search"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeMeiliIndex is an in-memory MeilisearchIndex that honours the createdAt
// cursor filters, the descending sort, offset and limit.
type fakeMeiliIndex struct {
	hits     []search.IndexSearchHit
	requests []search.IndexSearchRequest
}

var (
	meiliUntilRe = regexp.MustCompile(`createdAt < (\d+)`)
	meiliSinceRe = regexp.MustCompile(`createdAt > (\d+)`)
)

func (f *fakeMeiliIndex) AddDocuments([]search.NoteDocument) error  { return nil }
func (f *fakeMeiliIndex) DeleteDocument(string) error               { return nil }
func (f *fakeMeiliIndex) UpdateSettings(search.IndexSettings) error { return nil }
func (f *fakeMeiliIndex) Search(_ string, req search.IndexSearchRequest) (*search.IndexSearchResponse, error) {
	f.requests = append(f.requests, req)
	bound := func(re *regexp.Regexp) (int64, bool) {
		m := re.FindStringSubmatch(req.Filter)
		if m == nil {
			return 0, false
		}
		v, _ := strconv.ParseInt(m[1], 10, 64)
		return v, true
	}
	until, hasUntil := bound(meiliUntilRe)
	since, hasSince := bound(meiliSinceRe)
	var out []search.IndexSearchHit
	for _, h := range f.hits {
		if hasUntil && h.CreatedAt >= until {
			continue
		}
		if hasSince && h.CreatedAt <= since {
			continue
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	if int(req.Offset) >= len(out) {
		return &search.IndexSearchResponse{}, nil
	}
	out = out[req.Offset:]
	if int64(len(out)) > req.Limit {
		out = out[:req.Limit]
	}
	return &search.IndexSearchResponse{Hits: out}, nil
}

// refillFixture seeds notes with readable ids one minute apart (index 0 is the
// newest) and serves them through the SQL or the Meilisearch backend.
type refillFixture struct {
	h     *Handler
	repo  *testutil.MockNoteRepository
	meili *fakeMeiliIndex
	ids   []string
}

func newRefillFixture(t *testing.T, meili bool, hidden []bool) *refillFixture {
	t.Helper()
	repo := testutil.NewMockNoteRepository()
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	f := &refillFixture{repo: repo}
	base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	for i, hide := range hidden {
		at := base.Add(-time.Duration(i) * time.Minute)
		nid := gen.Generate(at)
		n := seedPublicNote(repo, nid)
		text := fmt.Sprintf("secret %d", i)
		n.Text = &text
		n.Tags = []string{"secret"}
		// requireSigninToViewContents の作者のノートは、未ログインの閲覧者には
		// 本文が隠れる (isHidden)。
		n.User.RequireSigninToViewContents = hide
		f.ids = append(f.ids, nid)
		if meili {
			if f.meili == nil {
				f.meili = &fakeMeiliIndex{}
			}
			f.meili.hits = append(f.meili.hits, search.IndexSearchHit{ID: nid, CreatedAt: at.UnixMilli()})
		}
	}
	var provider search.Provider = search.NewSQLLikeProvider(repo, nil)
	if meili {
		provider = search.NewMeilisearchProvider(f.meili, repo, nil, gen, search.IndexScopeGlobal)
	}
	pollRepo := testutil.NewMockPollRepository()
	createSvc := corenote.NewCreateService(repo, pollRepo, gen, nil, nil, nil, nil, nil, nil, nil)
	f.h = newHandlerNilDeps(repo, createSvc, corenote.NewDeleteService(repo), corenote.NewQueryService(repo, nil), nil, nil, nil, search.NewService(provider), gen)
	return f
}

func (f *refillFixture) call(t *testing.T, endpoint, body string) []map[string]any {
	t.Helper()
	call := (*Handler).Search
	if endpoint == "search-by-tag" {
		call = (*Handler).SearchByTag
	}
	c, rec := newJSONRequest(t, "/api/notes/"+endpoint, body)
	require.NoError(t, call(f.h, c))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func idsOf(resp []map[string]any) []string {
	out := make([]string, 0, len(resp))
	for _, n := range resp {
		out = append(out, n["id"].(string))
	}
	return out
}

// hiddenFlags returns n hidden notes followed by visible notes.
func hiddenFlags(hidden, visible int) []bool {
	out := make([]bool, 0, hidden+visible)
	for range hidden {
		out = append(out, true)
	}
	for range visible {
		out = append(out, false)
	}
	return out
}

type refillBackend struct {
	name     string
	meili    bool
	endpoint string
	body     string
}

var refillBackends = []refillBackend{
	{"search/sql", false, "search", `{"query":"secret","limit":2%s}`},
	{"search/meilisearch", true, "search", `{"query":"secret","limit":2%s}`},
	{"search-by-tag", false, "search-by-tag", `{"tag":"secret","limit":2%s}`},
}

// 1 ページ分がすべて隠れるノートでも、続きを取って見えるノートで埋める。
func TestSearch_RefillsPageOfHiddenNotes(t *testing.T) {
	for _, b := range refillBackends {
		t.Run(b.name, func(t *testing.T) {
			f := newRefillFixture(t, b.meili, hiddenFlags(2, 3))
			resp := f.call(t, b.endpoint, fmt.Sprintf(b.body, ""))
			assert.Equal(t, []string{f.ids[2], f.ids[3]}, idsOf(resp))
			for _, n := range resp {
				assert.NotEqual(t, true, n["isHidden"])
			}
		})
	}
}

// 上限の回数まで続きを取れば届くノートは返し、届かなければ空で返す。隠れる
// ノートを isHidden の形で返すと、検索語に一致したことが分かるため。
func TestSearch_RefillCapReturnsEmptyBeyondCap(t *testing.T) {
	const limit = 2
	fetches := limit * (1 + maxSearchRefills)
	for _, b := range refillBackends {
		t.Run(b.name+"/reachable", func(t *testing.T) {
			f := newRefillFixture(t, b.meili, hiddenFlags(fetches-1, 2))
			resp := f.call(t, b.endpoint, fmt.Sprintf(b.body, ""))
			assert.Equal(t, []string{f.ids[fetches-1]}, idsOf(resp))
		})
		t.Run(b.name+"/beyond cap", func(t *testing.T) {
			f := newRefillFixture(t, b.meili, hiddenFlags(fetches, 2))
			resp := f.call(t, b.endpoint, fmt.Sprintf(b.body, ""))
			assert.Empty(t, resp)
		})
	}
}

// limit=1 で、隠れるノートだけが一致する (件数が limit の倍数ちょうどで終わる)
// ときも、isHidden の形で返さない。
func TestSearch_LimitOneHiddenOnlyReturnsEmpty(t *testing.T) {
	for _, b := range refillBackends {
		for _, n := range []int{1, 2, 5} {
			t.Run(fmt.Sprintf("%s/%d", b.name, n), func(t *testing.T) {
				f := newRefillFixture(t, b.meili, hiddenFlags(n, 0))
				body := strings.Replace(fmt.Sprintf(b.body, ""), `"limit":2`, `"limit":1`, 1)
				require.Contains(t, body, `"limit":1`)
				resp := f.call(t, b.endpoint, body)
				assert.Empty(t, resp)
			})
		}
	}
}

// 結果の終わりまで読んで隠れるノートしか無ければ、空で返す (isHidden の形に
// 戻さない)。
func TestSearch_AllHiddenUntilEndReturnsEmpty(t *testing.T) {
	for _, b := range refillBackends {
		t.Run(b.name, func(t *testing.T) {
			f := newRefillFixture(t, b.meili, hiddenFlags(5, 0))
			resp := f.call(t, b.endpoint, fmt.Sprintf(b.body, ""))
			assert.Empty(t, resp)
		})
	}
}

// sinceId だけの要求は、SQL の backend では昇順に読むので、続きも新しい向きに取る。
// Meilisearch は sinceId があっても降順なので、続きは古い向きに取る。
func TestSearch_RefillFollowsSinceIDOrder(t *testing.T) {
	// 新しい順に: 見える 2 件、隠れる 2 件、sinceId の起点。
	flags := []bool{false, false, true, true, false}
	for _, b := range refillBackends {
		t.Run(b.name, func(t *testing.T) {
			f := newRefillFixture(t, b.meili, flags)
			resp := f.call(t, b.endpoint, fmt.Sprintf(b.body, `,"sinceId":"`+f.ids[4]+`"`))
			assert.ElementsMatch(t, []string{f.ids[0], f.ids[1]}, idsOf(resp))
		})
	}
	t.Run("meilisearch newest first", func(t *testing.T) {
		// 新しい順に: 隠れる 2 件、見える 2 件、sinceId の起点。
		f := newRefillFixture(t, true, []bool{true, true, false, false, false})
		resp := f.call(t, "search", `{"query":"secret","limit":2,"sinceId":"`+f.ids[4]+`"}`)
		assert.Equal(t, []string{f.ids[2], f.ids[3]}, idsOf(resp))
	})
}

// offset で読んでいる要求は、offset を進めて続きを取る。
func TestSearch_RefillAdvancesOffset(t *testing.T) {
	// 新しい順に: 読み飛ばす 1 件、隠れる 2 件、見える 2 件。
	f := newRefillFixture(t, true, []bool{false, true, true, false, false})
	resp := f.call(t, "search", `{"query":"secret","limit":2,"offset":1}`)
	assert.Equal(t, []string{f.ids[3], f.ids[4]}, idsOf(resp))
	require.Len(t, f.meili.requests, 2)
	assert.Equal(t, int64(3), f.meili.requests[1].Offset)
	assert.NotContains(t, f.meili.requests[1].Filter, "createdAt <")
}

// 追加で取った検索が失敗したら、notes/search は 500、notes/search-by-tag は
// 従来どおり空の配列を返す。
func TestSearchVisiblePage_FetchError(t *testing.T) {
	f := newRefillFixture(t, false, hiddenFlags(2, 2))
	calls := 0
	_, err := f.h.searchVisiblePage(t.Context(), nil, 2, searchPage{}, func(p searchPage) ([]*model.Note, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("boom")
		}
		return f.repo.SearchByTag([][]string{{"secret"}}, "", 2, p.sinceID, p.untilID, model.NoteSearchTagFilter{})
	})
	require.Error(t, err)
	assert.Equal(t, 2, calls)
	assert.Equal(t, "boom", errSearchByTagFetch{errors.New("boom")}.Error())
}
