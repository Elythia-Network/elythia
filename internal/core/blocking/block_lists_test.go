package blocking_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/blocking"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

// loggingUserListRepo records the list lookups Block makes and can fail them.
type loggingUserListRepo struct {
	*testutil.MockUserListRepository
	log        *eventLog
	failLookup bool
	// failFirstRemove はリストの順序に依らず最初の RemoveMember を失敗させ、
	// その list ID を failedList に残す。
	failFirstRemove bool
	failedList      string
}

func (r *loggingUserListRepo) ListsContainingMember(ownerID, memberUserID string) ([]*model.UserList, error) {
	if r.log != nil {
		r.log.entries = append(r.log.entries, "lists of "+ownerID+" containing "+memberUserID)
	}
	if r.failLookup {
		return nil, errStub
	}
	return r.MockUserListRepository.ListsContainingMember(ownerID, memberUserID)
}

func (r *loggingUserListRepo) RemoveMember(listID, userID string) error {
	if r.failFirstRemove && r.failedList == "" {
		r.failedList = listID
		return errStub
	}
	return r.MockUserListRepository.RemoveMember(listID, userID)
}

func seedList(t *testing.T, repo *testutil.MockUserListRepository, listID, ownerID string, members ...string) {
	t.Helper()
	require.NoError(t, repo.Create(&model.UserList{ID: listID, UserID: ownerID, Name: listID}))
	for _, m := range members {
		require.NoError(t, repo.AddMember(&model.UserListMembership{ID: listID + "-" + m, UserListID: listID, UserID: m}))
	}
}

func listMembers(repo *testutil.MockUserListRepository, listID string) []string {
	var out []string
	for _, m := range repo.Members {
		if m.UserListID == listID {
			out = append(out, m.UserID)
		}
	}
	return out
}

// 本家 UserBlockingService.block は removeFromList(blockee, blocker) を呼び、
// ブロックされた側 (bob) が持つ全てのリストからブロックした人 (alice) を外す。
// 逆向き (alice のリストの bob) と、第三者のリストは触らない。インポート
// (silent) でも外す (本家の removeFromList は silent を受け取らない)。
func TestBlock_RemovesBlockerFromBlockeeLists(t *testing.T) {
	for _, silent := range []bool{false, true} {
		svc, ur, _, _ := newSvc(t)
		for _, id := range []string{"alice", "bob", "carol", "dave"} {
			addUser(ur, id)
		}
		lists := testutil.NewMockUserListRepository()
		seedList(t, lists, "bob1", "bob", "alice", "carol")
		seedList(t, lists, "bob2", "bob", "alice")
		seedList(t, lists, "alice1", "alice", "bob")
		seedList(t, lists, "dave1", "dave", "alice")
		svc.SetUserListRepo(lists)

		block := svc.Block
		if silent {
			block = svc.BlockSilent
		}
		_, err := block("alice", "bob")
		require.NoError(t, err)

		assert.Equal(t, []string{"carol"}, listMembers(lists, "bob1"), "silent=%t", silent)
		assert.Empty(t, listMembers(lists, "bob2"), "silent=%t", silent)
		assert.Equal(t, []string{"bob"}, listMembers(lists, "alice1"), "the blocker's own lists are untouched")
		assert.Equal(t, []string{"alice"}, listMembers(lists, "dave1"), "third-party lists are untouched")
	}
}

// リストの片付けに失敗しても block は成立し、残りのリストも外し、Block も
// 配送する (best-effort)。
func TestBlock_ListCleanupErrorsAreBestEffort(t *testing.T) {
	t.Run("lookup fails", func(t *testing.T) {
		svc, ur, _, _ := newSvc(t)
		addUser(ur, "alice")
		addUser(ur, "bob")
		repo := &loggingUserListRepo{MockUserListRepository: testutil.NewMockUserListRepository(), failLookup: true}
		seedList(t, repo.MockUserListRepository, "bob1", "bob", "alice")
		svc.SetUserListRepo(repo)
		hook := &recordingFederationHook{}
		svc.SetFederationHook(hook)

		_, err := svc.Block("alice", "bob")
		require.NoError(t, err)
		assert.Equal(t, [][2]string{{"alice", "bob"}}, hook.blocked)
	})
	t.Run("one removal fails", func(t *testing.T) {
		svc, ur, _, _ := newSvc(t)
		addUser(ur, "alice")
		addUser(ur, "bob")
		repo := &loggingUserListRepo{
			MockUserListRepository: testutil.NewMockUserListRepository(),
			failFirstRemove:        true,
		}
		seedList(t, repo.MockUserListRepository, "bob1", "bob", "alice")
		seedList(t, repo.MockUserListRepository, "bob2", "bob", "alice")
		svc.SetUserListRepo(repo)

		_, err := svc.Block("alice", "bob")
		require.NoError(t, err)
		require.NotEmpty(t, repo.failedList)
		other := "bob2"
		if repo.failedList == "bob2" {
			other = "bob1"
		}
		assert.Equal(t, []string{"alice"}, listMembers(repo.MockUserListRepository, repo.failedList))
		assert.Empty(t, listMembers(repo.MockUserListRepository, other), "later lists are still cleaned up")
	})
}

func TestHasUserListRepo(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	assert.False(t, svc.HasUserListRepo())
	svc.SetUserListRepo(testutil.NewMockUserListRepository())
	assert.True(t, svc.HasUserListRepo())
	var empty blocking.Service
	empty.SetUnfollower(loggingUnfollower{&eventLog{}})
	assert.False(t, empty.HasUserListRepo(), "別の依存だけを配線しても false")
}
