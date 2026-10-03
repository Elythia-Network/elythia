package resetpassword

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/misc"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/misc/password"
	miscsmtp "github.com/shiroha-a/mk/internal/misc/smtp"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// --- Mock Repos ---

type mockUserRepo struct {
	users    map[string]*model.User        // keyed by ID
	profiles map[string]*model.UserProfile // keyed by userID
}

func newMockUserRepo() *mockUserRepo {
	return &mockUserRepo{
		users:    make(map[string]*model.User),
		profiles: make(map[string]*model.UserProfile),
	}
}

func (m *mockUserRepo) Create(u *model.User) error                { m.users[u.ID] = u; return nil }
func (m *mockUserRepo) FindByID(id string) (*model.User, error)   { return m.findByID(id) }
func (m *mockUserRepo) FindByURI(string) (*model.User, error)     { return nil, errMock }
func (m *mockUserRepo) FindByToken(string) (*model.User, error)   { return nil, errMock }
func (m *mockUserRepo) IncrementFollowingCount(string, int) error { return nil }
func (m *mockUserRepo) IncrementFollowersCount(string, int) error { return nil }
func (m *mockUserRepo) SearchUsers(string, string, int, int, string) ([]*model.User, error) {
	return nil, nil
}
func (m *mockUserRepo) SearchByUsernameAndHost(string, *string, bool, int) ([]*model.User, error) {
	return nil, nil
}
func (m *mockUserRepo) UpdateUser(string, map[string]any) error               { return nil }
func (m *mockUserRepo) CreateProfile(*model.UserProfile) error                { return nil }
func (m *mockUserRepo) ListUsers(model.UserListFilter) ([]*model.User, error) { return nil, nil }
func (m *mockUserRepo) ListRemoteInboxes() ([]model.RemoteInbox, error)       { return nil, nil }
func (m *mockUserRepo) CountOnlineUsers() (int64, error)                      { return 0, nil }
func (m *mockUserRepo) CountLocalUsers() (int64, error)                       { return 0, nil }
func (m *mockUserRepo) CountLocalUsersActiveSince(time.Time) (int64, error)   { return 0, nil }
func (m *mockUserRepo) ListLocalUserIDsRegisteredAfter(string) ([]string, error) {
	return nil, nil
}
func (m *mockUserRepo) ListLocalUserIDsActiveSince(time.Time) ([]string, error) {
	return nil, nil
}
func (m *mockUserRepo) ListUserRecommendations(string, time.Time, int, int) ([]*model.User, error) {
	return nil, nil
}

func (m *mockUserRepo) findByID(id string) (*model.User, error) {
	u, ok := m.users[id]
	if !ok {
		return nil, errMock
	}
	return u, nil
}

func (m *mockUserRepo) FindByUsernameLower(username string, host *string) (*model.User, error) {
	for _, u := range m.users {
		if u.UsernameLower == username && host == nil && u.Host == nil {
			return u, nil
		}
	}
	return nil, errMock
}

func (m *mockUserRepo) FindProfileByUserID(userID string) (*model.UserProfile, error) {
	p, ok := m.profiles[userID]
	if !ok {
		return nil, errMock
	}
	return p, nil
}

func (m *mockUserRepo) FindProfileByVerifyCode(string) (*model.UserProfile, error) {
	return nil, errMock
}

func (m *mockUserRepo) FindManyByIDs([]string) ([]*model.User, error) {
	return nil, nil
}

func (m *mockUserRepo) FindManyByUsernamesAndHost([]string, *string) ([]*model.User, error) {
	return nil, nil
}

func (m *mockUserRepo) IncrementNotesCount(string, int) error { return nil }

func (m *mockUserRepo) FindProfilesByUserIDs([]string) ([]*model.UserProfile, error) {
	return nil, nil
}

func (m *mockUserRepo) FindProfileByEmail(string) (*model.UserProfile, error) {
	return nil, errMock
}

func (m *mockUserRepo) UpdateProfile(userID string, fields map[string]any) error {
	p, ok := m.profiles[userID]
	if !ok {
		return errMock
	}
	if pw, has := fields["password"]; has {
		s := pw.(string)
		p.Password = &s
	}
	return nil
}

func (m *mockUserRepo) RemoveBackupCode(_, _ string) error { return nil }

func (m *mockUserRepo) UpdatePasswordIfCurrent(userID, currentHash, newHash string) (bool, error) {
	p, ok := m.profiles[userID]
	if !ok || p.Password == nil || *p.Password != currentHash {
		return false, nil
	}
	p.Password = &newHash
	return true, nil
}

// **not-found を模す。** 汎用 error だと #2792 の「DB 障害は 500」に引っかかる。
// repository は GORM の error をそのまま返すので、テストもそれに揃える。
var errMock = repository.ErrNotFound

type mockResetRepo struct {
	requests map[string]*model.PasswordResetRequest
}

func newMockResetRepo() *mockResetRepo {
	return &mockResetRepo{requests: make(map[string]*model.PasswordResetRequest)}
}

func (m *mockResetRepo) Create(req *model.PasswordResetRequest) error {
	m.requests[req.ID] = req
	return nil
}

func (m *mockResetRepo) FindByToken(token string) (*model.PasswordResetRequest, error) {
	for _, r := range m.requests {
		if r.Token == token {
			return r, nil
		}
	}
	return nil, errMock
}

func (m *mockResetRepo) Delete(id string) error {
	delete(m.requests, id)
	return nil
}

func newTestHandler() (*Handler, *mockUserRepo, *mockResetRepo) {
	idGen, _ := id.NewGenerator("aidx")
	userRepo := newMockUserRepo()
	resetRepo := newMockResetRepo()
	h := NewHandler(userRepo, resetRepo, idGen)
	h.SetServerURL("https://example.com")
	return h, userRepo, resetRepo
}

func post(handler func(echo.Context) error, body string) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = handler(c)
	return rec
}

// --- RequestReset ---

func TestRequestReset_Success(t *testing.T) {
	h, userRepo, resetRepo := newTestHandler()

	email := "test@example.com"
	userRepo.users["u1"] = &model.User{ID: "u1", Username: "testuser", UsernameLower: "testuser"}
	userRepo.profiles["u1"] = &model.UserProfile{
		UserID:        "u1",
		Email:         &email,
		EmailVerified: true,
	}

	var mu sync.Mutex
	var sentTo string
	var sent miscsmtp.Message
	h.SetEmailSender(func(to string, msg miscsmtp.Message) {
		mu.Lock()
		defer mu.Unlock()
		sentTo = to
		sent = msg
	})

	rec := post(h.RequestReset, `{"username":"testuser","email":"test@example.com"}`)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// リセットリクエストが保存されている
	assert.Len(t, resetRepo.requests, 1)

	// メール送信を待つ
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	assert.Equal(t, "test@example.com", sentTo)
	assert.Equal(t, "Password reset", sent.Subject)
	assert.NotEmpty(t, sent.Text, "text body は必須")
	assert.Contains(t, sent.HTML, "<!doctype html>", "HTML wrapper が同送される (#600 item 4)")
	mu.Unlock()
}

func TestRequestReset_SendsJapaneseEmailFromMetaLangsWhenProfileLangUnset(t *testing.T) {
	h, userRepo, resetRepo := newTestHandler()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x", Langs: []string{"ja-JP"}}
	h.SetMetaRepo(metaRepo)

	email := "test@example.com"
	userRepo.users["u1"] = &model.User{ID: "u1", Username: "testuser", UsernameLower: "testuser"}
	userRepo.profiles["u1"] = &model.UserProfile{UserID: "u1", Email: &email, EmailVerified: true}

	var mu sync.Mutex
	var sent miscsmtp.Message
	h.SetEmailSender(func(_ string, msg miscsmtp.Message) {
		mu.Lock()
		defer mu.Unlock()
		sent = msg
	})

	rec := post(h.RequestReset, `{"username":"testuser","email":"test@example.com"}`)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, resetRepo.requests, 1)

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	assert.Equal(t, "パスワードのリセット", sent.Subject)
	mu.Unlock()
}

func TestRequestReset_SendsJapaneseEmailWhenProfileLangIsJa(t *testing.T) {
	h, userRepo, resetRepo := newTestHandler()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x", Langs: []string{"en-US", "ja-JP"}}
	h.SetMetaRepo(metaRepo)

	email := "test@example.com"
	ja := "ja-JP"
	userRepo.users["u1"] = &model.User{ID: "u1", Username: "testuser", UsernameLower: "testuser"}
	userRepo.profiles["u1"] = &model.UserProfile{UserID: "u1", Email: &email, EmailVerified: true, Lang: &ja}

	var mu sync.Mutex
	var sent miscsmtp.Message
	h.SetEmailSender(func(_ string, msg miscsmtp.Message) {
		mu.Lock()
		defer mu.Unlock()
		sent = msg
	})

	rec := post(h.RequestReset, `{"username":"testuser","email":"test@example.com"}`)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, resetRepo.requests, 1)

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	assert.Equal(t, "パスワードのリセット", sent.Subject)
	assert.Contains(t, sent.Text, "パスワードをリセット")
	mu.Unlock()
}

func TestRequestReset_UserNotFound(t *testing.T) {
	h, _, resetRepo := newTestHandler()

	// ユーザーが存在しなくても成功レスポンス（情報漏洩防止）
	rec := post(h.RequestReset, `{"username":"ghost","email":"x@x.com"}`)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, resetRepo.requests, 0)
}

func TestRequestReset_EmailMismatch(t *testing.T) {
	h, userRepo, resetRepo := newTestHandler()

	email := "real@example.com"
	userRepo.users["u1"] = &model.User{ID: "u1", Username: "testuser", UsernameLower: "testuser"}
	userRepo.profiles["u1"] = &model.UserProfile{
		UserID:        "u1",
		Email:         &email,
		EmailVerified: true,
	}

	rec := post(h.RequestReset, `{"username":"testuser","email":"wrong@example.com"}`)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, resetRepo.requests, 0)
}

func TestRequestReset_EmailNotVerified(t *testing.T) {
	h, userRepo, resetRepo := newTestHandler()

	email := "test@example.com"
	userRepo.users["u1"] = &model.User{ID: "u1", Username: "testuser", UsernameLower: "testuser"}
	userRepo.profiles["u1"] = &model.UserProfile{
		UserID:        "u1",
		Email:         &email,
		EmailVerified: false,
	}

	rec := post(h.RequestReset, `{"username":"testuser","email":"test@example.com"}`)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, resetRepo.requests, 0)
}

func TestRequestReset_InvalidParam(t *testing.T) {
	h, _, _ := newTestHandler()

	rec := post(h.RequestReset, `{}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	rec = post(h.RequestReset, `{"username":"test"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Reset ---

func TestReset_Success(t *testing.T) {
	h, userRepo, resetRepo := newTestHandler()

	pw, _ := bcrypt.GenerateFromPassword([]byte("oldpassword"), 8)
	oldPw := string(pw)
	userRepo.profiles["u1"] = &model.UserProfile{UserID: "u1", Password: &oldPw}

	// 有効なリセットトークンを作成（IDは現在時刻で生成）
	idGen, _ := id.NewGenerator("aidx")
	resetID := idGen.Generate(time.Now())
	resetRepo.requests[resetID] = &model.PasswordResetRequest{
		ID:     resetID,
		Token:  "valid-token",
		UserID: "u1",
	}

	rec := post(h.Reset, `{"token":"valid-token","password":"newpassword"}`)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// パスワードが更新されている
	newPw := *userRepo.profiles["u1"].Password
	assert.True(t, strings.HasPrefix(newPw, "$2"))
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(newPw), []byte("newpassword")))
	cost, err := bcrypt.Cost([]byte(newPw))
	require.NoError(t, err)
	assert.Equal(t, password.Cost(), cost)

	// トークンが削除されている
	assert.Len(t, resetRepo.requests, 0)
}

func TestReset_TokenNotFound(t *testing.T) {
	h, _, _ := newTestHandler()

	rec := post(h.Reset, `{"token":"invalid","password":"newpw"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestReset_TokenExpired(t *testing.T) {
	h, _, resetRepo := newTestHandler()

	// 31分前のIDを生成
	idGen, _ := id.NewGenerator("aidx")
	resetID := idGen.Generate(time.Now().Add(-31 * time.Minute))
	resetRepo.requests[resetID] = &model.PasswordResetRequest{
		ID:     resetID,
		Token:  "expired-token",
		UserID: "u1",
	}

	rec := post(h.Reset, `{"token":"expired-token","password":"newpw"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]any)
	assert.Contains(t, errObj["message"], "expired")
}

func TestReset_InvalidParam(t *testing.T) {
	h, _, _ := newTestHandler()

	rec := post(h.Reset, `{}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	rec = post(h.Reset, `{"token":"t"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestReset_PasswordTooLong(t *testing.T) {
	h, _, resetRepo := newTestHandler()
	resetRepo.requests["tok1"] = &model.PasswordResetRequest{
		ID: "r1", Token: "tok1", UserID: "u1",
	}
	longPw := strings.Repeat("a", 73)
	rec := post(h.Reset, `{"token":"tok1","password":"`+longPw+`"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestSecureRandomHex(t *testing.T) {
	s := misc.SecureRandomHex(64)
	assert.Len(t, s, 64)
	s2 := misc.SecureRandomHex(64)
	assert.NotEqual(t, s, s2)
}

func (m *mockUserRepo) HardDeleteUser(string) error                { return nil }
func (m *mockUserRepo) RevokeDeletedLocalCredentials(string) error { return nil }

// DeleteOrphanRemoteUsers implements repository.UserRepository (#2340).
func (m *mockUserRepo) DeleteOrphanRemoteUsers(_, _ int) (int64, error) { return 0, nil }

// failingResetRepo makes every token lookup look like a database failure.
type failingResetRepo struct {
	*mockResetRepo
	err error
}

func (r *failingResetRepo) FindByToken(string) (*model.PasswordResetRequest, error) {
	return nil, r.err
}

// **DB 障害を「そんなトークンは無い」にしない** (#2792)。
//
// パスワードリセットは利用者がアカウントを取り戻す最後の経路なので、障害を
// 400 で返すと「リンクが切れた」と判断して問い合わせが来る。
func TestReset_DBFailureIsNot4xx(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(newMockUserRepo(), &failingResetRepo{
		mockResetRepo: newMockResetRepo(),
		err:           errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"),
	}, idGen)

	rec := post(h.Reset, `{"token":"t","password":"newpassword"}`)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func (m *mockUserRepo) EmailVerifiedInUse(string) (bool, error) { return false, nil }
