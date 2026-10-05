package entity

import (
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// PackAnnouncement converts a model.Announcement into the map shape used by
// /api/announcements and the `announcementCreated` WebSocket event body.
// isRead is a viewer-dependent flag controlled by the caller; for events
// emitted at creation time it should be false. forYou is set to true when
// the announcement targets a specific user (UserID != nil).
// PackAnnouncement serialises an announcement into the upstream-compatible shape.
// viewerID is the requesting user's ID ("" for me-less paths such as admin create
// and broadcast events); it drives forYou (#2106 L52: upstream は userId === me.id 判定)。
func PackAnnouncement(a *model.Announcement, idGen id.Generator, isRead bool, viewerID string) map[string]any {
	if a == nil {
		return nil
	}
	const tsFormat = "2006-01-02T15:04:05.000Z"
	createdAt := ""
	if idGen != nil {
		if t, err := idGen.ParseTime(a.ID); err == nil {
			createdAt = t.UTC().Format(tsFormat)
		}
	}
	var updatedAt *string
	if a.UpdatedAt != nil {
		s := a.UpdatedAt.UTC().Format(tsFormat)
		updatedAt = &s
	}
	return map[string]any{
		"id":        a.ID,
		"createdAt": createdAt,
		"updatedAt": updatedAt,
		"title":     a.Title,
		"text":      a.Text,
		// announcement の imageUrl は admin 設定だが remote URL を指せる。
		// frontend は <img :src="announcement.imageUrl"> で直接読むため、
		// remote origin は proxy 経由へ書き換えて IP 漏洩を防ぐ (#1529)。
		"imageUrl":               ProxyMediaURLPtr(a.ImageURL),
		"icon":                   a.Icon,
		"display":                a.Display,
		"needConfirmationToRead": a.NeedConfirmationToRead,
		"silence":                a.Silence,
		// #2106 L52: upstream は forYou = (announcement.userId === me?.id)。me-less (viewerID="")
		// では false。通常は List クエリで user-targeted 行が viewer にしか返らないため一致するが、
		// create レスポンス (me-less) や匿名閲覧で厳密一致させる。
		"forYou": a.UserID != nil && *a.UserID == viewerID,
		"isRead": isRead,
	}
}
