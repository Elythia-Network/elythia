package instance

import (
	"flag"
	"testing"

	"github.com/elythia-network/elythia/internal/testutil"
)

var updateGolden = flag.Bool("update-golden", false, "write testdata/pages.golden.json with the reading before the change (html.Parse)")

type iconLinks struct {
	Icon  string `json:"icon"`
	Apple string `json:"apple"`
}

// The icon links read from each page are the ones read before the change
// (x/net/html's tree).
func TestParseIconLinks_MatchGolden(t *testing.T) {
	const pageURL = "https://remote.example/"
	testutil.HTMLPagesMatchGolden(t, "testdata/pages.golden.json", *updateGolden,
		func(page []byte) iconLinks {
			icon, apple := legacyParseIconLinks(page, pageURL)
			return iconLinks{Icon: icon, Apple: apple}
		},
		func(page []byte) iconLinks {
			icon, apple := parseIconLinks(page, pageURL)
			return iconLinks{Icon: icon, Apple: apple}
		}, nil)
}
