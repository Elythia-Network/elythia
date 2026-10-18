package server

import (
	"strings"
	"testing"
)

// The notification Hook reads a notification's note once for its stream and
// Web Push payloads (#3611). The loader must read the relations the packers
// embed (author, renote / reply targets, polls); wiring the bare FindByID
// would drop them from both payloads. The existence check must be the note
// repository, whose ExistingNoteIDsOnPrimary reads the primary.
func TestNotificationNoteLoaderIsWired(t *testing.T) {
	// パッケージ全体を読む (#3672)。組み立てを別のファイルへ移しても同じ配線を見る。
	src := packageSourceWithoutComments(t)

	for _, call := range []string{
		"notificationHook.SetNoteLoader(noteRepo.FindByIDWithRelations)",
		"notificationHook.SetNoteExistenceChecker(noteRepo)",
	} {
		if !strings.Contains(src, call) {
			t.Errorf("internal/server must wire %s", call)
		}
	}
}
