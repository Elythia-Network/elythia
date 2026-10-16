package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The notification Hook reads a notification's note once for its stream and
// Web Push payloads (#3611). The loader must read the relations the packers
// embed (author, renote / reply targets, polls); wiring the bare FindByID
// would drop them from both payloads. The existence check must be the note
// repository, whose ExistingNoteIDsOnPrimary reads the primary.
func TestNotificationNoteLoaderIsWired(t *testing.T) {
	router := filepath.Join(repoRootDir(t), "internal", "server", "router.go")
	raw, err := os.ReadFile(router)
	require.NoError(t, err)
	src := stripComments(string(raw))

	for _, call := range []string{
		"notificationHook.SetNoteLoader(noteRepo.FindByIDWithRelations)",
		"notificationHook.SetNoteExistenceChecker(noteRepo)",
	} {
		if !strings.Contains(src, call) {
			t.Errorf("router.go must wire %s", call)
		}
	}
}
