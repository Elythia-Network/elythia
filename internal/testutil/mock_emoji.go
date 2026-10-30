package testutil

import (
	"sort"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/model"
)

// MockEmojiRepository is a test double for repository.EmojiRepository.
type MockEmojiRepository struct {
	// keyed by "name@host" (host="" for local)
	Emojis map[string]*model.Emoji
	// CreateErr/UpdateErr forces Create/UpdateFields to return the given error
	// without persisting. Used to exercise upsertEmojis error handling paths.
	CreateErr error
	UpdateErr error
	// DeleteErr forces Delete to return the given error (#2966 の補償処理で
	// 「絵文字は消せなかったが drive の複製は片付ける」経路を作るため)。
	DeleteErr error
}

func NewMockEmojiRepository() *MockEmojiRepository {
	return &MockEmojiRepository{Emojis: make(map[string]*model.Emoji)}
}

func (m *MockEmojiRepository) Create(e *model.Emoji) error {
	if m.CreateErr != nil {
		return m.CreateErr
	}
	key := e.Name + "@"
	if e.Host != nil {
		key += *e.Host
	}
	m.Emojis[key] = e
	return nil
}

func (m *MockEmojiRepository) FindByID(id string) (*model.Emoji, error) {
	for _, e := range m.Emojis {
		if e.ID == id {
			return e, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockEmojiRepository) UpdateFields(id string, fields map[string]any) error {
	if m.UpdateErr != nil {
		return m.UpdateErr
	}
	for oldKey, e := range m.Emojis {
		if e.ID == id {
			oldName := e.Name
			for k, v := range fields {
				switch k {
				case "name":
					if s, ok := v.(string); ok {
						e.Name = s
					}
				case "category":
					if s, ok := v.(string); ok {
						e.Category = &s
					}
				case "license":
					// resolver の license diff path (#731) は *string を直接
					// fields["license"] に入れる。handler 経路は string 値を
					// 入れるので両方受ける (nil pointer は明示 NULL 化を表す)。
					switch s := v.(type) {
					case string:
						e.License = &s
					case *string:
						e.License = s
					}
				case "aliases":
					// EmojiUpdate handler は model.StringArray でラップして渡す
					// (#729)。resolver / 旧 caller は []string を渡すので両方
					// 受ける (PostgreSQL は同じ varchar[] に変換される)。
					switch arr := v.(type) {
					case model.StringArray:
						e.Aliases = arr
					case []string:
						e.Aliases = arr
					}
				case "isSensitive":
					if b, ok := v.(bool); ok {
						e.IsSensitive = b
					}
				case "localOnly":
					if b, ok := v.(bool); ok {
						e.LocalOnly = b
					}
				case "updatedAt":
					if ts, ok := v.(time.Time); ok {
						e.UpdatedAt = &ts
					}
				case "originalUrl":
					if s, ok := v.(string); ok {
						e.OriginalURL = s
					}
				case "publicUrl":
					if s, ok := v.(string); ok {
						e.PublicURL = s
					}
				case "uri":
					if sp, ok := v.(*string); ok {
						e.URI = sp
					}
				case "type":
					switch s := v.(type) {
					case string:
						e.Type = &s
					case *string:
						e.Type = s
					}
				case "roleIdsThatCanBeUsedThisEmojiAsReaction":
					switch arr := v.(type) {
					case model.StringArray:
						e.RoleIDsThatCanBeUsedThisEmojiAsReaction = arr
					case []string:
						e.RoleIDsThatCanBeUsedThisEmojiAsReaction = arr
					}
				}
			}
			// rename 時は map key (name+"@"+host) を張り替えて、production の
			// name-unique 検索意味論 (FindByNameAndHost) と整合させる。
			if e.Name != oldName {
				host := ""
				if e.Host != nil {
					host = *e.Host
				}
				delete(m.Emojis, oldKey)
				m.Emojis[e.Name+"@"+host] = e
			}
			return nil
		}
	}
	return ErrNotFound
}

// FindManyByNamesAndHost returns emojis matching any of the given names for a specific host.
func (m *MockEmojiRepository) FindManyByNamesAndHost(names []string, host *string) ([]*model.Emoji, error) {
	if len(names) == 0 {
		return nil, nil
	}
	nameSet := make(map[string]struct{}, len(names))
	for _, n := range names {
		nameSet[n] = struct{}{}
	}
	var out []*model.Emoji
	for _, e := range m.Emojis {
		if _, ok := nameSet[e.Name]; !ok {
			continue
		}
		if host == nil && e.Host != nil {
			continue
		}
		if host != nil && (e.Host == nil || *e.Host != *host) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// FindManyByKeys returns emojis matching any of the given (name, host) pairs.
func (m *MockEmojiRepository) FindManyByKeys(keys []model.EmojiKey) ([]*model.Emoji, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	want := make(map[model.EmojiKey]struct{}, len(keys))
	for _, k := range keys {
		want[k] = struct{}{}
	}
	var out []*model.Emoji
	for _, e := range m.Emojis {
		if _, ok := want[model.EmojiKeyOf(e)]; ok {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *MockEmojiRepository) Delete(id string) error {
	if m.DeleteErr != nil {
		return m.DeleteErr
	}
	for k, e := range m.Emojis {
		if e.ID == id {
			delete(m.Emojis, k)
			return nil
		}
	}
	return nil
}

func (m *MockEmojiRepository) ListWithFilter(query, category string, local bool, sinceID, untilID string, limit, offset int) ([]*model.Emoji, error) {
	var result []*model.Emoji
	for _, e := range m.Emojis {
		if local && e.Host != nil {
			continue
		}
		if sinceID != "" && e.ID <= sinceID {
			continue
		}
		if untilID != "" && e.ID >= untilID {
			continue
		}
		result = append(result, e)
	}
	if limit <= 0 {
		limit = 50
	}
	if offset >= len(result) {
		return nil, nil
	}
	end := min(offset+limit, len(result))
	return result[offset:end], nil
}

func (m *MockEmojiRepository) ListLocal() ([]*model.Emoji, error) {
	var result []*model.Emoji
	for _, e := range m.Emojis {
		if e.Host == nil {
			result = append(result, e)
		}
	}
	return result, nil
}

func (m *MockEmojiRepository) FindByNameAndHost(name string, host *string) (*model.Emoji, error) {
	key := name + "@"
	if host != nil {
		key += *host
	}
	e, ok := m.Emojis[key]
	if !ok {
		return nil, ErrNotFound
	}
	return e, nil
}

func (m *MockEmojiRepository) FindManyByIDs(ids []string) ([]*model.Emoji, error) {
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	out := make([]*model.Emoji, 0)
	for _, e := range m.Emojis {
		if _, ok := set[e.ID]; ok {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *MockEmojiRepository) UpdateFieldsMany(ids []string, fields map[string]any) error {
	if len(ids) == 0 || len(fields) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	for _, e := range m.Emojis {
		if _, ok := set[e.ID]; !ok {
			continue
		}
		for k, v := range fields {
			switch k {
			case "category":
				// nil (any) は null reset (#1948-13)。string は値設定。
				if v == nil {
					e.Category = nil
				} else if s, ok := v.(string); ok {
					e.Category = &s
				}
			case "license":
				if v == nil {
					e.License = nil
				} else if s, ok := v.(string); ok {
					e.License = &s
				}
			case "aliases":
				// production 経路は model.StringArray で wrap して渡す (#882
				// で発覚した character varying[] vs record drift を回避)。
				// test/legacy で plain []string を渡す呼び出しもあるので両対応。
				if arr, ok := v.(model.StringArray); ok {
					e.Aliases = []string(arr)
				} else if arr, ok := v.([]string); ok {
					e.Aliases = arr
				}
			case "updatedAt":
				if ts, ok := v.(time.Time); ok {
					e.UpdatedAt = &ts
				}
			case "isSensitive":
				if b, ok := v.(bool); ok {
					e.IsSensitive = b
				}
			}
		}
	}
	return nil
}

func (m *MockEmojiRepository) DeleteMany(ids []string) error {
	for _, id := range ids {
		// 各 emoji は name@ と name@host の両方のキーで参照されうる。
		// Emojis[id] での逆引きは ID が key の場合にしか当たらないため、走査して削除する。
		for k, e := range m.Emojis {
			if e.ID == id {
				delete(m.Emojis, k)
			}
		}
	}
	return nil
}

func (m *MockEmojiRepository) ListRemoteWithFilter(query, host, sinceID, untilID string, limit, offset int) ([]*model.Emoji, error) {
	out := make([]*model.Emoji, 0)
	seen := make(map[string]bool)
	for _, e := range m.Emojis {
		if seen[e.ID] {
			continue
		}
		if e.Host == nil {
			continue
		}
		if host != "" && *e.Host != host {
			continue
		}
		// production ListRemoteWithFilter は case-sensitive LIKE (#1948-13) なので
		// mock も case-sensitive Contains に揃える。
		if query != "" && !strings.Contains(e.Name, query) {
			continue
		}
		if sinceID != "" && e.ID <= sinceID {
			continue
		}
		if untilID != "" && e.ID >= untilID {
			continue
		}
		seen[e.ID] = true
		out = append(out, e)
	}
	// id DESC で安定ソート
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[i].ID < out[j].ID {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if limit <= 0 {
		limit = 30
	}
	if offset >= len(out) {
		return []*model.Emoji{}, nil
	}
	end := offset + limit
	if end > len(out) {
		end = len(out)
	}
	return out[offset:end], nil
}

func (m *MockEmojiRepository) ListV2(filter model.EmojiV2Filter) ([]*model.Emoji, error) {
	all := m.filterV2(filter)
	m.sortV2(all, filter.SortKeys)

	limit := filter.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	offset := 0
	if filter.Page > 0 {
		offset = (filter.Page - 1) * limit
	}
	if offset >= len(all) {
		return []*model.Emoji{}, nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end], nil
}

func (m *MockEmojiRepository) CountV2(filter model.EmojiV2Filter) (int64, error) {
	return int64(len(m.filterV2(filter))), nil
}

// filterV2 extracts emojis matching the v2 filter criteria.
func (m *MockEmojiRepository) filterV2(filter model.EmojiV2Filter) []*model.Emoji {
	seen := make(map[string]bool)
	var out []*model.Emoji
	for _, e := range m.Emojis {
		if seen[e.ID] {
			continue
		}
		if filter.Query != nil {
			fq := filter.Query
			if fq.HostType == "local" && e.Host != nil {
				continue
			}
			if fq.HostType == "remote" && e.Host == nil {
				continue
			}
			if fq.Name != "" && !mockILIKE(e.Name, fq.Name) {
				continue
			}
			if fq.Host != "" {
				if e.Host == nil || !mockILIKE(*e.Host, fq.Host) {
					continue
				}
			}
			if fq.Category != "" {
				if e.Category == nil || !mockILIKE(*e.Category, fq.Category) {
					continue
				}
			}
			if fq.Type != "" {
				if e.Type == nil || !mockILIKE(*e.Type, fq.Type) {
					continue
				}
			}
			if fq.Aliases != "" {
				aliasStr := strings.Join(e.Aliases, ",")
				if !mockILIKE(aliasStr, fq.Aliases) {
					continue
				}
			}
			if fq.License != "" {
				if e.License == nil || !mockILIKE(*e.License, fq.License) {
					continue
				}
			}
			if fq.URI != "" {
				if e.URI == nil || !mockILIKE(*e.URI, fq.URI) {
					continue
				}
			}
			if fq.PublicURL != "" && !mockILIKE(e.PublicURL, fq.PublicURL) {
				continue
			}
			if fq.OriginalURL != "" && !mockILIKE(e.OriginalURL, fq.OriginalURL) {
				continue
			}
			if fq.IsSensitive != nil && e.IsSensitive != *fq.IsSensitive {
				continue
			}
			if fq.LocalOnly != nil && e.LocalOnly != *fq.LocalOnly {
				continue
			}
		}
		if filter.SinceID != "" && e.ID <= filter.SinceID {
			continue
		}
		if filter.UntilID != "" && e.ID >= filter.UntilID {
			continue
		}
		seen[e.ID] = true
		out = append(out, e)
	}
	return out
}

// sortV2 sorts emojis according to the given sortKeys.
// Falls back to id DESC when no valid sort key is found.
func (m *MockEmojiRepository) sortV2(emojis []*model.Emoji, sortKeys []string) {
	// 有効なsortKeyが1つもなければid DESCにfallback
	hasValid := false
	for _, sk := range sortKeys {
		if len(sk) >= 2 {
			hasValid = true
			break
		}
	}
	if !hasValid {
		sortKeys = []string{"-id"}
	}
	sort.SliceStable(emojis, func(i, j int) bool {
		for _, sk := range sortKeys {
			if len(sk) < 2 {
				continue
			}
			asc := sk[0] == '+'
			col := sk[1:]
			cmp := mockEmojiCompare(emojis[i], emojis[j], col)
			if cmp == 0 {
				continue
			}
			if asc {
				return cmp < 0
			}
			return cmp > 0
		}
		return false
	})
}

func mockEmojiCompare(a, b *model.Emoji, col string) int {
	switch col {
	case "id":
		return strings.Compare(a.ID, b.ID)
	case "name":
		return strings.Compare(a.Name, b.Name)
	case "host":
		ah, bh := "", ""
		if a.Host != nil {
			ah = *a.Host
		}
		if b.Host != nil {
			bh = *b.Host
		}
		return strings.Compare(ah, bh)
	case "category":
		ac, bc := "", ""
		if a.Category != nil {
			ac = *a.Category
		}
		if b.Category != nil {
			bc = *b.Category
		}
		return strings.Compare(ac, bc)
	default:
		return 0
	}
}

func mockILIKE(value, substr string) bool {
	return strings.Contains(strings.ToLower(value), strings.ToLower(substr))
}

// MockAvatarDecorationRepository is a test double for
// repository.AvatarDecorationRepository.
type MockAvatarDecorationRepository struct {
	Decorations map[string]*model.AvatarDecoration
	CreateErr   error
}

// NewMockAvatarDecorationRepository creates an empty mock.
func NewMockAvatarDecorationRepository() *MockAvatarDecorationRepository {
	return &MockAvatarDecorationRepository{Decorations: make(map[string]*model.AvatarDecoration)}
}

func (m *MockAvatarDecorationRepository) Create(d *model.AvatarDecoration) error {
	if m.CreateErr != nil {
		return m.CreateErr
	}
	m.Decorations[d.ID] = d
	return nil
}

func (m *MockAvatarDecorationRepository) FindByID(id string) (*model.AvatarDecoration, error) {
	d, ok := m.Decorations[id]
	if !ok {
		return nil, ErrNotFound
	}
	return d, nil
}

func (m *MockAvatarDecorationRepository) List() ([]*model.AvatarDecoration, error) {
	rows := make([]*model.AvatarDecoration, 0, len(m.Decorations))
	for _, d := range m.Decorations {
		rows = append(rows, d)
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

func (m *MockAvatarDecorationRepository) UpdateFields(id string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	d, ok := m.Decorations[id]
	if !ok {
		return nil
	}
	for k, v := range fields {
		switch k {
		case "name":
			if s, ok := v.(string); ok {
				d.Name = s
			}
		case "description":
			if s, ok := v.(string); ok {
				d.Description = s
			}
		case "url":
			if s, ok := v.(string); ok {
				d.URL = s
			}
		case "roleIdsThatCanBeUsedThisDecoration":
			// production code は model.StringArray でラップして送る (#931)。
			// 旧 path との互換のため []string も受ける。
			switch arr := v.(type) {
			case model.StringArray:
				d.RoleIDs = []string(arr)
			case []string:
				d.RoleIDs = arr
			}
		case "updatedAt":
			if ts, ok := v.(time.Time); ok {
				d.UpdatedAt = &ts
			}
		case "category":
			// upstream Misskey #17034 で導入された nullable field。
			// production は handler 側で *string を deref して string を渡す。
			if s, ok := v.(string); ok {
				cat := s
				d.Category = &cat
			} else if v == nil {
				d.Category = nil
			}
		}
	}
	return nil
}

func (m *MockAvatarDecorationRepository) Delete(id string) error {
	delete(m.Decorations, id)
	return nil
}
