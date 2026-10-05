package federation

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/repository"
)

// ProfileUpdateDeliveryHook implements api/i.ProfileUpdateHook by rendering an
// Update(Person) and delivering it to the local user's remote followers when
// they edit their profile (#1560, upstream AccountUpdateService)。これにより
// remote instance が name/avatar/bio 等の変更を即座に反映できる。
//
// 配信用 Person は RSA-only (ed25519 assertionMethod は付けない) で十分:
// Update activity 自体は DeliverActivity が signer の RSA 鍵で署名し、
// assertionMethod は verify capability の advertise でしかないため。
type ProfileUpdateDeliveryHook struct {
	deliver     *DeliverService
	renderer    *activitypub.Renderer
	userRepo    repository.UserRepository
	keypairRepo repository.UserKeypairRepository
	// keypairExtraRepo は ed25519 公開鍵 (FEP-521a Multikey) を Person の
	// assertionMethod に含めるための optional 依存。配信する Update(Person) を
	// actor endpoint と同じ shape にし、follower 側 cache merge で ed25519
	// verify capability が一時的に落ちないようにする (#1560 review)。
	keypairExtraRepo repository.UserKeypairExtraRepository
	// relay は profile 更新を relay subscriber にも fan-out する optional 依存
	// (upstream AccountUpdateService.publishToFollowers + deliverToRelays、
	// #1560 review)。nil なら relay 配送 skip。
	relay RelayBroadcaster
}

// NewProfileUpdateDeliveryHook constructs a ProfileUpdateDeliveryHook.
func NewProfileUpdateDeliveryHook(deliver *DeliverService, renderer *activitypub.Renderer, userRepo repository.UserRepository, keypairRepo repository.UserKeypairRepository) *ProfileUpdateDeliveryHook {
	return &ProfileUpdateDeliveryHook{deliver: deliver, renderer: renderer, userRepo: userRepo, keypairRepo: keypairRepo}
}

// SetKeypairExtraRepo wires the Ed25519 keypair repo so the delivered
// Update(Person) carries assertionMethod like the actor endpoint (#1560)。
func (h *ProfileUpdateDeliveryHook) SetKeypairExtraRepo(r repository.UserKeypairExtraRepository) {
	h.keypairExtraRepo = r
}

// SetRelayBroadcaster wires the relay fan-out target (#1560)。
func (h *ProfileUpdateDeliveryHook) SetRelayBroadcaster(r RelayBroadcaster) {
	h.relay = r
}

// lookupEd25519 returns the user's Ed25519 public key when a row exists
// (read-only; backfill は actor endpoint 側が担う)。無ければ nil で RSA-only。
func (h *ProfileUpdateDeliveryHook) lookupEd25519(userID string) ed25519.PublicKey {
	if h.keypairExtraRepo == nil {
		return nil
	}
	row, err := h.keypairExtraRepo.FindByUserID(userID)
	if err != nil || row == nil {
		return nil
	}
	pub, err := activitypub.ParseEd25519PublicKeyPEM(row.Ed25519PublicKey)
	if err != nil {
		return nil
	}
	return pub
}

// OnLocalProfileUpdated renders Update(Person) and fans it out to the user's
// remote followers (#1560)。失敗はベストエフォートで log のみ。
func (h *ProfileUpdateDeliveryHook) OnLocalProfileUpdated(userID string) {
	if h.deliver == nil || h.renderer == nil || h.userRepo == nil || h.keypairRepo == nil {
		return
	}
	user, err := h.userRepo.FindByID(userID)
	if err != nil || user == nil || !user.IsLocal() {
		return
	}
	profile, err := h.userRepo.FindProfileByUserID(userID)
	if err != nil {
		// profile 行が無くても actor の基本フィールドは出せるので nil で続行。
		profile = nil
	}
	keypair, err := h.keypairRepo.FindByUserID(userID)
	if err != nil || keypair == nil {
		slog.Warn("profile update delivery: keypair lookup failed", "userID", userID, "err", err)
		return
	}
	person := h.renderer.RenderPerson(user, profile, keypair.PublicKey, h.lookupEd25519(userID))
	update := h.renderer.RenderUpdate(person)
	body, err := json.Marshal(update)
	if err != nil {
		return
	}
	if err := h.deliver.DeliverToFollowers(userID, body); err != nil {
		slog.Warn("profile update delivery: deliver to followers failed", "userID", userID, "err", err)
	}
	// relay subscriber にも配信する (upstream deliverToRelays、#1560)。
	if h.relay != nil {
		if err := h.relay.DeliverToAccepted(context.Background(), userID, update); err != nil {
			slog.Warn("profile update delivery: relay deliver failed", "userID", userID, "err", err)
		}
	}
}
