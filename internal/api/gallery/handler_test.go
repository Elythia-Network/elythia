package gallery_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/api/gallery"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

var testDB *gorm.DB
var testIDGen id.Generator

func init() {
	testDB = testutil.MustOpenTestDB()
	testIDGen, _ = id.NewGenerator("aidx")
	testutil.ApplyMigrations(testDB)
	testDB.Exec(`INSERT INTO "user" (id, username, "usernameLower", "avatarDecorations") VALUES ('gal_u1', 'galuser', 'galuser', '[]') ON CONFLICT DO NOTHING`)
	testDB.Exec(`INSERT INTO "user" (id, username, "usernameLower", "avatarDecorations") VALUES ('gal_u2', 'galuser2', 'galuser2', '[]') ON CONFLICT DO NOTHING`)
}

func newHandler() *gallery.Handler {
	return gallery.NewHandler(testDB, testIDGen)
}

func brokenHandler() *gallery.Handler {
	db := testutil.MustOpenTestDB()
	sqlDB, _ := db.DB()
	_ = sqlDB.Close()
	return gallery.NewHandler(db, testIDGen)
}

// stubRanking is an in-memory GalleryRanking for #1548 tests. It records
// UpdateGalleryPostsRanking calls and returns a fixed ID list from Get.
type stubRanking struct {
	ids     []string
	getErr  error
	updates []ratingUpdate
}

type ratingUpdate struct {
	postID string
	score  float64
}

func (s *stubRanking) UpdateGalleryPostsRanking(_ context.Context, postID string, score float64) error {
	s.updates = append(s.updates, ratingUpdate{postID, score})
	return nil
}

func (s *stubRanking) GetGalleryPostsRanking(_ context.Context, _ int) ([]string, error) {
	return s.ids, s.getErr
}

// stubRoles implements gallery.RoleChecker.
type stubRoles struct{ moderators map[string]bool }

func (s *stubRoles) IsModerator(userID string) bool { return s.moderators[userID] }

// stubModLog records moderation log calls for #1548 tests.
type stubModLog struct{ calls []modLogCall }

type modLogCall struct {
	moderatorID string
	logType     moderationlog.LogType
	info        map[string]any
}

func (s *stubModLog) Log(_ context.Context, moderatorID string, t moderationlog.LogType, info map[string]any) {
	s.calls = append(s.calls, modLogCall{moderatorID, t, info})
}

func newHandlerWithRanking(r gallery.GalleryRanking) *gallery.Handler {
	h := gallery.NewHandler(testDB, testIDGen)
	h.SetRanking(r)
	return h
}

func brokenHandlerWithRanking(r gallery.GalleryRanking) *gallery.Handler {
	db := testutil.MustOpenTestDB()
	sqlDB, _ := db.DB()
	_ = sqlDB.Close()
	h := gallery.NewHandler(db, testIDGen)
	h.SetRanking(r)
	return h
}

func cleanup() {
	testDB.Exec(`DELETE FROM "gallery_like"`)
	testDB.Exec(`DELETE FROM "gallery_post"`)
	testDB.Exec(`DELETE FROM "drive_file" WHERE id LIKE 'galf_%'`)
}

// seedPost inserts a gallery post and fails the test if the insert did not
// succeed.
//
// **戻り値を捨てないこと。** 以前はここが `testDB.Create(...)` の呼び捨てで、
// 所有者 user が消えていると FK 違反で挿入が失敗しても黙って進み、行が無いまま
// `PostsUpdate` が走って「200 のはずが 400」という**原因から遠い症状**に化けて
// いた。実際 #2450 の調査はここで時間を取られた。
func seedPost(t *testing.T, p *model.GalleryPost) {
	t.Helper()
	require.NoError(t, testDB.Create(p).Error)
}

// seedDriveFile inserts a minimal drive file owned by ownerID for gallery tests.
func seedDriveFile(id, ownerID string) {
	uid := ownerID
	testDB.Create(&model.DriveFile{
		ID: id, UserID: &uid, MD5: "x", Name: id + ".png", Type: "image/png",
		Size: 1, StoredInternal: true, URL: "https://example/" + id + ".png",
	})
}

func doPost(h func(echo.Context) error, body string, user *model.User) *httptest.ResponseRecorder {
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

// --- Featured / Popular / Posts ---

// nil ranking (= 未配線) は空配列に degrade する (#1548)。
func TestFeatured_NilRankingEmpty(t *testing.T) {
	cleanup()
	rec := doPost(newHandler().Featured, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

// ranking が空 ID なら空配列 (#1548)。
func TestFeatured_EmptyRanking(t *testing.T) {
	cleanup()
	rec := doPost(newHandlerWithRanking(&stubRanking{ids: nil}).Featured, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

// ranking の ID で post を引いて返す (#1548)。
func TestFeatured_WithRanking(t *testing.T) {
	cleanup()
	seedPost(t, &model.GalleryPost{ID: "gp_f1", UpdatedAt: time.Now(), Title: "Art", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}})
	defer cleanup()
	rec := doPost(newHandlerWithRanking(&stubRanking{ids: []string{"gp_f1"}}).Featured, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Art")
}

// untilId フィルタで ID 範囲を絞る (#1548)。
func TestFeatured_UntilIDFilter(t *testing.T) {
	cleanup()
	seedPost(t, &model.GalleryPost{ID: "gp_aaa", UpdatedAt: time.Now(), Title: "Low", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}})
	seedPost(t, &model.GalleryPost{ID: "gp_zzz", UpdatedAt: time.Now(), Title: "High", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}})
	defer cleanup()
	rec := doPost(newHandlerWithRanking(&stubRanking{ids: []string{"gp_aaa", "gp_zzz"}}).Featured, `{"untilId":"gp_bbb"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Low")
	assert.NotContains(t, rec.Body.String(), "High")
}

// popular は likedCount>0 の post のみを likedCount DESC で返す (#1548)。
func TestPopular_FiltersZeroLikes(t *testing.T) {
	cleanup()
	seedPost(t, &model.GalleryPost{ID: "gp_p0", UpdatedAt: time.Now(), Title: "Zero", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}, LikedCount: 0})
	seedPost(t, &model.GalleryPost{ID: "gp_p1", UpdatedAt: time.Now(), Title: "Liked", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}, LikedCount: 3})
	defer cleanup()
	rec := doPost(newHandler().Popular, `{}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Liked")
	assert.NotContains(t, rec.Body.String(), "Zero")
}

func TestPosts_Success(t *testing.T) {
	cleanup()
	assert.Equal(t, http.StatusOK, doPost(newHandler().Posts, `{}`, nil).Code)
}

// list (Posts) が複数 post の files を batch 解決し (順序保持)、各 post の
// isLiked を viewer ごとに正しく埋める (liked=true / not-liked=false)。
func TestPosts_BatchFilesAndPerPostIsLiked(t *testing.T) {
	cleanup()
	defer cleanup()
	seedDriveFile("galf_a1", "gal_u1")
	seedDriveFile("galf_a2", "gal_u1")
	seedDriveFile("galf_b1", "gal_u1")
	// postA: files [a1,a2]、liked。postB: [b1]、not-liked。
	seedPost(t, &model.GalleryPost{ID: "gp_a", UpdatedAt: time.Now(), Title: "A", UserID: "gal_u1", FileIDs: []string{"galf_a1", "galf_a2"}, Tags: []string{}})
	seedPost(t, &model.GalleryPost{ID: "gp_b", UpdatedAt: time.Now(), Title: "B", UserID: "gal_u1", FileIDs: []string{"galf_b1"}, Tags: []string{}})
	testDB.Create(&model.GalleryLike{ID: "gl_a", UserID: "gal_u1", PostID: "gp_a"})

	rec := doPost(newHandler().Posts, `{}`, &model.User{ID: "gal_u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	byID := map[string]map[string]any{}
	for _, p := range resp {
		byID[p["id"].(string)] = p
	}
	a, b := byID["gp_a"], byID["gp_b"]
	require.NotNil(t, a)
	require.NotNil(t, b)
	// 各 post は自分の files のみを fileIds 順で持つ。
	assert.Equal(t, []any{"galf_a1", "galf_a2"}, a["fileIds"])
	assert.Len(t, a["files"].([]any), 2)
	assert.Len(t, b["files"].([]any), 1)
	// isLiked は post ごとに正しい (A=true, B=false)。
	assert.Equal(t, true, a["isLiked"])
	assert.Equal(t, false, b["isLiked"])
}

// #1773: tags は upstream 同様 length>0 のときのみ出力し、空のときは field 自体を
// omit する (空配列 [] を出さない)。fileIds は常に present のまま。
func TestPosts_TagsOmittedWhenEmpty(t *testing.T) {
	cleanup()
	defer cleanup()
	seedPost(t, &model.GalleryPost{ID: "gp_tagged", UpdatedAt: time.Now(), Title: "Tagged", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{"art", "wip"}})
	seedPost(t, &model.GalleryPost{ID: "gp_notags", UpdatedAt: time.Now(), Title: "NoTags", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}})

	rec := doPost(newHandler().Posts, `{}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	byID := map[string]map[string]any{}
	for _, p := range resp {
		byID[p["id"].(string)] = p
	}
	tagged, notags := byID["gp_tagged"], byID["gp_notags"]
	require.NotNil(t, tagged)
	require.NotNil(t, notags)
	assert.Equal(t, []any{"art", "wip"}, tagged["tags"])
	_, has := notags["tags"]
	assert.False(t, has, "empty tags must be omitted, not emitted as []")
	// fileIds は tags の有無に関わらず常に出力される (upstream も同様)。
	_, hasFileIDs := notags["fileIds"]
	assert.True(t, hasFileIDs, "fileIds must always be present")
}

func TestPostsUpdate_DBError(t *testing.T) {
	assert.Equal(t, http.StatusInternalServerError,
		doPost(brokenHandler().PostsUpdate, `{"postId":"x","fileIds":["galf_x"]}`, &model.User{ID: "gal_u1"}).Code)
}

// update で isSensitive 省略時は false で上書きされる (TS default:false)。
func TestPostsUpdate_IsSensitiveResetOnOmit(t *testing.T) {
	cleanup()
	defer cleanup()
	seedPost(t, &model.GalleryPost{ID: "gp_ir", UpdatedAt: time.Now(), Title: "Old", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}, IsSensitive: true})
	rec := doPost(newHandler().PostsUpdate, `{"postId":"gp_ir","title":"New"}`, &model.User{ID: "gal_u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["isSensitive"], "isSensitive 省略時は false で上書き (TS default)")
}

// create: title 空文字 / fileIds 重複 dedup / maxItems 超過。
func TestPostsCreate_TitleEmptyAndDedup(t *testing.T) {
	cleanup()
	defer cleanup()
	seedDriveFile("galf_d1", "gal_u1")
	// title 空は 400 (update 側 minLength)。create は title=="" を既存どおり弾く。
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().PostsCreate, `{"title":"","fileIds":["galf_d1"]}`, &model.User{ID: "gal_u1"}).Code)
	// 重複 fileId は dedup されて 1 件で保存される。
	rec := doPost(newHandler().PostsCreate, `{"title":"Dup","fileIds":["galf_d1","galf_d1"]}`, &model.User{ID: "gal_u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, []any{"galf_d1"}, resp["fileIds"], "重複は dedup")
}

// create: fileIds が 32 を超えると 400 (maxItems)。
func TestPostsCreate_TooManyFiles(t *testing.T) {
	ids := make([]string, 33)
	for i := range ids {
		ids[i] = `"f` + string(rune('a'+i%26)) + string(rune('0'+i/26)) + `"`
	}
	body := `{"title":"x","fileIds":[` + strings.Join(ids, ",") + `]}`
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().PostsCreate, body, &model.User{ID: "gal_u1"}).Code)
}

// 混在 (owned + foreign) は owned のみを入力順で残す。
func TestPostsCreate_MixedOwnedForeignOrder(t *testing.T) {
	cleanup()
	defer cleanup()
	seedDriveFile("galf_m1", "gal_u1")
	seedDriveFile("galf_m2", "gal_u1")
	seedDriveFile("galf_mforeign", "someone_else")
	rec := doPost(newHandler().PostsCreate, `{"title":"Mix","fileIds":["galf_m2","galf_mforeign","galf_m1"]}`, &model.User{ID: "gal_u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, []any{"galf_m2", "galf_m1"}, resp["fileIds"], "owned のみ入力順で残す")
}

func TestPosts_WithOffset(t *testing.T) {
	assert.Equal(t, http.StatusOK, doPost(newHandler().Posts, `{"limit":5,"offset":1}`, nil).Code)
}

func TestPosts_InvalidJSON(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().Posts, `invalid`, nil).Code)
}

func TestFeatured_DBError(t *testing.T) {
	// ranking が ID を返した後の id IN 取得で DB error → 500。
	h := brokenHandlerWithRanking(&stubRanking{ids: []string{"gp_x"}})
	assert.Equal(t, http.StatusInternalServerError, doPost(h.Featured, `{}`, nil).Code)
}

func TestPosts_DBError(t *testing.T) {
	assert.Equal(t, http.StatusInternalServerError, doPost(brokenHandler().Posts, `{}`, nil).Code)
}

// --- Create ---

func TestPostsCreate_Success(t *testing.T) {
	cleanup()
	defer cleanup()
	seedDriveFile("galf_c1", "gal_u1")
	user := &model.User{ID: "gal_u1", Username: "galuser"}
	rec := doPost(newHandler().PostsCreate, `{"title":"My Art","fileIds":["galf_c1"]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "My Art", resp["title"])
	// files が DriveFile に解決される (空配列でない)。
	files, ok := resp["files"].([]any)
	require.True(t, ok)
	assert.Len(t, files, 1)
	// 作成者視点なので isLiked=false が出る。
	assert.Equal(t, false, resp["isLiked"])
	shapetest.Assert(t, "GalleryPost", resp) // L3 (#1270)
}

// fileIds 必須: 省略時は 400。
func TestPostsCreate_FileIdsRequired(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest,
		doPost(newHandler().PostsCreate, `{"title":"x"}`, &model.User{ID: "gal_u1"}).Code)
}

// 他人所有の fileId のみだと有効ファイル 0 で 400。
func TestPostsCreate_ForeignFilesRejected(t *testing.T) {
	cleanup()
	defer cleanup()
	seedDriveFile("galf_other", "someone_else")
	rec := doPost(newHandler().PostsCreate, `{"title":"x","fileIds":["galf_other"]}`, &model.User{ID: "gal_u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPostsCreate_InvalidParam(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().PostsCreate, `{}`, &model.User{ID: "u1"}).Code)
}

func TestPostsCreate_DBError(t *testing.T) {
	// fileIds 検証 (ownedFileIDs) が壊れた DB で error → 500。
	assert.Equal(t, http.StatusInternalServerError, doPost(brokenHandler().PostsCreate, `{"title":"x","fileIds":["galf_x"]}`, &model.User{ID: "u1"}).Code)
}

// --- Show ---

func TestPostsShow_Found(t *testing.T) {
	cleanup()
	seedPost(t, &model.GalleryPost{ID: "gp_s1", UpdatedAt: time.Now(), Title: "Show", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}})
	defer cleanup()
	rec := doPost(newHandler().PostsShow, `{"postId":"gp_s1"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	shapetest.Assert(t, "GalleryPost", resp) // L3 (#1320)
}

// viewer が like 済みなら show の isLiked=true。未認証 viewer では isLiked 省略。
func TestPostsShow_IsLikedAndFiles(t *testing.T) {
	cleanup()
	defer cleanup()
	seedDriveFile("galf_s2", "gal_u1")
	seedPost(t, &model.GalleryPost{ID: "gp_s2", UpdatedAt: time.Now(), Title: "S", UserID: "gal_u1", FileIDs: []string{"galf_s2"}, Tags: []string{}})
	testDB.Create(&model.GalleryLike{ID: "gl_s2", UserID: "gal_u1", PostID: "gp_s2"})

	// 認証 viewer (like 済み): isLiked=true、files 解決。
	rec := doPost(newHandler().PostsShow, `{"postId":"gp_s2"}`, &model.User{ID: "gal_u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["isLiked"])
	files, _ := resp["files"].([]any)
	assert.Len(t, files, 1)

	// 未認証 viewer: isLiked は省略される (upstream optional)。
	rec = doPost(newHandler().PostsShow, `{"postId":"gp_s2"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var anon map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &anon))
	_, has := anon["isLiked"]
	assert.False(t, has, "未認証では isLiked を出さない")
}

func TestPostsShow_NotFound(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().PostsShow, `{"postId":"ghost"}`, nil).Code)
}

func TestPostsShow_InvalidParam(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().PostsShow, `{}`, nil).Code)
}

// --- Delete ---

func TestPostsDelete_Success(t *testing.T) {
	cleanup()
	seedPost(t, &model.GalleryPost{ID: "gp_d1", UpdatedAt: time.Now(), Title: "Del", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}})
	assert.Equal(t, http.StatusNoContent, doPost(newHandler().PostsDelete, `{"postId":"gp_d1"}`, &model.User{ID: "gal_u1"}).Code)
}

func TestPostsDelete_NotFound(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().PostsDelete, `{"postId":"ghost"}`, &model.User{ID: "u1"}).Code)
}

func TestPostsDelete_InvalidParam(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().PostsDelete, `{}`, &model.User{ID: "u1"}).Code)
}

// 非所有者かつ非モデレータの削除は ACCESS_DENIED (#1548)。
func TestPostsDelete_AccessDenied(t *testing.T) {
	cleanup()
	seedPost(t, &model.GalleryPost{ID: "gp_ad", UpdatedAt: time.Now(), Title: "Del", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}})
	defer cleanup()
	h := newHandler()
	h.SetRoleChecker(&stubRoles{moderators: map[string]bool{}})
	rec := doPost(h.PostsDelete, `{"postId":"gp_ad"}`, &model.User{ID: "gal_u2"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "c86e09de-1c48-43ac-a435-1c7e42ed4496")
	// post は削除されていない。
	var cnt int64
	testDB.Model(&model.GalleryPost{}).Where("id = ?", "gp_ad").Count(&cnt)
	assert.Equal(t, int64(1), cnt)
}

// モデレータは他人の post を削除でき、moderationLog を残す (#1548)。
func TestPostsDelete_ModeratorWithModLog(t *testing.T) {
	cleanup()
	seedPost(t, &model.GalleryPost{ID: "gp_mod", UpdatedAt: time.Now(), Title: "Del", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}})
	defer cleanup()
	h := newHandler()
	h.SetRoleChecker(&stubRoles{moderators: map[string]bool{"gal_u2": true}})
	ml := &stubModLog{}
	h.SetModLog(ml)
	rec := doPost(h.PostsDelete, `{"postId":"gp_mod"}`, &model.User{ID: "gal_u2"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	var cnt int64
	testDB.Model(&model.GalleryPost{}).Where("id = ?", "gp_mod").Count(&cnt)
	assert.Equal(t, int64(0), cnt)
	require.Len(t, ml.calls, 1)
	assert.Equal(t, moderationlog.LogDeleteGalleryPost, ml.calls[0].logType)
	assert.Equal(t, "gal_u2", ml.calls[0].moderatorID)
	assert.Equal(t, "gp_mod", ml.calls[0].info["postId"])
	assert.Equal(t, "gal_u1", ml.calls[0].info["postUserId"])
	assert.Equal(t, "galuser", ml.calls[0].info["postUserUsername"])
}

// 所有者自身の削除では moderationLog を残さない (#1548)。
func TestPostsDelete_OwnerNoModLog(t *testing.T) {
	cleanup()
	seedPost(t, &model.GalleryPost{ID: "gp_own", UpdatedAt: time.Now(), Title: "Del", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}})
	defer cleanup()
	h := newHandler()
	h.SetRoleChecker(&stubRoles{moderators: map[string]bool{"gal_u1": true}})
	ml := &stubModLog{}
	h.SetModLog(ml)
	rec := doPost(h.PostsDelete, `{"postId":"gp_own"}`, &model.User{ID: "gal_u1"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, ml.calls, "owner 自身の削除では監査ログを残さない")
}

// --- Update ---

func TestPostsUpdate_Success(t *testing.T) {
	cleanup()
	seedPost(t, &model.GalleryPost{ID: "gp_u1", UpdatedAt: time.Now(), Title: "Old", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}})
	defer cleanup()
	// upstream は更新後の GalleryPost を返す (204 ではない)。
	rec := doPost(newHandler().PostsUpdate, `{"postId":"gp_u1","title":"New"}`, &model.User{ID: "gal_u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "New", resp["title"])
}

func TestPostsUpdate_WithDescription(t *testing.T) {
	cleanup()
	seedPost(t, &model.GalleryPost{ID: "gp_ud1", UpdatedAt: time.Now(), Title: "Old", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}})
	defer cleanup()
	assert.Equal(t, http.StatusOK, doPost(newHandler().PostsUpdate, `{"postId":"gp_ud1","title":"New","description":"desc"}`, &model.User{ID: "gal_u1"}).Code)
}

// update で他人所有 fileId のみ指定すると有効ファイル 0 で 400 (更新されない)。
func TestPostsUpdate_ForeignFilesRejected(t *testing.T) {
	cleanup()
	defer cleanup()
	seedDriveFile("galf_foreign", "someone_else")
	seedPost(t, &model.GalleryPost{ID: "gp_uf", UpdatedAt: time.Now(), Title: "Old", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}})
	rec := doPost(newHandler().PostsUpdate, `{"postId":"gp_uf","fileIds":["galf_foreign"]}`, &model.User{ID: "gal_u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// update が fileIds / isSensitive を反映し、files を解決して返す。
func TestPostsUpdate_FileIdsAndSensitive(t *testing.T) {
	cleanup()
	defer cleanup()
	seedDriveFile("galf_u2a", "gal_u1")
	seedPost(t, &model.GalleryPost{ID: "gp_u2", UpdatedAt: time.Now(), Title: "Old", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}})
	rec := doPost(newHandler().PostsUpdate, `{"postId":"gp_u2","fileIds":["galf_u2a"],"isSensitive":true}`, &model.User{ID: "gal_u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["isSensitive"])
	assert.Equal(t, []any{"galf_u2a"}, resp["fileIds"])
	files, _ := resp["files"].([]any)
	assert.Len(t, files, 1)
}

func TestPostsUpdate_NotFound(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().PostsUpdate, `{"postId":"ghost","title":"x"}`, &model.User{ID: "u1"}).Code)
}

func TestPostsUpdate_InvalidParam(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().PostsUpdate, `{}`, &model.User{ID: "u1"}).Code)
}

// --- Like / Unlike ---

func TestPostsLike_Success(t *testing.T) {
	cleanup()
	pid := testIDGen.Generate(time.Now())
	seedPost(t, &model.GalleryPost{ID: pid, UpdatedAt: time.Now(), Title: "Likeable", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}})
	defer cleanup()
	rank := &stubRanking{}
	// gal_u2 が gal_u1 の post を like する (他人の post)。
	assert.Equal(t, http.StatusNoContent, doPost(newHandlerWithRanking(rank).PostsLike, `{"postId":"`+pid+`"}`, &model.User{ID: "gal_u2"}).Code)
	// 作成直後の post なので ranking 更新が +1 で走る。
	require.Len(t, rank.updates, 1)
	assert.Equal(t, ratingUpdate{pid, 1}, rank.updates[0])
}

// 自分の post を like すると YOUR_POST (#1548)。
func TestPostsLike_YourPost(t *testing.T) {
	cleanup()
	pid := testIDGen.Generate(time.Now())
	seedPost(t, &model.GalleryPost{ID: pid, UpdatedAt: time.Now(), Title: "Mine", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}})
	defer cleanup()
	rec := doPost(newHandler().PostsLike, `{"postId":"`+pid+`"}`, &model.User{ID: "gal_u1"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "f78f1511-5ebc-4478-a888-1198d752da68")
}

// 不在 post の like は NO_SUCH_POST (#1548)。
func TestPostsLike_NoSuchPost(t *testing.T) {
	cleanup()
	rec := doPost(newHandler().PostsLike, `{"postId":"ghost"}`, &model.User{ID: "gal_u2"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "56c06af3-1287-442f-9701-c93f7c4a62ff")
}

func TestPostsLike_AlreadyLiked(t *testing.T) {
	cleanup()
	pid := testIDGen.Generate(time.Now())
	seedPost(t, &model.GalleryPost{ID: pid, UpdatedAt: time.Now(), Title: "Liked", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}})
	defer cleanup()
	h := newHandler()
	doPost(h.PostsLike, `{"postId":"`+pid+`"}`, &model.User{ID: "gal_u2"})
	// upstream alreadyLiked は httpStatusCode 未指定で 400 を返す (#1773)。
	rec := doPost(h.PostsLike, `{"postId":"`+pid+`"}`, &model.User{ID: "gal_u2"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "40e9ed56-a59c-473a-bf3f-f289c54fb5a7")
}

func TestPostsLike_InvalidParam(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().PostsLike, `{}`, &model.User{ID: "u1"}).Code)
}

func TestPostsUnlike_Success(t *testing.T) {
	cleanup()
	pid := testIDGen.Generate(time.Now())
	seedPost(t, &model.GalleryPost{ID: pid, UpdatedAt: time.Now(), Title: "Unlikeable", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}, LikedCount: 1})
	testDB.Create(&model.GalleryLike{ID: testIDGen.Generate(time.Now()), UserID: "gal_u2", PostID: pid})
	defer cleanup()
	rank := &stubRanking{}
	rec := doPost(newHandlerWithRanking(rank).PostsUnlike, `{"postId":"`+pid+`"}`, &model.User{ID: "gal_u2"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, rank.updates, 1)
	assert.Equal(t, ratingUpdate{pid, -1}, rank.updates[0])
}

// 不在 post の unlike は NO_SUCH_POST (#1548)。
func TestPostsUnlike_NoSuchPost(t *testing.T) {
	cleanup()
	rec := doPost(newHandler().PostsUnlike, `{"postId":"ghost"}`, &model.User{ID: "gal_u2"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "c32e6dd0-b555-4413-925e-b3757d19ed84")
}

// 未 like の post を unlike すると NOT_LIKED (#1548)。
func TestPostsUnlike_NotLiked(t *testing.T) {
	cleanup()
	pid := testIDGen.Generate(time.Now())
	seedPost(t, &model.GalleryPost{ID: pid, UpdatedAt: time.Now(), Title: "Unliked", UserID: "gal_u1", FileIDs: []string{}, Tags: []string{}})
	defer cleanup()
	rec := doPost(newHandler().PostsUnlike, `{"postId":"`+pid+`"}`, &model.User{ID: "gal_u2"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "e3e8e06e-be37-41f7-a5b4-87a8250288f0")
}

func TestPostsUnlike_InvalidParam(t *testing.T) {
	assert.Equal(t, http.StatusBadRequest, doPost(newHandler().PostsUnlike, `{}`, &model.User{ID: "u1"}).Code)
}
