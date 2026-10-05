package federation

import (
	"net/url"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 既定ポートを明記した綴りが、ポート無しの綴りと同じ host になること。
//
// 剥がさないと `blocked.example:443` という**同じ authority の別綴り**が
// `blockedHosts` の suffix 一致 (instance.HostMatchesAny) から外れ、
// defederation した相手からの取り込みが続く。upstream の `extractDbHost` は
// WHATWG `new URL().host` なので剥がれる。
func TestHostFromURI_StripsDefaultPort(t *testing.T) {
	cases := []struct {
		name string
		uri  string
		want string
	}{
		{"https default port is stripped", "https://blocked.example:443/users/x", "blocked.example"},
		{"http default port is stripped", "http://blocked.example:80/users/x", "blocked.example"},
		{"no port stays as-is", "https://blocked.example/users/x", "blocked.example"},
		// 非既定ポートは別 host のまま残す (upstream も同じ)。既定ポートの
		// 判定が scheme を見ていないと、この 3 件のどれかが崩れる。
		{"https non-default port is kept", "https://blocked.example:8443/users/x", "blocked.example:8443"},
		{"443 is not http's default port", "http://blocked.example:443/users/x", "blocked.example:443"},
		{"80 is not https's default port", "https://blocked.example:80/users/x", "blocked.example:80"},
		// 既存の正規化 (小文字化 / punycode) は port を剥がしても効き続けること。
		{"case is still lowered", "https://Mixed.Example:443/users/x", "mixed.example"},
		{"IDN is still punycoded", "https://パイ.example:443/users/x", "xn--eckve.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := hostFromURI(tc.uri)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// 保存側 (hostFromURI) と配送先比較 (sameDeliveryHost) が同じ規則で正規化する
// こと。**片側だけが既定ポートを剥がしていた**のが元の欠陥で、`inbox` に
// `:443` を書いた相手は sameDeliveryHost を通る一方で blockedHosts からは
// 外れていた。
func TestHostFromURI_AgreesWithDeliveryHostRule(t *testing.T) {
	got, err := hostFromURI("https://remote.example:443/users/alice")
	require.NoError(t, err)
	assert.Equal(t, "remote.example", got)
	assert.True(t, sameDeliveryHost("https://remote.example/users/alice", "https://remote.example:443/inbox"),
		"既定ポート付きの inbox は同一 host とみなされる (元からこちら側は剥がしていた)")
}

// IPv6 リテラルは bracket を保つこと。`Hostname()` が bracket を外すので、
// 戻さないと `::1` + port が `::1:8443` になり、host 部にコロンを含むだけの
// 値と区別が付かなくなる。
func TestPunyHostPort_KeepsIPv6Brackets(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"https://[::1]:8443/x", "[::1]:8443"},
		{"https://[::1]:443/x", "[::1]"},
		{"https://[::1]/x", "[::1]"},
		{"http://[2001:db8::1]:80/x", "[2001:db8::1]"},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.raw)
		require.NoError(t, err)
		assert.Equal(t, tc.want, punyHostPort(u), tc.raw)
	}
}

// NormalizeGateHost は host を取り出せない入力で "" を返すこと。呼び出し側
// (queue の hostFromInbox) は "" を「判定できない」として扱う。
func TestNormalizeGateHost(t *testing.T) {
	assert.Equal(t, "blocked.example", NormalizeGateHost("https://blocked.example:443/inbox"))
	assert.Equal(t, "blocked.example", NormalizeGateHost("http://Blocked.Example:80/inbox"))
	assert.Equal(t, "blocked.example:8443", NormalizeGateHost("https://blocked.example:8443/inbox"))
	assert.Equal(t, "", NormalizeGateHost("://bad-url"), "parse できない URL")
	assert.Equal(t, "", NormalizeGateHost("https://:443/inbox"), "port だけで host が無い URL")
	// **非既定ポートで見ること。** 既定ポートは剥がされるので、host 不在の
	// 判定が無くても "" になってしまい、検査が空振りする。
	assert.Equal(t, "", NormalizeGateHost("https://:8443/inbox"), "host 不在は port を残さず \"\" にする")
	assert.Equal(t, "", NormalizeGateHost(""), "空文字")
}

// URL builder 未配線 (baseURL 空) では自ホストを決められないので、何にも
// マッチしないこと。`self == ""` を素通しにすると `isSelfHost("")` が true に
// なり、host を取り出せない入力を自ホスト扱いしてしまう。
func TestIsSelfHost_UnwiredBuilderMatchesNothing(t *testing.T) {
	r := &Resolver{urls: activitypub.NewURLBuilder("")}
	assert.Equal(t, "", r.selfHost())
	assert.False(t, r.isSelfHost(""), "未配線で空 host を自ホスト扱いしない")
	assert.False(t, r.isSelfHost("remote.example"))

	// 配線済みなら空 host はやはり自ホストではない。
	r2 := &Resolver{urls: activitypub.NewURLBuilder("https://example.com")}
	assert.Equal(t, "example.com", r2.selfHost())
	assert.False(t, r2.isSelfHost(""))
	assert.True(t, r2.isSelfHost("example.com"))

	// urls 自体が nil でも panic しない。
	var r3 Resolver
	assert.Equal(t, "", r3.selfHost())
	assert.False(t, r3.isSelfHost("example.com"))
}

// **既定ポートの判定は数値で行う (レビュー H2)。** `url.Port()` は `"0443"` を
// そのまま返すが、Go の HTTP client はそれを 443 として接続する。文字列一致だと
// `blocked.example:0443` が別 host として保存・比較され、**既定ポートの明記で
// gate を回避できるのと同じ穴が別の綴りで残る**。
func TestHostFromURI_StripsPaddedDefaultPort(t *testing.T) {
	cases := map[string]string{
		"https://blocked.example/x":        "blocked.example",
		"https://blocked.example:443/x":    "blocked.example",
		"https://blocked.example:0443/x":   "blocked.example",
		"https://blocked.example:00443/x":  "blocked.example",
		"http://blocked.example:80/x":      "blocked.example",
		"http://blocked.example:080/x":     "blocked.example",
		"http://blocked.example:0000080/x": "blocked.example",
		// 非既定ポートは別 host のまま (upstream も同じ)。
		"https://blocked.example:8443/x": "blocked.example:8443",
		"https://blocked.example:0844/x": "blocked.example:0844",
		// scheme が違えば既定も違う。
		"http://blocked.example:443/x": "blocked.example:443",
		"https://blocked.example:80/x": "blocked.example:80",
	}
	for raw, want := range cases {
		t.Run(raw, func(t *testing.T) {
			got, err := hostFromURI(raw)
			if err != nil {
				t.Fatalf("hostFromURI(%q) が error: %v", raw, err)
			}
			if got != want {
				t.Fatalf("hostFromURI(%q) = %q, want %q", raw, got, want)
			}
		})
	}
}
