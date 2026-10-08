package pluginsecrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/core/pluginsecret"
	"github.com/elythia-network/elythia/internal/core/pluginsecret/pgrepo"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/elythia-network/elythia/plugin"
)

var (
	dbOnce sync.Once
	testDB *gorm.DB
)

func openDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbOnce.Do(func() {
		testDB = testutil.MustOpenTestDB()
		testutil.ApplyMigrations(testDB)
	})
	return testDB
}

var testKey = bytes.Repeat([]byte{9}, pluginsecret.KeySize)

var declared = []plugin.SecretSpec{
	{Name: "apiKey", Description: "外部 API のキー"},
	{Name: "webhook"},
}

func newHandler(t *testing.T, pluginName string, key []byte) (*Handler, *pluginsecret.Service) {
	t.Helper()
	db := openDB(t)
	clean := func() {
		require.NoError(t, db.Exec(`DELETE FROM "plugin_secret" WHERE "pluginName" = ?`, pluginName).Error)
	}
	clean()
	t.Cleanup(clean)
	svc, err := pluginsecret.New(pgrepo.New(repository.NewPluginSecretRepository(db)), key)
	require.NoError(t, err)
	return New(svc, pluginName, declared), svc
}

func call(t *testing.T, fn echo.HandlerFunc, body string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), &model.User{ID: "admin1"})
	require.NoError(t, fn(c))
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body.Error.Code
}

// **保存した値は、どの応答にも出ない。** 一覧には「設定済み」と末尾 4 文字だけ。
func TestHandler_ValuesNeverAppearInResponses(t *testing.T) {
	h, svc := newHandler(t, "api-novalue", testKey)
	const value = "sk-live-TOPSECRET-value-9876"

	rec := call(t, h.Set, `{"i":"token","name":"apiKey","value":"`+value+`"}`)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Empty(t, rec.Body.String(), "保存の応答に何か返している")

	// プラグインが自分で置いた (宣言していない) 値も一覧に出るが、値は出ない。
	require.NoError(t, svc.Set(context.Background(), "api-novalue", "oauthToken", "runtime-TOPSECRET-token"))

	rec = call(t, h.List, `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	raw := rec.Body.String()
	assert.NotContains(t, raw, "TOPSECRET", "一覧に値が出ている")
	assert.NotContains(t, raw, "sk-live", "一覧に値の先頭が出ている")

	var res ListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	assert.True(t, res.Available)
	require.Len(t, res.Secrets, 3)

	api := res.Secrets[0]
	assert.Equal(t, "apiKey", api.Name)
	assert.Equal(t, "外部 API のキー", api.Description)
	assert.True(t, api.Declared)
	assert.True(t, api.Configured)
	assert.True(t, api.Readable)
	require.NotNil(t, api.Hint)
	assert.Equal(t, "9876", *api.Hint)
	assert.NotNil(t, api.UpdatedAt)

	wh := res.Secrets[1]
	assert.Equal(t, "webhook", wh.Name)
	assert.True(t, wh.Declared)
	assert.False(t, wh.Configured)
	assert.Nil(t, wh.Hint)
	assert.Nil(t, wh.UpdatedAt)

	extra := res.Secrets[2]
	assert.Equal(t, "oauthToken", extra.Name)
	assert.False(t, extra.Declared)
	assert.True(t, extra.Configured)

	got, err := svc.Get(context.Background(), "api-novalue", "apiKey")
	require.NoError(t, err)
	assert.Equal(t, value, got, "保存した値と違う")
}

// 宣言していない名前は管理画面から作れない。
func TestHandler_SetRejectsUndeclaredName(t *testing.T) {
	h, svc := newHandler(t, "api-undeclared", testKey)
	rec := call(t, h.Set, `{"name":"oauthToken","value":"overwrite"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, CodeUnknownSecret, errorCode(t, rec))
	_, err := svc.Get(context.Background(), "api-undeclared", "oauthToken")
	assert.ErrorIs(t, err, plugin.ErrSecretNotSet)
}

// **鍵が無ければ保存を断り、一覧でそれを知らせる。**
func TestHandler_WithoutKeyRefusesWrites(t *testing.T) {
	h, _ := newHandler(t, "api-nokey", nil)

	rec := call(t, h.Set, `{"name":"apiKey","value":"value"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, CodeUnavailable, errorCode(t, rec))

	rec = call(t, h.Delete, `{"name":"apiKey"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, CodeUnavailable, errorCode(t, rec))

	rec = call(t, h.List, `{}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var res ListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	assert.False(t, res.Available)
	for _, s := range res.Secrets {
		assert.False(t, s.Configured)
	}
}

func TestHandler_SetRejectsInvalidInput(t *testing.T) {
	h, _ := newHandler(t, "api-invalid", testKey)
	for _, body := range []string{`{"name":"apiKey","value":""}`, `{"name":"apiKey"}`} {
		rec := call(t, h.Set, body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, body)
		assert.Equal(t, CodeInvalidSecretParam, errorCode(t, rec), body)
	}
	rec := call(t, h.Set, `not json`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, CodeInvalidSecretParam, errorCode(t, rec))
	rec = call(t, h.Set, ``)
	assert.Equal(t, CodeUnknownSecret, errorCode(t, rec), "空の body は名前の無い要求")

	rec = call(t, h.Delete, `{"name":"bad name"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, CodeInvalidSecretParam, errorCode(t, rec))
	rec = call(t, h.Delete, `[`)
	assert.Equal(t, CodeInvalidSecretParam, errorCode(t, rec))
}

func TestHandler_DeleteRemovesDeclaredAndUndeclared(t *testing.T) {
	h, svc := newHandler(t, "api-delete", testKey)
	ctx := context.Background()
	require.NoError(t, svc.Set(ctx, "api-delete", "apiKey", "v1"))
	require.NoError(t, svc.Set(ctx, "api-delete", "oauthToken", "v2"))

	for _, name := range []string{"apiKey", "oauthToken"} {
		rec := call(t, h.Delete, `{"name":"`+name+`"}`)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
		_, err := svc.Get(ctx, "api-delete", name)
		assert.ErrorIs(t, err, plugin.ErrSecretNotSet, name)
	}
}

type brokenRepo struct{}

var errBroken = errors.New("broken")

func (brokenRepo) FindByName(context.Context, string, string) (*pluginsecret.Row, error) {
	return nil, errBroken
}

func (brokenRepo) ListByPlugin(context.Context, string) ([]pluginsecret.Row, error) {
	return nil, errBroken
}
func (brokenRepo) Upsert(context.Context, pluginsecret.Row) error { return errBroken }
func (brokenRepo) Delete(context.Context, string, string) error   { return errBroken }

// DB の障害は 500 にし、内部のエラー文を返さない。
func TestHandler_StorageErrorsAreInternal(t *testing.T) {
	svc, err := pluginsecret.New(brokenRepo{}, testKey)
	require.NoError(t, err)
	h := New(svc, "api-broken", declared)
	for name, fn := range map[string]echo.HandlerFunc{"list": h.List, "set": h.Set, "delete": h.Delete} {
		rec := call(t, fn, `{"name":"apiKey","value":"v"}`)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, name)
		assert.NotContains(t, rec.Body.String(), "broken", name)
	}
}

// 管理画面から貼り付けた値は前後の空白を落として保存する。空白だけなら断る。
// プラグイン自身の Set はバイト列をそのまま置く。
func TestHandler_SetTrimsSurroundingWhitespace(t *testing.T) {
	h, svc := newHandler(t, "api-trim", testKey)
	ctx := context.Background()

	rec := call(t, h.Set, `{"name":"apiKey","value":"  sk-pasted-key\n"}`)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	got, err := svc.Get(ctx, "api-trim", "apiKey")
	require.NoError(t, err)
	assert.Equal(t, "sk-pasted-key", got)

	rec = call(t, h.Set, `{"name":"apiKey","value":" \t\n "}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, CodeInvalidSecretParam, errorCode(t, rec))
	got, err = svc.Get(ctx, "api-trim", "apiKey")
	require.NoError(t, err)
	assert.Equal(t, "sk-pasted-key", got, "空白だけの値で上書きされた")

	require.NoError(t, svc.Set(ctx, "api-trim", "webhook", " raw\n"))
	got, err = svc.Get(ctx, "api-trim", "webhook")
	require.NoError(t, err)
	assert.Equal(t, " raw\n", got, "プラグイン側の Set は値を変えない")
}

// 不正な値の文言は利用者向けのものだけにする (内部の前置きを出さない)。
func TestHandler_InvalidValueMessageIsPlain(t *testing.T) {
	h, _ := newHandler(t, "api-msg", testKey)
	rec := call(t, h.Set, `{"name":"apiKey","value":""}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "INVALID_SECRET_PARAM", errorCode(t, rec), "本家の INVALID_PARAM と名前を分ける")
	assert.NotContains(t, rec.Body.String(), "pluginsecret:")
	assert.Contains(t, rec.Body.String(), "UTF-8")

	rec = call(t, h.Delete, `{"name":"bad name"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.NotContains(t, rec.Body.String(), "pluginsecret:")
	assert.Contains(t, rec.Body.String(), "名前が不正")
}
