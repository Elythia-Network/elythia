package antenna

import (
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSvcWithForeignList returns a service whose user-list repository holds a
// list "theirs" owned by "other" that has "alice" as a member.
func newSvcWithForeignList(t *testing.T) (*Service, *testutil.MockAntennaRepository) {
	t.Helper()
	svc, repo := newSvc(t)
	lists := testutil.NewMockUserListRepository()
	svc.SetUserListRepo(lists)
	require.NoError(t, lists.Create(&model.UserList{ID: "theirs", UserID: "other", Name: "theirs"}))
	require.NoError(t, lists.AddMember(&model.UserListMembership{UserListID: "theirs", UserID: "alice"}))
	return svc, repo
}

// **src が list 以外のときは userListId を保存しないこと。**
//
// 保存すると所有者検証を通っていない値が残り、後で src だけを list に
// 切り替えると他人の list を参照できる。upstream create.ts も検証済みの
// ときだけ保存する (`userList ? userList.id : null`)。
func TestCreate_NonListSourceDoesNotStoreUserListID(t *testing.T) {
	svc, _ := newSvcWithForeignList(t)
	theirs := "theirs"
	a, err := svc.Create(CreateInput{OwnerID: "u1", Name: "a", Src: model.AntennaSourceHome, UserListID: &theirs})
	require.NoError(t, err)
	assert.Nil(t, a.UserListID)
}

// **src だけを list に切り替えたとき、既存の userListId も検証すること。**
func TestUpdate_SwitchToListValidatesExistingUserListID(t *testing.T) {
	svc, repo := newSvcWithForeignList(t)
	theirs := "theirs"
	// 検証が入る前に保存された (= 未検証の) 値を持つアンテナ。
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1", Name: "a", Src: model.AntennaSourceHome, UserListID: &theirs}

	list := model.AntennaSourceList
	_, err := svc.Update("u1", "a1", UpdateInput{Src: &list})
	require.ErrorIs(t, err, ErrNoSuchUserList)
	assert.Equal(t, model.AntennaSourceHome, repo.Antennas["a1"].Src, "更新しないこと")
}

// src が list のまま userListId を送らない更新でも、既存の値を検証すること。
func TestUpdate_ListSourceValidatesExistingUserListID(t *testing.T) {
	svc, repo := newSvcWithForeignList(t)
	theirs := "theirs"
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1", Name: "a", Src: model.AntennaSourceList, UserListID: &theirs}

	name := "renamed"
	_, err := svc.Update("u1", "a1", UpdateInput{Name: &name})
	require.ErrorIs(t, err, ErrNoSuchUserList)
}

// list 以外の src へ更新するときは userListId を null にすること。送られた
// 値も既存の値も保存しない。
func TestUpdate_NonListSourceClearsUserListID(t *testing.T) {
	svc, repo := newSvcWithForeignList(t)
	theirs := "theirs"
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1", Name: "a", Src: model.AntennaSourceList, UserListID: &theirs}

	home := model.AntennaSourceHome
	_, err := svc.Update("u1", "a1", UpdateInput{Src: &home, UserListID: &theirs})
	require.NoError(t, err)
	v, ok := repo.LastUpdates["userListId"]
	require.True(t, ok, "userListId を書くこと")
	assert.Nil(t, v)

	// userListId を送らなくても、既存の値は消す。
	repo.Antennas["a2"] = &model.Antenna{ID: "a2", UserID: "u1", Name: "b", Src: model.AntennaSourceHome, UserListID: &theirs}
	name := "renamed"
	_, err = svc.Update("u1", "a2", UpdateInput{Name: &name})
	require.NoError(t, err)
	v, ok = repo.LastUpdates["userListId"]
	require.True(t, ok)
	assert.Nil(t, v)
}

// 自分の list へ切り替える通常の更新は通り、値が保存されること。空文字列は
// clear の convention (#2106)。
func TestUpdate_ListSourceOwnList(t *testing.T) {
	svc, repo := newSvcWithForeignList(t)
	lists := svc.userListRepo.(*testutil.MockUserListRepository)
	require.NoError(t, lists.Create(&model.UserList{ID: "mine", UserID: "u1", Name: "mine"}))
	repo.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "u1", Name: "a", Src: model.AntennaSourceHome}

	list := model.AntennaSourceList
	mine := "mine"
	_, err := svc.Update("u1", "a1", UpdateInput{Src: &list, UserListID: &mine})
	require.NoError(t, err)
	assert.Equal(t, &mine, repo.LastUpdates["userListId"])

	empty := ""
	_, err = svc.Update("u1", "a1", UpdateInput{Src: &list, UserListID: &empty})
	require.NoError(t, err)
	v, ok := repo.LastUpdates["userListId"]
	require.True(t, ok)
	assert.Nil(t, v)

	// src も userListId も触らない更新では書かない。
	name := "renamed"
	repo.Antennas["a1"].UserListID = &mine
	repo.Antennas["a1"].Src = model.AntennaSourceList
	_, err = svc.Update("u1", "a1", UpdateInput{Name: &name})
	require.NoError(t, err)
	_, ok = repo.LastUpdates["userListId"]
	assert.False(t, ok)
}

// **照合時にも list の所有者を見ること。**
//
// 検証が入る前に保存された他人の userListId が残っていても、他人の list の
// メンバーのノートをアンテナに流さない。
func TestMatchNote_ListSource_ForeignListDoesNotMatch(t *testing.T) {
	svc, _ := newSvcWithForeignList(t)
	theirs := "theirs"
	a := &model.Antenna{ID: "a1", UserID: "u1", Src: model.AntennaSourceList, UserListID: &theirs, IsActive: true}
	text := "hi"
	n := &model.Note{ID: "n1", UserID: "alice", Text: &text, Visibility: model.NoteVisibilityPublic}
	assert.False(t, svc.matchNote(a, n, &model.User{ID: "alice", Username: "alice"}, newMatchMemo()))
	assert.False(t, svc.matchNote(a, n, &model.User{ID: "alice", Username: "alice"}, nil))

	// 同じ list でも所有者のアンテナなら一致する (memo のキーが owner を含む)。
	memo := newMatchMemo()
	own := &model.Antenna{ID: "a2", UserID: "other", Src: model.AntennaSourceList, UserListID: &theirs, IsActive: true}
	assert.False(t, svc.matchNote(a, n, &model.User{ID: "alice", Username: "alice"}, memo))
	assert.True(t, svc.matchNote(own, n, &model.User{ID: "alice", Username: "alice"}, memo))
}
