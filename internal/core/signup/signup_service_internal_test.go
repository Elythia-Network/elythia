package signup

// Internal (white-box) tests that need access to the unexported
// maybeCreateEd25519Keypair / txInserter for error-path coverage that is not
// reachable via the public Service API.

import (
	"errors"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// failingTxInserter satisfies txInserter and always returns a *gorm.DB with
// the supplied error. Used to exercise the tx-create error wrap path of
// maybeCreateEd25519Keypair (#1075 follow-up). PromotePending tx 経路の error
// path は idGen が毎回新規 ULID を生成する関係で integration test での再現
// (FK/PK violation seed 等) が困難なので unit test 側で cover する。
type failingTxInserter struct {
	err error
}

func (f *failingTxInserter) Create(_ any) *gorm.DB {
	return &gorm.DB{Error: f.err}
}

func TestMaybeCreateEd25519Keypair_TxCreateError(t *testing.T) {
	svc := &Service{
		keypairExtraRepo: testutil.NewMockUserKeypairExtraRepository(),
	}
	tx := &failingTxInserter{err: errors.New("tx create failed")}
	err := svc.maybeCreateEd25519Keypair(tx, "u1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "signup: create ed25519 keypair (tx)")
}

// keypairExtraRepo 未配線 (nil) なら tx エラーは発生しないで早期 return nil する
// guard も明示的に cover する (= "extra repo nil なら何もしない" semantics)。
func TestMaybeCreateEd25519Keypair_NilRepoIsNoOp(t *testing.T) {
	svc := &Service{} // keypairExtraRepo == nil
	tx := &failingTxInserter{err: errors.New("must not be called")}
	err := svc.maybeCreateEd25519Keypair(tx, "u1")
	assert.NoError(t, err)
}

// MinUsernameLength / MaxUsernameLength が localUsernamePattern と食い違って
// いないこと (#3015)。
//
// **`admin/update-meta` の範囲検証はこの定数を読む**ので、ずれると
// 「保存できるのに登録が全滅する値」または「保存できない正当な値」が生まれる。
// pattern 側を変えたときに気付けるよう、パターンそのものに当てて確かめる。
func TestUsernameLengthBoundsMatchPattern(t *testing.T) {
	ok := func(n int) bool {
		return localUsernamePattern.MatchString(strings.Repeat("a", n))
	}

	assert.True(t, ok(MinUsernameLength), "下限が pattern に通らない")
	assert.True(t, ok(MaxUsernameLength), "上限が pattern に通らない")
	assert.False(t, ok(MinUsernameLength-1), "下限より 1 短いものが pattern を通る")
	assert.False(t, ok(MaxUsernameLength+1), "上限より 1 長いものが pattern を通る")
}
