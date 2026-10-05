package admin_test

import (
	"context"
	"encoding/json"
	"fmt"
	corerole "github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/misc/id"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// setupEmojiHandler returns a handler with EmojiRepo wired and optional
// seed rows. setupDriveFileHandler (handler_test.go) と同じ contract
// (#761)。
func setupEmojiHandler(t *testing.T, seed ...*model.Emoji) (*apiadmin.Handler, *testutil.MockEmojiRepository) {
	t.Helper()
	h, _, _, _ := newTestHandler(t)
	repo := testutil.NewMockEmojiRepository()
	for _, e := range seed {
		require.NoError(t, repo.Create(e))
	}
	h.SetEmojiRepo(repo)
	return h, repo
}

type failingListV2EmojiRepo struct {
	*testutil.MockEmojiRepository
}

func (f *failingListV2EmojiRepo) ListV2(_ model.EmojiV2Filter) ([]*model.Emoji, error) {
	return nil, assert.AnError
}

// stubEmojiImportEnqueuer records the last payload and optionally returns err.
type stubEmojiImportEnqueuer struct {
	lastUserID string
	lastFileID string
	err        error
}

func (s *stubEmojiImportEnqueuer) EnqueueImportCustomEmojis(p queue.ImportCustomEmojisPayload) error {
	if s.err != nil {
		return s.err
	}
	s.lastUserID = p.UserID
	s.lastFileID = p.FileID
	return nil
}

func (f *failingListV2EmojiRepo) CountV2(_ model.EmojiV2Filter) (int64, error) {
	return 0, assert.AnError
}

// --- Drive ------------------------------------------------------------------

func TestDriveFiles_FiltersByOrigin(t *testing.T) {
	localUser := "u1"
	remoteHost := "remote.example"
	h, _ := setupDriveFileHandler(t,
		&model.DriveFile{ID: "d1", UserID: &localUser, Type: "image/png"},
		&model.DriveFile{ID: "d2", UserHost: &remoteHost, Type: "image/jpeg"},
	)

	rec := doPost(h.DriveFiles, `{"origin":"remote","limit":10}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "d2", rows[0]["id"])
}

// #1545: origin 省略時は upstream paramDef default の 'local' (userHost IS NULL) を返す。
func TestDriveFiles_OriginDefaultsToLocal(t *testing.T) {
	localUser := "u1"
	remoteHost := "remote.example"
	h, _ := setupDriveFileHandler(t,
		&model.DriveFile{ID: "d1", UserID: &localUser, Type: "image/png"},
		&model.DriveFile{ID: "d2", UserHost: &remoteHost, Type: "image/jpeg"},
	)
	rec := doPost(h.DriveFiles, `{"limit":10}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "d1", rows[0]["id"])
}

func TestDriveFiles_FiltersByUserID(t *testing.T) {
	// admin/user/<id> のドライブタブは userId だけを送ってくる (#471)。
	// upstream は userId 指定時に origin / hostname を読まないので、それに
	// 合わせて他ユーザーの remote ファイルが混ざらないことを確認する。
	target := "u_target"
	other := "u_other"
	otherHost := "remote.example"
	h, _ := setupDriveFileHandler(t,
		&model.DriveFile{ID: "d_t1", UserID: &target, Type: "image/png"},
		&model.DriveFile{ID: "d_t2", UserID: &target, UserHost: &otherHost, Type: "image/jpeg"},
		&model.DriveFile{ID: "d_o1", UserID: &other, UserHost: &otherHost, Type: "image/png"},
	)

	rec := doPost(h.DriveFiles, `{"userId":"u_target","limit":10}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	gotIDs := make(map[string]bool, len(rows))
	for _, r := range rows {
		gotIDs[r["id"].(string)] = true
	}
	assert.True(t, gotIDs["d_t1"])
	assert.True(t, gotIDs["d_t2"])
	assert.False(t, gotIDs["d_o1"])
}

func TestDriveFiles_FiltersByTypePrefix(t *testing.T) {
	u := "u1"
	h, _ := setupDriveFileHandler(t,
		&model.DriveFile{ID: "d1", UserID: &u, Type: "image/png"},
		&model.DriveFile{ID: "d2", UserID: &u, Type: "video/mp4"},
	)

	// `image/*` が prefix filter の形 (#1772)。`/*` を落とすと production では
	// 完全一致になり 0 件になる。
	rec := doPost(h.DriveFiles, `{"type":"image/*","limit":10}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "d1", rows[0]["id"])
}

func TestDriveFiles_FiltersBySystemToken(t *testing.T) {
	// #686: userId="@system" 指定時は local な system 所有 (userId IS NULL
	// かつ userHost IS NULL) の drive file のみを返す。emoji copy / import
	// zip 経路で蓄積される system file を admin UI から可視化するための経路。
	u := "u1"
	remoteUser := "u_remote"
	host := "remote.example"
	h, _ := setupDriveFileHandler(t,
		&model.DriveFile{ID: "d_sys1", UserID: nil, Type: "image/png"},
		&model.DriveFile{ID: "d_sys2", UserID: nil, Type: "image/jpeg"},
		&model.DriveFile{ID: "d_user", UserID: &u, Type: "image/png"},
		&model.DriveFile{ID: "d_remote", UserID: &remoteUser, UserHost: &host, Type: "image/png"},
	)

	// const apiadmin.SystemUserIDToken を直接参照することで、token 値を後で変更しても
	// このテストが追従するようにする (文字列直書きを避ける)。
	body := fmt.Sprintf(`{"userId":%q,"limit":10}`, apiadmin.SystemUserIDToken)
	rec := doPost(h.DriveFiles, body, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	gotIDs := make(map[string]bool, len(rows))
	for _, r := range rows {
		gotIDs[r["id"].(string)] = true
	}
	assert.True(t, gotIDs["d_sys1"], "system file should be listed")
	assert.True(t, gotIDs["d_sys2"], "system file should be listed")
	assert.False(t, gotIDs["d_user"], "user-owned file must not appear")
	assert.False(t, gotIDs["d_remote"], "remote-owned file must not appear")
}

func TestDriveFiles_SystemTokenWithTypeFilter(t *testing.T) {
	// #686: @system token は type prefix filter と組み合わせ可能。MIME 種別で
	// system file を絞り込めることを保証する (image/* だけ見たい等)。
	h, _ := setupDriveFileHandler(t,
		&model.DriveFile{ID: "d_sys_img", UserID: nil, Type: "image/png"},
		&model.DriveFile{ID: "d_sys_zip", UserID: nil, Type: "application/zip"},
	)

	body := fmt.Sprintf(`{"userId":%q,"type":"image/*","limit":10}`, apiadmin.SystemUserIDToken)
	rec := doPost(h.DriveFiles, body, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "d_sys_img", rows[0]["id"])
}

func TestDriveShowFile_MissingBothFileIDAndURL(t *testing.T) {
	h, _ := setupDriveFileHandler(t)
	assert.Equal(t, http.StatusBadRequest,
		doPost(h.DriveShowFile, `{}`, adminUser).Code)
}

func TestDriveShowFile_ByFileID(t *testing.T) {
	u := "u1"
	h, _ := setupDriveFileHandler(t,
		&model.DriveFile{ID: "d1", UserID: &u, Name: "a.png", Type: "image/png"},
	)

	rec := doPost(h.DriveShowFile, `{"fileId":"d1"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestDriveShowFile_IncludesRequestIPAndHeaders guards #1148: frontend
// admin-file.root.vue の IP タブは `info.requestIp` / `info.requestHeaders`
// を参照するが、旧 mk-go は PackDriveFileWithRelations 直返しで両 field を
// 含めていなかった (= IP タブが空表示)。upstream admin/drive/show-file.ts
// と同 custom shape で出すよう修正、moderator viewer に対して上 2 field が
// 含まれることを確認。
func TestDriveShowFile_IncludesRequestIPAndHeaders(t *testing.T) {
	h, _, _, roleRepo, assignRepo := newTestHandlerWithAssign(t)
	// adminUser に moderator role を assign する。
	// **`canSearchIpHistory` も付ける。** IP は policy を持つ相手にだけ返す
	// (#3114 と同じ条件)。
	roleRepo.Roles["r_mod"] = &model.Role{ID: "r_mod", Name: "Mod", IsModerator: true,
		Policies: datatypes.JSON([]byte(`{"canSearchIpHistory":{"useDefault":false,"priority":1,"value":true}}`))}
	assignRepo.Assignments["admin1:r_mod"] = &model.RoleAssignment{
		ID: "ra_mod", UserID: "admin1", RoleID: "r_mod",
	}

	repo := testutil.NewMockDriveFileRepository()
	owner := "u_owner"
	ip := "203.0.113.42"
	headers := datatypes.JSON([]byte(`{"x-test":"hello","user-agent":"curl/8.0"}`))
	require.NoError(t, repo.Create(&model.DriveFile{
		ID:             "d_with_ip",
		UserID:         &owner,
		Name:           "secret.png",
		Type:           "image/png",
		RequestIP:      &ip,
		RequestHeaders: headers,
	}))
	h.SetDriveFileRepo(repo)

	rec := doPost(h.DriveShowFile, `{"fileId":"d_with_ip"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "203.0.113.42", resp["requestIp"], "moderator viewer は requestIp 取得可能")
	hdr, ok := resp["requestHeaders"].(map[string]any)
	require.True(t, ok, "requestHeaders は object として emit される")
	assert.Equal(t, "hello", hdr["x-test"])
	assert.Equal(t, "curl/8.0", hdr["user-agent"])
}

// TestDriveShowFile_HidesRequestHeadersFromModeratorOwner: file owner も
// moderator のときは requestHeaders を hide (upstream 仕様、モデレーター
// 同士の互いの個人情報保護)。requestIp は引き続き public。
func TestDriveShowFile_HidesRequestHeadersFromModeratorOwner(t *testing.T) {
	h, _, _, roleRepo, assignRepo := newTestHandlerWithAssign(t)
	// admin1 viewer + admin2 owner、両方 moderator role を持つ。
	// viewer 側には `canSearchIpHistory` も付ける (IP はそれが要る)。
	roleRepo.Roles["r_mod"] = &model.Role{ID: "r_mod", Name: "Mod", IsModerator: true,
		Policies: datatypes.JSON([]byte(`{"canSearchIpHistory":{"useDefault":false,"priority":1,"value":true}}`))}
	assignRepo.Assignments["admin1:r_mod"] = &model.RoleAssignment{
		ID: "ra1", UserID: "admin1", RoleID: "r_mod",
	}
	assignRepo.Assignments["admin2:r_mod"] = &model.RoleAssignment{
		ID: "ra2", UserID: "admin2", RoleID: "r_mod",
	}

	repo := testutil.NewMockDriveFileRepository()
	owner := "admin2"
	ip := "203.0.113.99"
	headers := datatypes.JSON([]byte(`{"secret":"shh"}`))
	require.NoError(t, repo.Create(&model.DriveFile{
		ID:             "d_mod_owner",
		UserID:         &owner,
		Name:           "mod-file.png",
		Type:           "image/png",
		RequestIP:      &ip,
		RequestHeaders: headers,
	}))
	h.SetDriveFileRepo(repo)

	rec := doPost(h.DriveShowFile, `{"fileId":"d_mod_owner"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "203.0.113.99", resp["requestIp"], "policy を持つ viewer には requestIp を出す")
	assert.Nil(t, resp["requestHeaders"], "owner が moderator のときは requestHeaders を null で hide")
}

// **ロールを引けない窓では隠す側に倒す (#3037 レビュー 3 周目)。**
//
// `IsModerator` は判定できないときに false を返すので、素で使うと
// `assignmentRepo.ListByUser` が一時的に失敗する窓で**他のモデレーターの
// `requestHeaders` が出る**。2 周目でここを fail-closed にしたが、テストが
// 無く `ownerIsModerator := true` を `false` に戻す変異が素通りしていた。
func TestDriveShowFile_HidesRequestHeadersWhenRoleLookupFails(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	roleRepo := testutil.NewMockRoleRepository()
	roleRepo.Roles["r_mod"] = &model.Role{ID: "r_mod", Name: "Mod", IsModerator: true}
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "ra1", UserID: "admin1", RoleID: "r_mod"}))
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	flaky := &failingAssignmentRepo{MockRoleAssignmentRepository: assignRepo}
	roleSvc := corerole.NewService(roleRepo, flaky, metaRepo, idGen)
	h := apiadmin.NewHandler(signup.NewService(userRepo, metaRepo, idGen), roleSvc, metaRepo, userRepo, idGen)

	repo := testutil.NewMockDriveFileRepository()
	owner := "admin2"
	ip := "203.0.113.99"
	require.NoError(t, repo.Create(&model.DriveFile{
		ID:             "d_flaky",
		UserID:         &owner,
		Name:           "mod-file.png",
		Type:           "image/png",
		RequestIP:      &ip,
		RequestHeaders: datatypes.JSON([]byte(`{"secret":"shh"}`)),
	}))
	h.SetDriveFileRepo(repo)

	rec := doPost(h.DriveShowFile, `{"fileId":"d_flaky"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, resp["requestHeaders"],
		"ロールを引けない窓で他のモデレーターの requestHeaders が出ている")
}

func TestDriveCleanup_InvokesDeleteOrphans(t *testing.T) {
	u := "u1"
	h, repo := setupDriveFileHandler(t,
		&model.DriveFile{ID: "orphan1"},
		&model.DriveFile{ID: "kept", UserID: &u},
	)

	rec := doPost(h.DriveCleanup, `{}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotContains(t, repo.Files, "orphan1", "orphan should be deleted")
	assert.Contains(t, repo.Files, "kept", "user-owned file should be kept")
}

// TestDriveCleanup_PreservesEmojiReferencedSystemFiles は #722 の handler 層
// regression guard。emoji が参照する system 所有 drive_file は cleanup で
// 巻き込まれないこと (= mock の EmojiReferencedURLs が ON だと該当 URL の
// file は保持される) を assert する。SQL 層の guard は repository test で
// 別途検証 (TestDriveFileRepository_DeleteOrphans_PreservesEmojiReferenced)。
func TestDriveCleanup_PreservesEmojiReferencedSystemFiles(t *testing.T) {
	emojiRefURL := "http://test/system_emoji.png"
	h, repo := setupDriveFileHandler(t,
		&model.DriveFile{ID: "pure_orph", UserID: nil, URL: "http://test/pure.bin"},
		&model.DriveFile{ID: "emoji_sys", UserID: nil, URL: emojiRefURL},
	)
	repo.EmojiReferencedURLs = map[string]bool{emojiRefURL: true}

	rec := doPost(h.DriveCleanup, `{}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotContains(t, repo.Files, "pure_orph", "pure orphan should be deleted")
	assert.Contains(t, repo.Files, "emoji_sys", "emoji-referenced system file must be preserved")
}

// cleanup: storage backend 配線時は orphan file の access/thumbnail/webpublic
// object を削除してから DB 行を消す。user 所有 / emoji 参照 file は保持する (#1724)。
func TestDriveCleanup_DeletesStorageObjects(t *testing.T) {
	u := "u1"
	ak1, tk1, ak2, aku := "ak1", "tk1", "ak2", "aku"
	emojiURL := "http://test/system_emoji.png"
	emojiKey := "akemoji"
	h, repo := setupDriveFileHandler(t,
		&model.DriveFile{ID: "orph1", UserID: nil, AccessKey: &ak1, ThumbnailAccessKey: &tk1},
		&model.DriveFile{ID: "orph2", UserID: nil, AccessKey: &ak2},
		&model.DriveFile{ID: "kept", UserID: &u, AccessKey: &aku},
		&model.DriveFile{ID: "emoji_sys", UserID: nil, URL: emojiURL, AccessKey: &emojiKey},
	)
	repo.EmojiReferencedURLs = map[string]bool{emojiURL: true}
	sd := &stubStorageDeleter{}
	h.SetStorageDeleter(sd)

	rec := doPost(h.DriveCleanup, `{}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotContains(t, repo.Files, "orph1")
	assert.NotContains(t, repo.Files, "orph2")
	assert.Contains(t, repo.Files, "kept", "user-owned file は保持される")
	assert.Contains(t, repo.Files, "emoji_sys", "emoji 参照 system file は保持される")
	// orphan の object key のみ削除 (kept / emoji 参照の key は対象外)。
	assert.ElementsMatch(t, []string{"ak1", "tk1", "ak2"}, sd.deleted)
}

// cleanup が driveCleanupBatchSize(100) を超える orphan を複数バッチで全削除する。
func TestDriveCleanup_MultiBatch(t *testing.T) {
	h, repo := setupDriveFileHandler(t)
	for i := 0; i < 150; i++ {
		ak := "ak" + strconv.Itoa(i)
		require.NoError(t, repo.Create(&model.DriveFile{
			ID: "o" + strconv.Itoa(i), UserID: nil, AccessKey: &ak,
		}))
	}
	sd := &stubStorageDeleter{}
	h.SetStorageDeleter(sd)

	rec := doPost(h.DriveCleanup, `{}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, repo.Files, "150 件すべて削除される (複数バッチ)")
	assert.Len(t, sd.deleted, 150, "全 access key が storage 削除される")
}

// storage Delete が失敗しても他 key の削除を続行し DB 行は消す (best-effort)。
func TestDriveCleanup_StorageDeleteFailureBestEffort(t *testing.T) {
	ak1, tk1, ak2 := "ak1", "tk1", "ak2"
	h, repo := setupDriveFileHandler(t,
		&model.DriveFile{ID: "o1", UserID: nil, AccessKey: &ak1, ThumbnailAccessKey: &tk1},
		&model.DriveFile{ID: "o2", UserID: nil, AccessKey: &ak2},
	)
	sd := &stubStorageDeleter{failKeys: map[string]bool{"ak1": true}}
	h.SetStorageDeleter(sd)

	rec := doPost(h.DriveCleanup, `{}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.ElementsMatch(t, []string{"ak1", "tk1", "ak2"}, sd.deleted)
	assert.Empty(t, repo.Files)
}

// ListOrphans (DB) 失敗は 500 (DB-only 経路の 500 と整合)。
func TestDriveCleanup_ListErrorReturns500(t *testing.T) {
	h, repo := setupDriveFileHandler(t)
	repo.ListOrphansErr = assertError{}
	h.SetStorageDeleter(&stubStorageDeleter{})
	rec := doPost(h.DriveCleanup, `{}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestDriveCleanRemoteFiles_InvokesDeleteRemoteCache(t *testing.T) {
	host := "remote.example"
	u := "u1"
	// 削除対象は「キャッシュ済みリモートファイル」= isLink=false かつ host あり。
	// isLink=true (link-only proxy) とローカルファイルは保持する (条件修正の回帰 guard)。
	h, repo := setupDriveFileHandler(t,
		&model.DriveFile{ID: "cached_remote", IsLink: false, UserHost: &host},
		&model.DriveFile{ID: "link_only", IsLink: true, UserHost: &host},
		&model.DriveFile{ID: "local1", UserID: &u},
	)

	rec := doPost(h.DriveCleanRemoteFiles, `{}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotContains(t, repo.Files, "cached_remote", "キャッシュ実体は削除される")
	assert.Contains(t, repo.Files, "link_only", "link-only proxy は保持される")
	assert.Contains(t, repo.Files, "local1", "ローカルファイルは保持される")
}

// --- Emoji ------------------------------------------------------------------

func TestEmojiListRemote_Pagination(t *testing.T) {
	host := "remote.example"
	h, _ := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "alpha", Host: &host},
		&model.Emoji{ID: "e2", Name: "beta", Host: &host},
		&model.Emoji{ID: "e3", Name: "gamma", Host: nil}, // local
	)

	rec := doPost(h.EmojiListRemote, `{"limit":10}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	// local の e3 は除外される
	assert.Len(t, rows, 2)
}

func TestEmojiListRemote_FilterByQuery(t *testing.T) {
	host := "remote.example"
	h, _ := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "cat_smile", Host: &host},
		&model.Emoji{ID: "e2", Name: "dog_run", Host: &host},
	)

	rec := doPost(h.EmojiListRemote, `{"query":"cat","limit":10}`, adminUser)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "e1", rows[0]["id"])
}

func TestEmojiCopy_DuplicateNameReturns400(t *testing.T) {
	// Misskey TS 互換 (#650 問題 1): copy は元 name のまま local 化するため、
	// 同名の local emoji が既存だと DUPLICATE_NAME を返す。
	remoteHost := "remote.example"
	// remote の "smile" を copy しようとするが、local にも同名 "smile" 存在。
	h, _ := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "smile", Host: &remoteHost, OriginalURL: "https://x"},
		&model.Emoji{ID: "e_local", Name: "smile", OriginalURL: "https://y"},
	)

	rec := doPost(h.EmojiCopy, `{"emojiId":"e1"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errField, ok := body["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "DUPLICATE_NAME", errField["code"])
}

// findEmojiByID walks the mock's Emojis map (which is keyed by name@host, not
// id) to fetch a row by id.
func findEmojiByID(t *testing.T, repo *testutil.MockEmojiRepository, id string) *model.Emoji {
	t.Helper()
	e, err := repo.FindByID(id)
	require.NoError(t, err)
	return e
}

func TestEmojiCopy_Success(t *testing.T) {
	// remote → local copy: copy 先が host=nil で同名 local emoji が無い
	// ので 200 を返す (Misskey TS 互換、#650 問題 1)。
	remoteHost := "remote.example"
	h, repo := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "unique", Host: &remoteHost, OriginalURL: "https://x"},
	)

	rec := doPost(h.EmojiCopy, `{"emojiId":"e1"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.NotEmpty(t, body["id"])
	// copied emoji は元 name "unique" で local 化されている
	copied := findEmojiByID(t, repo, body["id"].(string))
	assert.Equal(t, "unique", copied.Name)
	assert.Nil(t, copied.Host)
}

func TestEmojiCopy_NotFound(t *testing.T) {
	h, _ := setupEmojiHandler(t)
	assert.Equal(t, http.StatusBadRequest,
		doPost(h.EmojiCopy, `{"emojiId":"missing"}`, adminUser).Code)
}

// fakeEmojiImageFetcher captures FetchAndStore arguments and returns a
// scripted response so tests can verify EmojiCopy plumbs the fetcher
// correctly (#670).
type fakeEmojiImageFetcher struct {
	calls []struct {
		URL  string
		Name string
		User *model.User
	}
	returnDF  *model.DriveFile
	returnErr error

	// #2966 (承認時に system 所有へ複製する経路。#2999 で admin/emoji/add、
	// #3014 で admin/emoji/update も通る)
	copyCalls     []*model.DriveFile
	copyNames     []string
	copySensitive []bool
	copyDF        *model.DriveFile
	copyErr       error
	deletedIDs    []string
	deleteErr     error
}

func (f *fakeEmojiImageFetcher) FetchAndStore(_ context.Context, url string, user *model.User, name string) (*model.DriveFile, error) {
	f.calls = append(f.calls, struct {
		URL  string
		Name string
		User *model.User
	}{url, name, user})
	if f.returnErr != nil {
		return nil, f.returnErr
	}
	return f.returnDF, nil
}

func (f *fakeEmojiImageFetcher) CopyToSystemFile(_ context.Context, src *model.DriveFile, name string, sensitive bool) (*model.DriveFile, error) {
	f.copyCalls = append(f.copyCalls, src)
	f.copyNames = append(f.copyNames, name)
	f.copySensitive = append(f.copySensitive, sensitive)
	if f.copyErr != nil {
		return nil, f.copyErr
	}
	return f.copyDF, nil
}

func (f *fakeEmojiImageFetcher) DeleteSystemFile(_ context.Context, fileID string) error {
	f.deletedIDs = append(f.deletedIDs, fileID)
	return f.deleteErr
}

// TestEmojiCopy_StoresInDrive verifies that a wired fetcher is invoked and
// the returned drive URL replaces the source's OriginalURL/PublicURL on the
// new local emoji (#670). Without this rewrite the local copy stays bound to
// the remote server's URL and breaks when that server deletes the file.
func TestEmojiCopy_StoresInDrive(t *testing.T) {
	remoteHost := "remote.example"
	h, repo := setupEmojiHandler(t,
		&model.Emoji{
			ID: "src1", Name: "happy", Host: &remoteHost,
			OriginalURL: "https://remote.example/emoji/happy.png",
			PublicURL:   "https://remote.example/emoji/happy.png",
		},
	)

	webpub := "https://local.example/files/webpub.png"
	webpubType := "image/webp"
	fetcher := &fakeEmojiImageFetcher{
		returnDF: &model.DriveFile{
			ID:            "df1",
			URL:           "https://local.example/files/orig.png",
			WebpublicURL:  &webpub,
			WebpublicType: &webpubType,
			Type:          "image/png",
		},
	}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiCopy, `{"emojiId":"src1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	copied := findEmojiByID(t, repo, body["id"].(string))

	// fetcher was called exactly once with the source's OriginalURL, the
	// emoji name (used as drive file name), and a nil user (system-owned
	// drive file, matching Misskey TS uploadFromUrl({user: null}) — see
	// emoji.go EmojiCopy comment).
	require.Len(t, fetcher.calls, 1)
	assert.Equal(t, "https://remote.example/emoji/happy.png", fetcher.calls[0].URL)
	assert.Equal(t, "happy", fetcher.calls[0].Name)
	assert.Nil(t, fetcher.calls[0].User, "drive file must be system-owned (user nil)")

	// New emoji's URLs point at the drive file, not the remote source.
	assert.Equal(t, "https://local.example/files/orig.png", copied.OriginalURL)
	assert.Equal(t, "https://local.example/files/webpub.png", copied.PublicURL)
	require.NotNil(t, copied.Type)
	// Webpublic type is preferred when present (matches Misskey TS behaviour).
	assert.Equal(t, "image/webp", *copied.Type)
	// **成功したら消さない** (#2998)。後始末を `defer` へ動かすような変更で、
	// 取り込んだ画像が作成直後に消えて「絵文字はピッカーに出るのに画像は 404」に
	// なる。承認経路にも同じ形を足した
	// (`TestCreateFromRemoteApplicationKeepsFileOnSuccess`)。
	require.Empty(t, fetcher.deletedIDs, "成功した copy で drive ファイルを消さないこと")
}

// TestEmojiCopy_StoresInDrive_NoWebpublic falls back to df.URL / df.Type
// when the drive file has no webpublic variant (e.g. small images).
func TestEmojiCopy_StoresInDrive_NoWebpublic(t *testing.T) {
	remoteHost := "remote.example"
	h, repo := setupEmojiHandler(t,
		&model.Emoji{
			ID: "src1", Name: "tiny", Host: &remoteHost,
			OriginalURL: "https://remote.example/emoji/tiny.png",
		},
	)

	fetcher := &fakeEmojiImageFetcher{
		returnDF: &model.DriveFile{
			ID:   "df1",
			URL:  "https://local.example/files/tiny.png",
			Type: "image/png",
		},
	}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiCopy, `{"emojiId":"src1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	copied := findEmojiByID(t, repo, body["id"].(string))

	assert.Equal(t, "https://local.example/files/tiny.png", copied.OriginalURL)
	assert.Equal(t, "https://local.example/files/tiny.png", copied.PublicURL)
	require.NotNil(t, copied.Type)
	assert.Equal(t, "image/png", *copied.Type)
}

// TestEmojiCopy_FetcherErrorReturns500 verifies that a fetch failure aborts
// the copy (no row created) instead of silently falling back to the remote
// URL — falling back would re-introduce the #670 symptom.
func TestEmojiCopy_FetcherErrorReturns500(t *testing.T) {
	remoteHost := "remote.example"
	h, repo := setupEmojiHandler(t,
		&model.Emoji{
			ID: "src1", Name: "happy", Host: &remoteHost,
			OriginalURL: "https://remote.example/emoji/happy.png",
		},
	)

	fetcher := &fakeEmojiImageFetcher{returnErr: assert.AnError}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiCopy, `{"emojiId":"src1"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)

	// fetcher が失敗したら row は作られない (#670 fallback regression 防止)。
	// 元 src が 1 件だけ残っており、host=nil の copy が増えていないことを
	// repo の内部 map から直接確認する。
	assert.Len(t, repo.Emojis, 1, "no new emoji row should have been created on fetch failure")
}

// TestEmojiCopy_FetcherUnsetUsesLegacyURL keeps the pre-#670 behaviour for
// environments that have not wired a fetcher (test handlers, deployments
// before the upgrade, etc). Without this fallback every test handler would
// have to wire a drive stub.
func TestEmojiCopy_FetcherUnsetUsesLegacyURL(t *testing.T) {
	remoteHost := "remote.example"
	h, repo := setupEmojiHandler(t,
		&model.Emoji{
			ID: "src1", Name: "happy", Host: &remoteHost,
			OriginalURL: "https://remote.example/emoji/happy.png",
			PublicURL:   "https://remote.example/emoji/happy.png",
		},
	)

	rec := doPost(h.EmojiCopy, `{"emojiId":"src1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	copied := findEmojiByID(t, repo, body["id"].(string))
	assert.Equal(t, "https://remote.example/emoji/happy.png", copied.OriginalURL)
}

func TestEmojiSetCategoryBulk_BatchUpdate(t *testing.T) {
	h, repo := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "alpha"},
		&model.Emoji{ID: "e2", Name: "beta"},
	)

	rec := doPost(h.EmojiSetCategoryBulk,
		`{"ids":["e1","e2"],"category":"animals"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, findEmojiByID(t, repo, "e1").Category)
	assert.Equal(t, "animals", *findEmojiByID(t, repo, "e1").Category)
	require.NotNil(t, findEmojiByID(t, repo, "e2").Category)
	assert.Equal(t, "animals", *findEmojiByID(t, repo, "e2").Category)
}

func TestEmojiSetLicenseBulk_BatchUpdate(t *testing.T) {
	h, repo := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "alpha"},
	)

	rec := doPost(h.EmojiSetLicenseBulk, `{"ids":["e1"],"license":"CC-BY-4.0"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, findEmojiByID(t, repo, "e1").License)
	assert.Equal(t, "CC-BY-4.0", *findEmojiByID(t, repo, "e1").License)
}

func TestEmojiSetAliasesBulk_BatchUpdate(t *testing.T) {
	h, repo := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "alpha"},
		&model.Emoji{ID: "e2", Name: "beta"},
	)

	rec := doPost(h.EmojiSetAliasesBulk,
		`{"ids":["e1","e2"],"aliases":["x","y"]}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string(findEmojiByID(t, repo, "e1").Aliases), []string{"x", "y"})
	assert.Equal(t, []string(findEmojiByID(t, repo, "e2").Aliases), []string{"x", "y"})
}

func TestEmojiAddAliasesBulk_Merges(t *testing.T) {
	h, repo := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "alpha", Aliases: []string{"existing"}},
	)

	rec := doPost(h.EmojiAddAliasesBulk,
		`{"ids":["e1"],"aliases":["new1","new2"]}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	// dedupe + preserve order
	assert.Equal(t, []string(findEmojiByID(t, repo, "e1").Aliases), []string{"existing", "new1", "new2"})
}

func TestEmojiAddAliasesBulk_DedupesAgainstExisting(t *testing.T) {
	h, repo := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "alpha", Aliases: []string{"a", "b"}},
	)

	rec := doPost(h.EmojiAddAliasesBulk,
		`{"ids":["e1"],"aliases":["b","c"]}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string(findEmojiByID(t, repo, "e1").Aliases), []string{"a", "b", "c"})
}

func TestEmojiRemoveAliasesBulk_Filters(t *testing.T) {
	h, repo := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "alpha", Aliases: []string{"a", "b", "c"}},
	)

	rec := doPost(h.EmojiRemoveAliasesBulk,
		`{"ids":["e1"],"aliases":["b"]}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string(findEmojiByID(t, repo, "e1").Aliases), []string{"a", "c"})
}

func TestEmojiDeleteBulk_Batch(t *testing.T) {
	h, repo := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "alpha"},
		&model.Emoji{ID: "e2", Name: "beta"},
		&model.Emoji{ID: "e3", Name: "gamma"},
	)

	rec := doPost(h.EmojiDeleteBulk, `{"ids":["e1","e3"]}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	// Mock は name@ キーなので ID で存在確認するには FindByID を使う
	_, err := repo.FindByID("e1")
	assert.Error(t, err, "e1 should be deleted")
	_, err = repo.FindByID("e2")
	assert.NoError(t, err, "e2 should remain")
	_, err = repo.FindByID("e3")
	assert.Error(t, err, "e3 should be deleted")
}

// #1999: upstream import-zip.ts は drive file の存在確認をせず無条件で enqueue する。
// 不明な fileId でも同期 NO_SUCH_FILE で弾かず 204 を返して job 側に委ねる。
func TestEmojiImportZip_UnknownFileEnqueues(t *testing.T) {
	h, _ := setupDriveFileHandler(t)
	enq := &stubEmojiImportEnqueuer{}
	h.SetEmojiImportEnqueuer(enq)
	rec := doPost(h.EmojiImportZip, `{"fileId":"missing"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code, "存在確認せず enqueue (#1999)")
}

// --- EmojiListV2 -------------------------------------------------------------

func TestEmojiListV2_Basic(t *testing.T) {
	h, _ := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "smile", PublicURL: "https://example.com/smile.png"},
		&model.Emoji{ID: "e2", Name: "wave", OriginalURL: "https://example.com/wave-orig.png"},
	)

	rec := doPost(h.EmojiListV2, `{}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	emojis := resp["emojis"].([]any)
	assert.Len(t, emojis, 2)
	assert.EqualValues(t, 2, resp["count"])
	assert.EqualValues(t, 2, resp["allCount"])
	assert.EqualValues(t, 1, resp["allPages"])
	// v2はpackDetailedAdmin相当: publicUrl/originalUrlを直接返す（computed urlは含まない）
	for _, raw := range emojis {
		em := raw.(map[string]any)
		_, hasPublicURL := em["publicUrl"]
		assert.True(t, hasPublicURL, "v2 emoji should have publicUrl field")
	}
}

// TestEmojiListV2_RoleObjectsAndShape は v2/admin/emoji/list の
// roleIdsThatCanBeUsedThisEmojiAsReaction が golden EmojiDetailedAdmin と同じ
// {id, name} object 配列で返ること、未知 role が省かれることを検証する。
// #1300 (gate の array element shape 検出 #1298 に依存)。
func TestEmojiListV2_RoleObjectsAndShape(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	require.NoError(t, roleRepo.Create(&model.Role{ID: "role1", Name: "Cool"}))
	repo := testutil.NewMockEmojiRepository()
	require.NoError(t, repo.Create(&model.Emoji{
		ID: "e1", Name: "smile",
		PublicURL: "https://example.com/s.png", OriginalURL: "https://example.com/s-orig.png",
		Aliases:                                 model.StringArray{"happy"},
		RoleIDsThatCanBeUsedThisEmojiAsReaction: model.StringArray{"role1", "ghost"},
	}))
	h.SetEmojiRepo(repo)

	rec := doPost(h.EmojiListV2, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	emojis := resp["emojis"].([]any)
	require.Len(t, emojis, 1)
	em := emojis[0].(map[string]any)

	// roleIds は {id, name} の object 配列。未知 role "ghost" は省かれる。
	roles := em["roleIdsThatCanBeUsedThisEmojiAsReaction"].([]any)
	require.Len(t, roles, 1)
	r0 := roles[0].(map[string]any)
	assert.Equal(t, "role1", r0["id"])
	assert.Equal(t, "Cool", r0["name"])

	shapetest.Assert(t, "EmojiDetailedAdmin", em) // L3 (#1300)
}

// #1948-13: roleIdsThatCanBeUsedThisEmojiAsReaction は displayOrder DESC, id ASC
// で sort される (upstream packDetailedAdmin)。
func TestEmojiListV2_RoleSortByDisplayOrder(t *testing.T) {
	h, _, _, roleRepo := newTestHandler(t)
	// displayOrder: rB=10, rA=5, rC=5 (同 displayOrder は id ASC)。
	require.NoError(t, roleRepo.Create(&model.Role{ID: "rB", Name: "B", DisplayOrder: 10}))
	require.NoError(t, roleRepo.Create(&model.Role{ID: "rA", Name: "A", DisplayOrder: 5}))
	require.NoError(t, roleRepo.Create(&model.Role{ID: "rC", Name: "C", DisplayOrder: 5}))
	repo := testutil.NewMockEmojiRepository()
	require.NoError(t, repo.Create(&model.Emoji{
		ID: "e1", Name: "smile", PublicURL: "https://example.com/s.png",
		// 格納順は意図的にバラバラ。
		RoleIDsThatCanBeUsedThisEmojiAsReaction: model.StringArray{"rA", "rC", "rB"},
	}))
	h.SetEmojiRepo(repo)

	rec := doPost(h.EmojiListV2, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	em := resp["emojis"].([]any)[0].(map[string]any)
	roles := em["roleIdsThatCanBeUsedThisEmojiAsReaction"].([]any)
	require.Len(t, roles, 3)
	// 期待順: rB(displayOrder10) → rA(5,id A) → rC(5,id C)。
	assert.Equal(t, "rB", roles[0].(map[string]any)["id"], "displayOrder DESC で rB が先頭 (#1948-13)")
	assert.Equal(t, "rA", roles[1].(map[string]any)["id"], "同 displayOrder は id ASC で rA (#1948-13)")
	assert.Equal(t, "rC", roles[2].(map[string]any)["id"])
}

// #1948-13: set-category-bulk / set-license-bulk に null を送ると SQL NULL に
// reset される (旧 non-pointer string では "" になっていた)。
func TestEmojiSetCategoryLicenseBulk_NullClears(t *testing.T) {
	cat, lic := "anime", "MIT"
	h, repo := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "a", Category: &cat, License: &lic},
	)
	get := func() *model.Emoji {
		e, err := repo.FindByID("e1")
		require.NoError(t, err)
		return e
	}
	rec := doPost(h.EmojiSetCategoryBulk, `{"ids":["e1"],"category":null}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Nil(t, get().Category, "category null は NULL reset (#1948-13)")

	rec = doPost(h.EmojiSetLicenseBulk, `{"ids":["e1"],"license":null}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Nil(t, get().License, "license null は NULL reset (#1948-13)")

	// 非 null は値設定 (回帰防止)。
	rec = doPost(h.EmojiSetCategoryBulk, `{"ids":["e1"],"category":"manga"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.NotNil(t, get().Category)
	assert.Equal(t, "manga", *get().Category)
}

// #1948-13: list-remote の host は toPuny (lowercase + IDN punycode) 正規化される。
func TestEmojiListRemote_HostToPuny(t *testing.T) {
	punyHost := "xn--eckwd4c7c.example" // "ドメイン.example" の punycode
	h, _ := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "remote1", Host: &punyHost},
	)
	// Unicode IDN を query → handler が toPunyHost で正規化して match。
	rec := doPost(h.EmojiListRemote, `{"host":"ドメイン.example"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp, 1, "IDN host が punycode 正規化されて match する (#1948-13)")
}

// #1948-10: EmojiDetailedAdmin の updatedAt は upstream toISOString() (.000Z / null)。
// raw *time.Time の RFC3339Nano だと wire-byte が乖離する。
func TestEmojiListV2_UpdatedAtFormat(t *testing.T) {
	upd := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	h, _ := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "smile", PublicURL: "https://example.com/s.png", UpdatedAt: &upd},
		&model.Emoji{ID: "e2", Name: "wave", PublicURL: "https://example.com/w.png"}, // updatedAt nil
	)
	rec := doPost(h.EmojiListV2, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	byID := map[string]map[string]any{}
	for _, raw := range resp["emojis"].([]any) {
		em := raw.(map[string]any)
		byID[em["id"].(string)] = em
	}
	assert.Equal(t, "2026-01-02T03:04:05.000Z", byID["e1"]["updatedAt"], "updatedAt は .000Z (#1948-10)")
	assert.Nil(t, byID["e2"]["updatedAt"], "nil updatedAt は null (#1948-10)")
}

func TestEmojiListV2_NilRepo(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.EmojiListV2, `{}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Len(t, resp["emojis"].([]any), 0)
	assert.EqualValues(t, 0, resp["allCount"])
}

func TestEmojiListV2_InvalidJSON(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.EmojiListV2, `invalid`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestEmojiListV2_HostType(t *testing.T) {
	host := "remote.example"
	h, _ := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "local_only"},
		&model.Emoji{ID: "e2", Name: "remote_one", Host: &host},
	)

	// local のみ
	rec := doPost(h.EmojiListV2, `{"query":{"hostType":"local"}}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	emojis := resp["emojis"].([]any)
	assert.Len(t, emojis, 1)
	assert.Equal(t, "e1", emojis[0].(map[string]any)["id"])

	// remote のみ
	rec = doPost(h.EmojiListV2, `{"query":{"hostType":"remote"}}`, adminUser)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	emojis = resp["emojis"].([]any)
	assert.Len(t, emojis, 1)
	assert.Equal(t, "e2", emojis[0].(map[string]any)["id"])

	// all（デフォルト）
	rec = doPost(h.EmojiListV2, `{"query":{"hostType":"all"}}`, adminUser)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	emojis = resp["emojis"].([]any)
	assert.Len(t, emojis, 2)
}

func TestEmojiListV2_QueryFilter(t *testing.T) {
	h, _ := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "cat_smile"},
		&model.Emoji{ID: "e2", Name: "dog_run"},
		&model.Emoji{ID: "e3", Name: "caterpillar"},
	)

	rec := doPost(h.EmojiListV2, `{"query":{"name":"cat"}}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	emojis := resp["emojis"].([]any)
	assert.Len(t, emojis, 2, "cat_smile and caterpillar should match")
	assert.EqualValues(t, 2, resp["allCount"])
}

// #466: frontend が remote emoji 検索ボックスから uri / publicUrl /
// originalUrl filter を渡してくる。handler / model / repo がこれらを
// 受けないと silent ignore で「検索しても全件出る」状態になる。
func TestEmojiListV2_URIFilter(t *testing.T) {
	hostA := "alpha.example"
	hostB := "beta.example"
	uriA := "https://alpha.example/emojis/x"
	uriB := "https://beta.example/emojis/y"
	h, _ := setupEmojiHandler(t,
		&model.Emoji{
			ID: "e1", Name: "x", Host: &hostA, URI: &uriA,
			OriginalURL: "https://alpha-cdn.example/x.png",
			PublicURL:   "https://alpha-cdn.example/x.png",
		},
		&model.Emoji{
			ID: "e2", Name: "y", Host: &hostB, URI: &uriB,
			OriginalURL: "https://beta-cdn.example/y.png",
			PublicURL:   "https://beta-cdn.example/y.png",
		},
	)

	t.Run("uri filter narrows to alpha", func(t *testing.T) {
		rec := doPost(h.EmojiListV2, `{"query":{"uri":"alpha.example"}}`, adminUser)
		assert.Equal(t, http.StatusOK, rec.Code)
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		emojis := resp["emojis"].([]any)
		require.Len(t, emojis, 1)
		assert.Equal(t, "e1", emojis[0].(map[string]any)["id"])
	})
	t.Run("publicUrl filter narrows to beta", func(t *testing.T) {
		rec := doPost(h.EmojiListV2, `{"query":{"publicUrl":"beta-cdn"}}`, adminUser)
		assert.Equal(t, http.StatusOK, rec.Code)
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		emojis := resp["emojis"].([]any)
		require.Len(t, emojis, 1)
		assert.Equal(t, "e2", emojis[0].(map[string]any)["id"])
	})
	t.Run("originalUrl filter narrows to alpha", func(t *testing.T) {
		rec := doPost(h.EmojiListV2, `{"query":{"originalUrl":"alpha-cdn"}}`, adminUser)
		assert.Equal(t, http.StatusOK, rec.Code)
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		emojis := resp["emojis"].([]any)
		require.Len(t, emojis, 1)
		assert.Equal(t, "e1", emojis[0].(map[string]any)["id"])
	})
}

func TestEmojiListV2_Pagination(t *testing.T) {
	seeds := make([]*model.Emoji, 5)
	for i := 0; i < 5; i++ {
		id := "e" + string(rune('1'+i))
		seeds[i] = &model.Emoji{ID: id, Name: "emoji_" + id}
	}
	h, _ := setupEmojiHandler(t, seeds...)

	// page 1, limit 2
	rec := doPost(h.EmojiListV2, `{"limit":2,"page":1}`, adminUser)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	emojis := resp["emojis"].([]any)
	assert.Len(t, emojis, 2)
	assert.EqualValues(t, 5, resp["allCount"])
	assert.EqualValues(t, 3, resp["allPages"])

	// page 3 — 残り1件
	rec = doPost(h.EmojiListV2, `{"limit":2,"page":3}`, adminUser)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	emojis = resp["emojis"].([]any)
	assert.Len(t, emojis, 1)
	assert.EqualValues(t, 1, resp["count"])
}

func TestEmojiListV2_SortKeys(t *testing.T) {
	h, _ := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "beta"},
		&model.Emoji{ID: "e2", Name: "alpha"},
		&model.Emoji{ID: "e3", Name: "gamma"},
	)

	// name ASC
	rec := doPost(h.EmojiListV2, `{"sortKeys":["+name"]}`, adminUser)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	emojis := resp["emojis"].([]any)
	require.Len(t, emojis, 3)
	assert.Equal(t, "alpha", emojis[0].(map[string]any)["name"])
	assert.Equal(t, "beta", emojis[1].(map[string]any)["name"])
	assert.Equal(t, "gamma", emojis[2].(map[string]any)["name"])

	// デフォルト: id DESC
	rec = doPost(h.EmojiListV2, `{}`, adminUser)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	emojis = resp["emojis"].([]any)
	require.Len(t, emojis, 3)
	assert.Equal(t, "e3", emojis[0].(map[string]any)["id"])
	assert.Equal(t, "e1", emojis[2].(map[string]any)["id"])
}

func TestEmojiListV2_SensitiveFilter(t *testing.T) {
	h, _ := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "safe", IsSensitive: false},
		&model.Emoji{ID: "e2", Name: "nsfw", IsSensitive: true},
	)

	rec := doPost(h.EmojiListV2, `{"query":{"isSensitive":true}}`, adminUser)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	emojis := resp["emojis"].([]any)
	assert.Len(t, emojis, 1)
	assert.Equal(t, "e2", emojis[0].(map[string]any)["id"])
}

func TestEmojiListV2_LimitOutOfRangeRejected(t *testing.T) {
	h, _ := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "a"},
	)

	// upstream は ajv で maximum を強制するので範囲外は 400。
	rec := doPost(h.EmojiListV2, `{"limit":999}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// 範囲内なら従来どおり結果を返す。
	rec = doPost(h.EmojiListV2, `{"limit":100}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.EqualValues(t, 1, resp["allPages"])
}

func TestEmojiListV2_ListV2Error(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetEmojiRepo(&failingListV2EmojiRepo{testutil.NewMockEmojiRepository()})
	rec := doPost(h.EmojiListV2, `{}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- thin nil-repo smoke tests ---
//
// nil repo 経路で 204 / 200 が返ることを担保する軽量テスト。

func TestDriveCleanRemoteFiles(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.DriveCleanRemoteFiles, `{}`, adminUser).Code)
}

func TestDriveCleanup(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.DriveCleanup, `{}`, adminUser).Code)
}

func TestDriveFilesAdmin(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusOK, doPost(h.DriveFiles, `{}`, adminUser).Code)
}

func TestDriveShowFile_Empty(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusBadRequest, doPost(h.DriveShowFile, `{}`, adminUser).Code)
}

func TestDriveShowFile_NotFound(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusBadRequest, doPost(h.DriveShowFile, `{"fileId":"ghost"}`, adminUser).Code)
}

func TestEmojiAddAliasesBulk(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.EmojiAddAliasesBulk, `{}`, adminUser).Code)
}

func TestEmojiCopy(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// emojiId 欠落は 400
	assert.Equal(t, http.StatusBadRequest, doPost(h.EmojiCopy, `{}`, adminUser).Code)
}

func TestEmojiDeleteBulk(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.EmojiDeleteBulk, `{}`, adminUser).Code)
}

func TestEmojiListRemote(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusOK, doPost(h.EmojiListRemote, `{}`, adminUser).Code)
}

func TestEmojiRemoveAliasesBulk(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.EmojiRemoveAliasesBulk, `{}`, adminUser).Code)
}

func TestEmojiSetAliasesBulk(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.EmojiSetAliasesBulk, `{}`, adminUser).Code)
}

func TestEmojiSetCategoryBulk(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.EmojiSetCategoryBulk, `{}`, adminUser).Code)
}

func TestEmojiSetLicenseBulk(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusNoContent, doPost(h.EmojiSetLicenseBulk, `{}`, adminUser).Code)
}

// --- EmojiImportZip 詳細 (旧 handler_stubs_test.go から移動) ---

func TestEmojiImportZip_NoFileID(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// fileId missing → 400 InvalidParam
	assert.Equal(t, http.StatusBadRequest, doPost(h.EmojiImportZip, `{}`, adminUser).Code)
}

func TestEmojiImportZip_MalformedJSON(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusBadRequest, doPost(h.EmojiImportZip, `not-json`, adminUser).Code)
}

func TestEmojiImportZip_NoEnqueuer(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// enqueuer not set → 204 (no-op fallback so tests without worker still pass)
	assert.Equal(t, http.StatusNoContent, doPost(h.EmojiImportZip, `{"fileId":"f1"}`, adminUser).Code)
}

func TestEmojiImportZip_NilUser(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetEmojiImportEnqueuer(&stubEmojiImportEnqueuer{})
	assert.Equal(t, http.StatusNoContent, doPost(h.EmojiImportZip, `{"fileId":"f1"}`, nil).Code)
}

func TestEmojiImportZip_Success(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubEmojiImportEnqueuer{}
	h.SetEmojiImportEnqueuer(stub)
	rec := doPost(h.EmojiImportZip, `{"fileId":"f1"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "f1", stub.lastFileID)
	assert.Equal(t, "admin1", stub.lastUserID)
}

func TestEmojiImportZip_EnqueueFailure(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetEmojiImportEnqueuer(&stubEmojiImportEnqueuer{err: assertError{}})
	rec := doPost(h.EmojiImportZip, `{"fileId":"f1"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- moderation log assertions (#661 + #650) ---

func TestEmojiAdd_WritesModerationLog(t *testing.T) {
	h, _ := setupEmojiHandler(t)
	repo := attachModLog(t, h)

	rec := doPost(h.EmojiAdd, `{"name":"smile","url":"https://example/smile.png"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	logs := repo.Snapshot()
	assert.Equal(t, "addCustomEmoji", logs[0].Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	assert.NotEmpty(t, info["emojiId"])
	assert.NotNil(t, info["emoji"])
}

func TestEmojiAdd_PersistsParamsAndReturnsDetailed(t *testing.T) {
	h, repo := setupEmojiHandler(t)
	body := `{"name":"happy","url":"https://example/happy.png","category":"face","aliases":["joy","glad"],"license":"CC0","isSensitive":true,"localOnly":true,"roleIdsThatCanBeUsedThisEmojiAsReaction":["r1","r2"]}`
	rec := doPost(h.EmojiAdd, body, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	// response は EmojiDetailed (url あり、内部 field は漏れない)。
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "https://example/happy.png", resp["url"])
	assert.NotContains(t, resp, "originalUrl")
	assert.NotContains(t, resp, "publicUrl")
	assert.NotContains(t, resp, "uri")
	assert.Equal(t, "CC0", resp["license"])
	assert.Equal(t, []any{"r1", "r2"}, resp["roleIdsThatCanBeUsedThisEmojiAsReaction"])

	// params が永続化される。
	got, err := repo.FindByNameAndHost("happy", nil)
	require.NoError(t, err)
	require.NotNil(t, got.Category)
	assert.Equal(t, "face", *got.Category)
	assert.Equal(t, []string{"joy", "glad"}, []string(got.Aliases))
	assert.True(t, got.IsSensitive)
	assert.True(t, got.LocalOnly)
	assert.Equal(t, []string{"r1", "r2"}, []string(got.RoleIDsThatCanBeUsedThisEmojiAsReaction))
}

func TestEmojiAdd_DuplicateName(t *testing.T) {
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "dup"})
	rec := doPost(h.EmojiAdd, `{"name":"dup","url":"https://example/x.png"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "DUPLICATE_NAME")
}

func TestEmojiAdd_InvalidNamePattern(t *testing.T) {
	h, _ := setupEmojiHandler(t)
	rec := doPost(h.EmojiAdd, `{"name":"bad name!","url":"https://example/x.png"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestEmojiAdd_UnsupportedFileType(t *testing.T) {
	h, _ := setupEmojiHandler(t)
	dr := testutil.NewMockDriveFileRepository()
	owner := "u1"
	require.NoError(t, dr.Create(&model.DriveFile{ID: "f_txt", UserID: &owner, Type: "text/plain", URL: "https://example/x.txt"}))
	h.SetDriveFileRepo(dr)
	rec := doPost(h.EmojiAdd, `{"name":"x","fileId":"f_txt"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "UNSUPPORTED_FILE_TYPE")
}

// SVG は image/ prefix だが allowlist 外 (XSS 理由) なので拒否される。
func TestEmojiAdd_RejectsSvg(t *testing.T) {
	h, _ := setupEmojiHandler(t)
	dr := testutil.NewMockDriveFileRepository()
	owner := "u1"
	require.NoError(t, dr.Create(&model.DriveFile{ID: "f_svg", UserID: &owner, Type: "image/svg+xml", URL: "https://example/x.svg"}))
	h.SetDriveFileRepo(dr)
	rec := doPost(h.EmojiAdd, `{"name":"x","fileId":"f_svg"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "UNSUPPORTED_FILE_TYPE")
}

// fileId 経路の正常系: originalUrl=f.URL、publicUrl/type は webpublic variant を優先。
func TestEmojiAdd_FileIdImagePersistsWebpublic(t *testing.T) {
	h, repo := setupEmojiHandler(t)
	dr := testutil.NewMockDriveFileRepository()
	owner := "u1"
	wpURL := "https://example/wp.webp"
	wpType := "image/webp"
	require.NoError(t, dr.Create(&model.DriveFile{
		ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/orig.png",
		WebpublicURL: &wpURL, WebpublicType: &wpType,
	}))
	h.SetDriveFileRepo(dr)
	rec := doPost(h.EmojiAdd, `{"name":"happy","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	got, err := repo.FindByNameAndHost("happy", nil)
	require.NoError(t, err)
	assert.Equal(t, "https://example/orig.png", got.OriginalURL)
	assert.Equal(t, wpURL, got.PublicURL, "publicUrl は webpublicUrl を優先")
	require.NotNil(t, got.Type)
	assert.Equal(t, wpType, *got.Type, "type は webpublicType を優先")
}

// url + 非画像 fileId 併用時は url が勝ち、filetype 検証は走らない (legacy 挙動)。
func TestEmojiAdd_UrlWinsOverFileId(t *testing.T) {
	h, _ := setupEmojiHandler(t)
	dr := testutil.NewMockDriveFileRepository()
	owner := "u1"
	require.NoError(t, dr.Create(&model.DriveFile{ID: "f_txt", UserID: &owner, Type: "text/plain", URL: "https://example/x.txt"}))
	h.SetDriveFileRepo(dr)
	rec := doPost(h.EmojiAdd, `{"name":"x","url":"https://example/y.png","fileId":"f_txt"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code, "url 指定時は fileId 経路を通らず filetype 検証もしない")
}

// DUPLICATE_NAME は local (host=nil) のみ対象。remote 同名は衝突扱いしない。
func TestEmojiAdd_RemoteSameNameAllowed(t *testing.T) {
	remote := "remote.example"
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "er", Name: "dup", Host: &remote})
	rec := doPost(h.EmojiAdd, `{"name":"dup","url":"https://example/x.png"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code, "remote 同名 emoji があっても local add は通る")
}

func TestEmojiUpdate_FileIdReplacesImage(t *testing.T) {
	h, emojiRepo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "smile", OriginalURL: "old", PublicURL: "old"})
	dr := testutil.NewMockDriveFileRepository()
	owner := "u1"
	require.NoError(t, dr.Create(&model.DriveFile{ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/new.png"}))
	h.SetDriveFileRepo(dr)

	rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	got, err := emojiRepo.FindByID("e1")
	require.NoError(t, err)
	assert.Equal(t, "https://example/new.png", got.OriginalURL)
	assert.Equal(t, "https://example/new.png", got.PublicURL)
}

// #1772: emoji 不在 AND fileId 不正が同時のとき、upstream は file 検証を先に行うため
// NO_SUCH_FILE を返す (NO_SUCH_EMOJI でなく)。
func TestEmojiUpdate_FileCheckedBeforeEmoji(t *testing.T) {
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "smile"})
	dr := testutil.NewMockDriveFileRepository()
	h.SetDriveFileRepo(dr) // fileId は seed しない
	rec := doPost(h.EmojiUpdate, `{"id":"ghost","fileId":"badfile"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FILE")
	assert.Contains(t, rec.Body.String(), "14fb9fd9-0731-4e2f-aeb9-f09e4740333d")
}

func TestEmojiUpdate_RoleIds(t *testing.T) {
	h, emojiRepo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "smile"})
	rec := doPost(h.EmojiUpdate, `{"id":"e1","roleIdsThatCanBeUsedThisEmojiAsReaction":["r1","r2"]}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	got, err := emojiRepo.FindByID("e1")
	require.NoError(t, err)
	assert.Equal(t, []string{"r1", "r2"}, []string(got.RoleIDsThatCanBeUsedThisEmojiAsReaction))
}

func TestEmojiUpdate_SameNameExists(t *testing.T) {
	h, _ := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "alpha"},
		&model.Emoji{ID: "e2", Name: "beta"},
	)
	// e1 を beta にリネーム → 既存 beta と衝突。
	rec := doPost(h.EmojiUpdate, `{"id":"e1","name":"beta"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "SAME_NAME_EMOJI_EXISTS")
}

func TestEmojiUpdate_NoSuchFile(t *testing.T) {
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "smile"})
	dr := testutil.NewMockDriveFileRepository()
	h.SetDriveFileRepo(dr)
	rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"ghost"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_FILE")
}

// 同名への self-rename は重複扱いせず 204 で通る。
func TestEmojiUpdate_SelfRenameOk(t *testing.T) {
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "smile"})
	rec := doPost(h.EmojiUpdate, `{"id":"e1","name":"smile"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// upstream paramDef は {name} のみでの更新も許す。id 無し name 指定時は
// local emoji を name で解決して他フィールドを更新する (#1543)。
func TestEmojiUpdate_NameOnly(t *testing.T) {
	h, emojiRepo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "smile"})
	rec := doPost(h.EmojiUpdate, `{"name":"smile","category":"faces"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	got, err := emojiRepo.FindByNameAndHost("smile", nil)
	require.NoError(t, err)
	require.NotNil(t, got.Category)
	assert.Equal(t, "faces", *got.Category, "name で解決した emoji の category が更新される")
}

// name のみ指定で該当 local emoji が無ければ NO_SUCH_EMOJI (#1543)。
func TestEmojiUpdate_NameOnly_NotFound(t *testing.T) {
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "smile"})
	rec := doPost(h.EmojiUpdate, `{"name":"ghost","category":"x"}`, adminUser)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_EMOJI")
}

// id も name も無ければ INVALID_PARAM (#1543)。
func TestEmojiUpdate_NeitherIDNorName(t *testing.T) {
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "smile"})
	rec := doPost(h.EmojiUpdate, `{"category":"x"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestEmojiUpdate_UnsupportedFileType(t *testing.T) {
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "smile"})
	dr := testutil.NewMockDriveFileRepository()
	owner := "u1"
	require.NoError(t, dr.Create(&model.DriveFile{ID: "f_svg", UserID: &owner, Type: "image/svg+xml", URL: "https://example/x.svg"}))
	h.SetDriveFileRepo(dr)
	rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_svg"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "UNSUPPORTED_FILE_TYPE")
}

// fileId 差替も webpublic variant を優先する (add と同ロジック、F1)。
func TestEmojiUpdate_FileIdWebpublic(t *testing.T) {
	h, emojiRepo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "smile", OriginalURL: "old", PublicURL: "old"})
	dr := testutil.NewMockDriveFileRepository()
	owner := "u1"
	wpURL := "https://example/wp.webp"
	wpType := "image/webp"
	require.NoError(t, dr.Create(&model.DriveFile{
		ID: "f_img", UserID: &owner, Type: "image/png", URL: "https://example/orig.png",
		WebpublicURL: &wpURL, WebpublicType: &wpType,
	}))
	h.SetDriveFileRepo(dr)
	rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	got, err := emojiRepo.FindByID("e1")
	require.NoError(t, err)
	assert.Equal(t, "https://example/orig.png", got.OriginalURL)
	assert.Equal(t, wpURL, got.PublicURL)
	require.NotNil(t, got.Type)
	assert.Equal(t, wpType, *got.Type)
}

// driveFileRepo 未配線 + fileId 指定は silent 204 でなく 500 にする (F3)。
func TestEmojiUpdate_FileIdNoDriveRepo(t *testing.T) {
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "smile"})
	// SetDriveFileRepo を呼ばない (driveFileRepo == nil)。
	rec := doPost(h.EmojiUpdate, `{"id":"e1","fileId":"f_img"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestEmojiUpdate_WritesModerationLog_WithExtendedFields(t *testing.T) {
	// #650 問題 2 + #661: Misskey 互換の license/isSensitive/localOnly が
	// 永続化されること、updateCustomEmoji log に before/after が入ること。
	h, emojiRepo := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "smile"},
	)
	repo := attachModLog(t, h)

	body := `{"id":"e1","name":"smile2","license":"CC0","isSensitive":true,"localOnly":true}`
	rec := doPost(h.EmojiUpdate, body, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	// DB 更新の検証
	updated, err := emojiRepo.FindByID("e1")
	require.NoError(t, err)
	assert.Equal(t, "smile2", updated.Name)
	require.NotNil(t, updated.License)
	assert.Equal(t, "CC0", *updated.License)
	assert.True(t, updated.IsSensitive)
	assert.True(t, updated.LocalOnly)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	logs := repo.Snapshot()
	assert.Equal(t, "updateCustomEmoji", logs[0].Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	assert.Equal(t, "e1", info["emojiId"])
	require.NotNil(t, info["before"])
	require.NotNil(t, info["after"])
}

func TestEmojiUpdate_NoSuchEmojiOnMissingID(t *testing.T) {
	// #650 問題 2: id が DB に無いケースは NO_SUCH_EMOJI を返す。以前は
	// UpdateFields が RowsAffected を見ずに常に nil 返却で 204 になっていた。
	// #729: UUID は upstream `684dec9d-...` (mk-go の旧 typo `684b7e7e-...`
	// から修正) と完全一致することも assert する。
	h, _ := setupEmojiHandler(t)
	rec := doPost(h.EmojiUpdate, `{"id":"ghost","name":"x"}`, adminUser)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errField, ok := body["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "NO_SUCH_EMOJI", errField["code"])
	assert.Equal(t, apierr.UUIDNoSuchEmoji, errField["id"])
	assert.Equal(t, "684dec9d-a8c2-4364-9aa8-456c49cb1dc8", errField["id"], "UUID matches upstream noSuchEmoji")
}

func TestEmojiDelete_WritesModerationLog(t *testing.T) {
	h, _ := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "doomed"},
	)
	repo := attachModLog(t, h)

	rec := doPost(h.EmojiDelete, `{"id":"e1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	logs := repo.Snapshot()
	assert.Equal(t, "deleteCustomEmoji", logs[0].Type)
	var info map[string]any
	require.NoError(t, json.Unmarshal(logs[0].Info, &info))
	assert.Equal(t, "e1", info["emojiId"])
	assert.NotNil(t, info["emoji"])
}

func TestEmojiCopy_WritesModerationLog(t *testing.T) {
	remoteHost := "remote.example"
	h, _ := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "wave", Host: &remoteHost},
	)
	repo := attachModLog(t, h)

	rec := doPost(h.EmojiCopy, `{"emojiId":"e1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 1 }, 500*time.Millisecond, 5*time.Millisecond)
	assert.Equal(t, "addCustomEmoji", repo.Snapshot()[0].Type)
}

func TestEmojiDeleteBulk_WritesPerEmojiLog(t *testing.T) {
	// Misskey TS 互換: delete-bulk は対象絵文字数だけ deleteCustomEmoji
	// log を出す (CustomEmojiService.deleteBulk の for ループ相当)。
	h, _ := setupEmojiHandler(t,
		&model.Emoji{ID: "e1", Name: "a"},
		&model.Emoji{ID: "e2", Name: "b"},
		&model.Emoji{ID: "e3", Name: "c"},
	)
	repo := attachModLog(t, h)

	rec := doPost(h.EmojiDeleteBulk, `{"ids":["e1","e2","e3"]}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == 3 }, 500*time.Millisecond, 5*time.Millisecond)
	for _, l := range repo.Snapshot() {
		assert.Equal(t, "deleteCustomEmoji", l.Type)
	}
}

// TestEmojiDeleteBulk_BatchedSingleInsert guards the #671 contract: a bulk
// delete of N emoji results in exactly 1 CreateMany call (not N Create
// calls), so 数千件の operation でも N goroutine + N round-trips に
// fan-out しない。
func TestEmojiDeleteBulk_BatchedSingleInsert(t *testing.T) {
	const n = 50
	ids := make([]string, 0, n)
	emojis := make([]*model.Emoji, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("e%d", i)
		ids = append(ids, id)
		emojis = append(emojis, &model.Emoji{ID: id, Name: id})
	}
	h, _ := setupEmojiHandler(t, emojis...)
	repo := attachModLog(t, h)

	body, err := json.Marshal(map[string]any{"ids": ids})
	require.NoError(t, err)
	rec := doPost(h.EmojiDeleteBulk, string(body), adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Eventually(t, func() bool { return len(repo.Snapshot()) == n }, 1*time.Second, 5*time.Millisecond)

	create, createMany := repo.CallCounts()
	assert.Equal(t, 0, create, "Create must not be called per-item (would defeat #671 batching)")
	assert.Equal(t, 1, createMany, "CreateMany must be called exactly once for the whole batch")
}

// stubStorageDeleter records the access keys passed to Delete. failKeys 内の
// key には error を返す (best-effort 続行の検証用)。
type stubStorageDeleter struct {
	deleted  []string
	failKeys map[string]bool
}

func (s *stubStorageDeleter) Delete(key string) error {
	s.deleted = append(s.deleted, key)
	if s.failKeys[key] {
		return assertError{}
	}
	return nil
}

// clean-remote-files: storage backend 配線時は cached remote file の
// access/thumbnail/webpublic object を削除してから DB 行を消す。
func TestDriveCleanRemoteFiles_DeletesStorageObjects(t *testing.T) {
	host := "remote.example"
	ak1, tk1, ak2, akl := "ak1", "tk1", "ak2", "akl"
	h, repo := setupDriveFileHandler(t,
		&model.DriveFile{ID: "c1", IsLink: false, UserHost: &host, AccessKey: &ak1, ThumbnailAccessKey: &tk1},
		&model.DriveFile{ID: "c2", IsLink: false, UserHost: &host, AccessKey: &ak2},
		&model.DriveFile{ID: "link", IsLink: true, UserHost: &host, AccessKey: &akl},
	)
	sd := &stubStorageDeleter{}
	h.SetStorageDeleter(sd)

	rec := doPost(h.DriveCleanRemoteFiles, `{}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotContains(t, repo.Files, "c1")
	assert.NotContains(t, repo.Files, "c2")
	assert.Contains(t, repo.Files, "link", "link-only proxy は削除しない")
	// cached file の object key のみ削除 (link-only の akl は対象外)。
	assert.ElementsMatch(t, []string{"ak1", "tk1", "ak2"}, sd.deleted)
}

// delete-all-files-of-a-user: storage 配線時は対象ユーザーの file object も削除。
func TestDeleteAllFilesOfUser_DeletesStorageObjects(t *testing.T) {
	u, other := "u1", "u2"
	ak := "aku1"
	h, repo := setupDriveFileHandler(t,
		&model.DriveFile{ID: "f1", UserID: &u, AccessKey: &ak},
		&model.DriveFile{ID: "f2", UserID: &other},
	)
	sd := &stubStorageDeleter{}
	h.SetStorageDeleter(sd)

	rec := doPost(h.DeleteAllFilesOfUser, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotContains(t, repo.Files, "f1")
	assert.Contains(t, repo.Files, "f2", "他ユーザーの file は残す")
	assert.Contains(t, sd.deleted, "aku1")
}

// clean-remote-files が driveCleanupBatchSize(100) を超える件数を複数バッチで
// 全削除する (batching の終了性 + 継続)。
func TestDriveCleanRemoteFiles_MultiBatch(t *testing.T) {
	host := "remote.example"
	h, repo := setupDriveFileHandler(t)
	for i := 0; i < 150; i++ {
		ak := "ak" + strconv.Itoa(i)
		require.NoError(t, repo.Create(&model.DriveFile{
			ID: "c" + strconv.Itoa(i), IsLink: false, UserHost: &host, AccessKey: &ak,
		}))
	}
	sd := &stubStorageDeleter{}
	h.SetStorageDeleter(sd)

	rec := doPost(h.DriveCleanRemoteFiles, `{}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, repo.Files, "150 件すべて削除される (複数バッチ)")
	assert.Len(t, sd.deleted, 150, "全 access key が storage 削除される")
}

// storage Delete が失敗しても他 key の削除を続行し DB 行は消す (best-effort)。
func TestDriveCleanRemoteFiles_StorageDeleteFailureBestEffort(t *testing.T) {
	host := "remote.example"
	ak1, tk1, ak2 := "ak1", "tk1", "ak2"
	h, repo := setupDriveFileHandler(t,
		&model.DriveFile{ID: "c1", IsLink: false, UserHost: &host, AccessKey: &ak1, ThumbnailAccessKey: &tk1},
		&model.DriveFile{ID: "c2", IsLink: false, UserHost: &host, AccessKey: &ak2},
	)
	sd := &stubStorageDeleter{failKeys: map[string]bool{"ak1": true}}
	h.SetStorageDeleter(sd)

	rec := doPost(h.DriveCleanRemoteFiles, `{}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	// ak1 が失敗しても tk1 / ak2 の削除は続行され、DB 行は全て消える。
	assert.ElementsMatch(t, []string{"ak1", "tk1", "ak2"}, sd.deleted)
	assert.Empty(t, repo.Files)
}

// list (DB) 失敗は 500 (DB-only 経路の 500 と整合)。
func TestDriveCleanRemoteFiles_ListErrorReturns500(t *testing.T) {
	h, repo := setupDriveFileHandler(t)
	repo.ListRemoteCacheErr = assertError{}
	h.SetStorageDeleter(&stubStorageDeleter{})
	rec := doPost(h.DriveCleanRemoteFiles, `{}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// DeleteByIDs (DB) 失敗も 500。
func TestDeleteAllFilesOfUser_DeleteErrorReturns500(t *testing.T) {
	u := "u1"
	ak := "aku"
	h, repo := setupDriveFileHandler(t, &model.DriveFile{ID: "f1", UserID: &u, AccessKey: &ak})
	repo.DeleteByIDsErr = assertError{}
	h.SetStorageDeleter(&stubStorageDeleter{})
	rec := doPost(h.DeleteAllFilesOfUser, `{"userId":"u1"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// DeleteByIDs が行を消さない degenerate ケースでも maxBatches 安全弁で有限終了する。
func TestDriveCleanRemoteFiles_TerminatesWhenNoProgress(t *testing.T) {
	host := "remote.example"
	ak := "ak1"
	h, repo := setupDriveFileHandler(t,
		&model.DriveFile{ID: "c1", IsLink: false, UserHost: &host, AccessKey: &ak},
	)
	repo.DeleteByIDsNoOp = true // 行が縮小しない
	h.SetStorageDeleter(&stubStorageDeleter{})
	done := make(chan struct{})
	go func() {
		doPost(h.DriveCleanRemoteFiles, `{}`, adminUser)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deleteFilesBatched did not terminate (safety valve failed)")
	}
}

// webpublic access key も storage 削除対象に含まれ、空文字 key は skip される。
func TestDriveCleanRemoteFiles_WebpublicKeyAndEmptySkip(t *testing.T) {
	host := "remote.example"
	ak, wk, empty := "ak1", "wk1", ""
	h, _ := setupDriveFileHandler(t,
		&model.DriveFile{ID: "c1", IsLink: false, UserHost: &host, AccessKey: &ak, WebpublicAccessKey: &wk, ThumbnailAccessKey: &empty},
	)
	sd := &stubStorageDeleter{}
	h.SetStorageDeleter(sd)
	rec := doPost(h.DriveCleanRemoteFiles, `{}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	// access + webpublic は削除、空文字 thumbnail key は skip。
	assert.ElementsMatch(t, []string{"ak1", "wk1"}, sd.deleted)
}

// #2315: object storage 有効化より前に保存された storedInternal=true な行は
// ローカル FS に実体がある。primary (= object storage) に消しに行くと空振りし、
// ローカルに孤児が残る。
func TestDriveCleanup_RoutesDeletesByStoredInternal(t *testing.T) {
	legacy, current := "legacy-ak", "current-ak"
	h, repo := setupDriveFileHandler(t,
		&model.DriveFile{ID: "old", UserID: nil, AccessKey: &legacy, StoredInternal: true},
		&model.DriveFile{ID: "new", UserID: nil, AccessKey: &current, StoredInternal: false},
	)
	primary := &stubStorageDeleter{}
	local := &stubStorageDeleter{}
	h.SetStorageDeleter(primary)
	h.SetLocalStorageDeleter(local)

	rec := doPost(h.DriveCleanup, `{}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, repo.Files)
	assert.ElementsMatch(t, []string{"current-ak"}, primary.deleted)
	assert.ElementsMatch(t, []string{"legacy-ak"}, local.deleted)
}

// local deleter 未配線なら従来どおり primary に倒す。
func TestDriveCleanup_WithoutLocalDeleterUsesPrimary(t *testing.T) {
	ak := "ak"
	h, _ := setupDriveFileHandler(t,
		&model.DriveFile{ID: "old", UserID: nil, AccessKey: &ak, StoredInternal: true},
	)
	primary := &stubStorageDeleter{}
	h.SetStorageDeleter(primary)

	rec := doPost(h.DriveCleanup, `{}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.ElementsMatch(t, []string{"ak"}, primary.deleted)
}

// **リネーム時に name の pattern を掛ける。これは upstream より厳しい。**
//
// upstream の paramDef は `anyOf[{id 必須}, {name 必須 + pattern}]` なので、
// **`id` を渡すと `^[a-zA-Z0-9_]+$` は検査されない** — frontend の通常の
// 呼び方がそれにあたる。結果として `:foo bar:` のような名前が保存でき、
// AP で broadcast される。
func TestEmojiUpdate_RejectsInvalidName(t *testing.T) {
	for _, name := range []string{"foo bar", "foo:bar", "foo<x>", "", "絵文字", "a/b"} {
		t.Run(name, func(t *testing.T) {
			h, emojiRepo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "smile"})
			body, err := json.Marshal(map[string]any{"id": "e1", "name": name})
			require.NoError(t, err)

			rec := doPost(h.EmojiUpdate, string(body), adminUser)
			assert.Equalf(t, http.StatusBadRequest, rec.Code,
				"name=%q が通った。AP で broadcast される", name)
			assert.Contains(t, rec.Body.String(), "INVALID_PARAM")

			// 保存もされていないこと。
			got, ferr := emojiRepo.FindByID("e1")
			require.NoError(t, ferr)
			assert.Equal(t, "smile", got.Name, "不正な名前が保存されている")
		})
	}
}

// 正当な名前は通る (pattern を厳しくしすぎていないか)。
func TestEmojiUpdate_AcceptsValidName(t *testing.T) {
	for _, name := range []string{"smile2", "foo_bar", "A", "a1_B2"} {
		t.Run(name, func(t *testing.T) {
			h, emojiRepo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "smile"})
			body, err := json.Marshal(map[string]any{"id": "e1", "name": name})
			require.NoError(t, err)

			rec := doPost(h.EmojiUpdate, string(body), adminUser)
			require.Equalf(t, http.StatusNoContent, rec.Code, "name=%q が落ちた: %s", name, rec.Body.String())
			got, ferr := emojiRepo.FindByID("e1")
			require.NoError(t, ferr)
			assert.Equal(t, name, got.Name)
		})
	}
}

// **名前を変えない更新は、既存の名前が非準拠でも通ること。**
//
// frontend は名前を編集していなくても `name` を必ず送る
// (`custom-emojis-manager.local.list.vue` は `name: item.name`)。ここで
// pattern を掛けると、**既に非準拠な名前で保存されている絵文字のカテゴリや
// ライセンスが編集できなくなる**。そういう行は **TS から引き継いだ DB に残りうる**
// (upstream の `update` は `id` 経路で pattern を掛けない)。`EmojiCopy` は #2998 で
// 塞いだので、mk-go が新しく作る経路はもう無い。
func TestEmojiUpdate_KeepsNonConformingNameEditable(t *testing.T) {
	h, emojiRepo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "foo-bar.baz", Category: func() *string { v := "old"; return &v }()})

	body, err := json.Marshal(map[string]any{"id": "e1", "name": "foo-bar.baz", "category": "new"})
	require.NoError(t, err)
	rec := doPost(h.EmojiUpdate, string(body), adminUser)
	require.Equalf(t, http.StatusNoContent, rec.Code,
		"非準拠な名前を保ったままの更新が落ちた。カテゴリすら直せなくなる: %s", rec.Body.String())

	got, ferr := emojiRepo.FindByID("e1")
	require.NoError(t, ferr)
	assert.Equal(t, "foo-bar.baz", got.Name)
	require.NotNil(t, got.Category)
	assert.Equal(t, "new", *got.Category)
}

// 逆に、非準拠な名前へ**変える**のは拒む。
func TestEmojiUpdate_RejectsRenameToInvalid(t *testing.T) {
	h, emojiRepo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "foo-bar.baz"})

	body, err := json.Marshal(map[string]any{"id": "e1", "name": "still bad"})
	require.NoError(t, err)
	rec := doPost(h.EmojiUpdate, string(body), adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	got, ferr := emojiRepo.FindByID("e1")
	require.NoError(t, ferr)
	assert.Equal(t, "foo-bar.baz", got.Name, "拒んだのに保存されている")
}

// **制約外の名前をそのままコピーしない** (#2998)。
//
// リモート絵文字の `name` は相手サーバーが決める値で、upstream の
// `admin/emoji/add` が強制する `^[a-zA-Z0-9_]+$` を満たすとは限らない。そのまま
// コピーすると **MFM の `:name:` から参照できないローカル絵文字**ができ、同じ名前を
// `add` で作ろうとすると 400 で弾かれるので経路によって結果が変わる。ケースは本番の
// 実測にあった 3 つの形 (記号のみ / `@host` 付き / ハイフン) をそのまま並べてある。
//
// **画像を取りに行く前に弾く。** 後から弾くと、その都度リモートへ 1 往復して
// drive ファイルを作っては消すことになる。
func TestEmojiCopy_RejectsInvalidRemoteName(t *testing.T) {
	for _, name := range []string{"+_+", "ablobcatnodmeltcry@3.5mbps.net", "mikan_8-2"} {
		t.Run(name, func(t *testing.T) {
			remoteHost := "remote.example"
			h, repo := setupEmojiHandler(t, &model.Emoji{
				ID: "src1", Name: name, Host: &remoteHost,
				OriginalURL: "https://remote.example/emoji/x.png",
			})
			fetcher := &fakeEmojiImageFetcher{returnDF: &model.DriveFile{
				ID: "df1", URL: "https://local.example/files/x.png", Type: "image/png",
			}}
			h.SetEmojiImageFetcher(fetcher)

			rec := doPost(h.EmojiCopy, `{"emojiId":"src1"}`, adminUser)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
			assert.Empty(t, fetcher.calls, "弾くなら画像を取りに行かないこと")
			_, err := repo.FindByNameAndHost(name, nil)
			assert.Error(t, err, "ローカル絵文字が作られていないこと")
		})
	}
}

// **`name` を上書きすれば制約外の絵文字も取り込める** (#2998)。
//
// 弾くだけだと取り込む手段が無くなる (`admin/emoji/add` は URL からの取得をしない)
// ので、`category` などと同じ additive なパラメータで人が決められるようにしてある。
// **drive ファイル名も新しい名前になること**まで見る — 元の名前のままだと、drive を
// 開いたときに絵文字と対応が取れない。
func TestEmojiCopy_NameOverrideAllowsInvalidRemoteName(t *testing.T) {
	remoteHost := "remote.example"
	h, repo := setupEmojiHandler(t, &model.Emoji{
		ID: "src1", Name: "+_+", Host: &remoteHost,
		OriginalURL: "https://remote.example/emoji/x.png",
	})
	fetcher := &fakeEmojiImageFetcher{returnDF: &model.DriveFile{
		ID: "df1", URL: "https://local.example/files/x.png", Type: "image/png",
	}}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiCopy, `{"emojiId":"src1","name":"plus_eyes"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	copied := findEmojiByID(t, repo, body["id"].(string))
	assert.Equal(t, "plus_eyes", copied.Name)
	require.Len(t, fetcher.calls, 1)
	assert.Equal(t, "plus_eyes", fetcher.calls[0].Name, "drive ファイル名も上書き後の名前にすること")
}

// **上書きした名前も検証する** (#2998)。src 側が正当でも、上書きが制約外なら弾く。
func TestEmojiCopy_RejectsInvalidNameOverride(t *testing.T) {
	remoteHost := "remote.example"
	h, _ := setupEmojiHandler(t, &model.Emoji{
		ID: "src1", Name: "valid_name", Host: &remoteHost,
		OriginalURL: "https://remote.example/emoji/x.png",
	})
	fetcher := &fakeEmojiImageFetcher{returnDF: &model.DriveFile{
		ID: "df1", URL: "https://local.example/files/x.png", Type: "image/png",
	}}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiCopy, `{"emojiId":"src1","name":"bad-name"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, fetcher.calls)
}

// **重複チェックは上書き後の名前で行う** (#2998)。
//
// src の名前で見ていると、上書き先が既存のローカル絵文字と衝突しても通ってしまう。
// **一意制約は救ってくれない** — `IDX_emoji_name_host` は `(name, host)` で、ローカルは
// `host IS NULL`。PostgreSQL の既定 (NULLS DISTINCT) では NULL 同士が重複と見なされない
// ので、`:name:` が 2 行できて MFM の解決もピッカーも二重になる (upstream が
// `checkDuplicate` をアプリ側に持っているのはこのため)。
func TestEmojiCopy_DuplicateCheckUsesOverriddenName(t *testing.T) {
	remoteHost := "remote.example"
	h, _ := setupEmojiHandler(t,
		&model.Emoji{ID: "src1", Name: "free", Host: &remoteHost, OriginalURL: "https://remote.example/e.png"},
		&model.Emoji{ID: "local1", Name: "taken"},
	)
	fetcher := &fakeEmojiImageFetcher{returnDF: &model.DriveFile{
		ID: "df1", URL: "https://local.example/files/x.png", Type: "image/png",
	}}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiCopy, `{"emojiId":"src1","name":"taken"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "DUPLICATE_NAME")
	assert.Empty(t, fetcher.calls, "重複が分かっているなら画像を取りに行かないこと")
}

// **`Create` が失敗したら取り込んだ drive ファイルを片付ける** (#2998)。
//
// 残すと誰からも参照されない孤児になる。`DriveFileRepository.DeleteOrphans` の
// 対象ではあるが自動では走らないので、掃除するまで実体ストレージを食い続ける。
// 承認経路 (`emoji_application.go`) は #2966 で同じ形にしてある。
func TestEmojiCopy_CleansUpDriveFileWhenCreateFails(t *testing.T) {
	remoteHost := "remote.example"
	h, repo := setupEmojiHandler(t, &model.Emoji{
		ID: "src1", Name: "ok_name", Host: &remoteHost,
		OriginalURL: "https://remote.example/emoji/x.png",
	})
	h.SetEmojiRepo(&failingCreateEmojiRepo{MockEmojiRepository: repo})
	fetcher := &fakeEmojiImageFetcher{returnDF: &model.DriveFile{
		ID: "df1", URL: "https://local.example/files/x.png", Type: "image/png",
	}}
	h.SetEmojiImageFetcher(fetcher)

	rec := doPost(h.EmojiCopy, `{"emojiId":"src1"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, []string{"df1"}, fetcher.deletedIDs, "取り込んだ drive ファイルを消すこと")
}

// **取り込んだものの MIME を見る** (#2998)。
//
// 承認経路は #2966 で既に見ているので、こちらだけ無検査だと**承認で弾かれるものが
// copy なら通る**という迂回路になる。相手が `originalUrl` に非画像を置いたときの形。
// **弾いたものを片付けること**まで見る — その時点で誰からも参照されない孤児になる。
func TestEmojiCopy_RejectsUnsupportedFileType(t *testing.T) {
	for _, tc := range []struct {
		name string
		mime string
	}{
		{"非画像", "application/zip"},
		// upstream の `FILE_TYPE_IMAGE.includes("")` は false。drive 側が type を
		// 埋められなかった行をそのまま絵文字にしない。
		{"空の MIME", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remoteHost := "remote.example"
			h, repo := setupEmojiHandler(t, &model.Emoji{
				ID: "src1", Name: "not_an_image", Host: &remoteHost,
				OriginalURL: "https://remote.example/emoji/x.bin",
			})
			fetcher := &fakeEmojiImageFetcher{returnDF: &model.DriveFile{
				ID: "df1", URL: "https://local.example/files/x.bin", Type: tc.mime,
			}}
			h.SetEmojiImageFetcher(fetcher)

			rec := doPost(h.EmojiCopy, `{"emojiId":"src1"}`, adminUser)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "UNSUPPORTED_FILE_TYPE")
			assert.Equal(t, []string{"df1"}, fetcher.deletedIDs, "弾いたものを片付けること")
			_, err := repo.FindByNameAndHost("not_an_image", nil)
			assert.Error(t, err, "ローカル絵文字が作られていないこと")
		})
	}
}

// **名前の長さも見る** (#2998)。
//
// `^[a-zA-Z0-9_]+$` は文字種しか縛らないので、`emoji.name` varchar(128) を
// 超える名前が素通りすると `Create` が SQLSTATE 22001 で落ちる。**弾くのは
// リモートへ問い合わせる前**でないと、1 往復して drive ファイルを作った後に
// 500 を返すことになる。空文字は `+` 量化子が弾く。
func TestEmojiCopy_RejectsNameOverrideByLength(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override string
	}{
		{"空文字", ""},
		{"128 文字ちょうどは通る", strings.Repeat("a", 128)},
		{"129 文字は弾く", strings.Repeat("a", 129)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remoteHost := "remote.example"
			h, _ := setupEmojiHandler(t, &model.Emoji{
				ID: "src1", Name: "src_name", Host: &remoteHost,
				OriginalURL: "https://remote.example/emoji/x.png",
			})
			fetcher := &fakeEmojiImageFetcher{returnDF: &model.DriveFile{
				ID: "df1", URL: "https://local.example/files/x.png", Type: "image/png",
			}}
			h.SetEmojiImageFetcher(fetcher)

			body, err := json.Marshal(map[string]any{"emojiId": "src1", "name": tc.override})
			require.NoError(t, err)
			rec := doPost(h.EmojiCopy, string(body), adminUser)
			if len(tc.override) == 128 {
				require.Equal(t, http.StatusOK, rec.Code)
				return
			}
			require.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Empty(t, fetcher.calls, "弾くなら画像を取りに行かないこと")
		})
	}
}

// **`admin/emoji/add` も名前の長さで 5xx を立てない** (#2998)。
//
// upstream の paramDef に `maxLength` は無いので、128 文字を超える名前は
// `emoji.name` varchar(128) に入らず SQLSTATE 22001 が生のまま 500 になる。
// `copy` と申請経路が 400 で弾く以上、**同じ入力が endpoint 次第で 400 と 500 に
// 分かれる**のは筋が通らない。
func TestEmojiAdd_RejectsOverlongName(t *testing.T) {
	h, _ := setupEmojiHandler(t)
	body, err := json.Marshal(map[string]any{"name": strings.Repeat("a", 129), "url": "https://local.example/x.png"})
	require.NoError(t, err)
	rec := doPost(h.EmojiAdd, string(body), adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")

	ok, err := json.Marshal(map[string]any{"name": strings.Repeat("a", 128), "url": "https://local.example/x.png"})
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, doPost(h.EmojiAdd, string(ok), adminUser).Code, "128 文字ちょうどは通ること")
}

// **素のモデレーターには IP を返さないこと。**
//
// #3114 が `admin/show-user` の signin IP に入れたのと同じ条件。同じ種類の
// 情報 (ローカル利用者の接続元 IP) を素のモデレーター権限だけで全件返す口が
// ここに残ると、policy が門として成立しない。`admin/drive/files` に userId を
// 渡してファイル ID を列挙すれば、アップロード元 IP の履歴がそのまま取れる。
func TestDriveShowFile_RequestIPRequiresPolicy(t *testing.T) {
	h, _, _, roleRepo, assignRepo := newTestHandlerWithAssign(t)
	// policy を持たない素のモデレーター。
	roleRepo.Roles["r_mod"] = &model.Role{ID: "r_mod", Name: "Mod", IsModerator: true}
	assignRepo.Assignments["mod9:r_mod"] = &model.RoleAssignment{
		ID: "ra_plain", UserID: "mod9", RoleID: "r_mod",
	}

	repo := testutil.NewMockDriveFileRepository()
	owner := "u_owner"
	ip := "203.0.113.7"
	headers := datatypes.JSON([]byte(`{"x-real-ip":"203.0.113.7"}`))
	require.NoError(t, repo.Create(&model.DriveFile{
		ID:             "d_policy",
		UserID:         &owner,
		Name:           "f.png",
		Type:           "image/png",
		RequestIP:      &ip,
		RequestHeaders: headers,
	}))
	h.SetDriveFileRepo(repo)

	rec := doPost(h.DriveShowFile, `{"fileId":"d_policy"}`, &model.User{ID: "mod9"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, resp["requestIp"],
		"policy を持たないモデレーターには requestIp を返さないこと")
	assert.Nil(t, resp["requestHeaders"],
		"**ヘッダにも接続元 IP が載る** (本番の nginx は x-real-ip を必ず付ける) ので同じ条件で伏せること")
}
