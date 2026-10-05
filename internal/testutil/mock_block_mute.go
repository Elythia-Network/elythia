package testutil

import (
	"sort"
	"time"

	"github.com/elythia-network/elythia/internal/model"
)

// MockBlockingRepository is a test double for repository.BlockingRepository.
type MockBlockingRepository struct {
	Blockings map[string]*model.Blocking // keyed by ID
	// ExistsErr, when set, is returned by Exists to exercise fail-closed paths.
	ExistsErr error
}

func NewMockBlockingRepository() *MockBlockingRepository {
	return &MockBlockingRepository{Blockings: make(map[string]*model.Blocking)}
}

func (m *MockBlockingRepository) Create(b *model.Blocking) error {
	m.Blockings[b.ID] = b
	return nil
}

func (m *MockBlockingRepository) Delete(b *model.Blocking) error {
	delete(m.Blockings, b.ID)
	return nil
}

func (m *MockBlockingRepository) FindByPair(blockerID, blockeeID string) (*model.Blocking, error) {
	for _, b := range m.Blockings {
		if b.BlockerID == blockerID && b.BlockeeID == blockeeID {
			return b, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockBlockingRepository) Exists(blockerID, blockeeID string) (bool, error) {
	if m.ExistsErr != nil {
		return false, m.ExistsErr
	}
	_, err := m.FindByPair(blockerID, blockeeID)
	return err == nil, nil
}

func (m *MockBlockingRepository) ListByBlocker(blockerID, sinceID, untilID string, limit, offset int) ([]*model.Blocking, error) {
	var rows []*model.Blocking
	for _, b := range m.Blockings {
		if b.BlockerID != blockerID {
			continue
		}
		if sinceID != "" && b.ID <= sinceID {
			continue
		}
		if untilID != "" && b.ID >= untilID {
			continue
		}
		rows = append(rows, b)
	}
	return paginateBlockings(rows, sinceID, untilID, limit, offset), nil
}

// ListBlockerIDs returns blockerIDs of every block row whose blockeeId equals
// the given user. Mirrors the production blocking repo used by note-list
// endpoints to drop notes from users who blocked the viewer (#1544). ExistsErr
// is reused to exercise the fail-closed path on repository error.
func (m *MockBlockingRepository) ListBlockerIDs(blockeeID string) ([]string, error) {
	if blockeeID == "" {
		return nil, nil
	}
	if m.ExistsErr != nil {
		return nil, m.ExistsErr
	}
	var ids []string
	for _, b := range m.Blockings {
		if b.BlockeeID == blockeeID {
			ids = append(ids, b.BlockerID)
		}
	}
	return ids, nil
}

// ListBlockeeIDs returns blockeeIDs of every block row whose blockerId equals
// blockerID (#2106 N3).
func (m *MockBlockingRepository) ListBlockeeIDs(blockerID string) ([]string, error) {
	if blockerID == "" {
		return nil, nil
	}
	if m.ExistsErr != nil {
		return nil, m.ExistsErr
	}
	var ids []string
	for _, b := range m.Blockings {
		if b.BlockerID == blockerID {
			ids = append(ids, b.BlockeeID)
		}
	}
	return ids, nil
}

// MockMutingRepository is a test double for repository.MutingRepository.
type MockMutingRepository struct {
	Mutings map[string]*model.Muting
}

func NewMockMutingRepository() *MockMutingRepository {
	return &MockMutingRepository{Mutings: make(map[string]*model.Muting)}
}

func (m *MockMutingRepository) Create(rec *model.Muting) error {
	m.Mutings[rec.ID] = rec
	return nil
}

func (m *MockMutingRepository) Delete(rec *model.Muting) error {
	delete(m.Mutings, rec.ID)
	return nil
}

func (m *MockMutingRepository) FindByPair(muterID, muteeID string) (*model.Muting, error) {
	for _, r := range m.Mutings {
		if r.MuterID == muterID && r.MuteeID == muteeID {
			return r, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockMutingRepository) Exists(muterID, muteeID string) (bool, error) {
	now := time.Now()
	for _, r := range m.Mutings {
		if r.MuterID == muterID && r.MuteeID == muteeID {
			if r.ExpiresAt != nil && r.ExpiresAt.Before(now) {
				return false, nil
			}
			return true, nil
		}
	}
	return false, nil
}

func (m *MockMutingRepository) ListByMuter(muterID, sinceID, untilID string, limit, offset int) ([]*model.Muting, error) {
	var rows []*model.Muting
	for _, r := range m.Mutings {
		if r.MuterID != muterID {
			continue
		}
		if sinceID != "" && r.ID <= sinceID {
			continue
		}
		if untilID != "" && r.ID >= untilID {
			continue
		}
		rows = append(rows, r)
	}
	return paginateMutings(rows, sinceID, untilID, limit, offset), nil
}

// ListMuteeIDs returns muteeIDs for muterID where the mute is active
// (= ExpiresAt nil or in future). production の挙動と同じ filter logic で
// timeline test の user mute 経路を担保する (#874)。
func (m *MockMutingRepository) ListMuteeIDs(muterID string) ([]string, error) {
	if muterID == "" {
		return nil, nil
	}
	var ids []string
	now := time.Now()
	for _, r := range m.Mutings {
		if r.MuterID != muterID {
			continue
		}
		if r.ExpiresAt != nil && !r.ExpiresAt.After(now) {
			continue
		}
		ids = append(ids, r.MuteeID)
	}
	return ids, nil
}

// ListByMutee returns the active (non-expired) mute rows whose muteeId matches
// (= who mutes this user)。id 昇順で返し、caller が順序に依存できるようにする。
func (m *MockMutingRepository) ListByMutee(muteeID string) ([]*model.Muting, error) {
	if muteeID == "" {
		return nil, nil
	}
	var rows []*model.Muting
	now := time.Now()
	for _, r := range m.Mutings {
		if r.MuteeID != muteeID {
			continue
		}
		if r.ExpiresAt != nil && !r.ExpiresAt.After(now) {
			continue
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows, nil
}

// ListAllMuteeIDs returns every muteeId for muterID (expiry filter なし)。
func (m *MockMutingRepository) ListAllMuteeIDs(muterID string) ([]string, error) {
	if muterID == "" {
		return nil, nil
	}
	var ids []string
	for _, r := range m.Mutings {
		if r.MuterID != muterID {
			continue
		}
		ids = append(ids, r.MuteeID)
	}
	return ids, nil
}

// DeleteExpired removes muting rows whose ExpiresAt has passed. Returns the
// muterId of every removed row (one entry per row, so duplicates are possible),
// matching repository.MutingRepository.
//
// map の走査順は不定なので、返る順序は保証しない。呼び出し側は集合として扱うこと
// (実装も重複排除してから publish する)。
func (m *MockMutingRepository) DeleteExpired(now time.Time) ([]string, error) {
	muterIDs := []string{}
	for k, r := range m.Mutings {
		if r.ExpiresAt != nil && r.ExpiresAt.Before(now) {
			delete(m.Mutings, k)
			muterIDs = append(muterIDs, r.MuterID)
		}
	}
	return muterIDs, nil
}

// MockRenoteMutingRepository is a test double for repository.RenoteMutingRepository.
type MockRenoteMutingRepository struct {
	Mutings map[string]*model.RenoteMuting
}

func NewMockRenoteMutingRepository() *MockRenoteMutingRepository {
	return &MockRenoteMutingRepository{Mutings: make(map[string]*model.RenoteMuting)}
}

func (m *MockRenoteMutingRepository) Create(rec *model.RenoteMuting) error {
	m.Mutings[rec.ID] = rec
	return nil
}

func (m *MockRenoteMutingRepository) Delete(rec *model.RenoteMuting) error {
	delete(m.Mutings, rec.ID)
	return nil
}

func (m *MockRenoteMutingRepository) FindByPair(muterID, muteeID string) (*model.RenoteMuting, error) {
	for _, r := range m.Mutings {
		if r.MuterID == muterID && r.MuteeID == muteeID {
			return r, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockRenoteMutingRepository) Exists(muterID, muteeID string) (bool, error) {
	_, err := m.FindByPair(muterID, muteeID)
	return err == nil, nil
}

// ListMuteeIDs returns muteeIDs of all renote-mute rows for muterID.
// timeline endpoint で renote-mute された user の pure renote を除外する
// filter 用 (#903)。renote_muting には expiresAt が無いので active filter 不要。
func (m *MockRenoteMutingRepository) ListMuteeIDs(muterID string) ([]string, error) {
	if muterID == "" {
		return nil, nil
	}
	var ids []string
	for _, r := range m.Mutings {
		if r.MuterID != muterID {
			continue
		}
		ids = append(ids, r.MuteeID)
	}
	return ids, nil
}

func (m *MockRenoteMutingRepository) ListByMuter(muterID, sinceID, untilID string, limit, offset int) ([]*model.RenoteMuting, error) {
	var rows []*model.RenoteMuting
	for _, r := range m.Mutings {
		if r.MuterID != muterID {
			continue
		}
		if sinceID != "" && r.ID <= sinceID {
			continue
		}
		if untilID != "" && r.ID >= untilID {
			continue
		}
		rows = append(rows, r)
	}
	return paginateRenoteMutings(rows, sinceID, untilID, limit, offset), nil
}

// applyCursorPagination applies the same ordering / limit / cursor-vs-offset
// rules as the real repositories so mock tests exercise upstream
// makePaginationQuery semantics. ASC ordering kicks in when sinceID is set
// without untilID (matches paginationOrder in repository package).
func paginateBlockings(rows []*model.Blocking, sinceID, untilID string, limit, offset int) []*model.Blocking {
	asc := sinceID != "" && untilID == ""
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			less := rows[i].ID < rows[j].ID
			if (asc && !less) || (!asc && less) {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	if limit <= 0 {
		limit = 30
	}
	if sinceID == "" && untilID == "" {
		if offset >= len(rows) {
			return nil
		}
		end := min(offset+limit, len(rows))
		return rows[offset:end]
	}
	if limit > len(rows) {
		limit = len(rows)
	}
	return rows[:limit]
}

func paginateMutings(rows []*model.Muting, sinceID, untilID string, limit, offset int) []*model.Muting {
	asc := sinceID != "" && untilID == ""
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			less := rows[i].ID < rows[j].ID
			if (asc && !less) || (!asc && less) {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	if limit <= 0 {
		limit = 30
	}
	if sinceID == "" && untilID == "" {
		if offset >= len(rows) {
			return nil
		}
		end := min(offset+limit, len(rows))
		return rows[offset:end]
	}
	if limit > len(rows) {
		limit = len(rows)
	}
	return rows[:limit]
}

func paginateRenoteMutings(rows []*model.RenoteMuting, sinceID, untilID string, limit, offset int) []*model.RenoteMuting {
	asc := sinceID != "" && untilID == ""
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			less := rows[i].ID < rows[j].ID
			if (asc && !less) || (!asc && less) {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	if limit <= 0 {
		limit = 30
	}
	if sinceID == "" && untilID == "" {
		if offset >= len(rows) {
			return nil
		}
		end := min(offset+limit, len(rows))
		return rows[offset:end]
	}
	if limit > len(rows) {
		limit = len(rows)
	}
	return rows[:limit]
}
