package admin_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/core/captcha"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// secretValue is the distinctive value each secret field is set to.
func secretValue(field string) string { return "plain-" + field + "-value" }

// setAllMetaSecrets sets every secret field of meta to secretValue(field).
func setAllMetaSecrets(t *testing.T, meta *model.Meta) {
	t.Helper()
	vals := map[string]string{}
	for _, f := range model.MetaSecretFields {
		vals[f] = secretValue(f)
	}
	b, err := json.Marshal(vals)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, meta))
}

func hasName(field string) string {
	return "has" + strings.ToUpper(field[:1]) + field[1:]
}

func TestAdminMeta_DoesNotReturnSecrets(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	setAllMetaSecrets(t, metaRepo.Meta)

	rec := doPost(h.AdminMeta, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	for _, f := range model.MetaSecretFields {
		assert.NotContains(t, body, secretValue(f), "%s leaks", f)
		assert.Equal(t, model.MetaSecretPlaceholder, got[f], f)
		assert.Equal(t, true, got[hasName(f)], hasName(f))
	}
}

func TestAdminMeta_UnsetSecretsStayNull(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.AdminMeta, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	for _, f := range model.MetaSecretFields {
		v, present := got[f]
		assert.True(t, present, "%s is still in the response", f)
		assert.Nil(t, v, f)
		assert.Equal(t, false, got[hasName(f)], hasName(f))
	}
}

// 本家の管理画面と同じく、admin/meta で受け取った値をそのまま送り返しても、
// 秘密の欄は UPDATE に載らない (保存した値が伏せ字で上書きされない)。
func TestUpdateMeta_SendingBackAdminMetaKeepsSecrets(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	setAllMetaSecrets(t, metaRepo.Meta)
	rec := doPost(h.AdminMeta, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	body := map[string]any{"name": "renamed"}
	for _, f := range model.MetaSecretFields {
		body[f] = got[f]
	}
	b, err := json.Marshal(body)
	require.NoError(t, err)

	rec = doPost(h.UpdateMeta, string(b), adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Contains(t, metaRepo.LastUpdateFields, "name")
	for _, f := range model.MetaSecretFields {
		assert.NotContains(t, metaRepo.LastUpdateFields, f, "%s would be overwritten", f)
	}
}

// 空文字・新しい値は、今までどおり UPDATE に載る (消す・変える手段は残す)。
func TestUpdateMeta_EmptyAndNewSecretsAreWritten(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	rec := doPost(h.UpdateMeta, `{"smtpPass":"","objectStorageSecretKey":"new-key"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Contains(t, metaRepo.LastUpdateFields, "smtpPass")
	assert.Equal(t, "new-key", metaRepo.LastUpdateFields["objectStorageSecretKey"])
}

// captcha/save は伏せ字の secret を「変えない」として、保存済みの値で検証する。
func TestCaptchaSave_PlaceholderUsesStoredSecret(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	stored := "plain-stored-turnstile"
	metaRepo.Meta.TurnstileSecretKey = &stored
	var verifiedWith string
	h.SetCaptchaVerifierFactory(func(_, secret, _, _ string) captcha.Verifier {
		verifiedWith = secret
		return stubCaptchaVerifier{}
	})
	body := `{"provider":"turnstile","sitekey":"site","secret":"` + model.MetaSecretPlaceholder + `","captchaResult":"ok"}`
	rec := doPost(h.CaptchaSave, body, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, stored, verifiedWith)
	assert.NotContains(t, metaRepo.LastUpdateFields, "turnstileSecretKey")
	assert.Equal(t, stored, *metaRepo.Meta.TurnstileSecretKey)
}

// 保存済みの secret が無いのに伏せ字が来たら、secret 無しとして断る。
func TestCaptchaSave_PlaceholderWithoutStoredSecretIsRejected(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetCaptchaVerifierFactory(func(string, string, string, string) captcha.Verifier { return stubCaptchaVerifier{} })
	body := `{"provider":"hcaptcha","sitekey":"site","secret":"` + model.MetaSecretPlaceholder + `","captchaResult":"ok"}`
	rec := doPost(h.CaptchaSave, body, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
