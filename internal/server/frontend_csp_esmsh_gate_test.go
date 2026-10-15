package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CSP が許す esm.sh のパスが、frontend が実際に読むものと揃っているか検査する。
//
// **これが無いと shiki を上げた瞬間に、enforce の構成でコードブロックの
// ハイライトが黙って消える。** パスに版が入るので、package.json の版と
// frontendShikiVersion がずれると CSP が新しい版の読み込みを弾く。
//
// **frontend/ は本体で追跡している (#3379) ので skip しない。**
func TestFrontendCSP_EsmShSourcesMatchFrontend(t *testing.T) {
	root := repoRootDir(t)
	for _, pkg := range []string{"frontend", "frontend-embed"} {
		t.Run(pkg, func(t *testing.T) {
			rel := filepath.Join("frontend/packages", pkg, "package.json")
			raw, err := os.ReadFile(filepath.Join(root, rel))
			require.NoError(t, err, "frontend/ は追跡しているので、%s を読めないのはパスの誤り", rel)
			var manifest struct {
				Dependencies map[string]string `json:"dependencies"`
			}
			require.NoError(t, json.Unmarshal(raw, &manifest))
			assert.Equal(t, frontendShikiVersion, manifest.Dependencies["shiki"],
				"%s の shiki と frontendShikiVersion がずれている。CSP が esm.sh の新しい版を弾く", rel)

			// vite の書き換え先が esm.sh の `shiki@<版>/` のままか。別の CDN や
			// 別のパスに変わったら、esmShSources も直す必要がある。
			vite := filepath.Join("frontend/packages", pkg, "vite.config.ts")
			src, err := os.ReadFile(filepath.Join(root, vite))
			require.NoError(t, err)
			assert.Contains(t, string(src), "`https://esm.sh/shiki@${packageInfo.dependencies.shiki}/${match['subPkg']}`",
				"%s の shiki の読み込み先が変わった。esmShSources を見直すこと", vite)
		})
	}

	// 版を埋めたパスが CSP に入っていること (定数の組み立て間違いを拾う)。
	policy := buildFrontendCSP(false, cspExtras{})
	for _, src := range wantEsmShSources {
		assert.Contains(t, policy, " "+src)
		assert.True(t, strings.Contains(src, "@"+frontendShikiVersion+"/"), src)
	}
}
