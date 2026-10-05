package webpush_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/webpush"
	"github.com/stretchr/testify/assert"
)

func TestTruncateBody_NonNotificationReturnsBody(t *testing.T) {
	body := map[string]any{"foo": "bar"}
	got := webpush.TruncateBody(webpush.TypeReadAllNotifications, body)
	assert.Equal(t, body, got)
}

func TestTruncateBody_NewChatMessageReturnsBody(t *testing.T) {
	body := map[string]any{"id": "m1", "text": "hi"}
	got := webpush.TruncateBody(webpush.TypeNewChatMessage, body)
	assert.Equal(t, body, got)
}

func TestTruncateBody_NilBody(t *testing.T) {
	got := webpush.TruncateBody(webpush.TypeNotification, nil)
	assert.Nil(t, got)
}

func TestTruncateBody_NoNoteKey(t *testing.T) {
	body := map[string]any{"type": "follow"}
	got := webpush.TruncateBody(webpush.TypeNotification, body)
	assert.Equal(t, body, got)
}

func TestTruncateBody_NoteNotMap(t *testing.T) {
	body := map[string]any{"note": "string-value"}
	got := webpush.TruncateBody(webpush.TypeNotification, body)
	assert.Equal(t, body, got)
}

func TestTruncateBody_Notification_StripsFields(t *testing.T) {
	body := map[string]any{
		"type": "mention",
		"note": map[string]any{
			"id":     "n1",
			"text":   "hello world",
			"cw":     "spoiler",
			"reply":  map[string]any{"id": "p1"},
			"renote": map[string]any{"id": "r1"},
			"user":   map[string]any{"id": "u1"},
		},
	}
	got := webpush.TruncateBody(webpush.TypeNotification, body)

	note := got["note"].(map[string]any)
	// summary source は note 自体 (non-renote型)。cwが優先されるので
	// summary は "spoiler" となる。
	assert.Equal(t, "spoiler", note["text"])
	assert.Nil(t, note["cw"])
	assert.Nil(t, note["reply"])
	assert.Nil(t, note["renote"])
	assert.Nil(t, note["user"])
	assert.Equal(t, "n1", note["id"])
}

func TestTruncateBody_UnreadAntennaNote_KeepsUser(t *testing.T) {
	body := map[string]any{
		"type": "unreadAntennaNote",
		"note": map[string]any{
			"text": "body",
			"user": map[string]any{"id": "u1"},
		},
	}
	got := webpush.TruncateBody(webpush.TypeUnreadAntennaNote, body)
	note := got["note"].(map[string]any)
	assert.Equal(t, map[string]any{"id": "u1"}, note["user"])
}

func TestTruncateBody_RenoteUsesRenoteSource(t *testing.T) {
	body := map[string]any{
		"type": "renote",
		"note": map[string]any{
			"text":   "",
			"renote": map[string]any{"text": "original"},
		},
	}
	got := webpush.TruncateBody(webpush.TypeNotification, body)
	note := got["note"].(map[string]any)
	assert.Equal(t, "original", note["text"])
	// renote field still stripped after summary extraction
	assert.Nil(t, note["renote"])
}

func TestTruncateBody_RenoteTypeWithNilRenote(t *testing.T) {
	body := map[string]any{
		"type": "renote",
		"note": map[string]any{
			"text":   "fallback",
			"renote": nil,
		},
	}
	got := webpush.TruncateBody(webpush.TypeNotification, body)
	note := got["note"].(map[string]any)
	// renote が nil の場合は note 自体を summary source として使う
	assert.Equal(t, "fallback", note["text"])
}

func TestTruncateBody_DoesNotMutateInput(t *testing.T) {
	input := map[string]any{
		"type": "mention",
		"note": map[string]any{
			"text": "keep",
			"cw":   "cw",
		},
	}
	_ = webpush.TruncateBody(webpush.TypeNotification, input)
	// 元の map が変更されていないことを確認
	origNote := input["note"].(map[string]any)
	assert.Equal(t, "keep", origNote["text"])
	assert.Equal(t, "cw", origNote["cw"])
}
