package meta

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// **`requireSetup` をサーバー側の受け入れ条件と揃える (#3037)。**
//
// `admin/accounts/create` の初回セットアップ窓は「`rootUserId` 未設定 **かつ**
// ローカル利用者 0」でしか開かない。フロントの判定を `rootUserId` だけに
// しておくと、TS から引き継いだ DB などで **セットアップ画面が出続けて作成
// ボタンが必ず `ACCESS_DENIED` を返す**。
func TestRequireSetup(t *testing.T) {
	rootID := "someroot"

	t.Run("rootUserId 設定済みなら false", func(t *testing.T) {
		d := NewSetupDetector(func() (int64, error) { return 0, nil })
		assert.False(t, d.RequireSetup(&model.Meta{RootUserID: &rootID}))
	})

	t.Run("未設定 + 利用者 0 なら true", func(t *testing.T) {
		d := NewSetupDetector(func() (int64, error) { return 0, nil })
		assert.True(t, d.RequireSetup(&model.Meta{}))
	})

	t.Run("未設定でも利用者が居れば false", func(t *testing.T) {
		d := NewSetupDetector(func() (int64, error) { return 3, nil })
		assert.False(t, d.RequireSetup(&model.Meta{}), "作成ボタンが必ず失敗する画面を出している")
	})

	// **一度 false になったら戻らない。** 利用者は増える一方なので、観測した
	// 時点で確定してよい。以後 COUNT を発行しないための掛け金。
	t.Run("観測したら以後は数えない", func(t *testing.T) {
		calls := 0
		d := NewSetupDetector(func() (int64, error) { calls++; return 1, nil })
		assert.False(t, d.RequireSetup(&model.Meta{}))
		assert.False(t, d.RequireSetup(&model.Meta{}))
		assert.Equal(t, 1, calls, "毎回 COUNT を発行している")
	})

	// 数え方が無ければ (nil の値・零値・nil の関数) 従来どおり `rootUserId` だけで判定する。
	t.Run("数え方が無ければ従来どおり", func(t *testing.T) {
		for _, d := range []*SetupDetector{nil, {}, NewSetupDetector(nil)} {
			assert.True(t, d.RequireSetup(&model.Meta{}))
			assert.False(t, d.RequireSetup(&model.Meta{RootUserID: &rootID}))
		}
	})

	// **数えられないときは促さない。** 促して失敗させるより出さないほうが
	// 害が小さい (本当に初回なら次のリクエストで出る)。
	t.Run("数えられなければ false", func(t *testing.T) {
		d := NewSetupDetector(func() (int64, error) { return 0, errors.New("db down") })
		assert.False(t, d.RequireSetup(&model.Meta{}))
	})

	t.Run("nil meta は false", func(t *testing.T) {
		assert.False(t, NewSetupDetector(func() (int64, error) { return 0, nil }).RequireSetup(nil))
	})
}

// **印は組み立てごとの値で、次の世代へ持ち越さない (#3803)。**
//
// 以前はパッケージの変数だったので、利用者の居る DB で一度観測すると、DB を
// 戻して組み立て直した後の世代でも数えずに false を返した。利用者の居ない DB
// へ戻した後は、セットアップ画面を出すべきなのに出なくなる。
func TestRequireSetup_LatchIsPerDetector(t *testing.T) {
	users := int64(1)
	calls := 0
	count := func() (int64, error) { calls++; return users, nil }

	first := NewSetupDetector(count)
	require.False(t, first.RequireSetup(&model.Meta{}))
	require.False(t, first.RequireSetup(&model.Meta{}))
	require.Equal(t, 1, calls, "同じ値の中では印が効いて数え直さない (前提)")

	// 利用者の居ない DB へ戻し、組み立て直した。
	users = 0
	second := NewSetupDetector(count)
	assert.True(t, second.RequireSetup(&model.Meta{}), "前の世代の印を持ち越している")
	assert.Equal(t, 2, calls, "新しい世代は最初のリクエストで数え直す")

	// 前の世代の値は、自分の印のまま数えない。
	assert.False(t, first.RequireSetup(&model.Meta{}))
	assert.Equal(t, 2, calls)
}
