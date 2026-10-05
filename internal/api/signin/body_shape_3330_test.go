package signin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/api/signin"
	"github.com/elythia-network/elythia/internal/core/twofactor"
	"github.com/elythia-network/elythia/internal/model"
)

func newPasskeyTestHandler(t *testing.T) *signin.Handler {
	t.Helper()
	if signinTestRedis == nil {
		t.Skip("redis testcontainer unavailable")
	}
	signinTestRedis.FlushAll(context.Background())
	h, _ := newTestHandler(t)
	svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
	require.NoError(t, err)
	h.SetWebAuthn(svc, &inMemorySK{keys: map[string][]*model.UserSecurityKey{}})
	return h
}

func postWithContentType(h func(echo.Context) error, contentType, body string) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/signin-with-passkey", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set(echo.HeaderContentType, contentType)
	}
	rec := httptest.NewRecorder()
	_ = h(e.NewContext(req, rec))
	return rec
}

func assertPasskeyChallenge(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotNil(t, resp["option"])
	assert.NotEmpty(t, resp["context"])
}

// Upstream SigninWithPasskeyApiService reads `body['credential']` without
// checking the body's type and starts a challenge when it is falsy, so a
// body that is not an object and a falsy credential both get a challenge.
func TestSigninWithPasskey_NoCredentialStartsChallengeLikeUpstream(t *testing.T) {
	h := newPasskeyTestHandler(t)
	for _, body := range []string{
		`[]`,
		`"credential"`,
		`1`,
		`true`,
		`null`,
		`{"credential":null}`,
		`{"credential":false}`,
		`{"credential":0}`,
		`{"credential":-0.0}`,
		`{"credential":""}`,
		// credential が無ければ context は読まれない (型が違っても 400 にしない)。
		`{"credential":null,"context":1}`,
		`{"context":{"a":1}}`,
	} {
		t.Run(body, func(t *testing.T) {
			assertPasskeyChallenge(t, doPost(h.SigninWithPasskey, body))
		})
	}
	t.Run("text/plain body", func(t *testing.T) {
		assertPasskeyChallenge(t, postWithContentType(h.SigninWithPasskey, "text/plain", `{"credential":{"id":"x"}}`))
	})
	t.Run("no body", func(t *testing.T) {
		assertPasskeyChallenge(t, postWithContentType(h.SigninWithPasskey, "", ""))
	})
}

// Upstream answers a context that is not a string with the same 400
// (1658cc2e) as a malformed one; mk-go used to answer it with its own
// ed1d7571 id.
func TestSigninWithPasskey_NonStringContextIsInvalidContext(t *testing.T) {
	h := newPasskeyTestHandler(t)
	for _, ctx := range []string{`1`, `true`, `{}`, `[]`, `null`, `["00112233445566778899aabbccddeeff"]`} {
		t.Run(ctx, func(t *testing.T) {
			body := `{"credential":{"id":"x","rawId":"x","type":"public-key","response":{}},"context":` + ctx + `}`
			rec := doPost(h.SigninWithPasskey, body)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			var resp map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			errMap, ok := resp["error"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, "1658cc2e-4495-461f-aee4-d403cdf073c1", errMap["id"])
		})
	}
}

// A truthy credential that is not an assertion object is handed to the
// verifier like upstream does, which fails with b18c89a7 (its `id` is
// undefined, so @simplewebauthn throws `Missing credential ID`).
func TestSigninWithPasskey_TruthyNonObjectCredentialIsVerified(t *testing.T) {
	h := newPasskeyTestHandler(t)
	for _, cred := range []string{`true`, `"x"`, `1`, `[]`} {
		t.Run(cred, func(t *testing.T) {
			body := `{"credential":` + cred + `,"context":"` + passkeyChallengeContext(t, h) + `"}`
			rec := doPost(h.SigninWithPasskey, body)
			require.Equal(t, http.StatusForbidden, rec.Code)
			assert.Contains(t, rec.Body.String(), "b18c89a7-5b5e-4cec-bb5b-0419f332d430")
		})
	}
}

// Upstream reads `body['username']` etc. off the parsed JS object, so keys
// match only exactly. The handler must not depend on the echo serializer's
// matching (echo's default one ignores case, which is what these tests use).
func TestSigninFlow_KeysMatchExactly(t *testing.T) {
	t.Run("USERNAME is not username", func(t *testing.T) {
		h, repo := newTestHandler(t)
		createTestUser(repo, "alice", "pass")
		rec := doPost(h.SigninFlow, `{"USERNAME":"alice","password":"pass"}`)
		assertEmpty400(t, rec)
	})
	t.Run("legacy signin: USERNAME is not username", func(t *testing.T) {
		h, repo := newTestHandler(t)
		createTestUser(repo, "alice", "pass")
		rec := doPost(h.Signin, `{"USERNAME":"alice","password":"pass"}`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("PASSWORD is not password", func(t *testing.T) {
		h, repo := newTestHandler(t)
		newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", nil)
		rec := doPost(h.SigninFlow, `{"username":"alice","PASSWORD":"pass"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "password", resp["next"], "the password step must not be taken")
	})
	t.Run("CREDENTIAL is not credential", func(t *testing.T) {
		h, repo := newTestHandler(t)
		if signinTestRedis == nil {
			t.Skip("redis testcontainer unavailable")
		}
		signinTestRedis.FlushAll(context.Background())
		svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
		require.NoError(t, err)
		h.SetWebAuthn(svc, &inMemorySK{keys: map[string][]*model.UserSecurityKey{
			"u1": {{ID: "AAEC", PublicKey: "AwQF", UserID: "u1"}},
		}})
		newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", nil)
		rec := doPost(h.SigninFlow, `{"username":"alice","password":"pass","CREDENTIAL":{"id":"x","rawId":"x","type":"public-key","response":{}}}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, "passkey", resp["next"])
	})
}

// Upstream signin-flow takes the key branch only when `body.credential` is
// truthy. A falsy credential falls through to issuing the passkey challenge
// instead of failing verification with 93b86c4b.
func TestSigninFlow_FalsyCredentialIssuesChallenge(t *testing.T) {
	for _, cred := range []string{`null`, `false`, `0`, `""`} {
		t.Run(cred, func(t *testing.T) {
			h, repo := newTestHandler(t)
			if signinTestRedis == nil {
				t.Skip("redis testcontainer unavailable")
			}
			signinTestRedis.FlushAll(context.Background())
			svc, err := twofactor.NewWebAuthnService("https://example.com", "Misskey", signinTestRedis.Client)
			require.NoError(t, err)
			h.SetWebAuthn(svc, &inMemorySK{keys: map[string][]*model.UserSecurityKey{
				"u1": {{ID: "AAEC", PublicKey: "AwQF", UserID: "u1"}},
			}})
			newTestUserWithTOTP(repo, "alice", "pass", "JBSWY3DPEHPK3PXP", nil)

			rec := doPost(h.SigninFlow, `{"username":"alice","password":"pass","credential":`+cred+`}`)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, "passkey", resp["next"])
		})
	}
}
