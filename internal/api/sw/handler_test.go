package sw

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/shiroha-a/mk/internal/core/webpush"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

// --- Mock Repositories ---

var errMock = assert.AnError

type mockSwRepo struct {
	subs      map[string]*model.SwSubscription // userID:endpoint -> sub
	createErr error
	updateErr error
	// findErr injects an arbitrary error from FindByUserAndEndpoint regardless
	// of map state. 非 NotFound DB error の handler 分岐 (#918 / #917 観測性
	// pattern) を test するため。
	findErr   error
	deleteErr error
}

func newMockSwRepo() *mockSwRepo {
	return &mockSwRepo{subs: make(map[string]*model.SwSubscription)}
}

func (m *mockSwRepo) FindByUserAndEndpoint(userID, endpoint string) (*model.SwSubscription, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	key := userID + ":" + endpoint
	if s, ok := m.subs[key]; ok {
		return s, nil
	}
	// 実 repo (gorm) と同じく ErrRecordNotFound を返す。handler は errors.Is で
	// not-found / DB error を区別する設計。
	return nil, gorm.ErrRecordNotFound
}

func (m *mockSwRepo) FindByUserEndpointAuthKey(userID, endpoint, auth, publicKey string) (*model.SwSubscription, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	if s, ok := m.subs[userID+":"+endpoint]; ok && s.Auth == auth && s.PublicKey == publicKey {
		return s, nil
	}
	return nil, gorm.ErrRecordNotFound
}

func (m *mockSwRepo) FindByUserID(userID string) ([]*model.SwSubscription, error) {
	out := make([]*model.SwSubscription, 0)
	for _, s := range m.subs {
		if s.UserID == userID {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *mockSwRepo) Create(sub *model.SwSubscription) error {
	if m.createErr != nil {
		return m.createErr
	}
	m.subs[sub.UserID+":"+sub.Endpoint] = sub
	return nil
}

func (m *mockSwRepo) Update(sub *model.SwSubscription) error {
	if m.updateErr != nil {
		return m.updateErr
	}
	m.subs[sub.UserID+":"+sub.Endpoint] = sub
	return nil
}

func (m *mockSwRepo) FindByEndpointAuthKey(userID *string, endpoint, auth, publicKey string) ([]*model.SwSubscription, error) {
	if m.findErr != nil {
		return nil, m.findErr
	}
	out := make([]*model.SwSubscription, 0)
	for _, s := range m.subs {
		if userID != nil && s.UserID != *userID {
			continue
		}
		if s.Endpoint == endpoint && s.Auth == auth && s.PublicKey == publicKey {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *mockSwRepo) DeleteByIDs(ids []string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	for _, target := range ids {
		for k, s := range m.subs {
			if s.ID == target {
				delete(m.subs, k)
			}
		}
	}
	return nil
}

func (m *mockSwRepo) DeleteByUserAndEndpoint(userID, endpoint string) error {
	delete(m.subs, userID+":"+endpoint)
	return nil
}

type mockMetaRepo struct {
	meta *model.Meta
}

func (m *mockMetaRepo) Fetch() (*model.Meta, error) {
	if m.meta != nil {
		return m.meta, nil
	}
	return nil, errMock
}
func (m *mockMetaRepo) Update(_ map[string]any) error { return nil }
func (m *mockMetaRepo) EnsureInitial(_ string) error  { return nil }

func newTestHandler() (*Handler, *mockSwRepo) {
	repo := newMockSwRepo()
	swKey := "test-sw-key"
	metaRepo := &mockMetaRepo{meta: &model.Meta{SwPublicKey: &swKey}}
	idGen, _ := id.NewGenerator("aidx")
	return NewHandler(repo, metaRepo, idGen, nil), repo
}

func post(handler func(echo.Context) error, body string, user *model.User) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	_ = handler(c)
	return rec
}

// --- Register ---

func TestRegister_New(t *testing.T) {
	h, repo := newTestHandler()
	user := &model.User{ID: "u1"}
	rec := post(h.Register, `{"endpoint":"https://push.example/1","auth":"a1","publickey":"pk1"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "subscribed", resp["state"])
	assert.Equal(t, "test-sw-key", resp["key"])
	assert.Len(t, repo.subs, 1)
}

func TestRegister_AlreadySubscribed(t *testing.T) {
	h, repo := newTestHandler()
	// #1775: already-subscribed は (userId, endpoint, auth, publickey) が完全一致した
	// ときのみ。seed と request のキーを揃える。
	repo.subs["u1:https://push.example/1"] = &model.SwSubscription{
		ID: "s1", UserID: "u1", Endpoint: "https://push.example/1", Auth: "a1", PublicKey: "pk1", SendReadMessage: true,
	}
	rec := post(h.Register, `{"endpoint":"https://push.example/1","auth":"a1","publickey":"pk1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "already-subscribed", resp["state"])
}

// #1775: 同じ endpoint で auth/publickey が rotate した再 subscribe は
// already-subscribed にせず新規登録し、最新キーを永続化する (upstream は新行を
// insert。2-tuple match だと stale キーのままで Web Push が壊れていた)。
func TestRegister_KeyRotationInsertsFreshKeys(t *testing.T) {
	h, repo := newTestHandler()
	repo.subs["u1:https://push.example/1"] = &model.SwSubscription{
		ID: "s1", UserID: "u1", Endpoint: "https://push.example/1", Auth: "old-auth", PublicKey: "old-pk", SendReadMessage: true,
	}
	rec := post(h.Register, `{"endpoint":"https://push.example/1","auth":"new-auth","publickey":"new-pk"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "subscribed", resp["state"], "rotated keys must not match the stale row")
	// 最新キーが永続化されていること。
	stored := repo.subs["u1:https://push.example/1"]
	require.NotNil(t, stored)
	assert.Equal(t, "new-auth", stored.Auth)
	assert.Equal(t, "new-pk", stored.PublicKey)
}

func TestRegister_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.Register, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRegister_CreateError(t *testing.T) {
	h, repo := newTestHandler()
	repo.createErr = errMock
	rec := post(h.Register, `{"endpoint":"https://push.example/1","auth":"a1","publickey":"pk1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRegister_NoMeta(t *testing.T) {
	repo := newMockSwRepo()
	metaRepo := &mockMetaRepo{} // meta is nil
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(repo, metaRepo, idGen, nil)

	rec := post(h.Register, `{"endpoint":"https://push.example/1","auth":"a1","publickey":"pk1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, resp["key"])
}

// --- ShowRegistration ---

func TestShowRegistration_Found(t *testing.T) {
	h, repo := newTestHandler()
	repo.subs["u1:https://push.example/1"] = &model.SwSubscription{
		UserID: "u1", Endpoint: "https://push.example/1", SendReadMessage: false,
	}
	rec := post(h.ShowRegistration, `{"endpoint":"https://push.example/1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "u1", resp["userId"])
}

// TestShowRegistration_NotFound: upstream Misskey TS と同じく該当 row なし
// は 204 No Content を返す。旧実装は 200 + JSON null だったが、drop-in 互換
// のため 204 に揃えた (#918)。
func TestShowRegistration_NotFound(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.ShowRegistration, `{"endpoint":"https://ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String(), "204 response should have empty body")
}

// TestShowRegistration_DBError は #918 review fix の regression guard。
// gorm.ErrRecordNotFound 以外の error (= DB 障害等) は 204 で潰さず、500 で
// 観測性を保つ (#917 federation/show-instance と同 pattern)。
func TestShowRegistration_DBError(t *testing.T) {
	h, repo := newTestHandler()
	repo.findErr = errors.New("connection reset by peer")
	rec := post(h.ShowRegistration, `{"endpoint":"https://x"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestShowRegistration_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.ShowRegistration, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- UpdateRegistration ---

func TestUpdateRegistration_Success(t *testing.T) {
	h, repo := newTestHandler()
	repo.subs["u1:https://push.example/1"] = &model.SwSubscription{
		UserID: "u1", Endpoint: "https://push.example/1", SendReadMessage: false,
	}
	rec := post(h.UpdateRegistration, `{"endpoint":"https://push.example/1","sendReadMessage":true}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["sendReadMessage"])
}

// Updating a registration drops the owner's subscription cache, like
// upstream update-registration's refreshCache.
func TestUpdateRegistration_InvalidatesCache(t *testing.T) {
	h, repo, inv := newUnregisterHandler()
	repo.subs["u1:https://push.example/9"] = &model.SwSubscription{
		UserID: "u1", Endpoint: "https://push.example/9", SendReadMessage: false,
	}
	rec := post(h.UpdateRegistration, `{"endpoint":"https://push.example/9","sendReadMessage":true}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, []string{"u1"}, inv.users)
}

func TestUpdateRegistration_NoSendReadMessage(t *testing.T) {
	h, repo := newTestHandler()
	repo.subs["u1:https://push.example/1"] = &model.SwSubscription{
		UserID: "u1", Endpoint: "https://push.example/1", SendReadMessage: false,
	}
	rec := post(h.UpdateRegistration, `{"endpoint":"https://push.example/1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestUpdateRegistration_NotFound(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.UpdateRegistration, `{"endpoint":"https://ghost"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestUpdateRegistration_DBError は #918 review fix の regression guard。
// gorm.ErrRecordNotFound 以外の error (= DB 障害等) は 404 で潰さず、500 で
// 観測性を保つ。
func TestUpdateRegistration_DBError(t *testing.T) {
	h, repo := newTestHandler()
	repo.findErr = errors.New("connection reset by peer")
	rec := post(h.UpdateRegistration, `{"endpoint":"https://x"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestUpdateRegistration_InvalidParam(t *testing.T) {
	h, _ := newTestHandler()
	rec := post(h.UpdateRegistration, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpdateRegistration_UpdateError(t *testing.T) {
	h, repo := newTestHandler()
	repo.subs["u1:https://push.example/1"] = &model.SwSubscription{
		UserID: "u1", Endpoint: "https://push.example/1",
	}
	repo.updateErr = errMock
	rec := post(h.UpdateRegistration, `{"endpoint":"https://push.example/1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- Unregister ---

// seedUnregisterSubs stores the same browser subscription for two accounts
// (one browser shared by u1 and u2) plus an unrelated subscription of u1.
func seedUnregisterSubs(repo *mockSwRepo) {
	repo.subs["u1:https://push.example/1"] = &model.SwSubscription{
		ID: "s1", UserID: "u1", Endpoint: "https://push.example/1", Auth: "a1", PublicKey: "pk1",
	}
	repo.subs["u2:https://push.example/1"] = &model.SwSubscription{
		ID: "s2", UserID: "u2", Endpoint: "https://push.example/1", Auth: "a1", PublicKey: "pk1",
	}
	repo.subs["u1:https://push.example/other"] = &model.SwSubscription{
		ID: "s3", UserID: "u1", Endpoint: "https://push.example/other", Auth: "a3", PublicKey: "pk3",
	}
}

func remainingSubIDs(repo *mockSwRepo) []string {
	ids := make([]string, 0, len(repo.subs))
	for _, s := range repo.subs {
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)
	return ids
}

// recordingInvalidator records which users had their cache dropped.
type recordingInvalidator struct{ users []string }

func (r *recordingInvalidator) Invalidate(_ context.Context, userID string) {
	r.users = append(r.users, userID)
}

func newUnregisterHandler() (*Handler, *mockSwRepo, *recordingInvalidator) {
	repo := newMockSwRepo()
	seedUnregisterSubs(repo)
	inv := &recordingInvalidator{}
	idGen, _ := id.NewGenerator("aidx")
	return NewHandler(repo, &mockMetaRepo{}, idGen, inv), repo, inv
}

// assertInvalidParam checks upstream's schema-validation envelope for a
// failed paramDef check.
func assertInvalidParam(t *testing.T, rec *httptest.ResponseRecorder, param, reason string) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var resp struct {
		Error struct {
			Code string `json:"code"`
			ID   string `json:"id"`
			Kind string `json:"kind"`
			Info struct {
				Param  string `json:"param"`
				Reason string `json:"reason"`
			} `json:"info"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "INVALID_PARAM", resp.Error.Code)
	assert.Equal(t, "3d81ceae-475f-4600-b2a8-2bc116157532", resp.Error.ID)
	assert.Equal(t, "client", resp.Error.Kind)
	assert.Equal(t, param, resp.Error.Info.Param)
	assert.Equal(t, reason, resp.Error.Info.Reason)
}

// endpoint / auth / publickey are all required, as in upstream 2026.10.0, and
// a rejected request deletes nothing even when the endpoint alone matches.
func TestUnregister_RequiresEndpointAuthAndPublicKey(t *testing.T) {
	cases := []struct {
		name, body, missing string
	}{
		{"empty", `{}`, "endpoint"},
		{"endpoint only", `{"endpoint":"https://push.example/1"}`, "auth"},
		{"no publickey", `{"endpoint":"https://push.example/1","auth":"a1"}`, "publickey"},
		{"no auth", `{"endpoint":"https://push.example/1","publickey":"pk1"}`, "auth"},
	}
	for _, tc := range cases {
		for _, user := range []*model.User{nil, {ID: "u1"}} {
			t.Run(tc.name, func(t *testing.T) {
				h, repo, inv := newUnregisterHandler()
				rec := post(h.Unregister, tc.body, user)
				assertInvalidParam(t, rec, "#/required", "must have required property '"+tc.missing+"'")
				assert.Equal(t, []string{"s1", "s2", "s3"}, remainingSubIDs(repo))
				assert.Empty(t, inv.users)
			})
		}
	}
}

func TestUnregister_RejectsNonStringParams(t *testing.T) {
	for _, tc := range []struct{ body, param string }{
		{`{"endpoint":1,"auth":"a1","publickey":"pk1"}`, "endpoint"},
		{`{"endpoint":"https://push.example/1","auth":null,"publickey":"pk1"}`, "auth"},
		{`{"endpoint":"https://push.example/1","auth":"a1","publickey":["pk1"]}`, "publickey"},
	} {
		h, repo, _ := newUnregisterHandler()
		rec := post(h.Unregister, tc.body, nil)
		assertInvalidParam(t, rec, "#/properties/"+tc.param+"/type", "must be string")
		assert.Len(t, repo.subs, 3)
	}
}

func TestUnregister_InvalidJSON(t *testing.T) {
	h, repo, _ := newUnregisterHandler()
	rec := post(h.Unregister, `{invalid`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Len(t, repo.subs, 3)
}

// A mismatching auth or publickey matches nothing: 204 and every row stays.
func TestUnregister_WrongKeysDeleteNothing(t *testing.T) {
	for _, body := range []string{
		`{"endpoint":"https://push.example/1","auth":"wrong","publickey":"pk1"}`,
		`{"endpoint":"https://push.example/1","auth":"a1","publickey":"wrong"}`,
		`{"endpoint":"https://push.example/1","auth":"a3","publickey":"pk3"}`,
	} {
		for _, user := range []*model.User{nil, {ID: "u1"}} {
			h, repo, inv := newUnregisterHandler()
			rec := post(h.Unregister, body, user)
			assert.Equal(t, http.StatusNoContent, rec.Code)
			assert.Equal(t, []string{"s1", "s2", "s3"}, remainingSubIDs(repo), body)
			assert.Empty(t, inv.users)
		}
	}
}

// Without a credential every account registered with the subscription is
// removed (the browser drops the subscription itself), and each owner's cache
// is refreshed once.
func TestUnregister_AnonymousDeletesEveryAccountOfSubscription(t *testing.T) {
	h, repo, inv := newUnregisterHandler()
	rec := post(h.Unregister, `{"endpoint":"https://push.example/1","auth":"a1","publickey":"pk1"}`, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"s3"}, remainingSubIDs(repo))
	sort.Strings(inv.users)
	assert.Equal(t, []string{"u1", "u2"}, inv.users)
}

// With a credential only the caller's own registration is removed.
func TestUnregister_AuthenticatedDeletesOnlyOwn(t *testing.T) {
	h, repo, inv := newUnregisterHandler()
	rec := post(h.Unregister, `{"endpoint":"https://push.example/1","auth":"a1","publickey":"pk1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"s2", "s3"}, remainingSubIDs(repo))
	assert.Equal(t, []string{"u1"}, inv.users)
}

func TestUnregister_FindError(t *testing.T) {
	h, repo, inv := newUnregisterHandler()
	repo.findErr = errMock
	rec := post(h.Unregister, `{"endpoint":"https://push.example/1","auth":"a1","publickey":"pk1"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, inv.users)
}

func TestUnregister_DeleteError(t *testing.T) {
	h, repo, inv := newUnregisterHandler()
	repo.deleteErr = errMock
	rec := post(h.Unregister, `{"endpoint":"https://push.example/1","auth":"a1","publickey":"pk1"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Len(t, repo.subs, 3)
	assert.Empty(t, inv.users)
}

// The Web Push delivery reads subscriptions through webpush.SubscriptionCache
// (memory 3 min / Redis 1 h). register and unregister must drop the entry so
// the next delivery sees the change, as upstream's refreshCache does.
func TestRegisterAndUnregister_RefreshSubscriptionCache(t *testing.T) {
	repo := newMockSwRepo()
	cache := webpush.NewSubscriptionCache(repo, nil)
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(repo, &mockMetaRepo{}, idGen, cache)
	ctx := context.Background()

	subs, err := cache.Get(ctx, "u1")
	require.NoError(t, err)
	require.Empty(t, subs)

	rec := post(h.Register, `{"endpoint":"https://push.example/1","auth":"a1","publickey":"pk1"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	subs, err = cache.Get(ctx, "u1")
	require.NoError(t, err)
	assert.Len(t, subs, 1, "register left the cached empty list in place")

	rec = post(h.Unregister, `{"endpoint":"https://push.example/1","auth":"a1","publickey":"pk1"}`, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	subs, err = cache.Get(ctx, "u1")
	require.NoError(t, err)
	assert.Empty(t, subs, "unregister left the deleted subscription in the cache")
}

// already-subscribed creates nothing, so it does not need to touch the cache.
func TestRegister_AlreadySubscribedKeepsCache(t *testing.T) {
	repo := newMockSwRepo()
	repo.subs["u1:https://push.example/1"] = &model.SwSubscription{
		ID: "s1", UserID: "u1", Endpoint: "https://push.example/1", Auth: "a1", PublicKey: "pk1",
	}
	inv := &recordingInvalidator{}
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(repo, &mockMetaRepo{}, idGen, inv)
	rec := post(h.Register, `{"endpoint":"https://push.example/1","auth":"a1","publickey":"pk1"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, inv.users)
}

// failingSWRepo makes every subscription lookup look like a database failure.
type failingSWRepo struct {
	repository.SwSubscriptionRepository
	err error
}

func (r *failingSWRepo) FindByUserEndpointAuthKey(_, _, _, _ string) (*model.SwSubscription, error) {
	return nil, r.err
}

// **DB 障害で重複チェックを skip しない** (#2792)。
//
// `sw_subscription` に unique index は無いので、skip すると**重複行が恒久的に
// 残り、その端末へ web push が二重配信される**。
func TestRegister_DBFailureIsNot2xx(t *testing.T) {
	swKey := "test-sw-key"
	metaRepo := &mockMetaRepo{meta: &model.Meta{SwPublicKey: &swKey}}
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(&failingSWRepo{
		SwSubscriptionRepository: newMockSwRepo(),
		err:                      errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"),
	}, metaRepo, idGen, nil)

	rec := post(h.Register, `{"endpoint":"https://push.example/e","auth":"a","publickey":"k"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"DB 障害で重複チェックが skip されている (#2792)")
}

// **列に入らない値は 400 (#3025)。** ここは「見つからない」に丸めてはいけない
// 数少ない形 — 下の重複チェックは `IsNotFound` を「重複ではない」と読んで
// 新規登録へ進むので、通すと INSERT が SQLSTATE 22021 で落ちて 500 になる。
func TestRegister_RejectsUnstorableValues(t *testing.T) {
	for _, body := range []string{
		`{"endpoint":"a\u0000b","auth":"a1","publickey":"pk1"}`,
		`{"endpoint":"https://push.example/1","auth":"a\u0000b","publickey":"pk1"}`,
		`{"endpoint":"https://push.example/1","auth":"a1","publickey":"p\u0000k"}`,
	} {
		h, repo := newTestHandler()
		rec := post(h.Register, body, &model.User{ID: "u1"})
		assert.Equal(t, http.StatusBadRequest, rec.Code,
			"列に入らない値を INSERT へ流している: %s", rec.Body.String())
		assert.Empty(t, repo.subs, "弾いたはずの値で行を作っている")
	}
}

// assertInvalidEndpoint checks that rec carries upstream's sw/register
// invalidEndpoint error (code, id, kind and the default 400 status).
func assertInvalidEndpoint(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var resp struct {
		Error struct {
			Code string `json:"code"`
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "INVALID_ENDPOINT", resp.Error.Code)
	assert.Equal(t, "4432adbe-17c0-4f9f-b43c-9ceb2f8910fe", resp.Error.ID)
	assert.Equal(t, "client", resp.Error.Kind)
}

func TestRegister_RejectsInvalidEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"http://push.example/1",
		"https://user@push.example/1",
		"https://user:pass@push.example/1",
		"ftp://push.example/1",
		"push.example/1",
		"https:push.example/1",
	} {
		t.Run(endpoint, func(t *testing.T) {
			h, repo := newTestHandler()
			body, err := json.Marshal(map[string]any{"endpoint": endpoint, "auth": "a1", "publickey": "pk1"})
			require.NoError(t, err)
			rec := post(h.Register, string(body), &model.User{ID: "u1"})
			assertInvalidEndpoint(t, rec)
			assert.Empty(t, repo.subs)
		})
	}
}

// 既に同じ (userId, endpoint, auth, publickey) の行があっても、endpoint が
// 不正なら already-subscribed ではなく INVALID_ENDPOINT を返す (本家は
// 既存の確認より前に検証する)。
func TestRegister_InvalidEndpointCheckedBeforeExisting(t *testing.T) {
	h, repo := newTestHandler()
	repo.subs["u1:http://push.example/1"] = &model.SwSubscription{
		ID: "s1", UserID: "u1", Endpoint: "http://push.example/1", Auth: "a1", PublicKey: "pk1",
	}
	rec := post(h.Register, `{"endpoint":"http://push.example/1","auth":"a1","publickey":"pk1"}`, &model.User{ID: "u1"})
	assertInvalidEndpoint(t, rec)
}
