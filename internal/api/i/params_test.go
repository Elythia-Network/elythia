package i

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// TestHandlers_IntegerParamsAcceptFractionForms pins that the integer params
// of the i/* list endpoints accept every JSON number with an integer value
// (10.0, 1e1), as upstream's ajv "type": "integer" does (#3851).
func TestHandlers_IntegerParamsAcceptFractionForms(t *testing.T) {
	h, _ := newExtraHandler(t)
	h.accessTokenRepo = testutil.NewMockAccessTokenRepository()
	h.SetGalleryRepo(&stubGalleryRepo{})
	h.SetPageRepo(testutil.NewMockPageRepository())
	h.SetPageLikeRepo(testutil.NewMockPageLikeRepository())
	h.SetSigninRepo(testutil.NewMockSigninRepository())
	const cursor = `{"limit":10.0,"sinceDate":1e1,"untilDate":4102444800000.0}`
	cases := []struct {
		name string
		fn   func(echo.Context) error
		body string
	}{
		{"i/favorites", h.Favorites, cursor},
		{"i/gallery/likes", h.GalleryLikes, cursor},
		{"i/gallery/posts", h.GalleryPosts, cursor},
		{"i/page-likes", h.PageLikes, cursor},
		{"i/signin-history", h.SigninHistory, cursor},
		{"i/authorized-apps", h.AuthorizedApps, `{"limit":1e1,"offset":1.0}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postExtra(tc.fn, tc.body, stubUser)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}

// offsetRecordingGalleryRepo records the offset the handler asks for.
type offsetRecordingGalleryRepo struct {
	stubGalleryRepo
	offsets []int
}

func (r *offsetRecordingGalleryRepo) ListByUser(_, _, _ string, _, offset int) ([]*model.GalleryPost, error) {
	r.offsets = append(r.offsets, offset)
	return nil, nil
}

func (r *offsetRecordingGalleryRepo) ListLikesByUser(_, _, _ string, _, offset int) ([]*model.GalleryLike, error) {
	r.offsets = append(r.offsets, offset)
	return nil, nil
}

// offsetRecordingPageLikeRepo records the offset the handler asks for.
type offsetRecordingPageLikeRepo struct {
	*testutil.MockPageLikeRepository
	offsets []int
}

func (r *offsetRecordingPageLikeRepo) ListByUser(_, _, _ string, _, offset int) ([]*model.PageLike, error) {
	r.offsets = append(r.offsets, offset)
	return nil, nil
}

// 本家の i/gallery/likes・i/gallery/posts・i/page-likes の paramDef に offset は
// 無い。本家は読まないので、型の違う値も通し、件数も飛ばさない (#3851)。
func TestHandlers_OffsetIsNotRead(t *testing.T) {
	h, _ := newExtraHandler(t)
	gallery := &offsetRecordingGalleryRepo{}
	pageLikes := &offsetRecordingPageLikeRepo{MockPageLikeRepository: testutil.NewMockPageLikeRepository()}
	h.SetGalleryRepo(gallery)
	h.SetPageRepo(testutil.NewMockPageRepository())
	h.SetPageLikeRepo(pageLikes)
	for name, fn := range map[string]func(echo.Context) error{
		"i/gallery/likes": h.GalleryLikes,
		"i/gallery/posts": h.GalleryPosts,
		"i/page-likes":    h.PageLikes,
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, http.StatusOK, postExtra(fn, `{"offset":"x"}`, stubUser).Code)
			assert.Equal(t, http.StatusOK, postExtra(fn, `{"offset":5}`, stubUser).Code)
		})
	}
	assert.Equal(t, []int{0, 0, 0, 0}, gallery.offsets)
	assert.Equal(t, []int{0, 0}, pageLikes.offsets)
}

// errorOf returns the error code and id of an API error body.
func errorOf(t *testing.T, body []byte) (code, id string) {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
			ID   string `json:"id"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &env), string(body))
	return env.Error.Code, env.Error.ID
}

// 空のパスワードは、本家と同じくパスワードの照合に通す。2 段階認証の操作は本家と
// 同じ INCORRECT_PASSWORD、i/change-password・i/delete-account・
// i/regenerate-token は Elythia の「パスワードが違う」(本家は 500) にする。キーが
// 無いものは本家も 400 なので、今までどおり INVALID_PARAM にする (#3851)。
func TestHandlers_EmptyPasswordIsIncorrect(t *testing.T) {
	cases := []struct {
		name            string
		fn              func(*Handler) func(echo.Context) error
		empty, missing  string
		incorrectID     string
		requireWebAuthn bool
	}{
		{"i/2fa/register", func(h *Handler) func(echo.Context) error { return h.TwoFARegister },
			`{"password":""}`, `{}`, "78d6c839-20c9-4c66-b90a-fc0542168b48", false},
		{"i/2fa/unregister", func(h *Handler) func(echo.Context) error { return h.TwoFAUnregister },
			`{"password":""}`, `{}`, "7add0395-9901-4098-82f9-4f67af65f775", false},
		{"i/2fa/register-key", func(h *Handler) func(echo.Context) error { return h.TwoFARegisterKey },
			`{"password":""}`, `{}`, "38769596-efe2-4faf-9bec-abbb3f2cd9ba", true},
		{"i/2fa/key-done", func(h *Handler) func(echo.Context) error { return h.TwoFAKeyDone },
			`{"password":"","name":"k","credential":{}}`, `{"name":"k","credential":{}}`, "0d7ec6d2-e652-443e-a7bf-9ee9a0cd77b0", true},
		{"i/2fa/remove-key", func(h *Handler) func(echo.Context) error { return h.TwoFARemoveKey },
			`{"password":"","credentialId":"k"}`, `{"credentialId":"k"}`, "141c598d-a825-44c8-9173-cfb9d92be493", true},
		{"i/change-password", func(h *Handler) func(echo.Context) error { return h.ChangePassword },
			`{"currentPassword":"","newPassword":"next"}`, `{"newPassword":"next"}`, "932c904e-9460-45b7-9ce6-7ed33be7eb2c", false},
		{"i/delete-account", func(h *Handler) func(echo.Context) error { return h.DeleteAccount },
			`{"password":""}`, `{}`, "932c904e-9460-45b7-9ce6-7ed33be7eb2c", false},
		{"i/regenerate-token", func(h *Handler) func(echo.Context) error { return h.RegenerateToken },
			`{"password":""}`, `{}`, "932c904e-9460-45b7-9ce6-7ed33be7eb2c", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var h *Handler
			var repo *testutil.MockUserRepository
			if tc.requireWebAuthn {
				h, repo, _ = newWebAuthnHandler(t)
			} else {
				h, repo, _ = newDeleteAccountHandler(t)
			}
			user := setupUserWithPassword(repo, "u1", "pass")
			rec := postExtra(tc.fn(h), tc.empty, user)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			code, id := errorOf(t, rec.Body.Bytes())
			assert.Equal(t, "INCORRECT_PASSWORD", code)
			assert.Equal(t, tc.incorrectID, id)
			// 削除・再発行・変更は起きない。
			assert.False(t, repo.Users["u1"].IsDeleted)
			assert.Equal(t, "tok12345678901234", *repo.Users["u1"].Token)

			rec = postExtra(tc.fn(h), tc.missing, user)
			testutil.AssertInvalidParam(t, rec)
		})
	}
}

// i/2fa/remove-key の空の credentialId は、本家と同じくパスワードを照合した後で
// 消しに行き、一致する鍵が無いので何も消さずに {} を返す (#3851)。
func TestTwoFARemoveKey_EmptyCredentialIDRemovesNothing(t *testing.T) {
	h, repo, skRepo := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	require.NoError(t, skRepo.Create(&model.UserSecurityKey{ID: "key1", UserID: "u1", Name: "k", PublicKey: "pk"}))
	rec := postExtra(h.TwoFARemoveKey, `{"password":"pass","credentialId":""}`, user)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `{}`, rec.Body.String())
	keys, err := skRepo.ListByUser("u1")
	require.NoError(t, err)
	assert.Len(t, keys, 1)
	// パスワードは空の credentialId でも照合する。
	rec = postExtra(h.TwoFARemoveKey, `{"password":"wrong","credentialId":""}`, user)
	code, _ := errorOf(t, rec.Body.Bytes())
	assert.Equal(t, "INCORRECT_PASSWORD", code)
}

// i/2fa/update-key の空の credentialId は、本家と同じく鍵を引きに行き
// NO_SUCH_KEY になる。キーが無いものは本家も 400 (#3851)。
func TestTwoFAUpdateKey_EmptyCredentialIDIsNoSuchKey(t *testing.T) {
	h, _ := newExtraHandler(t)
	h.securityKeyRepo = newInMemSKRepo()
	rec := postExtra(h.TwoFAUpdateKey, `{"name":"k","credentialId":""}`, stubUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	code, _ := errorOf(t, rec.Body.Bytes())
	assert.Equal(t, "NO_SUCH_KEY", code)
	testutil.AssertInvalidParam(t, postExtra(h.TwoFAUpdateKey, `{"name":"k"}`, stubUser))
}

// i/2fa/done の空の token は先に拒否せず、ほかの token と同じ経路を通す。設定を
// 始めていなければ 400、始めていれば照合して 403 INVALID_TOKEN (#3851)。
func TestTwoFADone_EmptyTokenTakesTheUsualPath(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	testutil.AssertInvalidParam(t, postExtra(h.TwoFADone, `{"token":""}`, user))
	testutil.AssertInvalidParam(t, postExtra(h.TwoFADone, `{}`, user))

	secret := "JBSWY3DPEHPK3PXP"
	repo.Profiles["u1"].TwoFactorTempSecret = &secret
	rec := postExtra(h.TwoFADone, `{"token":""}`, user)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	code, _ := errorOf(t, rec.Body.Bytes())
	assert.Equal(t, "INVALID_TOKEN", code)
	assert.False(t, repo.Profiles["u1"].TwoFactorEnabled)
	testutil.AssertInvalidParam(t, postExtra(h.TwoFADone, `{}`, user))
}

// i/move の空の moveToAccount は、本家と同じく root の判定より先に NO_SUCH_USER
// を返す。キーが無いものは本家も 400 (#3851)。
func TestMove_EmptyMoveToAccountIsNoSuchUser(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	sm := &stubMover{}
	h.SetAccountMover(sm)
	for _, u := range []*model.User{{ID: "me"}, {ID: "me", IsRoot: true}} {
		rec := post(h.Move, `{"moveToAccount":""}`, u)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		code, id := errorOf(t, rec.Body.Bytes())
		assert.Equal(t, "NO_SUCH_USER", code)
		assert.Equal(t, "fcd2eef9-a9b2-4c4f-8624-038099e90aa5", id)
	}
	assert.False(t, sm.called)
	testutil.AssertInvalidParam(t, post(h.Move, `{}`, &model.User{ID: "me"}))
}

// i/registry/get・get-detail・remove の空の key は、本家と同じく引きに行く (消し
// に行く)。一致する値は無いので、get と get-detail は NO_SUCH_KEY、remove は何も
// 消さずに 204。キーが無いものは本家も 400 (#3851)。
func TestRegistry_EmptyKeyIsLookedUp(t *testing.T) {
	h, _ := newExtraHandler(t)
	reg := testutil.NewMockRegistryRepository()
	require.NoError(t, reg.Set(&model.RegistryItem{ID: "r1", UserID: stubUser.ID, Key: "a", Scope: model.StringArray{}}))
	h.SetRegistryRepo(reg)

	for name, fn := range map[string]func(echo.Context) error{
		"i/registry/get":        h.RegistryGet,
		"i/registry/get-detail": h.RegistryGetDetail,
	} {
		rec := postExtra(fn, `{"key":"","scope":[]}`, stubUser)
		assert.Equal(t, http.StatusBadRequest, rec.Code, name)
		code, _ := errorOf(t, rec.Body.Bytes())
		assert.Equal(t, "NO_SUCH_KEY", code, name)
		testutil.AssertInvalidParam(t, postExtra(fn, `{"scope":[]}`, stubUser))
	}
	assert.Equal(t, http.StatusNoContent, postExtra(h.RegistryRemove, `{"key":"","scope":[]}`, stubUser).Code)
	_, err := reg.Get(stubUser.ID, "a", []string{}, nil)
	assert.NoError(t, err)
	testutil.AssertInvalidParam(t, postExtra(h.RegistryRemove, `{"scope":[]}`, stubUser))
}

// i/revoke-token の null や空の token は、本家と同じく何もせずに 204 を返す。
// tokenId も token も無いものは本家も 400 (#3851)。
func TestRevokeToken_NullOrEmptyTokenIsNoOp(t *testing.T) {
	h, _ := newExtraHandler(t)
	tokens := testutil.NewMockAccessTokenRepository()
	// 生の token を持たない (hash だけの) token。空の token で引くと一致してしまう。
	tokens.Tokens["h1"] = &model.AccessToken{ID: "t1", Hash: "h1", UserID: stubUser.ID}
	h.accessTokenRepo = tokens
	for _, body := range []string{`{"token":null}`, `{"token":""}`} {
		assert.Equal(t, http.StatusNoContent, postExtra(h.RevokeToken, body, stubUser).Code, body)
	}
	_, err := tokens.FindByID("t1")
	assert.NoError(t, err)
	testutil.AssertInvalidParam(t, postExtra(h.RevokeToken, `{}`, stubUser))
}
