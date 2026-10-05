package mediaproxy_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/mediaproxy"
)

var expSecret = []byte("s3cret")

func TestSignURLUntil_RoundTrip(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	sig := mediaproxy.SignURLUntil(expSecret, "https://example.com/a.png", now.Add(time.Hour))

	assert.True(t, mediaproxy.VerifyExpiringHMAC(expSecret, "https://example.com/a.png", sig, now))
}

func TestVerifyExpiringHMAC(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const target = "https://example.com/a.png"
	sig := mediaproxy.SignURLUntil(expSecret, target, now.Add(time.Hour))

	for _, tt := range []struct {
		name string
		url  string
		sig  string
		at   time.Time
		want bool
	}{
		{"期限内", target, sig, now, true},
		// 期限ちょうどは通す (`<=`)。
		{"期限ちょうど", target, sig, now.Add(time.Hour), true},
		{"期限切れ", target, sig, now.Add(time.Hour + time.Second), false},
		{"別の URL", "https://example.com/b.png", sig, now, false},
		{"別の鍵で作った署名", target, mediaproxy.SignURLUntil([]byte("other"), target, now.Add(time.Hour)), now, false},
		// **期限だけ書き換えても通らない。** digest が期限も覆っている。
		{"期限を伸ばした", target, "9999999999" + sig[len("1700003600"):], now.Add(2 * time.Hour), false},
		// 期限の形をしていないものは扱わない (`VerifyHMAC` の担当)。
		{"無期限の署名", target, mediaproxy.SignURL(expSecret, target), now, false},
		{"空", target, "", now, false},
		{"区切りだけ", target, ".", now, false},
		{"期限が数値でない", target, "later." + sig[len("1700003600")+1:], now, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, mediaproxy.VerifyExpiringHMAC(expSecret, tt.url, tt.sig, tt.at))
		})
	}
}

// **URL と期限の境界を固定する (#3037)。** 区切り無しで連結すると
// `url="…a" exp="12"` と `url="…a1" exp="2"` が同じ digest になり、
// **署名を作り直さずに期限を伸ばせる**。
func TestSignURLUntil_BoundaryIsUnambiguous(t *testing.T) {
	a := mediaproxy.SignURLUntil(expSecret, "https://example.com/a", time.Unix(12, 0))
	b := mediaproxy.SignURLUntil(expSecret, "https://example.com/a1", time.Unix(2, 0))

	// digest 部分 (区切りの後ろ) を比べる。
	require.NotEqual(t, a[len("12")+1:], b[len("2")+1:])
}

// **素の署名が期限付き署名として通らないこと (#3037 レビュー)。**
//
// タグが無いと `U' = <url> + "\n" + <exp>` に対して発行された素の署名が、
// そのまま `(url, exp)` の期限付き署名として通る = 期限の無いトークンになる。
// 今はその URL を署名させる経路が無い (`url.Parse` が制御文字を拒否する) が、
// 守っているのが暗黙の副作用 1 つだけなのでここで固定する。
func TestExpiringSignatureIsDomainSeparatedFromPlain(t *testing.T) {
	const target = "https://example.com/a.png"
	exp := "1700003600"

	// 素の署名を「url + \n + exp」に対して発行する。
	confused := mediaproxy.SignURL(expSecret, target+"\n"+exp)

	assert.False(t,
		mediaproxy.VerifyExpiringHMAC(expSecret, target, exp+"."+confused, time.Unix(1_700_000_000, 0)),
		"素の署名が期限付き署名として通っている")
}
