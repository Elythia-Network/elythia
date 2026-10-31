package testutil

import (
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/model"
)

// MockAntennaRepository is a test double for repository.AntennaRepository.
type MockAntennaRepository struct {
	Antennas  map[string]*model.Antenna
	CreateErr error
	UpdateErr error
	// LastUpdates は UpdateFields に最後に渡された fields map を capture
	// する。caller が model.StringArray 等で正しく wrap しているかを test 側で
	// assert するため (#896 と同 pattern)。
	LastUpdates map[string]any
}

// NewMockAntennaRepository creates an empty MockAntennaRepository.
func NewMockAntennaRepository() *MockAntennaRepository {
	return &MockAntennaRepository{Antennas: make(map[string]*model.Antenna)}
}

func (m *MockAntennaRepository) Create(a *model.Antenna) error {
	if m.CreateErr != nil {
		return m.CreateErr
	}
	m.Antennas[a.ID] = a
	return nil
}

func (m *MockAntennaRepository) FindByID(id string) (*model.Antenna, error) {
	a, ok := m.Antennas[id]
	if !ok {
		return nil, ErrNotFound
	}
	return a, nil
}

func (m *MockAntennaRepository) UpdateFields(antennaID string, fields map[string]any) error {
	m.LastUpdates = fields
	if m.UpdateErr != nil {
		return m.UpdateErr
	}
	if len(fields) == 0 {
		return nil
	}
	a, ok := m.Antennas[antennaID]
	if !ok {
		return ErrNotFound
	}
	applyAntennaFields(a, fields)
	return nil
}

func (m *MockAntennaRepository) Delete(a *model.Antenna) error {
	delete(m.Antennas, a.ID)
	return nil
}

func (m *MockAntennaRepository) ListByUser(userID string) ([]*model.Antenna, error) {
	var rows []*model.Antenna
	for _, a := range m.Antennas {
		if a.UserID == userID {
			rows = append(rows, a)
		}
	}
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if rows[i].ID < rows[j].ID {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	return rows, nil
}

func (m *MockAntennaRepository) CountByUser(userID string) (int64, error) {
	var count int64
	for _, a := range m.Antennas {
		if a.UserID == userID {
			count++
		}
	}
	return count, nil
}

func (m *MockAntennaRepository) ListAllActive() ([]*model.Antenna, error) {
	var rows []*model.Antenna
	for _, a := range m.Antennas {
		if a.IsActive {
			rows = append(rows, a)
		}
	}
	return rows, nil
}

func (m *MockAntennaRepository) DeactivateUnusedSince(cutoff time.Time) (int64, error) {
	var n int64
	for _, a := range m.Antennas {
		if a.IsActive && a.LastUsedAt.Before(cutoff) {
			a.IsActive = false
			n++
		}
	}
	return n, nil
}

func applyAntennaFields(a *model.Antenna, fields map[string]any) {
	for k, v := range fields {
		switch k {
		case "name":
			if s, ok := v.(string); ok {
				a.Name = s
			}
		case "src":
			if s, ok := v.(model.AntennaSource); ok {
				a.Src = s
			}
		case "users":
			if arr, ok := v.([]string); ok {
				a.Users = arr
			}
		case "keywords":
			if b, ok := v.([]byte); ok {
				a.Keywords = b
			}
		case "excludeKeywords":
			if b, ok := v.([]byte); ok {
				a.ExcludeKeywords = b
			}
		case "caseSensitive":
			if b, ok := v.(bool); ok {
				a.CaseSensitive = b
			}
		case "excludeBots":
			if b, ok := v.(bool); ok {
				a.ExcludeBots = b
			}
		case "withReplies":
			if b, ok := v.(bool); ok {
				a.WithReplies = b
			}
		case "withFile":
			if b, ok := v.(bool); ok {
				a.WithFile = b
			}
		case "isActive":
			if b, ok := v.(bool); ok {
				a.IsActive = b
			}
		case "localOnly":
			if b, ok := v.(bool); ok {
				a.LocalOnly = b
			}
		case "excludeNotesInSensitiveChannel":
			if b, ok := v.(bool); ok {
				a.ExcludeNotesInSensitiveChannel = b
			}
		case "lastUsedAt":
			if t, ok := v.(time.Time); ok {
				a.LastUsedAt = t
			}
		}
	}
}

// MockChannelRepository is a test double for repository.ChannelRepository.
type MockChannelRepository struct {
	Channels  map[string]*model.Channel
	CreateErr error
	UpdateErr error
	// PostedAuthors records the authors IncrementUsersCountOnFirstPost has
	// seen per channel. The mock treats the first call for an author as the
	// first post (it has no note table to look at).
	PostedAuthors map[string]map[string]bool
}

// NewMockChannelRepository creates an empty MockChannelRepository.
func NewMockChannelRepository() *MockChannelRepository {
	return &MockChannelRepository{Channels: make(map[string]*model.Channel)}
}

func (m *MockChannelRepository) Create(c *model.Channel) error {
	if m.CreateErr != nil {
		return m.CreateErr
	}
	m.Channels[c.ID] = c
	return nil
}

func (m *MockChannelRepository) FindByID(id string) (*model.Channel, error) {
	c, ok := m.Channels[id]
	if !ok {
		return nil, ErrNotFound
	}
	return c, nil
}

func (m *MockChannelRepository) FindByIDs(ids []string) ([]*model.Channel, error) {
	var result []*model.Channel
	for _, id := range ids {
		if ch, ok := m.Channels[id]; ok {
			result = append(result, ch)
		}
	}
	return result, nil
}

func (m *MockChannelRepository) UpdateFields(channelID string, fields map[string]any) error {
	if m.UpdateErr != nil {
		return m.UpdateErr
	}
	if len(fields) == 0 {
		return nil
	}
	c, ok := m.Channels[channelID]
	if !ok {
		return ErrNotFound
	}
	applyChannelFields(c, fields)
	return nil
}

func (m *MockChannelRepository) IncrementCount(channelID, column string, delta int) error {
	c, ok := m.Channels[channelID]
	if !ok {
		return ErrNotFound
	}
	switch column {
	case "notesCount":
		c.NotesCount += delta
	case "usersCount":
		c.UsersCount += delta
	}
	return nil
}

// IncrementUsersCountOnFirstPost adds 1 to usersCount on the first call for
// (channelID, authorID). 実装は note を見て判定するが、mock は呼び出しの記録で
// 代える。判定そのものは repository の実 DB テストが見ている。
func (m *MockChannelRepository) IncrementUsersCountOnFirstPost(channelID, authorID, _ string) error {
	c, ok := m.Channels[channelID]
	if !ok {
		return nil
	}
	if m.PostedAuthors == nil {
		m.PostedAuthors = make(map[string]map[string]bool)
	}
	if m.PostedAuthors[channelID] == nil {
		m.PostedAuthors[channelID] = make(map[string]bool)
	}
	if m.PostedAuthors[channelID][authorID] {
		return nil
	}
	m.PostedAuthors[channelID][authorID] = true
	c.UsersCount++
	return nil
}

// List returns channels matching the most common filter predicates. テストの
// 安定性のため id 昇順で返す。
func (m *MockChannelRepository) List(filter model.ChannelListFilter) ([]*model.Channel, error) {
	var rows []*model.Channel
	for _, c := range m.Channels {
		if filter.OwnerID != "" {
			if c.UserID == nil || *c.UserID != filter.OwnerID {
				continue
			}
		}
		if filter.Query != "" {
			match := strings.Contains(c.Name, filter.Query)
			// SearchDescription 時は description も対象 (production の ILIKE と整合)。
			if !match && filter.SearchDescription && c.Description != nil {
				match = strings.Contains(*c.Description, filter.Query)
			}
			if !match {
				continue
			}
		}
		if filter.IsArchived != nil && c.IsArchived != *filter.IsArchived {
			continue
		}
		if filter.LastNotedAtNotNull && c.LastNotedAt == nil {
			continue
		}
		rows = append(rows, c)
	}
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if rows[i].ID > rows[j].ID {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	if filter.Offset >= len(rows) {
		return nil, nil
	}
	end := filter.Offset + limit
	if end > len(rows) {
		end = len(rows)
	}
	return rows[filter.Offset:end], nil
}

func applyChannelFields(c *model.Channel, fields map[string]any) {
	for k, v := range fields {
		switch k {
		case "name":
			if s, ok := v.(string); ok {
				c.Name = s
			}
		case "description":
			if s, ok := v.(*string); ok {
				c.Description = s
			}
		case "color":
			if s, ok := v.(string); ok {
				c.Color = s
			}
		case "isArchived":
			if b, ok := v.(bool); ok {
				c.IsArchived = b
			}
		case "isSensitive":
			if b, ok := v.(bool); ok {
				c.IsSensitive = b
			}
		case "allowRenoteToExternal":
			if b, ok := v.(bool); ok {
				c.AllowRenoteToExternal = b
			}
		case "lastNotedAt":
			if t, ok := v.(*time.Time); ok {
				c.LastNotedAt = t
			}
		case "bannerId":
			switch s := v.(type) {
			case string:
				c.BannerID = &s
			case *string:
				c.BannerID = s
			case nil:
				c.BannerID = nil
			}
		case "pinnedNoteIds":
			switch arr := v.(type) {
			case model.StringArray:
				c.PinnedNoteIDs = arr
			case []string:
				c.PinnedNoteIDs = arr
			}
		}
	}
}

// MockChannelFollowingRepository is a test double for the channel_following
// repository.
type MockChannelFollowingRepository struct {
	Followings map[string]*model.ChannelFollowing
}

// NewMockChannelFollowingRepository creates an empty MockChannelFollowingRepository.
func NewMockChannelFollowingRepository() *MockChannelFollowingRepository {
	return &MockChannelFollowingRepository{Followings: make(map[string]*model.ChannelFollowing)}
}

func (m *MockChannelFollowingRepository) Create(f *model.ChannelFollowing) error {
	m.Followings[f.ID] = f
	return nil
}

func (m *MockChannelFollowingRepository) Delete(f *model.ChannelFollowing) error {
	delete(m.Followings, f.ID)
	return nil
}

func (m *MockChannelFollowingRepository) FindByPair(followerID, channelID string) (*model.ChannelFollowing, error) {
	for _, f := range m.Followings {
		if f.FollowerID == followerID && f.FolloweeID == channelID {
			return f, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockChannelFollowingRepository) Exists(followerID, channelID string) (bool, error) {
	_, err := m.FindByPair(followerID, channelID)
	return err == nil, nil
}

func (m *MockChannelFollowingRepository) ExistsMany(followerID string, channelIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(channelIDs))
	if followerID == "" {
		return out, nil
	}
	for _, cid := range channelIDs {
		if ok, _ := m.Exists(followerID, cid); ok {
			out[cid] = true
		}
	}
	return out, nil
}

func (m *MockChannelFollowingRepository) ListFollowed(userID, sinceID, untilID string, limit, offset int) ([]*model.ChannelFollowing, error) {
	var rows []*model.ChannelFollowing
	for _, f := range m.Followings {
		if f.FollowerID != userID {
			continue
		}
		// cursor は followeeId (= channel.id) で絞る (#520 review)。
		if sinceID != "" && f.FolloweeID <= sinceID {
			continue
		}
		if untilID != "" && f.FolloweeID >= untilID {
			continue
		}
		rows = append(rows, f)
	}
	asc := sinceID != "" && untilID == ""
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			less := rows[i].FolloweeID < rows[j].FolloweeID
			if (asc && !less) || (!asc && less) {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	if sinceID == "" && untilID == "" {
		if offset >= len(rows) {
			return nil, nil
		}
		end := offset + limit
		if end > len(rows) {
			end = len(rows)
		}
		return rows[offset:end], nil
	}
	if limit > len(rows) {
		limit = len(rows)
	}
	return rows[:limit], nil
}

// ListFollowerIDsPage returns a page of followerIds for a given channel,
// ordered by row id ASC. Follows the production repository's cursor-based
// pagination contract (#320).
func (m *MockChannelFollowingRepository) ListFollowerIDsPage(channelID, afterRowID string, limit int) ([]string, string, error) {
	// Collect matching rows in insertion order (map iteration is random;
	// sort by ID for deterministic paging).
	type pair struct{ id, follower string }
	var rows []pair
	for _, f := range m.Followings {
		if f.FolloweeID == channelID {
			rows = append(rows, pair{id: f.ID, follower: f.FollowerID})
		}
	}
	// stable sort by id ASC
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if rows[j].id < rows[i].id {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	// Keep entries strictly greater than afterRowID (matches production's
	// `WHERE "id" > ?`; exact-match cursor would diverge if the cursor row
	// has been deleted between pages).
	if afterRowID != "" {
		kept := rows[:0]
		for _, r := range rows {
			if r.id > afterRowID {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	if limit <= 0 {
		limit = 500
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	if len(rows) == 0 {
		return nil, "", nil
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.follower)
	}
	return ids, rows[len(rows)-1].id, nil
}

// MockAntennaNoteUnreadRepository is a test double for
// repository.AntennaNoteUnreadRepository.
type MockAntennaNoteUnreadRepository struct {
	Rows []*model.AntennaNoteUnread
}

// NewMockAntennaNoteUnreadRepository returns an empty repository.
func NewMockAntennaNoteUnreadRepository() *MockAntennaNoteUnreadRepository {
	return &MockAntennaNoteUnreadRepository{}
}

// Create records the unread row.
func (m *MockAntennaNoteUnreadRepository) Create(row *model.AntennaNoteUnread) error {
	m.Rows = append(m.Rows, row)
	return nil
}

// HasAnyByUser returns whether any row exists for the userID.
func (m *MockAntennaNoteUnreadRepository) HasAnyByUser(userID string) (bool, error) {
	for _, r := range m.Rows {
		if r.UserID == userID {
			return true, nil
		}
	}
	return false, nil
}

// DeleteByAntennaUser removes all rows for the (user, antenna) pair.
func (m *MockAntennaNoteUnreadRepository) DeleteByAntennaUser(userID, antennaID string) error {
	out := m.Rows[:0]
	for _, r := range m.Rows {
		if r.UserID == userID && r.AntennaID == antennaID {
			continue
		}
		out = append(out, r)
	}
	m.Rows = out
	return nil
}

// MockChannelNoteUnreadRepository is a test double for
// repository.ChannelNoteUnreadRepository.
type MockChannelNoteUnreadRepository struct {
	Rows []*model.ChannelNoteUnread
}

// NewMockChannelNoteUnreadRepository returns an empty repository.
func NewMockChannelNoteUnreadRepository() *MockChannelNoteUnreadRepository {
	return &MockChannelNoteUnreadRepository{}
}

// Create records the unread row.
func (m *MockChannelNoteUnreadRepository) Create(row *model.ChannelNoteUnread) error {
	m.Rows = append(m.Rows, row)
	return nil
}

// CreateMany appends all rows, silently skipping entries whose
// (UserID, ChannelID, NoteID) triple already exists (mirrors the real
// repository's ON CONFLICT DO NOTHING semantics).
func (m *MockChannelNoteUnreadRepository) CreateMany(rows []*model.ChannelNoteUnread) error {
	for _, r := range rows {
		dup := false
		for _, existing := range m.Rows {
			if existing.UserID == r.UserID && existing.ChannelID == r.ChannelID && existing.NoteID == r.NoteID {
				dup = true
				break
			}
		}
		if !dup {
			m.Rows = append(m.Rows, r)
		}
	}
	return nil
}

// HasAnyByUser returns whether any row exists for the userID.
func (m *MockChannelNoteUnreadRepository) HasAnyByUser(userID string) (bool, error) {
	for _, r := range m.Rows {
		if r.UserID == userID {
			return true, nil
		}
	}
	return false, nil
}

// DeleteByChannelUser removes all rows for the (user, channel) pair.
func (m *MockChannelNoteUnreadRepository) DeleteByChannelUser(userID, channelID string) error {
	out := m.Rows[:0]
	for _, r := range m.Rows {
		if r.UserID == userID && r.ChannelID == channelID {
			continue
		}
		out = append(out, r)
	}
	m.Rows = out
	return nil
}

// MockChannelFavoriteRepository is a test double for repository.ChannelFavoriteRepository.
type MockChannelFavoriteRepository struct {
	Favorites map[string]*model.ChannelFavorite // keyed by "userId:channelId"
}

func NewMockChannelFavoriteRepository() *MockChannelFavoriteRepository {
	return &MockChannelFavoriteRepository{Favorites: make(map[string]*model.ChannelFavorite)}
}

func (m *MockChannelFavoriteRepository) Create(fav *model.ChannelFavorite) error {
	m.Favorites[fav.UserID+":"+fav.ChannelID] = fav
	return nil
}

func (m *MockChannelFavoriteRepository) Delete(userID, channelID string) error {
	delete(m.Favorites, userID+":"+channelID)
	return nil
}

func (m *MockChannelFavoriteRepository) ListByUser(userID string) ([]*model.ChannelFavorite, error) {
	var result []*model.ChannelFavorite
	for _, f := range m.Favorites {
		if f.UserID == userID {
			result = append(result, f)
		}
	}
	return result, nil
}

func (m *MockChannelFavoriteRepository) Exists(userID, channelID string) (bool, error) {
	_, ok := m.Favorites[userID+":"+channelID]
	return ok, nil
}

func (m *MockChannelFavoriteRepository) ExistsMany(userID string, channelIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(channelIDs))
	if userID == "" {
		return out, nil
	}
	for _, cid := range channelIDs {
		if _, ok := m.Favorites[userID+":"+cid]; ok {
			out[cid] = true
		}
	}
	return out, nil
}

// MockChannelMutingRepository is a test double for repository.ChannelMutingRepository.
type MockChannelMutingRepository struct {
	Mutings map[string]*model.ChannelMuting // keyed by "userId:channelId"
}

func NewMockChannelMutingRepository() *MockChannelMutingRepository {
	return &MockChannelMutingRepository{Mutings: make(map[string]*model.ChannelMuting)}
}

func (m *MockChannelMutingRepository) Create(mut *model.ChannelMuting) error {
	m.Mutings[mut.UserID+":"+mut.ChannelID] = mut
	return nil
}

func (m *MockChannelMutingRepository) Delete(userID, channelID string) error {
	delete(m.Mutings, userID+":"+channelID)
	return nil
}

func (m *MockChannelMutingRepository) ListByUser(userID string) ([]*model.ChannelMuting, error) {
	now := time.Now()
	var result []*model.ChannelMuting
	for _, mut := range m.Mutings {
		if mut.UserID == userID && (mut.ExpiresAt == nil || mut.ExpiresAt.After(now)) {
			result = append(result, mut)
		}
	}
	return result, nil
}

func (m *MockChannelMutingRepository) Exists(userID, channelID string) (bool, error) {
	mut, ok := m.Mutings[userID+":"+channelID]
	if !ok {
		return false, nil
	}
	return mut.ExpiresAt == nil || mut.ExpiresAt.After(time.Now()), nil
}

// DeleteExpired removes channel mutes whose ExpiresAt has passed. Returns the
// userId of every removed row (one entry per row, so duplicates are possible),
// matching repository.ChannelMutingRepository. 返る順序は保証しない。
func (m *MockChannelMutingRepository) DeleteExpired(now time.Time) ([]string, error) {
	userIDs := []string{}
	for k, mut := range m.Mutings {
		if mut.ExpiresAt != nil && mut.ExpiresAt.Before(now) {
			delete(m.Mutings, k)
			userIDs = append(userIDs, mut.UserID)
		}
	}
	return userIDs, nil
}
