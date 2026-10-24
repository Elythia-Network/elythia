package move

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/testutil"
)

// NewService は受け取った引き継ぎの依存を service に持たせる (#3823)。依存を
// 構築子の必須の引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを
// 見なくなった。構築子が引数を捨てても build は通るので、ここで固定する。
//
// 以前の述語 (HasCarryOverRepos) が一緒に見ていた blockQueue は setter のまま。
// 本番の組み立てで入っていることは TestBuiltHandlersHaveNoNilDependencies
// (internal/server) が見る。
func TestNewServiceKeepsCarryOverDependencies(t *testing.T) {
	blocks := testutil.NewMockBlockingRepository()
	mutes := testutil.NewMockMutingRepository()
	lists := testutil.NewMockUserListRepository()
	gen, err := id.NewGenerator("aidx")
	assert.NoError(t, err)
	s := NewService(nil, nil, nil, nil, nil, nil, blocks, mutes, lists, gen)
	assert.Same(t, blocks, s.blockingRepo)
	assert.Same(t, mutes, s.mutingRepo)
	assert.Same(t, lists, s.userListRepo)
	assert.Equal(t, gen, s.idGen)
}
