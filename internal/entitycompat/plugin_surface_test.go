package entitycompat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/pluginspec"
)

// TestPluginSurfaceDrift is the public-API gate for plugin/ (#2478).
//
// plugin/ は第三者が書いたプラグインが import する唯一の入口で、公開した時点
// から semver の義務を負う。export が増減したらこのテストが落ちるので、
//
//   - 意図せず公開面が広がるのを防げる
//   - golden の再生成が diff に出るのでレビューで必ず目に入る
//   - ドキュメントの一覧 (docs/plugins/authoring.md) は手で書かれているが、
//     golden との整合を TestPluginDoc_* が検査する (#2639)
//
// 差分が出たら、それが**意図した変更か**を確認してから
// `go run ./tools/pluginspec -write` で更新すること。互換性を壊す変更なら
// plugin.APIVersion も上げる。
func TestPluginSurfaceDrift(t *testing.T) {
	root := repoRoot(t)

	got, err := pluginspec.SurfaceAll(root, pluginspec.TrackedDirs)
	require.NoError(t, err)
	require.NotEmpty(t, got, "公開面が空になっている (解析に失敗した可能性)")

	golden, err := os.ReadFile(filepath.Join(root, pluginspec.DefaultGolden))
	require.NoError(t, err)

	assert.Equal(t, string(golden), pluginspec.Render(got),
		"plugin/ の公開面が golden と一致しない。意図した変更なら "+
			"`go run ./tools/pluginspec -write` で更新し、破壊的変更なら "+
			"plugin.APIVersion も上げること")
}
