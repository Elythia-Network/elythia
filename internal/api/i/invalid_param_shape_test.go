package i

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/model"
)

// invalidParamBody decodes an INVALID_PARAM response and checks the parts
// every ajv failure shares with upstream (endpoint-base.ts): code, message
// and id 3d81ceae. It returns the info object (nil when absent).
func invalidParamBody(t *testing.T, code int, body []byte) any {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, code, string(body))
	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	errObj := out["error"].(map[string]any)
	assert.Equal(t, "INVALID_PARAM", errObj["code"])
	assert.Equal(t, "Invalid param.", errObj["message"])
	assert.Equal(t, apierr.UUIDInvalidParam, errObj["id"])
	return errObj["info"]
}

// TestClaimAchievement_AjvInfo pins upstream's ajv error for
// i/claim-achievement (paramDef: required ['name'], name type string and enum
// ACHIEVEMENT_TYPES) instead of mk-go's former id ed1d7571-….
func TestClaimAchievement_AjvInfo(t *testing.T) {
	cases := []struct {
		name, body string
		want       map[string]any
	}{
		{"missing name", `{}`, map[string]any{"param": "#/required", "reason": "must have required property 'name'"}},
		{"null name", `{"name":null}`, map[string]any{"param": "#/properties/name/type", "reason": "must be string"}},
		{"number name", `{"name":1}`, map[string]any{"param": "#/properties/name/type", "reason": "must be string"}},
		{"empty name", `{"name":""}`, map[string]any{"param": "#/properties/name/enum", "reason": "must be equal to one of the allowed values"}},
		{"unknown name", `{"name":"bogusAchievement"}`, map[string]any{"param": "#/properties/name/enum", "reason": "must be equal to one of the allowed values"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newExtraHandler(t)
			rec := postExtra(h.ClaimAchievement, tc.body, &model.User{ID: "u1"})
			assert.Equal(t, tc.want, invalidParamBody(t, rec.Code, rec.Body.Bytes()))
		})
	}
}

// TestTwoFAUpdateKey_NameLengthInCodePoints pins that the 30-character cap
// counts code points like ajv's maxLength (a 30-character Japanese name is
// accepted; it is 90 bytes) and that a longer name gets ajv's info.
func TestTwoFAUpdateKey_NameLengthInCodePoints(t *testing.T) {
	h, repo, skRepo := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	require.NoError(t, skRepo.Create(&model.UserSecurityKey{ID: "key1", UserID: "u1", Name: "old", PublicKey: "pk"}))

	name30 := strings.Repeat("鍵", 30)
	rec := postExtra(h.TwoFAUpdateKey, `{"credentialId":"key1","name":"`+name30+`"}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, name30, skRepo.keys["key1"].Name)

	rec = postExtra(h.TwoFAUpdateKey, `{"credentialId":"key1","name":"`+name30+`a"}`, user)
	info := invalidParamBody(t, rec.Code, rec.Body.Bytes())
	assert.Equal(t, map[string]any{"param": "#/properties/name/maxLength", "reason": "must NOT have more than 30 characters"}, info)
	assert.Equal(t, name30, skRepo.keys["key1"].Name)
}

// TestTwoFAKeyDone_NameTooLongInfo pins the same ajv info on
// i/2fa/key-done, with the cap counted in code points: a 30-character
// Japanese name (90 bytes) passes the length check, 31 characters do not.
func TestTwoFAKeyDone_NameTooLongInfo(t *testing.T) {
	h, repo, _ := newWebAuthnHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	keyDone := func(name string) (int, string) {
		body := `{"password":"pass","token":"backup1","name":"` + name + `","credential":{"id":"x"}}`
		rec := postExtra(h.TwoFAKeyDone, body, user)
		return rec.Code, rec.Body.String()
	}
	maxLengthInfo := map[string]any{"param": "#/properties/name/maxLength", "reason": "must NOT have more than 30 characters"}

	code, body := keyDone(strings.Repeat("a", 31))
	assert.Equal(t, maxLengthInfo, invalidParamBody(t, code, []byte(body)))

	// 30 字なら長さの検査を抜け、その先 (credential の検証) で別の理由で落ちる。
	_, body = keyDone(strings.Repeat("鍵", 30))
	assert.NotContains(t, body, "maxLength")

	code, body = keyDone(strings.Repeat("鍵", 31))
	assert.Equal(t, maxLengthInfo, invalidParamBody(t, code, []byte(body)))
}

// TestTwoFA_MissingParamsUseAjvID pins that the 2FA endpoints answer a
// missing required param with upstream's ajv id, not ed1d7571-….
func TestTwoFA_MissingParamsUseAjvID(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	for name, call := range map[string]func() (int, []byte){
		"register":     func() (int, []byte) { r := postExtra(h.TwoFARegister, `{}`, user); return r.Code, r.Body.Bytes() },
		"done":         func() (int, []byte) { r := postExtra(h.TwoFADone, `{}`, user); return r.Code, r.Body.Bytes() },
		"unregister":   func() (int, []byte) { r := postExtra(h.TwoFAUnregister, `{}`, user); return r.Code, r.Body.Bytes() },
		"register-key": func() (int, []byte) { r := postExtra(h.TwoFARegisterKey, `{}`, user); return r.Code, r.Body.Bytes() },
		"key-done":     func() (int, []byte) { r := postExtra(h.TwoFAKeyDone, `{}`, user); return r.Code, r.Body.Bytes() },
		"remove-key":   func() (int, []byte) { r := postExtra(h.TwoFARemoveKey, `{}`, user); return r.Code, r.Body.Bytes() },
		"update-key":   func() (int, []byte) { r := postExtra(h.TwoFAUpdateKey, `{}`, user); return r.Code, r.Body.Bytes() },
		"password-less": func() (int, []byte) {
			r := postExtra(h.TwoFAPasswordLess, `{"value":"x"}`, user)
			return r.Code, r.Body.Bytes()
		},
	} {
		t.Run(name, func(t *testing.T) {
			code, body := call()
			assert.Nil(t, invalidParamBody(t, code, body))
		})
	}
}
