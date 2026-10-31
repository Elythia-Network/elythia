package entitycompat

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// 移行の確認: upstream_paramdefs.json の meta から作る期待値が、tools/permspec・
// securespec が作っていた 4 つの golden と完全に一致すること (#3369)。golden を
// 消すコミットで、このテストも消す。
func TestUpstreamMetaMatchesLegacyGoldens(t *testing.T) {
	metas := upstreamMetas(t)
	read := func(file string, v any) {
		t.Helper()
		raw, err := os.ReadFile("testdata/" + file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if err := json.Unmarshal(raw, v); err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
	}
	var perms, kinds map[string]string
	var secure []string
	var cache map[string]int
	read("golden_permissions.json", &perms)
	read("golden_oauth_kinds.json", &kinds)
	read("golden_secure_endpoints.json", &secure)
	read("golden_cache_sec.json", &cache)
	gotCache, err := upstreamCacheSecs(metas)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ got, want any }{
		"permissions": {upstreamPermissions(metas), perms},
		"oauth kinds": {upstreamOAuthKinds(metas), kinds},
		"secure":      {upstreamSecureEndpoints(metas), secure},
		"cacheSec":    {gotCache, cache},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("%s derived from upstream_paramdefs.json differs from the legacy golden", name)
		}
	}
	t.Logf("permissions=%d secure=%d kinds=%d cacheSec=%d", len(perms), len(secure), len(kinds), len(cache))
}
