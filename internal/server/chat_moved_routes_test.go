package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// TestSetupRoutes_ChatMessageCreateRejectsMovedAccount checks, through the
// real router built by New, that every chat message create endpoint rejects a
// moved account with YOUR_ACCOUNT_MOVED, and that the gate lets a non-moved
// account through to the handler.
//
// 3本は同じ MessagesCreate を呼ぶので、どれか1本でも RequireNotMoved を欠くと
// そこから移行済みのアカウントが送信できてしまう (#3437)。middleware 単体の
// テストでは配線の抜けを拾えないので、router.go の登録をそのまま通す。
func TestSetupRoutes_ChatMessageCreateRejectsMovedAccount(t *testing.T) {
	// 既定の trustProxy で組んだ、本番と同じ router を使う。
	srv := newServerWithTrustProxy(t, nil)
	db := serverIntegrationDB

	const (
		movedID, movedToken   = "chat-moved-user", "chatmovedtoken01"
		activeID, activeToken = "chat-active-user", "chatactivetoken1"
	)
	ids := []string{movedID, activeID}
	cleanup := func() {
		require.NoError(t, db.Where("id IN ?", ids).Delete(&model.User{}).Error)
	}
	cleanup()
	t.Cleanup(cleanup)

	movedTo := "https://remote.example/users/moved"
	userRepo := repository.NewUserRepository(db)
	require.NoError(t, userRepo.Create(&model.User{
		ID: movedID, Username: "chat_moved", UsernameLower: "chat_moved",
		Token: new(movedToken), MovedToURI: &movedTo,
	}))
	require.NoError(t, userRepo.Create(&model.User{
		ID: activeID, Username: "chat_active", UsernameLower: "chat_active",
		Token: new(activeToken),
	}))

	post := func(path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	errorCode := func(t *testing.T, rec *httptest.ResponseRecorder) string {
		t.Helper()
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
		return body.Error.Code
	}

	// 宛先は、自分自身と存在しないルーム。gate を抜けた後は handler の検査で
	// 400 になり、メッセージは作られない。active の応答をこの handler 由来の
	// エラーで固定し、gate を抜けて handler まで届いたことを確かめる。
	tests := []struct {
		name, path, body, activeCode string
	}{
		{"create", "/api/chat/messages/create", `{"text":"hi","toUserId":"{self}"}`, "RECIPIENT_IS_YOURSELF"},
		{"create-to-user", "/api/chat/messages/create-to-user", `{"text":"hi","toUserId":"{self}"}`, "RECIPIENT_IS_YOURSELF"},
		{"create-to-room", "/api/chat/messages/create-to-room", `{"text":"hi","toRoomId":"chat-moved-no-room"}`, "NO_SUCH_ROOM"},
	}

	var movedBodies []string
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(tc.path, movedToken, strings.ReplaceAll(tc.body, "{self}", movedID))
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			assert.Equal(t, "YOUR_ACCOUNT_MOVED", errorCode(t, rec))
			assert.Contains(t, rec.Body.String(), "56f20ec9-fd06-4fa5-841b-edd6d7d4fa31")
			movedBodies = append(movedBodies, rec.Body.String())

			rec = post(tc.path, activeToken, strings.ReplaceAll(tc.body, "{self}", activeID))
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Equal(t, tc.activeCode, errorCode(t, rec), "a non-moved account must pass the gate and reach the handler")
		})
	}
	// 3本とも同じ応答を返すこと (1本だけ別の gate で弾かれていないこと)。
	// -run でサブテストを絞ったときは比べる相手が揃わないので、2 本以上走ったときだけ比べる
	if len(movedBodies) < 2 {
		return
	}
	for _, b := range movedBodies[1:] {
		assert.JSONEq(t, movedBodies[0], b)
	}
}
