package server

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// creditImageOrigins が `/about-misskey` の実態を覆っているか検査する (#2892)。
//
// **これが無いと upstream の更新で黙って壊れる。** あのページは #2700 で
// 「upstream の謝辞は消さない」方針にしたので、fork は中身を追従で丸ごと
// 受け取る。upstream が別の host からアイコンを読むようにした瞬間、CSP が
// それを弾いて壊れた画像に戻る — **その状態は 62 枚が全滅していた #2892 以前と
// 同じで、誰も気付かないまま恒久化する**。
//
// **frontend/ は本体で追跡している (#3379) ので skip しない。** 読めないのはパスの
// 誤りで、required の `test` で落ちる。
func TestCreditImageOriginsCoverAboutMisskey(t *testing.T) {
	rel := "frontend/packages/frontend/src/pages/about-misskey.vue"
	body, err := os.ReadFile(filepath.Join(repoRootDir(t), rel))
	if err != nil {
		require.NoError(t, err, "frontend/ は追跡しているので、%s を読めないのはパスの誤り", rel)
	}

	// 静的な `src="https://..."` (メンバー 6 + スポンサー 6) と、`patronsWithIcon` の
	// `icon: 'https://...'` (パトロン 50) の両方を拾う。
	//
	// **記法ごとに 1 件以上を要求する。** 片方をまとめて拾えなくなっても、もう片方が
	// 残っていると `NotEmpty` は通ってしまい、**検査していない 50 枚が黙って外れる**
	// (実測: `icon:` の抽出を落としても gate は緑のままだった)。
	forms := []struct {
		name    string
		pattern *regexp.Regexp
	}{
		{`src="https://…"`, regexp.MustCompile(`src="(https://[^"]+)"`)},
		{`icon: 'https://…'`, regexp.MustCompile(`icon: '(https://[^']+)'`)},
	}

	seen := map[string]int{}
	for _, f := range forms {
		matches := f.pattern.FindAllStringSubmatch(string(body), -1)
		require.NotEmpty(t, matches,
			"%s から %s の形の外部画像 URL を 1 つも拾えない。"+
				"**書式が変わったならこの正規表現も直すこと** — "+
				"拾えないまま放置すると、その分を検査していないのに緑になる", rel, f.name)
		for _, m := range matches {
			u, err := url.Parse(m[1])
			require.NoError(t, err, "URL を parse できない: %s", m[1])
			seen[u.Scheme+"://"+u.Host]++
		}
	}

	origins := make([]string, 0, len(seen))
	for o := range seen {
		origins = append(origins, o)
	}
	sort.Strings(origins)

	for _, o := range origins {
		assert.Contains(t, creditImageOrigins, o,
			"%s が %d 箇所から読まれているが CSP の img-src に無い。"+
				"upstream が host を足したなら creditImageOrigins にも足すこと "+
				"(足さないとそのアイコンだけ黙って壊れる)", o, seen[o])
	}

	// **使わなくなった origin も落とす。** 許可したままにすると、ポリシーが
	// 「実際に必要な範囲」から乖離していく (`cspExtras` のコメントと同じ方針)。
	for _, o := range creditImageOrigins {
		assert.Contains(t, origins, o,
			"%s は CSP で許可しているが %s からは読まれていない。使わない host は落とすこと", o, rel)
	}
}

// repoRootDir walks up from the test's working directory to the module root.
func repoRootDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	for d := wd; ; {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		require.NotEqual(t, parent, d, "go.mod が %s から見つからない", wd)
		d = parent
	}
}
