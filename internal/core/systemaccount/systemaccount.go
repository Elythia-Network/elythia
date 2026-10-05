// Package systemaccount manages built-in local users that represent
// subsystems of the instance itself (relay actor, instance actor,
// proxy fetcher etc). 本家 Misskey の SystemAccountService 相当。
//
// 各 kind に 1 つずつ user + user_profile + user_keypair + system_account
// 行を作成・取得する。初回 Fetch 時に行が無ければ lazy に作成する。
package systemaccount

import (
	"errors"
	"fmt"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"gorm.io/gorm"
)

// Service fetches (or lazily creates) the built-in system users used
// for subsystem-to-AP interactions. 各 kind は一意なので 1 行のみ。
type Service struct {
	userRepo         repository.UserRepository
	keypairRepo      repository.UserKeypairRepository
	keypairExtraRepo repository.UserKeypairExtraRepository
	saRepo           repository.SystemAccountRepository
	idGen            id.Generator
	clock            func() time.Time
}

// NewService constructs a Service.
func NewService(
	userRepo repository.UserRepository,
	keypairRepo repository.UserKeypairRepository,
	saRepo repository.SystemAccountRepository,
	idGen id.Generator,
) *Service {
	return &Service{
		userRepo:    userRepo,
		keypairRepo: keypairRepo,
		saRepo:      saRepo,
		idGen:       idGen,
		clock:       time.Now,
	}
}

// SetClock replaces the clock, primarily for tests.
func (s *Service) SetClock(now func() time.Time) {
	if now != nil {
		s.clock = now
	}
}

// SetKeypairExtraRepo wires the Ed25519 keypair repository. When set, system
// account creation also generates an Ed25519 keypair so the actor JSON can
// publish it via assertionMethod[] (FEP-521a Multikey). Optional: 未配線でも
// RSA only で動く。
func (s *Service) SetKeypairExtraRepo(r repository.UserKeypairExtraRepository) {
	s.keypairExtraRepo = r
}

// Fetch returns the system user for the given kind, creating it if
// necessary. kind は TS Misskey 本家と同じ識別子 ("relay", "actor",
// "proxy") を想定する。
func (s *Service) Fetch(kind string) (*model.User, error) {
	if kind == "" {
		return nil, errors.New("systemaccount: kind is required")
	}
	sa, err := s.saRepo.FindByType(kind)
	if err == nil {
		return s.userRepo.FindByID(sa.UserID)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	// 前回の create が途中で失敗して user 行だけ残っているかもしれない
	// ("relay.actor" 等の usernameLower は unique WHERE host IS NULL 制約)。
	// 素朴に再 create すると unique violation で永続的に復旧不能になるため、
	// 既存 user を検出できたら再利用して欠けた行だけ埋めに行く。
	username := kind + ".actor"
	if orphan, oerr := s.userRepo.FindByUsernameLower(username, nil); oerr == nil && orphan != nil {
		return s.completeFromOrphan(kind, orphan)
	}
	return s.create(kind)
}

// create materialises a new system user. username は kind ごとに決め打ち
// ("relay.actor" / "instance.actor" / "proxy.actor") で、本家 Misskey の
// SystemAccountService.createCorrespondingUser と同じ形式を採用する。
func (s *Service) create(kind string) (*model.User, error) {
	now := s.clock()
	username := kind + ".actor"
	user := &model.User{
		ID:            s.idGen.Generate(now),
		Username:      username,
		UsernameLower: username,
		Host:          nil,
		IsExplorable:  false,
		IsBot:         true,
		IsLocked:      true,
	}
	if err := s.userRepo.Create(user); err != nil {
		return nil, fmt.Errorf("systemaccount: create user: %w", err)
	}
	return s.completeFromOrphan(kind, user)
}

// completeFromOrphan fills in whatever follow-up rows are missing for an
// already-existing user (either a freshly inserted one from create, or
// an orphan left over from a previous partial failure). Each secondary
// insert is idempotent: a pre-existing row is treated as success so a
// second Fetch after a keypair-insert-succeeded-but-system_account-failed
// crash completes on the second try.
func (s *Service) completeFromOrphan(kind string, user *model.User) (*model.User, error) {
	if _, err := s.userRepo.FindProfileByUserID(user.ID); err != nil {
		profile := &model.UserProfile{UserID: user.ID}
		if perr := s.userRepo.CreateProfile(profile); perr != nil {
			return nil, fmt.Errorf("systemaccount: create profile: %w", perr)
		}
	}
	if _, err := s.keypairRepo.FindByUserID(user.ID); err != nil {
		privPEM, pubPEM, kerr := activitypub.GenerateRSAKeypair()
		if kerr != nil {
			return nil, fmt.Errorf("systemaccount: generate keypair: %w", kerr)
		}
		if kerr := s.keypairRepo.Create(&model.UserKeypair{
			UserID:     user.ID,
			PublicKey:  pubPEM,
			PrivateKey: privPEM,
		}); kerr != nil {
			return nil, fmt.Errorf("systemaccount: create keypair: %w", kerr)
		}
	}
	// Ed25519 keypair も idempotent に発行・保存 (#1067 / #1068)。
	// keypairExtraRepo 未配線時は何もしない (= RSA only fallback)。
	if s.keypairExtraRepo != nil {
		if _, err := s.keypairExtraRepo.FindByUserID(user.ID); err != nil {
			privPEM, pubPEM, kerr := activitypub.GenerateEd25519Keypair()
			if kerr != nil {
				return nil, fmt.Errorf("systemaccount: generate ed25519 keypair: %w", kerr)
			}
			if kerr := s.keypairExtraRepo.Upsert(&model.UserKeypairExtra{
				UserID:            user.ID,
				Ed25519PublicKey:  pubPEM,
				Ed25519PrivateKey: privPEM,
			}); kerr != nil {
				return nil, fmt.Errorf("systemaccount: create ed25519 keypair: %w", kerr)
			}
		}
	}
	if err := s.saRepo.Create(&model.SystemAccount{
		ID:     s.idGen.Generate(s.clock()),
		UserID: user.ID,
		Type:   kind,
	}); err != nil {
		return nil, fmt.Errorf("systemaccount: create system_account: %w", err)
	}
	return user, nil
}
