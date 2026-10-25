package blocking

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/testutil"
)

// NewService は受け取った依存を service に持たせる (#3825)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewServiceKeepsDependencies(t *testing.T) {
	unfollower := &constructorUnfollower{}
	canceller := &constructorCanceller{}
	lists := testutil.NewMockUserListRepository()
	rooms := testutil.NewMockChatRepository()
	svc := NewService(nil, nil, nil, unfollower, canceller, lists, rooms)
	assert.Same(t, unfollower, svc.unfollower)
	assert.Same(t, canceller, svc.followRequestCanceller)
	assert.Same(t, lists, svc.userListRepo)
	assert.Same(t, rooms, svc.roomInvitations)
}

type constructorUnfollower struct{}

func (*constructorUnfollower) UnfollowForBlock(string, string, bool) error { return nil }

type constructorCanceller struct{}

func (*constructorCanceller) CancelFollowRequestsBetween(string, string, bool) error { return nil }
