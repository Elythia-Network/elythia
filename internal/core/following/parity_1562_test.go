package following_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/following"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// IsFollowing は following/update の NOT_FOLLOWING 判定に使う (#1562)。
func TestIsFollowing(t *testing.T) {
	svc, ur, _, _ := newSvc(t)
	addUser(t, ur, "alice", false)
	addUser(t, ur, "bob", false)

	ok, err := svc.IsFollowing("alice", "bob")
	require.NoError(t, err)
	assert.False(t, ok)

	_, err = svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err)

	ok, err = svc.IsFollowing("alice", "bob")
	require.NoError(t, err)
	assert.True(t, ok)
	// 逆方向は false のまま
	ok, err = svc.IsFollowing("bob", "alice")
	require.NoError(t, err)
	assert.False(t, ok)
}
