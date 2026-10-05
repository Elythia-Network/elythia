package federation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// **自ホストの acct は WebFinger を踏まずローカル行へ短絡すること** (#2704)。
//
// upstream も `toPuny` を掛けた host を `toPuny(config.host)` と比べる
// (`RemoteUserResolveService.ts:59`)。片側だけだと、`url` を Unicode IDN で書いた
// instance で短絡が効かなくなり、自分自身への WebFinger 往復を誘発する。
//
// 同じ不変条件は `internal/core/user` 側にもあるが、**このパッケージだけを
// テストしても落ちる**ようにしておく (ここを触る人が同パッケージのテストだけ
// 回して気付かないのを避ける)。
func TestRemoteUserResolver_LocalHostShortCircuit(t *testing.T) {
	for _, tc := range []struct{ name, localHost, asked string }{
		{"ascii", "local.example", "local.example"},
		{"ascii uppercase", "local.example", "LOCAL.EXAMPLE"},
		{"unicode local, unicode ask", "パイ.example", "パイ.example"},
		{"unicode local, punycode ask", "パイ.example", "xn--eckve.example"},
		{"punycode local, unicode ask", "xn--eckve.example", "パイ.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			repo.Users["me"] = &model.User{ID: "me", Username: "Alice", UsernameLower: "alice"}
			// webfinger / resolver は nil。短絡できなければ panic か error になる。
			r := NewRemoteUserResolver(nil, nil, repo, tc.localHost)

			got, err := r.ResolveByUsernameHost("alice", tc.asked)
			require.NoError(t, err, "localHost=%q に対し host=%q が短絡すること", tc.localHost, tc.asked)
			assert.Equal(t, "me", got.ID)
		})
	}
}
