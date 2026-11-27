package urlpreview

import (
	"bytes"
	"flag"
	"testing"

	"github.com/elythia-network/elythia/internal/testutil"
)

var updateGolden = flag.Bool("update-golden", false, "write testdata/pages.golden.json with the reading before the change (html.Parse)")

type pageRead struct {
	OK          bool     `json:"ok"`
	Meta        metaMap  `json:"meta"`
	IframeSrc   string   `json:"iframeSrc"`
	IframeAllow []string `json:"iframeAllow"`
}

// The meta, title, link and iframe values read from each page are the ones
// read before the change (x/net/html's tree).
func TestReadPages_MatchGolden(t *testing.T) {
	testutil.HTMLPagesMatchGolden(t, "testdata/pages.golden.json", *updateGolden,
		func(page []byte) pageRead {
			m, ok := legacyParseMeta(page)
			src, allow := legacyExtractIframe(string(page))
			return pageRead{OK: ok, Meta: m, IframeSrc: src, IframeAllow: allow}
		},
		func(page []byte) pageRead {
			m, ok := extractMeta(bytes.NewReader(page))
			if !ok {
				m = nil
			}
			src, allow := extractIframe(string(page))
			return pageRead{OK: ok, Meta: m, IframeSrc: src, IframeAllow: allow}
		}, nil)
}
