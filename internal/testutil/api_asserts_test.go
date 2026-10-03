package testutil

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// AssertFastifyError の happy-path test。helper が require ベースなので
// 失敗時は fail-fast、ここまで戻ってこれた = assertion を通過した、と
// いうのが test の意味。complementary な fail path は require/testify
// 標準動作に依拠 (= helper 自身ではなく testify の責務) ので test しない。

func TestAssertFastifyError_400DuplicatedUsername(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Code = http.StatusBadRequest
	body, _ := json.Marshal(map[string]any{
		"statusCode": http.StatusBadRequest,
		"error":      "Bad Request",
		"message":    "Error: DUPLICATED_USERNAME",
	})
	_, _ = rec.Body.Write(body)

	AssertFastifyError(t, rec, http.StatusBadRequest, "DUPLICATED_USERNAME")
}

func TestAssertFastifyError_410Expired(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Code = http.StatusGone
	body, _ := json.Marshal(map[string]any{
		"statusCode": http.StatusGone,
		"error":      "Gone",
		"message":    "Error: EXPIRED",
	})
	_, _ = rec.Body.Write(body)

	AssertFastifyError(t, rec, http.StatusGone, "EXPIRED")
}

func TestAssertFastifyError_500Internal(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Code = http.StatusInternalServerError
	body, _ := json.Marshal(map[string]any{
		"statusCode": http.StatusInternalServerError,
		"error":      "Internal Server Error",
		"message":    "Error: INTERNAL_ERROR",
	})
	_, _ = rec.Body.Write(body)

	AssertFastifyError(t, rec, http.StatusInternalServerError, "INTERNAL_ERROR")
}

func TestAssertInvalidParam(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Code = http.StatusBadRequest
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{"code": "INVALID_PARAM", "id": "3d81ceae-475f-4600-b2a8-2bc116157532"},
	})
	_, _ = rec.Body.Write(body)

	AssertInvalidParam(t, rec)
}
