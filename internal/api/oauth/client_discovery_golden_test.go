package oauth

import (
	"flag"
	"testing"

	"github.com/elythia-network/elythia/internal/testutil"
)

var updateGolden = flag.Bool("update-golden", false, "write testdata/pages.golden.json with the reading before the change (html.Parse)")

// The redirect URIs, name and logo read from each page are the ones read before
// the change (x/net/html's tree).
func TestApplyHTMLMicroformats_MatchGolden(t *testing.T) {
	const id = "https://app.example/"
	testutil.HTMLPagesMatchGolden(t, "testdata/pages.golden.json", *updateGolden,
		func(page []byte) clientInfo {
			info := clientInfo{ID: id}
			legacyApplyHTMLMicroformats(&info, id, id, page)
			return info
		},
		func(page []byte) clientInfo {
			info := clientInfo{ID: id}
			applyHTMLMicroformats(&info, id, id, page)
			return info
		}, nil)
}
