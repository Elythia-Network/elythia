package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// 本ファイルは#260 repository coverage 90%到達のために作成。
// 各パッケージの残り0%関数を最小限のsmoke testで埋める。
// 個別ファイルの意図的な設計テストは個別のファイルに書く。

// --- access_token ---

func TestAccessTokenRepository_FindByID_ListByUserID_DeleteByID(t *testing.T) {
	repo := NewAccessTokenRepository(testDB)
	u := insertTestUser(t, "at_cov_u1", "atcov")
	defer cleanupUser(t, u.ID)

	tok := &model.AccessToken{
		ID:     "at_cov_1",
		Token:  "tok_cov_1",
		Hash:   "hash_cov_1",
		UserID: u.ID,
	}
	require.NoError(t, testDB.Create(tok).Error)
	defer testDB.Exec(`DELETE FROM "access_token" WHERE id = ?`, tok.ID)

	// FindByID
	got, err := repo.FindByID(tok.ID)
	require.NoError(t, err)
	assert.Equal(t, u.ID, got.UserID)

	// ListByUserID
	list, err := repo.ListByUserID(u.ID)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	// DeleteByID
	require.NoError(t, repo.DeleteByID(tok.ID))
	_, err = repo.FindByID(tok.ID)
	assert.Error(t, err)
}

func TestAccessTokenRepository_FindByID_NotFound(t *testing.T) {
	repo := NewAccessTokenRepository(testDB)
	_, err := repo.FindByID("at_cov_nonexistent")
	assert.Error(t, err)
}

// --- auth_session ---

func TestAuthSessionRepository_FindAppByID_ListAppsByUserID(t *testing.T) {
	repo := NewAuthSessionRepository(testDB)
	u := insertTestUser(t, "as_cov_u1", "ascov")
	defer cleanupUser(t, u.ID)

	app := &model.App{
		ID:          "app_cov_1",
		UserID:      &u.ID,
		Secret:      "sec_cov_1",
		Name:        "test app",
		Description: "",
		Permission:  model.StringArray{"read"},
	}
	require.NoError(t, repo.CreateApp(app))
	defer testDB.Exec(`DELETE FROM "app" WHERE id = ?`, app.ID)

	got, err := repo.FindAppByID(app.ID)
	require.NoError(t, err)
	assert.Equal(t, "test app", got.Name)

	list, err := repo.ListAppsByUserID(u.ID, 10, 0)
	require.NoError(t, err)
	assert.NotEmpty(t, list)
}

// --- channel.FindByIDs ---

func TestChannelRepository_FindByIDs(t *testing.T) {
	repo := NewChannelRepository(testDB)
	u := insertTestUser(t, "ch_fi_u1", "chfi")
	defer cleanupUser(t, u.ID)
	seedChannel(t, "ch_fi_c1", u.ID)
	seedChannel(t, "ch_fi_c2", u.ID)

	chs, err := repo.FindByIDs([]string{"ch_fi_c1", "ch_fi_c2", "missing"})
	require.NoError(t, err)
	assert.Len(t, chs, 2)

	empty, err := repo.FindByIDs(nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// --- clip.ListPublicByUser ---

func TestClipRepository_ListPublicByUser(t *testing.T) {
	repo := NewClipRepository(testDB)
	u := insertTestUser(t, "cl_pub_u1", "clpub")
	defer cleanupUser(t, u.ID)

	// publicなclip1つ + privateなclip1つ
	require.NoError(t, testDB.Exec(
		`INSERT INTO "clip" (id, "userId", name, "isPublic") VALUES (?, ?, ?, true), (?, ?, ?, false)`,
		"cl_pub_1", u.ID, "pub",
		"cl_priv_1", u.ID, "priv",
	).Error)
	defer testDB.Exec(`DELETE FROM "clip" WHERE id IN (?, ?)`, "cl_pub_1", "cl_priv_1")

	rows, err := repo.ListPublicByUser(u.ID, "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	assert.Equal(t, "cl_pub_1", rows[0].ID)

	// limit=0 → default, limit>100 → cap
	_, err = repo.ListPublicByUser(u.ID, "", "", 0, 0)
	require.NoError(t, err)
	_, err = repo.ListPublicByUser(u.ID, "", "", 999, 0)
	require.NoError(t, err)
}

// --- flash.ListPublicByUser ---

func TestFlashRepository_ListPublicByUser(t *testing.T) {
	repo := NewFlashRepository(testDB)
	u := insertTestUser(t, "fl_pub_u1", "flpub")
	defer cleanupUser(t, u.ID)

	require.NoError(t, testDB.Exec(
		`INSERT INTO "flash" (id, "updatedAt", "userId", title, summary, script, permissions, visibility)
		 VALUES (?, NOW(), ?, ?, ?, ?, ?, ?), (?, NOW(), ?, ?, ?, ?, ?, ?)`,
		"fl_pub_1", u.ID, "pub", "", "", model.StringArray{}, "public",
		"fl_priv_1", u.ID, "priv", "", "", model.StringArray{}, "private",
	).Error)
	defer testDB.Exec(`DELETE FROM "flash" WHERE id IN (?, ?)`, "fl_pub_1", "fl_priv_1")

	rows, err := repo.ListPublicByUser(u.ID, "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	assert.Equal(t, "fl_pub_1", rows[0].ID)

	_, err = repo.ListPublicByUser(u.ID, "", "", 0, 0)
	require.NoError(t, err)
	_, err = repo.ListPublicByUser(u.ID, "", "", 999, 0)
	require.NoError(t, err)
}

// --- page.ListPublicByUser ---

func TestPageRepository_ListPublicByUser(t *testing.T) {
	repo := NewPageRepository(testDB)
	u := insertTestUser(t, "pg_pub_u1", "pgpub")
	defer cleanupUser(t, u.ID)

	require.NoError(t, testDB.Exec(
		`INSERT INTO "page" (id, "updatedAt", title, name, "userId", content, variables, visibility, "hideTitleWhenPinned")
		 VALUES (?, NOW(), ?, ?, ?, '[]'::jsonb, '[]'::jsonb, ?, false), (?, NOW(), ?, ?, ?, '[]'::jsonb, '[]'::jsonb, ?, false)`,
		"pg_pub_1", "pub", "p1", u.ID, "public",
		"pg_priv_1", "priv", "p2", u.ID, "specified",
	).Error)
	defer testDB.Exec(`DELETE FROM "page" WHERE id IN (?, ?)`, "pg_pub_1", "pg_priv_1")

	rows, err := repo.ListPublicByUser(u.ID, "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1)
	assert.Equal(t, "pg_pub_1", rows[0].ID)
}

// --- drive_folder.FindByName ---

func TestDriveFolderRepository_FindByName(t *testing.T) {
	repo := NewDriveFolderRepository(testDB)
	u := insertTestUser(t, "df_fn_u1", "dffn")
	defer cleanupUser(t, u.ID)

	require.NoError(t, testDB.Exec(
		`INSERT INTO "drive_folder" (id, name, "userId") VALUES (?, ?, ?) ON CONFLICT (id) DO NOTHING`,
		"df_fn_1", "my_folder", u.ID,
	).Error)
	defer testDB.Exec(`DELETE FROM "drive_folder" WHERE id = ?`, "df_fn_1")

	// parentFolderID nil (root)
	found, err := repo.FindByName(u.ID, "my_folder", nil)
	require.NoError(t, err)
	assert.Len(t, found, 1)
	assert.Equal(t, "df_fn_1", found[0].ID)

	// parentFolderID 指定 (マッチしない分岐)
	dummy := "other_parent"
	found, err = repo.FindByName(u.ID, "my_folder", &dummy)
	require.NoError(t, err)
	assert.Empty(t, found)

	// 存在しない名前
	found, err = repo.FindByName(u.ID, "nonexistent", nil)
	require.NoError(t, err)
	assert.Empty(t, found)
}

// --- note_reaction.FindByUserAndNoteIDs ---

func TestNoteReactionRepository_FindByUserAndNoteIDs(t *testing.T) {
	rRepo := NewNoteReactionRepository(testDB)
	nRepo := NewNoteRepository(testDB)
	u := insertTestUser(t, "nr_fu_u1", "nrfu")
	defer cleanupUser(t, u.ID)

	txt := "hi"
	n := &model.Note{
		ID:         "nr_fu_n1",
		UserID:     u.ID,
		Text:       &txt,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, nRepo.Create(n))
	defer cleanupNote(t, n.ID)

	react := &model.NoteReaction{ID: "nr_fu_r1", UserID: u.ID, NoteID: n.ID, Reaction: "👍"}
	require.NoError(t, rRepo.Create(react))
	defer testDB.Exec(`DELETE FROM "note_reaction" WHERE id = ?`, react.ID)

	got, err := rRepo.FindByUserAndNoteIDs(u.ID, []string{n.ID, "missing"})
	require.NoError(t, err)
	assert.Len(t, got, 1)

	empty, err := rRepo.FindByUserAndNoteIDs(u.ID, nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// --- page_like.ListByUser ---

func TestPageLikeRepository_ListByUser(t *testing.T) {
	repo := NewPageLikeRepository(testDB)
	u := insertTestUser(t, "pl_lb_u1", "pllb")
	defer cleanupUser(t, u.ID)

	require.NoError(t, testDB.Exec(
		`INSERT INTO "page" (id, "updatedAt", title, name, "userId", content, variables, visibility, "hideTitleWhenPinned")
		 VALUES (?, NOW(), ?, ?, ?, '[]'::jsonb, '[]'::jsonb, 'public', false)`,
		"pl_lb_p1", "pg", "pg_name", u.ID,
	).Error)
	defer testDB.Exec(`DELETE FROM "page" WHERE id = ?`, "pl_lb_p1")

	require.NoError(t, testDB.Exec(
		`INSERT INTO "page_like" (id, "userId", "pageId") VALUES (?, ?, ?)`,
		"pl_lb_1", u.ID, "pl_lb_p1",
	).Error)
	defer testDB.Exec(`DELETE FROM "page_like" WHERE id = ?`, "pl_lb_1")

	likes, err := repo.ListByUser(u.ID, "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, likes, 1)
}

// --- user.FindProfileByEmail / CountOnlineUsers ---

func TestUserRepository_FindProfileByEmail(t *testing.T) {
	repo := NewUserRepository(testDB)
	u := insertTestUser(t, "u_em_1", "emuser")
	defer cleanupUser(t, u.ID)

	email := "em_unique@example.com"
	require.NoError(t, testDB.Exec(
		`INSERT INTO "user_profile" ("userId", email, password) VALUES (?, ?, ?) ON CONFLICT ("userId") DO UPDATE SET email = EXCLUDED.email`,
		u.ID, email, "hash",
	).Error)
	defer testDB.Exec(`DELETE FROM "user_profile" WHERE "userId" = ?`, u.ID)

	prof, err := repo.FindProfileByEmail(email)
	require.NoError(t, err)
	assert.Equal(t, u.ID, prof.UserID)

	// 存在しない
	_, err = repo.FindProfileByEmail("noone@example.com")
	assert.Error(t, err)
}

func TestUserRepository_CountOnlineUsers(t *testing.T) {
	repo := NewUserRepository(testDB)
	// アクティブユーザーを1人用意
	u := insertTestUser(t, "u_on_1", "onuser")
	defer cleanupUser(t, u.ID)
	require.NoError(t, testDB.Exec(
		`UPDATE "user" SET "lastActiveDate" = NOW() WHERE id = ?`, u.ID,
	).Error)

	// CountOnlineUsersは内部で10分thresholdを使う
	cnt, err := repo.CountOnlineUsers()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, cnt, int64(1))
}

// --- user_list.UpdateList / UpdateMembership / ListsContainingMember ---

func TestUserListRepository_UpdateList_UpdateMembership_ListsContainingMember(t *testing.T) {
	repo := NewUserListRepository(testDB)
	owner := insertTestUser(t, "ul_cov_o1", "ulcovo")
	defer cleanupUser(t, owner.ID)
	member := insertTestUser(t, "ul_cov_m1", "ulcovm")
	defer cleanupUser(t, member.ID)

	require.NoError(t, testDB.Exec(
		`INSERT INTO "user_list" (id, "userId", name, "isPublic") VALUES (?, ?, ?, false)`,
		"ul_cov_l1", owner.ID, "original_name",
	).Error)
	defer testDB.Exec(`DELETE FROM "user_list" WHERE id = ?`, "ul_cov_l1")

	// UpdateList
	require.NoError(t, repo.UpdateList("ul_cov_l1", map[string]any{"name": "renamed_list"}))

	// UpdateListの空fieldsはno-op
	require.NoError(t, repo.UpdateList("ul_cov_l1", map[string]any{}))

	// membership
	require.NoError(t, testDB.Exec(
		`INSERT INTO "user_list_membership" (id, "userListId", "userId") VALUES (?, ?, ?)`,
		"ul_cov_mem1", "ul_cov_l1", member.ID,
	).Error)
	defer testDB.Exec(`DELETE FROM "user_list_membership" WHERE id = ?`, "ul_cov_mem1")

	wrTrue := true
	require.NoError(t, repo.UpdateMembership("ul_cov_l1", member.ID, &wrTrue))

	// ListsContainingMember (owner視点でmemberを含むリスト一覧)
	lists, err := repo.ListsContainingMember(owner.ID, member.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, lists)
}
