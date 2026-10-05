package i

import (
	"errors"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/core/move"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"golang.org/x/crypto/bcrypt"
)

// stubMover lets tests dictate what AccountMover.Move returns.
type stubMover struct {
	err    error
	called bool
	gotSrc *model.User
	gotURI string
}

func (s *stubMover) Move(src *model.User, dstURI string) error {
	s.called = true
	s.gotSrc = src
	s.gotURI = dstURI
	return s.err
}

func TestMove_InvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetAccountMover(&stubMover{})
	// moveToAccount 欠落は 400 (#1546: password は任意なので password だけでも moveToAccount 無しは 400)。
	assert.Equal(t, http.StatusBadRequest, post(h.Move, `{"password":"pw"}`, &model.User{ID: "me"}).Code)
	assert.Equal(t, http.StatusBadRequest, post(h.Move, `{}`, &model.User{ID: "me"}).Code)
}

// #1546: password は任意。未指定でも 400 にならず移行が進む (upstream は password
// param を持たず secure:true session 検証で代替するため)。
func TestMove_PasswordOptional(t *testing.T) {
	h, user, setMover := moveHandlerWithPasswordUser(t)
	sm := &stubMover{}
	setMover(sm)
	// password を送らない。
	rec := post(h.Move, `{"moveToAccount":"https://other.example/users/x"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, sm.called)
	assert.Equal(t, "https://other.example/users/x", sm.gotURI)
}

// #1546: root ユーザーは移行不可 (NOT_ROOT_FORBIDDEN)。
func TestMove_RootForbidden(t *testing.T) {
	h, _, setMover := moveHandlerWithPasswordUser(t)
	sm := &stubMover{}
	setMover(sm)
	root := &model.User{ID: "me", Username: "me", IsRoot: true}
	rec := post(h.Move, `{"moveToAccount":"https://x","password":"pw"}`, root)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NOT_ROOT_FORBIDDEN")
	assert.Contains(t, rec.Body.String(), "4362e8dc-731f-4ad8-a694-be2a88922a24")
	assert.False(t, sm.called)
}

// #1546: acct 形式 (@user@host) は canonical URI へ解決してから mover に渡す。
func TestMove_AcctFormatResolved(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	user := &model.User{ID: "me", Username: "me"}
	userRepo.Users["me"] = user
	// 移行先 remote user を local DB に用意し、acct → URI 解決させる。
	host := "remote.example"
	uri := "https://remote.example/users/bob"
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob", Host: &host, URI: &uri}
	h.SetUserRepo(userRepo)
	sm := &stubMover{}
	h.SetAccountMover(sm)
	rec := post(h.Move, `{"moveToAccount":"@bob@remote.example"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, sm.called)
	assert.Equal(t, "https://remote.example/users/bob", sm.gotURI)
}

// acct を解決できなければ NO_SUCH_USER。
func TestMove_AcctFormatNotFound(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	user := &model.User{ID: "me", Username: "me"}
	userRepo.Users["me"] = user
	h.SetUserRepo(userRepo)
	h.SetAccountMover(&stubMover{})
	rec := post(h.Move, `{"moveToAccount":"@ghost@remote.example"}`, user)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER")
}

func TestMove_NoProfile(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	// user row はあるが profile 未登録 (= password 未設定)
	userRepo.Users["me"] = &model.User{ID: "me"}
	h.SetAccountMover(&stubMover{})
	rec := post(h.Move, `{"moveToAccount":"https://x","password":"pw"}`, &model.User{ID: "me"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "ACCESS_DENIED")
}

func TestMove_WrongPassword(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("correct"), bcrypt.MinCost)
	hashStr := string(hash)
	userRepo.Users["me"] = &model.User{ID: "me"}
	userRepo.Profiles["me"] = &model.UserProfile{UserID: "me", Password: &hashStr}
	sm := &stubMover{}
	h.SetAccountMover(sm)
	rec := post(h.Move, `{"moveToAccount":"https://x","password":"wrong"}`, &model.User{ID: "me"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INCORRECT_PASSWORD")
	assert.False(t, sm.called, "パスワード誤りなら Move は呼ばれない")
}

func TestMove_NoMover(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	hashStr := string(hash)
	userRepo.Users["me"] = &model.User{ID: "me"}
	userRepo.Profiles["me"] = &model.UserProfile{UserID: "me", Password: &hashStr}
	// mover unset
	rec := post(h.Move, `{"moveToAccount":"https://other.example/users/x","password":"pw"}`, &model.User{ID: "me"})
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// moveHandlerWithPasswordUser wires a user + profile + bcrypt hash so the
// password pre-check passes, then returns the authenticated user to pass to
// post(). Body must include "password":"pw".
func moveHandlerWithPasswordUser(t *testing.T) (*Handler, *model.User, func(mover AccountMover)) {
	t.Helper()
	h, userRepo, _, _ := newTestHandler(t)
	hash, _ := bcrypt.GenerateFromPassword([]byte("pw"), bcrypt.MinCost)
	hashStr := string(hash)
	user := &model.User{ID: "me", Username: "me"}
	userRepo.Users["me"] = user
	userRepo.Profiles["me"] = &model.UserProfile{UserID: "me", Password: &hashStr}
	return h, user, func(m AccountMover) { h.SetAccountMover(m) }
}

func TestMove_Success(t *testing.T) {
	h, user, setMover := moveHandlerWithPasswordUser(t)
	sm := &stubMover{}
	setMover(sm)

	rec := post(h.Move, `{"moveToAccount":"https://other.example/users/x","password":"pw"}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, sm.called)
	assert.Equal(t, "https://other.example/users/x", sm.gotURI)
}

func TestMove_NoSuchUser(t *testing.T) {
	h, user, setMover := moveHandlerWithPasswordUser(t)
	setMover(&stubMover{err: move.ErrNoSuchUser})
	rec := post(h.Move, `{"moveToAccount":"https://x","password":"pw"}`, user)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER")
}

func TestMove_AlreadyMoved(t *testing.T) {
	h, user, setMover := moveHandlerWithPasswordUser(t)
	setMover(&stubMover{err: move.ErrAlreadyMoved})
	rec := post(h.Move, `{"moveToAccount":"https://x","password":"pw"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "ALREADY_MOVED")
}

func TestMove_DestinationForbids(t *testing.T) {
	h, user, setMover := moveHandlerWithPasswordUser(t)
	setMover(&stubMover{err: move.ErrDestinationForbids})
	rec := post(h.Move, `{"moveToAccount":"https://x","password":"pw"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "DESTINATION_ACCOUNT_FORBIDS")
}

func TestMove_RemoteSource(t *testing.T) {
	h, user, setMover := moveHandlerWithPasswordUser(t)
	setMover(&stubMover{err: move.ErrRemoteSourceForbidden})
	rec := post(h.Move, `{"moveToAccount":"https://x","password":"pw"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
}

func TestMove_UnexpectedError(t *testing.T) {
	h, user, setMover := moveHandlerWithPasswordUser(t)
	setMover(&stubMover{err: errors.New("boom")})
	rec := post(h.Move, `{"moveToAccount":"https://x","password":"pw"}`, user)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}
