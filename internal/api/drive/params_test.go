package drive

import (
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本家の paramDef は整数の値を持つ小数 (10.0、1e1) を integer として通す (#3839)。
func TestDriveParams_AcceptIntegerValuedFractions(t *testing.T) {
	h, fileRepo, _ := newHandlerWithRepos(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid}
	for _, tc := range []struct {
		name    string
		handler func(echo.Context) error
		body    string
	}{
		{"drive/files", h.FilesList, `{"limit":10.0,"sinceDate":1e1,"untilDate":4102444800000.0}`},
		{"drive/files/attached-notes", h.FilesAttachedNotes, `{"fileId":"f1","limit":1e1,"sinceDate":1.0,"untilDate":4102444800000.0}`},
		{"drive/stream", h.Stream, `{"limit":100.0,"sinceDate":1e1,"untilDate":4102444800000.0}`},
		{"drive/folders", h.FoldersList, `{"limit":1e1,"sinceDate":10.0,"untilDate":4102444800000.0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newJSONReq(t, tc.body)
			setUser(c, "u1")
			require.NoError(t, tc.handler(c))
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		})
	}
}

// 本家の paramDef は md5 を required の string とするだけなので、空文字は false
// になる (#3839)。キーが無ければ今までどおり 400。
func TestFilesCheckExistence_EmptyMD5IsFalse(t *testing.T) {
	h, fileRepo, _ := newHandlerWithRepos(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, MD5: "abc123"}
	c, rec := newJSONReq(t, `{"md5":""}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCheckExistence(c))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `false`, rec.Body.String())

	c, rec = newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesCheckExistence(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 本家は空の url も paramDef で通して 204 を返し、取得は裏で失敗する (#3839)。
// キーが無ければ今までどおり 400。
func TestFilesUploadFromURL_EmptyURLIsAccepted(t *testing.T) {
	h, _, _ := newHandler(t)
	rp := &recordingProcessor{calls: make(chan URLUploadInput, 1)}
	h.SetURLUploader(rp)

	c, rec := newJSONReq(t, `{"url":""}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesUploadFromURL(c))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	select {
	case in := <-rp.calls:
		assert.Empty(t, in.URL)
	case <-time.After(2 * time.Second):
		t.Fatal("Process was not invoked")
	}

	c, rec = newJSONReq(t, `{}`)
	setUser(c, "u1")
	require.NoError(t, h.FilesUploadFromURL(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 本家の paramDef は anyOf で、{fileId: misskey:id} か {url: string} の片方を満たせば
// もう片方の型は問わず、実行部は fileId のキーがあれば fileId で引く (#3839)。
func TestFilesShow_AnyOfLikeUpstream(t *testing.T) {
	h, fileRepo, _ := newHandler(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, Name: "hi", URL: "https://example.com/f1"}
	for _, tc := range []struct {
		body string
		code int
		err  string
	}{
		{`{"fileId":"f1","url":1}`, http.StatusOK, ""},
		{`{"fileId":"f1","url":null}`, http.StatusOK, ""},
		{`{"url":"https://example.com/f1"}`, http.StatusOK, ""},
		{`{"fileId":"","url":"https://example.com/f1"}`, http.StatusBadRequest, "NO_SUCH_FILE"},
		{`{"fileId":"x-y","url":"https://example.com/f1"}`, http.StatusBadRequest, "NO_SUCH_FILE"},
		{`{"fileId":1,"url":"https://example.com/f1"}`, http.StatusBadRequest, "NO_SUCH_FILE"},
		{`{"fileId":null,"url":"https://example.com/f1"}`, http.StatusBadRequest, "NO_SUCH_FILE"},
		{`{"fileId":"x-y"}`, http.StatusBadRequest, "INVALID_PARAM"},
		{`{"fileId":""}`, http.StatusBadRequest, "INVALID_PARAM"},
		{`{"fileId":1}`, http.StatusBadRequest, "INVALID_PARAM"},
		{`{"url":1}`, http.StatusBadRequest, "INVALID_PARAM"},
		{`{"fileId":null,"url":null}`, http.StatusBadRequest, "INVALID_PARAM"},
	} {
		c, rec := newJSONReq(t, tc.body)
		setUser(c, "u1")
		require.NoError(t, h.FilesShow(c))
		assert.Equal(t, tc.code, rec.Code, "%s: %s", tc.body, rec.Body.String())
		if tc.err != "" {
			assert.Contains(t, rec.Body.String(), tc.err, tc.body)
		} else {
			assert.Contains(t, rec.Body.String(), `"id":"f1"`, tc.body)
		}
	}
}
