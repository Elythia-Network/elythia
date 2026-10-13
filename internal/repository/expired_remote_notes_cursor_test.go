package repository

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// 掃除のジョブが「再開した」と記録するかの判定は、DeleteExpiredRemoteNotesAfter が
// カーソルを捨てる判定と同じ期限で行うこと (#3551)。
func TestExpiredRemoteNotesCursorInRange(t *testing.T) {
	day := 24 * time.Hour
	at := func(ago time.Duration) string { return aidxCutoffID(time.Now().Add(-ago)) }

	assert.False(t, ExpiredRemoteNotesCursorInRange(90, ""), "カーソルが無ければ先頭から")
	assert.True(t, ExpiredRemoteNotesCursorInRange(90, at(90*day+12*time.Hour)), "期限より古いカーソルからは再開する")
	assert.False(t, ExpiredRemoteNotesCursorInRange(90, at(89*day+12*time.Hour)), "期限より新しいカーソルは捨てる")
	// 期限 (日数) で結果が変わる。
	assert.False(t, ExpiredRemoteNotesCursorInRange(91, at(90*day+12*time.Hour)))
}
