package signin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/api/signin"
	"github.com/shiroha-a/mk/internal/core/captcha"
	"github.com/shiroha-a/mk/internal/core/twofactor"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/testutil"
)

// assertEmpty400 asserts upstream's `reply.code(400); return;`: a 400 with
// no body and no Content-Type.
func assertEmpty400(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Empty(t, rec.Body.String())
	assert.Empty(t, rec.Header().Get("Content-Type"))
}

func assertErrorID(t *testing.T, rec *httptest.ResponseRecorder, status int, id string) {
	t.Helper()
	require.Equal(t, status, rec.Code, rec.Body.String())
	var resp struct {
		Error map[string]any `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, id, resp.Error["id"])
}

// Upstream SigninApiService answers a username that is not a string with an
// empty 400 before looking the user up. A body that is not an object has no
// username either.
func TestSigninFlow_NonStringUsernameIsEmpty400(t *testing.T) {
	for _, body := range []string{
		`{}`,
		`{"username":null}`,
		`{"username":1}`,
		`{"username":true}`,
		`{"username":{"a":1}}`,
		`{"username":["alice"]}`,
		`[]`,
		`"alice"`,
		`1`,
		`null`,
	} {
		t.Run(body, func(t *testing.T) {
			h, repo := newTestHandler(t)
			createTestUser(repo, "alice", "pass")
			assertEmpty400(t, doPost(h.SigninFlow, body))
		})
	}
}

// An empty username is still a string, so upstream looks it up and answers
// 404 6cc579cc.
func TestSigninFlow_EmptyStringUsernameIsNotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	assertErrorID(t, doPost(h.SigninFlow, `{"username":""}`), http.StatusNotFound, "6cc579cc-885d-43d8-95c2-b8c7fc963280")
}

// A token that is neither null nor a string is an empty 400, checked before
// the user is looked up (so a nonexistent user gets 400, not 404).
func TestSigninFlow_NonStringTokenIsEmpty400(t *testing.T) {
	for _, tok := range []string{`1`, `true`, `false`, `{}`, `[]`} {
		t.Run(tok, func(t *testing.T) {
			h, repo := newTestHandler(t)
			newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", nil)
			assertEmpty400(t, doPost(h.SigninFlow, `{"username":"alice","password":"pass","token":`+tok+`}`))
			assertEmpty400(t, doPost(h.SigninFlow, `{"username":"ghost","token":`+tok+`}`))
		})
	}
	t.Run("null is no token", func(t *testing.T) {
		h, repo := newTestHandler(t)
		newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", nil)
		rec := doPost(h.SigninFlow, `{"username":"alice","password":"pass","token":null}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"next":"totp"`)
	})
}

// A password that is neither null nor a string is an empty 400, checked after
// the user lookup and the suspension check. null is the first step.
func TestSigninFlow_NonStringPasswordIsEmpty400(t *testing.T) {
	for _, pw := range []string{`1`, `true`, `{}`, `[]`} {
		t.Run(pw, func(t *testing.T) {
			h, repo := newTestHandler(t)
			createTestUser(repo, "alice", "pass")
			assertEmpty400(t, doPost(h.SigninFlow, `{"username":"alice","password":`+pw+`}`))
			assertErrorID(t, doPost(h.SigninFlow, `{"username":"ghost","password":`+pw+`}`),
				http.StatusNotFound, "6cc579cc-885d-43d8-95c2-b8c7fc963280")
			repo.Users["u1"].IsSuspended = true
			assertErrorID(t, doPost(h.SigninFlow, `{"username":"alice","password":`+pw+`}`),
				http.StatusForbidden, "e03a5f46-d309-4865-9b69-56282d94e1eb")
		})
	}
	t.Run("null is the first step", func(t *testing.T) {
		h, repo := newTestHandler(t)
		createTestUser(repo, "alice", "pass")
		rec := doPost(h.SigninFlow, `{"username":"alice","password":null}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"next":"captcha"`)
	})
}

// A captcha response that is not a string fails captcha verification like
// upstream (which hands it to the provider), instead of failing the body.
func TestSigninFlow_NonStringCaptchaResponseFailsCaptcha(t *testing.T) {
	for _, v := range []string{`1`, `true`, `{}`, `["testcaptcha-passed"]`, `null`} {
		t.Run(v, func(t *testing.T) {
			h, repo := newTestHandler(t)
			createTestUser(repo, "alice", "pass")
			h.SetCaptcha(captcha.NewService(&model.Meta{EnableTestcaptcha: true}))
			rec := doPost(h.SigninFlow, `{"username":"alice","password":"pass","testcaptcha-response":`+v+`}`)
			testutil.AssertFastifyError(t, rec, http.StatusBadRequest, "CAPTCHA_FAILED")
		})
	}
	t.Run("captcha disabled ignores the value", func(t *testing.T) {
		h, repo := newTestHandler(t)
		createTestUser(repo, "alice", "pass")
		rec := doPost(h.SigninFlow, `{"username":"alice","password":"pass","hcaptcha-response":1}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), `"finished":true`)
	})
}

// --- signin-with-passkey: one error id per failure, like upstream ---

// passkeyChallengeContext starts a passkey challenge and returns its context.
func passkeyChallengeContext(t *testing.T, h *signin.Handler) string {
	t.Helper()
	ctx, _ := passkeyChallenge(t, h)
	return ctx
}

func passkeyChallenge(t *testing.T, h *signin.Handler) (ctx, challenge string) {
	t.Helper()
	rec := doPost(h.SigninWithPasskey, `{}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Option struct {
			Challenge string `json:"challenge"`
		} `json:"option"`
		Context string `json:"context"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Context, resp.Option.Challenge
}

func postPasskey(t *testing.T, h *signin.Handler, ctx string, cred json.RawMessage) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{"context": ctx, "credential": cred})
	require.NoError(t, err)
	return doPost(h.SigninWithPasskey, string(body))
}

// setupPasskeySignin wires a passwordless user u1 ("alice") with one stored
// key and returns the handler, the user repo and the key.
func setupPasskeySignin(t *testing.T, wrap func(*inMemorySK) repository.UserSecurityKeyRepository) (*signin.Handler, *testutil.MockUserRepository, *uvTestKey) {
	t.Helper()
	h, repo := newTestHandler(t)
	if signinTestRedis == nil {
		t.Skip("redis testcontainer unavailable")
	}
	signinTestRedis.FlushAll(context.Background())
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
	require.NoError(t, err)
	key := newUVTestKey(t)
	store := &inMemorySK{keys: map[string][]*model.UserSecurityKey{"u1": {key.row(t, "u1")}}}
	var repoSK repository.UserSecurityKeyRepository = store
	if wrap != nil {
		repoSK = wrap(store)
	}
	h.SetWebAuthn(svc, repoSK)
	newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", nil)
	repo.Profiles["u1"].UsePasswordLessLogin = true
	return h, repo, key
}

// failingFindSK fails the key lookup like a database outage.
type failingFindSK struct{ *inMemorySK }

func (failingFindSK) FindByID(string) (*model.UserSecurityKey, error) { return nil, assert.AnError }

func TestSigninWithPasskey_Success(t *testing.T) {
	h, _, key := setupPasskeySignin(t, nil)
	ctx, ch := passkeyChallenge(t, h)
	rec := postPasskey(t, h, ctx, key.credential(t, ch, "u1", true))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"signinResponse"`)
}

// A context whose challenge does not exist (never issued, or already used)
// is upstream's 2d16e51c.
func TestSigninWithPasskey_ChallengeNotFound(t *testing.T) {
	h, _, key := setupPasskeySignin(t, nil)
	assertErrorID(t, postPasskey(t, h, "00112233445566778899aabbccddeeff", key.credential(t, "x", "u1", true)),
		http.StatusForbidden, "2d16e51c-007b-4edd-afd2-f7dd02c947f6")

	ctx, ch := passkeyChallenge(t, h)
	require.Equal(t, http.StatusOK, postPasskey(t, h, ctx, key.credential(t, ch, "u1", true)).Code)
	assertErrorID(t, postPasskey(t, h, ctx, key.credential(t, ch, "u1", true)),
		http.StatusForbidden, "2d16e51c-007b-4edd-afd2-f7dd02c947f6")
}

// A credential id no stored key has is upstream's 36b96a7d, and the
// challenge is consumed anyway (upstream getdel's it first).
func TestSigninWithPasskey_UnknownKey(t *testing.T) {
	h, _, _ := setupPasskeySignin(t, nil)
	stranger := newUVTestKey(t)
	stranger.credID = []byte("someone-elses-key")

	ctx, ch := passkeyChallenge(t, h)
	assertErrorID(t, postPasskey(t, h, ctx, stranger.credential(t, ch, "u1", true)),
		http.StatusForbidden, "36b96a7d-b547-412d-aeed-2d611cdc8cdc")
	assertErrorID(t, postPasskey(t, h, ctx, stranger.credential(t, ch, "u1", true)),
		http.StatusForbidden, "2d16e51c-007b-4edd-afd2-f7dd02c947f6")

	ctx = passkeyChallengeContext(t, h)
	assertErrorID(t, postPasskey(t, h, ctx, json.RawMessage(`{"id":1,"rawId":"x","type":"public-key","response":{}}`)),
		http.StatusForbidden, "36b96a7d-b547-412d-aeed-2d611cdc8cdc")
}

// A well-formed assertion whose signature does not verify makes upstream's
// verifier return null, which is 932c904e.
func TestSigninWithPasskey_SignatureMismatch(t *testing.T) {
	h, _, key := setupPasskeySignin(t, nil)
	forger := newUVTestKey(t)
	forger.credID = key.credID

	ctx, ch := passkeyChallenge(t, h)
	assertErrorID(t, postPasskey(t, h, ctx, forger.credential(t, ch, "u1", true)),
		http.StatusForbidden, "932c904e-9460-45b7-9ce6-7ed33be7eb2c")
}

// Any other rejection is upstream's b18c89a7: a missing UV flag, a missing
// credential id.
func TestSigninWithPasskey_VerificationFailed(t *testing.T) {
	h, _, key := setupPasskeySignin(t, nil)

	ctx, ch := passkeyChallenge(t, h)
	assertErrorID(t, postPasskey(t, h, ctx, key.credential(t, ch, "u1", false)),
		http.StatusForbidden, "b18c89a7-5b5e-4cec-bb5b-0419f332d430")

	ctx, ch = passkeyChallenge(t, h)
	var cred map[string]any
	require.NoError(t, json.Unmarshal(key.credential(t, ch, "u1", true), &cred))
	delete(cred, "id")
	raw, err := json.Marshal(cred)
	require.NoError(t, err)
	assertErrorID(t, postPasskey(t, h, ctx, raw),
		http.StatusForbidden, "b18c89a7-5b5e-4cec-bb5b-0419f332d430")
}

// The key's owner not being a local user is reported after verification,
// as upstream's 652f899f.
func TestSigninWithPasskey_OwnerMissing(t *testing.T) {
	h, repo, key := setupPasskeySignin(t, nil)
	delete(repo.Users, "u1")

	ctx, ch := passkeyChallenge(t, h)
	assertErrorID(t, postPasskey(t, h, ctx, key.credential(t, ch, "u1", true)),
		http.StatusForbidden, "652f899f-66d4-490e-993e-6606c8ec04c3")
}

// A database outage is not an authentication failure (#2792).
func TestSigninWithPasskey_KeyLookupFailureIs500(t *testing.T) {
	h, _, key := setupPasskeySignin(t, func(s *inMemorySK) repository.UserSecurityKeyRepository { return failingFindSK{s} })
	ctx, ch := passkeyChallenge(t, h)
	rec := postPasskey(t, h, ctx, key.credential(t, ch, "u1", true))
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "INTERNAL_ERROR")
}
