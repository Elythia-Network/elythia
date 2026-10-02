package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMoveTargetLocalURIResolutionIsWired(t *testing.T) {
	router := filepath.Join(repoRootDir(t), "internal", "server", "router.go")
	raw, err := os.ReadFile(router)
	require.NoError(t, err)
	src := stripComments(string(raw))

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
