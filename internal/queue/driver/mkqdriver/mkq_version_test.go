package mkqdriver

import (
	"strconv"
	"strings"
	"testing"

	"github.com/elythia-network/mkq"
	"github.com/stretchr/testify/require"
)

// minMkqForLongIdlePoll is the first mkq whose dispatcher does not sleep for
// IdlePollInterval after a failed dequeue (mkq#154).
var minMkqForLongIdlePoll = [3]int{1, 5, 2}

// TestMkqVersion_SupportsLongIdlePoll pins #4215: the default idle poll of
// 120 seconds is safe only with an mkq that backs off on its own after a
// failed dequeue.
//
// それより前の mkq に戻すと、Redis の瞬断の後に全 worker が 120 秒止まる。
func TestMkqVersion_SupportsLongIdlePoll(t *testing.T) {
	parts := strings.SplitN(mkq.Version, ".", 3)
	require.Len(t, parts, 3, "mkq.Version %q", mkq.Version)
	var got [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(strings.SplitN(p, "-", 2)[0])
		require.NoError(t, err, "mkq.Version %q", mkq.Version)
		got[i] = n
	}
	for i := range got {
		if got[i] != minMkqForLongIdlePoll[i] {
			require.Greater(t, got[i], minMkqForLongIdlePoll[i], "mkq %s is older than %v", mkq.Version, minMkqForLongIdlePoll)
			return
		}
	}
}
