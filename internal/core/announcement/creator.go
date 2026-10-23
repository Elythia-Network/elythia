// Package announcement provides a thin creation service that persists an
// announcement and emits the realtime `announcementCreated` main-stream
// event, mirroring upstream AnnouncementService.create (#1606).
//
// Repository の Create を直接呼ぶ経路 (例: #1563 の moderator-activity check)
// は stream event が出ず、対象 user が realtime に announcement へ気付けない。
// 永続化と event 発行を常に対で行いたい caller はこの Creator を使う。
// announcements handler (AdminCreate) は modlog 等が絡む既存経路のため
// 本パッケージへは未統合 (同 shape の publish を handler 内で行っている)。
package announcement

import (
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// Repo is the persistence dependency (repository.AnnouncementRepository).
type Repo interface {
	Create(a *model.Announcement) error
}

// MainStreamPublisher emits events to a user's `main` WebSocket channel
// (internal/stream). Accepted as an interface to avoid a hard dependency on
// internal/stream (循環依存回避、api/announcements の同名 interface と同型)。
type MainStreamPublisher interface {
	PublishMainEvent(userID, eventType string, body any)
}

// BroadcastPublisher emits an instance-wide event to every connected client.
// global announcement の announcementCreated 配信に使う (#2056)。
type BroadcastPublisher interface {
	PublishBroadcast(eventType string, body any)
}

// Creator persists announcements and publishes the `announcementCreated`
// event for per-user announcements, like upstream AnnouncementService.create.
type Creator struct {
	repo      Repo
	idGen     id.Generator
	publisher MainStreamPublisher
	broadcast BroadcastPublisher
	// entityPacker は entity を包むときの依存 (role 由来の値、メディアのプロキシ等) を
	// 持つ (#3796)。組み立てで渡す。
	entityPacker *entity.Packer
}

// SetEntityPacker wires the Packer this Creator packs entities with (#3796).
func (c *Creator) SetEntityPacker(p *entity.Packer) { c.entityPacker = p }

// HasEntityPacker reports whether SetEntityPacker received a Packer that
// rewrites media URLs. 無いとお知らせの imageUrl を生のまま返し、閲覧者の IP が
// リモートへ漏れる。
func (c *Creator) HasEntityPacker() bool { return c.entityPacker.HasMediaURL() }

// NewCreator constructs a Creator. publisher / broadcast may be nil to disable
// the corresponding stream emit (graceful for tests / partial wiring)。
func NewCreator(repo Repo, idGen id.Generator, publisher MainStreamPublisher, broadcast BroadcastPublisher) *Creator {
	return &Creator{repo: repo, idGen: idGen, publisher: publisher, broadcast: broadcast}
}

// Create persists the announcement and, for a per-user announcement
// (UserID != nil), publishes `announcementCreated` to the target user's main
// channel with the upstream event body shape ({"announcement": packed}).
//
// global announcement (UserID == nil) は upstream 同様 broadcast stream へ
// publish する (#2056、broadcast インフラ #2046)。
func (c *Creator) Create(a *model.Announcement) error {
	if err := c.repo.Create(a); err != nil {
		return err
	}
	// #2106 L52: broadcast event は me-less なので viewerID="" (forYou=false)。
	body := map[string]any{"announcement": c.entityPacker.PackAnnouncement(a, c.idGen, false, "")}
	if a.UserID != nil {
		if c.publisher != nil {
			c.publisher.PublishMainEvent(*a.UserID, "announcementCreated", body)
		}
	} else if c.broadcast != nil {
		c.broadcast.PublishBroadcast("announcementCreated", body)
	}
	return nil
}
