// Package webpush implements Misskey-compatible Web Push delivery.
// Truncation of notification payloads mirrors PushNotificationService.ts
// in the upstream Misskey codebase.
package webpush

import (
	"maps"

	"github.com/elythia-network/elythia/internal/misc/notesummary"
)

// Push types recognized by the Misskey service worker (sw.js).
const (
	TypeNotification         = "notification"
	TypeUnreadAntennaNote    = "unreadAntennaNote"
	TypeReadAllNotifications = "readAllNotifications"
	TypeNewChatMessage       = "newChatMessage"
)

// TruncateBody returns a shallow clone of body with size-heavy fields stripped
// for the given push type. Mirrors truncateBody() in the upstream
// PushNotificationService.ts.
//
// `notification` と `unreadAntennaNote` のみが対象。他のタイプはbodyを
// そのまま返す。body が nil、または note を含まない場合もそのまま返す。
func TruncateBody(pushType string, body map[string]any) map[string]any {
	if body == nil {
		return nil
	}
	if pushType != TypeNotification && pushType != TypeUnreadAntennaNote {
		return body
	}

	out := make(map[string]any, len(body))
	maps.Copy(out, body)

	rawNote, ok := out["note"]
	if !ok || rawNote == nil {
		return out
	}
	note, ok := rawNote.(map[string]any)
	if !ok {
		return out
	}

	// 本家の truncateBody は renote 通知の場合だけ renote 側の text を
	// 要約ソースにする。元の note は mutate せずクローンを返す。
	summarySource := note
	if t, ok := out["type"].(string); ok && t == "renote" {
		if rn, ok := note["renote"].(map[string]any); ok && rn != nil {
			summarySource = rn
		}
	}

	truncated := make(map[string]any, len(note))
	maps.Copy(truncated, note)
	truncated["text"] = notesummary.Get(summarySource)
	truncated["cw"] = nil
	truncated["reply"] = nil
	truncated["renote"] = nil
	if pushType == TypeNotification {
		truncated["user"] = nil
	}
	out["note"] = truncated
	return out
}
