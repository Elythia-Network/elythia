package mfm

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var updateGolden = flag.Bool("update-golden", false, "write testdata/fromhtml/*.golden.json with the conversion before the change (html.Parse)")

type fromHTMLGolden struct {
	FromHTML           string `json:"fromHTML"`
	WithoutQuoteInline string `json:"withoutQuoteInline"`
}

// fromHTMLDifferences lists the fixtures whose conversion is meant to differ
// from the golden one, with the conversion they give now.
//
// testdata/fromhtml/*.golden.json は、x/net/html (v0.62.0) の木 (html.Parse) を
// 使っていたときの変換 (from_html_legacy_test.go) の結果。-update-golden で作り直せる。
// 違ってよいのは、x/net/html の木の構築が要素を組み替える形だけ
// (docs/divergence/federation.md)。
var fromHTMLDifferences = map[string]fromHTMLGolden{
	// 書式の要素の中で開いたブロックの要素が閉じないうちに、書式の要素を閉じた形。
	// x/net/html はブロックを外へ出して書式を付け直すが、ここでは書式の要素ごと閉じる。
	"broken-block-in-formatting": {FromHTML: "**bold\nblock**aftertail", WithoutQuoteInline: "**bold\nblock**aftertail"},
	// 表の中でセルの外に書いた文字。x/net/html は表の前へ移すが、ここでは移さない。
	"broken-text-in-table": {FromHTML: "cellstray", WithoutQuoteInline: "cellstray"},
}

// Every fixture converts to its golden text, except the ones listed in
// fromHTMLDifferences.
func TestFromHTML_MatchesGolden(t *testing.T) {
	files, err := filepath.Glob("testdata/fromhtml/*.html")
	require.NoError(t, err)
	used := map[string]bool{}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".html")
		golden := strings.TrimSuffix(f, ".html") + ".golden.json"
		body, err := os.ReadFile(f)
		require.NoError(t, err)
		if *updateGolden {
			var out bytes.Buffer
			enc := json.NewEncoder(&out)
			enc.SetEscapeHTML(false)
			enc.SetIndent("", "  ")
			require.NoError(t, enc.Encode(fromHTMLGolden{
				FromHTML:           legacyFromHTMLPublic(string(body)),
				WithoutQuoteInline: legacyFromHTMLWithoutQuoteInline(string(body)),
			}))
			require.NoError(t, os.WriteFile(golden, out.Bytes(), 0o644))
		}
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(golden)
			require.NoError(t, err)
			var want fromHTMLGolden
			require.NoError(t, json.Unmarshal(raw, &want))
			got := fromHTMLGolden{
				FromHTML:           FromHTML(string(body)),
				WithoutQuoteInline: FromHTMLWithoutQuoteInline(string(body)),
			}
			if d, ok := fromHTMLDifferences[name]; ok {
				used[name] = true
				assert.NotEqual(t, want, got, "the fixture no longer differs; remove it from fromHTMLDifferences")
				want = d
			}
			assert.Equal(t, want, got)
		})
	}
	// 走査が空振りしても緑にならないよう、件数の下限を置く。
	assert.GreaterOrEqual(t, len(files), 50)
	for name := range fromHTMLDifferences {
		assert.True(t, used[name], "fromHTMLDifferences has an entry for a missing fixture: %s", name)
	}
}
