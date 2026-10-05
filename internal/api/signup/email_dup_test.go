package signup

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/require"
)

// **確認済みのアドレスが重複していることを書き込み側でも見ること。**
//
// 判定は `email-address/available` にしか実装が無く、実際に書く 3 経路
// (signup / i/update-email / signup-application/register) はどれも通って
// いなかった。DB にも UNIQUE が無いので、1 つのメールボックスから無制限に
// アカウントを作れた。
func TestEmailVerifiedInUse_Mock(t *testing.T) {
	t.Parallel()

	repo := testutil.NewMockUserRepository()
	addr := "dup@example.test"
	verified := true
	repo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Email: &addr, EmailVerified: verified}

	inUse, err := repo.EmailVerifiedInUse(addr)
	require.NoError(t, err)
	require.True(t, inUse, "確認済みの行があれば使用中")

	inUse, err = repo.EmailVerifiedInUse("other@example.test")
	require.NoError(t, err)
	require.False(t, inUse)

	// **未確認の行は見ない。** 見ると、他人のアドレスを登録途中で放置するだけで
	// 本人の登録を妨害できる。
	unverified := "pending@example.test"
	repo.Profiles["u2"] = &model.UserProfile{UserID: "u2", Email: &unverified}
	inUse, err = repo.EmailVerifiedInUse(unverified)
	require.NoError(t, err)
	require.False(t, inUse, "未確認の行は使用中にしない")
}
