package apierr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func invokeFastify(t *testing.T, fn func(*echo.Context) error) (int, map[string]any) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = fn(c)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return rec.Code, body
}

func TestFastifyReply_400DuplicatedUsername(t *testing.T) {
	code, body := invokeFastify(t, func(c *echo.Context) error {
		return FastifyReply(c, http.StatusBadRequest, "DUPLICATED_USERNAME")
	})
	assert.Equal(t, http.StatusBadRequest, code)
	assert.EqualValues(t, http.StatusBadRequest, body["statusCode"])
	assert.Equal(t, "Bad Request", body["error"])
	assert.Equal(t, "Error: DUPLICATED_USERNAME", body["message"])
	// nested {error:{code,id,message}} 形式が混入していないことも確認 (drift 防止)
	_, hasNestedError := body["error"].(map[string]any)
	assert.False(t, hasNestedError, "Fastify shape の error は string のはず")
}

func TestFastifyReply_410Expired(t *testing.T) {
	code, body := invokeFastify(t, func(c *echo.Context) error {
		return FastifyReply(c, http.StatusGone, "EXPIRED")
	})
	assert.Equal(t, http.StatusGone, code)
	assert.EqualValues(t, http.StatusGone, body["statusCode"])
	assert.Equal(t, "Gone", body["error"])
	assert.Equal(t, "Error: EXPIRED", body["message"])
}
