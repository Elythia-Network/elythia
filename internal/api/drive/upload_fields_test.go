package drive

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/api/apierr"
)

// field is one multipart text field; a slice keeps order and allows repeats.
type field struct{ key, value string }

// newUploadReq builds a drive/files/create multipart request with a file and
// the given fields, sent to target (which may carry a query string).
func newUploadReq(t *testing.T, target string, fields []field) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	part, err := mw.CreateFormFile("file", "hello.txt")
	require.NoError(t, err)
	_, err = part.Write([]byte("hello"))
	require.NoError(t, err)
	for _, f := range fields {
		require.NoError(t, mw.WriteField(f.key, f.value))
	}
	require.NoError(t, mw.Close())
	req := httptest.NewRequest(http.MethodPost, target, body)
	req.Header.Set(echo.HeaderContentType, mw.FormDataContentType())
	rec := httptest.NewRecorder()
	return echo.New().NewContext(req, rec), rec
}

// TestFilesCreate_BooleanFields pins upstream's handling of the multipart
// boolean fields (isSensitive, force): ApiCallService casts them with
// JSON.parse before ajv (0b5f1631 when that fails), ajv then requires a
// boolean, and only multipart fields count as params.
func TestFilesCreate_BooleanFields(t *testing.T) {
	castErr := func(param string) map[string]any {
		return map[string]any{"param": param, "reason": "cannot cast to boolean"}
	}
	typeErr := func(param string) map[string]any {
		return map[string]any{"param": "#/properties/" + param + "/type", "reason": "must be boolean"}
	}
	cases := []struct {
		name          string
		target        string
		fields        []field
		wantID        string // "" = success
		wantInfo      map[string]any
		wantSensitive bool
	}{
		{name: "not JSON", fields: []field{{"isSensitive", "abc"}}, wantID: apierr.UUIDInvalidParamCast, wantInfo: castErr("isSensitive")},
		{name: "capitalized TRUE", fields: []field{{"force", "TRUE"}}, wantID: apierr.UUIDInvalidParamCast, wantInfo: castErr("force")},
		{name: "empty value", fields: []field{{"force", ""}}, wantID: apierr.UUIDInvalidParamCast, wantInfo: castErr("force")},
		{name: "paramDef order", fields: []field{{"force", "x"}, {"isSensitive", "y"}}, wantID: apierr.UUIDInvalidParamCast, wantInfo: castErr("isSensitive")},
		{name: "number is not boolean", fields: []field{{"isSensitive", "1"}}, wantID: apierr.UUIDInvalidParam, wantInfo: typeErr("isSensitive")},
		{name: "null is not boolean", fields: []field{{"force", "null"}}, wantID: apierr.UUIDInvalidParam, wantInfo: typeErr("force")},
		// 変換は ajv より先なので、後ろの field の変換失敗が先に出る。
		{name: "cast runs before ajv", fields: []field{{"isSensitive", "1"}, {"force", "bad"}}, wantID: apierr.UUIDInvalidParamCast, wantInfo: castErr("force")},
		{name: "true", fields: []field{{"isSensitive", "true"}, {"force", "false"}}, wantSensitive: true},
		{name: "JSON whitespace", fields: []field{{"isSensitive", " true "}}, wantSensitive: true},
		{name: "false", fields: []field{{"isSensitive", "false"}}},
		// 本家は multipart の field だけを見る。query の値は使わない。
		{name: "query is not a param", target: "/?isSensitive=true&force=bad"},
		// 同じ名前が 2 回来ると本家では undefined (既定の false)。
		{name: "repeated field is absent", fields: []field{{"isSensitive", "true"}, {"isSensitive", "true"}}},
		// ajv は paramDef の順 (comment が isSensitive より前) で最初の違反を返す。
		{name: "comment maxLength before boolean type", fields: []field{{"isSensitive", "1"}, {"comment", strings.Repeat("あ", 513)}},
			wantID: apierr.UUIDInvalidParam, wantInfo: map[string]any{"param": "#/properties/comment/maxLength", "reason": "must NOT have more than 512 characters"}},
		// 変換は ajv より先。
		{name: "cast before comment maxLength", fields: []field{{"comment", strings.Repeat("あ", 513)}, {"force", "bad"}},
			wantID: apierr.UUIDInvalidParamCast, wantInfo: castErr("force")},
		// ajv の検査は名前の検証 (INVALID_FILE_NAME、handler の中) より先。
		{name: "comment maxLength before file name check", fields: []field{{"name", "a/b"}, {"comment", strings.Repeat("あ", 513)}},
			wantID: apierr.UUIDInvalidParam, wantInfo: map[string]any{"param": "#/properties/comment/maxLength", "reason": "must NOT have more than 512 characters"}},
		// 長さは収まるが列に入らない値は info の無い INVALID_PARAM (本家に無い検査)。
		{name: "comment with NUL", fields: []field{{"comment", "a\x00b"}}, wantID: apierr.UUIDInvalidParam, wantInfo: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newHandler(t)
			target := tc.target
			if target == "" {
				target = "/"
			}
			c, rec := newUploadReq(t, target, tc.fields)
			setUser(c, "u1")
			require.NoError(t, h.FilesCreate(c))
			var resp map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			if tc.wantID == "" {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Equal(t, tc.wantSensitive, resp["isSensitive"])
				return
			}
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			errObj := resp["error"].(map[string]any)
			assert.Equal(t, "INVALID_PARAM", errObj["code"])
			assert.Equal(t, "Invalid param.", errObj["message"])
			assert.Equal(t, tc.wantID, errObj["id"])
			if tc.wantInfo == nil {
				assert.Nil(t, errObj["info"])
			} else {
				assert.Equal(t, tc.wantInfo, errObj["info"])
			}
		})
	}
}

// TestFilesCreate_FieldsComeFromMultipartOnly pins that name / folderId /
// comment are read from the multipart body only: a query string value does
// not reach the upload (upstream's params are the multipart fields).
func TestFilesCreate_FieldsComeFromMultipartOnly(t *testing.T) {
	h, _, _ := newHandler(t)
	c, rec := newUploadReq(t, "/?folderId=nosuch&comment=q&name=q.txt", nil)
	setUser(c, "u1")
	require.NoError(t, h.FilesCreate(c))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "hello.txt", resp["name"])
	assert.Nil(t, resp["comment"])
	assert.Nil(t, resp["folderId"])
}
