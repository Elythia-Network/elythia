package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanupUserList(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "user_list_membership" WHERE "userListId" = ?`, id)
	testDB.Exec(`DELETE FROM "user_list" WHERE id = ?`, id)
}

func TestUserListRepository_CRUD(t *testing.T) {
	repo := NewUserListRepository(testDB)
	createTestUser(t, "ul_owner")

	list := &model.UserList{ID: "ul_1", UserID: "ul_owner", Name: "My List"}
	require.NoError(t, repo.Create(list))
	defer cleanupUserList(t, list.ID)

	found, err := repo.FindByID(list.ID)
	require.NoError(t, err)
	assert.Equal(t, "My List", found.Name)

	lists, err := repo.ListByUser("ul_owner")
	require.NoError(t, err)
	assert.Len(t, lists, 1)

	count, err := repo.CountByUser("ul_owner")
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	require.NoError(t, repo.Delete(list.ID))
	_, err = repo.FindByID(list.ID)
	assert.Error(t, err)
}

func TestUserListRepository_FindByID_NotFound(t *testing.T) {
	repo := NewUserListRepository(testDB)
	_, err := repo.FindByID("ghost")
	assert.Error(t, err)
}

func TestUserListRepository_Members(t *testing.T) {
	repo := NewUserListRepository(testDB)
	createTestUser(t, "ul_o2")
	createTestUser(t, "ul_m1")

	list := &model.UserList{ID: "ul_2", UserID: "ul_o2", Name: "Test"}
	require.NoError(t, repo.Create(list))
	defer cleanupUserList(t, list.ID)

	m := &model.UserListMembership{ID: "ulm_1", UserListID: list.ID, UserID: "ul_m1"}
	require.NoError(t, repo.AddMember(m))

	members, err := repo.ListMembers(list.ID)
	require.NoError(t, err)
	assert.Len(t, members, 1)

	count, err := repo.CountMembers(list.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	require.NoError(t, repo.RemoveMember(list.ID, "ul_m1"))
	members, _ = repo.ListMembers(list.ID)
	assert.Empty(t, members)
}

func TestUserListRepository_ListByUser_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewUserListRepository(testDB.WithContext(ctx))
	_, err := repo.ListByUser("x")
	assert.Error(t, err)
	_, err = repo.CountByUser("x")
	assert.Error(t, err)
}

func TestUserListRepository_ListMembers_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewUserListRepository(testDB.WithContext(ctx))
	_, err := repo.ListMembers("x")
	assert.Error(t, err)
	_, err = repo.CountMembers("x")
	assert.Error(t, err)
}

// cancel 済 context で ListMembersByListIDs が err を伝播することを確認する
// (handler 側 best-effort fallback の発動経路担保、#876)。
func TestUserListRepository_ListMembersByListIDs_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewUserListRepository(testDB.WithContext(ctx))
	_, err := repo.ListMembersByListIDs([]string{"x"})
	assert.Error(t, err)
}

// users/lists/list の N+1 を消す batch fetch (#876)。空 input は空 map、
// 複数 list 跨ぎは listID -> userIDs に正しく分配されることを確認する。
func TestUserListRepository_ListMembersByListIDs(t *testing.T) {
	repo := NewUserListRepository(testDB)
	createTestUser(t, "ul_b_o")
	createTestUser(t, "ul_b_m1")
	createTestUser(t, "ul_b_m2")

	// 空 input → 空 map (query を発行しない short-circuit)
	out, err := repo.ListMembersByListIDs(nil)
	require.NoError(t, err)
	assert.Empty(t, out)
	out, err = repo.ListMembersByListIDs([]string{})
	require.NoError(t, err)
	assert.Empty(t, out)

	// list A: m1 + m2、list B: m1 のみ
	listA := &model.UserList{ID: "ul_b_a", UserID: "ul_b_o", Name: "A"}
	require.NoError(t, repo.Create(listA))
	defer cleanupUserList(t, listA.ID)
	listB := &model.UserList{ID: "ul_b_b", UserID: "ul_b_o", Name: "B"}
	require.NoError(t, repo.Create(listB))
	defer cleanupUserList(t, listB.ID)

	require.NoError(t, repo.AddMember(&model.UserListMembership{ID: "ulm_b_1", UserListID: listA.ID, UserID: "ul_b_m1"}))
	require.NoError(t, repo.AddMember(&model.UserListMembership{ID: "ulm_b_2", UserListID: listA.ID, UserID: "ul_b_m2"}))
	require.NoError(t, repo.AddMember(&model.UserListMembership{ID: "ulm_b_3", UserListID: listB.ID, UserID: "ul_b_m1"}))

	out, err = repo.ListMembersByListIDs([]string{listA.ID, listB.ID})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"ul_b_m1", "ul_b_m2"}, out[listA.ID])
	assert.ElementsMatch(t, []string{"ul_b_m1"}, out[listB.ID])

	// member の居ない list は map に key が出ない (caller 側で nil slice
	// として扱う前提、entity.PackUserList は [] で serialize)。
	listEmpty := &model.UserList{ID: "ul_b_empty", UserID: "ul_b_o", Name: "E"}
	require.NoError(t, repo.Create(listEmpty))
	defer cleanupUserList(t, listEmpty.ID)
	out, err = repo.ListMembersByListIDs([]string{listEmpty.ID})
	require.NoError(t, err)
	_, ok := out[listEmpty.ID]
	assert.False(t, ok)
}

// TestUserListRepository_ListMembershipsPage は #1550 の get-memberships 用
// cursor pagination + User preload を検証する。
func TestUserListRepository_ListMembershipsPage(t *testing.T) {
	repo := NewUserListRepository(testDB)
	createTestUser(t, "ulp_o")
	createTestUser(t, "ulp_m1")
	createTestUser(t, "ulp_m2")
	createTestUser(t, "ulp_m3")
	list := &model.UserList{ID: "ulp_list", UserID: "ulp_o", Name: "L"}
	require.NoError(t, repo.Create(list))
	defer cleanupUserList(t, list.ID)
	require.NoError(t, repo.AddMember(&model.UserListMembership{ID: "ulm_p_1", UserListID: list.ID, UserID: "ulp_m1"}))
	require.NoError(t, repo.AddMember(&model.UserListMembership{ID: "ulm_p_2", UserListID: list.ID, UserID: "ulp_m2"}))
	require.NoError(t, repo.AddMember(&model.UserListMembership{ID: "ulm_p_3", UserListID: list.ID, UserID: "ulp_m3"}))

	ids := func(rows []*model.UserListMembership) []string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.ID)
		}
		return out
	}

	// default: id DESC、User preload あり。
	rows, err := repo.ListMembershipsPage(list.ID, "", "", 30)
	require.NoError(t, err)
	assert.Equal(t, []string{"ulm_p_3", "ulm_p_2", "ulm_p_1"}, ids(rows))
	require.NotNil(t, rows[0].User, "User が preload される")
	assert.Equal(t, "ulp_m3", rows[0].User.ID)

	// limit cap。
	rows, err = repo.ListMembershipsPage(list.ID, "", "", 2)
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	// untilID: cursor 未満 (DESC)。
	rows, err = repo.ListMembershipsPage(list.ID, "", "ulm_p_3", 30)
	require.NoError(t, err)
	assert.Equal(t, []string{"ulm_p_2", "ulm_p_1"}, ids(rows))

	// sinceID-only: cursor 超 + ASC。
	rows, err = repo.ListMembershipsPage(list.ID, "ulm_p_1", "", 30)
	require.NoError(t, err)
	assert.Equal(t, []string{"ulm_p_2", "ulm_p_3"}, ids(rows))
}

// TestUserListRepository_AddMember_Duplicate verifies the (userListId, userId)
// unique-constraint violation is mapped to ErrUserListDuplicateMember (#396),
// so the API layer can return the TS-compat ALREADY_ADDED error.
func TestUserListRepository_AddMember_Duplicate(t *testing.T) {
	repo := NewUserListRepository(testDB)
	createTestUser(t, "ul_o3")
	createTestUser(t, "ul_m3")

	list := &model.UserList{ID: "ul_3", UserID: "ul_o3", Name: "Dup"}
	require.NoError(t, repo.Create(list))
	defer cleanupUserList(t, list.ID)

	first := &model.UserListMembership{ID: "ulm_3a", UserListID: list.ID, UserID: "ul_m3"}
	require.NoError(t, repo.AddMember(first))

	dup := &model.UserListMembership{ID: "ulm_3b", UserListID: list.ID, UserID: "ul_m3"}
	err := repo.AddMember(dup)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUserListDuplicateMember),
		"既 member の AddMember は ErrUserListDuplicateMember を返すこと, got: %v", err)
}

// ListIDsAndOwnersByMember は fanoutToUserLists が followers visibility note
// の per-list owner follow gate を 1 query で済ませるために導入 (#1465)。
// 同じ member が複数 owner の list に属するケース、member の居ない list が
// 出てこないケース、空 result のケースを check する。
func TestUserListRepository_ListIDsAndOwnersByMember(t *testing.T) {
	repo := NewUserListRepository(testDB)
	createTestUser(t, "ul_lo_o1")
	createTestUser(t, "ul_lo_o2")
	createTestUser(t, "ul_lo_m1")
	createTestUser(t, "ul_lo_m2")

	// member 不在 → 空 map
	out, err := repo.ListIDsAndOwnersByMember("nonexistent")
	require.NoError(t, err)
	assert.Empty(t, out)

	// o1 が list A1 (m1, m2) と list A2 (m1) を所有、o2 が list B1 (m1) を所有
	listA1 := &model.UserList{ID: "ul_lo_a1", UserID: "ul_lo_o1", Name: "A1"}
	require.NoError(t, repo.Create(listA1))
	defer cleanupUserList(t, listA1.ID)
	listA2 := &model.UserList{ID: "ul_lo_a2", UserID: "ul_lo_o1", Name: "A2"}
	require.NoError(t, repo.Create(listA2))
	defer cleanupUserList(t, listA2.ID)
	listB1 := &model.UserList{ID: "ul_lo_b1", UserID: "ul_lo_o2", Name: "B1"}
	require.NoError(t, repo.Create(listB1))
	defer cleanupUserList(t, listB1.ID)
	// m1 が含まれない list (regression guard: 含まれない list は出てこない)
	listC := &model.UserList{ID: "ul_lo_c", UserID: "ul_lo_o2", Name: "C"}
	require.NoError(t, repo.Create(listC))
	defer cleanupUserList(t, listC.ID)

	require.NoError(t, repo.AddMember(&model.UserListMembership{ID: "ulm_lo_1", UserListID: listA1.ID, UserID: "ul_lo_m1"}))
	require.NoError(t, repo.AddMember(&model.UserListMembership{ID: "ulm_lo_2", UserListID: listA1.ID, UserID: "ul_lo_m2"}))
	require.NoError(t, repo.AddMember(&model.UserListMembership{ID: "ulm_lo_3", UserListID: listA2.ID, UserID: "ul_lo_m1"}))
	require.NoError(t, repo.AddMember(&model.UserListMembership{ID: "ulm_lo_4", UserListID: listB1.ID, UserID: "ul_lo_m1"}))
	// listC は member 無し

	// m1 視点: A1 (o1) + A2 (o1) + B1 (o2) の 3 件、C は出ない
	out, err = repo.ListIDsAndOwnersByMember("ul_lo_m1")
	require.NoError(t, err)
	require.Len(t, out, 3)
	assert.Equal(t, "ul_lo_o1", out[listA1.ID])
	assert.Equal(t, "ul_lo_o1", out[listA2.ID])
	assert.Equal(t, "ul_lo_o2", out[listB1.ID])
	_, hasC := out[listC.ID]
	assert.False(t, hasC, "member が居ない list は map に含まれない")

	// m2 視点: A1 のみ
	out, err = repo.ListIDsAndOwnersByMember("ul_lo_m2")
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "ul_lo_o1", out[listA1.ID])
}

// 不正な query (= 壊れた DB session) で error が返ることを confirm。
// MustOpenTestDB で取った session を意図的に close して reuse する形で
// session 失敗を再現する。
func TestUserListRepository_ListIDsAndOwnersByMember_Error(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // ctx を canceled state にして session 経由の query を fail させる
	repo := NewUserListRepository(testDB.WithContext(ctx))
	_, err := repo.ListIDsAndOwnersByMember("anyone")
	assert.Error(t, err)
}

// ListMembershipsByUser は userId 起点で membership を全件返す (#2419 アカウント
// 移行のリスト張り替え用)。既存の membership 系は listID 起点しか無かった。
func TestUserListRepository_ListMembershipsByUser(t *testing.T) {
	repo := NewUserListRepository(testDB)
	owner := insertTestUser(t, "u_ulmbu_o", "ulmbuo")
	defer cleanupUser(t, owner.ID)
	member := insertTestUser(t, "u_ulmbu_m", "ulmbum")
	defer cleanupUser(t, member.ID)
	stranger := insertTestUser(t, "u_ulmbu_x", "ulmbux")
	defer cleanupUser(t, stranger.ID)

	listA := &model.UserList{ID: "ulmbu_a", UserID: owner.ID, Name: "A"}
	require.NoError(t, repo.Create(listA))
	defer testDB.Exec(`DELETE FROM "user_list" WHERE id = ?`, listA.ID)
	listB := &model.UserList{ID: "ulmbu_b", UserID: owner.ID, Name: "B"}
	require.NoError(t, repo.Create(listB))
	defer testDB.Exec(`DELETE FROM "user_list" WHERE id = ?`, listB.ID)

	for _, m := range []*model.UserListMembership{
		{ID: "ulmbu_m1", UserID: member.ID, UserListID: listA.ID, UserListUserID: &owner.ID},
		{ID: "ulmbu_m2", UserID: member.ID, UserListID: listB.ID, UserListUserID: &owner.ID},
		// 別ユーザーの membership は対象外。
		{ID: "ulmbu_m3", UserID: stranger.ID, UserListID: listA.ID, UserListUserID: &owner.ID},
	} {
		require.NoError(t, repo.AddMember(m))
		defer testDB.Exec(`DELETE FROM "user_list_membership" WHERE id = ?`, m.ID)
	}

	rows, err := repo.ListMembershipsByUser(member.ID)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	var listIDs []string
	for _, r := range rows {
		listIDs = append(listIDs, r.UserListID)
		assert.Equal(t, member.ID, r.UserID)
	}
	assert.ElementsMatch(t, []string{listA.ID, listB.ID}, listIDs)

	// userID 空は nil 返却。
	rows, err = repo.ListMembershipsByUser("")
	require.NoError(t, err)
	assert.Nil(t, rows)
}
