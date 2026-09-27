package federation_test

import (
	"testing"

	"github.com/shiroha-a/mk/internal/core/federation"
	"github.com/shiroha-a/mk/internal/core/instance"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockedHosts に入るのは host だけ (管理画面が受け取るのもその形)。取り込み /
// 配送側が `:443` 付きの host を作ると `HostMatchesAny` の suffix 一致から外れる
// ので、突き合わせる値は既定ポートを剥がした形でなければならない。
func TestNormalizeGateHost_MatchesBlockedHostsEntry(t *testing.T) {
	blocked := []string{"blocked.example"}

	assert.True(t, instance.HostMatchesAny(blocked, federation.NormalizeGateHost("https://blocked.example:443/inbox")),
		"既定ポートを明記した inbox が blockedHosts から外れている")
	assert.True(t, instance.HostMatchesAny(blocked, federation.NormalizeGateHost("http://sub.blocked.example:80/inbox")),
		"サブドメイン + 既定ポートも blockedHosts に当たる")
	assert.True(t, instance.HostMatchesAny(blocked, federation.NormalizeGateHost("https://blocked.example/inbox")))

	// 非既定ポートは保存する host としては別 authority のまま残る
	// (NormalizeGateHost は `blocked.example:8443` を返す) が、拒否リストの
	// 照合ではポートを落とした形でも当てる。upstream は当てないが、それだと
	// 相手がポートを変えるだけでブロックを回避できる (docs/divergence.md)。
	assert.True(t, instance.HostMatchesAny(blocked, federation.NormalizeGateHost("https://blocked.example:8443/inbox")),
		"非既定ポートの綴りで blockedHosts を回避できてはいけない")
}

// 配送側の block 判定が既定ポート付きの inbox でも効くこと。stubBlocker は
// 完全一致の map なので、正規化しないと `bad.example:443` は素通りする。
func TestDeliverActivity_SkipsBlockedHostSpelledWithDefaultPort(t *testing.T) {
	svc, enq, userRepo, _, keypairRepo := newDeliverService(t)
	installLocalSigner(t, userRepo, keypairRepo)
	svc.SetHostBlockChecker(&stubBlocker{blocked: map[string]bool{"bad.example": true}})

	require.NoError(t, svc.DeliverActivity("alice", []byte(`{}`), []string{
		"https://bad.example:443/inbox",
		"http://bad.example:80/inbox",
		"https://good.example:443/inbox",
	}))
	require.Len(t, enq.calls, 1, "既定ポート付きの綴りで block を回避できてはいけない")
	assert.Equal(t, "https://good.example:443/inbox", enq.calls[0].Inbox)
}

// federation: specified (allowlist) 側も同じこと。blocked とは独立した経路。
func TestDeliverActivity_SkipsDisallowedHostSpelledWithDefaultPort(t *testing.T) {
	svc, enq, userRepo, _, keypairRepo := newDeliverService(t)
	installLocalSigner(t, userRepo, keypairRepo)
	svc.SetHostBlockChecker(&stubBlocker{disallowed: map[string]bool{"other.example": true}})

	require.NoError(t, svc.DeliverActivity("alice", []byte(`{}`), []string{
		"https://other.example:443/inbox",
		"https://allowed.example/inbox",
	}))
	require.Len(t, enq.calls, 1)
	assert.Equal(t, "https://allowed.example/inbox", enq.calls[0].Inbox)
}

// 配送側が blocker に渡す host は非既定ポートを残した形 (= 保存される
// `instance.host` と同じ綴り) であること。stubBlocker は完全一致の map なので、
// ここでは「正規化がポートを落とさない」ことだけを見ている。ポートを落として
// ブロックに当てるのは blocker 本体 (`instance.HostMatchesAny`) の仕事で、
// そちらは TestNormalizeGateHost_MatchesBlockedHostsEntry が見る。
func TestDeliverActivity_NonDefaultPortIsADifferentHost(t *testing.T) {
	svc, enq, userRepo, _, keypairRepo := newDeliverService(t)
	installLocalSigner(t, userRepo, keypairRepo)
	svc.SetHostBlockChecker(&stubBlocker{blocked: map[string]bool{"bad.example": true}})

	require.NoError(t, svc.DeliverActivity("alice", []byte(`{}`), []string{"https://bad.example:8443/inbox"}))
	require.Len(t, enq.calls, 1)
}

// UTS#46 の mapping 対象の文字で綴った host も blockedHosts に当たること。
// 接続先は `evil.example` なので、表記の違いでブロックを回避できてはいけない。
func TestNormalizeGateHost_MappedSpellingsMatchBlockedHosts(t *testing.T) {
	blocked := []string{"evil.example"}
	for _, u := range []string{
		"https://ｅｖｉｌ.example/inbox",
		"https://evil。example/inbox",
		"https://evil.ex\u00adample/inbox",
	} {
		assert.True(t, instance.HostMatchesAny(blocked, federation.NormalizeGateHost(u)), u)
	}
}
