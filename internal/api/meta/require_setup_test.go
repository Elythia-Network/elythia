package meta

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

// **`requireSetup` をサーバー側の受け入れ条件と揃える (#3037)。**
//
// `admin/accounts/create` の初回セットアップ窓は「`rootUserId` 未設定 **かつ**
// ローカル利用者 0」でしか開かない。フロントの判定を `rootUserId` だけに
// しておくと、TS から引き継いだ DB などで **セットアップ画面が出続けて作成
// ボタンが必ず `ACCESS_DENIED` を返す**。
func TestRequireSetup(t *testing.T) {
	t.Cleanup(func() {
		SetLocalUserCounter(nil)
		ResetSetupLatchForTest()
	})
	rootID := "someroot"

	t.Run("rootUserId 設定済みなら false", func(t *testing.T) {
		ResetSetupLatchForTest()
		SetLocalUserCounter(func() (int64, error) { return 0, nil })
		assert.False(t, RequireSetup(&model.Meta{RootUserID: &rootID}))
	})

	t.Run("未設定 + 利用者 0 なら true", func(t *testing.T) {
		ResetSetupLatchForTest()
		SetLocalUserCounter(func() (int64, error) { return 0, nil })
		assert.True(t, RequireSetup(&model.Meta{}))
	})

	t.Run("未設定でも利用者が居れば false", func(t *testing.T) {
		ResetSetupLatchForTest()
		SetLocalUserCounter(func() (int64, error) { return 3, nil })
		assert.False(t, RequireSetup(&model.Meta{}), "作成ボタンが必ず失敗する画面を出している")
	})

	// **一度 false になったら戻らない。** 利用者は増える一方なので、観測した
	// 時点で確定してよい。以後 COUNT を発行しないための掛け金。
	t.Run("観測したら以後は数えない", func(t *testing.T) {
		ResetSetupLatchForTest()
		calls := 0
		SetLocalUserCounter(func() (int64, error) { calls++; return 1, nil })
		assert.False(t, RequireSetup(&model.Meta{}))
		assert.False(t, RequireSetup(&model.Meta{}))
		assert.Equal(t, 1, calls, "毎回 COUNT を発行している")
	})

	// counter 未配線なら従来どおり `rootUserId` だけで判定する。
	t.Run("counter 未配線は従来どおり", func(t *testing.T) {
		ResetSetupLatchForTest()
		SetLocalUserCounter(nil)
		assert.True(t, RequireSetup(&model.Meta{}))
		assert.False(t, RequireSetup(&model.Meta{RootUserID: &rootID}))
	})

	// **数えられないときは促さない。** 促して失敗させるより出さないほうが
	// 害が小さい (本当に初回なら次のリクエストで出る)。
	t.Run("数えられなければ false", func(t *testing.T) {
		ResetSetupLatchForTest()
		SetLocalUserCounter(func() (int64, error) { return 0, errors.New("db down") })
		assert.False(t, RequireSetup(&model.Meta{}))
	})

	t.Run("nil meta は false", func(t *testing.T) {
		ResetSetupLatchForTest()
		assert.False(t, RequireSetup(nil))
	})
}
