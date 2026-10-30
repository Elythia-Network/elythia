package testutil

import (
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/model"
)

// MockPageRepository is a test double for repository.PageRepository.
type MockPageRepository struct {
	Pages     map[string]*model.Page
	CreateErr error
	UpdateErr error
	// UpdateFieldsCalls は UpdateFields に渡された field map を順に記録する。
	// jsonb 列へ `[]byte` を渡していないか等、**値の型そのもの**を検査する用。
	UpdateFieldsCalls []map[string]any
}

// NewMockPageRepository creates an empty MockPageRepository.
func NewMockPageRepository() *MockPageRepository {
	return &MockPageRepository{Pages: make(map[string]*model.Page)}
}

func (m *MockPageRepository) Create(p *model.Page) error {
	if m.CreateErr != nil {
		return m.CreateErr
	}
	m.Pages[p.ID] = p
	return nil
}

func (m *MockPageRepository) FindByID(id string) (*model.Page, error) {
	p, ok := m.Pages[id]
	if !ok {
		return nil, ErrNotFound
	}
	return p, nil
}

func (m *MockPageRepository) FindManyByIDs(ids []string) ([]*model.Page, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out := make([]*model.Page, 0, len(ids))
	for _, id := range ids {
		if p, ok := m.Pages[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func (m *MockPageRepository) FindByUserAndName(userID, name string) (*model.Page, error) {
	for _, p := range m.Pages {
		if p.UserID == userID && p.Name == name {
			return p, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockPageRepository) UpdateFields(pageID string, fields map[string]any) error {
	// **渡された field map をそのまま記録する。** 実 repo は値の型を driver へ
	// そのまま流すので、jsonb 列に `[]byte` を載せると bytea として送られて
	// SQLSTATE 22P02 で落ちる。in-memory の値を見るだけでは型の誤りを
	// 検出できないので、呼び出し側が何を渡したか自体を検査対象にする。
	m.UpdateFieldsCalls = append(m.UpdateFieldsCalls, fields)
	if m.UpdateErr != nil {
		return m.UpdateErr
	}
	if len(fields) == 0 {
		return nil
	}
	p, ok := m.Pages[pageID]
	if !ok {
		return ErrNotFound
	}
	applyPageFields(p, fields)
	return nil
}

func (m *MockPageRepository) Delete(p *model.Page) error {
	delete(m.Pages, p.ID)
	return nil
}

func (m *MockPageRepository) ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.Page, error) {
	var rows []*model.Page
	for _, p := range m.Pages {
		if p.UserID != userID {
			continue
		}
		if sinceID != "" && p.ID <= sinceID {
			continue
		}
		if untilID != "" && p.ID >= untilID {
			continue
		}
		rows = append(rows, p)
	}
	return paginatePages(rows, sinceID, untilID, limit, offset), nil
}

func (m *MockPageRepository) ListPublicByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.Page, error) {
	var rows []*model.Page
	for _, p := range m.Pages {
		if p.UserID != userID || p.Visibility != model.PageVisibilityPublic {
			continue
		}
		if sinceID != "" && p.ID <= sinceID {
			continue
		}
		if untilID != "" && p.ID >= untilID {
			continue
		}
		rows = append(rows, p)
	}
	return paginatePages(rows, sinceID, untilID, limit, offset), nil
}

func (m *MockPageRepository) ListFeatured(sinceID, untilID string, limit, offset int) ([]*model.Page, error) {
	var rows []*model.Page
	for _, p := range m.Pages {
		if p.Visibility != model.PageVisibilityPublic {
			continue
		}
		if sinceID != "" && p.ID <= sinceID {
			continue
		}
		if untilID != "" && p.ID >= untilID {
			continue
		}
		rows = append(rows, p)
	}
	if sinceID != "" || untilID != "" {
		return paginatePages(rows, sinceID, untilID, limit, offset), nil
	}
	// offset mode: 元実装と同じく likedCount DESC で sort
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if rows[i].LikedCount < rows[j].LikedCount ||
				(rows[i].LikedCount == rows[j].LikedCount && rows[i].ID < rows[j].ID) {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	return paginatePages(rows, "", "", limit, offset), nil
}

func (m *MockPageRepository) IncrementCount(pageID, column string, delta int) error {
	p, ok := m.Pages[pageID]
	if !ok {
		return ErrNotFound
	}
	if column == "likedCount" {
		p.LikedCount += delta
	}
	return nil
}

func paginatePages(rows []*model.Page, sinceID, untilID string, limit, offset int) []*model.Page {
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

func applyPageFields(p *model.Page, fields map[string]any) {
	for k, v := range fields {
		switch k {
		case "title":
			if s, ok := v.(string); ok {
				p.Title = s
			}
		case "name":
			if s, ok := v.(string); ok {
				p.Name = s
			}
		case "summary":
			if s, ok := v.(*string); ok {
				p.Summary = s
			}
		case "alignCenter":
			if b, ok := v.(bool); ok {
				p.AlignCenter = b
			}
		case "hideTitleWhenPinned":
			if b, ok := v.(bool); ok {
				p.HideTitleWhenPinned = b
			}
		case "font":
			if s, ok := v.(string); ok {
				p.Font = s
			}
		case "eyeCatchingImageId":
			if s, ok := v.(*string); ok {
				p.EyeCatchingImageID = s
			}
		case "content":
			if b, ok := v.([]byte); ok {
				p.Content = b
			}
		case "variables":
			if b, ok := v.([]byte); ok {
				p.Variables = b
			}
		case "script":
			if s, ok := v.(string); ok {
				p.Script = s
			}
		case "visibility":
			if vis, ok := v.(model.PageVisibility); ok {
				p.Visibility = vis
			}
		case "updatedAt":
			if t, ok := v.(time.Time); ok {
				p.UpdatedAt = t
			}
		}
	}
}

// MockFlashRepository is a test double for repository.FlashRepository.
type MockFlashRepository struct {
	Flashes   map[string]*model.Flash
	CreateErr error
	UpdateErr error
	// LastUpdates は UpdateFields に最後に渡された fields map を capture
	// する。caller が slice value を model.StringArray 等で正しく wrap して
	// いるかを test 側で assert するため (#896)。
	LastUpdates map[string]any
}

// NewMockFlashRepository creates an empty MockFlashRepository.
func NewMockFlashRepository() *MockFlashRepository {
	return &MockFlashRepository{Flashes: make(map[string]*model.Flash)}
}

func (m *MockFlashRepository) Create(f *model.Flash) error {
	if m.CreateErr != nil {
		return m.CreateErr
	}
	m.Flashes[f.ID] = f
	return nil
}

func (m *MockFlashRepository) FindByID(id string) (*model.Flash, error) {
	f, ok := m.Flashes[id]
	if !ok {
		return nil, ErrNotFound
	}
	return f, nil
}

func (m *MockFlashRepository) UpdateFields(flashID string, fields map[string]any) error {
	m.LastUpdates = fields
	if m.UpdateErr != nil {
		return m.UpdateErr
	}
	if len(fields) == 0 {
		return nil
	}
	f, ok := m.Flashes[flashID]
	if !ok {
		return ErrNotFound
	}
	applyFlashFields(f, fields)
	return nil
}

func (m *MockFlashRepository) Delete(f *model.Flash) error {
	delete(m.Flashes, f.ID)
	return nil
}

func (m *MockFlashRepository) ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.Flash, error) {
	var rows []*model.Flash
	for _, f := range m.Flashes {
		if f.UserID != userID {
			continue
		}
		if sinceID != "" && f.ID <= sinceID {
			continue
		}
		if untilID != "" && f.ID >= untilID {
			continue
		}
		rows = append(rows, f)
	}
	return paginateFlashes(rows, sinceID, untilID, limit, offset), nil
}

func (m *MockFlashRepository) ListPublicByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.Flash, error) {
	var rows []*model.Flash
	for _, f := range m.Flashes {
		if f.UserID != userID || f.Visibility != "public" {
			continue
		}
		if sinceID != "" && f.ID <= sinceID {
			continue
		}
		if untilID != "" && f.ID >= untilID {
			continue
		}
		rows = append(rows, f)
	}
	return paginateFlashes(rows, sinceID, untilID, limit, offset), nil
}

func (m *MockFlashRepository) ListFeatured(sinceID, untilID string, limit, offset int) ([]*model.Flash, error) {
	var rows []*model.Flash
	for _, f := range m.Flashes {
		if sinceID != "" && f.ID <= sinceID {
			continue
		}
		if untilID != "" && f.ID >= untilID {
			continue
		}
		rows = append(rows, f)
	}
	if sinceID != "" || untilID != "" {
		return paginateFlashes(rows, sinceID, untilID, limit, offset), nil
	}
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if rows[i].LikedCount < rows[j].LikedCount ||
				(rows[i].LikedCount == rows[j].LikedCount && rows[i].ID < rows[j].ID) {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
	return paginateFlashes(rows, "", "", limit, offset), nil
}

func (m *MockFlashRepository) Search(query, sinceID, untilID string, limit, offset int) ([]*model.Flash, error) {
	var rows []*model.Flash
	q := strings.ToLower(query)
	for _, f := range m.Flashes {
		if !(strings.Contains(strings.ToLower(f.Title), q) ||
			strings.Contains(strings.ToLower(f.Summary), q)) {
			continue
		}
		if sinceID != "" && f.ID <= sinceID {
			continue
		}
		if untilID != "" && f.ID >= untilID {
			continue
		}
		rows = append(rows, f)
	}
	if sinceID != "" || untilID != "" {
		return paginateFlashes(rows, sinceID, untilID, limit, offset), nil
	}
	sortFlashesByUpdatedDesc(rows)
	return paginateFlashes(rows, "", "", limit, offset), nil
}

func (m *MockFlashRepository) IncrementCount(flashID, column string, delta int) error {
	f, ok := m.Flashes[flashID]
	if !ok {
		return ErrNotFound
	}
	if column == "likedCount" {
		f.LikedCount += delta
	}
	return nil
}

func sortFlashesByUpdatedDesc(rows []*model.Flash) {
	for i := range rows {
		for j := i + 1; j < len(rows); j++ {
			if rows[i].UpdatedAt.Before(rows[j].UpdatedAt) ||
				(rows[i].UpdatedAt.Equal(rows[j].UpdatedAt) && rows[i].ID < rows[j].ID) {
				rows[i], rows[j] = rows[j], rows[i]
			}
		}
	}
}

func paginateFlashes(rows []*model.Flash, sinceID, untilID string, limit, offset int) []*model.Flash {
	asc := sinceID != "" && untilID == ""
	if sinceID != "" || untilID != "" {
		// cursor mode は id 順に並べ替える (offset mode の updatedAt 順
		// は既に呼び出し側で sort 済み)。
		for i := 0; i < len(rows); i++ {
			for j := i + 1; j < len(rows); j++ {
				less := rows[i].ID < rows[j].ID
				if (asc && !less) || (!asc && less) {
					rows[i], rows[j] = rows[j], rows[i]
				}
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

func applyFlashFields(f *model.Flash, fields map[string]any) {
	for k, v := range fields {
		switch k {
		case "title":
			if s, ok := v.(string); ok {
				f.Title = s
			}
		case "summary":
			if s, ok := v.(string); ok {
				f.Summary = s
			}
		case "script":
			if s, ok := v.(string); ok {
				f.Script = s
			}
		case "permissions":
			if arr, ok := v.([]string); ok {
				f.Permissions = arr
			}
		case "visibility":
			if s, ok := v.(string); ok {
				f.Visibility = s
			}
		case "updatedAt":
			if t, ok := v.(time.Time); ok {
				f.UpdatedAt = t
			}
		}
	}
}

// MockFlashLikeRepository is a test double for repository.FlashLikeRepository.
type MockFlashLikeRepository struct {
	Likes map[string]*model.FlashLike
}

// NewMockFlashLikeRepository creates an empty MockFlashLikeRepository.
func NewMockFlashLikeRepository() *MockFlashLikeRepository {
	return &MockFlashLikeRepository{Likes: make(map[string]*model.FlashLike)}
}

func (m *MockFlashLikeRepository) Create(l *model.FlashLike) error {
	m.Likes[l.ID] = l
	return nil
}

func (m *MockFlashLikeRepository) Delete(l *model.FlashLike) error {
	delete(m.Likes, l.ID)
	return nil
}

func (m *MockFlashLikeRepository) FindByPair(userID, flashID string) (*model.FlashLike, error) {
	for _, l := range m.Likes {
		if l.UserID == userID && l.FlashID == flashID {
			return l, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockFlashLikeRepository) Exists(userID, flashID string) (bool, error) {
	_, err := m.FindByPair(userID, flashID)
	return err == nil, nil
}

func (m *MockFlashLikeRepository) ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.FlashLike, error) {
	var rows []*model.FlashLike
	for _, l := range m.Likes {
		if l.UserID != userID {
			continue
		}
		if sinceID != "" && l.ID <= sinceID {
			continue
		}
		if untilID != "" && l.ID >= untilID {
			continue
		}
		rows = append(rows, l)
	}
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
	if sinceID != "" || untilID != "" {
		if limit > len(rows) {
			limit = len(rows)
		}
		return rows[:limit], nil
	}
	if offset >= len(rows) {
		return nil, nil
	}
	end := min(offset+limit, len(rows))
	return rows[offset:end], nil
}

// ListByUserSearch ignores the search filter (the mock holds no flash title /
// summary to match against) and delegates to ListByUser. The SQL search path is
// covered by the repository-level DB test.
func (m *MockFlashLikeRepository) ListByUserSearch(userID, _, sinceID, untilID string, limit, offset int) ([]*model.FlashLike, error) {
	return m.ListByUser(userID, sinceID, untilID, limit, offset)
}

// ListLikedFlashIDs returns the subset of flashIDs liked by userID.
func (m *MockFlashLikeRepository) ListLikedFlashIDs(userID string, flashIDs []string) ([]string, error) {
	want := make(map[string]struct{}, len(flashIDs))
	for _, id := range flashIDs {
		want[id] = struct{}{}
	}
	var out []string
	for _, l := range m.Likes {
		if l.UserID != userID {
			continue
		}
		if _, ok := want[l.FlashID]; ok {
			out = append(out, l.FlashID)
		}
	}
	return out, nil
}

// MockPageLikeRepository is a test double for repository.PageLikeRepository.
type MockPageLikeRepository struct {
	Likes map[string]*model.PageLike
}

// NewMockPageLikeRepository creates an empty MockPageLikeRepository.
func NewMockPageLikeRepository() *MockPageLikeRepository {
	return &MockPageLikeRepository{Likes: make(map[string]*model.PageLike)}
}

func (m *MockPageLikeRepository) Create(l *model.PageLike) error {
	m.Likes[l.ID] = l
	return nil
}

func (m *MockPageLikeRepository) Delete(l *model.PageLike) error {
	delete(m.Likes, l.ID)
	return nil
}

func (m *MockPageLikeRepository) FindByPair(userID, pageID string) (*model.PageLike, error) {
	for _, l := range m.Likes {
		if l.UserID == userID && l.PageID == pageID {
			return l, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockPageLikeRepository) Exists(userID, pageID string) (bool, error) {
	_, err := m.FindByPair(userID, pageID)
	return err == nil, nil
}

func (m *MockPageLikeRepository) ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.PageLike, error) {
	out := make([]*model.PageLike, 0)
	for _, l := range m.Likes {
		if l.UserID != userID {
			continue
		}
		// cursor filter: real repo は id > sinceID / id < untilID で絞り込む。
		// mock も string 比較で同 semantics (aidx は lex-sortable なので OK)。
		if sinceID != "" && l.ID <= sinceID {
			continue
		}
		if untilID != "" && l.ID >= untilID {
			continue
		}
		out = append(out, l)
	}
	// tiny in-memory paging; sort not necessary for unit tests
	if sinceID == "" && untilID == "" && offset > 0 {
		if offset >= len(out) {
			return nil, nil
		}
		out = out[offset:]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListLikedPageIDs returns the subset of pageIDs liked by userID.
func (m *MockPageLikeRepository) ListLikedPageIDs(userID string, pageIDs []string) ([]string, error) {
	want := make(map[string]struct{}, len(pageIDs))
	for _, id := range pageIDs {
		want[id] = struct{}{}
	}
	var ids []string
	for _, l := range m.Likes {
		if l.UserID != userID {
			continue
		}
		if _, ok := want[l.PageID]; ok {
			ids = append(ids, l.PageID)
		}
	}
	return ids, nil
}

// MockGalleryRepository is a test double for repository.GalleryRepository.
type MockGalleryRepository struct {
	Posts map[string]*model.GalleryPost
	Likes map[string][]*model.GalleryLike // keyed by userID
	// LikedBy holds "<userID>:<postID>" pairs reported by ExistsLike.
	LikedBy map[string]bool
	Err     error
}

// NewMockGalleryRepository creates an empty MockGalleryRepository.
func NewMockGalleryRepository() *MockGalleryRepository {
	return &MockGalleryRepository{
		Posts:   make(map[string]*model.GalleryPost),
		Likes:   make(map[string][]*model.GalleryLike),
		LikedBy: make(map[string]bool),
	}
}

func (m *MockGalleryRepository) ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.GalleryPost, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	var out []*model.GalleryPost
	for _, p := range m.Posts {
		if p.UserID == userID {
			out = append(out, p)
		}
	}
	return out, nil
}

func (m *MockGalleryRepository) ListLikesByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.GalleryLike, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	return m.Likes[userID], nil
}

func (m *MockGalleryRepository) FindPostsByIDs(ids []string) ([]*model.GalleryPost, error) {
	if m.Err != nil {
		return nil, m.Err
	}
	var out []*model.GalleryPost
	for _, id := range ids {
		if p, ok := m.Posts[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func (m *MockGalleryRepository) ExistsLike(userID, postID string) (bool, error) {
	if m.Err != nil {
		return false, m.Err
	}
	return m.LikedBy[userID+":"+postID], nil
}
