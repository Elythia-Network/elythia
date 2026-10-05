package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/core/captcha"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubCaptchaVerifier returns a fixed error from Verify (nil = pass).
type stubCaptchaVerifier struct{ err error }

func (s stubCaptchaVerifier) Verify(context.Context, string) error { return s.err }

// --- thin smoke tests ---

func TestCaptchaCurrent(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusOK, doPost(h.CaptchaCurrent, `{}`, adminUser).Code)
}

// provider=none で captcha 無効化 (検証不要) → 204。
func TestCaptchaSave_None(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta.EnableHcaptcha = true
	rec := doPost(h.CaptchaSave, `{"provider":"none"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.False(t, metaRepo.Meta.EnableHcaptcha, "none で全 provider が無効化される")
}

// --- captcha/current nested shape ---

func TestCaptchaCurrent_WithHcaptchaEnabled(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	siteKey := "sk-hcap"
	secret := "ss-hcap"
	metaRepo.Meta.EnableHcaptcha = true
	metaRepo.Meta.HcaptchaSiteKey = &siteKey
	metaRepo.Meta.HcaptchaSecretKey = &secret
	rec := doPost(h.CaptchaCurrent, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "hcaptcha", got["provider"])
	// nested provider object shape (upstream current.ts res)。
	hcap := got["hcaptcha"].(map[string]any)
	assert.Equal(t, "sk-hcap", hcap["siteKey"])
	assert.Equal(t, "ss-hcap", hcap["secretKey"])
	// 全 provider object が key として存在する。
	for _, p := range []string{"hcaptcha", "mcaptcha", "recaptcha", "turnstile"} {
		_, ok := got[p].(map[string]any)
		assert.True(t, ok, p+" object が存在する")
	}
	// mcaptcha は instanceUrl も持つ。
	mcap := got["mcaptcha"].(map[string]any)
	_, hasInstance := mcap["instanceUrl"]
	assert.True(t, hasInstance)
}

func TestCaptchaCurrent_NoneEnabled(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.CaptchaCurrent, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "none", got["provider"], "無効時は provider='none'")
}

// --- captcha/save ---

// 検証 pass 時に enable flag と generic sitekey/secret が永続化される。
func TestCaptchaSave_VerifiesAndPersists(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	h.SetCaptchaVerifierFactory(func(string, string, string, string) captcha.Verifier {
		return stubCaptchaVerifier{err: nil}
	})
	rec := doPost(h.CaptchaSave,
		`{"provider":"turnstile","sitekey":"sk","secret":"ss","captchaResult":"tok"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, metaRepo.Meta.EnableTurnstile)
	assert.False(t, metaRepo.Meta.EnableHcaptcha)
	require.NotNil(t, metaRepo.Meta.TurnstileSiteKey)
	assert.Equal(t, "sk", *metaRepo.Meta.TurnstileSiteKey)
	require.NotNil(t, metaRepo.Meta.TurnstileSecretKey)
	assert.Equal(t, "ss", *metaRepo.Meta.TurnstileSecretKey)
}

// mcaptcha は sitekey/secret/instanceUrl を generic param から保存する。
func TestCaptchaSave_Mcaptcha(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	h.SetCaptchaVerifierFactory(func(string, string, string, string) captcha.Verifier {
		return stubCaptchaVerifier{err: nil}
	})
	rec := doPost(h.CaptchaSave,
		`{"provider":"mcaptcha","sitekey":"mk","secret":"ms","instanceUrl":"https://mcap.example","captchaResult":"tok"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, metaRepo.Meta.EnableMcaptcha)
	require.NotNil(t, metaRepo.Meta.McaptchaSiteKey)
	assert.Equal(t, "mk", *metaRepo.Meta.McaptchaSiteKey)
	require.NotNil(t, metaRepo.Meta.McaptchaInstanceURL)
	assert.Equal(t, "https://mcap.example", *metaRepo.Meta.McaptchaInstanceURL)
}

// 検証失敗時は VERIFICATION_FAILED を返し、設定を永続化しない。
func TestCaptchaSave_VerificationFailed(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	h.SetCaptchaVerifierFactory(func(string, string, string, string) captcha.Verifier {
		return stubCaptchaVerifier{err: captcha.ErrVerificationFail}
	})
	rec := doPost(h.CaptchaSave,
		`{"provider":"hcaptcha","sitekey":"sk","secret":"ss","captchaResult":"bad"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "VERIFICATION_FAILED")
	assert.False(t, metaRepo.Meta.EnableHcaptcha, "検証失敗時は永続化しない")
}

// 必須 param 欠落は INVALID_PARAMETERS。
func TestCaptchaSave_MissingParams(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.CaptchaSave, `{"provider":"hcaptcha"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAMETERS")
}

// 未知 provider は INVALID_PROVIDER。
func TestCaptchaSave_UnknownProvider(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.CaptchaSave, `{"provider":"garbage"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PROVIDER")
}

// provider 未指定 (enum 外) も INVALID_PROVIDER。
func TestCaptchaSave_MissingProvider(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.CaptchaSave, `{}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PROVIDER")
}

// testcaptcha は captchaResult のみ必須 (secret/sitekey 不要) で、検証 pass 時に
// enableTestcaptcha=true を保存する。
func TestCaptchaSave_Testcaptcha(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	h.SetCaptchaVerifierFactory(func(string, string, string, string) captcha.Verifier {
		return stubCaptchaVerifier{err: nil}
	})
	rec := doPost(h.CaptchaSave, `{"provider":"testcaptcha","captchaResult":"tok"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, metaRepo.Meta.EnableTestcaptcha)
	assert.False(t, metaRepo.Meta.EnableHcaptcha)
}

// mcaptcha は instanceUrl 等が欠けると INVALID_PARAMETERS。
func TestCaptchaSave_McaptchaMissingInstanceURL(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.CaptchaSave,
		`{"provider":"mcaptcha","sitekey":"mk","secret":"ms","captchaResult":"tok"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAMETERS")
}

// 検証エラーの sentinel は upstream save.ts の error code にマップされる。
func TestCaptchaSave_ErrorCodeMapping(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{"requestFailed", captcha.ErrRequestFailed, http.StatusInternalServerError, "REQUEST_FAILED"},
		{"noResponse", captcha.ErrNoResponse, http.StatusBadRequest, "NO_RESPONSE_PROVIDED"},
		{"unknown", assertError{}, http.StatusInternalServerError, "UNKNOWN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, metaRepo, _ := newTestHandler(t)
			h.SetCaptchaVerifierFactory(func(string, string, string, string) captcha.Verifier {
				return stubCaptchaVerifier{err: tc.err}
			})
			rec := doPost(h.CaptchaSave,
				`{"provider":"hcaptcha","sitekey":"sk","secret":"ss","captchaResult":"tok"}`, adminUser)
			assert.Equal(t, tc.wantCode, rec.Code)
			assert.Contains(t, rec.Body.String(), tc.wantBody)
			assert.False(t, metaRepo.Meta.EnableHcaptcha, "検証エラー時は永続化しない")
		})
	}
}

// factory が nil verifier を返す misconfiguration では UNKNOWN を返す。
func TestCaptchaSave_NilVerifier(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetCaptchaVerifierFactory(func(string, string, string, string) captcha.Verifier {
		return nil
	})
	rec := doPost(h.CaptchaSave,
		`{"provider":"hcaptcha","sitekey":"sk","secret":"ss","captchaResult":"tok"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Contains(t, rec.Body.String(), "UNKNOWN")
}

// CaptchaCurrent の provider 判定が各 enable flag に対応する (upstream get の順序)。
func TestCaptchaCurrent_ProviderDetection(t *testing.T) {
	cases := []struct {
		field func(m *model.Meta)
		want  string
	}{
		{func(m *model.Meta) { m.EnableMcaptcha = true }, "mcaptcha"},
		{func(m *model.Meta) { m.EnableRecaptcha = true }, "recaptcha"},
		{func(m *model.Meta) { m.EnableTurnstile = true }, "turnstile"},
		{func(m *model.Meta) { m.EnableTestcaptcha = true }, "testcaptcha"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			h, _, metaRepo, _ := newTestHandler(t)
			tc.field(metaRepo.Meta)
			rec := doPost(h.CaptchaCurrent, `{}`, adminUser)
			require.Equal(t, http.StatusOK, rec.Code)
			var got map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			assert.Equal(t, tc.want, got["provider"])
			// nil *string field は JSON null として直列化される。
			mcap := got["mcaptcha"].(map[string]any)
			_, hasSecret := mcap["secretKey"]
			assert.True(t, hasSecret)
		})
	}
}
