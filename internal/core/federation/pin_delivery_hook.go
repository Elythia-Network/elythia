package federation

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// PinDeliveryHook delivers an Add / Remove(featured collection) activity to a
// local user's remote followers when they pin / unpin a note (#2024、upstream
// NotePiningService.deliverPinnedChange)。これにより remote instance が actor の
// featured collection (pinned notes) の変更を反映できる。
//
// upstream の gate に合わせ、local user かつ note が !localOnly かつ visibility が
// public/home のときだけ配信する (それ以外は連合露出範囲外)。失敗はベスト
// エフォートで log のみ。
type PinDeliveryHook struct {
	deliver  *DeliverService
	renderer *activitypub.Renderer
	userRepo repository.UserRepository
	noteRepo repository.NoteRepository
	// relay は pin 変更を relay subscriber にも fan-out する optional 依存
	// (upstream deliverToRelays)。nil なら relay 配送 skip。
	relay RelayBroadcaster
}

// NewPinDeliveryHook constructs a PinDeliveryHook.
func NewPinDeliveryHook(deliver *DeliverService, renderer *activitypub.Renderer, userRepo repository.UserRepository, noteRepo repository.NoteRepository) *PinDeliveryHook {
	return &PinDeliveryHook{deliver: deliver, renderer: renderer, userRepo: userRepo, noteRepo: noteRepo}
}

// SetRelayBroadcaster wires the relay fan-out target (#2024)。
func (h *PinDeliveryHook) SetRelayBroadcaster(r RelayBroadcaster) {
	h.relay = r
}

// OnLocalPinChanged renders Add (pin) / Remove (unpin) and fans it out to the
// user's remote followers + relays (#2024)。
func (h *PinDeliveryHook) OnLocalPinChanged(userID, noteID string, isAddition bool) {
	if h.deliver == nil || h.renderer == nil || h.userRepo == nil || h.noteRepo == nil {
		return
	}
	user, err := h.userRepo.FindByID(userID)
	if err != nil || user == nil || !user.IsLocal() {
		return
	}
	note, err := h.noteRepo.FindByID(noteID)
	if err != nil || note == nil {
		return
	}
	// upstream NotePiningService の gate: localOnly や followers/specified note は
	// 連合配信しない (featured collection に出ない)。
	if note.LocalOnly || (note.Visibility != model.NoteVisibilityPublic && note.Visibility != model.NoteVisibilityHome) {
		return
	}
	var activity any
	if isAddition {
		activity = h.renderer.RenderAddToFeatured(userID, noteID)
	} else {
		activity = h.renderer.RenderRemoveFromFeatured(userID, noteID)
	}
	body, err := json.Marshal(activity)
	if err != nil {
		return
	}
	if err := h.deliver.DeliverToFollowers(userID, body); err != nil {
		slog.Warn("pin delivery: deliver to followers failed", "userID", userID, "noteID", noteID, "err", err)
	}
	if h.relay != nil {
		if err := h.relay.DeliverToAccepted(context.Background(), userID, activity); err != nil {
			slog.Warn("pin delivery: relay deliver failed", "userID", userID, "noteID", noteID, "err", err)
		}
	}
}
