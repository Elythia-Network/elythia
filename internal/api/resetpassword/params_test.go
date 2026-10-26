package resetpassword

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

// 本家の paramDef は username・email を required の string とするだけなので、空文字も
// 通し、該当する利用者がいないので 204 を返す。キーが無ければ今までどおり 400 (#3845)。
func TestRequestReset_EmptyStringsAreAccepted(t *testing.T) {
	h, _, resetRepo := newTestHandler()
	for _, body := range []string{`{"username":"","email":"a@example.com"}`, `{"username":"a","email":""}`} {
		rec := post(h.RequestReset, body)
		assert.Equal(t, http.StatusNoContent, rec.Code, body)
	}
	assert.Empty(t, resetRepo.requests)
	assert.Equal(t, http.StatusBadRequest, post(h.RequestReset, `{"username":"a"}`).Code)
}

// 空の token は存在しない token と同じ応答にする。空の password と 72 バイトを
// 超える password は、token の照合の後で拒否し、token は消さない (#3845)。
func TestReset_TokenIsCheckedBeforePassword(t *testing.T) {
	h, userRepo, resetRepo := newTestHandler()
	userRepo.profiles["u1"] = &model.UserProfile{UserID: "u1"}
	idGen, _ := id.NewGenerator("aidx")
	resetID := idGen.Generate(time.Now())
	resetRepo.requests[resetID] = &model.PasswordResetRequest{ID: resetID, Token: "valid-token", UserID: "u1"}

	// 空の token の行があっても当てない。mock の FindByToken("") は行を返すので、
	// 空の token を引く前に止めていることを確かめられる。
	emptyID := idGen.Generate(time.Now())
	resetRepo.requests[emptyID] = &model.PasswordResetRequest{ID: emptyID, Token: "", UserID: "u1"}

	const invalidToken = "6382759e-0a0d-4e32-893e-0e1e66cec4d5"
	for _, body := range []string{
		`{"token":"","password":"newpassword"}`,
		`{"token":"","password":""}`,
		`{"token":"ghost","password":""}`,
		`{"token":"ghost","password":"` + strings.Repeat("a", 73) + `"}`,
	} {
		rec := post(h.Reset, body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, body)
		assert.Contains(t, rec.Body.String(), invalidToken, body)
	}

	for _, body := range []string{
		`{"token":"valid-token","password":""}`,
		`{"token":"valid-token","password":"` + strings.Repeat("a", 73) + `"}`,
	} {
		rec := post(h.Reset, body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, body)
		assert.Contains(t, rec.Body.String(), "3d81ceae-475f-4600-b2a8-2bc116157532", body)
		assert.NotContains(t, rec.Body.String(), invalidToken, body)
	}
	assert.Len(t, resetRepo.requests, 2, "a rejected password must not consume the token")
	assert.Nil(t, userRepo.profiles["u1"].Password)

	assert.Equal(t, http.StatusBadRequest, post(h.Reset, `{"token":"valid-token"}`).Code)
}
