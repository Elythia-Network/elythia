package federation

import (
	"encoding/json"
	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// BlockingDeliveryHook implements core/blocking.FederationHook by delivering
// Block / Undo(Block) activities to a remote blockee's inbox when a local user
// blocks / unblocks them (#1560, upstream UserBlockingService.block/unblock の
// AP delivery)。blockee がローカル or URI 無しなら no-op。
type BlockingDeliveryHook struct {
	deliver  *DeliverService
	userRepo repository.UserRepository
	renderer *activitypub.Renderer
}

// NewBlockingDeliveryHook constructs a BlockingDeliveryHook.
func NewBlockingDeliveryHook(deliver *DeliverService, userRepo repository.UserRepository, renderer *activitypub.Renderer) *BlockingDeliveryHook {
	return &BlockingDeliveryHook{deliver: deliver, userRepo: userRepo, renderer: renderer}
}

// OnBlocked delivers a Block activity to the remote blockee (#1560)。
func (h *BlockingDeliveryHook) OnBlocked(blockerID, blockeeID string) {
	blocker, blockee, ok := h.resolve(blockerID, blockeeID)
	if !ok {
		return
	}
	body, err := json.Marshal(h.renderer.RenderBlock(blocker.ID, *blockee.URI))
	if err != nil {
		return
	}
	if err := h.deliver.DeliverToUser(blocker.ID, blockee, body); err != nil {
		slog.Warn("blocking delivery: block failed", "blocker", blocker.ID, "blockee", blockee.ID, "err", err)
	}
}

// OnUnblocked delivers an Undo(Block) activity to the remote blockee (#1560)。
func (h *BlockingDeliveryHook) OnUnblocked(blockerID, blockeeID string) {
	blocker, blockee, ok := h.resolve(blockerID, blockeeID)
	if !ok {
		return
	}
	body, err := json.Marshal(h.renderer.RenderUndoBlock(blocker.ID, *blockee.URI))
	if err != nil {
		return
	}
	if err := h.deliver.DeliverToUser(blocker.ID, blockee, body); err != nil {
		slog.Warn("blocking delivery: undo(block) failed", "blocker", blocker.ID, "blockee", blockee.ID, "err", err)
	}
}

// resolve loads blocker / blockee and reports whether AP delivery is needed:
// blocker がローカル、blockee がリモートで URI / inbox を持つときのみ true。
func (h *BlockingDeliveryHook) resolve(blockerID, blockeeID string) (blocker, blockee *model.User, ok bool) {
	if h.deliver == nil || h.userRepo == nil || h.renderer == nil {
		return nil, nil, false
	}
	blocker, err := h.userRepo.FindByID(blockerID)
	if err != nil || blocker == nil || !blocker.IsLocal() {
		return nil, nil, false
	}
	blockee, err = h.userRepo.FindByID(blockeeID)
	if err != nil || blockee == nil || blockee.IsLocal() || blockee.URI == nil || *blockee.URI == "" {
		return nil, nil, false
	}
	return blocker, blockee, true
}
