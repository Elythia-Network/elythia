package middleware

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const uploadRoute = "/api/drive/files/create"

// multipartBody builds a multipart/form-data body with the given plain
// fields and, when fileField is not empty, one file part under that name.
func multipartBody(t *testing.T, fileField string, fields map[string]string) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		require.NoError(t, w.WriteField(k, v))
	}
	if fileField != "" {
		fw, err := w.CreateFormFile(fileField, "a.txt")
		require.NoError(t, err)
		_, err = fw.Write([]byte("hello"))
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return w.FormDataContentType(), buf.String()
}

// runMultipartGuard sends one request through RequireMultipartFile and
// reports the recorder, whether the next handler ran, and the returned
// error.
func runMultipartGuard(t *testing.T, method, path, contentType string, body io.Reader) (*httptest.ResponseRecorder, bool, error) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(method, "/x", body)
	if contentType != "" {
		req.Header.Set(echo.HeaderContentType, contentType)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath(path)
	ran := false
	err := RequireMultipartFile(uploadRoute)(func(c echo.Context) error {
		ran = true
		// 後段が同じ body をもう一度読めることも確かめる。
		if IsJSONContentType(c.Request().Header.Get(echo.HeaderContentType)) {
			b, _ := io.ReadAll(c.Request().Body)
			return c.String(http.StatusOK, string(b))
		}
		return c.NoContent(http.StatusOK)
	})(c)
	return rec, ran, err
}

// TestRequireMultipartFile pins upstream handleMultipartRequest's bare 400
// (no body, no Content-Type) for an upload request without a file part,
// and the requests it leaves to the rest of the pipeline.
func TestRequireMultipartFile(t *testing.T) {
	withFile, withFileBody := multipartBody(t, "file", map[string]string{"i": "tok"})
	otherName, otherNameBody := multipartBody(t, "upload", nil)
	noFile, noFileBody := multipartBody(t, "", map[string]string{"i": "tok", "name": "a"})

	cases := []struct {
		name     string
		method   string
		path     string
		ct       string
		body     string
		wantNext bool
	}{
		{name: "multipart with file", ct: withFile, body: withFileBody, wantNext: true},
		{name: "file under another field name", ct: otherName, body: otherNameBody, wantNext: true},
		{name: "multipart without file part", ct: noFile, body: noFileBody},
		{name: "multipart without boundary", ct: "multipart/form-data", body: "x"},
		{name: "malformed multipart content type", ct: "Multipart/Form-Data; boundary", body: "x"},
		{name: "no body", ct: "", body: ""},
		{name: "text/plain", ct: "text/plain", body: "hello"},
		{name: "JSON object", ct: echo.MIMEApplicationJSON, body: `{"i":"tok"}`},
		{name: "JSON null", ct: echo.MIMEApplicationJSON, body: `null`},
		{name: "unknown media type", ct: "application/x-www-form-urlencoded", body: "a=1"},
		// Fastify の JSON parser が先に弾くものは JSONBodyParse に任せる。
		{name: "empty JSON body", ct: echo.MIMEApplicationJSON, body: "", wantNext: true},
		{name: "invalid JSON body", ct: echo.MIMEApplicationJSON, body: "{", wantNext: true},
		{name: "poisoned JSON body", ct: echo.MIMEApplicationJSON, body: `{"__proto__":{}}`, wantNext: true},
		{name: "other route", path: "/api/drive/files/update", ct: "", body: "", wantNext: true},
		{name: "GET", method: http.MethodGet, ct: "", body: "", wantNext: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			method, path := tc.method, tc.path
			if method == "" {
				method = http.MethodPost
			}
			if path == "" {
				path = uploadRoute
			}
			rec, ran, err := runMultipartGuard(t, method, path, tc.ct, strings.NewReader(tc.body))
			require.NoError(t, err)
			assert.Equal(t, tc.wantNext, ran)
			if tc.wantNext {
				assert.Equal(t, http.StatusOK, rec.Code)
				if IsJSONContentType(tc.ct) {
					assert.Equal(t, tc.body, rec.Body.String(), "the body must be handed on unchanged")
				}
				return
			}
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Empty(t, rec.Body.String())
			assert.Empty(t, rec.Header().Get(echo.HeaderContentType))
			assert.Equal(t, "private, max-age=0, must-revalidate", rec.Header().Get("Cache-Control"))
		})
	}
}

// TestRequireMultipartFile_ReadErrors pins how body read failures are
// answered: BodyLimitByPath's 413 is returned as is (multipart and JSON), and
// another read error on a JSON body gets Fastify's invalid-JSON envelope.
func TestRequireMultipartFile_ReadErrors(t *testing.T) {
	tooLarge := echo.NewHTTPError(http.StatusRequestEntityTooLarge)
	ct, body := multipartBody(t, "file", nil)

	t.Run("multipart over the limit", func(t *testing.T) {
		r := io.MultiReader(strings.NewReader(body[:20]), iotest.ErrReader(tooLarge))
		_, ran, err := runMultipartGuard(t, http.MethodPost, uploadRoute, ct, r)
		assert.False(t, ran)
		assert.Same(t, tooLarge, err)
	})
	t.Run("JSON over the limit", func(t *testing.T) {
		_, ran, err := runMultipartGuard(t, http.MethodPost, uploadRoute, echo.MIMEApplicationJSON, iotest.ErrReader(tooLarge))
		assert.False(t, ran)
		assert.Same(t, tooLarge, err)
	})
	t.Run("JSON read error", func(t *testing.T) {
		rec, ran, err := runMultipartGuard(t, http.MethodPost, uploadRoute, echo.MIMEApplicationJSON, iotest.ErrReader(errors.New("reset")))
		require.NoError(t, err)
		assert.False(t, ran)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.JSONEq(t, invalidEnvelope, rec.Body.String())
	})
}
