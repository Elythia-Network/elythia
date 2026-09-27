package i

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/core/notification"
	coreuser "github.com/shiroha-a/mk/internal/core/user"
	"github.com/shiroha-a/mk/internal/entitycompat/shapetest"
	"github.com/shiroha-a/mk/internal/misc/id"
	passwordutil "github.com/shiroha-a/mk/internal/misc/password"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/server/middleware"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/datatypes"
)

func newExtraHandler(t *testing.T) (*Handler, *testutil.MockUserRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	h := NewHandler(svc, idGen)
	h.SetFavoriteRepo(testutil.NewMockNoteFavoriteRepository())
	return h, userRepo
}

// postExtraCanceled は**キャンセル済みの request context** で叩く。
// Argon2id の検証枠を取れなかった経路 (OutcomeUnavailable) を待ち時間ゼロで再現する。
func postExtraCanceled(h func(echo.Context) error, body string, user *model.User) *httptest.ResponseRecorder {
	e := echo.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	_ = h(c)
	return rec
}

func postExtra(h func(echo.Context) error, body string, user *model.User) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	_ = h(c)
	return rec
}

func setupUserWithPassword(repo *testutil.MockUserRepository, uid, plain string) *model.User {
	hash, _ := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.MinCost)
	hashStr := string(hash)
	token := "tok12345678901234"
	user := &model.User{ID: uid, Username: uid, Token: &token}
	repo.Users[uid] = user
	repo.Profiles[uid] = &model.UserProfile{UserID: uid, Password: &hashStr}
	return user
}

func setupUserWithArgon2Password(repo *testutil.MockUserRepository, uid, plain string) *model.User {
	salt := []byte("0123456789abcdef")
	digest := argon2.IDKey([]byte(plain), salt, 3, 64*1024, 4, 32)
	hash := fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=4$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest))
	token := "tok12345678901234"
	user := &model.User{ID: uid, Username: uid, Token: &token}
	repo.Users[uid] = user
	repo.Profiles[uid] = &model.UserProfile{UserID: uid, Password: &hash}
	return user
}

// stubUser is the throwaway authenticated principal used by per-handler test
// files (apps_test.go, signin_history_test.go, email_update_test.go, etc.).
var stubUser = &model.User{ID: "u1"}

// hashPassword returns a bcrypt hash for the given plaintext password. Used by
// per-handler test files that exercise password-protected endpoints
// (email_update_test.go, move_test.go).
func hashPassword(pw string) string {
	h, _ := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	return string(h)
}

// --- ChangePassword ---

func TestChangePassword_Success(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "oldpass")
	rec := postExtra(h.ChangePassword, `{"currentPassword":"oldpass","newPassword":"newpass"}`, user)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestChangePassword_AcceptsArgon2AndStoresBcrypt(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithArgon2Password(repo, "u1", "oldpass")
	rec := postExtra(h.ChangePassword, `{"currentPassword":"oldpass","newPassword":"newpass"}`, user)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	stored := *repo.Profiles["u1"].Password
	assert.True(t, strings.HasPrefix(stored, "$2"))
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(stored), []byte("newpass")))
	cost, err := bcrypt.Cost([]byte(stored))
	require.NoError(t, err)
	assert.Equal(t, passwordutil.Cost(), cost)
}

func TestChangePassword_RejectsWrongArgon2PasswordWithoutRewrite(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithArgon2Password(repo, "u1", "oldpass")
	before := *repo.Profiles["u1"].Password
	rec := postExtra(h.ChangePassword, `{"currentPassword":"wrong","newPassword":"newpass"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, before, *repo.Profiles["u1"].Password)
}

func TestChangePassword_WrongPassword(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "correct")
	rec := postExtra(h.ChangePassword, `{"currentPassword":"wrong","newPassword":"x"}`, user)
	// upstream Misskey TS は raw `throw new Error` を framework が 401 へ
	// 変換 (#885)。drop-in 互換のため 401 に揃える (旧 mk-go は 403)。
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestChangePassword_NoProfile(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := &model.User{ID: "u1"}
	repo.Users["u1"] = user
	rec := postExtra(h.ChangePassword, `{"currentPassword":"x","newPassword":"y"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestChangePassword_InvalidParam(t *testing.T) {
	h, _ := newExtraHandler(t)
	rec := postExtra(h.ChangePassword, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TOTP gate (upstream drop-in 互換): 2FA 有効ユーザが token 無しで
// change-password を呼ぶと 403 INVALID_TOKEN で refuse される。
func TestChangePassword_With2FA_RequiresToken(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "oldpass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	rec := postExtra(h.ChangePassword, `{"currentPassword":"oldpass","newPassword":"newpass"}`, user)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_TOKEN")
}

// 2FA 有効でも valid token (backup code) を渡せば成功する。
func TestChangePassword_With2FA_AcceptsBackupCode(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "oldpass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	rec := postExtra(h.ChangePassword, `{"currentPassword":"oldpass","newPassword":"newpass","token":"backup1"}`, user)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// 73 byte 以上の newPassword は bcrypt の上限を超えるため 400 + PASSWORD_TOO_LONG (#1075)。
func TestChangePassword_NewPasswordTooLong(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "oldpass")
	longPw := strings.Repeat("a", 73)
	body := `{"currentPassword":"oldpass","newPassword":"` + longPw + `"}`
	rec := postExtra(h.ChangePassword, body, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "PASSWORD_TOO_LONG")
}

// --- DeleteAccount ---

func TestDeleteAccount_Success(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	rec := postExtra(h.DeleteAccount, `{"password":"pass"}`, user)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, repo.Users["u1"].IsSuspended)
	assert.True(t, repo.Users["u1"].IsDeleted)
}

func TestDeleteAccount_WrongPassword(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	_ = user
	rec := postExtra(h.DeleteAccount, `{"password":"wrong"}`, repo.Users["u1"])
	// upstream Misskey TS と drop-in 互換: 401 (#885)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDeleteAccount_NoProfile(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := &model.User{ID: "u1"}
	repo.Users["u1"] = user
	rec := postExtra(h.DeleteAccount, `{"password":"x"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDeleteAccount_InvalidParam(t *testing.T) {
	h, _ := newExtraHandler(t)
	rec := postExtra(h.DeleteAccount, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TOTP gate (upstream drop-in 互換): 2FA 有効ユーザが token 無しで
// delete-account を呼ぶと 403 INVALID_TOKEN で refuse される。
func TestDeleteAccount_With2FA_RequiresToken(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	rec := postExtra(h.DeleteAccount, `{"password":"pass"}`, user)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_TOKEN")
	// gate で弾かれているので account は削除されていないこと
	assert.False(t, repo.Users["u1"].IsDeleted)
}

// 2FA 有効でも valid token (backup code) を渡せば成功する。
func TestDeleteAccount_With2FA_AcceptsBackupCode(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	enableTwoFactorWithBackupCodes(repo, "u1")
	rec := postExtra(h.DeleteAccount, `{"password":"pass","token":"backup1"}`, user)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, repo.Users["u1"].IsDeleted)
}

// #962 P0 + #967: 論理削除完了後に auth middleware の tokenCache を
// **user 単位で** invalidate する。本 user の他 device session
// (= 別 token を持つ web / mobile / 3rd party app セッション) も即時
// 失効させ、削除済 user 名義での操作が cache TTL 内 (最大 30s) に
// 残らないようにする。
func TestDeleteAccount_InvalidatesAllUserTokens(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")

	inv := &stubTokenInvalidator{}
	h.SetAuthInvalidator(inv)

	rec := postExtra(h.DeleteAccount, `{"password":"pass"}`, user)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, []string{"u1"}, inv.userCalls,
		"DeleteAccount 成功時は user の全 token を invalidate するべき (admin/suspend-user #966 と同 shape の self-bypass を塞ぐ)")
	// token 単独 invalidate (#963) からの移行で、token-base は呼ばれない
	// 想定。# 967 では他 device session も含めて全部消すのが目的。
	assert.Empty(t, inv.calls,
		"user-level invalidate に切り替えたので token 単独 invalidate は呼ばれない")
}

// invalidator が wire されていないとき (test 直叩き / router 配線忘れ) は
// 削除自体は成功して 204 を返す。core 削除挙動が invalidator dependency に
// 引きずられない defensive。
func TestDeleteAccount_NoInvalidatorIsNoop(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")

	rec := postExtra(h.DeleteAccount, `{"password":"pass"}`, user)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, repo.Users["u1"].IsDeleted)
}

// --- Favorites ---

func TestFavorites_Success(t *testing.T) {
	h, _ := newExtraHandler(t)
	rec := postExtra(h.Favorites, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFavorites_NilRepo(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(userRepo, noteRepo, piningRepo, idGen)
	h := NewHandler(svc, idGen)
	// favoriteRepo not set
	rec := postExtra(h.Favorites, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestFavorites_InvalidJSON(t *testing.T) {
	h, _ := newExtraHandler(t)
	rec := postExtra(h.Favorites, `invalid`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestFavorites_WithNote(t *testing.T) {
	h, _ := newExtraHandler(t)
	favRepo := testutil.NewMockNoteFavoriteRepository()
	favRepo.Favorites["u1:n1"] = &model.NoteFavorite{
		ID: "f1", UserID: "u1", NoteID: "n1",
		Note: &model.Note{ID: "n1", UserID: "u1", Visibility: "public"},
	}
	h.SetFavoriteRepo(favRepo)
	rec := postExtra(h.Favorites, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	shapetest.Assert(t, "NoteFavorite", resp[0]) // L3 (#1286)
	// #1556 createdAt は固定 ms 精度 ISO-8601 (RFC3339Nano ではない)。
	createdAt, _ := resp[0]["createdAt"].(string)
	_, perr := time.Parse("2006-01-02T15:04:05.000Z", createdAt)
	assert.NoError(t, perr, "createdAt は ms 精度 ISO-8601: %q", createdAt)
}

// **閲覧資格を失った followers note は本文を伏せて返すこと。**
//
// favorite した後で作者のフォローを外した (外された) 場合、upstream は
// NoteFavoriteEntityService → noteEntityService.pack(note, me) の hideNote で
// 本文を空にして `isHidden: true` で返す。行そのものは残す。自分の note は
// 伏せない。
func TestFavorites_HidesNoteNoLongerVisible(t *testing.T) {
	h, _ := newExtraHandler(t)
	favRepo := testutil.NewMockNoteFavoriteRepository()
	secret := "followers only secret"
	mine := "my own followers note"
	favRepo.Favorites["u1:n1"] = &model.NoteFavorite{
		ID: "f1", UserID: "u1", NoteID: "n1",
		Note: &model.Note{ID: "n1", UserID: "author", Visibility: model.NoteVisibilityFollowers, Text: &secret},
	}
	favRepo.Favorites["u1:n2"] = &model.NoteFavorite{
		ID: "f2", UserID: "u1", NoteID: "n2",
		Note: &model.Note{ID: "n2", UserID: "u1", Visibility: model.NoteVisibilityFollowers, Text: &mine},
	}
	h.SetFavoriteRepo(favRepo)
	rec := postExtra(h.Favorites, `{}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), secret)

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 2, "行は落とさない")
	byNote := map[string]map[string]any{}
	for _, r := range resp {
		byNote[r["noteId"].(string)] = r["note"].(map[string]any)
	}
	assert.Equal(t, true, byNote["n1"]["isHidden"])
	assert.Nil(t, byNote["n1"]["text"])
	assert.Equal(t, mine, byNote["n2"]["text"], "自分の note は伏せない")
}

// untilId 経由の cursor pagination が repo に正しく伝わり、cursor 以下の
// favorite だけ返ることを検証する (#424 の core fix)。
func TestFavorites_UntilIDPaginates(t *testing.T) {
	h, _ := newExtraHandler(t)
	favRepo := testutil.NewMockNoteFavoriteRepository()
	for _, fid := range []string{"f1", "f2", "f3"} {
		nid := "n_" + fid
		favRepo.Favorites["u1:"+nid] = &model.NoteFavorite{
			ID: fid, UserID: "u1", NoteID: nid,
			Note: &model.Note{ID: nid, UserID: "u1", Visibility: "public"},
		}
	}
	h.SetFavoriteRepo(favRepo)

	// untilId=f3 → f3 より小さい id (f1,f2) のみ返るはず
	rec := postExtra(h.Favorites, `{"untilId":"f3","limit":10}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Len(t, got, 2)
	for _, item := range got {
		id := item["id"].(string)
		assert.Less(t, id, "f3")
	}
}

// --- RegenerateToken ---

func TestRegenerateToken_Success(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	rec := postExtra(h.RegenerateToken, `{"password":"pass"}`, user)
	// upstream Misskey TS と drop-in 互換のため 204 No Content (#883)。
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestRegenerateToken_WrongPassword(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	_ = user
	rec := postExtra(h.RegenerateToken, `{"password":"wrong"}`, repo.Users["u1"])
	// upstream Misskey TS は raw `throw new Error('incorrect password')` →
	// framework が 401 (#885)。
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestRegenerateToken_NoProfile(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := &model.User{ID: "u1"}
	repo.Users["u1"] = user
	rec := postExtra(h.RegenerateToken, `{"password":"x"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// stubIMainStreamPublisher captures PublishMainEvent calls for assertion.
type stubIMainStreamPublisher struct {
	calls []iMainEventCall
}

type iMainEventCall struct {
	userID    string
	eventType string
	body      any
}

func (s *stubIMainStreamPublisher) PublishMainEvent(userID, eventType string, body any) {
	s.calls = append(s.calls, iMainEventCall{userID, eventType, body})
}

func TestRegenerateToken_PublishesMyTokenRegenerated(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	pub := &stubIMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)
	rec := postExtra(h.RegenerateToken, `{"password":"pass"}`, user)
	// 204 No Content (= drop-in 互換、#883)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Len(t, pub.calls, 1)
	assert.Equal(t, "u1", pub.calls[0].userID)
	assert.Equal(t, "myTokenRegenerated", pub.calls[0].eventType)
	// TS本家はbody無し (type のみ) のため nil であること。
	assert.Nil(t, pub.calls[0].body)
}

// stubTokenInvalidator captures InvalidateToken / InvalidateTokensForUser
// calls for assertion. token-base (#884) と user-base (#965/#967) の両 API
// を持つ TokenInvalidator interface を test 内で full にスタブ化する。
type stubTokenInvalidator struct {
	calls     []string // token-base 履歴
	userCalls []string // user-base 履歴 (for #967 / i/delete-account)
}

func (s *stubTokenInvalidator) InvalidateToken(token string) {
	s.calls = append(s.calls, token)
}

func (s *stubTokenInvalidator) InvalidateTokensForUser(userID string) {
	s.userCalls = append(s.userCalls, userID)
}

// TestRegenerateToken_InvalidatesOldToken verifies #884: the old API token
// must be removed from the auth cache so it stops being accepted by /api/i.
func TestRegenerateToken_InvalidatesOldToken(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	oldToken := "old-token-value"
	user.Token = &oldToken
	repo.Users["u1"] = user

	inv := &stubTokenInvalidator{}
	h.SetAuthInvalidator(inv)

	rec := postExtra(h.RegenerateToken, `{"password":"pass"}`, user)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Len(t, inv.calls, 1, "old token must be invalidated exactly once")
	assert.Equal(t, oldToken, inv.calls[0])
}

// --- Error path tests ---

type failingUpdateUserRepo struct {
	*testutil.MockUserRepository
}

func (f *failingUpdateUserRepo) UpdateUser(_ string, _ map[string]any) error {
	return testutil.ErrNotFound
}

func (f *failingUpdateUserRepo) UpdateProfile(_ string, _ map[string]any) error {
	return testutil.ErrNotFound
}

func TestChangePassword_UpdateError(t *testing.T) {
	failRepo := &failingUpdateUserRepo{testutil.NewMockUserRepository()}
	hash, _ := bcrypt.GenerateFromPassword([]byte("pass"), bcrypt.MinCost)
	hashStr := string(hash)
	failRepo.Users["u1"] = &model.User{ID: "u1"}
	failRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Password: &hashStr}
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(failRepo, testutil.NewMockNoteRepository(), testutil.NewMockUserNotePiningRepository(), idGen)
	h := NewHandler(svc, idGen)
	rec := postExtra(h.ChangePassword, `{"currentPassword":"pass","newPassword":"new"}`, failRepo.Users["u1"])
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestDeleteAccount_UpdateError(t *testing.T) {
	failRepo := &failingUpdateUserRepo{testutil.NewMockUserRepository()}
	hash, _ := bcrypt.GenerateFromPassword([]byte("pass"), bcrypt.MinCost)
	hashStr := string(hash)
	failRepo.Users["u1"] = &model.User{ID: "u1"}
	failRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Password: &hashStr}
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(failRepo, testutil.NewMockNoteRepository(), testutil.NewMockUserNotePiningRepository(), idGen)
	h := NewHandler(svc, idGen)
	rec := postExtra(h.DeleteAccount, `{"password":"pass"}`, failRepo.Users["u1"])
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRegenerateToken_UpdateError(t *testing.T) {
	failRepo := &failingUpdateUserRepo{testutil.NewMockUserRepository()}
	hash, _ := bcrypt.GenerateFromPassword([]byte("pass"), bcrypt.MinCost)
	hashStr := string(hash)
	failRepo.Users["u1"] = &model.User{ID: "u1"}
	failRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Password: &hashStr}
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(failRepo, testutil.NewMockNoteRepository(), testutil.NewMockUserNotePiningRepository(), idGen)
	h := NewHandler(svc, idGen)
	rec := postExtra(h.RegenerateToken, `{"password":"pass"}`, failRepo.Users["u1"])
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

type failingFavListRepo struct {
	*testutil.MockNoteFavoriteRepository
}

func (f *failingFavListRepo) ListByUser(_ string, _, _ string, _ int) ([]*model.NoteFavorite, error) {
	return nil, testutil.ErrNotFound
}

func TestFavorites_ListError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(userRepo, testutil.NewMockNoteRepository(), testutil.NewMockUserNotePiningRepository(), idGen)
	h := NewHandler(svc, idGen)
	h.SetFavoriteRepo(&failingFavListRepo{testutil.NewMockNoteFavoriteRepository()})
	rec := postExtra(h.Favorites, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestRegenerateToken_InvalidParam(t *testing.T) {
	h, _ := newExtraHandler(t)
	rec := postExtra(h.RegenerateToken, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- ClaimAchievement ---

func TestClaimAchievement_New(t *testing.T) {
	h, userRepo := newExtraHandler(t)
	pw, _ := bcrypt.GenerateFromPassword([]byte("pass"), bcrypt.MinCost)
	pwStr := string(pw)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "test"}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Password: &pwStr}

	rec := postExtra(h.ClaimAchievement, `{"name":"notes1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// 実績が保存されたか確認
	profile := userRepo.Profiles["u1"]
	var achievements []map[string]any
	_ = json.Unmarshal(profile.Achievements, &achievements)
	require.Len(t, achievements, 1)
	assert.Equal(t, "notes1", achievements[0]["name"])
}

func TestClaimAchievement_Duplicate(t *testing.T) {
	h, userRepo := newExtraHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1"}
	userRepo.Profiles["u1"] = &model.UserProfile{
		UserID:       "u1",
		Achievements: datatypes.JSON(`[{"name":"notes1","unlockedAt":1000}]`),
	}

	rec := postExtra(h.ClaimAchievement, `{"name":"notes1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// 実績が増えていないか確認
	var achievements []map[string]any
	_ = json.Unmarshal(userRepo.Profiles["u1"].Achievements, &achievements)
	assert.Len(t, achievements, 1)
}

func TestClaimAchievement_NoProfile(t *testing.T) {
	h, _ := newExtraHandler(t)
	rec := postExtra(h.ClaimAchievement, `{"name":"notes1"}`, &model.User{ID: "ghost"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestClaimAchievement_InvalidParam(t *testing.T) {
	h, _ := newExtraHandler(t)
	rec := postExtra(h.ClaimAchievement, `{}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

type failingProfileUpdateRepo struct {
	*testutil.MockUserRepository
}

func (f *failingProfileUpdateRepo) UpdateProfile(_ string, _ map[string]any) error {
	return testutil.ErrNotFound
}

func TestClaimAchievement_UpdateError(t *testing.T) {
	baseRepo := testutil.NewMockUserRepository()
	baseRepo.Users["u1"] = &model.User{ID: "u1"}
	baseRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1"}

	noteRepo := testutil.NewMockNoteRepository()
	piningRepo := testutil.NewMockUserNotePiningRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(&failingProfileUpdateRepo{baseRepo}, noteRepo, piningRepo, idGen)
	h := NewHandler(svc, idGen)

	rec := postExtra(h.ClaimAchievement, `{"name":"notes1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

type stubAchievementNotifier struct {
	calls []notification.CreateInput
}

func (s *stubAchievementNotifier) Create(_ context.Context, in notification.CreateInput) (*notification.Notification, error) {
	s.calls = append(s.calls, in)
	return &notification.Notification{}, nil
}

// 新規解除では achievementEarned 通知を 1 件作る (notifier 無し / 実績名を
// Extra["achievement"] に格納)。サイレント獲得バグの回帰防止。
func TestClaimAchievement_New_EmitsNotification(t *testing.T) {
	h, userRepo := newExtraHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1"}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1"}
	notifier := &stubAchievementNotifier{}
	h.SetAchievementNotifier(notifier)

	rec := postExtra(h.ClaimAchievement, `{"name":"notes1"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, notifier.calls, 1)
	assert.Equal(t, "u1", notifier.calls[0].NotifieeID)
	assert.Equal(t, notification.TypeAchievementEarned, notifier.calls[0].Type)
	assert.Equal(t, "notes1", notifier.calls[0].Extra["achievement"])
	assert.Empty(t, notifier.calls[0].NotifierID, "achievementEarned は notifier を持たない")
}

// 未知の実績名は upstream paramDef enum と同じく 400 で弾き、profile も通知も
// 触らない。
func TestClaimAchievement_UnknownName(t *testing.T) {
	h, userRepo := newExtraHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1"}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1"}
	notifier := &stubAchievementNotifier{}
	h.SetAchievementNotifier(notifier)

	rec := postExtra(h.ClaimAchievement, `{"name":"bogusAchievement"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, notifier.calls)
	assert.Nil(t, userRepo.Profiles["u1"].Achievements, "未知の実績は記録しない")
}

// 既獲得の再 claim では実績も増えず通知も作らない。
func TestClaimAchievement_Duplicate_NoNotification(t *testing.T) {
	h, userRepo := newExtraHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1"}
	userRepo.Profiles["u1"] = &model.UserProfile{
		UserID:       "u1",
		Achievements: datatypes.JSON(`[{"name":"notes1","unlockedAt":1000}]`),
	}
	notifier := &stubAchievementNotifier{}
	h.SetAchievementNotifier(notifier)

	rec := postExtra(h.ClaimAchievement, `{"name":"notes1"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, notifier.calls)
}

// --- #2230: i/delete-account の cascade 削除 + 連合 Delete ---

type fakeDeleteEnqueuer struct {
	called   []string
	payloads []queue.DeleteAccountPayload
	err      error
}

func (f *fakeDeleteEnqueuer) EnqueueDeleteAccount(p queue.DeleteAccountPayload) error {
	f.called = append(f.called, p.UserID)
	f.payloads = append(f.payloads, p)
	return f.err
}

type fakeAccountDeletionFed struct{ deleted []string }

func (f *fakeAccountDeletionFed) OnUserDeleted(u *model.User) {
	f.deleted = append(f.deleted, u.ID)
}

// #2230: noteIds/drive purge の cascade job enqueue と AP Delete(actor) 配信が
// 走ることを検証する (論理削除フラグだけでなく実削除が triggered される)。
func TestDeleteAccount_EnqueuesCascadeAndFederates(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	enq := &fakeDeleteEnqueuer{}
	fed := &fakeAccountDeletionFed{}
	h.SetDeleteAccountEnqueuer(enq)
	h.SetAccountDeletionFederationHook(fed)

	rec := postExtra(h.DeleteAccount, `{"password":"pass"}`, user)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, repo.Users["u1"].IsDeleted)
	assert.Equal(t, []string{"u1"}, enq.called, "cascade delete job must be enqueued")
	assert.Equal(t, []string{"u1"}, fed.deleted, "AP Delete(actor) must be delivered")
	// 自己削除は local user なので user 行を物理削除する hard delete (Soft=false)。
	require.Len(t, enq.payloads, 1)
	assert.False(t, enq.payloads[0].Soft, "self-delete must hard-delete the user row")
}

// #2230: enqueue が失敗しても論理削除フラグは立ったままなので 204 を返す
// (フラグが source of truth、cleanup は次回 retry に委ねる)。
func TestDeleteAccount_EnqueueErrorStillReturns204(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	h.SetDeleteAccountEnqueuer(&fakeDeleteEnqueuer{err: errors.New("boom")})

	rec := postExtra(h.DeleteAccount, `{"password":"pass"}`, user)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.True(t, repo.Users["u1"].IsDeleted)
}

// #2230: root アカウントの自己削除は cascade 前に 403 で拒否し、削除も enqueue もしない。
func TestDeleteAccount_ProtectedRootRejected(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	user.IsRoot = true
	enq := &fakeDeleteEnqueuer{}
	h.SetDeleteAccountEnqueuer(enq)

	rec := postExtra(h.DeleteAccount, `{"password":"pass"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "ACCESS_DENIED")
	assert.False(t, repo.Users["u1"].IsDeleted, "protected account must not be marked deleted")
	assert.Empty(t, enq.called, "protected account must not enqueue cascade")
}

// #2230: ローカル system account (host=nil + username に '.') も自己削除を拒否する。
func TestDeleteAccount_ProtectedSystemRejected(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "pass")
	user.Username = "relay.actor"
	user.Host = nil

	rec := postExtra(h.DeleteAccount, `{"password":"pass"}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.False(t, repo.Users["u1"].IsDeleted)
}

// change-password でも枠を取れなければ 503 を返す (#2849)。**400 に潰さない** —
// 現パスワードは正しいかもしれない。
func TestChangePassword_VerifierBusyReturns503(t *testing.T) {
	h, repo := newExtraHandler(t)
	user := setupUserWithArgon2Password(repo, "u1", "oldpass")

	rec := postExtraCanceled(h.ChangePassword, `{"currentPassword":"oldpass","newPassword":"newpass"}`, user)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Equal(t, "360", rec.Header().Get("Retry-After"))
	// **kind は server。** 飽和はサーバー側の事情なので client に倒すと意味が逆。
	var body struct {
		Error struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, apierr.UUIDPasswordVerificationUnavailable, body.Error.ID)
	assert.Equal(t, apierr.KindServer, body.Error.Kind)
	// **hash は書き換わっていないこと。**
	assert.True(t, strings.HasPrefix(*repo.Profiles["u1"].Password, "$argon2id$"))
}

// 未対応 profile では 400 のまま **hash を書き換えない** (#2849)。
//
// signin 側には同等のテストがあるのに `internal/api/i` だけ欠けており、
// `!outcome.OK()` を `== OutcomeMismatch` に変える変異が素通りしていた。
// その変異下では、壊れた hash を持つ利用者が**任意の現パスワードで
// パスワードを変更できる**。
func TestChangePassword_UnsupportedProfileStays400(t *testing.T) {
	const stored = "$argon2id$v=19$m=4096,t=3,p=1$YWJjZGVmZ2hpamtsbW5vcA$YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXoxMjM0NTY"
	h, repo := newExtraHandler(t)
	user := setupUserWithPassword(repo, "u1", "irrelevant")
	p := stored
	repo.Profiles["u1"].Password = &p

	buf := captureExtraLogs(t)
	rec := postExtra(h.ChangePassword, `{"currentPassword":"anything","newPassword":"newpass"}`, user)

	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Equal(t, stored, *repo.Profiles["u1"].Password, "hash が書き換わっている")

	out := buf.String()
	require.Contains(t, out, "unsupported password hash")
	assert.Contains(t, out, "m=4096,t=3,p=1")
	parts := strings.Split(stored, "$")
	assert.NotContains(t, out, parts[4], "salt がログに出ている")
	assert.NotContains(t, out, parts[5], "digest がログに出ている")
}

func captureExtraLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// 自己削除の root 保護は `meta.rootUserId` を見ること。
//
// **`user.isRoot` だけでは足りない。** あれは upstream が system_account 移行で
// DROP 済みの残存列で、mk-go は drop-in のために持っているだけ。列を追加した
// migration より前に作られた root は `isRoot = false` のままなので、meta を
// 見ないとガードが発火しない (本番の root がまさにその状態だった)。
//
// 通ると `user` 行が物理削除され、`meta` に FK が無い構成では `rootUserId` が
// 消えた ID を指したまま残るので **root が永久に不在**になる。`update-meta` は
// `rootUserId` を落とすので API 経由で指名し直す手段が無い。
func TestDeleteAccount_RootProtectionUsesMeta(t *testing.T) {
	setup := func(t *testing.T, rootID *string) (*Handler, *testutil.MockUserRepository, *testutil.MockMetaRepository) {
		t.Helper()
		h, repo := newExtraHandler(t)
		metaRepo := testutil.NewMockMetaRepository()
		metaRepo.Meta = &model.Meta{ID: "x", RootUserID: rootID}
		h.SetMetaRepo(metaRepo)
		return h, repo, metaRepo
	}

	t.Run("isRoot が false でも meta が指していれば拒否", func(t *testing.T) {
		rootID := "u1"
		h, repo, _ := setup(t, &rootID)
		user := setupUserWithPassword(repo, "u1", "pass")
		user.IsRoot = false // 列が追加される前に作られた root

		rec := postExtra(h.DeleteAccount, `{"password":"pass"}`, user)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "ACCESS_DENIED")
		assert.False(t, repo.Users["u1"].IsDeleted, "削除が進んでいる")
	})

	// **弾きすぎていないことを見る。** 常に拒否する実装でも上は緑になる。
	t.Run("root でない利用者は削除できる", func(t *testing.T) {
		rootID := "other"
		h, repo, _ := setup(t, &rootID)
		user := setupUserWithPassword(repo, "u1", "pass")

		rec := postExtra(h.DeleteAccount, `{"password":"pass"}`, user)
		assert.Equal(t, http.StatusNoContent, rec.Code)
		assert.True(t, repo.Users["u1"].IsDeleted)
	})

	// **meta が読めなければ 500 (#3037)。**
	//
	// 削除は通さない (DB 障害を「root ではない」と解釈して不可逆な操作を
	// 通すわけにいかない) が、**`ACCESS_DENIED` で返すのも事実ではない** —
	// 「root だから断った」のではなく「判定できなかった」。4xx に丸めると
	// 本人の退会が自分のせいに見えるうえ、監視にも 4xx しか出ない (#2792)。
	t.Run("meta が読めなければ 500", func(t *testing.T) {
		h, repo, metaRepo := setup(t, nil)
		metaRepo.Meta = nil // Fetch が error を返す
		user := setupUserWithPassword(repo, "u1", "pass")

		rec := postExtra(h.DeleteAccount, `{"password":"pass"}`, user)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		assert.False(t, repo.Users["u1"].IsDeleted, "判定できないまま削除している")
	})

	// 従来の `isRoot` 判定も残っていること (drop-in で TS から引き継いだ列)。
	t.Run("isRoot が true なら meta が指していなくても拒否", func(t *testing.T) {
		rootID := "other"
		h, repo, _ := setup(t, &rootID)
		user := setupUserWithPassword(repo, "u1", "pass")
		user.IsRoot = true

		rec := postExtra(h.DeleteAccount, `{"password":"pass"}`, user)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
}
