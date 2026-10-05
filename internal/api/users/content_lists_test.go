package users

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// stubGalleryRepo は users 向けに ListByUser のみ返す最小スタブ。
// Misskey の GalleryRepository は ListByUser / ListLikesByUser の 2 メソッド
// しか持たず、MockGalleryRepository は別パッケージに定義されていないため
// 直接 interface を満たす定義を置く。
type stubGalleryRepo struct {
	posts []*model.GalleryPost
}

func (s *stubGalleryRepo) ListByUser(_, _, _ string, _, _ int) ([]*model.GalleryPost, error) {
	return s.posts, nil
}
func (s *stubGalleryRepo) ListLikesByUser(_, _, _ string, _, _ int) ([]*model.GalleryLike, error) {
	return nil, nil
}
func (s *stubGalleryRepo) FindPostsByIDs(_ []string) ([]*model.GalleryPost, error) {
	return nil, nil
}
func (s *stubGalleryRepo) ExistsLike(_, _ string) (bool, error) {
	return false, nil
}

// --- Clips ---

func TestClips(t *testing.T) {
	h, _ := newTestHandler(t)
	// userId 欠落は 400
	rec := postStub(h.Clips, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	// repo 未注入でも 200 空配列
	assert.Equal(t, http.StatusOK, postStub(h.Clips, `{"userId":"u1"}`, nil).Code)
}

func TestClips_FiltersNonPublicForStranger(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockClipRepository()
	require.NoError(t, repo.Create(&model.Clip{ID: "c1", UserID: "owner", Name: "pub", IsPublic: true}))
	require.NoError(t, repo.Create(&model.Clip{ID: "c2", UserID: "owner", Name: "priv", IsPublic: false}))
	h.SetClipRepo(repo)

	rec := postStub(h.Clips, `{"userId":"owner"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "c1", rows[0]["id"])
}

// 非所有者向けのクエリが SQL 側で public 絞り込みされるため、
// private が多くても LIMIT が有意味 (旧 post-fetch filter だと空結果になる)。
func TestClips_PublicFilterPushedDown(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockClipRepository()
	// private 5 件 + public 2 件。limit=2 で non-owner は public 2 件を得たい。
	for i := 0; i < 5; i++ {
		require.NoError(t, repo.Create(&model.Clip{
			ID: "priv-" + string(rune('a'+i)), UserID: "owner", IsPublic: false,
		}))
	}
	require.NoError(t, repo.Create(&model.Clip{ID: "pub-1", UserID: "owner", IsPublic: true}))
	require.NoError(t, repo.Create(&model.Clip{ID: "pub-2", UserID: "owner", IsPublic: true}))
	h.SetClipRepo(repo)

	rec := postStub(h.Clips, `{"userId":"owner","limit":2}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Len(t, rows, 2, "LIMIT must apply to already-public-filtered set")
}

// #1784: upstream clips.ts は owner でも公開 clip のみ返す (clip.isPublic=true)。
// 非公開 clip は owner であっても users/clips には現れない。
func TestClips_OwnerSeesOnlyPublic(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockClipRepository()
	require.NoError(t, repo.Create(&model.Clip{ID: "c1", UserID: "owner", IsPublic: true}))
	require.NoError(t, repo.Create(&model.Clip{ID: "c2", UserID: "owner", IsPublic: false}))
	h.SetClipRepo(repo)

	rec := postStub(h.Clips, `{"userId":"owner"}`, &model.User{ID: "owner"})
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "c1", rows[0]["id"])
}

// misskey_dart の Clip.fromJson が非null必須とする createdAt / user /
// favoritedCount が users/clips のレスポンスに含まれること (#1237)。
func TestClips_FullShape(t *testing.T) {
	h, userRepo := newTestHandler(t)
	h.SetUserRepo(userRepo)
	userRepo.Users["owner"] = &model.User{
		ID: "owner", Username: "owner", UsernameLower: "owner",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	idGen, _ := id.NewGenerator("aidx")
	clipID := idGen.Generate(time.Now())
	repo := testutil.NewMockClipRepository()
	require.NoError(t, repo.Create(&model.Clip{ID: clipID, UserID: "owner", Name: "favs", IsPublic: true}))
	h.SetClipRepo(repo)

	rec := postStub(h.Clips, `{"userId":"owner"}`, &model.User{ID: "owner"})
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	cl := rows[0]
	createdAt, ok := cl["createdAt"].(string)
	assert.True(t, ok, "createdAt must be a non-null string")
	assert.NotEmpty(t, createdAt)
	assert.Equal(t, float64(0), cl["favoritedCount"])
	user, ok := cl["user"].(map[string]any)
	require.True(t, ok, "user must be a non-null object")
	assert.Equal(t, "owner", user["id"])
}

// --- Flashs ---

func TestFlashs(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := postStub(h.Flashs, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, http.StatusOK, postStub(h.Flashs, `{"userId":"u1"}`, nil).Code)
}

func TestFlashs_HidesNonPublic(t *testing.T) {
	h, _ := newTestHandler(t)
	repo := testutil.NewMockFlashRepository()
	require.NoError(t, repo.Create(&model.Flash{ID: "f1", UserID: "owner", Visibility: "public"}))
	require.NoError(t, repo.Create(&model.Flash{ID: "f2", UserID: "owner", Visibility: "private"}))
	h.SetFlashRepo(repo)
	rec := postStub(h.Flashs, `{"userId":"owner"}`, nil)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "f1", rows[0]["id"])
	// #1766: upstream Flash schema に permissions は無い (create paramDef のみ)。
	_, hasPerm := rows[0]["permissions"]
	assert.False(t, hasPerm, "permissions は packed Flash に含めない")
}

// --- GalleryPosts ---

func TestGalleryPosts(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := postStub(h.GalleryPosts, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, http.StatusOK, postStub(h.GalleryPosts, `{"userId":"u1"}`, nil).Code)
}

func TestGalleryPosts_ReturnsAll(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetGalleryRepo(&stubGalleryRepo{posts: []*model.GalleryPost{
		{ID: "g1", UserID: "owner", Title: "t1"},
		{ID: "g2", UserID: "owner", Title: "t2"},
	}})
	rec := postStub(h.GalleryPosts, `{"userId":"owner"}`, nil)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Len(t, rows, 2)
	// #1766: 空 tags は出力しない (upstream は length>0 のみ)。
	_, hasTags := rows[0]["tags"]
	assert.False(t, hasTags, "空 tags は省略される")
}

// #1555 users/gallery/posts も fileIds から DriveFile を解決する
// (i/gallery/posts と同じ)。未認証 viewer なので isLiked は省略される。
func TestGalleryPosts_ResolvesFiles(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetGalleryRepo(&stubGalleryRepo{posts: []*model.GalleryPost{
		{ID: "g1", UserID: "owner", Title: "t1", FileIDs: []string{"f1"}},
	}})
	driveRepo := testutil.NewMockDriveFileRepository()
	owner := "owner"
	driveRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &owner, Name: "a.png", Type: "image/png"}
	h.SetDriveFileRepo(driveRepo)

	rec := postStub(h.GalleryPosts, `{"userId":"owner"}`, nil)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	files, ok := rows[0]["files"].([]any)
	require.True(t, ok)
	require.Len(t, files, 1)
	assert.Equal(t, "f1", files[0].(map[string]any)["id"])
	// 未認証 viewer なので isLiked は出ない。
	_, hasLiked := rows[0]["isLiked"]
	assert.False(t, hasLiked)
}

// --- Pages ---

func TestPages(t *testing.T) {
	h, userRepo := newTestHandler(t)
	// owner lookup を満たすためテストユーザーを登録 (#1134 で users/pages が
	// owner 必須 entity shape を返すため)。
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "u1", UsernameLower: "u1"}
	rec := postStub(h.Pages, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, http.StatusOK, postStub(h.Pages, `{"userId":"u1"}`, nil).Code)
}

func TestPages_HidesNonPublic(t *testing.T) {
	h, userRepo := newTestHandler(t)
	userRepo.Users["owner"] = &model.User{ID: "owner", Username: "owner", UsernameLower: "owner"}
	repo := testutil.NewMockPageRepository()
	require.NoError(t, repo.Create(&model.Page{ID: "p1", UserID: "owner", Visibility: model.PageVisibilityPublic}))
	require.NoError(t, repo.Create(&model.Page{ID: "p2", UserID: "owner", Visibility: model.PageVisibilityFollowers}))
	h.SetPageRepo(repo)
	rec := postStub(h.Pages, `{"userId":"owner"}`, nil)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "p1", rows[0]["id"])
	// #1134: frontend MkPagePreview が `page.user.username` を unconditional に
	// 参照するため、user (UserLite) が必ず含まれていることを guard する。
	user, ok := rows[0]["user"].(map[string]any)
	require.True(t, ok, "page entity must include user field")
	assert.Equal(t, "owner", user["username"])
}

// #1662 users/pages も page content の image block / eyeCatchingImageId から
// drive file を解決する (i/pages / featured と同じ packer 経路)。
func TestPages_ResolvesDriveFiles(t *testing.T) {
	h, userRepo := newTestHandler(t)
	userRepo.Users["owner"] = &model.User{ID: "owner", Username: "owner", UsernameLower: "owner"}
	repo := testutil.NewMockPageRepository()
	eyeID := "eye1"
	require.NoError(t, repo.Create(&model.Page{
		ID: "p1", UserID: "owner", Visibility: model.PageVisibilityPublic,
		Content: []byte(`[{"type":"image","fileId":"f1"}]`), EyeCatchingImageID: &eyeID,
	}))
	h.SetPageRepo(repo)
	driveRepo := testutil.NewMockDriveFileRepository()
	owner := "owner"
	driveRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &owner, Name: "a.png", Type: "image/png"}
	driveRepo.Files["eye1"] = &model.DriveFile{ID: "eye1", UserID: &owner, Name: "e.png", Type: "image/png"}
	h.SetDriveFileRepo(driveRepo)

	rec := postStub(h.Pages, `{"userId":"owner"}`, nil)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	attached, ok := rows[0]["attachedFiles"].([]any)
	require.True(t, ok)
	require.Len(t, attached, 1)
	assert.Equal(t, "f1", attached[0].(map[string]any)["id"])
	eye, ok := rows[0]["eyeCatchingImage"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "eye1", eye["id"])
}
