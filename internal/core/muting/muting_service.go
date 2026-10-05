// Package muting provides UserMutingService and RenoteMutingService.
package muting

import (
	"context"
	"errors"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// Errors returned by Service / RenoteService.
var (
	// ErrSelfMute is returned when a user attempts to mute themselves.
	ErrSelfMute = errors.New("cannot mute yourself")
	// ErrMuteeNotFound is returned when the target user does not exist.
	ErrMuteeNotFound = errors.New("mutee not found")
	// ErrAlreadyMuting is returned when the muter already mutes the mutee.
	ErrAlreadyMuting = errors.New("already muting")
	// ErrNotMuting is returned when there is no muting relationship to delete.
	ErrNotMuting = errors.New("not muting")
)

// Service manages user-level mutes (full mute, not renote-only).
type Service struct {
	userRepo   repository.UserRepository
	mutingRepo repository.MutingRepository
	idGen      id.Generator
	// userMaterializer はリレーでしか観測していない相手を DB へ昇格させる
	// (#2332)。muting.muteeId / blocking.blockeeId が user への外部キー。
	userMaterializer UserMaterializer
	// relationReload は mute 変更を streaming connection へ通知する (#2400)。
	relationReload RelationReloadPublisher
}

// NewService constructs a UserMutingService.
func NewService(
	userRepo repository.UserRepository,
	mutingRepo repository.MutingRepository,
	idGen id.Generator,
) *Service {
	return &Service{userRepo: userRepo, mutingRepo: mutingRepo, idGen: idGen}
}

// RelationReloadPublisher notifies streaming connections that a viewer's
// relation snapshot changed (#2400). 実装は stream 側の adapter。
//
// mute は接続確立時の snapshot にしか反映されないため、これが無いと mute した
// 直後も既存の WebSocket に対象 user の event が届き続ける (再接続するまで)。
type RelationReloadPublisher interface {
	PublishMuteBlockReload(userID string)
}

// SetRelationReloadPublisher wires the streaming reload publisher. 未配線なら
// 通知しない (= 従来どおり再接続まで stale)。
func (s *Service) SetRelationReloadPublisher(p RelationReloadPublisher) {
	s.relationReload = p
}

// publishReload notifies the muter's streaming connections. best-effort。
func (s *Service) publishReload(muterID string) {
	if s.relationReload == nil || muterID == "" {
		return
	}
	s.relationReload.PublishMuteBlockReload(muterID)
}

// Mute creates a muting relationship. expiresAt may be nil for indefinite mutes.
func (s *Service) Mute(muterID, muteeID string, expiresAt *time.Time) (*model.Muting, error) {
	if muterID == muteeID {
		return nil, ErrSelfMute
	}
	_, uerr := s.userRepo.FindByID(muteeID)
	// **materialize の前に分ける** (#2799)。not-found でない error は
	// materialize しても直らないのに、EnsureUser / EnsureNote が ephemeral
	// store の引きを 1 回余分に走らせる。ヒットすればそのまま WebFinger +
	// actor fetch まで進むので、DB 断中は 1 リクエストごとに outbound HTTP が
	// 出うる。
	if uerr != nil && !repository.IsNotFound(uerr) {
		return nil, uerr
	}
	if s.materializeUserIfMissing(muteeID, uerr) {
		_, uerr = s.userRepo.FindByID(muteeID)
	}
	if uerr != nil {
		// **DB 障害を not-found に丸めない** (#2799)。materialize の再試行後も
		// 種別は残るので、ここで分ける。
		if !repository.IsNotFound(uerr) {
			return nil, uerr
		}
		return nil, ErrMuteeNotFound
	}

	if exists, err := s.mutingRepo.Exists(muterID, muteeID); err != nil {
		return nil, err
	} else if exists {
		return nil, ErrAlreadyMuting
	}

	// 既に失効している expiresAt (<= now) の mute は no-op で成功扱いにする
	// (upstream mute/create.ts の `if (ps.expiresAt && ps.expiresAt <= Date.now())
	// return;`、#1557)。Muting row を作らないことで対象を re-muteable のまま残す。
	// self / not-found / already-muting の検証より後 (= upstream の順序) に置く。
	if expiresAt != nil && !expiresAt.After(time.Now()) {
		return nil, nil
	}

	rec := &model.Muting{
		ID:        s.idGen.Generate(time.Now()),
		MuterID:   muterID,
		MuteeID:   muteeID,
		ExpiresAt: expiresAt,
	}
	if err := s.mutingRepo.Create(rec); err != nil {
		return nil, err
	}
	// mute した本人 (muter) の snapshot が変わる。mutee 側は変わらない。
	s.publishReload(muterID)
	return rec, nil
}

// Unmute removes the muting relationship.
func (s *Service) Unmute(muterID, muteeID string) error {
	if muterID == muteeID {
		return ErrSelfMute
	}
	rec, err := s.mutingRepo.FindByPair(muterID, muteeID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrNotMuting
	}
	if err := s.mutingRepo.Delete(rec); err != nil {
		return err
	}
	s.publishReload(muterID)
	return nil
}

// IsMuted reports whether muter currently mutes mutee.
func (s *Service) IsMuted(muterID, muteeID string) (bool, error) {
	return s.mutingRepo.Exists(muterID, muteeID)
}

// List returns the user's mutings with cursor (sinceID/untilID) or offset
// pagination. Cursor 指定時は offset 無視 (upstream makePaginationQuery と一致)。
func (s *Service) List(muterID, sinceID, untilID string, limit, offset int) ([]*model.Muting, error) {
	if limit <= 0 {
		limit = 10
	}
	return s.mutingRepo.ListByMuter(muterID, sinceID, untilID, limit, offset)
}

// RenoteService manages renote-only mutes (the mutee can still post original
// notes, but their renotes are hidden from the muter's timelines).
type RenoteService struct {
	userRepo         repository.UserRepository
	renoteMutingRepo repository.RenoteMutingRepository
	idGen            id.Generator
	// relationReload は renote-mute 変更を streaming connection へ通知する (#2400)。
	relationReload RelationReloadPublisher
}

// NewRenoteService constructs a RenoteMutingService.
func NewRenoteService(
	userRepo repository.UserRepository,
	renoteMutingRepo repository.RenoteMutingRepository,
	idGen id.Generator,
) *RenoteService {
	return &RenoteService{userRepo: userRepo, renoteMutingRepo: renoteMutingRepo, idGen: idGen}
}

// Mute creates a renote-mute relationship.
// SetRelationReloadPublisher wires the streaming reload publisher for renote
// mutes (#2400). 未配線なら通知しない。
func (s *RenoteService) SetRelationReloadPublisher(p RelationReloadPublisher) {
	s.relationReload = p
}

func (s *RenoteService) publishReload(muterID string) {
	if s.relationReload == nil || muterID == "" {
		return
	}
	s.relationReload.PublishMuteBlockReload(muterID)
}

func (s *RenoteService) Mute(muterID, muteeID string) (*model.RenoteMuting, error) {
	if muterID == muteeID {
		return nil, ErrSelfMute
	}
	if _, err := s.userRepo.FindByID(muteeID); err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrMuteeNotFound
	}
	if exists, err := s.renoteMutingRepo.Exists(muterID, muteeID); err != nil {
		return nil, err
	} else if exists {
		return nil, ErrAlreadyMuting
	}

	rec := &model.RenoteMuting{
		ID:      s.idGen.Generate(time.Now()),
		MuterID: muterID,
		MuteeID: muteeID,
	}
	if err := s.renoteMutingRepo.Create(rec); err != nil {
		return nil, err
	}
	s.publishReload(muterID)
	return rec, nil
}

// Unmute removes the renote mute.
func (s *RenoteService) Unmute(muterID, muteeID string) error {
	if muterID == muteeID {
		return ErrSelfMute
	}
	rec, err := s.renoteMutingRepo.FindByPair(muterID, muteeID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrNotMuting
	}
	if err := s.renoteMutingRepo.Delete(rec); err != nil {
		return err
	}
	s.publishReload(muterID)
	return nil
}

// IsRenoteMuted reports whether muter has renote-muted mutee.
func (s *RenoteService) IsRenoteMuted(muterID, muteeID string) (bool, error) {
	return s.renoteMutingRepo.Exists(muterID, muteeID)
}

// List returns the user's renote mutings with cursor (sinceID/untilID) or
// offset pagination.
func (s *RenoteService) List(muterID, sinceID, untilID string, limit, offset int) ([]*model.RenoteMuting, error) {
	if limit <= 0 {
		limit = 10
	}
	return s.renoteMutingRepo.ListByMuter(muterID, sinceID, untilID, limit, offset)
}

// UserMaterializer promotes a relay-only author out of the ephemeral store
// into a real database row (#2332)。実装は core/ephemeral.Materializer。
//
// ミュート / ブロックは user への外部キーだけを要求し、ノート行は要らない。
// リレーでしか観測していない相手をミュートしようとしたときに、対象が DB に
// 居ないと登録できないため。
type UserMaterializer interface {
	EnsureUser(ctx context.Context, userID string) (*model.User, error)
}

// SetUserMaterializer attaches the ephemeral-author materializer. Optional.
func (s *Service) SetUserMaterializer(m UserMaterializer) {
	s.userMaterializer = m
}

// materializeUserIfMissing promotes an ephemeral author only when the database
// lookup already failed.
func (s *Service) materializeUserIfMissing(userID string, lookupErr error) bool {
	if lookupErr == nil || s.userMaterializer == nil {
		return false
	}
	_, err := s.userMaterializer.EnsureUser(context.Background(), userID)
	return err == nil
}
