package testutil

import (
	"sync"

	"github.com/elythia-network/elythia/internal/model"
)

// MockUserKeypairRepository is a test double for repository.UserKeypairRepository.
type MockUserKeypairRepository struct {
	Keypairs map[string]*model.UserKeypair // keyed by userID
}

func NewMockUserKeypairRepository() *MockUserKeypairRepository {
	return &MockUserKeypairRepository{Keypairs: make(map[string]*model.UserKeypair)}
}

func (m *MockUserKeypairRepository) Create(k *model.UserKeypair) error {
	m.Keypairs[k.UserID] = k
	return nil
}

func (m *MockUserKeypairRepository) FindByUserID(userID string) (*model.UserKeypair, error) {
	k, ok := m.Keypairs[userID]
	if !ok {
		return nil, ErrNotFound
	}
	return k, nil
}

// NormalizePrivateKeysToPKCS8 は mock では no-op (#2378)。
func (m *MockUserKeypairRepository) NormalizePrivateKeysToPKCS8() (int, error) {
	return 0, nil
}

// MockUserKeypairExtraRepository is a test double for
// repository.UserKeypairExtraRepository.
//
// `keypairs` map は concurrent backfill (= TestUser_LazyBackfill_RaceSafe
// のような 8 goroutine 並列で InsertIfAbsent + FindByUserID を呼ぶシナリオ)
// で race detector に検出されるため、全 method を mu で保護する (#1091)。
// field を unexport にして外部からの直接 read を実装で塞ぎ、必ず
// `Get(userID)` 経由で読み出させる。
type MockUserKeypairExtraRepository struct {
	mu       sync.RWMutex
	keypairs map[string]*model.UserKeypairExtra // keyed by userID
}

func NewMockUserKeypairExtraRepository() *MockUserKeypairExtraRepository {
	return &MockUserKeypairExtraRepository{keypairs: make(map[string]*model.UserKeypairExtra)}
}

func (m *MockUserKeypairExtraRepository) Upsert(k *model.UserKeypairExtra) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keypairs[k.UserID] = k
	return nil
}

// InsertIfAbsent inserts only when no entry exists for the userId. Returns
// (inserted, nil) where inserted=true if a new row was inserted, false if a
// row already existed (= ON CONFLICT DO NOTHING semantic of the production
// repository).
func (m *MockUserKeypairExtraRepository) InsertIfAbsent(k *model.UserKeypairExtra) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.keypairs[k.UserID]; exists {
		return false, nil
	}
	m.keypairs[k.UserID] = k
	return true, nil
}

func (m *MockUserKeypairExtraRepository) FindByUserID(userID string) (*model.UserKeypairExtra, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.keypairs[userID]
	if !ok {
		return nil, ErrNotFound
	}
	return k, nil
}

func (m *MockUserKeypairExtraRepository) Delete(userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.keypairs, userID)
	return nil
}

// Get は test assertion 用の helper。production interface の FindByUserID は
// error 返却で "not found vs DB error" を区別するが、test では存在の有無
// だけ確認したいケースが多いため `(value, ok)` 形式で expose する。内部
// map は unexport なので、外部からの read は必ず本 helper を通る。
func (m *MockUserKeypairExtraRepository) Get(userID string) (*model.UserKeypairExtra, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	k, ok := m.keypairs[userID]
	return k, ok
}

// MockUserPendingRepository is a test double for repository.UserPendingRepository.
type MockUserPendingRepository struct {
	Rows map[string]*model.UserPending // keyed by ID
}

// NewMockUserPendingRepository creates an empty MockUserPendingRepository.
func NewMockUserPendingRepository() *MockUserPendingRepository {
	return &MockUserPendingRepository{Rows: make(map[string]*model.UserPending)}
}

func (m *MockUserPendingRepository) Create(p *model.UserPending) error {
	m.Rows[p.ID] = p
	return nil
}

// DeleteOlderThan removes rows whose id sorts before thresholdID.
//
// **実データに合わせて件数を返す。** 常に 0 を返すと、掃除を検査するテストが
// 空虚になる (#3037)。
func (m *MockUserPendingRepository) DeleteOlderThan(thresholdID string) (int64, error) {
	var n int64
	for id := range m.Rows {
		if id < thresholdID {
			delete(m.Rows, id)
			n++
		}
	}
	return n, nil
}

func (m *MockUserPendingRepository) FindByCode(code string) (*model.UserPending, error) {
	for _, r := range m.Rows {
		if r.Code == code {
			return r, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockUserPendingRepository) Delete(id string) error {
	delete(m.Rows, id)
	return nil
}

// MockUserMemoRepository is a test double for repository.UserMemoRepository.
type MockUserMemoRepository struct {
	// keyed by "userID:targetUserID"
	Memos map[string]*model.UserMemo
}

// NewMockUserMemoRepository creates an empty MockUserMemoRepository.
func NewMockUserMemoRepository() *MockUserMemoRepository {
	return &MockUserMemoRepository{Memos: make(map[string]*model.UserMemo)}
}

func (m *MockUserMemoRepository) CreateOrUpdate(memo *model.UserMemo) error {
	m.Memos[memo.UserID+":"+memo.TargetUserID] = memo
	return nil
}

func (m *MockUserMemoRepository) FindByPair(userID, targetUserID string) (*model.UserMemo, error) {
	if memo, ok := m.Memos[userID+":"+targetUserID]; ok {
		return memo, nil
	}
	return nil, ErrNotFound
}

func (m *MockUserMemoRepository) Delete(userID, targetUserID string) error {
	delete(m.Memos, userID+":"+targetUserID)
	return nil
}

// MockUserPublickeyRepository is a test double for federation.PublickeyStore.
type MockUserPublickeyRepository struct {
	// keyed by userID
	Keys map[string]*model.UserPublickey
	// FindByKeyIDErr forces FindByKeyID to fail with a non-not-found error,
	// for exercising the paths that must retry instead of dropping (#3121).
	FindByKeyIDErr error
}

// NewMockUserPublickeyRepository creates an empty MockUserPublickeyRepository.
func NewMockUserPublickeyRepository() *MockUserPublickeyRepository {
	return &MockUserPublickeyRepository{Keys: make(map[string]*model.UserPublickey)}
}

func (m *MockUserPublickeyRepository) Upsert(pk *model.UserPublickey) error {
	m.Keys[pk.UserID] = pk
	return nil
}

func (m *MockUserPublickeyRepository) FindByUserID(userID string) (*model.UserPublickey, error) {
	if pk, ok := m.Keys[userID]; ok {
		return pk, nil
	}
	return nil, ErrNotFound
}

// FindByKeyID mirrors repository.userPublickeyRepository.FindByKeyID。
// LD-Signature verify は signature.creator (= keyId) で lookup するため、
// userID baseの map を線形 search する (= mock なのでコスト OK)。
func (m *MockUserPublickeyRepository) FindByKeyID(keyID string) (*model.UserPublickey, error) {
	if m.FindByKeyIDErr != nil {
		return nil, m.FindByKeyIDErr
	}
	for _, pk := range m.Keys {
		if pk.KeyID == keyID {
			return pk, nil
		}
	}
	return nil, ErrNotFound
}

// Delete removes the public key for userID. interface 互換のため実装。
func (m *MockUserPublickeyRepository) Delete(userID string) error {
	delete(m.Keys, userID)
	return nil
}

// MockSystemAccountRepository is a test double for repository.SystemAccountRepository.
type MockSystemAccountRepository struct {
	Accounts map[string]*model.SystemAccount // keyed by type
}

func NewMockSystemAccountRepository() *MockSystemAccountRepository {
	return &MockSystemAccountRepository{Accounts: make(map[string]*model.SystemAccount)}
}

func (m *MockSystemAccountRepository) FindByType(typ string) (*model.SystemAccount, error) {
	if sa, ok := m.Accounts[typ]; ok {
		return sa, nil
	}
	return nil, ErrNotFound
}

func (m *MockSystemAccountRepository) Create(sa *model.SystemAccount) error {
	m.Accounts[sa.Type] = sa
	return nil
}

// MockUsedUsernameRepository is a test double for repository.UsedUsernameRepository.
type MockUsedUsernameRepository struct {
	Usernames map[string]bool
}

func NewMockUsedUsernameRepository() *MockUsedUsernameRepository {
	return &MockUsedUsernameRepository{Usernames: make(map[string]bool)}
}

func (m *MockUsedUsernameRepository) Create(username string) error {
	m.Usernames[username] = true
	return nil
}

func (m *MockUsedUsernameRepository) Exists(username string) (bool, error) {
	return m.Usernames[username], nil
}
