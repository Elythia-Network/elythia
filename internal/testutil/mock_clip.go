package testutil

import (
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/model"
)

// MockClipRepository is a test double for repository.ClipRepository.
type MockClipRepository struct {
	Clips     map[string]*model.Clip
	CreateErr error
	UpdateErr error
}

// NewMockClipRepository creates an empty MockClipRepository.
func NewMockClipRepository() *MockClipRepository {
	return &MockClipRepository{Clips: make(map[string]*model.Clip)}
}

func (m *MockClipRepository) Create(c *model.Clip) error {
	if m.CreateErr != nil {
		return m.CreateErr
	}
	m.Clips[c.ID] = c
	return nil
}

func (m *MockClipRepository) FindByID(id string) (*model.Clip, error) {
	c, ok := m.Clips[id]
	if !ok {
		return nil, ErrNotFound
	}
	return c, nil
}

func (m *MockClipRepository) UpdateFields(clipID string, fields map[string]any) error {
	if m.UpdateErr != nil {
		return m.UpdateErr
	}
	if len(fields) == 0 {
		return nil
	}
	c, ok := m.Clips[clipID]
	if !ok {
		return ErrNotFound
	}
	applyClipFields(c, fields)
	return nil
}

func (m *MockClipRepository) Delete(c *model.Clip) error {
	delete(m.Clips, c.ID)
	return nil
}

func (m *MockClipRepository) ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.Clip, error) {
	var rows []*model.Clip
	for _, c := range m.Clips {
		if c.UserID != userID {
			continue
		}
		if sinceID != "" && c.ID <= sinceID {
			continue
		}
		if untilID != "" && c.ID >= untilID {
			continue
		}
		rows = append(rows, c)
	}
	return paginateClips(rows, sinceID, untilID, limit, offset), nil
}

func (m *MockClipRepository) CountByUser(userID string) (int64, error) {
	var count int64
	for _, c := range m.Clips {
		if c.UserID == userID {
			count++
		}
	}
	return count, nil
}

func (m *MockClipRepository) ListPublicByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.Clip, error) {
	var rows []*model.Clip
	for _, c := range m.Clips {
		if c.UserID != userID || !c.IsPublic {
			continue
		}
		if sinceID != "" && c.ID <= sinceID {
			continue
		}
		if untilID != "" && c.ID >= untilID {
			continue
		}
		rows = append(rows, c)
	}
	return paginateClips(rows, sinceID, untilID, limit, offset), nil
}

// ListPublicByIDs returns the public clips whose id is in ids (any owner).
func (m *MockClipRepository) ListPublicByIDs(ids []string) ([]*model.Clip, error) {
	want := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		want[id] = struct{}{}
	}
	var rows []*model.Clip
	for _, c := range m.Clips {
		if _, ok := want[c.ID]; ok && c.IsPublic {
			rows = append(rows, c)
		}
	}
	return rows, nil
}

// paginateClips applies the same ordering / limit / cursor-vs-offset rules as
// the real clipRepository so mock-based tests exercise the upstream
// makePaginationQuery semantics. ASC ordering kicks in when sinceID is set
// without untilID (matches paginationOrder helper in repository package).
func paginateClips(rows []*model.Clip, sinceID, untilID string, limit, offset int) []*model.Clip {
	asc := sinceID != "" && untilID == ""
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			less := rows[i].ID < rows[j].ID
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

func applyClipFields(c *model.Clip, fields map[string]any) {
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
		case "isPublic":
			if b, ok := v.(bool); ok {
				c.IsPublic = b
			}
		case "lastClippedAt":
			if t, ok := v.(*time.Time); ok {
				c.LastClippedAt = t
			}
		}
	}
}

// MockClipNoteRepository is a test double for repository.ClipNoteRepository.
type MockClipNoteRepository struct {
	Entries map[string]*model.ClipNote
	// Notes / Following は ListByClipVisible の visibility push-down 再現に
	// 使う。Notes は noteID -> note (visibility 判定用)、Following は
	// followerID -> followeeIDs。未設定なら note 不在扱い (= 非可視)。
	Notes     map[string]*model.Note
	Following map[string][]string
}

// NewMockClipNoteRepository creates an empty MockClipNoteRepository.
func NewMockClipNoteRepository() *MockClipNoteRepository {
	return &MockClipNoteRepository{Entries: make(map[string]*model.ClipNote)}
}

func (m *MockClipNoteRepository) Create(cn *model.ClipNote) error {
	m.Entries[cn.ID] = cn
	return nil
}

func (m *MockClipNoteRepository) Delete(cn *model.ClipNote) error {
	delete(m.Entries, cn.ID)
	return nil
}

// DeleteByPair removes the matching entries and returns how many were removed.
func (m *MockClipNoteRepository) DeleteByPair(clipID, noteID string) (int64, error) {
	var n int64
	for id, cn := range m.Entries {
		if cn.ClipID == clipID && cn.NoteID == noteID {
			delete(m.Entries, id)
			n++
		}
	}
	return n, nil
}

func (m *MockClipNoteRepository) FindByPair(clipID, noteID string) (*model.ClipNote, error) {
	for _, cn := range m.Entries {
		if cn.ClipID == clipID && cn.NoteID == noteID {
			return cn, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockClipNoteRepository) CountByClip(clipID string) (int64, error) {
	var count int64
	for _, cn := range m.Entries {
		if cn.ClipID == clipID {
			count++
		}
	}
	return count, nil
}

// ListClipIDsByNote returns the distinct clipIds containing noteID.
func (m *MockClipNoteRepository) ListClipIDsByNote(noteID string) ([]string, error) {
	seen := make(map[string]struct{})
	var ids []string
	for _, cn := range m.Entries {
		if cn.NoteID == noteID {
			if _, dup := seen[cn.ClipID]; !dup {
				seen[cn.ClipID] = struct{}{}
				ids = append(ids, cn.ClipID)
			}
		}
	}
	return ids, nil
}

func (m *MockClipNoteRepository) ListByClip(clipID string, untilID, sinceID string, limit int) ([]*model.ClipNote, error) {
	return m.listByClip(clipID, "", false, untilID, sinceID, limit, nil)
}

// noteMatchesSearchWords mirrors the real repo's clips/notes search predicate:
// every word must hit note.text OR note.cw (substring, case-insensitive)。
// 空 slice / note 不在 (search 無条件) は true。
func noteMatchesSearchWords(n *model.Note, searchWords []string) bool {
	if len(searchWords) == 0 {
		return true
	}
	if n == nil {
		return false
	}
	text := ""
	if n.Text != nil {
		text = strings.ToLower(*n.Text)
	}
	cw := ""
	if n.CW != nil {
		cw = strings.ToLower(*n.CW)
	}
	for _, w := range searchWords {
		w = strings.ToLower(w)
		if !strings.Contains(text, w) && !strings.Contains(cw, w) {
			return false
		}
	}
	return true
}

// ListByClipVisible mirrors the real repo's visibility push-down: clip entries
// whose note the viewer cannot see are dropped before the limit slice.
// searchWords は実 repo と同じく text / cw への部分一致 (語内 OR、語間 AND)
// を case-insensitive で再現する (#1562)。
func (m *MockClipNoteRepository) ListByClipVisible(clipID, viewerID, untilID, sinceID string, limit int, searchWords []string) ([]*model.ClipNote, error) {
	return m.listByClip(clipID, viewerID, true, untilID, sinceID, limit, searchWords)
}

func (m *MockClipNoteRepository) listByClip(clipID, viewerID string, filterVisibility bool, untilID, sinceID string, limit int, searchWords []string) ([]*model.ClipNote, error) {
	var rows []*model.ClipNote
	for _, cn := range m.Entries {
		if cn.ClipID != clipID {
			continue
		}
		if filterVisibility && !noteVisibleToViewer(viewerID, m.Notes[cn.NoteID], m.Following) {
			continue
		}
		if !noteMatchesSearchWords(m.Notes[cn.NoteID], searchWords) {
			continue
		}
		// cursor / sort は note.id (= clip_note.noteId) で行う。client は note.id を
		// 渡し upstream も note.id でページネーションする (#1950、production listByClip と一致)。
		if untilID != "" && cn.NoteID >= untilID {
			continue
		}
		if sinceID != "" && cn.NoteID <= sinceID {
			continue
		}
		rows = append(rows, cn)
	}
	// production の paginationOrder と同じ ASC-flip (#405 Devin指摘)。比較列は noteId。
	asc := sinceID != "" && untilID == ""
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if asc {
				if rows[i].NoteID > rows[j].NoteID {
					rows[i], rows[j] = rows[j], rows[i]
				}
			} else {
				if rows[i].NoteID < rows[j].NoteID {
					rows[i], rows[j] = rows[j], rows[i]
				}
			}
		}
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

// MockClipFavoriteRepository is a test double for repository.ClipFavoriteRepository.
type MockClipFavoriteRepository struct {
	Favorites map[string]*model.ClipFavorite // keyed by "userId:clipId"
}

func NewMockClipFavoriteRepository() *MockClipFavoriteRepository {
	return &MockClipFavoriteRepository{Favorites: make(map[string]*model.ClipFavorite)}
}

func (m *MockClipFavoriteRepository) Create(fav *model.ClipFavorite) error {
	m.Favorites[fav.UserID+":"+fav.ClipID] = fav
	return nil
}

func (m *MockClipFavoriteRepository) Delete(userID, clipID string) error {
	delete(m.Favorites, userID+":"+clipID)
	return nil
}

func (m *MockClipFavoriteRepository) ListByUser(userID string) ([]*model.ClipFavorite, error) {
	var result []*model.ClipFavorite
	for _, f := range m.Favorites {
		if f.UserID == userID {
			result = append(result, f)
		}
	}
	return result, nil
}

func (m *MockClipFavoriteRepository) Exists(userID, clipID string) (bool, error) {
	_, ok := m.Favorites[userID+":"+clipID]
	return ok, nil
}

func (m *MockClipFavoriteRepository) CountByClip(clipID string) (int64, error) {
	var count int64
	for _, f := range m.Favorites {
		if f.ClipID == clipID {
			count++
		}
	}
	return count, nil
}
