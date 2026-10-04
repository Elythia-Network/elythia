package users

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	corefollowing "github.com/shiroha-a/mk/internal/core/following"
	coreuser "github.com/shiroha-a/mk/internal/core/user"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/server/middleware"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// countingRemoteResolver records how many times the remote resolution path was
// entered, so a test can assert that users/followers and users/following never
// fetch an unknown acct over WebFinger.
type countingRemoteResolver struct {
	repo  *testutil.MockUserRepository
	calls int
}

func (r *countingRemoteResolver) ResolveByUsernameHost(username, host string) (*model.User, error) {
	r.calls++
	// 実物の resolver は解決したリモート user 行を作る。stub でも repo へ入れて、
	// 取りに行った場合に 200 が返る (= テストが落ちる) ようにする。
	h := host
	u := &model.User{
		ID:                "remoteResolved",
		Username:          username,
		UsernameLower:     strings.ToLower(username),
		Host:              &h,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	r.repo.Users[u.ID] = u
	return u, nil
}

func newRelationsLookupHandler(t *testing.T) (*Handler, *countingRemoteResolver) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	fRepo := testutil.NewMockFollowingRepository()
	frRepo := testutil.NewMockFollowRequestRepository()
	idGen, _ := id.NewGenerator("aidx")
	resolver := &countingRemoteResolver{repo: userRepo}
	svc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	svc.SetRemoteUserResolver(resolver)
	fSvc := corefollowing.NewService(userRepo, fRepo, frRepo, idGen)
	h := NewHandler(svc, fSvc, noteRepo, idGen)
	h.SetFollowingRepo(fRepo)
	h.SetFollowRequestRepo(frRepo)
	return h, resolver
}

func relationsStatus(t *testing.T, fn func(echo.Context) error, body string, viewer *model.User) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/users/followers", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if viewer != nil {
		c.Set(string(middleware.UserContextKey), viewer)
	}
	require.NoError(t, fn(c))
	return rec
}

var relationEndpoints = []struct {
	name string
	pick func(*Handler) func(echo.Context) error
	nsu  string
}{
	{"followers", func(h *Handler) func(echo.Context) error { return h.Followers }, "27fa5435-88ab-43de-9360-387de88727cd"},
	{"following", func(h *Handler) func(echo.Context) error { return h.Following }, "63e4aba4-4156-4e53-be25-c9559e42d71b"},
}

// 本家 followers.ts / following.ts は usersRepository.findOneBy だけで利用者を
// 引き、無ければ NO_SUCH_USER を返す。未知の acct を WebFinger で取りに行かない
// (#3330)。閲覧者の有無と ugcVisibility を問わない。
func TestRelations_UnknownRemoteAcctIsNotFetched(t *testing.T) {
	for _, ep := range relationEndpoints {
		for _, tc := range []struct {
			name   string
			viewer *model.User
			ugc    string
		}{
			{"anonymous/local", nil, "local"},
			{"anonymous/all", nil, "all"},
			{"authenticated/local", &model.User{ID: "viewer1"}, "local"},
		} {
			t.Run(ep.name+"/"+tc.name, func(t *testing.T) {
				h, resolver := newRelationsLookupHandler(t)
				h.SetUGCVisibility(tc.ugc)

				rec := relationsStatus(t, ep.pick(h), `{"username":"bob","host":"remote.example"}`, tc.viewer)
				assert.Equal(t, http.StatusNotFound, rec.Code)
				assert.Equal(t, 0, resolver.calls, "users/followers・following は DB だけを引く")

				var resp map[string]any
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				errObj := resp["error"].(map[string]any)
				assert.Equal(t, "NO_SUCH_USER", errObj["code"])
				// endpoint ごとに別 id が振られている (upstream と同じ)。
				assert.Equal(t, ep.nsu, errObj["id"])
			})
		}
	}
}

// DB にあるリモートの利用者は、匿名の閲覧者でも ugcVisibility が local でも引ける
// (本家の両 endpoint には visitor 向けの絞り込みが無い)。
func TestRelations_KnownRemoteUserResolvesFromDB(t *testing.T) {
	for _, ep := range relationEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			h, resolver := newRelationsLookupHandler(t)
			h.SetUGCVisibility("local")
			host := "remote.example"
			resolver.repo.Users["remote-bob"] = &model.User{
				ID: "remote-bob", Username: "Bob", UsernameLower: "bob", Host: &host,
				AvatarDecorations: datatypes.JSON([]byte("[]")),
			}

			rec := relationsStatus(t, ep.pick(h), `{"username":"BOB","host":"remote.example"}`, nil)
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, 0, resolver.calls)
		})
	}
}

// host 指定が無いローカルの利用者も DB から引く (前後の空白は除く、#2106 L10)。
func TestRelations_LocalLookup(t *testing.T) {
	h, resolver := newRelationsLookupHandler(t)
	h.SetUGCVisibility("local")
	resolver.repo.Users["local1"] = &model.User{
		ID: "local1", Username: "alice", UsernameLower: "alice",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	rec := relationsStatus(t, h.Followers, `{"username":" alice "}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 0, resolver.calls)
}

// DB 障害は not-found に丸めず 500 にする (#2792 / #2996)。
func TestRelations_LookupDBErrorIs500(t *testing.T) {
	for _, ep := range relationEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			h, resolver := newRelationsLookupHandler(t)
			resolver.repo.FindByUsernameLowerFn = func(string, *string) (*model.User, error) {
				return nil, errors.New("connection refused")
			}
			rec := relationsStatus(t, ep.pick(h), `{"username":"bob","host":"remote.example"}`, nil)
			assert.Equal(t, http.StatusInternalServerError, rec.Code)
			assert.Equal(t, 0, resolver.calls)
		})
	}
}
