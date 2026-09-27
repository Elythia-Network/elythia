package idnhost

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/idna"
)

// **比較の両辺を揃えるための正規化** (#2704)。保存側も #2706 で `hostFromURI` が同じ
// 正規化を掛けるので、引き当ては正規形どうしの完全一致になる (#2996 で読み取り側の
// 両当たりを撤去した。`idnhost.go` のコメント参照)。
func TestPuny(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"unicode IDN to punycode", "パイ.example", "xn--eckve.example"},
		{"already punycode", "xn--eckve.example", "xn--eckve.example"},
		{"uppercase punycode", "XN--ECKVE.EXAMPLE", "xn--eckve.example"},
		{"uppercase ascii", "Remote.Example", "remote.example"},
		{"plain ascii", "remote.example", "remote.example"},
		{"empty", "", ""},
		{"host with port", "remote.example:3000", "remote.example:3000"},
		{"unicode with port", "パイ.example:3000", "xn--eckve.example:3000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Puny(tc.in))
		})
	}

	t.Run("idna が失敗する入力は小文字化だけして返す", func(t *testing.T) {
		// 不正な punycode ラベル。ここで空文字や panic を返すと、比較の片側が
		// 消えて**別ホストを同一視する**方向に倒れるので、素通しが安全側。
		assert.Equal(t, "xn--0", Puny("XN--0"))
	})

	t.Run("idempotent", func(t *testing.T) {
		for _, in := range []string{"パイ.example", "xn--eckve.example", "remote.example"} {
			assert.Equal(t, Puny(in), Puny(Puny(in)), in)
		}
	})

	t.Run("UTS46 の mapping で接続先と同じ名前に畳む", func(t *testing.T) {
		// net/http は非 ASCII の host を idna.Lookup で変換してから dial する。
		// 保存と比較がそれと違う形を作ると、接続先は evil.example なのに
		// blockedHosts の evil.example に当たらない。
		for _, in := range []string{
			"ｅｖｉｌ.example",
			"evil\u3002example",
			"evil\uff0eexample",
			"evil\uff61example",
			"evil.ex\u00adample",
			"ＥＶＩＬ.example",
		} {
			assert.Equal(t, "evil.example", Puny(in), "%q", in)
		}
		assert.Equal(t, "evil.example:8443", Puny("ｅｖｉｌ.example:8443"))
		assert.Equal(t, "xn--eckve.example", Puny("パイ\u3002example"))
		assert.Equal(t, "xn--eckve.example", Puny("ﾊﾟｲ.example"), "半角カナも同じ名前")
	})

	t.Run("正当な IDN の表記は変わらない", func(t *testing.T) {
		// mapping の対象にならない文字だけの IDN は、以前の profile と同じ値を作る。
		// 保存済みの行が変わらないことの裏取り。
		for in, want := range map[string]string{
			"パイ.example":        "xn--eckve.example",
			"faß.de":            "xn--fa-hia.de",
			"bücher.example":    "xn--bcher-kva.example",
			"例え.テスト":            "xn--r8jz45g.xn--zckzah",
			"xn--eckve.example": "xn--eckve.example",
		} {
			assert.Equal(t, want, Puny(in), in)
			legacy, err := idna.ToASCII(in)
			require.NoError(t, err)
			assert.Equal(t, legacy, Puny(in), "以前の profile と同じ: %s", in)
		}
	})

	t.Run("Lookup が拒否する入力は従来の形", func(t *testing.T) {
		assert.Equal(t, "xn--_x-mg4ash.example", Puny("パイ_x.example"))
		assert.Equal(t, "a_b.example", Puny("A_B.example"))
	})

	t.Run("ASCII は小文字化だけ", func(t *testing.T) {
		// net/http は ASCII の host をそのまま dial するので、xn-- ラベルを
		// 解釈し直すと接続先とずれる。
		assert.Equal(t, "xn--qi7ciaj2b.example", Puny("XN--QI7CIAJ2B.example"))
	})

	t.Run("net/http が dial する名前と一致する", func(t *testing.T) {
		for _, in := range []string{"ｅｖｉｌ.example", "evil\u3002example", "evil.ex\u00adample", "パイ.example"} {
			dialed, err := idna.Lookup.ToASCII(in)
			require.NoError(t, err)
			assert.Equal(t, dialed, Puny(in), in)
		}
	})
}

func TestMatchesBlockList_MappedSpellings(t *testing.T) {
	for _, in := range []string{"ｅｖｉｌ.example", "evil\u3002example", "evil.ex\u00adample", "sub.ｅｖｉｌ.example:8443"} {
		assert.True(t, MatchesBlockList([]string{"evil.example"}, in), "%q", in)
		assert.True(t, MatchesBlockList([]string{"ｅｖｉｌ.example"}, Puny(in)), "%q", in)
	}
}

// **正規形を作る規則は 1 つだけ (#2994)。** 比較側 (`sameDeliveryHost`) と保存側
// (`chat_room.uri`) が別々に持つと、「比較では同じ authority なのに保存は別物」という
// 綴り違いの取り違えが生まれる。
func TestHostPort(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"そのまま", "https://example.com/x", "example.com"},
		{"大文字は畳む", "https://Example.COM/x", "example.com"},
		{"https の既定ポートは剥がす", "https://example.com:443/x", "example.com"},
		{"http の既定ポートは剥がす", "http://example.com:80/x", "example.com"},
		{"非既定ポートは残す", "https://example.com:8443/x", "example.com:8443"},
		{"scheme が違えば 443 も残す", "http://example.com:443/x", "example.com:443"},
		{"punycode へ畳む", "https://パイ.example/x", "xn--eckve.example"},
		{"IPv6 は bracket を戻す", "https://[::1]:8443/x", "[::1]:8443"},
		{"IPv6 の既定ポート", "https://[::1]:443/x", "[::1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.raw)
			require.NoError(t, err)
			assert.Equal(t, tc.want, HostPort(u))
		})
	}
}

func TestCanonicalURI(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"そのまま", "https://example.com/chat/rooms/r1", "https://example.com/chat/rooms/r1"},
		{"authority を畳む", "https://Example.COM:443/chat/rooms/r1", "https://example.com/chat/rooms/r1"},
		{"scheme を小文字化", "HTTPS://example.com/chat/rooms/r1", "https://example.com/chat/rooms/r1"},
		{"punycode", "https://パイ.example/chat/rooms/r1", "https://xn--eckve.example/chat/rooms/r1"},
		{"非既定ポートは残す", "https://example.com:8443/chat/rooms/r1", "https://example.com:8443/chat/rooms/r1"},
		// host が無いものは身元にできない。
		{"host 不在", "/chat/rooms/r1", ""},
		{"空", "", ""},
		{"解析できない", "://bad", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, CanonicalURI(tc.raw))
		})
	}
}

// 同じ authority の別綴りは同じ正規形になること (取り違えの本体)。
func TestCanonicalURI_FoldsSpellings(t *testing.T) {
	want := CanonicalURI("https://remote.example/chat/rooms/r1")
	require.NotEmpty(t, want)
	for _, raw := range []string{
		"https://remote.example:443/chat/rooms/r1",
		"https://REMOTE.EXAMPLE/chat/rooms/r1",
		"HTTPS://Remote.Example:443/chat/rooms/r1",
	} {
		assert.Equal(t, want, CanonicalURI(raw), raw)
	}
}

// 旧 build が mapping 無しで punycode 化した行を、接続先と同じ名前へ戻すこと。
// 正当な IDN と Lookup が拒否する形は変えない。
func TestRepairLegacyPuny(t *testing.T) {
	for in, want := range map[string]string{
		"xn--qi7ciaj2b.example":      "evil.example",
		"xn--evilexample-7e3j":       "evil.example",
		"evil.xn--example-mka":       "evil.example",
		"xn--qi7ciaj2b.example:8443": "evil.example:8443",
		"XN--QI7CIAJ2B.example":      "evil.example",
		"xn--eckve.example":          "xn--eckve.example",
		"xn--_x-mg4ash.example":      "xn--_x-mg4ash.example",
		"xn--zz.example":             "xn--zz.example",
		"Mixed.Example":              "mixed.example",
		"パイ.example":                 "xn--eckve.example",
		"[::1]:8443":                 "[::1]:8443",
	} {
		assert.Equal(t, want, RepairLegacyPuny(in), in)
	}
	for _, in := range []string{"xn--qi7ciaj2b.example", "xn--eckve.example", "remote.example"} {
		assert.Equal(t, RepairLegacyPuny(in), RepairLegacyPuny(RepairLegacyPuny(in)), "idempotent: %s", in)
	}
}
