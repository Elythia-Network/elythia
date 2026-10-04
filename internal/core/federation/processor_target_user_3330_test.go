package federation_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/federation"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

// targetUserURICases are object URIs naming the local user bob the way
// upstream ApDbResolverService.getUserFromApId reads them (parseUri: the host
// decides locality, the id is the path segment after `users`).
var targetUserURICases = []struct {
	name string
	uri  string
}{
	{name: "query string is not part of the id", uri: "https://example.com/users/bob?foo=1"},
	{name: "fragment is not part of the id", uri: "https://example.com/users/bob#main-key"},
	{name: "trailing slash", uri: "https://example.com/users/bob/"},
	{name: "http scheme on the local host", uri: "http://example.com/users/bob"},
	{name: "sub path under the user", uri: "https://example.com/users/bob/outbox"},
}

const (
	targetTestAliceID  = "alice1"
	targetTestAliceURI = "https://remote.example/users/alice"
)

// seedTargetUsers registers the remote actor alice and the local user bob
// (uri NULL like production local rows).
func seedTargetUsers(p *federation.Processor, repo *testutil.MockUserRepository, bobDeleted bool) {
	// 本番と同じく base URL を配線する。旧実装 (prefix 一致) はこれが無いと
	// 何も当てないので、配線した状態で本家との差を見る。
	p.SetLocalBaseURL("https://example.com")
	aliceURI := targetTestAliceURI
	host := "remote.example"
	repo.Users[targetTestAliceID] = &model.User{ID: targetTestAliceID, Username: "alice", UsernameLower: "alice", URI: &aliceURI, Host: &host}
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob", IsDeleted: bobDeleted}
}

func followBody(object string) []byte {
	return fmt.Appendf(nil, `{"type":"Follow","actor":%q,"object":%q}`, targetTestAliceURI, object)
}

func undoBody(innerType, object string) []byte {
	return fmt.Appendf(nil, `{"type":"Undo","actor":%q,"object":{"type":%q,"actor":%q,"object":%q}}`,
		targetTestAliceURI, innerType, targetTestAliceURI, object)
}

func blockBody(object string) []byte {
	return fmt.Appendf(nil, `{"type":"Block","actor":%q,"object":%q}`, targetTestAliceURI, object)
}

func TestProcess_FollowTargetUserLikeGetUserFromApId(t *testing.T) {
	for _, tc := range targetUserURICases {
		t.Run(tc.name, func(t *testing.T) {
			p, repo, followingRepo, _ := newProcessor(t, aliceActor)
			seedTargetUsers(p, repo, false)
			require.NoError(t, p.Process(followBody(tc.uri)))
			require.Len(t, followingRepo.Followings, 1)
			for _, f := range followingRepo.Followings {
				assert.Equal(t, targetTestAliceID, f.FollowerID)
				assert.Equal(t, "bob", f.FolloweeID)
			}
		})
	}
}

func TestProcess_UndoFollowTargetUserLikeGetUserFromApId(t *testing.T) {
	for _, tc := range targetUserURICases {
		t.Run(tc.name, func(t *testing.T) {
			p, repo, followingRepo, _ := newProcessor(t, aliceActor)
			seedTargetUsers(p, repo, false)
			followingRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: targetTestAliceID, FolloweeID: "bob"}
			require.NoError(t, p.Process(undoBody("Follow", tc.uri)))
			assert.Empty(t, followingRepo.Followings)
		})
	}
}

func TestProcess_BlockTargetUserLikeGetUserFromApId(t *testing.T) {
	for _, tc := range targetUserURICases {
		t.Run(tc.name, func(t *testing.T) {
			p, repo, blockingRepo := newProcessorWithBlocking(t)
			seedTargetUsers(p, repo, false)
			require.NoError(t, p.Process(blockBody(tc.uri)))
			exists, err := blockingRepo.Exists(targetTestAliceID, "bob")
			require.NoError(t, err)
			assert.True(t, exists)
		})
	}
}

func TestProcess_UndoBlockTargetUserLikeGetUserFromApId(t *testing.T) {
	for _, tc := range targetUserURICases {
		t.Run(tc.name, func(t *testing.T) {
			p, repo, blockingRepo := newProcessorWithBlocking(t)
			seedTargetUsers(p, repo, false)
			blockingRepo.Blockings["b1"] = &model.Blocking{ID: "b1", BlockerID: targetTestAliceID, BlockeeID: "bob"}
			require.NoError(t, p.Process(undoBody("Block", tc.uri)))
			assert.Empty(t, blockingRepo.Blockings)
		})
	}
}

// A deleted local user is not found (upstream `isDeleted: false`), so the
// activity is skipped and acked instead of acting on the deleted user.
func TestProcess_TargetUserDeletedIsSkipped(t *testing.T) {
	const bobURI = "https://example.com/users/bob"

	t.Run("Follow", func(t *testing.T) {
		p, repo, followingRepo, _ := newProcessor(t, aliceActor)
		seedTargetUsers(p, repo, true)
		require.NoError(t, p.Process(followBody(bobURI)))
		assert.Empty(t, followingRepo.Followings)
	})
	t.Run("Undo(Follow)", func(t *testing.T) {
		p, repo, followingRepo, _ := newProcessor(t, aliceActor)
		seedTargetUsers(p, repo, true)
		followingRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: targetTestAliceID, FolloweeID: "bob"}
		require.NoError(t, p.Process(undoBody("Follow", bobURI)))
		assert.Len(t, followingRepo.Followings, 1)
	})
	t.Run("Block", func(t *testing.T) {
		p, repo, blockingRepo := newProcessorWithBlocking(t)
		seedTargetUsers(p, repo, true)
		require.NoError(t, p.Process(blockBody(bobURI)))
		assert.Empty(t, blockingRepo.Blockings)
	})
	t.Run("Undo(Block)", func(t *testing.T) {
		p, repo, blockingRepo := newProcessorWithBlocking(t)
		seedTargetUsers(p, repo, true)
		blockingRepo.Blockings["b1"] = &model.Blocking{ID: "b1", BlockerID: targetTestAliceID, BlockeeID: "bob"}
		require.NoError(t, p.Process(undoBody("Block", bobURI)))
		assert.Len(t, blockingRepo.Blockings, 1)
	})
}

// A local URI outside `/users/` names nobody (upstream `parsed.type !==
// 'users'` → null), so the activity is skipped.
func TestProcess_TargetUserNonUsersLocalPathIsSkipped(t *testing.T) {
	p, repo, followingRepo, _ := newProcessor(t, aliceActor)
	seedTargetUsers(p, repo, false)
	require.NoError(t, p.Process(followBody("https://example.com/notes/bob")))
	assert.Empty(t, followingRepo.Followings)
}

// A lookup failure other than not-found is propagated so that the inbox job
// is retried (same treatment as Accept / Reject, #3115).
func TestProcess_TargetUserLookupErrorPropagates(t *testing.T) {
	const bobURI = "https://example.com/users/bob"
	dbErr := errors.New("connection refused")
	bodies := map[string][]byte{
		"Follow":       followBody(bobURI),
		"Undo(Follow)": undoBody("Follow", bobURI),
		"Block":        blockBody(bobURI),
		"Undo(Block)":  undoBody("Block", bobURI),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			p, repo, _ := newProcessorWithBlocking(t)
			seedTargetUsers(p, repo, false)
			repo.FindErr = dbErr
			err := p.Process(body)
			require.Error(t, err)
			assert.ErrorIs(t, err, dbErr)
		})
	}
}
