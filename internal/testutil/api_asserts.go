package testutil

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// AssertFastifyError verifies that the recorder body is the upstream
// Misskey TS Fastify-style reply error shape:
//
//	{"statusCode": <status>, "error": <http.StatusText(status)>, "message": "Error: <code>"}
//
// 対応する production helper は internal/api/apierr.FastifyReply。drop-in
// 互換 fix (#802 / #809 / #810) の handler_test 群が共有する assertion で、
// signup / signin endpoint をまたいで重複定義していた assertion を集約する。
//
// 失敗時は require 経由で fail-fast (= subtest 単位で打ち切り)。
func AssertFastifyError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	require.Equal(t, status, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.EqualValues(t, status, body["statusCode"])
	require.Equal(t, http.StatusText(status), body["error"])
	require.Equal(t, "Error: "+code, body["message"])
}

// invalidParamID is upstream's INVALID_PARAM error id (endpoint-base.ts).
// apierr を import すると testutil → api の逆向き依存になるので値を写す。
const invalidParamID = "3d81ceae-475f-4600-b2a8-2bc116157532"

// AssertInvalidParam verifies that the recorder holds a 400 INVALID_PARAM
// Misskey API error with upstream's id.
func AssertInvalidParam(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var body struct {
		Error struct {
			Code string `json:"code"`
			ID   string `json:"id"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "INVALID_PARAM", body.Error.Code)
	require.Equal(t, invalidParamID, body.Error.ID)
}
