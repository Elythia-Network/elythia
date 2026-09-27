package i

import (
	"net/http"
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// streamEventLog records main-stream publishes and stream revocations in the
// order they happen so tests can assert the ordering between them.
type streamEventLog struct {
	events []string
}

type loggingMainPublisher struct{ log *streamEventLog }

func (p loggingMainPublisher) PublishMainEvent(userID, eventType string, _ any) {
	p.log.events = append(p.log.events, "main:"+userID+":"+eventType)
}

type loggingStreamRevoker struct{ log *streamEventLog }

func (r loggingStreamRevoker) RevokeUserStreams(userID string) {
	r.log.events = append(r.log.events, "revokeUser:"+userID)
}

func (r loggingStreamRevoker) RevokeNativeTokenStreams(userID, token string) {
	r.log.events = append(r.log.events, "revokeNative:"+userID+":"+token)
}

func (r loggingStreamRevoker) RevokeAccessTokenStreams(userID, tokenID string) {
	r.log.events = append(r.log.events, "revokeApp:"+userID+":"+tokenID)
}

// 旧 token で張られた WebSocket を閉じる。**myTokenRegenerated を送った後に**
// 閉じる (閉じる側は猶予の間に届いた event を送り切るので、この順序でないと
// 通知が落ちうる)。
func TestRegenerateToken_RevokesOldTokenStreamsAfterNotifying(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	oldToken := *user.Token
	log := &streamEventLog{}
	h.SetMainStreamPublisher(loggingMainPublisher{log})
	h.SetStreamRevoker(loggingStreamRevoker{log})
	assert.True(t, h.HasStreamRevoker())

	rec := postExtra(h.RegenerateToken, `{"password":"pass"}`, user)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{
		"main:u1:myTokenRegenerated",
		"revokeNative:u1:" + oldToken,
	}, log.events)
}

// パスワードが違えば token は変わらないので、接続も閉じない。
func TestRegenerateToken_WrongPasswordDoesNotRevokeStreams(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	log := &streamEventLog{}
	h.SetStreamRevoker(loggingStreamRevoker{log})

	rec := postExtra(h.RegenerateToken, `{"password":"wrong"}`, user)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, log.events)
}

// 失効させた access token で張られた WebSocket を閉じる (アプリ連携の解除も
// この endpoint を通る)。
func TestRevokeToken_RevokesThatTokenStreams(t *testing.T) {
	h, _ := newExtraHandler(t)
	tokens := testutil.NewMockAccessTokenRepository()
	tokens.Tokens["h1"] = &model.AccessToken{ID: "t1", Hash: "h1", UserID: stubUser.ID}
	h.SetAccessTokenRepo(tokens)
	log := &streamEventLog{}
	h.SetStreamRevoker(loggingStreamRevoker{log})

	rec := postExtra(h.RevokeToken, `{"tokenId":"t1"}`, stubUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"revokeApp:" + stubUser.ID + ":t1"}, log.events)
}

// 他人の token は no-op (204) なので、接続も閉じない。
func TestRevokeToken_ForeignTokenDoesNotRevokeStreams(t *testing.T) {
	h, _ := newExtraHandler(t)
	tokens := testutil.NewMockAccessTokenRepository()
	tokens.Tokens["h1"] = &model.AccessToken{ID: "t1", Hash: "h1", UserID: "someone-else"}
	h.SetAccessTokenRepo(tokens)
	log := &streamEventLog{}
	h.SetStreamRevoker(loggingStreamRevoker{log})

	rec := postExtra(h.RevokeToken, `{"tokenId":"t1"}`, stubUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, log.events)
}

// 自己削除は全端末の WebSocket を閉じる。
func TestDeleteAccount_RevokesAllUserStreams(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	log := &streamEventLog{}
	h.SetStreamRevoker(loggingStreamRevoker{log})

	rec := postExtra(h.DeleteAccount, `{"password":"pass"}`, user)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"revokeUser:u1"}, log.events)
}

func TestHasStreamRevoker_Unwired(t *testing.T) {
	h, _ := newExtraHandler(t)
	assert.False(t, h.HasStreamRevoker())
}
