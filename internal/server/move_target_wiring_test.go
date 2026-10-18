package server

import (
	"strings"
	"testing"
)

func TestMoveTargetLocalURIResolutionIsWired(t *testing.T) {
	// パッケージ全体を読む (#3672)。組み立てを別のファイルへ移しても同じ配線を見る。
	src := packageSourceWithoutComments(t)

	for _, wiring := range []struct {
		call, endpoint string
	}{
		{"usersHandler.SetServerURL(s.config.URL)", "users/show"},
		{"iHandler.SetServerURL(s.config.URL)", "/api/i"},
	} {
		if !strings.Contains(src, wiring.call) {
			t.Errorf("%s must receive the canonical server URL used to resolve local move targets", wiring.endpoint)
		}
	}
}
