// Package relay implements the ActivityPub relay subscription protocol.
// It ties together the local `relay` table, the instance's relay system
// actor (via core/systemaccount), and the outbound HTTP-signed delivery
// pipeline (via DeliverEnqueuer).
//
// Upstream reference: Misskey's core/RelayService.ts.
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// Relay statuses — mirror the upstream string values so admin UIs and
// Accept/Reject inbox handlers can interoperate.
const (
	StatusRequesting = "requesting"
	StatusAccepted   = "accepted"
	StatusRejected   = "rejected"
)

// SystemAccountFetcher abstracts core/systemaccount.Service so relay
// can be unit-tested without pulling in the user/keypair/SA repos.
type SystemAccountFetcher interface {
	Fetch(kind string) (*model.User, error)
}

// Deliverer is the subset of federation.DeliverService that relay
// depends on. Separated as an interface so mocks can record activity
// payloads without pulling the whole delivery pipeline.
type Deliverer interface {
	DeliverActivity(signerUserID string, body []byte, inboxes []string) error
}

// KeypairProvider resolves a local user's RSA keypair for LD signing.
// repository.UserKeypairRepository がこれを満たす。
type KeypairProvider interface {
	FindByUserID(userID string) (*model.UserKeypair, error)
}

// LdSigner attaches an RsaSignature2017 LD-Signature to a JSON-LD activity map.
// *activitypub/ld.Processor がこの signature を構造的に満たす。
type LdSigner interface {
	SignRsaSignature2017(activity map[string]any, privateKeyPEM, creator string, created time.Time) (map[string]any, error)
}

// Service orchestrates add/remove/mark/list/deliver operations against
// the local relay table and the AP delivery pipeline.
type Service struct {
	repo     repository.RelayRepository
	sysAcct  SystemAccountFetcher
	renderer *activitypub.Renderer
	deliver  Deliverer
	idGen    id.Generator
	clock    func() time.Time

	// LD-Signature 配送 (optional)。3 つとも揃ったときだけ relay 配送 activity に
	// RsaSignature2017 を付与する (#1870)。未配線なら従来どおり HTTP-sig のみ。
	keypairRepo KeypairProvider
	ldSigner    LdSigner
	baseURL     string

	// accepted list cache with 10-minute TTL, matching upstream.
	cacheMu     sync.RWMutex
	cacheLoaded time.Time
	cacheRelays []*model.Relay
	cacheMaxAge time.Duration
}

// NewService constructs a Service.
func NewService(
	repo repository.RelayRepository,
	sysAcct SystemAccountFetcher,
	renderer *activitypub.Renderer,
	deliver Deliverer,
	idGen id.Generator,
) *Service {
	return &Service{
		repo:        repo,
		sysAcct:     sysAcct,
		renderer:    renderer,
		deliver:     deliver,
		idGen:       idGen,
		clock:       time.Now,
		cacheMaxAge: 10 * time.Minute,
	}
}

// SetClock replaces the clock, primarily for tests.
func (s *Service) SetClock(now func() time.Time) {
	if now != nil {
		s.clock = now
	}
}

// SetLdSigning wires the RsaSignature2017 LD-Signature path for relay delivery
// (upstream deliverToRelays → attachLdSignature, #1870)。baseURL は creator key
// URI 構築用 (例: "https://example.com")。引数のいずれかが nil/空なら LD 署名は
// 無効化され、従来どおり HTTP-sig のみで配送する。
func (s *Service) SetLdSigning(keypairRepo KeypairProvider, signer LdSigner, baseURL string) {
	s.keypairRepo = keypairRepo
	s.ldSigner = signer
	s.baseURL = baseURL
}

// SetCacheMaxAge overrides the default 10-minute TTL on the accepted
// relays cache. 0 以下は無視。
func (s *Service) SetCacheMaxAge(d time.Duration) {
	if d > 0 {
		s.cacheMaxAge = d
	}
}

// Add creates a relay row (status=requesting), renders a Follow
// activity signed by the relay actor, and enqueues it to the relay
// inbox. Returns the persisted row.
//
// Follow 配信が失敗した場合は挿入済みの行をロールバックする。inbox 列は
// unique index なので orphan 行が残ると同じ inbox での再 Add が
// unique 制約で失敗してしまい、管理者が復旧できなくなるため。
func (s *Service) Add(ctx context.Context, inbox string) (*model.Relay, error) {
	if inbox == "" {
		return nil, errors.New("relay: inbox is required")
	}
	rel := &model.Relay{
		ID:     s.idGen.Generate(s.clock()),
		Inbox:  inbox,
		Status: StatusRequesting,
	}
	if err := s.repo.Create(rel); err != nil {
		return nil, fmt.Errorf("relay: create row: %w", err)
	}
	if err := s.sendFollow(ctx, rel); err != nil {
		_ = s.repo.Delete(rel.ID)
		return nil, err
	}
	s.invalidateCache()
	return rel, nil
}

// Remove sends an Undo(Follow) to the relay inbox and deletes the row.
// 行が無ければ nil (idempotent)。
//
// Undo 配信が失敗しても行自体は削除する: 行が残ると「削除したはずの relay が
// admin UI から消えない」状態になり、管理者が二進も三進も行かなくなるため。
// 失敗はログで警告するに留める。
func (s *Service) Remove(ctx context.Context, id string) error {
	rel, err := s.repo.FindByID(id)
	if err != nil {
		return nil
	}
	if err := s.sendUndoFollow(ctx, rel); err != nil {
		slog.Warn("relay: undo follow delivery failed; deleting row anyway",
			"relayId", rel.ID, "inbox", rel.Inbox, "err", err)
	}
	if err := s.repo.Delete(id); err != nil {
		return fmt.Errorf("relay: delete row: %w", err)
	}
	s.invalidateCache()
	return nil
}

// List returns every relay row (regardless of status).
func (s *Service) List(ctx context.Context) ([]*model.Relay, error) {
	return s.repo.List()
}

// FindByID returns the relay row with the given id.
//
// inbox の Accept / Reject ハンドラが、status を書き換える前に「送信元 actor が
// その relay 自身か」を検証するために使う (federation.RelayStatusMarker)。
func (s *Service) FindByID(ctx context.Context, id string) (*model.Relay, error) {
	return s.repo.FindByID(id)
}

// MarkAccepted flips the status of the given relay to "accepted". Used
// by the inbox Accept handler when it detects a follow-relay activity
// whose sender is that relay. Only a relay that is still "requesting" is
// updated; a late Accept for an accepted / rejected relay is ignored.
func (s *Service) MarkAccepted(ctx context.Context, id string) error {
	return s.markFromRequesting(id, StatusAccepted)
}

// MarkRejected flips the status to "rejected", under the same
// "requesting only" condition as MarkAccepted.
func (s *Service) MarkRejected(ctx context.Context, id string) error {
	return s.markFromRequesting(id, StatusRejected)
}

// markFromRequesting は本家 RelayService.updateRequestingRelayStatus
// (2026.10.0) に揃え、status が requesting の行だけを書き換える。条件が
// 無いと、relay (または relay を名乗れる相手) が後から Accept / Reject を
// 送るだけで accepted と rejected を何度でも行き来させられる。管理者が
// 登録し直さない限り、一度決まった応答は動かさない。
func (s *Service) markFromRequesting(id, to string) error {
	changed, err := s.repo.UpdateStatusFrom(id, StatusRequesting, to)
	if err != nil {
		return err
	}
	if !changed {
		slog.Info("relay: ignoring status change for relay not in requesting state",
			"relayId", id, "to", to)
		return nil
	}
	s.invalidateCache()
	return nil
}

// DeliverToAccepted enqueues activity to every accepted relay's inbox.
// Signer is the author of the activity (typically a local user posting
// a public note); the relay itself is not used as the signer because
// the relay actor would not be recognised as the activity's author.
//
// activity は JSON にシリアライズ可能な構造体 or map。
func (s *Service) DeliverToAccepted(ctx context.Context, signerUserID string, activity any) error {
	if activity == nil {
		return nil
	}
	relays, err := s.acceptedCached()
	if err != nil {
		return err
	}
	if len(relays) == 0 {
		return nil
	}
	body, err := s.marshalForRelay(ctx, signerUserID, activity)
	if err != nil {
		return err
	}
	inboxes := make([]string, 0, len(relays))
	for _, r := range relays {
		inboxes = append(inboxes, r.Inbox)
	}
	return s.deliver.DeliverActivity(signerUserID, body, inboxes)
}

// marshalForRelay serializes activity for relay delivery, attaching an
// RsaSignature2017 LD-Signature authored by signerUserID when LD signing is
// wired (upstream deliverToRelays → attachLdSignature, #1870)。relay は
// downstream subscriber へ転送する際に元 activity の HTTP signature を保持
// できないため、LD-Signature が無いと downstream (Mastodon relay / 署名検証する
// Misskey 等) が relay 経由 note の真正性を検証できず reject する。
//
// 署名は best-effort: keypair 解決や sign に失敗したら警告ログを残して unsigned で
// 配送する (= 従来挙動。署名できないことを理由に relay 配送自体を止めない)。
func (s *Service) marshalForRelay(ctx context.Context, signerUserID string, activity any) ([]byte, error) {
	raw, err := json.Marshal(activity)
	if err != nil {
		return nil, fmt.Errorf("relay: marshal activity: %w", err)
	}
	// LD 署名が未配線なら従来どおりそのまま配送。
	if s.ldSigner == nil || s.keypairRepo == nil || s.baseURL == "" {
		return raw, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		// object でない activity (理論上発生しない) は署名対象外。
		return raw, nil
	}
	// upstream: to が無ければ Public を補完してから署名する (空配列は尊重)。
	if v, ok := m["to"]; !ok || v == nil {
		m["to"] = []any{activitypub.Public}
	}
	kp, kerr := s.keypairRepo.FindByUserID(signerUserID)
	if kerr != nil || kp == nil {
		slog.WarnContext(ctx, "relay: LD signing skipped (keypair unavailable)",
			"signerUserId", signerUserID, "err", kerr)
		return json.Marshal(m)
	}
	creator := s.baseURL + "/users/" + signerUserID + "#main-key"
	signed, serr := s.ldSigner.SignRsaSignature2017(m, kp.PrivateKey, creator, s.clock())
	if serr != nil {
		slog.WarnContext(ctx, "relay: LD signing failed, delivering unsigned",
			"signerUserId", signerUserID, "err", serr)
		return json.Marshal(m)
	}
	return json.Marshal(signed)
}

// sendFollow renders a Follow-Relay activity and enqueues it, signed by
// the relay system actor.
func (s *Service) sendFollow(ctx context.Context, rel *model.Relay) error {
	relayActor, err := s.sysAcct.Fetch("relay")
	if err != nil {
		return fmt.Errorf("relay: fetch relay actor: %w", err)
	}
	follow := s.renderer.RenderFollowRelay(rel.ID, relayActor.ID)
	body, err := json.Marshal(follow)
	if err != nil {
		return fmt.Errorf("relay: marshal follow: %w", err)
	}
	return s.deliver.DeliverActivity(relayActor.ID, body, []string{rel.Inbox})
}

// sendUndoFollow emits Undo(Follow) for the relay. The Follow is
// re-rendered from the stored relay id (the id format is stable) so
// callers do not need to have kept the original Follow object.
func (s *Service) sendUndoFollow(ctx context.Context, rel *model.Relay) error {
	relayActor, err := s.sysAcct.Fetch("relay")
	if err != nil {
		return fmt.Errorf("relay: fetch relay actor: %w", err)
	}
	follow := s.renderer.RenderFollowRelay(rel.ID, relayActor.ID)
	undo := map[string]any{
		"@context": follow.Context,
		"id":       follow.ID + "/undo",
		"type":     "Undo",
		"actor":    follow.Actor,
		"object":   follow,
	}
	body, err := json.Marshal(undo)
	if err != nil {
		return fmt.Errorf("relay: marshal undo: %w", err)
	}
	return s.deliver.DeliverActivity(relayActor.ID, body, []string{rel.Inbox})
}

// acceptedCached returns the cached list of accepted relays, refreshing
// from the DB when the cache is stale or empty.
func (s *Service) acceptedCached() ([]*model.Relay, error) {
	s.cacheMu.RLock()
	if s.cacheRelays != nil && s.clock().Sub(s.cacheLoaded) < s.cacheMaxAge {
		out := s.cacheRelays
		s.cacheMu.RUnlock()
		return out, nil
	}
	s.cacheMu.RUnlock()

	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	// 再チェック (他の goroutine が先に埋めたかも)
	if s.cacheRelays != nil && s.clock().Sub(s.cacheLoaded) < s.cacheMaxAge {
		return s.cacheRelays, nil
	}
	fresh, err := s.repo.ListByStatus(StatusAccepted)
	if err != nil {
		return nil, err
	}
	s.cacheRelays = fresh
	s.cacheLoaded = s.clock()
	return fresh, nil
}

func (s *Service) invalidateCache() {
	s.cacheMu.Lock()
	s.cacheRelays = nil
	s.cacheMu.Unlock()
}

// IsRelayActor reports whether the given remote user matches one of the
// accepted registered relays. Used by federation/processor.handleAnnounce
// (upstream Misskey #17308 / triage #1002) to detect relay-delivered Announce
// activities and avoid wrapping them in a local renote.
//
// 比較は actor.Inbox / actor.SharedInbox を accepted 状態の relay.Inbox と
// 突き合わせる (upstream `isRelayActor` 互換)。nil user / inbox 不在 / cache
// 取得失敗の場合は false を返す (= relay 由来ではない扱い、従来の renote path
// に流す)。
func (s *Service) IsRelayActor(actor *model.User) bool {
	if actor == nil {
		return false
	}
	if actor.Inbox == nil && actor.SharedInbox == nil {
		return false
	}
	relays, err := s.acceptedCached()
	if err != nil {
		slog.Warn("relay: failed to fetch accepted relays for IsRelayActor", "err", err)
		return false
	}
	for _, r := range relays {
		if r.Inbox == "" {
			continue
		}
		if actor.Inbox != nil && r.Inbox == *actor.Inbox {
			return true
		}
		if actor.SharedInbox != nil && r.Inbox == *actor.SharedInbox {
			return true
		}
	}
	return false
}
