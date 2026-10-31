package semverrange

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
)

// tableCase is one row of testdata/satisfies.json.
type tableCase struct {
	Version string `json:"v"`
	Range   string `json:"r"`
	OK      bool   `json:"ok"`
}

// TestSatisfiesMatchesNodeSemver compares Satisfies with a table produced by
// node-semver 7.8.5 itself.
//
// 対照表は本家の semver で作る。作り直すときは、frontend の依存を入れた checkout の
// ルートで次を実行する(乱数の種は固定なので、同じ版なら同じ表になる)。
//
//	node internal/misc/semverrange/testdata/gen.js \
//	  frontend/node_modules/.pnpm/semver@7.8.5/node_modules/semver \
//	  > internal/misc/semverrange/testdata/satisfies.json
func TestSatisfiesMatchesNodeSemver(t *testing.T) {
	data, err := os.ReadFile("testdata/satisfies.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []tableCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	// 読み込みが空振りしても緑にならないよう、件数と両方の結果の下限を置く。
	if len(cases) < 1000 {
		t.Fatalf("table has %d cases, want at least 1000", len(cases))
	}
	var trues, mismatches int
	for _, c := range cases {
		if c.OK {
			trues++
		}
		if got := Satisfies(c.Version, c.Range); got != c.OK {
			mismatches++
			if mismatches <= 30 {
				t.Errorf("Satisfies(%q, %q) = %v, node-semver says %v", c.Version, c.Range, got, c.OK)
			}
		}
	}
	if mismatches > 0 {
		t.Errorf("%d of %d cases differ from node-semver", mismatches, len(cases))
	}
	if trues < 100 || len(cases)-trues < 100 {
		t.Errorf("table is lopsided: %d true of %d", trues, len(cases))
	}
}

// TestSatisfiesTableCoversFediverseVersions makes sure the table keeps the
// versions and ranges the issue is about.
func TestSatisfiesTableCoversFediverseVersions(t *testing.T) {
	data, err := os.ReadFile("testdata/satisfies.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []tableCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	versions := map[string]bool{}
	ranges := map[string]bool{}
	for _, c := range cases {
		versions[c.Version] = true
		ranges[c.Range] = true
	}
	for _, v := range []string{"2025.4.0", "2024.11.0-beta.3", "4.3.2+glitch", "4.4.0-beta.1+glitch", "2.7.0-1234-gabc", "2024.11.1-cherrypick-4.14.0", "13.14.2", "1.0", "v1.2.3", "?", "", " 1.2.3 "} {
		if !versions[v] {
			t.Errorf("table has no case for version %q", v)
		}
	}
	for _, r := range []string{"*", "<=2024.11.0", "<2025", ">=4.0.0 <4.3.0", "1.2.3 - 2.3.4", "~1.2", "^0.2.3", "1.x", "1.2.*", ">=1.2.3 || <0.1", "=1.2.3", "1.2.3", ">=", "abc", "1.2.3 -", "||"} {
		if !ranges[r] {
			t.Errorf("table has no case for range %q", r)
		}
	}
}

// TestSatisfies documents the behaviour the delivery check relies on. The
// expectations were checked against node-semver 7.8.5.
func TestSatisfies(t *testing.T) {
	tests := []struct {
		name    string
		version string
		rng     string
		want    bool
	}{
		{"prerelease below an upper bound", "2024.11.0-beta.3", "<=2024.11.0", true},
		{"release at an inclusive upper bound", "2024.11.0", "<=2024.11.0", true},
		{"release above an upper bound", "2024.11.1", "<=2024.11.0", false},
		{"exclusive upper bound excludes the bound", "2025.0.0", "<2025.0.0", false},
		{"exclusive upper bound excludes its prereleases with x", "2025.0.0-rc.1", "<2025", false},
		{"build metadata is ignored", "4.3.2+glitch", "4.3.2", true},
		{"space separated comparators are ANDed", "4.3.0", ">=4.0.0 <4.3.0", false},
		{"hyphen range includes the upper patch", "2.3.4", "1.2.3 - 2.3.4", true},
		{"hyphen range stops before prereleases of the next patch", "2.3.5-0", "1.2.3 - 2.3.4", false},
		{"hyphen range with a partial upper bound", "2.3.9", "1.2 - 2.3", true},
		{"caret on 0.x keeps the minor", "0.3.0", "^0.2.3", false},
		{"caret on 0.0.x keeps the patch", "0.0.2", "^0.0.1", false},
		{"caret on 1.x allows the next minor", "1.9.0", "^1.2.3", true},
		{"tilde allows the next patch", "1.2.9", "~1.2.3", true},
		{"or of ranges", "0.0.5", ">=1.2.3 || <0.1", true},
		{"invalid range", "1.2.3", ">=", false},
		{"invalid version", "1.0", "*", false},
		{"empty version", "", "*", false},
		{"v prefix on version", "v1.2.3", "=1.2.3", true},
		{"equals prefix on version is invalid", "=1.2.3", "*", false},
		{"too long version", "1.2.3+" + strings.Repeat("a", 251), "*", false},
		{"major above MAX_SAFE_INTEGER", "9007199254740992.0.0", "*", false},
		{"double space left by build metadata breaks a hyphen range", "1.5.0", "1.2.3 +x - 2.3.4", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Satisfies(tt.version, tt.rng); got != tt.want {
				t.Errorf("Satisfies(%q, %q) = %v, want %v", tt.version, tt.rng, got, tt.want)
			}
		})
	}
}

func TestCompareIdentifiers(t *testing.T) {
	num := func(n int64) identifier { return identifier{isNum: true, num: n} }
	str := func(s string) identifier { return identifier{str: s} }
	tests := []struct {
		name string
		a, b identifier
		want int
	}{
		{"numbers", num(1), num(2), -1},
		{"number before string", num(9), str("a"), -1},
		{"string after number", str("a"), num(9), 1},
		{"strings", str("b"), str("a"), 1},
		{"equal strings", str("a"), str("a"), 0},
		{"big numeric strings round to the same double", str("9007199254740993"), str("9007199254740992"), 0},
		{"big numeric string after a number", str("9007199254740991"), num(1), 1},
		{"number before a big numeric string", num(1), str("9007199254740991"), -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compareIdentifiers(tt.a, tt.b); got != tt.want {
				t.Errorf("compareIdentifiers(%v, %v) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestIncrPanicsOnNonNumber(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("incr did not panic")
		}
	}()
	incr("x")
}

// 範囲の解析結果は覚えておき、上限を超えたら捨てる (#3558)。覚えた結果を使っても、
// 捨てた後に解析し直しても、判定は同じになること。
func TestRangeCache(t *testing.T) {
	rangeCache.Lock()
	saved := rangeCache.m
	rangeCache.m = make(map[string]parsedRange)
	rangeCache.Unlock()
	t.Cleanup(func() {
		rangeCache.Lock()
		rangeCache.m = saved
		rangeCache.Unlock()
	})

	parsed := Satisfies("1.2.3", "<=1.2.3")
	cached := Satisfies("1.2.3", "<=1.2.3")
	if !parsed || !cached {
		t.Fatal("cached range must give the same result")
	}
	rangeCache.Lock()
	_, hit := rangeCache.m["<=1.2.3"]
	rangeCache.Unlock()
	if !hit {
		t.Fatal("parsed range was not kept")
	}
	// 不正な範囲も覚える (解析し直しても false のまま)。
	parsedInvalid := Satisfies("1.2.3", ">=")
	cachedInvalid := Satisfies("1.2.3", ">=")
	if parsedInvalid || cachedInvalid {
		t.Fatal("invalid range must not match")
	}

	for i := 0; i < rangeCacheMax+10; i++ {
		Satisfies("1.2.3", fmt.Sprintf(">=1.0.%d", i))
	}
	rangeCache.Lock()
	n := len(rangeCache.m)
	rangeCache.Unlock()
	if n > rangeCacheMax {
		t.Fatalf("cache holds %d ranges, want at most %d", n, rangeCacheMax)
	}
	if !Satisfies("1.2.3", "<=1.2.3") {
		t.Fatal("range parsed again after eviction must still match")
	}
}

// 配送の worker から同時に呼ばれても、表を壊さないこと。-race で走らせて守る。
func TestRangeCacheConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < rangeCacheMax+20; i++ {
				// 1.2.3 は <=1.2.i に i >= 3 で当たる (>=9.* には当たらない)。
				if got := Satisfies("1.2.3", fmt.Sprintf("<=1.2.%d || >=9.%d.%d", i, g, i)); got != (i >= 3) {
					t.Errorf("Satisfies(1.2.3, <=1.2.%d ...) = %v", i, got)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
