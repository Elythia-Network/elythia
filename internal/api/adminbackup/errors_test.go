package adminbackup

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/backupadmin"
	"github.com/elythia-network/elythia/internal/core/strongauth"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newCtx() (echo.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	return echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/", nil), rec), rec
}

func TestServiceErrorMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{backupadmin.ErrNotConfigured, http.StatusBadRequest, "BACKUP_NOT_CONFIGURED"},
		{backupadmin.ErrInvalidID, http.StatusBadRequest, "INVALID_PARAM"},
		{backupadmin.ErrNotFound, http.StatusBadRequest, "NO_SUCH_GENERATION"},
		{fmt.Errorf("%w: x", backupadmin.ErrIncomplete), http.StatusBadRequest, "GENERATION_INCOMPLETE"},
		{backupadmin.ErrServiceNotConfigured, http.StatusBadRequest, "BACKUP_SERVICE_NOT_CONFIGURED"},
		{backupadmin.ErrServiceBusy, http.StatusConflict, "BACKUP_SERVICE_BUSY"},
		{fmt.Errorf("%w: 500", backupadmin.ErrServiceFailed), http.StatusBadGateway, "BACKUP_SERVICE_ERROR"},
		{errors.New("other"), http.StatusInternalServerError, "INTERNAL_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			c, rec := newCtx()
			require.NoError(t, serviceError(c, "op", tc.err))
			assert.Equal(t, tc.status, rec.Code)
			assert.Equal(t, tc.code, errCode(t, rec))
		})
	}
}

func TestRefusalMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{&strongauth.Error{Reason: strongauth.ReasonPasswordNotSet}, http.StatusForbidden, "PASSWORD_NOT_SET"},
		{&strongauth.Error{Reason: strongauth.ReasonCodeReused}, http.StatusForbidden, "TWO_FACTOR_CODE_ALREADY_USED"},
		{&strongauth.Error{Reason: strongauth.ReasonPasskeyUnavailable}, http.StatusBadRequest, "PASSKEY_UNAVAILABLE"},
		{&strongauth.Error{Reason: strongauth.ReasonUnavailable, Err: errors.New("redis")}, http.StatusServiceUnavailable, "AUTHENTICATION_UNAVAILABLE"},
		{fmt.Errorf("wrapped: %w", &strongauth.Error{Reason: strongauth.ReasonFailed}), http.StatusForbidden, "REAUTHENTICATION_FAILED"},
		{errors.New("not a strongauth error"), http.StatusInternalServerError, "INTERNAL_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			c, rec := newCtx()
			require.NoError(t, refusal(c, tc.err))
			assert.Equal(t, tc.status, rec.Code)
			assert.Equal(t, tc.code, errCode(t, rec))
		})
	}
	c, rec := newCtx()
	require.NoError(t, refusal(c, &strongauth.Error{Reason: strongauth.ReasonRateLimited, RetryAfter: 1500 * time.Millisecond}))
	assert.Equal(t, "2", rec.Header().Get("Retry-After"))
}

func TestGuardRejectsMalformedBodies(t *testing.T) {
	f := newFixture(t, true)
	cases := map[string]struct{ body, contentType string }{
		"not json":  {"{", echo.MIMEApplicationJSON},
		"too large": {`{"password":"` + strings.Repeat("x", reauthBodyLimit) + `"}`, echo.MIMEApplicationJSON},
		// handler の Bind と読み方が食い違わないよう、JSON 以外は受けない。
		"text/plain": {`{"password":"correct horse","token":"123456"}`, echo.MIMETextPlain},
		"form":       {"password=correct+horse&token=123456", echo.MIMEApplicationForm},
		"no type":    {`{"password":"correct horse","token":"123456"}`, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/admin/backup/list", strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set(echo.HeaderContentType, tc.contentType)
			}
			req.Header.Set("X-Test-User", adminID)
			req.Header.Set("X-Test-Token", userToken+adminID)
			rec := httptest.NewRecorder()
			f.e.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Zero(t, f.storage.accesses)
		})
	}
}

// TestGuardRejectsAValidBodyOverTheLimit: the body would still decode when
// cut at the limit (trailing spaces), so only the size check refuses it.
func TestGuardRejectsAValidBodyOverTheLimit(t *testing.T) {
	f := newFixture(t, false)
	body := `{"password":"` + password + `","token":"` + f.code(t) + `"}`
	send := func(body string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/backup/list", strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		req.Header.Set("X-Test-User", adminID)
		req.Header.Set("X-Test-Token", userToken+adminID)
		rec := httptest.NewRecorder()
		f.e.ServeHTTP(rec, req)
		return rec.Code
	}
	assert.Equal(t, http.StatusBadRequest, send(body+strings.Repeat(" ", reauthBodyLimit)))
	assert.Zero(t, f.storage.accesses)
	assert.Empty(t, f.log.entries)
	// 上限の内側なら通る (同じコードはまだ使われていない)。
	assert.Equal(t, http.StatusOK, send(body+strings.Repeat(" ", reauthBodyLimit-len(body))))
}

func TestOperationsRequireID(t *testing.T) {
	for _, path := range []string{"/api/admin/backup/verify", "/api/admin/backup/delete", "/api/admin/backup/download"} {
		t.Run(path, func(t *testing.T) {
			f := newFixture(t, false)
			rec := f.post(t, path, native(adminID), map[string]any{"password": password, "token": f.code(t)})
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Equal(t, "INVALID_PARAM", errCode(t, rec))
			assert.Empty(t, f.log.entries)
		})
	}
}

func TestOperationFailuresAreNotLogged(t *testing.T) {
	f := newFixture(t, false)
	f.control.err = backupadmin.ErrServiceBusy
	rec := f.post(t, "/api/admin/backup/take", native(adminID), map[string]any{"password": password, "token": f.code(t)})
	assert.Equal(t, http.StatusConflict, rec.Code)
	rec = f.post(t, "/api/admin/backup/verify", native(adminID), map[string]any{"id": "20991231T000000Z", "password": password, "token": f.code(t)})
	assert.Equal(t, "NO_SUCH_GENERATION", errCode(t, rec))
	rec = f.post(t, "/api/admin/backup/download", native(adminID), map[string]any{"id": "bad", "password": password, "token": f.code(t)})
	assert.Equal(t, "INVALID_PARAM", errCode(t, rec))
	f.storage.getErr = errors.New("io")
	rec = f.post(t, "/api/admin/backup/list", native(adminID), map[string]any{"password": password, "token": f.code(t)})
	assert.Equal(t, http.StatusOK, rec.Code, "an unreadable meta.json is shown, not fatal")
	assert.Equal(t, 1, len(f.log.entries))
}

func TestHandlerWithoutModLog(t *testing.T) {
	h := NewHandler(backupadmin.NewService(backupadmin.Options{}), nil, nil)
	c, _ := newCtx()
	h.log(c, "x", nil) // must not panic
}
