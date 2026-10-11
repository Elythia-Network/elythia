package admin_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/core/captcha"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
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

func TestUpdateMeta_ModerationLogHasNoSecrets(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	setAllMetaSecrets(t, metaRepo.Meta)
	logs := testutil.NewMockModerationLogRepository()
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h.SetModLogService(moderationlog.New(logs, gen))

	rec := doPost(h.UpdateMeta, `{"name":"renamed","deeplAuthKey":"plain-new-deepl"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Eventually(t, func() bool { return len(logs.Snapshot()) == 1 }, time.Second, 5*time.Millisecond)
	entry := logs.Snapshot()[0]
	assert.Equal(t, "updateServerSettings", entry.Type)
	info := string(entry.Info)
	for _, f := range model.MetaSecretFields {
		assert.NotContains(t, info, secretValue(f), "%s is logged in plain text", f)
	}
	assert.NotContains(t, info, "plain-new-deepl")
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(entry.Info, &parsed))
	before, ok := parsed["before"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, model.MetaSecretPlaceholder, before["smtpPass"])
	_, ok = parsed["changedSecrets"]
	assert.True(t, ok, "changedSecrets is recorded")
}

// 伏せる前に記録された平文の行も、返すときには伏せる。
func TestShowModerationLogs_MasksSecretsOfOldRows(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	logs := testutil.NewMockModerationLogRepository()
	require.NoError(t, logs.Create(&model.ModerationLog{
		ID: "l1", Type: "updateServerSettings",
		Info: []byte(`{"before":{"name":"a","smtpPass":"plain-old-pass"},"after":{"name":"b","objectStorageSecretKey":"plain-old-key","deeplAuthKey":null}}`),
	}))
	require.NoError(t, logs.Create(&model.ModerationLog{ID: "l2", Type: "suspend", Info: []byte(`{"userId":"u1"}`)}))
	require.NoError(t, logs.Create(&model.ModerationLog{ID: "l3", Type: "suspend"}))
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h.SetModLogService(moderationlog.New(logs, gen))

	rec := doPost(h.ShowModerationLogs, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "plain-old-pass")
	assert.NotContains(t, body, "plain-old-key")
	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	byID := map[string]map[string]any{}
	for _, l := range got {
		byID[l["id"].(string)] = l
	}
	info := byID["l1"]["info"].(map[string]any)
	assert.Equal(t, model.MetaSecretPlaceholder, info["before"].(map[string]any)["smtpPass"])
	assert.Equal(t, "a", info["before"].(map[string]any)["name"], "other fields are kept")
	assert.Nil(t, info["after"].(map[string]any)["deeplAuthKey"], "unset stays null")
	assert.Equal(t, "u1", byID["l2"]["info"].(map[string]any)["userId"], "other log types are untouched")
	assert.Nil(t, byID["l3"]["info"], "an empty info stays null")
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

// 前後に空白の付いた伏せ字も「変えない」と読む。update-meta は一部の欄を trim して
// から保存するので、そのまま通すと trim された伏せ字が秘密として残る。
func TestUpdateMeta_PaddedPlaceholderKeepsSecrets(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	body := map[string]any{"name": "renamed"}
	for _, f := range model.MetaSecretFields {
		body[f] = " " + model.MetaSecretPlaceholder + "\n"
	}
	b, err := json.Marshal(body)
	require.NoError(t, err)
	rec := doPost(h.UpdateMeta, string(b), adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	for _, f := range model.MetaSecretFields {
		assert.NotContains(t, metaRepo.LastUpdateFields, f, "%s would be overwritten", f)
	}
}

func TestCaptchaSave_PaddedPlaceholderUsesStoredSecret(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	stored := "plain-stored-hcaptcha"
	metaRepo.Meta.HcaptchaSecretKey = &stored
	var verifiedWith string
	h.SetCaptchaVerifierFactory(func(_, secret, _, _ string) captcha.Verifier {
		verifiedWith = secret
		return stubCaptchaVerifier{}
	})
	body := `{"provider":"hcaptcha","sitekey":"site","secret":" ` + model.MetaSecretPlaceholder + ` ","captchaResult":"ok"}`
	rec := doPost(h.CaptchaSave, body, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, stored, verifiedWith)
	assert.NotContains(t, metaRepo.LastUpdateFields, "hcaptchaSecretKey")
}

// secretDestinationCases pairs each secret with one of its destinations and the
// stored / changed destination values.
var secretDestinationCases = []struct {
	secret, dest string
	stored       any
	changed      any
	same         []any // 正規化すると今の値と同じになる値
}{
	{"smtpPass", "smtpHost", "smtp.example.com", "smtp.example.com.attacker.example", []any{" SMTP.example.com ", "smtp.example.com."}},
	{"smtpPass", "smtpPort", float64(587), float64(2525), []any{float64(587)}},
	{"truemailAuthKey", "truemailInstance", "https://truemail.example", "https://truemail.example.attacker.example", []any{"https://TRUEMAIL.example/", " https://truemail.example "}},
	{"sensitiveMediaDetectionApiKey", "sensitiveMediaDetectionApiUrl", "https://detect.example/api", "https://detect.example/api2", []any{"https://detect.example/api/", "HTTPS://detect.example/api"}},
	{"mcaptchaSecretKey", "mcaptchaInstanceUrl", "https://mcaptcha.example", "http://mcaptcha.example", []any{"https://mcaptcha.example/"}},
}

func newDestinationHandler(t *testing.T, secret, dest string, stored any) (*apiadmin.Handler, *testutil.MockMetaRepository) {
	t.Helper()
	h, _, metaRepo, _ := newTestHandler(t)
	b, err := json.Marshal(map[string]any{secret: secretValue(secret), dest: stored})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(b, metaRepo.Meta))
	return h, metaRepo
}

func updateMetaBody(t *testing.T, kv map[string]any) string {
	t.Helper()
	b, err := json.Marshal(kv)
	require.NoError(t, err)
	return string(b)
}

// 送り先を変えるのに秘密を入力し直さない (伏せ字・欄なし) なら断る。
func TestUpdateMeta_DestinationChangeRequiresSecret(t *testing.T) {
	for _, tc := range secretDestinationCases {
		for name, body := range map[string]map[string]any{
			"placeholder": {tc.dest: tc.changed, tc.secret: model.MetaSecretPlaceholder},
			"omitted":     {tc.dest: tc.changed},
		} {
			t.Run(tc.dest+"/"+name, func(t *testing.T) {
				h, metaRepo := newDestinationHandler(t, tc.secret, tc.dest, tc.stored)
				rec := doPost(h.UpdateMeta, updateMetaBody(t, body), adminUser)
				require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "SECRET_REENTRY_REQUIRED")
				assert.Nil(t, metaRepo.LastUpdateFields, "nothing is written")
			})
		}
	}
}

// 秘密を入力し直す (新しい値・空・null) なら、送り先を変えられる。
func TestUpdateMeta_DestinationChangeWithNewSecret(t *testing.T) {
	for _, tc := range secretDestinationCases {
		for name, secret := range map[string]any{"new": "plain-new-secret", "empty": "", "null": nil} {
			t.Run(tc.dest+"/"+name, func(t *testing.T) {
				h, _ := newDestinationHandler(t, tc.secret, tc.dest, tc.stored)
				rec := doPost(h.UpdateMeta, updateMetaBody(t, map[string]any{tc.dest: tc.changed, tc.secret: secret}), adminUser)
				assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
			})
		}
	}
}

// 送り先が (正規化すると) 変わらないなら、伏せ字のままでよい。本家の画面は
// 全部の欄を送り返す。
func TestUpdateMeta_UnchangedDestinationKeepsSecret(t *testing.T) {
	for _, tc := range secretDestinationCases {
		for i, same := range append([]any{tc.stored}, tc.same...) {
			t.Run(fmt.Sprintf("%s/%d", tc.dest, i), func(t *testing.T) {
				h, metaRepo := newDestinationHandler(t, tc.secret, tc.dest, tc.stored)
				rec := doPost(h.UpdateMeta, updateMetaBody(t, map[string]any{tc.dest: same, tc.secret: model.MetaSecretPlaceholder}), adminUser)
				require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
				assert.NotContains(t, metaRepo.LastUpdateFields, tc.secret)
			})
		}
	}
}

// 今の秘密が未設定なら、渡るものが無いので送り先だけを変えられる。
func TestUpdateMeta_DestinationChangeWithoutStoredSecret(t *testing.T) {
	for _, tc := range secretDestinationCases {
		t.Run(tc.dest, func(t *testing.T) {
			h, _, _, _ := newTestHandler(t)
			rec := doPost(h.UpdateMeta, updateMetaBody(t, map[string]any{tc.dest: tc.changed}), adminUser)
			assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		})
	}
}

// failFirstFetch fails only the first Fetch, so that the destination check
// is the one that sees the failure.
type failFirstFetch struct {
	*testutil.MockMetaRepository
	failed bool
}

func (f *failFirstFetch) Fetch() (*model.Meta, error) {
	if !f.failed {
		f.failed = true
		return nil, errors.New("db down")
	}
	return f.MockMetaRepository.Fetch()
}

// 今の値が読めないと送り先が変わるかを判定できないので、通さない。
func TestUpdateMeta_DestinationCheckFailsClosedWithoutMeta(t *testing.T) {
	_, userRepo, metaRepo, _ := newTestHandler(t)
	stored, host := "plain-smtp", "smtp.example.com"
	metaRepo.Meta.SMTPPass, metaRepo.Meta.SMTPHost = &stored, &host
	repo := &failFirstFetch{MockMetaRepository: metaRepo}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h := apiadmin.NewHandler(signup.NewService(userRepo, repo, idGen), nil, repo, userRepo, idGen)
	rec := doPost(h.UpdateMeta, `{"smtpHost":"smtp.attacker.example"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Nil(t, metaRepo.LastUpdateFields, "nothing is written")
}

// captcha/save: 伏せ字の secret は、保存済みの instanceUrl へだけ送る。
func TestCaptchaSave_McaptchaPlaceholderRequiresStoredInstanceURL(t *testing.T) {
	stored, storedURL := "plain-stored-mcaptcha", "https://mcaptcha.example"
	for name, tc := range map[string]struct {
		url  string
		code int
	}{
		"changed":    {"https://mcaptcha.example.attacker.example", http.StatusBadRequest},
		"same":       {"https://mcaptcha.example", http.StatusNoContent},
		"normalized": {" https://MCAPTCHA.example/ ", http.StatusNoContent},
	} {
		t.Run(name, func(t *testing.T) {
			h, _, metaRepo, _ := newTestHandler(t)
			metaRepo.Meta.McaptchaSecretKey = &stored
			metaRepo.Meta.McaptchaInstanceURL = &storedURL
			called := false
			h.SetCaptchaVerifierFactory(func(string, string, string, string) captcha.Verifier {
				called = true
				return stubCaptchaVerifier{}
			})
			body := updateMetaBody(t, map[string]any{"provider": "mcaptcha", "sitekey": "site", "instanceUrl": tc.url,
				"secret": model.MetaSecretPlaceholder, "captchaResult": "ok"})
			rec := doPost(h.CaptchaSave, body, adminUser)
			assert.Equal(t, tc.code, rec.Code, rec.Body.String())
			assert.Equal(t, tc.code == http.StatusNoContent, called, "the stored secret is sent only to the stored instance")
		})
	}
}
