package i

import (
	"bytes"
	"flag"
	"testing"

	"github.com/elythia-network/elythia/internal/testutil"
)

var updateGolden = flag.Bool("update-golden", false, "write testdata/pages.golden.json with the reading before the change (html.Parse)")

type linksBack struct {
	Profile      bool `json:"profile"`
	ProfileSlash bool `json:"profileSlash"`
}

// Whether each page links back is the same as before the change (x/net/html's
// tree).
func TestHTMLLinksBackTo_MatchGolden(t *testing.T) {
	const me, meSlash = "https://elythia.example/@alice", "https://elythia.example/@alice/"
	testutil.HTMLPagesMatchGolden(t, "testdata/pages.golden.json", *updateGolden,
		func(page []byte) linksBack {
			return linksBack{Profile: legacyLinksBackTo(page, me), ProfileSlash: legacyLinksBackTo(page, meSlash)}
		},
		func(page []byte) linksBack {
			return linksBack{
				Profile:      htmlLinksBackTo(bytes.NewReader(page), me),
				ProfileSlash: htmlLinksBackTo(bytes.NewReader(page), meSlash),
			}
		}, nil)
}
