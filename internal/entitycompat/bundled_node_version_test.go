package entitycompat

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nodeImageVersionRe captures the version from a `node:<version>-<distro>` image.
var nodeImageVersionRe = regexp.MustCompile(`^(?:docker\.io/(?:library/)?)?node:(\d+\.\d+\.\d+)-`)

// TestBundledNodeVersionMatchesFrontend checks that Dockerfile.bundled builds the
// frontend with the Node.js version pinned in frontend/.node-version.
//
// Dockerfile は .node-version を読めないので、版を FROM に書き写している。CI
// (frontend.yml) と手元 (make e2e-frontend-build) は .node-version を読むので、
// 書き写しが古いと配る image だけ別の Node でビルドされ、CI で確かめた成果物と
// 違うものが出る (#3379)。
func TestBundledNodeVersionMatchesFrontend(t *testing.T) {
	want := strings.TrimSpace(readRepoFile(t, "frontend/.node-version"))
	require.NotEmpty(t, want, "frontend/.node-version が空")

	var got []string
	for _, ref := range externalBaseImages(readRepoFile(t, "Dockerfile.bundled")) {
		if m := nodeImageVersionRe.FindStringSubmatch(ref.value); m != nil {
			got = append(got, m[1])
		}
	}
	require.Len(t, got, 1, "Dockerfile.bundled から node の FROM を 1 つだけ拾えるはず。書式が変わったならこのゲートも直すこと")
	assert.Equalf(t, want, got[0],
		"Dockerfile.bundled の node (%s) が frontend/.node-version (%s) と違う。digest も取り直すこと", got[0], want)
}
