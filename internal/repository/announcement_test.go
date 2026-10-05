package repository

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanupAnnouncement(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "announcement_read" WHERE "announcementId" = ?`, id)
	testDB.Exec(`DELETE FROM "announcement" WHERE id = ?`, id)
}

func TestAnnouncementRepository_CRUD(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)

	a := &model.Announcement{ID: "ann_1", Title: "Test", Text: "Hello", Icon: "info", Display: "normal", IsActive: true}
	require.NoError(t, repo.Create(a))
	defer cleanupAnnouncement(t, a.ID)

	found, err := repo.FindByID(a.ID)
	require.NoError(t, err)
	assert.Equal(t, "Test", found.Title)

	// List (active) — 作成した active な ann_1 が返る
	items, err := repo.List(true, 10, 0, "", "")
	require.NoError(t, err)
	assert.NotEmpty(t, items)

	// #2106 N7: List は isActive を等値フィルタで扱う。List(false)=inactive のみなので
	// active な ann_1 は含まれない (旧実装は false を「フィルタ無し」と誤解釈していた)。
	items, err = repo.List(false, 10, 0, "", "")
	require.NoError(t, err)
	for _, it := range items {
		assert.NotEqual(t, "ann_1", it.ID, "List(false) は active announcement を含めない")
	}

	// UpdateFields
	require.NoError(t, repo.UpdateFields(a.ID, map[string]any{"title": "Updated"}))
	found, _ = repo.FindByID(a.ID)
	assert.Equal(t, "Updated", found.Title)

	// Delete
	require.NoError(t, repo.Delete(a.ID))
	_, err = repo.FindByID(a.ID)
	assert.Error(t, err)
}

func TestAnnouncementRepository_FindByID_NotFound(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)
	_, err := repo.FindByID("ghost")
	assert.Error(t, err)
}

func TestAnnouncementRepository_List_Pagination(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)
	a1 := &model.Announcement{ID: "ann_p1", Title: "A", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	a2 := &model.Announcement{ID: "ann_p2", Title: "B", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	require.NoError(t, repo.Create(a1))
	require.NoError(t, repo.Create(a2))
	defer cleanupAnnouncement(t, a1.ID)
	defer cleanupAnnouncement(t, a2.ID)

	items, err := repo.List(true, 1, 0, "", "")
	require.NoError(t, err)
	assert.Len(t, items, 1)

	items, err = repo.List(true, 10, 1, "", "")
	require.NoError(t, err)
	assert.NotEmpty(t, items)

	// default limit
	items, err = repo.List(true, 0, 0, "", "")
	require.NoError(t, err)
	assert.NotEmpty(t, items)

	// limit cap
	items, err = repo.List(true, 999, 0, "", "")
	require.NoError(t, err)
	assert.LessOrEqual(t, len(items), 100)
}

func TestAnnouncementRepository_List_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewAnnouncementRepository(testDB.WithContext(ctx))
	_, err := repo.List(true, 10, 0, "", "")
	assert.Error(t, err)
}

func TestAnnouncementRepository_ListGlobal_ExcludesPerUser(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)
	otherID := "u_lg_other"
	seedUser(t, otherID) // FK制約があるので先にuser行を作る
	other := otherID
	global := &model.Announcement{ID: "ann_lg_g", Title: "G", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	targeted := &model.Announcement{ID: "ann_lg_t", Title: "T", Text: "t", Icon: "info", Display: "normal", IsActive: true, UserID: &other}
	// defer は Create の前に登録して、Create 途中失敗時も確実にcleanupする。
	defer cleanupAnnouncement(t, global.ID)
	defer cleanupAnnouncement(t, targeted.ID)
	require.NoError(t, repo.Create(global))
	require.NoError(t, repo.Create(targeted))

	items, err := repo.ListGlobal(true, 100, 0, "", "")
	require.NoError(t, err)
	ids := make(map[string]bool, len(items))
	for _, a := range items {
		ids[a.ID] = true
	}
	assert.True(t, ids[global.ID])
	assert.False(t, ids[targeted.ID])
}

func TestAnnouncementRepository_ListGlobal_Pagination(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)
	a1 := &model.Announcement{ID: "ann_gp1", Title: "A", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	a2 := &model.Announcement{ID: "ann_gp2", Title: "B", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	defer cleanupAnnouncement(t, a1.ID)
	defer cleanupAnnouncement(t, a2.ID)
	require.NoError(t, repo.Create(a1))
	require.NoError(t, repo.Create(a2))

	items, err := repo.ListGlobal(true, 1, 0, "", "")
	require.NoError(t, err)
	assert.Len(t, items, 1)
	// default limitとcap経路
	items, err = repo.ListGlobal(true, 0, 0, "", "")
	require.NoError(t, err)
	assert.NotEmpty(t, items)
	items, err = repo.ListGlobal(true, 999, 0, "", "")
	require.NoError(t, err)
	assert.LessOrEqual(t, len(items), 100)
	// offset
	items, err = repo.ListGlobal(true, 10, 1, "", "")
	require.NoError(t, err)
	assert.NotEmpty(t, items)
}

func TestAnnouncementRepository_ListGlobal_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewAnnouncementRepository(testDB.WithContext(ctx))
	_, err := repo.ListGlobal(true, 10, 0, "", "")
	assert.Error(t, err)
}

func TestAnnouncementRepository_ListForUser_IncludesGlobalAndOwnTargeted(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)
	me := "u_lfu_me"
	other := "u_lfu_oth"
	seedUser(t, me)
	seedUser(t, other)
	global := &model.Announcement{ID: "ann_lfu_g", Title: "G", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	mine := &model.Announcement{ID: "ann_lfu_m", Title: "Mine", Text: "t", Icon: "info", Display: "normal", IsActive: true, UserID: &me}
	others := &model.Announcement{ID: "ann_lfu_o", Title: "Other", Text: "t", Icon: "info", Display: "normal", IsActive: true, UserID: &other}
	defer cleanupAnnouncement(t, global.ID)
	defer cleanupAnnouncement(t, mine.ID)
	defer cleanupAnnouncement(t, others.ID)
	require.NoError(t, repo.Create(global))
	require.NoError(t, repo.Create(mine))
	require.NoError(t, repo.Create(others))

	items, err := repo.ListForUser(me, true, 100, 0, "", "")
	require.NoError(t, err)
	ids := make(map[string]bool, len(items))
	for _, a := range items {
		ids[a.ID] = true
	}
	assert.True(t, ids[global.ID])
	assert.True(t, ids[mine.ID])
	assert.False(t, ids[others.ID])
}

func TestAnnouncementRepository_ListForUser_Pagination(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)
	a1 := &model.Announcement{ID: "ann_fp1", Title: "A", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	a2 := &model.Announcement{ID: "ann_fp2", Title: "B", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	defer cleanupAnnouncement(t, a1.ID)
	defer cleanupAnnouncement(t, a2.ID)
	require.NoError(t, repo.Create(a1))
	require.NoError(t, repo.Create(a2))

	items, err := repo.ListForUser("u", true, 1, 0, "", "")
	require.NoError(t, err)
	assert.Len(t, items, 1)
	items, err = repo.ListForUser("u", true, 0, 0, "", "")
	require.NoError(t, err)
	assert.NotEmpty(t, items)
	items, err = repo.ListForUser("u", true, 999, 0, "", "")
	require.NoError(t, err)
	assert.LessOrEqual(t, len(items), 100)
	items, err = repo.ListForUser("u", true, 10, 1, "", "")
	require.NoError(t, err)
	assert.NotEmpty(t, items)
}

func TestAnnouncementRepository_ListGlobal_CursorPagination(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)
	// IDの辞書順 = 時系列順になるようにIDを付ける
	a1 := &model.Announcement{ID: "ann_cur_a", Title: "A", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	a2 := &model.Announcement{ID: "ann_cur_b", Title: "B", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	a3 := &model.Announcement{ID: "ann_cur_c", Title: "C", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	defer cleanupAnnouncement(t, a1.ID)
	defer cleanupAnnouncement(t, a2.ID)
	defer cleanupAnnouncement(t, a3.ID)
	require.NoError(t, repo.Create(a1))
	require.NoError(t, repo.Create(a2))
	require.NoError(t, repo.Create(a3))

	// untilId: a3より古いもの → a1, a2
	items, err := repo.ListGlobal(true, 100, 0, "", a3.ID)
	require.NoError(t, err)
	ids := make(map[string]bool, len(items))
	for _, a := range items {
		ids[a.ID] = true
	}
	assert.True(t, ids[a1.ID])
	assert.True(t, ids[a2.ID])
	assert.False(t, ids[a3.ID])

	// sinceId: a1より新しいもの → a2, a3
	items, err = repo.ListGlobal(true, 100, 0, a1.ID, "")
	require.NoError(t, err)
	ids = make(map[string]bool, len(items))
	for _, a := range items {
		ids[a.ID] = true
	}
	assert.False(t, ids[a1.ID])
	assert.True(t, ids[a2.ID])
	assert.True(t, ids[a3.ID])

	// sinceId + untilId: a1 < id < a3 → a2のみ
	items, err = repo.ListGlobal(true, 100, 0, a1.ID, a3.ID)
	require.NoError(t, err)
	assert.Len(t, items, 1)
	assert.Equal(t, a2.ID, items[0].ID)
}

func TestAnnouncementRepository_ListForUser_CursorPagination(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)
	a1 := &model.Announcement{ID: "ann_ucur_a", Title: "A", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	a2 := &model.Announcement{ID: "ann_ucur_b", Title: "B", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	a3 := &model.Announcement{ID: "ann_ucur_c", Title: "C", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	defer cleanupAnnouncement(t, a1.ID)
	defer cleanupAnnouncement(t, a2.ID)
	defer cleanupAnnouncement(t, a3.ID)
	require.NoError(t, repo.Create(a1))
	require.NoError(t, repo.Create(a2))
	require.NoError(t, repo.Create(a3))

	// untilId: a3より古いもの → a1, a2
	items, err := repo.ListForUser("u", true, 100, 0, "", a3.ID)
	require.NoError(t, err)
	ids := make(map[string]bool, len(items))
	for _, a := range items {
		ids[a.ID] = true
	}
	assert.True(t, ids[a1.ID])
	assert.True(t, ids[a2.ID])
	assert.False(t, ids[a3.ID])

	// sinceId: a1より新しいもの → a2, a3
	items, err = repo.ListForUser("u", true, 100, 0, a1.ID, "")
	require.NoError(t, err)
	ids = make(map[string]bool, len(items))
	for _, a := range items {
		ids[a.ID] = true
	}
	assert.False(t, ids[a1.ID])
	assert.True(t, ids[a2.ID])
	assert.True(t, ids[a3.ID])
}

// 本家QueryService.makePaginationQueryはsinceID単独指定時にASCに反転する。
// DESCのままだと「次ページ(新しい方向)を取る」操作でカーソルが壊れるので
// 回帰防止テスト。
func TestAnnouncementRepository_SinceIDFlipsOrderASC(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)
	a1 := &model.Announcement{ID: "ann_ord_a", Title: "A", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	a2 := &model.Announcement{ID: "ann_ord_b", Title: "B", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	a3 := &model.Announcement{ID: "ann_ord_c", Title: "C", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	defer cleanupAnnouncement(t, a1.ID)
	defer cleanupAnnouncement(t, a2.ID)
	defer cleanupAnnouncement(t, a3.ID)
	require.NoError(t, repo.Create(a1))
	require.NoError(t, repo.Create(a2))
	require.NoError(t, repo.Create(a3))

	// sinceID単独指定: ASC (古い→新しい)
	items, err := repo.List(true, 100, 0, a1.ID, "")
	require.NoError(t, err)
	assertOwn := func(items []*model.Announcement) []*model.Announcement {
		out := make([]*model.Announcement, 0, len(items))
		for _, a := range items {
			if a.ID == a2.ID || a.ID == a3.ID {
				out = append(out, a)
			}
		}
		return out
	}
	own := assertOwn(items)
	require.Len(t, own, 2)
	assert.Equal(t, a2.ID, own[0].ID)
	assert.Equal(t, a3.ID, own[1].ID)

	// untilID単独指定: DESC (新しい→古い)
	items, err = repo.List(true, 100, 0, "", a3.ID)
	require.NoError(t, err)
	own = assertOwn(items)
	require.Len(t, own, 1) // a1, a2のうちtest fixtureではa2のみがa3より前
	// ownに含まれる順序もDESCであること
	// (a2が最初に来るはずだがfixtureに他のレコードが混ざる可能性があるため
	// 先頭/末尾ではなく全体のID降順を検証)
	for i := 1; i < len(items); i++ {
		assert.GreaterOrEqual(t, items[i-1].ID, items[i].ID, "DESC order expected")
	}

	// 両方指定: DESC (既存挙動)
	items, err = repo.List(true, 100, 0, a1.ID, a3.ID)
	require.NoError(t, err)
	for i := 1; i < len(items); i++ {
		assert.GreaterOrEqual(t, items[i-1].ID, items[i].ID, "DESC order expected")
	}

	// ListGlobal / ListForUserも同様にASC反転すること
	items, err = repo.ListGlobal(true, 100, 0, a1.ID, "")
	require.NoError(t, err)
	own = assertOwn(items)
	require.Len(t, own, 2)
	assert.Equal(t, a2.ID, own[0].ID)
	assert.Equal(t, a3.ID, own[1].ID)

	items, err = repo.ListForUser("u", true, 100, 0, a1.ID, "")
	require.NoError(t, err)
	own = assertOwn(items)
	require.Len(t, own, 2)
	assert.Equal(t, a2.ID, own[0].ID)
	assert.Equal(t, a3.ID, own[1].ID)
}

func TestAnnouncementRepository_ListForUser_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewAnnouncementRepository(testDB.WithContext(ctx))
	_, err := repo.ListForUser("u", true, 10, 0, "", "")
	assert.Error(t, err)
}

// OR の括弧漏れで isActive filter が効かなくなる GORM gotcha
// (SQL優先順位 AND > OR) の回帰防止テスト。
func TestAnnouncementRepository_ListForUser_ActiveOnlyAppliesToGlobal(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)
	// model.Announcement の gorm tag に default:true があり、Goのbool zero
	// valueをGORMが「未指定」と見なしてDB defaultを適用してしまうため、
	// 一度IsActive=trueで作ってからUpdateFieldsでfalseに落とす。
	inactive := &model.Announcement{ID: "ann_ra_g", Title: "G-off", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	defer cleanupAnnouncement(t, inactive.ID)
	require.NoError(t, repo.Create(inactive))
	require.NoError(t, repo.UpdateFields(inactive.ID, map[string]any{"isActive": false}))

	// activeOnly=true の場合、inactive な global announcement も除外される
	// はず(括弧無しだと isActive filter が global に適用されず漏れる)。
	items, err := repo.ListForUser("some_user", true, 100, 0, "", "")
	require.NoError(t, err)
	for _, a := range items {
		assert.NotEqual(t, inactive.ID, a.ID)
	}
}

func TestAnnouncementRepository_ReadManagement(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)
	createTestUser(t, "ann_reader")

	a := &model.Announcement{ID: "ann_r1", Title: "R", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	require.NoError(t, repo.Create(a))
	defer cleanupAnnouncement(t, a.ID)

	// Not read yet
	read, err := repo.IsRead("ann_reader", a.ID)
	require.NoError(t, err)
	assert.False(t, read)

	// Unread list
	unread, err := repo.UnreadForUser("ann_reader")
	require.NoError(t, err)
	assert.NotEmpty(t, unread)

	// Mark read
	require.NoError(t, repo.MarkRead(&model.AnnouncementRead{ID: "ar_1", UserID: "ann_reader", AnnouncementID: a.ID}))

	read, err = repo.IsRead("ann_reader", a.ID)
	require.NoError(t, err)
	assert.True(t, read)

	unread, err = repo.UnreadForUser("ann_reader")
	require.NoError(t, err)
	assert.Empty(t, unread)
}

func TestAnnouncementRepository_UnreadForUser_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewAnnouncementRepository(testDB.WithContext(ctx))
	_, err := repo.UnreadForUser("x")
	assert.Error(t, err)
}

func TestAnnouncementRepository_IsRead_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewAnnouncementRepository(testDB.WithContext(ctx))
	_, err := repo.IsRead("x", "y")
	assert.Error(t, err)
}

func TestAnnouncementRepository_ListForAdmin_FiltersByUserID(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)

	// 念のため、過去 run の残骸を消してから始める。同名 fixture を使う
	// と FK / pkey 違反になりやすい。
	testDB.Exec(`DELETE FROM "announcement" WHERE id IN (?, ?, ?)`, "ann_lfa_g", "ann_lfa_t", "ann_lfa_o")

	tu := insertTestUser(t, "u_ann_tgt", "annt")
	defer cleanupUser(t, tu.ID)
	ou := insertTestUser(t, "u_ann_oth", "anno")
	defer cleanupUser(t, ou.ID)
	global := &model.Announcement{ID: "ann_lfa_g", Title: "g", Text: "g", Icon: "info", Display: "normal", IsActive: true}
	tUser := &model.Announcement{ID: "ann_lfa_t", Title: "t", Text: "t", Icon: "info", Display: "normal", IsActive: true, UserID: &tu.ID}
	oUser := &model.Announcement{ID: "ann_lfa_o", Title: "o", Text: "o", Icon: "info", Display: "normal", IsActive: true, UserID: &ou.ID}
	require.NoError(t, repo.Create(global))
	require.NoError(t, repo.Create(tUser))
	require.NoError(t, repo.Create(oUser))
	defer cleanupAnnouncement(t, global.ID)
	defer cleanupAnnouncement(t, tUser.ID)
	defer cleanupAnnouncement(t, oUser.ID)

	// userId 指定 → そのユーザー宛のみ
	rows, err := repo.ListForAdmin(tu.ID, "active", 10, 0, "", "")
	require.NoError(t, err)
	ids := make(map[string]bool)
	for _, r := range rows {
		ids[r.ID] = true
	}
	assert.True(t, ids[tUser.ID])
	assert.False(t, ids[global.ID])
	assert.False(t, ids[oUser.ID])

	// userId 未指定 → global only
	rows, err = repo.ListForAdmin("", "active", 10, 0, "", "")
	require.NoError(t, err)
	ids = make(map[string]bool)
	for _, r := range rows {
		ids[r.ID] = true
	}
	assert.True(t, ids[global.ID])
	assert.False(t, ids[tUser.ID])
	assert.False(t, ids[oUser.ID])
}

func TestAnnouncementRepository_ListForAdmin_StatusBranches(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)
	active := &model.Announcement{ID: "ann_lfa_act", Title: "a", Text: "a", Icon: "info", Display: "normal", IsActive: true}
	// GORM の `default:true` tag により bool の zero value は DB default
	// (true) が適用される (`internal/model/announcement.go:15` 参照)。
	// archived row を作るときは一度 true で insert してから UpdateFields
	// で false に落とすのが既存パターン。
	archived := &model.Announcement{ID: "ann_lfa_arc", Title: "x", Text: "x", Icon: "info", Display: "normal", IsActive: true}
	require.NoError(t, repo.Create(active))
	require.NoError(t, repo.Create(archived))
	require.NoError(t, repo.UpdateFields(archived.ID, map[string]any{"isActive": false}))
	defer cleanupAnnouncement(t, active.ID)
	defer cleanupAnnouncement(t, archived.ID)

	// archived: archived row が含まれ active row が含まれないこと。
	rows, err := repo.ListForAdmin("", "archived", 10, 0, "", "")
	require.NoError(t, err)
	archivedIDs := make(map[string]bool)
	for _, r := range rows {
		assert.False(t, r.IsActive)
		archivedIDs[r.ID] = true
	}
	assert.True(t, archivedIDs[archived.ID])
	assert.False(t, archivedIDs[active.ID])
	// all (両方含む)
	rows, err = repo.ListForAdmin("", "all", 10, 0, "", "")
	require.NoError(t, err)
	ids := make(map[string]bool)
	for _, r := range rows {
		ids[r.ID] = true
	}
	assert.True(t, ids[active.ID])
	assert.True(t, ids[archived.ID])

	// limit / offset / since / until / clamp paths
	_, err = repo.ListForAdmin("", "active", 0, 0, "", "")
	require.NoError(t, err)
	_, err = repo.ListForAdmin("", "active", 1000, 5, "", "")
	require.NoError(t, err)
	_, err = repo.ListForAdmin("", "active", 10, 0, "since_x", "until_x")
	require.NoError(t, err)
}

func TestAnnouncementRepository_CountReadsByAnnouncementIDs(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)
	u1 := insertTestUser(t, "u_ann_cr1", "annc1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "u_ann_cr2", "annc2")
	defer cleanupUser(t, u2.ID)
	a := &model.Announcement{ID: "ann_cnt_1", Title: "t", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	require.NoError(t, repo.Create(a))
	defer cleanupAnnouncement(t, a.ID)
	require.NoError(t, repo.MarkRead(&model.AnnouncementRead{ID: "ar_cnt_1", UserID: u1.ID, AnnouncementID: a.ID}))
	require.NoError(t, repo.MarkRead(&model.AnnouncementRead{ID: "ar_cnt_2", UserID: u2.ID, AnnouncementID: a.ID}))

	out, err := repo.CountReadsByAnnouncementIDs([]string{a.ID, "ann_no_such"})
	require.NoError(t, err)
	assert.EqualValues(t, 2, out[a.ID])
	_, missing := out["ann_no_such"]
	assert.False(t, missing)

	// empty input は早期 return
	out, err = repo.CountReadsByAnnouncementIDs(nil)
	require.NoError(t, err)
	assert.Empty(t, out)
}

// #2106 N7: ListGlobal は isActive を等値フィルタで扱う (false=inactive のみ、active を混ぜない)。
func TestAnnouncementRepository_ListGlobal_IsActiveFilter(t *testing.T) {
	repo := NewAnnouncementRepository(testDB)
	active := &model.Announcement{ID: "ann_n7_act", Title: "A", Text: "t", Icon: "info", Display: "normal", IsActive: true}
	inactive := &model.Announcement{ID: "ann_n7_inact", Title: "B", Text: "t", Icon: "info", Display: "normal", IsActive: false}
	require.NoError(t, repo.Create(active))
	require.NoError(t, repo.Create(inactive))
	// GORM は bool zero-value (false) を Create で省略し column default:true を当てるため、
	// inactive を確実に isActive=false にするには明示 UpdateFields する。
	require.NoError(t, repo.UpdateFields(inactive.ID, map[string]any{"isActive": false}))
	defer cleanupAnnouncement(t, active.ID)
	defer cleanupAnnouncement(t, inactive.ID)

	contains := func(items []*model.Announcement, id string) bool {
		for _, a := range items {
			if a.ID == id {
				return true
			}
		}
		return false
	}

	got, err := repo.ListGlobal(true, 100, 0, "", "")
	require.NoError(t, err)
	assert.True(t, contains(got, "ann_n7_act"), "isActive=true returns active")
	assert.False(t, contains(got, "ann_n7_inact"), "isActive=true excludes inactive")

	got, err = repo.ListGlobal(false, 100, 0, "", "")
	require.NoError(t, err)
	assert.True(t, contains(got, "ann_n7_inact"), "isActive=false returns inactive")
	assert.False(t, contains(got, "ann_n7_act"), "isActive=false must NOT mix in active announcements")
}
