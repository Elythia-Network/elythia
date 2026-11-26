package fetchrss

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gofeedDifferences lists, per fixture, the JSON paths where the response is
// meant to differ from the *.golden.json files.
//
// testdata/feeds/*.golden.json は、gofeed v1.3.0 (golang.org/x/net v0.62.0) で読んだ
// ときの応答 (packFeed の結果) で、このテストを足したときに gofeed で作った。
// -update-golden で、gofeed の応答から作り直せる。
var gofeedDifferences = map[string][]string{}

var updateGolden = flag.Bool("update-golden", false, "write testdata/feeds/*.golden.json from the response gofeed gives")

// The response for every fixture must match the golden response, except for
// the paths listed in gofeedDifferences.
func TestParseFeed_MatchesGofeed(t *testing.T) {
	files, err := filepath.Glob("testdata/feeds/*")
	require.NoError(t, err)
	seen := 0
	used := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, ".golden.json") {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f))
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(f)
			require.NoError(t, err)
			golden := strings.TrimSuffix(f, filepath.Ext(f)) + ".golden.json"
			got := []byte(`{"error":true}`)
			if parsed, err := parseFeed(body); err == nil {
				got, err = json.Marshal(packFeed(parsed))
				require.NoError(t, err)
			}
			if *updateGolden {
				var out bytes.Buffer
				require.NoError(t, json.Indent(&out, got, "", "  "))
				out.WriteByte('\n')
				require.NoError(t, os.WriteFile(golden, out.Bytes(), 0o644))
			}
			want, err := os.ReadFile(golden)
			require.NoError(t, err)
			var w, g any
			require.NoError(t, json.Unmarshal(want, &w))
			require.NoError(t, json.Unmarshal(got, &g))
			diffs := jsonDiffPaths("", w, g)
			sort.Strings(diffs)
			expected := append([]string(nil), gofeedDifferences[name]...)
			sort.Strings(expected)
			if len(expected) > 0 {
				used[name] = true
			}
			assert.Equal(t, expected, diffs, "got %s", got)
		})
		seen++
	}
	// 走査が空振りしても緑にならないよう、件数の下限を置く。
	assert.GreaterOrEqual(t, seen, 40)
	for name := range gofeedDifferences {
		assert.True(t, used[name], "gofeedDifferences has an entry for a missing fixture: %s", name)
	}
}

// jsonDiffPaths returns the paths where a and b differ.
func jsonDiffPaths(path string, a, b any) []string {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		var out []string
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		for k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			out = append(out, jsonDiffPaths(p, am[k], bm[k])...)
		}
		return out
	}
	as, aok := a.([]any)
	bs, bok := b.([]any)
	if aok && bok && len(as) == len(bs) {
		var out []string
		for i := range as {
			out = append(out, jsonDiffPaths(fmt.Sprintf("%s[%d]", path, i), as[i], bs[i])...)
		}
		return out
	}
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	if string(aj) != string(bj) {
		return []string{path}
	}
	return nil
}
