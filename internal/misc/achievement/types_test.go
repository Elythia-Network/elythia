package achievement

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/elythia-network/elythia/internal/upstreamsrc"
)

func TestIsValidType(t *testing.T) {
	valid := []string{"notes1", "login1000", "bubbleGameDoubleExplodingHead", "iLoveMisskey", "markedAsCat"}
	for _, n := range valid {
		if !IsValidType(n) {
			t.Errorf("IsValidType(%q) = false, want true", n)
		}
	}
	invalid := []string{"", "notes2", "bogus", "Notes1", "achievementEarned"}
	for _, n := range invalid {
		if IsValidType(n) {
			t.Errorf("IsValidType(%q) = true, want false", n)
		}
	}
}

// Misskey ACHIEVEMENT_TYPES の件数 (本家のソースが無い CI でも効く軽量 tripwire)。
func TestCount(t *testing.T) {
	if got := Count(); got != 78 {
		t.Errorf("Count() = %d, want 78 (Misskey ACHIEVEMENT_TYPES)", got)
	}
}

// TestTypes_MatchUpstream は本家の ACHIEVEMENT_TYPES と types map が完全一致する
// ことを検証する drift gate。本家は .cache/misskey/<版> から読む (#3378)。
// 本家が無ければ skip するが、MK_UPSTREAM_REQUIRE を立てた CI (`make
// upstream-check`) では落とす。追加 / 削除 / リネームのいずれも検出するので、
// 件数が変わらないリネームも TestCount をすり抜けずに捕まえられる。
func TestTypes_MatchUpstream(t *testing.T) {
	dir, err := upstreamsrc.Locate(repoRoot(t))
	if err != nil {
		if upstreamsrc.Required() {
			t.Fatalf("%s が立っているのに本家を読めない: %v", upstreamsrc.EnvRequire, err)
		}
		t.Skipf("upstream Misskey not available, skipping drift check: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "packages", "backend", "src", "models", "UserProfile.ts"))
	if err != nil {
		t.Fatalf("read upstream UserProfile.ts: %v", err)
	}
	upstream := extractAchievementTypes(string(data))
	if len(upstream) == 0 {
		t.Fatal("no ACHIEVEMENT_TYPES parsed from upstream (upstream format change?)")
	}
	for name := range upstream {
		if !IsValidType(name) {
			t.Errorf("upstream ACHIEVEMENT_TYPES has %q but mk-go types map is missing it", name)
		}
	}
	for name := range types {
		if _, ok := upstream[name]; !ok {
			t.Errorf("mk-go types map has %q but upstream ACHIEVEMENT_TYPES does not", name)
		}
	}
}

// extractAchievementTypes pulls the quoted names out of the
// `export const ACHIEVEMENT_TYPES = [ ... ] as const;` block.
func extractAchievementTypes(src string) map[string]struct{} {
	block := regexp.MustCompile(`(?s)export const ACHIEVEMENT_TYPES = \[(.*?)\] as const`).FindStringSubmatch(src)
	if len(block) != 2 {
		return nil
	}
	out := map[string]struct{}{}
	for _, m := range regexp.MustCompile(`'([^']+)'`).FindAllStringSubmatch(block[1], -1) {
		out[m[1]] = struct{}{}
	}
	return out
}

// repoRoot walks up from the test's working directory to the module root
// (the directory containing go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for d := wd; ; {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			t.Fatalf("go.mod not found from %s", wd)
		}
		d = parent
	}
}
