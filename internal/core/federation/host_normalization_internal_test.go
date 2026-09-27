package federation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostFromURI は保存形の host を作る**唯一の入口**なので、ここで正規化する
// (#2706)。`url.Parse` は小文字化も punycode 化もしないため、正規化しないと
// `Mixed.Example` のような表記の行が入り、連合ゲートや timeline の instance-mute
// (完全一致) が取りこぼす。
func TestHostFromURI_Normalizes(t *testing.T) {
	cases := []struct {
		name string
		uri  string
		want string
	}{
		{"already normalized", "https://remote.example/users/x", "remote.example"},
		{"mixed case is lowercased", "https://Mixed.Example/users/x", "mixed.example"},
		{"unicode IDN becomes punycode", "https://パイ.example/users/x", "xn--eckve.example"},
		{"punycode is kept", "https://xn--eckve.example/users/x", "xn--eckve.example"},
		{"port is kept", "https://Mixed.Example:8443/users/x", "mixed.example:8443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := hostFromURI(tc.uri)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// host が無い / 壊れた URI は従来どおりエラー。
func TestHostFromURI_Rejects(t *testing.T) {
	for _, uri := range []string{"", "/users/x", "not a uri\x7f"} {
		_, err := hostFromURI(uri)
		assert.Error(t, err, uri)
	}
}

// 保存形と比較用が同じ値を作ること。ずれると、正規化して保存した行を
// 読み取り側が引けなくなる。
func TestHostFromURI_MatchesPunyHost(t *testing.T) {
	for _, uri := range []string{
		"https://Mixed.Example/users/x",
		"https://パイ.example/users/x",
		"https://remote.example/users/x",
	} {
		got, err := hostFromURI(uri)
		require.NoError(t, err)
		assert.Equal(t, punyHost(got), got, uri)
	}
}

// port だけで host が無い URI は弾くこと。`u.Host` を見ていると `":8443"` が
// 通ってしまう (#2714 review LOW-9)。
func TestHostFromURI_RejectsPortWithoutHost(t *testing.T) {
	_, err := hostFromURI("https://:8443/users/x")
	assert.Error(t, err)
}

// 全角英字・句点・soft hyphen を含む URI は、Go の HTTP client が
// idna.Lookup で `evil.example` に変換して dial する。保存形と比較の値が
// 接続先と違う形になると、blockedHosts の `evil.example` を素通りする。
func TestHostFromURI_FoldsUTS46MappedSpellings(t *testing.T) {
	for _, uri := range []string{
		"https://ｅｖｉｌ.example/users/x",
		"https://evil。example/users/x",
		"https://evil.ex\u00adample/users/x",
		"https://ｅｖｉｌ.example:443/users/x",
	} {
		got, err := hostFromURI(uri)
		require.NoError(t, err)
		assert.Equal(t, "evil.example", got, uri)
		assert.Equal(t, "evil.example", NormalizeGateHost(uri), uri)
		assert.True(t, sameDeliveryHost(uri, "https://evil.example/inbox"), uri)
		assert.NoError(t, assertResponseHostMatches("https://evil.example/users/x", uri), uri)
	}
	got, err := hostFromURI("https://ｅｖｉｌ.example:8443/users/x")
	require.NoError(t, err)
	assert.Equal(t, "evil.example:8443", got)
}
