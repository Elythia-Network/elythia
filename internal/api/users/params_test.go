package users

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newParamsFixture wires a handler whose list endpoints all answer 200 for
// user1 (and its public list list1), so a request that passes Bind is visible
// as a 200.
func newParamsFixture(t *testing.T) (*Handler, *model.User) {
	t.Helper()
	h, repo := newTestHandler(t)
	viewer := addTestUser(repo)
	h.SetClipRepo(testutil.NewMockClipRepository())
	h.SetFlashRepo(testutil.NewMockFlashRepository())
	h.SetGalleryRepo(&stubGalleryRepo{})
	h.SetPageRepo(testutil.NewMockPageRepository())
	lists := testutil.NewMockUserListRepository()
	require.NoError(t, lists.Create(&model.UserList{ID: "list1", UserID: viewer.ID, Name: "l", IsPublic: true}))
	h.SetUserListRepo(lists)
	return h, viewer
}

func errorCode(rec *httptest.ResponseRecorder) string {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return env.Error.Code
}

// 本家の paramDef は整数の値を持つ小数 (10.0、1e1、100e0) を integer として通す (#3837)。
func TestUsersParams_AcceptIntegerValuedFractions(t *testing.T) {
	h, viewer := newParamsFixture(t)
	for _, tc := range []struct {
		name    string
		handler func(echo.Context) error
		body    string
	}{
		{"users", h.List, `{"limit":10.0,"offset":1e1}`},
		{"users/search", h.Search, `{"query":"a","limit":1e1,"offset":0.0}`},
		{"users/notes", h.Notes, `{"userId":"user1","limit":10.0,"sinceDate":1e1,"untilDate":4102444800000.0}`},
		{"users/followers", h.Followers, `{"userId":"user1","limit":10.0,"sinceDate":1e1,"untilDate":4102444800000.0}`},
		{"users/following", h.Following, `{"userId":"user1","limit":10.0,"sinceDate":1e1,"untilDate":4102444800000.0}`},
		{"users/clips", h.Clips, `{"userId":"user1","limit":10.0,"sinceDate":1e1,"untilDate":4102444800000.0}`},
		{"users/flashs", h.Flashs, `{"userId":"user1","limit":10.0,"sinceDate":1e1,"untilDate":4102444800000.0}`},
		{"users/gallery/posts", h.GalleryPosts, `{"userId":"user1","limit":10.0,"sinceDate":1e1,"untilDate":4102444800000.0}`},
		{"users/pages", h.Pages, `{"userId":"user1","limit":10.0,"sinceDate":1e1,"untilDate":4102444800000.0}`},
		{"users/reactions", h.Reactions, `{"userId":"user1","limit":10.0,"sinceDate":1e1,"untilDate":4102444800000.0}`},
		{"users/featured-notes", h.FeaturedNotes, `{"userId":"user1","limit":100e0}`},
		{"users/search-by-username-and-host", h.SearchByUsernameAndHost, `{"username":"test","limit":10.0}`},
		{"users/lists/get-memberships", h.ListsGetMemberships, `{"listId":"list1","limit":10.0,"sinceDate":1e1,"untilDate":4102444800000.0}`},
		{"users/get-frequently-replied-users", h.GetFrequentlyRepliedUsers, `{"userId":"user1","limit":10.0}`},
		{"users/get-following-users-by-birthday single", h.GetFollowingUsersByBirthday, `{"limit":10.0,"offset":1e1,"birthday":{"month":1e1,"day":3.0}}`},
		{"users/get-following-users-by-birthday range", h.GetFollowingUsersByBirthday, `{"birthday":{"begin":{"month":12.0,"day":2.5e1},"end":{"month":1e0,"day":5.0}}}`},
		{"users/recommendation", h.UserRecommendation, `{"limit":10.0,"offset":1e1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postStub(tc.handler, tc.body, viewer)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}

// GET の users/featured-notes も、本家は query を JSON.parse してから ajv に渡すので
// limit=10.0 を 10 として通す (#3837)。
func TestFeaturedNotes_GETAcceptsIntegerValuedFractions(t *testing.T) {
	h, _ := newParamsFixture(t)
	for _, q := range []string{"limit=10.0", "limit=1e1"} {
		req := httptest.NewRequest(http.MethodGet, "/api/users/featured-notes?userId=user1&"+q, nil)
		rec := httptest.NewRecorder()
		require.NoError(t, h.FeaturedNotes(echo.New().NewContext(req, rec)))
		assert.Equal(t, http.StatusOK, rec.Code, "%s: %s", q, rec.Body.String())
	}
}

// offset は本家の paramDef に無く、本家は知らない項目として無視する。整数でない値を
// 送っても拒否しない (#3837)。
func TestUsersParams_IgnoreOffsetUpstreamDoesNotHave(t *testing.T) {
	h, viewer := newParamsFixture(t)
	for name, handler := range map[string]func(echo.Context) error{
		"users/clips":         h.Clips,
		"users/flashs":        h.Flashs,
		"users/gallery/posts": h.GalleryPosts,
		"users/pages":         h.Pages,
		"users/followers":     h.Followers,
		"users/following":     h.Following,
	} {
		t.Run(name, func(t *testing.T) {
			rec := postStub(handler, `{"userId":"user1","offset":"x"}`, viewer)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}

// offset を読まないので、件数も飛ばさない (#3837)。
func TestUsersParams_OffsetDoesNotSkipRows(t *testing.T) {
	h, viewer := newParamsFixture(t)
	clips := testutil.NewMockClipRepository()
	for _, id := range []string{"c1", "c2"} {
		require.NoError(t, clips.Create(&model.Clip{ID: id, UserID: "user1", Name: id, IsPublic: true}))
	}
	h.SetClipRepo(clips)
	rec := postStub(h.Clips, `{"userId":"user1","offset":1}`, viewer)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Len(t, rows, 2)
}

// 本家は空の username も paramDef で通し、該当する利用者がいないので NO_SUCH_USER
// を返す (#3837)。
func TestRelations_EmptyUsernameIsNoSuchUser(t *testing.T) {
	h, _ := newParamsFixture(t)
	for _, tc := range []struct {
		name    string
		handler func(echo.Context) error
		id      string
	}{
		{"users/followers", h.Followers, "27fa5435-88ab-43de-9360-387de88727cd"},
		{"users/following", h.Following, "63e4aba4-4156-4e53-be25-c9559e42d71b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, body := range []string{`{"username":"","host":null}`, `{"username":"","host":"remote.example"}`} {
				rec := postStub(tc.handler, body, nil)
				assert.Equal(t, http.StatusBadRequest, rec.Code, body)
				assert.Equal(t, "NO_SUCH_USER", errorCode(rec), body)
				assert.Contains(t, rec.Body.String(), tc.id, body)
			}
			// どちらの形も無いものは、今までどおり 400。
			rec := postStub(tc.handler, `{}`, nil)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, "INVALID_PARAM", errorCode(rec))
		})
	}
}

// 本家は `'userId' in ps` で分岐する。空の userId や null の userId を username と
// 一緒に送っても、username の利用者の一覧は返さない (#3837)。
func TestRelations_UserIDKeyWinsOverUsername(t *testing.T) {
	h, viewer := newParamsFixture(t)
	for _, tc := range []struct {
		name    string
		handler func(echo.Context) error
		id      string
	}{
		{"users/followers", h.Followers, "27fa5435-88ab-43de-9360-387de88727cd"},
		{"users/following", h.Following, "63e4aba4-4156-4e53-be25-c9559e42d71b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// username だけなら、その利用者の一覧を返す (比べる基準)。
			rec := postStub(tc.handler, `{"username":"`+viewer.Username+`","host":null}`, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			for _, body := range []string{
				`{"userId":"","username":"` + viewer.Username + `","host":null}`,
				`{"userId":null,"username":"` + viewer.Username + `","host":null}`,
			} {
				rec := postStub(tc.handler, body, nil)
				assert.Equal(t, http.StatusBadRequest, rec.Code, body)
				assert.Equal(t, "NO_SUCH_USER", errorCode(rec), body)
				assert.Contains(t, rec.Body.String(), tc.id, body)
			}
			// userId だけなら、今までどおりその利用者を引く。
			rec = postStub(tc.handler, `{"userId":"`+viewer.ID+`"}`, nil)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}
