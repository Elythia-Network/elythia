package notesummary_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/misc/notesummary"
	"github.com/stretchr/testify/assert"
)

func TestGet_Text(t *testing.T) {
	note := map[string]any{"text": "hello world"}
	assert.Equal(t, "hello world", notesummary.Get(note))
}

func TestGet_CWTakesPrecedence(t *testing.T) {
	note := map[string]any{"cw": "spoiler", "text": "body"}
	assert.Equal(t, "spoiler", notesummary.Get(note))
}

func TestGet_Deleted(t *testing.T) {
	note := map[string]any{"deletedAt": "2026-01-01T00:00:00Z", "text": "x"}
	assert.Equal(t, "(\u274C\u26D4)", notesummary.Get(note))
}

func TestGet_Hidden(t *testing.T) {
	note := map[string]any{"isHidden": true, "text": "x"}
	assert.Equal(t, "(\u26D4)", notesummary.Get(note))
}

func TestGet_Files(t *testing.T) {
	note := map[string]any{"text": "check", "files": []any{map[string]any{}, map[string]any{}}}
	assert.Equal(t, "check (\U0001F4CE2)", notesummary.Get(note))
}

func TestGet_Poll(t *testing.T) {
	note := map[string]any{"text": "vote", "poll": map[string]any{"choices": []any{}}}
	assert.Equal(t, "vote (\U0001F4CA)", notesummary.Get(note))
}

func TestGet_ReplyRecursive(t *testing.T) {
	note := map[string]any{
		"text":    "reply body",
		"replyId": "n1",
		"reply":   map[string]any{"text": "parent"},
	}
	assert.Equal(t, "reply body\n\nRE: parent", notesummary.Get(note))
}

func TestGet_RenoteRecursive(t *testing.T) {
	note := map[string]any{
		"text":     "",
		"renoteId": "n2",
		"renote":   map[string]any{"text": "quoted"},
	}
	assert.Equal(t, "RN: quoted", notesummary.Get(note))
}

func TestGet_ReplyIDWithoutReplyObject(t *testing.T) {
	note := map[string]any{
		"text":    "x",
		"replyId": "n1",
	}
	// #2106 L63: replyId が立っているのに reply が未 hydrate でも upstream は "RE: ..." を付与する。
	assert.Equal(t, "x\n\nRE: ...", notesummary.Get(note))
}

// #2106 L63: renoteId が立っているのに renote が未 hydrate でも "RN: ..." を付与する。
func TestGet_RenoteIDWithoutRenoteObject(t *testing.T) {
	note := map[string]any{
		"text":     "y",
		"renoteId": "n2",
	}
	assert.Equal(t, "y\n\nRN: ...", notesummary.Get(note))
}

// #2106 L64: 空文字 CW は (null ではないので) text にフォールバックせず空 CW を採用する。
func TestGet_EmptyStringCW(t *testing.T) {
	note := map[string]any{
		"cw":   "",
		"text": "body",
	}
	assert.Equal(t, "", notesummary.Get(note))
}

func TestGet_Nil(t *testing.T) {
	assert.Equal(t, "", notesummary.Get(nil))
}

func TestGet_Empty(t *testing.T) {
	assert.Equal(t, "", notesummary.Get(map[string]any{}))
}

func TestGet_DeletedAtNilValue(t *testing.T) {
	note := map[string]any{"deletedAt": nil, "text": "alive"}
	assert.Equal(t, "alive", notesummary.Get(note))
}

func TestGet_FilesEmptyArray(t *testing.T) {
	note := map[string]any{"text": "x", "files": []any{}}
	assert.Equal(t, "x", notesummary.Get(note))
}

func TestGet_PollNil(t *testing.T) {
	note := map[string]any{"text": "x", "poll": nil}
	assert.Equal(t, "x", notesummary.Get(note))
}

func TestGet_NestedReplyAndRenote(t *testing.T) {
	note := map[string]any{
		"text":     "top",
		"replyId":  "r1",
		"reply":    map[string]any{"cw": "cw1", "text": "inner"},
		"renoteId": "rn1",
		"renote":   map[string]any{"text": "quoted"},
	}
	got := notesummary.Get(note)
	assert.Equal(t, "top\n\nRE: cw1\n\nRN: quoted", got)
}
