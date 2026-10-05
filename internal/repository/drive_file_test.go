package repository

import (
	"context"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func cleanupDriveFile(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "drive_file" WHERE id = ?`, id)
}

func newTestDriveFile(id, userID, md5 string, folderID *string) *model.DriveFile {
	uid := userID
	return &model.DriveFile{
		ID:             id,
		UserID:         &uid,
		MD5:            md5,
		Name:           "test.bin",
		Type:           "application/octet-stream",
		Size:           10,
		StoredInternal: true,
		URL:            "http://example.com/files/" + id,
		Properties:     datatypes.JSON([]byte("{}")),
		RequestHeaders: datatypes.JSON([]byte("{}")),
		FolderID:       folderID,
	}
}

func TestDriveFileRepository_CRUD(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	user := insertTestUser(t, "u_df_1", "dfu1")
	defer cleanupUser(t, user.ID)

	f := newTestDriveFile("f1", user.ID, "abc123", nil)
	require.NoError(t, repo.Create(f))
	defer cleanupDriveFile(t, f.ID)

	got, err := repo.FindByID("f1")
	require.NoError(t, err)
	assert.Equal(t, "test.bin", got.Name)

	require.NoError(t, repo.Update("f1", map[string]any{"name": "renamed.bin"}))
	got, _ = repo.FindByID("f1")
	assert.Equal(t, "renamed.bin", got.Name)

	require.NoError(t, repo.Update("f1", map[string]any{}))

	require.NoError(t, repo.Delete(f))
	_, err = repo.FindByID("f1")
	assert.Error(t, err)
}

func TestDriveFileRepository_FindByMD5(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	user := insertTestUser(t, "u_df_2", "dfu2")
	defer cleanupUser(t, user.ID)

	f1 := newTestDriveFile("f_md5_1", user.ID, "hash", nil)
	f2 := newTestDriveFile("f_md5_2", user.ID, "hash", nil)
	require.NoError(t, repo.Create(f1))
	require.NoError(t, repo.Create(f2))
	defer cleanupDriveFile(t, f1.ID)
	defer cleanupDriveFile(t, f2.ID)

	got, err := repo.FindByMD5(user.ID, "hash")
	require.NoError(t, err)
	// 最新 (id降順) を返す
	assert.Equal(t, "f_md5_2", got.ID)
}

func TestDriveFileRepository_ListByUser(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	folderRepo := NewDriveFolderRepository(testDB)
	user := insertTestUser(t, "u_df_3", "dfu3")
	defer cleanupUser(t, user.ID)

	uid := user.ID
	folder := &model.DriveFolder{ID: "fold_1", Name: "F", UserID: &uid}
	require.NoError(t, folderRepo.Create(folder))
	defer testDB.Exec(`DELETE FROM "drive_folder" WHERE id = ?`, folder.ID)

	folderID := "fold_1"
	root := newTestDriveFile("f_lst_1", user.ID, "h1", nil)
	infolder := newTestDriveFile("f_lst_2", user.ID, "h2", &folderID)
	require.NoError(t, repo.Create(root))
	require.NoError(t, repo.Create(infolder))
	defer cleanupDriveFile(t, root.ID)
	defer cleanupDriveFile(t, infolder.ID)

	rows, err := repo.ListByUser(user.ID, nil, false, "", "", "", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "f_lst_1", rows[0].ID)

	rows, err = repo.ListByUser(user.ID, &folderID, false, "", "", "", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "f_lst_2", rows[0].ID)

	// untilID/sinceIDの分岐を踏む (root のみ)
	rows, err = repo.ListByUser(user.ID, nil, false, "", "", "z", "", 10)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	rows, err = repo.ListByUser(user.ID, nil, false, "", "", "", "a", 10)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

func TestDriveFileRepository_ListByUser_TypeSortAnyFolder(t *testing.T) {
	// #1564: type filter (完全一致 / "/*" prefix)、sort、anyFolder の分岐。
	repo := NewDriveFileRepository(testDB)
	user := insertTestUser(t, "u_df_ts", "dfts")
	defer cleanupUser(t, user.ID)

	folderRepo := NewDriveFolderRepository(testDB)
	uid := user.ID
	folder := &model.DriveFolder{ID: "fold_ts", Name: "F", UserID: &uid}
	require.NoError(t, folderRepo.Create(folder))
	defer testDB.Exec(`DELETE FROM "drive_folder" WHERE id = ?`, folder.ID)
	folderID := folder.ID

	png := newTestDriveFile("f_ts_1", user.ID, "t1", nil)
	png.Type = "image/png"
	png.Name = "bbb.png"
	png.Size = 10
	jpg := newTestDriveFile("f_ts_2", user.ID, "t2", &folderID)
	jpg.Type = "image/jpeg"
	jpg.Name = "aaa.jpg"
	jpg.Size = 30
	mp4 := newTestDriveFile("f_ts_3", user.ID, "t3", nil)
	mp4.Type = "video/mp4"
	mp4.Name = "ccc.mp4"
	mp4.Size = 20
	for _, f := range []*model.DriveFile{png, jpg, mp4} {
		require.NoError(t, repo.Create(f))
		defer cleanupDriveFile(t, f.ID)
	}

	// anyFolder=true は folder 条件なし (サブフォルダの jpg も入る)
	rows, err := repo.ListByUser(user.ID, nil, true, "", "", "", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 3)

	// type prefix ("image/*") + anyFolder
	rows, err = repo.ListByUser(user.ID, nil, true, "image/*", "", "", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	// type 完全一致
	rows, err = repo.ListByUser(user.ID, nil, true, "video/mp4", "", "", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, mp4.ID, rows[0].ID)

	// sort 各種 (anyFolder で全件対象)
	for _, tc := range []struct {
		sort  string
		first string
	}{
		{"+createdAt", mp4.ID}, // id DESC
		{"-createdAt", png.ID}, // id ASC
		{"+name", mp4.ID},      // name DESC (ccc)
		{"-name", jpg.ID},      // name ASC (aaa)
		{"+size", jpg.ID},      // size DESC (30)
		{"-size", png.ID},      // size ASC (10)
	} {
		rows, err = repo.ListByUser(user.ID, nil, true, "", tc.sort, "", "", 10)
		require.NoError(t, err, tc.sort)
		require.Len(t, rows, 3, tc.sort)
		assert.Equal(t, tc.first, rows[0].ID, tc.sort)
	}
}

func TestDriveFileRepository_FindAllByMD5(t *testing.T) {
	// #1564: find-by-hash の一致全件 (id ASC)。
	repo := NewDriveFileRepository(testDB)
	user := insertTestUser(t, "u_df_am", "dfam")
	defer cleanupUser(t, user.ID)

	f1 := newTestDriveFile("f_am_1", user.ID, "samehash", nil)
	f2 := newTestDriveFile("f_am_2", user.ID, "samehash", nil)
	f3 := newTestDriveFile("f_am_3", user.ID, "otherhash", nil)
	for _, f := range []*model.DriveFile{f1, f2, f3} {
		require.NoError(t, repo.Create(f))
		defer cleanupDriveFile(t, f.ID)
	}

	got, err := repo.FindAllByMD5(user.ID, "samehash")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "f_am_1", got[0].ID)
	assert.Equal(t, "f_am_2", got[1].ID)

	got, err = repo.FindAllByMD5(user.ID, "ghost")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestDriveFileRepository_FindByAnyURL(t *testing.T) {
	// #1564: drive/files/show の url anyOf 検索 (url/webpublicUrl/thumbnailUrl)。
	repo := NewDriveFileRepository(testDB)
	user := insertTestUser(t, "u_df_au", "dfau")
	defer cleanupUser(t, user.ID)

	web := "http://example.com/files/f_au_web"
	thumb := "http://example.com/files/f_au_thumb"
	f := newTestDriveFile("f_au", user.ID, "aumd5", nil)
	f.WebpublicURL = &web
	f.ThumbnailURL = &thumb
	require.NoError(t, repo.Create(f))
	defer cleanupDriveFile(t, f.ID)

	for _, u := range []string{f.URL, web, thumb} {
		got, err := repo.FindByAnyURL(u)
		require.NoError(t, err, u)
		assert.Equal(t, f.ID, got.ID)
	}
	_, err := repo.FindByAnyURL("")
	assert.Error(t, err)
	_, err = repo.FindByAnyURL("http://example.com/none")
	assert.Error(t, err)
}

// TestDriveFileRepository_URLIndexes verifies that the #1625 url-column
// indexes (migration 000059-000061) are created and actually usable for the
// FindByAnyURL OR-equality query. testutil.ApplyMigrations は migration の
// エラーを swallow するため、これらが落ちても CI は沈黙し drive_file 肥大に
// 比例して静かに性能退行する。000054/000055 の GIN index guard
// (TestNoteRepository_MentionsFileIdsGINIndexes) と同じ regression guard。
func TestDriveFileRepository_URLIndexes(t *testing.T) {
	// 1. index が存在すること (CONCURRENTLY migration が適用されたことの確認
	//    も兼ねる)。nullable 列の 2 本は部分 index (WHERE IS NOT NULL) で
	//    あることも固定する。
	for _, tc := range []struct {
		index   string
		partial bool
	}{
		{"IDX_drive_file_url", false},
		{"IDX_drive_file_webpublicUrl", true},
		{"IDX_drive_file_thumbnailUrl", true},
	} {
		indexdef := indexDef(t, "drive_file", tc.index)
		require.NotEmpty(t, indexdef, "index %s must exist (migration applied)", tc.index)
		if tc.partial {
			assert.Contains(t, indexdef, "IS NOT NULL", "%s must be a partial index", tc.index)
		}
	}

	// 2. planner が各列の equality に index を選べること。小さな test テーブル
	//    では planner が seq scan を選ぶため、同一接続上で enable_seqscan=off
	//    を効かせて index 経路を強制し、plan に index 名が現れることを確認する
	//    (index が無ければ強制しても seq scan のまま)。SET LOCAL は transaction
	//    スコープなので Begin/Rollback で閉じる。
	checkPlanUsesIndex := func(query, arg, indexName string) {
		tx := testDB.Begin()
		defer tx.Rollback()
		require.NoError(t, tx.Exec(`SET LOCAL enable_seqscan = off`).Error)
		var lines []string
		require.NoError(t, tx.Raw(`EXPLAIN `+query, arg).Scan(&lines).Error)
		plan := strings.Join(lines, "\n")
		assert.Contains(t, plan, indexName,
			"equality query should use %s under enable_seqscan=off; plan was:\n%s", indexName, plan)
	}
	checkPlanUsesIndex(
		`SELECT * FROM "drive_file" WHERE "url" = ?`, "http://example.com/x", "IDX_drive_file_url")
	checkPlanUsesIndex(
		`SELECT * FROM "drive_file" WHERE "webpublicUrl" = ?`, "http://example.com/x", "IDX_drive_file_webpublicUrl")
	checkPlanUsesIndex(
		`SELECT * FROM "drive_file" WHERE "thumbnailUrl" = ?`, "http://example.com/x", "IDX_drive_file_thumbnailUrl")
}

func TestDriveFileRepository_FindByAnyAccessKey(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	user := insertTestUser(t, "u_df_ak", "dfak")
	defer cleanupUser(t, user.ID)

	primary := "pk-" + user.ID
	thumb := "tk-" + user.ID
	web := "wk-" + user.ID
	f := newTestDriveFile("f_ak", user.ID, "akmd5", nil)
	f.AccessKey = &primary
	f.ThumbnailAccessKey = &thumb
	f.WebpublicAccessKey = &web
	require.NoError(t, repo.Create(f))
	defer cleanupDriveFile(t, f.ID)

	// primary / thumbnail / webpublic いずれの key でも同じ行に解決する
	for _, k := range []string{primary, thumb, web} {
		got, err := repo.FindByAnyAccessKey(k)
		require.NoError(t, err, "lookup %s", k)
		assert.Equal(t, f.ID, got.ID)
	}

	// 空文字 / 未マッチは ErrRecordNotFound
	_, err := repo.FindByAnyAccessKey("")
	assert.Error(t, err)
	_, err = repo.FindByAnyAccessKey("does-not-exist")
	assert.Error(t, err)
}

func TestDriveFileRepository_QueryErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewDriveFileRepository(testDB.WithContext(ctx))

	err := repo.Create(&model.DriveFile{ID: "x"})
	assert.Error(t, err)
	_, err = repo.FindByID("a")
	assert.Error(t, err)
	_, err = repo.FindByMD5("a", "b")
	assert.Error(t, err)
	err = repo.Update("a", map[string]any{"name": "b"})
	assert.Error(t, err)
	_, err = repo.ListByUser("a", nil, false, "", "", "", "", 10)
	assert.Error(t, err)
}

func TestDriveFileRepository_ListForAdmin(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	user := insertTestUser(t, "u_admin_list", "adlf")
	defer cleanupUser(t, user.ID)

	localFile := newTestDriveFile("adl_local", user.ID, "md5local", nil)
	remoteHost := "remote.example"
	remoteFile := newTestDriveFile("adl_remote", user.ID, "md5remote", nil)
	remoteFile.UserHost = &remoteHost
	remoteFile.Type = "image/png"
	videoFile := newTestDriveFile("adl_video", user.ID, "md5video", nil)
	videoFile.Type = "video/mp4"
	// **owner 無しのリモート添付** (#2717 / #2340 由来)。#2753 で @system 一覧
	// から外したので、こちらに出ることが「消せないし見えない」を防ぐ唯一の
	// 保証になる。origin=remote は userHost で絞るので出る。
	remoteUnowned := newTestDriveFile("adl_ru", user.ID, "md5adlru", nil)
	remoteUnowned.UserID = nil
	remoteUnowned.UserHost = &remoteHost
	remoteUnowned.Type = "image/png"
	require.NoError(t, repo.Create(localFile))
	require.NoError(t, repo.Create(remoteFile))
	require.NoError(t, repo.Create(videoFile))
	require.NoError(t, repo.Create(remoteUnowned))
	defer cleanupDriveFile(t, remoteUnowned.ID)
	defer cleanupDriveFile(t, localFile.ID)
	defer cleanupDriveFile(t, remoteFile.ID)
	defer cleanupDriveFile(t, videoFile.ID)

	// origin = remote → only the remote-host file
	rows, err := repo.ListForAdmin("", "remote", "", "", "", "", 10)
	require.NoError(t, err)
	ids := make(map[string]bool)
	for _, r := range rows {
		ids[r.ID] = true
	}
	assert.True(t, ids[remoteFile.ID])
	assert.False(t, ids[localFile.ID])
	assert.True(t, ids[remoteUnowned.ID],
		"owner 無しのリモート添付も origin=remote には出る (#2753 で @system から外した分の受け皿)")

	// #1772: type=image/* は prefix LIKE → image/png にマッチ。
	rows, err = repo.ListForAdmin("", "", "", "image/*", "", "", 10)
	require.NoError(t, err)
	ids = make(map[string]bool)
	for _, r := range rows {
		ids[r.ID] = true
	}
	assert.True(t, ids[remoteFile.ID])
	assert.False(t, ids[videoFile.ID])

	// #1772: type=image/png は完全一致 → image/png にマッチ、video/mp4 は不一致。
	rows, err = repo.ListForAdmin("", "", "", "image/png", "", "", 10)
	require.NoError(t, err)
	ids = make(map[string]bool)
	for _, r := range rows {
		ids[r.ID] = true
	}
	assert.True(t, ids[remoteFile.ID])
	assert.False(t, ids[videoFile.ID])

	// #1772: wildcard 無しの "image/" は完全一致扱いなので image/png に当たらない
	// (旧 prefix LIKE 挙動が消えたことの回帰 guard)。
	rows, err = repo.ListForAdmin("", "", "", "image/", "", "", 10)
	require.NoError(t, err)
	ids = make(map[string]bool)
	for _, r := range rows {
		ids[r.ID] = true
	}
	assert.False(t, ids[remoteFile.ID], `"image/" は完全一致で image/png に当たらない`)

	// host exact match
	rows, err = repo.ListForAdmin("", "", remoteHost, "", "", "", 10)
	require.NoError(t, err)
	found := false
	for _, r := range rows {
		if r.ID == remoteFile.ID {
			found = true
		}
	}
	assert.True(t, found)

	// userId narrows to that user's files only and ignores origin/host (#471)
	otherUser := insertTestUser(t, "u_adm_lst2", "adlf2")
	defer cleanupUser(t, otherUser.ID)
	otherFile := newTestDriveFile("adl_other", otherUser.ID, "md5other", nil)
	require.NoError(t, repo.Create(otherFile))
	defer cleanupDriveFile(t, otherFile.ID)
	rows, err = repo.ListForAdmin(user.ID, "remote", "", "", "", "", 10)
	require.NoError(t, err)
	for _, r := range rows {
		require.NotNil(t, r.UserID)
		assert.Equal(t, user.ID, *r.UserID)
	}

	// default limit / clamp
	_, err = repo.ListForAdmin("", "", "", "", "", "", 0)
	require.NoError(t, err)
	_, err = repo.ListForAdmin("", "", "", "", "", "", 1000)
	require.NoError(t, err)
}

func TestDriveFileRepository_ListSystemFiles(t *testing.T) {
	// #686: local な system file (userId IS NULL かつ userHost IS NULL。custom
	// emoji copy / import zip 経路で蓄積される) を一覧する経路。type prefix と
	// pagination が効くこと、user 所有 / remote 所有 / **owner 無しのリモート**
	// (#2753) のいずれも混ざらないことを保証する。
	repo := NewDriveFileRepository(testDB)
	user := insertTestUser(t, "u_sys_lst", "syslst")
	defer cleanupUser(t, user.ID)

	// system file 2 件 (image/png + application/zip)、user 所有 1 件、remote 所有 1 件
	sysImg := newTestDriveFile("sys_img1", user.ID, "md5sysimg", nil)
	sysImg.UserID = nil
	sysImg.Type = "image/png"
	sysZip := newTestDriveFile("sys_zip1", user.ID, "md5syszip", nil)
	sysZip.UserID = nil
	sysZip.Type = "application/zip"
	userFile := newTestDriveFile("u_sys_uf", user.ID, "md5user", nil)
	userFile.Type = "image/png"
	remoteHost := "remote.example"
	remoteFile := newTestDriveFile("u_sys_rf", user.ID, "md5remote", nil)
	remoteFile.UserHost = &remoteHost
	remoteFile.Type = "image/png"
	// **owner 無しのリモート添付** (#2717 / #2340 由来)。userId IS NULL だけで
	// 絞ると混ざり、admin UI から「system file」として消せてしまう (#2753)。
	remoteUnowned := newTestDriveFile("u_sys_ru", user.ID, "md5remoteunowned", nil)
	remoteUnowned.UserID = nil
	remoteUnowned.UserHost = &remoteHost
	remoteUnowned.Type = "image/png"
	require.NoError(t, repo.Create(sysImg))
	require.NoError(t, repo.Create(sysZip))
	require.NoError(t, repo.Create(userFile))
	require.NoError(t, repo.Create(remoteFile))
	require.NoError(t, repo.Create(remoteUnowned))
	defer cleanupDriveFile(t, remoteUnowned.ID)
	defer cleanupDriveFile(t, sysImg.ID)
	defer cleanupDriveFile(t, sysZip.ID)
	defer cleanupDriveFile(t, userFile.ID)
	defer cleanupDriveFile(t, remoteFile.ID)

	// type 無指定: system file 2 件のみ
	rows, err := repo.ListSystemFiles("", "", "", 10)
	require.NoError(t, err)
	ids := make(map[string]bool, len(rows))
	for _, r := range rows {
		ids[r.ID] = true
	}
	assert.True(t, ids[sysImg.ID])
	assert.True(t, ids[sysZip.ID])
	assert.False(t, ids[userFile.ID], "user-owned file must not appear")
	assert.False(t, ids[remoteFile.ID], "remote-owned file must not appear")
	assert.False(t, ids[remoteUnowned.ID],
		"owner 無しのリモート添付は system file として出さない (#2753)")

	// #1772: type=image/* (wildcard) で絞り込み: 本 test の fixture では sysImg のみ
	// image/* に該当。他 test が UserID=NULL + Type=image/* な system file 行を残しても
	// 本 test を巻き込まないよう、 fixture ID で scoped assert する (#1098)。
	rows, err = repo.ListSystemFiles("image/*", "", "", 10)
	require.NoError(t, err)
	ids = make(map[string]bool, len(rows))
	for _, r := range rows {
		ids[r.ID] = true
	}
	assert.True(t, ids[sysImg.ID], "sysImg (image/png) should appear in image/* filter")
	assert.False(t, ids[sysZip.ID], "sysZip (application/zip) must not appear")
	assert.False(t, ids[userFile.ID], "user-owned file must not appear")
	assert.False(t, ids[remoteFile.ID], "remote-owned file must not appear")
	assert.False(t, ids[remoteUnowned.ID],
		"owner 無しのリモート添付は wildcard 経路でも出さない (#2753)")

	// limit 範囲外 (0 / 過大) でも error にならず default / clamp が効く
	_, err = repo.ListSystemFiles("", "", "", 0)
	require.NoError(t, err)
	_, err = repo.ListSystemFiles("", "", "", 1000)
	require.NoError(t, err)

	// pagination untilId: id < untilId (exclusive boundary)。sysZip より前
	// なら sysImg が出て sysZip は出ない。
	rows, err = repo.ListSystemFiles("", sysZip.ID, "", 10)
	require.NoError(t, err)
	gotIDs := make(map[string]bool, len(rows))
	for _, r := range rows {
		gotIDs[r.ID] = true
	}
	assert.True(t, gotIDs[sysImg.ID], "sysImg has smaller ID, should be included")
	assert.False(t, gotIDs[sysZip.ID], "sysZip is the cutoff and should be excluded")

	// pagination sinceId: id > sinceId (exclusive boundary)。sysImg より後
	// なら sysZip が出て sysImg は出ない。境界の semantics と sort 方向が
	// untilId 経路と対称であることを保証する。
	rows, err = repo.ListSystemFiles("", "", sysImg.ID, 10)
	require.NoError(t, err)
	gotIDs = make(map[string]bool, len(rows))
	for _, r := range rows {
		gotIDs[r.ID] = true
	}
	assert.True(t, gotIDs[sysZip.ID], "sysZip has larger ID, should be included")
	assert.False(t, gotIDs[sysImg.ID], "sysImg is the cutoff and should be excluded")
}

func TestDriveFileRepository_DeleteOrphansAndRemoteCache(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	user := insertTestUser(t, "u_del_src", "dels")
	defer cleanupUser(t, user.ID)

	// 孤立ファイル (userId NULL) と userId ありファイル
	orphan := newTestDriveFile("orph1", user.ID, "md5o", nil)
	orphan.UserID = nil
	kept := newTestDriveFile("kept1", user.ID, "md5k", nil)

	// cached remote file (isLink=false, userHost set) は ExpireRemoteCache の
	// 対象。link-only proxy (isLink=true) は対象外で保持される (upstream 互換)。
	host := "cache.example"
	remoteCache := newTestDriveFile("rc1", user.ID, "md5rc", nil)
	remoteCache.IsLink = false
	remoteCache.UserHost = &host
	rcURI := "https://cache.example/files/rc1"
	remoteCache.URI = &rcURI
	remoteCache.Size = 4096
	rcKey := "rc1-access-key"
	remoteCache.AccessKey = &rcKey
	linkOnly := newTestDriveFile("lo1", user.ID, "md5lo", nil)
	linkOnly.IsLink = true
	linkOnly.UserHost = &host

	require.NoError(t, repo.Create(orphan))
	require.NoError(t, repo.Create(kept))
	require.NoError(t, repo.Create(remoteCache))
	require.NoError(t, repo.Create(linkOnly))
	defer cleanupDriveFile(t, orphan.ID)
	defer cleanupDriveFile(t, kept.ID)
	defer cleanupDriveFile(t, remoteCache.ID)
	defer cleanupDriveFile(t, linkOnly.ID)

	n, err := repo.DeleteOrphans()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(1))
	_, err = repo.FindByID(orphan.ID)
	assert.Error(t, err)
	_, err = repo.FindByID(kept.ID)
	assert.NoError(t, err)

	n, err = repo.ExpireRemoteCache()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(1))

	// **行は消さず link に倒す** (#3102)。消すと `note.fileIds` の指す先が
	// 無くなり、過去の投稿から添付が黙って消える (upstream は残す)。
	got, err := repo.FindByID(remoteCache.ID)
	require.NoError(t, err, "キャッシュ実体の行は残る")
	assert.True(t, got.IsLink, "link に倒っていない")
	assert.Equal(t, rcURI, got.URL, "url が uri になっていない")
	assert.False(t, got.StoredInternal)
	// mk-go が普段作る link 行と同じ形にする (size 0 / access key なし)。
	assert.Equal(t, 0, got.Size, "size が残ると使用量が嘘になる")
	assert.Nil(t, got.AccessKey, "実体が消えた後も古い /files URL が生きている")
	assert.Nil(t, got.ThumbnailURL)
	assert.Nil(t, got.WebpublicURL)

	_, err = repo.FindByID(linkOnly.ID)
	assert.NoError(t, err, "isLink=true の link-only は保持される")

	// **2 回目は対象にならない。** 倒した行は isLink=true なので条件から外れる
	// (外れないとバッチが同じ行を返し続けて進まない)。
	again, err := repo.ExpireRemoteCache()
	require.NoError(t, err)
	assert.Equal(t, int64(0), again, "倒した行がまだ対象に残っている")
}

// **`uri` を持たない行は倒せないので消す** (#3102、upstream の else 枝と同じ)。
// link 行の `url` は `uri` から作るので、NULL のままでは表示できない行が残る。
func TestDriveFileRepository_ExpireRemoteCache_DeletesRowsWithoutURI(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	user := insertTestUser(t, "u_exp_nouri", "expn")
	defer cleanupUser(t, user.ID)

	host := "nouri.example"
	noURI := newTestDriveFile("nouri1", user.ID, "md5nu", nil)
	noURI.IsLink = false
	noURI.UserHost = &host
	noURI.URI = nil
	require.NoError(t, repo.Create(noURI))
	defer cleanupDriveFile(t, noURI.ID)

	n, err := repo.ExpireRemoteCache()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(1))
	_, err = repo.FindByID(noURI.ID)
	assert.Error(t, err, "uri が無い行は倒せないので消える")
}

// TestDriveFileRepository_DeleteOrphans_PreservesEmojiReferenced は #722
// regression guard: emoji.originalUrl / publicUrl が参照している system 所有
// drive_file は cleanup で巻き込まれずに保持されること。#670 で導入された
// emoji copy / import zip の保管先が cleanup 実行で吹き飛ぶ即死バグの再発
// 防止。
func TestDriveFileRepository_DeleteOrphans_PreservesEmojiReferenced(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	emojiRepo := NewEmojiRepository(testDB)
	user := insertTestUser(t, "u_orph_emoji", "orphemo")
	defer cleanupUser(t, user.ID)

	// orphan true (emoji 参照無し): 削除されるべき
	pureOrphan := newTestDriveFile("orph_pure_722", user.ID, "md5o", nil)
	pureOrphan.UserID = nil
	pureOrphan.URL = "http://test/orph_pure_722.bin"

	// emoji.originalUrl 参照あり (system file): 削除してはいけない
	emojiOriginalRef := newTestDriveFile("emoji_orig_722", user.ID, "md5eo", nil)
	emojiOriginalRef.UserID = nil
	emojiOriginalRef.URL = "http://test/emoji_orig_722.png"

	// emoji.publicUrl 参照あり (webpublic 経由系): 削除してはいけない
	emojiPublicRef := newTestDriveFile("emoji_pub_722", user.ID, "md5ep", nil)
	emojiPublicRef.UserID = nil
	emojiPublicRef.URL = "http://test/emoji_pub_722.png"

	require.NoError(t, repo.Create(pureOrphan))
	require.NoError(t, repo.Create(emojiOriginalRef))
	require.NoError(t, repo.Create(emojiPublicRef))
	defer cleanupDriveFile(t, pureOrphan.ID)
	defer cleanupDriveFile(t, emojiOriginalRef.ID)
	defer cleanupDriveFile(t, emojiPublicRef.ID)

	// 2 件 emoji 行を seed (1 件は originalUrl で参照、もう 1 件は publicUrl で参照)
	emoOrig := &model.Emoji{
		ID:          "e_orig_722",
		Name:        "guard_emoji_orig_722",
		OriginalURL: emojiOriginalRef.URL,
		PublicURL:   emojiOriginalRef.URL,
	}
	emoPub := &model.Emoji{
		ID:          "e_pub_722",
		Name:        "guard_emoji_pub_722",
		OriginalURL: "http://test/some_other_url.png", // originalUrl は別
		PublicURL:   emojiPublicRef.URL,               // publicUrl で参照
	}
	require.NoError(t, emojiRepo.Create(emoOrig))
	require.NoError(t, emojiRepo.Create(emoPub))
	defer testDB.Exec(`DELETE FROM "emoji" WHERE id IN (?, ?)`, emoOrig.ID, emoPub.ID)

	n, err := repo.DeleteOrphans()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(1), "pureOrphan should be counted in deleted rows")

	// pureOrphan は消えている
	_, err = repo.FindByID(pureOrphan.ID)
	assert.Error(t, err, "pure orphan must be deleted")

	// emojiOriginalRef / emojiPublicRef は保持されている
	_, err = repo.FindByID(emojiOriginalRef.ID)
	assert.NoError(t, err, "emoji.originalUrl 参照の system file は保持される")
	_, err = repo.FindByID(emojiPublicRef.ID)
	assert.NoError(t, err, "emoji.publicUrl 参照の system file は保持される")
}

// TestDriveFileRepository_ListOrphans は ListOrphans が orphanWhere どおりに
// 行を返すことを、userId / emoji の 2 次元と limit / 非破壊性について検証する
// (#1724 admin/drive/cleanup の storage 削除経路)。
// 選択条件の残り 1 次元 (userHost) は drive_file_limits_test.go の
// TestDriveFile_OrphanCleanupKeepsUnownedRemoteFiles が張る。
func TestDriveFileRepository_ListOrphans(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	emojiRepo := NewEmojiRepository(testDB)
	user := insertTestUser(t, "u_listorph", "listorph")
	defer cleanupUser(t, user.ID)

	pureOrphan := newTestDriveFile("lo_pure", user.ID, "lm5o", nil)
	pureOrphan.UserID = nil
	pureOrphan.URL = "http://test/lo_pure.bin"
	kept := newTestDriveFile("lo_kept", user.ID, "lm5k", nil)
	emojiRef := newTestDriveFile("lo_emoji", user.ID, "lm5e", nil)
	emojiRef.UserID = nil
	emojiRef.URL = "http://test/lo_emoji.png"

	require.NoError(t, repo.Create(pureOrphan))
	require.NoError(t, repo.Create(kept))
	require.NoError(t, repo.Create(emojiRef))
	defer cleanupDriveFile(t, pureOrphan.ID)
	defer cleanupDriveFile(t, kept.ID)
	defer cleanupDriveFile(t, emojiRef.ID)

	emo := &model.Emoji{
		ID:          "e_listorph",
		Name:        "guard_listorph",
		OriginalURL: emojiRef.URL,
		PublicURL:   emojiRef.URL,
	}
	require.NoError(t, emojiRepo.Create(emo))
	defer testDB.Exec(`DELETE FROM "emoji" WHERE id = ?`, emo.ID)

	rows, err := repo.ListOrphans(0)
	require.NoError(t, err)
	ids := make(map[string]bool, len(rows))
	for _, r := range rows {
		ids[r.ID] = true
	}
	assert.True(t, ids[pureOrphan.ID], "pure orphan は一覧に含まれる")
	assert.False(t, ids[kept.ID], "user 所有 file は除外される")
	assert.False(t, ids[emojiRef.ID], "emoji 参照 file は除外される")

	// limit を尊重し、かつ ListOrphans 自体は削除しない (べき等)。
	limited, err := repo.ListOrphans(1)
	require.NoError(t, err)
	assert.Len(t, limited, 1)
	_, err = repo.FindByID(pureOrphan.ID)
	assert.NoError(t, err, "ListOrphans は行を削除しない")
}

// --- 追加テスト (#260 repository coverage) ---

func TestDriveFileRepository_FindByIDs(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	seedUser(t, "df_ids_u1")
	f1 := newTestDriveFile("df_ids_1", "df_ids_u1", "m1", nil)
	f2 := newTestDriveFile("df_ids_2", "df_ids_u1", "m2", nil)
	require.NoError(t, repo.Create(f1))
	require.NoError(t, repo.Create(f2))
	t.Cleanup(func() { cleanupDriveFile(t, f1.ID); cleanupDriveFile(t, f2.ID) })

	files, err := repo.FindByIDs([]string{f1.ID, f2.ID, "missing"})
	require.NoError(t, err)
	assert.Len(t, files, 2)

	empty, err := repo.FindByIDs(nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestDriveFileRepository_FindByName(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	seedUser(t, "df_name_u1")
	f := newTestDriveFile("df_name_1", "df_name_u1", "mn", nil)
	f.Name = "uniq_name.png"
	require.NoError(t, repo.Create(f))
	t.Cleanup(func() { cleanupDriveFile(t, f.ID) })

	// folderID nil path (root)
	got, err := repo.FindByName("df_name_u1", "uniq_name.png", nil)
	require.NoError(t, err)
	assert.Len(t, got, 1)

	// folderID non-nil path (should not match because no folder set)
	other := "folder_dummy"
	got, err = repo.FindByName("df_name_u1", "uniq_name.png", &other)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestDriveFileRepository_ExistsByMD5(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	seedUser(t, "df_md5_u1")
	f := newTestDriveFile("df_md5_1", "df_md5_u1", "md5_unique", nil)
	require.NoError(t, repo.Create(f))
	t.Cleanup(func() { cleanupDriveFile(t, f.ID) })

	ok, err := repo.ExistsByMD5("df_md5_u1", "md5_unique")
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = repo.ExistsByMD5("df_md5_u1", "md5_nonexistent")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestDriveFileRepository_ListByFileIDs(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	seedUser(t, "df_lbf_u1")
	f1 := newTestDriveFile("df_lbf_1", "df_lbf_u1", "lbf1", nil)
	require.NoError(t, repo.Create(f1))
	t.Cleanup(func() { cleanupDriveFile(t, f1.ID) })

	got, err := repo.ListByFileIDs([]string{f1.ID})
	require.NoError(t, err)
	assert.Len(t, got, 1)

	empty, err := repo.ListByFileIDs(nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestDriveFileRepository_UsageByUser(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	seedUser(t, "df_usg_u1")
	f := newTestDriveFile("df_usg_1", "df_usg_u1", "usg", nil)
	f.Size = 1234
	require.NoError(t, repo.Create(f))
	t.Cleanup(func() { cleanupDriveFile(t, f.ID) })

	total, err := repo.UsageByUser("df_usg_u1")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, total, int64(1234))

	// 存在しないユーザー
	total, err = repo.UsageByUser("df_usg_none")
	require.NoError(t, err)
	assert.Equal(t, int64(0), total)
}

// TestDriveFileRepository_UsageByUser_ExcludesLink: upstream calcDriveUsageOf は
// isLink=FALSE の行のみ SUM(size) する。link (リモート/プロキシ) file の size は
// usage に含めない (#1831)。
func TestDriveFileRepository_UsageByUser_ExcludesLink(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	seedUser(t, "df_link_u1")

	own := newTestDriveFile("df_link_own", "df_link_u1", "own", nil)
	own.Size = 100
	require.NoError(t, repo.Create(own))
	t.Cleanup(func() { cleanupDriveFile(t, own.ID) })

	link := newTestDriveFile("df_link_remote", "df_link_u1", "remote", nil)
	link.Size = 999
	link.IsLink = true
	require.NoError(t, repo.Create(link))
	t.Cleanup(func() { cleanupDriveFile(t, link.ID) })

	total, err := repo.UsageByUser("df_link_u1")
	require.NoError(t, err)
	assert.Equal(t, int64(100), total, "link file の size は usage に含めない")
}

func TestDriveFileRepository_UpdateBulkFolder(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	seedUser(t, "df_bulk_u1")
	// folderを作成
	require.NoError(t, testDB.Exec(
		`INSERT INTO "drive_folder" (id, name, "userId") VALUES (?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		"df_bulk_folder", "folder_x", "df_bulk_u1",
	).Error)
	f := newTestDriveFile("df_bulk_1", "df_bulk_u1", "bk", nil)
	require.NoError(t, repo.Create(f))
	t.Cleanup(func() {
		cleanupDriveFile(t, f.ID)
		testDB.Exec(`DELETE FROM "drive_folder" WHERE id = ?`, "df_bulk_folder")
	})

	target := "df_bulk_folder"
	require.NoError(t, repo.UpdateBulkFolder("df_bulk_u1", []string{f.ID}, &target))

	got, err := repo.FindByID(f.ID)
	require.NoError(t, err)
	require.NotNil(t, got.FolderID)
	assert.Equal(t, "df_bulk_folder", *got.FolderID)

	// IDOR guard: 別ユーザーIDで同じ fileId を移動しても、所有者が違うため
	// 行は更新されない (folderId が維持される)。
	other := "df_bulk_other_folder"
	require.NoError(t, repo.UpdateBulkFolder("someone_else", []string{f.ID}, &other))
	got, err = repo.FindByID(f.ID)
	require.NoError(t, err)
	require.NotNil(t, got.FolderID)
	assert.Equal(t, "df_bulk_folder", *got.FolderID, "other user must not move the file (IDOR)")
}

func TestDriveFileRepository_DeleteByUser(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	seedUser(t, "df_del_u1")
	f := newTestDriveFile("df_del_1", "df_del_u1", "dl", nil)
	require.NoError(t, repo.Create(f))

	// 空userIDはno-op
	n, err := repo.DeleteByUser("")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)

	// 該当ユーザー分が削除される
	n, err = repo.DeleteByUser("df_del_u1")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(1))

	_, err = repo.FindByID(f.ID)
	assert.Error(t, err)
}

var _ = context.Background // import guard

// ListRemoteCache / ListByUserAll / DeleteByIDs の動作 (#H-PR8f)。
func TestDriveFileRepository_CleanupHelpers(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	user := insertTestUser(t, "u_clh", "clh")
	defer cleanupUser(t, user.ID)
	host := "clh.example"

	cached := newTestDriveFile("clh_c", user.ID, "md5c", nil)
	cached.IsLink = false
	cached.UserHost = &host
	link := newTestDriveFile("clh_l", user.ID, "md5l", nil)
	link.IsLink = true
	link.UserHost = &host
	local := newTestDriveFile("clh_loc", user.ID, "md5loc", nil)
	for _, f := range []*model.DriveFile{cached, link, local} {
		require.NoError(t, repo.Create(f))
		defer cleanupDriveFile(t, f.ID)
	}

	idset := func(fs []*model.DriveFile) map[string]bool {
		m := map[string]bool{}
		for _, f := range fs {
			m[f.ID] = true
		}
		return m
	}

	// ListRemoteCache は isLink=false かつ host あり (= キャッシュ実体) のみ。
	rc, err := repo.ListRemoteCache(100)
	require.NoError(t, err)
	rcIDs := idset(rc)
	assert.True(t, rcIDs[cached.ID], "cached を含む")
	assert.False(t, rcIDs[link.ID], "link-only は含まない")
	assert.False(t, rcIDs[local.ID], "local は含まない")

	// ListByUserAll は user 所有の全 file (folder 問わず)。
	ua, err := repo.ListByUserAll(user.ID, 100)
	require.NoError(t, err)
	uaIDs := idset(ua)
	assert.True(t, uaIDs[cached.ID] && uaIDs[link.ID] && uaIDs[local.ID])

	// DeleteByIDs は指定 ID のみ削除。
	n, err := repo.DeleteByIDs([]string{cached.ID, link.ID})
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
	_, err = repo.FindByID(cached.ID)
	assert.Error(t, err)
	_, err = repo.FindByID(local.ID)
	assert.NoError(t, err, "指定外の local は残る")
}

// TestDriveFileRepository_ListOrphanRemoteAttachmentCandidates は #2722 の
// 候補条件を固定する。
//
// **守るべきは「参照されている行を候補に出さない」ほう。** owner 無しの
// リモート添付は materialize (ephemeral.Materializer) で永続化された note からも、
// dedup (upsertAttachments の FindByURI) で再利用されて owner 有りの note からも
// 参照されうる。「owner 無し = ゴミ」は成り立たない。
func TestDriveFileRepository_ListOrphanRemoteAttachmentCandidates(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	host := "orphanatt.example"
	author := insertTestUser(t, "u_oa_author", "oaauthor")
	defer cleanupUser(t, author.ID)

	mk := func(id string, mutate func(*model.DriveFile)) *model.DriveFile {
		f := newTestDriveFile(id, author.ID, "md5"+id, nil)
		f.UserID = nil
		f.UserHost = &host
		f.IsLink = true
		f.StoredInternal = false
		if mutate != nil {
			mutate(f)
		}
		require.NoError(t, repo.Create(f))
		t.Cleanup(func() { cleanupDriveFile(t, f.ID) })
		return f
	}

	// cutoff より古い = 候補になりうる行。
	garbage := mk("oa_old_garbage", nil)
	referenced := mk("oa_old_referenced", nil)
	mk("oa_old_owned", func(f *model.DriveFile) { f.UserID = &author.ID })
	mk("oa_old_local", func(f *model.DriveFile) { f.UserHost = nil })
	mk("oa_old_cached", func(f *model.DriveFile) { f.IsLink = false })
	// cutoff より新しい = 猶予の内側にある行。
	mk("oa_zz_fresh", nil)

	// referenced だけを note.fileIds に載せる。
	text := "ref"
	note := &model.Note{
		ID: "n_oa_ref", UserID: author.ID, Text: &text,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
		FileIDs:    model.StringArray{referenced.ID},
	}
	require.NoError(t, testDB.Create(note).Error)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, note.ID)

	ids, err := repo.ListOrphanRemoteAttachmentCandidates("oa_zz", "", 100)
	require.NoError(t, err)
	assert.Equal(t, []string{garbage.ID}, ids,
		"候補は参照が無く cutoff より古い owner 無しリモート添付だけ")
}

// TestDriveFileRepository_ListOrphanRemoteAttachmentCandidates_Cursor は
// keyset cursor と limit の境界を固定する。
//
// **cursor が効かないと実行が終わらない。** 消さずに残る行 (参照されている /
// ephemeral が生きている) は条件に合致し続けるので、毎回先頭から引くと同じ
// 行を読み直す。実測で 50 万行に対し 1 バッチ 17.6 秒かかり、それがバッチ数
// だけ繰り返された。
func TestDriveFileRepository_ListOrphanRemoteAttachmentCandidates_Cursor(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	host := "orphanattc.example"
	author := insertTestUser(t, "u_oac", "oacauthor")
	defer cleanupUser(t, author.ID)

	for _, id := range []string{"oac_1", "oac_2", "oac_3"} {
		f := newTestDriveFile(id, author.ID, "md5"+id, nil)
		f.UserID = nil
		f.UserHost = &host
		f.IsLink = true
		require.NoError(t, repo.Create(f))
		defer cleanupDriveFile(t, f.ID)
	}

	first, err := repo.ListOrphanRemoteAttachmentCandidates("oac_z", "", 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"oac_1", "oac_2"}, first, "id 昇順で limit ぶん返る")

	next, err := repo.ListOrphanRemoteAttachmentCandidates("oac_z", first[len(first)-1], 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"oac_3"}, next, "cursor の続きから返る")

	last, err := repo.ListOrphanRemoteAttachmentCandidates("oac_z", "oac_3", 2)
	require.NoError(t, err)
	assert.Empty(t, last, "末尾まで進んだら空")

	// cutoff は排他。境界の行そのものは候補にしない。
	boundary, err := repo.ListOrphanRemoteAttachmentCandidates("oac_1", "", 10)
	require.NoError(t, err)
	assert.Empty(t, boundary, "cutoff と同じ id は含めない")
}

// TestDriveFileRepository_ListOrphanRemoteAttachmentCandidates_Guards は、
// 掃除が暴走する形の入力で何も返さないことを固定する。
//
// **空 cutoff のケースは早期 return を守っていない。** SQL 側の `id < ”` も
// どの ID にも一致しないので、関数冒頭の分岐を外しても結果は変わらない
// (変異させても落ちない)。ここで固定しているのは「空 cutoff は何も返さない」
// という**振る舞い**であって、その実現手段ではない。
func TestDriveFileRepository_ListOrphanRemoteAttachmentCandidates_Guards(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	host := "orphanattg.example"
	author := insertTestUser(t, "u_oag", "oagauthor")
	defer cleanupUser(t, author.ID)

	f := newTestDriveFile("oag_1", author.ID, "md5oag", nil)
	f.UserID = nil
	f.UserHost = &host
	f.IsLink = true
	require.NoError(t, repo.Create(f))
	defer cleanupDriveFile(t, f.ID)

	ids, err := repo.ListOrphanRemoteAttachmentCandidates("", "", 100)
	require.NoError(t, err)
	assert.Empty(t, ids, "cutoff 未指定は何も返さない")

	// limit <= 0 は既定値に倒す (0 件で返さない)。
	ids, err = repo.ListOrphanRemoteAttachmentCandidates("oag_z", "", 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"oag_1"}, ids)
}

// **`type` の LIKE パターンを escape する (#3037)。**
//
// SQL は upstream も `type.replace('/*','/') + '%'` を生のまま載せる。
// **ただし upstream ではその値が LIKE に届かない** — ajv の `pattern`
// (`drive/files.ts:40` / `admin/drive/files.ts:40`) に `_` が入っていないので
// `a_b/*` は 400 で弾かれる。mk-go はこの pattern を持たないため `_` が
// 1 文字 wildcard として働き、「その型だけ」を指定したつもりの絞り込みが
// 別の型まで拾う。MIME の subtype に `_` は稀だが使える。
// mk-go は #1054 から LIKE を必ず escape する方針で、ここだけ通っていなかった。
func TestDriveFileRepository_TypeFilterEscapesLike(t *testing.T) {
	db := testDB
	repo := NewDriveFileRepository(db)

	require.NoError(t, db.Exec(`DELETE FROM drive_file`).Error)
	// **wildcard が効くのは `/*` 終端の prefix だけ** (upstream と同じ
	// semantics)。`_` はその prefix 側に置かないと LIKE に載らない。
	for i, typ := range []string{"a_b/x", "axb/x"} {
		require.NoError(t, db.Create(&model.DriveFile{
			ID: "dtf" + string(rune('0'+i)), Name: "n", Type: typ,
			MD5: "m", Size: 1, URL: "https://e/x",
		}).Error)
	}
	t.Cleanup(func() { db.Exec(`DELETE FROM drive_file WHERE id LIKE 'dtf%'`) })

	// `a_b/*` は `a_b/` で始まる型だけを指す。escape していないと `_` が
	// 1 文字 wildcard になって `axb/x` まで拾う。
	got, err := repo.ListForAdmin("", "local", "", "a_b/*", "", "", 10)
	require.NoError(t, err)
	require.Len(t, got, 1, "1 文字 wildcard として解釈されている")
	assert.Equal(t, "a_b/x", got[0].Type)
}

// **列に入らない `type` は引く前に空にする (#3037)。** NUL も不正な UTF-8 も
// 比較の右辺に置くだけで PostgreSQL がクエリごと落とす。`drive/files` は
// 任意の認証ユーザーが叩けるので、そのままだと 500 を起こせる。
func TestDriveFileRepository_TypeFilterRejectsUnstorable(t *testing.T) {
	db := testDB
	repo := NewDriveFileRepository(db)

	for name, typ := range map[string]string{
		"NUL":           "image/a\x00b",
		"invalid UTF-8": "image/a\x80b",
		"NUL in prefix": "image/a\x00*",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := repo.ListForAdmin("", "local", "", typ, "", "", 10)
			require.NoError(t, err, "SELECT に載せてしまっている")
			assert.Empty(t, got)

			got, err = repo.ListByUser("u1", nil, true, typ, "", "", "", 10)
			require.NoError(t, err, "SELECT に載せてしまっている")
			assert.Empty(t, got)
		})
	}
}

// **普通の絞り込みは効いたまま。** これが無いと「常に空を返す」実装でも上が通る。
func TestDriveFileRepository_TypeFilterStillMatches(t *testing.T) {
	db := testDB
	repo := NewDriveFileRepository(db)

	require.NoError(t, db.Exec(`DELETE FROM drive_file`).Error)
	require.NoError(t, db.Create(&model.DriveFile{
		ID: "dtf9", Name: "n", Type: "image/png", MD5: "m", Size: 1, URL: "https://e/x",
	}).Error)
	t.Cleanup(func() { db.Exec(`DELETE FROM drive_file WHERE id = 'dtf9'`) })

	got, err := repo.ListForAdmin("", "local", "", "image/*", "", "", 10)
	require.NoError(t, err)
	require.Len(t, got, 1, "prefix LIKE が効いていない")

	got, err = repo.ListForAdmin("", "local", "", "image/png", "", "", 10)
	require.NoError(t, err)
	assert.Len(t, got, 1, "完全一致が効いていない")
}
