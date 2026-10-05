package entitycompat

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/config"
	"github.com/shiroha-a/mk/internal/upstreamsrc"
)

// upstreamImageRe matches the official Misskey image tag the e2e stacks run as
// the TS side.
var upstreamImageRe = regexp.MustCompile(`misskey/misskey:([0-9][0-9A-Za-z.\-]*)`)

// TestUpstreamVersionIsConsistent pins every place that names the upstream
// Misskey version to UPSTREAM_MISSKEY_VERSION (#3378).
//
// 追従するたびに、比較対象の本家 (`.cache/misskey/<版>`)・`/api/meta` の `version`
// (`config.MisskeyVersion`)・e2e で TS 側として立てる公式 image の tag を揃えて
// 上げる。どれかを上げ忘れると、golden は新しい版、e2e は古い版、と食い違った
// まま緑になる。
//
// 過去の記録 (CHANGELOG・docs・migration の注記) は当時の版を書いているので見ない。
func TestUpstreamVersionIsConsistent(t *testing.T) {
	root := repoRoot(t)
	want, err := upstreamsrc.Version(root)
	require.NoError(t, err)
	require.Equal(t, want, config.MisskeyVersion,
		"internal/config.MisskeyVersion を %s と揃える", upstreamsrc.VersionFile)

	found := map[string]bool{}
	for p := range gitTrackedSet(t, root) {
		if strings.HasPrefix(p, "docs/") || strings.HasPrefix(p, "migration/") ||
			p == "CHANGELOG.md" {
			continue
		}
		if !strings.HasSuffix(p, ".yml") && !strings.HasSuffix(p, ".yaml") {
			continue
		}
		for _, m := range upstreamImageRe.FindAllStringSubmatch(readRepoFile(t, p), -1) {
			found[p] = true
			require.Equalf(t, want, m[1], "%s の misskey/misskey:%s を %s (%s) と揃える", p, m[1], want, upstreamsrc.VersionFile)
		}
	}
	// **拾えなかったら落とす。** 書式が変わって 1 つも拾えないと、検査していない
	// のに緑になる。実在する対象を名指しで要求する。
	for _, must := range []string{".github/workflows/dropin-e2e.yml", "tests/dropin/compose.yml"} {
		require.Truef(t, found[must], "%s から misskey/misskey:<版> を拾えていない", must)
	}
}
