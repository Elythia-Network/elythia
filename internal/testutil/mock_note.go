package testutil

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// MockNoteRepository is a test double for repository.NoteRepository.
type MockNoteRepository struct {
	Notes          map[string]*model.Note
	ReactionCounts map[string]map[string]int // noteID -> reaction -> count
	// ExistingOnPrimaryErr, when set, is returned by ExistingNoteIDsOnPrimary
	// so callers can exercise the fail-safe path (#2719).
	ExistingOnPrimaryErr error
	// ListRenoteOrReplyErr, when set, is returned by
	// ListRenoteOrReplyRemoteUserIDs so callers can exercise the lookup
	// failure path (#2995).
	ListRenoteOrReplyErr error
	// FindByURIErr forces FindByURI to fail with a non-not-found error, for
	// exercising the paths that must retry instead of acking (#3115).
	FindByURIErr error
	// FindErr forces FindByID (and the wrappers that delegate to it) to fail
	// with a non-not-found error, for exercising the paths that must retry
	// instead of acking (#3121).
	FindErr error
	// Following は ListByUserIDFiltered の visibility push-down (followers note
	// の follow 判定) に使う followerID -> followeeIDs map。未設定なら follow
	// なし扱い (= 非 follower viewer)。testutil は core/note を import すると
	// repository 内部テストとの循環になるため CanSeeNote は使わず inline で
	// 可視性を再現する (条件は CanSeeNote / real repo SQL と一致させる)。
	Following map[string][]string
	// UserListMembers は ListByUserList の member-filter + reply gate (#1495 /
	// #1496) を mock 側で再現するための listID -> memberships。テストは
	// noteRepo.UserListMembers[listID] = []*model.UserListMembership{...} の
	// shape で直接 seed する。空 / nil の listID は member 不在として空結果。
	UserListMembers map[string][]*model.UserListMembership
	// UpdateFieldsCalls は UpdateFields に渡された field map を順に記録する。
	//
	// **in-memory の値を見るだけでは「fields への載せ忘れ」を検出できない** —
	// 呼び出し側が持つ `*model.Note` は `Notes[id]` と同じ pointer なので、
	// `existing.X = v` だけしても mock 上は反映されて見える。実 DB では
	// `fields` に載っていない列は UPDATE 文に出ないので、**呼び出し側から見えて
	// いる値と DB の値が食い違う** (#2729)。
	//
	// 同じ理由で、**`UpdateFields` の switch に列を足すときは慎重に。** 足すと
	// 「`fields` に載せた」と「呼び出し側が in-memory を書き換えた」を
	// 区別できなくなる。実際 `case "url"` を足すと、呼び出し側の
	// `existing.URL = &u` を消す変異がどのテストにも掛からなくなる
	// (`fields["url"]` 側を消す変異はこの記録が捕まえるので落ちる)。
	UpdateFieldsCalls []map[string]any
}

func NewMockNoteRepository() *MockNoteRepository {
	return &MockNoteRepository{
		Notes:           make(map[string]*model.Note),
		ReactionCounts:  make(map[string]map[string]int),
		Following:       make(map[string][]string),
		UserListMembers: make(map[string][]*model.UserListMembership),
	}
}

// Create inserts a note, enforcing the production partial UNIQUE index on
// `note.uri` (migration/000057_note_uri_unique_index).
//
// **制約を模さないと「同じ URI を二重に取り込む」形の欠陥がテストを素通りする。**
// 実際 #2686 は、inbox 直送でピン留め投稿を取り込むと同じ note を二度作りに行き、
// 外側の Create が UNIQUE に当たって created=false になる (= 通知が落ちる) という
// ものだったが、素の map 書き込みでは 2 行できるだけで誰も気付けなかった。
//
// 全パッケージで壊れるテストは無いことを確認済み。
func (m *MockNoteRepository) Create(note *model.Note) error {
	// `IDX_note_uri` の WHERE は `uri IS NOT NULL` なので、**空文字も index
	// 対象**。本番なら衝突する値をここで通すと、制約を模した意味が薄れる。
	if note.URI != nil {
		for _, existing := range m.Notes {
			if existing.ID != note.ID && existing.URI != nil && *existing.URI == *note.URI {
				return errors.New("mock: duplicate note.uri; PostgreSQL rejects it with SQLSTATE 23505 (UNIQUE IDX_note_uri)")
			}
		}
	}
	m.Notes[note.ID] = note
	return nil
}

// CreateWithTx mirrors Create for callers whose production insert shares a
// transaction with a user-row lock.
func (m *MockNoteRepository) CreateWithTx(note *model.Note, _ *gorm.DB) error {
	return m.Create(note)
}

func (m *MockNoteRepository) FindByID(id string) (*model.Note, error) {
	if m.FindErr != nil {
		return nil, m.FindErr
	}
	n, ok := m.Notes[id]
	if !ok {
		return nil, ErrNotFound
	}
	return n, nil
}

// FindByIDWithUser returns the stored note as-is (User pointer that the
// caller set is preserved). Renote / Reply の embed は行わず、軽量経路 (#425)
// として visibility / userId 参照だけが必要な caller 用。
func (m *MockNoteRepository) FindByIDWithUser(id string) (*model.Note, error) {
	return m.FindByID(id)
}

func (m *MockNoteRepository) FindByIDWithUserOnPrimary(id string) (*model.Note, error) {
	return m.FindByIDWithUser(id)
}

// FindByIDWithRelations mirrors the production hydrateNoteRelations behavior:
// shallow-copy the stored note and embed Renote / Reply targets from the map
// when their IDs are set (#425)。コピーに書き込むので、同じ note を
// FindByID で取り直した時に relation が連鎖的に残らない。
func (m *MockNoteRepository) FindByIDWithRelations(id string) (*model.Note, error) {
	n, err := m.FindByID(id)
	if err != nil {
		return nil, err
	}
	out := *n
	if out.Renote == nil && out.RenoteID != nil {
		if target, ok := m.Notes[*out.RenoteID]; ok {
			out.Renote = target
		}
	}
	if out.Reply == nil && out.ReplyID != nil {
		if target, ok := m.Notes[*out.ReplyID]; ok {
			out.Reply = target
		}
	}
	return &out, nil
}

func (m *MockNoteRepository) FindByIDWithRelationsOnPrimary(id string) (*model.Note, error) {
	return m.FindByIDWithRelations(id)
}

func (m *MockNoteRepository) FindByURI(uri string) (*model.Note, error) {
	// FindByURIErr は not-found でない error を返させる口 (#3115)。
	if m.FindByURIErr != nil {
		return nil, m.FindByURIErr
	}
	for _, n := range m.Notes {
		if n.URI != nil && *n.URI == uri {
			return n, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockNoteRepository) Delete(note *model.Note) error {
	delete(m.Notes, note.ID)
	return nil
}

func (m *MockNoteRepository) Update(note *model.Note, column string, value any) error {
	return nil
}

// UpdateFields applies field updates to the in-memory note. テストで参照される
// 列だけを反映する; 拡張時はここに追記する。
func (m *MockNoteRepository) UpdateFields(noteID string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	n, ok := m.Notes[noteID]
	if !ok {
		return ErrNotFound
	}
	m.UpdateFieldsCalls = append(m.UpdateFieldsCalls, fields)
	for k, v := range fields {
		switch k {
		case "text":
			if s, ok := v.(*string); ok {
				n.Text = s
			}
		case "cw":
			if s, ok := v.(*string); ok {
				n.CW = s
			}
		case "mentions":
			if a, ok := v.([]string); ok {
				n.Mentions = a
			}
		}
	}
	return nil
}

// IncrementCount mutates the in-memory note's counter column for tests.
func (m *MockNoteRepository) IncrementCount(noteID, column string, delta int) error {
	n, ok := m.Notes[noteID]
	if !ok {
		return ErrNotFound
	}
	switch column {
	case "renoteCount":
		n.RenoteCount = floorZero(int(n.RenoteCount) + delta)
	case "repliesCount":
		n.RepliesCount = floorZero(int(n.RepliesCount) + delta)
	case "clippedCount":
		n.ClippedCount = floorZero(int(n.ClippedCount) + delta)
	}
	return nil
}

// floorZero mirrors the real repository's IncrementCount, which never takes a
// counter below 0.
func floorZero(v int) int16 {
	if v < 0 {
		return 0
	}
	return int16(v)
}

// IncrementReaction adjusts an in-memory reaction count map.
// テスト用に Reactions を JSON Map にデコードして加算する。
func (m *MockNoteRepository) IncrementReaction(noteID, reaction string, delta int) error {
	n, ok := m.Notes[noteID]
	if !ok {
		return ErrNotFound
	}
	if m.ReactionCounts == nil {
		m.ReactionCounts = make(map[string]map[string]int)
	}
	if m.ReactionCounts[noteID] == nil {
		m.ReactionCounts[noteID] = make(map[string]int)
	}
	c := m.ReactionCounts[noteID][reaction] + delta
	if c <= 0 {
		delete(m.ReactionCounts[noteID], reaction)
	} else {
		m.ReactionCounts[noteID][reaction] = c
	}
	_ = n // 実際のJSONBは更新しない
	return nil
}

// ListRenotesOf returns notes whose renoteId equals noteID.
func (m *MockNoteRepository) ListRenotesOf(noteID, viewerID, untilID, sinceID string, limit int) ([]*model.Note, error) {
	return m.listFiltered(func(n *model.Note) bool {
		// viewer 視点の可視性を LIMIT 前に絞る (#1500、real repo の SQL push-down 相当)。
		if !m.canViewerSeeNote(viewerID, n) {
			return false
		}
		return n.RenoteID != nil && *n.RenoteID == noteID
	}, untilID, sinceID, limit), nil
}

// ListRenoteOrReplyRemoteUserIDs returns the distinct ids of remote users who
// renoted or replied to noteID (#2995)。`note.userHost` が非 NULL の行だけを見る
// のは本物の SQL と同じ。
func (m *MockNoteRepository) ListRenoteOrReplyRemoteUserIDs(noteID string) ([]string, error) {
	if m.ListRenoteOrReplyErr != nil {
		return nil, m.ListRenoteOrReplyErr
	}
	if noteID == "" {
		return nil, nil
	}
	seen := map[string]struct{}{}
	var ids []string
	for _, n := range m.Notes {
		if n == nil || n.UserHost == nil || *n.UserHost == "" {
			continue
		}
		if (n.RenoteID == nil || *n.RenoteID != noteID) && (n.ReplyID == nil || *n.ReplyID != noteID) {
			continue
		}
		if _, dup := seen[n.UserID]; dup {
			continue
		}
		seen[n.UserID] = struct{}{}
		ids = append(ids, n.UserID)
	}
	return ids, nil
}

// ListRepliesOf returns notes whose replyId equals noteID.
func (m *MockNoteRepository) ListRepliesOf(noteID, viewerID, untilID, sinceID string, limit int) ([]*model.Note, error) {
	return m.listFiltered(func(n *model.Note) bool {
		if !m.canViewerSeeNote(viewerID, n) {
			return false
		}
		return n.ReplyID != nil && *n.ReplyID == noteID
	}, untilID, sinceID, limit), nil
}

// ListChildrenOf returns notes that reply to or quote the given noteID.
// upstream notes/children (children.ts:53-67) に合わせ、renote 側は本文・ファイル・
// 投票のいずれかを持つ引用 renote のみ child として返す。pure renote (Text==nil かつ
// FileIDs 空かつ HasPoll==false) は除外する (#1554)。reply 側にはこのガードを掛けない。
func (m *MockNoteRepository) ListChildrenOf(noteID, viewerID, untilID, sinceID string, limit int) ([]*model.Note, error) {
	return m.listFiltered(func(n *model.Note) bool {
		if !m.canViewerSeeNote(viewerID, n) {
			return false
		}
		if n.ReplyID != nil && *n.ReplyID == noteID {
			return true
		}
		if n.RenoteID != nil && *n.RenoteID == noteID {
			// pure renote 判定は repository note.go の pureRenoteCondSQL
			// (text/cw/fileIds/poll/replyId 全空) と一致させる (#1888)。
			isPureRenote := n.Text == nil && n.CW == nil && len(n.FileIDs) == 0 && !n.HasPoll && n.ReplyID == nil
			return !isPureRenote
		}
		return false
	}, untilID, sinceID, limit), nil
}

// SearchByFilter returns public/home notes matching the filter (lower() + LIKE +
// optional userId / channelId / host)。`Host == "."` はローカル限定。
func (m *MockNoteRepository) SearchByFilter(f model.NoteSearchFilter) ([]*model.Note, error) {
	q := strings.ToLower(f.Query)
	limit := f.Limit
	if limit <= 0 {
		limit = 10
	}
	return m.listFiltered(func(n *model.Note) bool {
		if n.Visibility != model.NoteVisibilityPublic && n.Visibility != model.NoteVisibilityHome {
			return false
		}
		if n.Text == nil {
			return false
		}
		if !strings.Contains(strings.ToLower(*n.Text), q) {
			return false
		}
		// upstream searchNoteByLike は userId 優先の排他 (userId があれば channelId は
		// 無視)。real repo (SearchByFilter) の else-if と挙動を揃える (#1938)。
		if f.UserID != "" {
			if n.UserID != f.UserID {
				return false
			}
		} else if f.ChannelID != "" {
			if n.ChannelID == nil || *n.ChannelID != f.ChannelID {
				return false
			}
		}
		if f.Host != "" {
			if f.Host == "." {
				if n.UserHost != nil {
					return false
				}
			} else {
				if n.UserHost == nil || *n.UserHost != f.Host {
					return false
				}
			}
		}
		if f.LocalUsersOnly && n.UserHost != nil {
			return false
		}
		return true
	}, f.UntilID, f.SinceID, limit), nil
}

// listFiltered iterates the in-memory notes, applies filter, sorts with the
// paginationOrder semantics (ASC when only sinceID is supplied, DESC otherwise),
// and returns up to `limit` entries. #384 対応: mock も production 側の
// ASC-flip 挙動に合わせる。
func (m *MockNoteRepository) listFiltered(filter func(*model.Note) bool, untilID, sinceID string, limit int) []*model.Note {
	var out []*model.Note
	for _, n := range m.Notes {
		if !filter(n) {
			continue
		}
		if untilID != "" && n.ID >= untilID {
			continue
		}
		if sinceID != "" && n.ID <= sinceID {
			continue
		}
		out = append(out, n)
	}
	asc := sinceID != "" && untilID == ""
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if asc {
				if out[i].ID > out[j].ID {
					out[i], out[j] = out[j], out[i]
				}
			} else {
				if out[i].ID < out[j].ID {
					out[i], out[j] = out[j], out[i]
				}
			}
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ListByChannelID mirrors the real repo's visibility push-down for
// channels/timeline (#1440). viewerID="" は匿名 (public/home のみ)。
func (m *MockNoteRepository) ListByChannelID(channelID, viewerID, untilID, sinceID string, limit int) ([]*model.Note, error) {
	return m.listFiltered(func(n *model.Note) bool {
		if n.ChannelID == nil || *n.ChannelID != channelID {
			return false
		}
		return m.canViewerSeeNote(viewerID, n)
	}, untilID, sinceID, limit), nil
}

// ListLocalByChannelID is ListByChannelID limited to local authors
// (`userHost IS NULL`), mirroring the real repo.
func (m *MockNoteRepository) ListLocalByChannelID(channelID, viewerID, untilID, sinceID string, limit int) ([]*model.Note, error) {
	return m.listFiltered(func(n *model.Note) bool {
		if n.ChannelID == nil || *n.ChannelID != channelID || n.UserHost != nil {
			return false
		}
		return m.canViewerSeeNote(viewerID, n)
	}, untilID, sinceID, limit), nil
}

func (m *MockNoteRepository) ListByUserID(userID string, untilID, sinceID string, limit int) ([]*model.Note, error) {
	// listFiltered に委譲して他の List系と同じくASC-flip 挙動を継承する
	// (#405 Devin指摘)。
	return m.listFiltered(func(n *model.Note) bool {
		return n.UserID == userID
	}, untilID, sinceID, limit), nil
}

// ListPublicNotesForFeed mirrors the repository query: renote を除いた
// public / home のノートを新しい順に返す。
func (m *MockNoteRepository) ListPublicNotesForFeed(userID string, limit int) ([]*model.Note, error) {
	return m.listFiltered(func(n *model.Note) bool {
		return n.UserID == userID && n.RenoteID == nil &&
			(n.Visibility == model.NoteVisibilityPublic || n.Visibility == model.NoteVisibilityHome)
	}, "", "", limit), nil
}

func (m *MockNoteRepository) ListPublicByUserID(userID string, untilID, sinceID string, limit int) ([]*model.Note, error) {
	return m.listFiltered(func(n *model.Note) bool {
		return n.UserID == userID && !n.LocalOnly &&
			(n.Visibility == model.NoteVisibilityPublic || n.Visibility == model.NoteVisibilityHome)
	}, untilID, sinceID, limit), nil
}

// ListByUserIDFiltered は ListByUserID に upstream `users/notes` 互換の
// filter 引数を適用した版。production GORM impl と同 logic で predicate に
// 詰めて listFiltered に委譲する (#1021)。bool 4 引数は repository interface
// の signature に整合 (= struct を介さない理由は testutil ↔ repository の
// import cycle 回避)。
func (m *MockNoteRepository) ListByUserIDFiltered(userID, viewerID, untilID, sinceID string, limit int, withFiles, withReplies, withRenotes, withChannelNotes bool) ([]*model.Note, error) {
	return m.listFiltered(func(n *model.Note) bool {
		if n.UserID != userID {
			return false
		}
		// visibility push-down を real repo SQL と揃えて inline 再現する。
		if !m.canViewerSeeNote(viewerID, n) {
			return false
		}
		if withFiles && len(n.FileIDs) == 0 {
			return false
		}
		if !withReplies && n.ReplyID != nil {
			return false
		}
		if !withRenotes {
			// pure renote (text/cw/fileIds/poll/replyId 全空 + renoteId あり) を除外。
			// real SQL pureRenoteCondSQL と一致させる (#1888)。
			if n.RenoteID != nil && n.Text == nil && n.CW == nil && len(n.FileIDs) == 0 && !n.HasPoll && n.ReplyID == nil {
				return false
			}
		}
		if !withChannelNotes && n.ChannelID != nil {
			return false
		}
		return true
	}, untilID, sinceID, limit), nil
}

// canViewerSeeNote replicates the real repo's visibility push-down inline via
// the shared noteVisibleToViewer helper.
func (m *MockNoteRepository) canViewerSeeNote(viewerID string, n *model.Note) bool {
	return noteVisibleToViewer(viewerID, n, m.Following)
}

// ExistingNoteIDsOnPrimary implements repository.NoteRepository (#2719).
//
// mock は単一ストアなのでレプリカ遅延を模さない。production は primary に
// 固定した SELECT で、ここでは Notes に有るかどうかをそのまま返す。
func (m *MockNoteRepository) ExistingNoteIDsOnPrimary(ids []string) ([]string, error) {
	if m.ExistingOnPrimaryErr != nil {
		return nil, m.ExistingOnPrimaryErr
	}
	var out []string
	for _, id := range ids {
		if _, ok := m.Notes[id]; ok {
			out = append(out, id)
		}
	}
	return out, nil
}

func (m *MockNoteRepository) FindManyByIDsWithUser(ids []string) ([]*model.Note, error) {
	out := make([]*model.Note, 0, len(ids))
	for _, id := range ids {
		if n, ok := m.Notes[id]; ok {
			out = append(out, n)
		}
	}
	return out, nil
}

func (m *MockNoteRepository) FindManyLocalReferenceMetadataByIDs(ids []string) ([]*model.Note, error) {
	if m.FindErr != nil {
		return nil, m.FindErr
	}
	return m.FindManyByIDsWithUser(ids)
}

func (m *MockNoteRepository) FindManyByIDsWithUserOnPrimary(ids []string) ([]*model.Note, error) {
	return m.FindManyByIDsWithUser(ids)
}

// mockIsPureRenote mirrors the repository's pure-renote condition.
func mockIsPureRenote(n *model.Note) bool {
	return n.RenoteID != nil && n.Text == nil && n.CW == nil && len(n.FileIDs) == 0 && !n.HasPoll && n.ReplyID == nil
}

func (m *MockNoteRepository) ListFeatured(channelID, untilID string, limit, offset int) ([]*model.Note, error) {
	var result []*model.Note
	for _, n := range m.Notes {
		if string(n.Visibility) != "public" || n.ReplyID != nil || mockIsPureRenote(n) {
			continue
		}
		if channelID != "" {
			if n.ChannelID == nil || *n.ChannelID != channelID {
				continue
			}
		} else if n.ChannelID != nil || n.UserHost != nil {
			continue
		}
		if untilID != "" && n.ID >= untilID {
			continue
		}
		result = append(result, n)
	}
	if limit <= 0 {
		limit = 10
	}
	if untilID == "" && offset > 0 {
		if offset >= len(result) {
			return nil, nil
		}
		end := min(offset+limit, len(result))
		return result[offset:end], nil
	}
	if limit > len(result) {
		limit = len(result)
	}
	return result[:limit], nil
}

// mockFeaturedNotesPerUserPoolSize は repository.FeaturedNotesPerUserPoolSize
// と揃える pool size (#1487 / #1491)。selection 段で engagement DESC top-N を
// 取り、display 段で id DESC + untilID + limit を適用する 2 段構成を mock も
// 同じ semantics で再現する。
//
// production code ファイルから repository を import すると repository の test
// build 経由で import cycle になる (mock_chat_test.go のコメント参照)。よって
// 定数は二重定義のままにし、testutil パッケージの _test.go で repository から
// import した本物と一致しているかを runtime check する
// (featured_pool_size_test.go の TestFeaturedPoolSizeMatchesRepo)。
// drift があれば testutil パッケージ自身のテストが落ちて即座に検出される。
const mockFeaturedNotesPerUserPoolSize = 50

// mockFeaturedNotesPerUserCandidateWindow は
// repository.FeaturedNotesPerUserCandidateWindow と揃える候補の窓 (#3612)。
// 二重定義の理由と drift の検査は mockFeaturedNotesPerUserPoolSize と同じ
// (featured_pool_size_test.go)。
const mockFeaturedNotesPerUserCandidateWindow = 1000

// ListFeaturedByUser mirrors the real repository's 2-stage selection:
// engagement DESC で top-50 を選抜したあと、id DESC でページングする。
// channel 投稿は除外する。real repo の SQL push-down と一致 (#1487 / #1491)。
func (m *MockNoteRepository) ListFeaturedByUser(userID, viewerID, untilID string, limit int) ([]*model.Note, error) {
	if limit <= 0 {
		limit = 10
	}
	// === selection: viewer 視点で見える non-channel note を engagement DESC で
	//     top-N 選抜 ===
	var pool []*model.Note
	for _, n := range m.Notes {
		if n.UserID != userID {
			continue
		}
		if n.ChannelID != nil {
			continue
		}
		if !noteVisibleToViewer(viewerID, n, m.Following) {
			continue
		}
		pool = append(pool, n)
	}
	// 実 repo と同じく、見えるノートの新しい方から窓の件数だけを候補にする (#3612)。
	sort.Slice(pool, func(i, j int) bool {
		return pool[i].ID > pool[j].ID
	})
	if len(pool) > mockFeaturedNotesPerUserCandidateWindow {
		pool = pool[:mockFeaturedNotesPerUserCandidateWindow]
	}
	sort.Slice(pool, func(i, j int) bool {
		ei := pool[i].RenoteCount + pool[i].RepliesCount
		ej := pool[j].RenoteCount + pool[j].RepliesCount
		if ei != ej {
			return ei > ej
		}
		return pool[i].ID > pool[j].ID
	})
	if len(pool) > mockFeaturedNotesPerUserPoolSize {
		pool = pool[:mockFeaturedNotesPerUserPoolSize]
	}
	// === display: id DESC sort → untilID filter → 先頭 limit 件 ===
	sort.Slice(pool, func(i, j int) bool {
		return pool[i].ID > pool[j].ID
	})
	if untilID != "" {
		filtered := pool[:0]
		for _, n := range pool {
			if n.ID < untilID {
				filtered = append(filtered, n)
			}
		}
		pool = filtered
	}
	if len(pool) > limit {
		pool = pool[:limit]
	}
	return pool, nil
}

func (m *MockNoteRepository) ListRenotesByUser(userID, renoteID string) ([]*model.Note, error) {
	notes := make([]*model.Note, 0)
	for _, n := range m.Notes {
		if n.UserID == userID && n.RenoteID != nil && *n.RenoteID == renoteID {
			notes = append(notes, n)
		}
	}
	return notes, nil
}

// ListMentions accepts the `following` flag for signature parity but does not
// filter on it: the mock has no following graph, so the followee restriction
// is exercised by the real-DB repository test instead (#1554)。
func (m *MockNoteRepository) ListMentions(userID, visibility string, following bool, limit int, sinceID, untilID string, ascending bool) ([]*model.Note, error) {
	_ = following
	if limit <= 0 {
		limit = 10
	}
	out := m.listFiltered(func(n *model.Note) bool {
		// viewer (= mention 対象 = userID) が見られる note のみ (#1441)。
		if !m.canViewerSeeNote(userID, n) {
			return false
		}
		// visibility 指定時は exact-match で絞る (#1451)。listFiltered は
		// filter -> sort -> limit 順なので、ここで弾けば SQL push-down 同様に
		// LIMIT 前で絞られ under-fill しない。
		if visibility != "" && string(n.Visibility) != visibility {
			return false
		}
		// TS notes/mentions parity: mentions または visibleUserIds の
		// どちらかに viewer が含まれれば対象 (#1484)。本文 @mention の無い
		// specified DM も visibleUserIds 経由で拾う。
		for _, mention := range n.Mentions {
			if mention == userID {
				return true
			}
		}
		for _, vu := range n.VisibleUserIDs {
			if vu == userID {
				return true
			}
		}
		return false
	}, untilID, sinceID, 0)
	// 本物と同じく、並びは ascending で決めてから limit で切る (#3369)。
	sort.SliceStable(out, func(a, b int) bool {
		if ascending {
			return out[a].ID < out[b].ID
		}
		return out[a].ID > out[b].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *MockNoteRepository) SearchByTag(tagGroups [][]string, viewerID string, limit int, sinceID, untilID string, filter model.NoteSearchTagFilter) ([]*model.Note, error) {
	if limit <= 0 {
		limit = 10
	}
	// 空 tag を除いた有効 group のみ。有効 group が無ければ空。
	groups := make([][]string, 0, len(tagGroups))
	for _, g := range tagGroups {
		clean := make([]string, 0, len(g))
		for _, t := range g {
			if t != "" {
				clean = append(clean, t)
			}
		}
		if len(clean) > 0 {
			groups = append(groups, clean)
		}
	}
	if len(groups) == 0 {
		return []*model.Note{}, nil
	}
	// note.tags が group の全 tag を含むか (AND)、いずれかの group を満たすか (OR)。
	hasAll := func(n *model.Note, g []string) bool {
		for _, want := range g {
			found := false
			for _, t := range n.Tags {
				if t == want {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	return m.listFiltered(func(n *model.Note) bool {
		if !m.canViewerSeeNote(viewerID, n) {
			return false
		}
		// reply/renote/poll/withFiles filter (#1554)。
		if filter.Reply != nil && (n.ReplyID != nil) != *filter.Reply {
			return false
		}
		if filter.Renote != nil && (n.RenoteID != nil) != *filter.Renote {
			return false
		}
		if filter.Poll != nil && n.HasPoll != *filter.Poll {
			return false
		}
		if filter.WithFiles && len(n.FileIDs) == 0 {
			return false
		}
		if filter.LocalUsersOnly && n.UserHost != nil {
			return false
		}
		for _, g := range groups {
			if hasAll(n, g) {
				return true
			}
		}
		return false
	}, untilID, sinceID, limit), nil
}

func (m *MockNoteRepository) ListByFileID(_, _, _ string, _ int) ([]*model.Note, error) {
	return nil, nil
}
func (m *MockNoteRepository) IncrementUserNotesCount(_ string, _ int) error { return nil }

// ListHomeTimeline / ListLocalTimeline / ListGlobalTimeline return public or
// home-visibility notes via the shared listFiltered helper. The mock does not
// reproduce the production timeline filter semantics (follows / userHost /
// muted channels / etc.); only the pagination cursor and sinceID ASC flip
// match production behaviour.
func (m *MockNoteRepository) ListHomeTimeline(_ string, limit int, sinceID, untilID string, _ model.TimelineDBFilter) ([]*model.Note, error) {
	return m.listFiltered(func(n *model.Note) bool {
		return string(n.Visibility) == "public" || string(n.Visibility) == "home"
	}, untilID, sinceID, limit), nil
}

func (m *MockNoteRepository) ListLocalTimeline(limit int, sinceID, untilID string, _ model.TimelineDBFilter) ([]*model.Note, error) {
	return m.listFiltered(func(n *model.Note) bool {
		return string(n.Visibility) == "public" || string(n.Visibility) == "home"
	}, untilID, sinceID, limit), nil
}

func (m *MockNoteRepository) ListGlobalTimeline(limit int, sinceID, untilID string, f model.TimelineDBFilter) ([]*model.Note, error) {
	return m.listFiltered(func(n *model.Note) bool {
		if f.LocalUsersOnly && n.UserHost != nil {
			return false
		}
		return string(n.Visibility) == "public" || string(n.Visibility) == "home"
	}, untilID, sinceID, limit), nil
}

func (m *MockNoteRepository) ListPublicNotes(filter model.PublicNotesFilter, limit int, sinceID, untilID string) ([]*model.Note, error) {
	return m.listFiltered(func(n *model.Note) bool {
		if string(n.Visibility) != "public" || n.LocalOnly {
			return false
		}
		if filter.Local && n.UserHost != nil {
			return false
		}
		if filter.Reply != nil && (*filter.Reply) != (n.ReplyID != nil) {
			return false
		}
		if filter.Renote != nil && (*filter.Renote) != (n.RenoteID != nil) {
			return false
		}
		if filter.WithFiles != nil && (*filter.WithFiles) != (len(n.FileIDs) > 0) {
			return false
		}
		if filter.Poll != nil && (*filter.Poll) != n.HasPoll {
			return false
		}
		return true
	}, untilID, sinceID, limit), nil
}

func (m *MockNoteRepository) DeleteExpiredRemoteNotes(_, _ int) (int64, error) {
	return 0, nil
}

func (m *MockNoteRepository) DeleteExpiredRemoteNotesAfter(_, _ int, _ string) (int64, int, string, error) {
	return 0, 0, "", nil
}

func (m *MockNoteRepository) DeleteByUserBatch(userID string, batchSize int) (int64, error) {
	if batchSize <= 0 {
		batchSize = 100
	}
	n := int64(0)
	for id, note := range m.Notes {
		if n >= int64(batchSize) {
			break
		}
		if note.UserID == userID {
			delete(m.Notes, id)
			n++
		}
	}
	return n, nil
}

// ListByUserList mirrors the real repository's SQL push-down for user-list
// timeline (#1452 visibility / #1496 withReplies / #1498 renote / WithFiles).
// Caller seeds UserListMembers[listID] = []*model.UserListMembership{...}.
//
// 旧 stub は (nil, nil) を返していて real SQL との parity drift があり、
// handler-layer 以外で visibility / withReplies / withRenotes / withFiles の
// regression を mock 経由で検出できなかった (#1491 audit 指摘 1)。本実装で
// real repo と同 semantics を再現する。
//
// 適用順:
//  1. listID member でない author の note は除外
//  2. channel note は除外
//  3. visibility push-down: public/home は常時、followers は author 本人か
//     viewer が follow 済みのときのみ、specified は list timeline に出さない
//     (DM 非表示, ListByUserList の real SQL も同じ)
//  4. reply gate (#1496): replyId IS NULL || replyUserId==note.userId
//     (self-thread) || replyUserId==viewerID || membership.WithReplies
//  5. sinceID / untilID cursor
//  6. WithFiles (= ファイル添付必須) / WithRenotes nil=true (false なら pure
//     renote 除外) / IncludeMyRenotes・IncludeRenotedMyNotes・IncludeLocalRenotes
//     (いずれも nil=true、false で対応する pure renote 分岐を除外) を
//     applyTimelineFilter と同じ条件で適用
//  7. paginationOrder: sinceID only → ASC, otherwise DESC
//
// 未実装 (= parity 対象外): MutedChannelIDs / muting subquery (UseMutingSubquery /
// MutedUserIDs)。user-list-timeline では real repo もこれらを積まない方針
// なので mock も省略する。これらを使う test は専用 fake (userListNotesRepo 系)
// で受けること。
//
// muting subquery 系 filter が渡された場合は panic で loud-fail させる (#1506)。
// docstring の案内だけでは将来の test 著者が読まずに mock 経由で書く可能性が
// あり、その場合 real SQL では mute されるはずの note が mock で素通りする
// silent regression を引き起こす。stacktrace から専用 fake への escalation を
// 即座に促すために panic を選択している。
func (m *MockNoteRepository) ListByUserList(listID string, limit int, sinceID, untilID string, filter model.TimelineDBFilter) ([]*model.Note, error) {
	if filter.UseMutingSubquery || len(filter.MutedUserIDs) > 0 || len(filter.MutedChannelIDs) > 0 {
		panic("testutil.MockNoteRepository.ListByUserList: muting subquery filter " +
			"(UseMutingSubquery / MutedUserIDs / MutedChannelIDs) is not implemented in this mock. " +
			"Escalate to a dedicated fake such as userListNotesRepo in internal/api/notes/handler_extra_test.go, " +
			"or exercise the real SQL push-down in internal/repository/note.go applyTimelineFilter via a DB-backed test (#1506).")
	}
	if limit <= 0 {
		limit = 10
	}
	members := m.UserListMembers[listID]
	if len(members) == 0 {
		return nil, nil
	}
	memberOf := make(map[string]*model.UserListMembership, len(members))
	for _, mem := range members {
		memberOf[mem.UserID] = mem
	}
	viewerID := filter.ViewerID
	withRenotes := true
	if filter.WithRenotes != nil {
		withRenotes = *filter.WithRenotes
	}
	var result []*model.Note
	for _, n := range m.Notes {
		mem, ok := memberOf[n.UserID]
		if !ok {
			continue
		}
		if n.ChannelID != nil {
			continue
		}
		// visibility: list timeline は specified を出さない (real SQL も同じ)。
		switch n.Visibility {
		case model.NoteVisibilityPublic, model.NoteVisibilityHome:
			// always visible
		case model.NoteVisibilityFollowers:
			if viewerID == "" {
				continue
			}
			if viewerID != n.UserID {
				// #2106 N27: visibleUserIds / mentions に含まれる、または reply 先が
				// viewer の followers note は閲覧可 (CanSeeNote / real SQL と整合)。
				visible := false
				for _, id := range n.VisibleUserIDs {
					if id == viewerID {
						visible = true
						break
					}
				}
				if !visible {
					for _, id := range n.Mentions {
						if id == viewerID {
							visible = true
							break
						}
					}
				}
				if !visible && n.ReplyUserID != nil && *n.ReplyUserID == viewerID {
					visible = true
				}
				if !visible {
					for _, fid := range m.Following[viewerID] {
						if fid == n.UserID {
							visible = true
							break
						}
					}
				}
				if !visible {
					continue
				}
			}
		default:
			// specified / その他 (= 未知) は drop。
			continue
		}
		// reply gate (#1496)
		if n.ReplyID != nil {
			allowed := false
			if n.ReplyUserID != nil {
				if *n.ReplyUserID == n.UserID {
					allowed = true
				} else if viewerID != "" && *n.ReplyUserID == viewerID {
					allowed = true
				}
			}
			if !allowed && mem.WithReplies {
				allowed = true
			}
			if !allowed {
				continue
			}
		}
		if sinceID != "" && n.ID <= sinceID {
			continue
		}
		if untilID != "" && n.ID >= untilID {
			continue
		}
		if filter.WithFiles && len(n.FileIDs) == 0 {
			continue
		}
		// pure renote 判定: real SQL pureRenoteCondSQL (renoteId IS NOT NULL AND
		// text/cw/fileIds/poll/replyId 全空) と一致させる (#1888)。
		isPureRenote := n.RenoteID != nil && n.Text == nil && n.CW == nil && len(n.FileIDs) == 0 && !n.HasPoll && n.ReplyID == nil
		if !withRenotes && isPureRenote {
			// pure renote (= boost) excluded; quote renote (text or files あり) は通る。
			continue
		}
		// IncludeMyRenotes nil=true。false かつ pure renote かつ author == viewer
		// (= 自分の renote) を除外 (real SQL: note.userId = viewerID, #1498)。
		if filter.IncludeMyRenotes != nil && !*filter.IncludeMyRenotes && viewerID != "" &&
			isPureRenote && n.UserID == viewerID {
			continue
		}
		// IncludeRenotedMyNotes nil=true。false かつ pure renote かつ
		// renoteUserId == viewer (= 自分の note が renote された) を除外。
		if filter.IncludeRenotedMyNotes != nil && !*filter.IncludeRenotedMyNotes && viewerID != "" &&
			isPureRenote && n.RenoteUserID != nil && *n.RenoteUserID == viewerID {
			continue
		}
		// IncludeLocalRenotes nil=true。false かつ pure renote かつ
		// renoteUserHost == nil (= local user の renote) を除外。
		if filter.IncludeLocalRenotes != nil && !*filter.IncludeLocalRenotes &&
			isPureRenote && n.RenoteUserHost == nil {
			continue
		}
		result = append(result, n)
	}
	sort.Slice(result, func(i, j int) bool {
		if sinceID != "" && untilID == "" {
			return result[i].ID < result[j].ID
		}
		return result[i].ID > result[j].ID
	})
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (m *MockNoteRepository) CountReplyTargets(userID, viewerID string, limit int) ([]model.ReplyTargetCount, error) {
	if limit <= 0 {
		limit = 10
	}
	counts := make(map[string]int64)
	for _, n := range m.Notes {
		if n.UserID != userID || n.ReplyID == nil || n.ReplyUserID == nil {
			continue
		}
		if *n.ReplyUserID == userID {
			continue
		}
		// visibility push-down: viewer が見られない note は集計しない (#1486)。
		if !noteVisibleToViewer(viewerID, n, m.Following) {
			continue
		}
		counts[*n.ReplyUserID]++
	}
	rows := make([]model.ReplyTargetCount, 0, len(counts))
	for uid, c := range counts {
		rows = append(rows, model.ReplyTargetCount{UserID: uid, Count: c})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Count > rows[j].Count })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

// CountLocalNotes counts notes whose author is local (UserHost nil).
func (m *MockNoteRepository) CountLocalNotes() (int64, error) {
	var n int64
	for _, note := range m.Notes {
		if note.UserHost == nil {
			n++
		}
	}
	return n, nil
}

// CountLocalComments counts local notes that are replies.
func (m *MockNoteRepository) CountLocalComments() (int64, error) {
	var n int64
	for _, note := range m.Notes {
		if note.UserHost == nil && note.ReplyID != nil {
			n++
		}
	}
	return n, nil
}

// MockNoteFavoriteRepository is a test double for repository.NoteFavoriteRepository.
type MockNoteFavoriteRepository struct {
	Favorites map[string]*model.NoteFavorite // keyed by "userId:noteId"
}

func NewMockNoteFavoriteRepository() *MockNoteFavoriteRepository {
	return &MockNoteFavoriteRepository{Favorites: make(map[string]*model.NoteFavorite)}
}

func (m *MockNoteFavoriteRepository) Create(f *model.NoteFavorite) error {
	m.Favorites[f.UserID+":"+f.NoteID] = f
	return nil
}

func (m *MockNoteFavoriteRepository) Delete(userID, noteID string) error {
	delete(m.Favorites, userID+":"+noteID)
	return nil
}

func (m *MockNoteFavoriteRepository) Exists(userID, noteID string) (bool, error) {
	_, ok := m.Favorites[userID+":"+noteID]
	return ok, nil
}

func (m *MockNoteFavoriteRepository) ListByUser(userID, untilID, sinceID string, limit int) ([]*model.NoteFavorite, error) {
	var result []*model.NoteFavorite
	for _, f := range m.Favorites {
		if f.UserID != userID {
			continue
		}
		// keyset cursor を mock 側でも正しく適用しないと、handler の
		// pagination 動作テストが repo 全件返却で偽陽性になる (#424)。
		if untilID != "" && f.ID >= untilID {
			continue
		}
		if sinceID != "" && f.ID <= sinceID {
			continue
		}
		result = append(result, f)
	}
	// id DESC (untilID 経路) / ASC (sinceID-only 経路) を実 repo と揃える。
	sort.Slice(result, func(i, j int) bool {
		if sinceID != "" && untilID == "" {
			return result[i].ID < result[j].ID
		}
		return result[i].ID > result[j].ID
	})
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	if len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

// MockNoteReactionRepository is a test double for repository.NoteReactionRepository.
type MockNoteReactionRepository struct {
	Reactions map[string]*model.NoteReaction // keyed by id
	CreateErr error                          // optional error to return on Create
	// Following は ListByUserID の visibility filter (followers note の follow
	// 判定) に使う followerID -> followeeIDs map。未設定なら follow なし扱い
	// (= 非 follower viewer)。testutil は repository を import すると repository
	// 内部テストとの循環になるため、core/note.CanSeeNote は使わず inline で
	// 可視性を再現する (条件は CanSeeNote と一致させる)。
	Following map[string][]string
}

func NewMockNoteReactionRepository() *MockNoteReactionRepository {
	return &MockNoteReactionRepository{Reactions: make(map[string]*model.NoteReaction)}
}

func (m *MockNoteReactionRepository) Create(r *model.NoteReaction) error {
	if m.CreateErr != nil {
		return m.CreateErr
	}
	m.Reactions[r.ID] = r
	return nil
}

func (m *MockNoteReactionRepository) Delete(r *model.NoteReaction) (int64, error) {
	if _, ok := m.Reactions[r.ID]; !ok {
		return 0, nil
	}
	delete(m.Reactions, r.ID)
	return 1, nil
}

func (m *MockNoteReactionRepository) FindByPair(userID, noteID string) (*model.NoteReaction, error) {
	for _, r := range m.Reactions {
		if r.UserID == userID && r.NoteID == noteID {
			return r, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockNoteReactionRepository) FindByUserAndNoteIDs(userID string, noteIDs []string) (map[string]*model.NoteReaction, error) {
	result := make(map[string]*model.NoteReaction)
	noteIDSet := make(map[string]bool, len(noteIDs))
	for _, id := range noteIDs {
		noteIDSet[id] = true
	}
	for _, r := range m.Reactions {
		if r.UserID == userID && noteIDSet[r.NoteID] {
			result[r.NoteID] = r
		}
	}
	return result, nil
}

func (m *MockNoteReactionRepository) ListByNoteID(noteID, untilID, sinceID string, limit int, reactions []string) ([]*model.NoteReaction, error) {
	var rows []*model.NoteReaction
	for _, r := range m.Reactions {
		if r.NoteID != noteID {
			continue
		}
		if len(reactions) > 0 {
			matched := false
			for _, rx := range reactions {
				if r.Reaction == rx {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		if untilID != "" && r.ID >= untilID {
			continue
		}
		if sinceID != "" && r.ID <= sinceID {
			continue
		}
		rows = append(rows, r)
	}
	// production の paginationOrder と同じく sinceID単独指定時のみ ASC、
	// それ以外は DESC (#405 Devin指摘)。
	asc := sinceID != "" && untilID == ""
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if asc {
				if rows[i].ID > rows[j].ID {
					rows[i], rows[j] = rows[j], rows[i]
				}
			} else {
				if rows[i].ID < rows[j].ID {
					rows[i], rows[j] = rows[j], rows[i]
				}
			}
		}
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

// ListReactionPairs returns up to `limit` "<userId>/<reaction>" pairs for a note,
// oldest (smallest id) first, mirroring the production repo (#2067)。
func (m *MockNoteReactionRepository) ListReactionPairs(noteID string, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	var matched []*model.NoteReaction
	for _, r := range m.Reactions {
		if r.NoteID == noteID {
			matched = append(matched, r)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].ID < matched[j].ID })
	if len(matched) > limit {
		matched = matched[:limit]
	}
	pairs := make([]string, 0, len(matched))
	for _, r := range matched {
		pairs = append(pairs, r.UserID+"/"+r.Reaction)
	}
	return pairs, nil
}

// ListByUserID returns reactions made by a user. paginationOrder と同じく
// sinceID 単独指定時のみ ASC、それ以外 DESC (ListByNoteID と同 logic)。
func (m *MockNoteReactionRepository) ListByUserID(userID, viewerID, untilID, sinceID string, limit int) ([]*model.NoteReaction, error) {
	var rows []*model.NoteReaction
	for _, r := range m.Reactions {
		if r.UserID != userID {
			continue
		}
		// real repo の EXISTS visibility subquery を inline 再現する (LIMIT 前に
		// 絞ることで viewer が見られない note の reaction を除外)。
		if !m.canViewerSeeNote(viewerID, r.Note) {
			continue
		}
		if untilID != "" && r.ID >= untilID {
			continue
		}
		if sinceID != "" && r.ID <= sinceID {
			continue
		}
		rows = append(rows, r)
	}
	asc := sinceID != "" && untilID == ""
	for i := 0; i < len(rows); i++ {
		for j := i + 1; j < len(rows); j++ {
			if asc {
				if rows[i].ID > rows[j].ID {
					rows[i], rows[j] = rows[j], rows[i]
				}
			} else {
				if rows[i].ID < rows[j].ID {
					rows[i], rows[j] = rows[j], rows[i]
				}
			}
		}
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

// canViewerSeeNote replicates the real repo's visibility EXISTS subquery
// inline. 条件は core/note.CanSeeNote / real repo の SQL と一致させる
// (public/home は全員、followers は author 本人または follow 済み、specified は
// author 本人または visibleUserIds に含まれる)。n == nil は real repo の EXISTS
// が note 行を要求して false になるのと揃えて除外する (production は FK + Preload
// で note が必ず存在するためこの分岐は起きない。test は note を必ず seed する)。
func (m *MockNoteReactionRepository) canViewerSeeNote(viewerID string, n *model.Note) bool {
	return noteVisibleToViewer(viewerID, n, m.Following)
}

// noteVisibleToViewer replicates core/note.CanSeeNote / the real repo の
// visibility SQL を inline で再現する共通ヘルパー。testutil は core/note を
// import すると repository 内部テストとの循環になるためロジックを複製する
// (条件は CanSeeNote と一致させる)。following は followerID -> followeeIDs。
func noteVisibleToViewer(viewerID string, n *model.Note, following map[string][]string) bool {
	if n == nil {
		return false
	}
	switch n.Visibility {
	case model.NoteVisibilityPublic, model.NoteVisibilityHome:
		return true
	}
	if viewerID == "" {
		return false
	}
	// author 本人は followers/specified を問わず閲覧可。
	if viewerID == n.UserID {
		return true
	}
	// #2106 N27: visibility 横断で mentions / visibleUserIds に含まれる viewer は閲覧可
	// (CanSeeNote の N27 緩和と整合させる drift detector #1507)。
	for _, id := range n.Mentions {
		if id == viewerID {
			return true
		}
	}
	for _, id := range n.VisibleUserIDs {
		if id == viewerID {
			return true
		}
	}
	switch n.Visibility {
	case model.NoteVisibilityFollowers:
		// #2106 N27: reply 先が viewer の followers note は閲覧可。
		if n.ReplyUserID != nil && *n.ReplyUserID == viewerID {
			return true
		}
		for _, followee := range following[viewerID] {
			if followee == n.UserID {
				return true
			}
		}
		return false
	case model.NoteVisibilitySpecified:
		// visibleUserIds は上で判定済。
		return false
	}
	return false
}

// MockUserNotePiningRepository is a test double for repository.UserNotePiningRepository.
type MockUserNotePiningRepository struct {
	Pinings map[string]*model.UserNotePining // keyed by ID
	// ReplaceErr forces ReplaceByUser to fail, for exercising the
	// best-effort paths that must not abort actor resolution.
	ReplaceErr error
	// CountErr forces CountByUser to fail, for exercising the paths that must
	// not silently skip the pin cap when the count is unavailable.
	CountErr error
	// FindErr forces FindByPair to fail with a non-not-found error, for
	// exercising the paths that must retry instead of acking (#3115).
	FindErr error
	// DeleteErr forces Delete to fail, for exercising the paths that must not
	// swallow the write failure after deciding to unpin (#3115).
	DeleteErr error
}

func NewMockUserNotePiningRepository() *MockUserNotePiningRepository {
	return &MockUserNotePiningRepository{Pinings: make(map[string]*model.UserNotePining)}
}

func (m *MockUserNotePiningRepository) Create(p *model.UserNotePining) error {
	m.Pinings[p.ID] = p
	return nil
}

func (m *MockUserNotePiningRepository) Delete(p *model.UserNotePining) error {
	if m.DeleteErr != nil {
		return m.DeleteErr
	}
	delete(m.Pinings, p.ID)
	return nil
}

func (m *MockUserNotePiningRepository) FindByPair(userID, noteID string) (*model.UserNotePining, error) {
	if m.FindErr != nil {
		return nil, m.FindErr
	}
	for _, p := range m.Pinings {
		if p.UserID == userID && p.NoteID == noteID {
			return p, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockUserNotePiningRepository) ListByUser(userID string) ([]*model.UserNotePining, error) {
	var rows []*model.UserNotePining
	for _, p := range m.Pinings {
		if p.UserID == userID {
			rows = append(rows, p)
		}
	}
	// 実装 (repository/user_note_pining.go) は ORDER BY id DESC を使っている。
	// map 反復は非決定的なので、同じ順序でソートして将来の順序依存テストに
	// 備える。
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID > rows[j].ID })
	return rows, nil
}

// ListByUsers implements repository.UserNotePiningBatchReader.
func (m *MockUserNotePiningRepository) ListByUsers(userIDs []string) ([]*model.UserNotePining, error) {
	want := make(map[string]bool, len(userIDs))
	for _, id := range userIDs {
		want[id] = true
	}
	var rows []*model.UserNotePining
	for _, p := range m.Pinings {
		if want[p.UserID] {
			rows = append(rows, p)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID > rows[j].ID })
	return rows, nil
}

func (m *MockUserNotePiningRepository) ReplaceByUser(userID string, pins []*model.UserNotePining) error {
	if m.ReplaceErr != nil {
		return m.ReplaceErr
	}
	for key, p := range m.Pinings {
		if p.UserID == userID {
			delete(m.Pinings, key)
		}
	}
	for _, p := range pins {
		m.Pinings[p.ID] = p
	}
	return nil
}

func (m *MockUserNotePiningRepository) CountByUser(userID string) (int, error) {
	if m.CountErr != nil {
		return 0, m.CountErr
	}
	count := 0
	for _, p := range m.Pinings {
		if p.UserID == userID {
			count++
		}
	}
	return count, nil
}

// MockPollRepository is a test double for repository.PollRepository.
type MockPollRepository struct {
	Polls map[string]*model.Poll
	// FindErr, when non-nil, is returned by FindByNoteID. not-found ではない
	// 障害を ack していないことを試すために要る (#3116)。
	FindErr error
}

func NewMockPollRepository() *MockPollRepository {
	return &MockPollRepository{Polls: make(map[string]*model.Poll)}
}

func (m *MockPollRepository) Create(poll *model.Poll) error {
	m.Polls[poll.NoteID] = poll
	return nil
}

func (m *MockPollRepository) FindByNoteID(noteID string) (*model.Poll, error) {
	if m.FindErr != nil {
		return nil, m.FindErr
	}
	p, ok := m.Polls[noteID]
	if !ok {
		return nil, ErrNotFound
	}
	return p, nil
}

func (m *MockPollRepository) IncrementVote(noteID string, choice int, delta int) error {
	p, ok := m.Polls[noteID]
	if !ok {
		return ErrNotFound
	}
	if choice < 0 || choice >= len(p.Votes) {
		return nil
	}
	p.Votes[choice] += int64(delta)
	return nil
}

// UpdateVotes overwrites the poll's votes array (#1779)。
func (m *MockPollRepository) UpdateVotes(noteID string, votes []int64) error {
	p, ok := m.Polls[noteID]
	if !ok {
		return ErrNotFound
	}
	p.Votes = append(p.Votes[:0:0], votes...)
	return nil
}

// ListExpiredUnnotified mirrors the live repo: returns polls with expiresAt
// before `now` and notifiedAt == nil. ordering by expiresAt ASC for
// deterministic testing.
func (m *MockPollRepository) ListExpiredUnnotified(now time.Time, limit int) ([]*model.Poll, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows []*model.Poll
	for _, p := range m.Polls {
		if p.ExpiresAt != nil && p.ExpiresAt.Before(now) && p.NotifiedAt == nil {
			rows = append(rows, p)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].ExpiresAt.Before(*rows[j].ExpiresAt)
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

// MarkNotified stamps notifiedAt on the in-memory poll. unknown noteID is a
// no-op (matches GORM's UPDATE WHERE behaviour for non-matching rows).
func (m *MockPollRepository) MarkNotified(noteID string, t time.Time) error {
	if p, ok := m.Polls[noteID]; ok {
		p.NotifiedAt = &t
	}
	return nil
}

// VotedNoteIDs lets tests mark which (viewer,noteId) votes exist so
// ListRecommendation can exclude them; keyed by "<userID>:<noteID>".
var pollMockVoted = map[string]struct{}{}

// MockPollVoted records a vote so MockPollRepository.ListRecommendation excludes
// the note for that viewer. Test helper.
func MockPollVoted(userID, noteID string) { pollMockVoted[userID+":"+noteID] = struct{}{} }

// ListRecommendation mirrors the live query in-memory: local public unexpired
// polls not authored by / voted by the viewer, author not muted; when
// excludeChannels is true all channel polls are dropped; ordered by noteId
// DESC; limit/offset applied.
func (m *MockPollRepository) ListRecommendation(viewerID string, mutedUserIDs []string, excludeChannels bool, limit, offset int) ([]string, error) {
	if limit <= 0 {
		limit = 10
	}
	muted := make(map[string]struct{}, len(mutedUserIDs))
	for _, id := range mutedUserIDs {
		muted[id] = struct{}{}
	}
	now := time.Now()
	var ids []string
	for _, p := range m.Polls {
		if p.UserHost != nil {
			continue
		}
		if p.UserID == viewerID {
			continue
		}
		if p.NoteVisibility != model.NoteVisibilityPublic {
			continue
		}
		if p.ExpiresAt != nil && !p.ExpiresAt.After(now) {
			continue
		}
		if _, ok := pollMockVoted[viewerID+":"+p.NoteID]; ok {
			continue
		}
		if _, ok := muted[p.UserID]; ok {
			continue
		}
		if excludeChannels && p.ChannelID != nil {
			continue
		}
		ids = append(ids, p.NoteID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] > ids[j] }) // noteId DESC
	if offset > 0 {
		if offset >= len(ids) {
			return nil, nil
		}
		ids = ids[offset:]
	}
	if len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

// MockPollVoteRepository is a test double for repository.PollVoteRepository.
type MockPollVoteRepository struct {
	Votes map[string]*model.PollVote // keyed by id
}

func NewMockPollVoteRepository() *MockPollVoteRepository {
	return &MockPollVoteRepository{Votes: make(map[string]*model.PollVote)}
}

func (m *MockPollVoteRepository) Create(v *model.PollVote) error {
	m.Votes[v.ID] = v
	return nil
}

// CreateIfNotVoted mirrors the live repo's duplicate check (not its locking).
func (m *MockPollVoteRepository) CreateIfNotVoted(v *model.PollVote, single bool) (bool, error) {
	for _, e := range m.Votes {
		if e.UserID == v.UserID && e.NoteID == v.NoteID && (single || e.Choice == v.Choice) {
			return false, nil
		}
	}
	m.Votes[v.ID] = v
	return true, nil
}

func (m *MockPollVoteRepository) FindByUserAndChoice(userID, noteID string, choice int) (*model.PollVote, error) {
	for _, v := range m.Votes {
		if v.UserID == userID && v.NoteID == noteID && v.Choice == choice {
			return v, nil
		}
	}
	return nil, ErrNotFound
}

func (m *MockPollVoteRepository) CountByUserAndNote(userID, noteID string) (int64, error) {
	var n int64
	for _, v := range m.Votes {
		if v.UserID == userID && v.NoteID == noteID {
			n++
		}
	}
	return n, nil
}

func (m *MockPollVoteRepository) ListByNoteID(noteID string) ([]*model.PollVote, error) {
	var rows []*model.PollVote
	for _, v := range m.Votes {
		if v.NoteID == noteID {
			rows = append(rows, v)
		}
	}
	return rows, nil
}

// FindByUserAndNoteIDs mirrors the live repo: groups choices by noteID for the
// given user across the requested noteIDs. Empty noteIDs returns empty map.
func (m *MockPollVoteRepository) FindByUserAndNoteIDs(userID string, noteIDs []string) (map[string][]int, error) {
	out := map[string][]int{}
	if userID == "" || len(noteIDs) == 0 {
		return out, nil
	}
	want := make(map[string]bool, len(noteIDs))
	for _, id := range noteIDs {
		want[id] = true
	}
	for _, v := range m.Votes {
		if v.UserID == userID && want[v.NoteID] {
			out[v.NoteID] = append(out[v.NoteID], v.Choice)
		}
	}
	return out, nil
}

// MockNoteUnreadRepository is a test double for repository.NoteUnreadRepository.
type MockNoteUnreadRepository struct {
	Rows []*model.NoteUnread
}

// NewMockNoteUnreadRepository returns an empty repository.
func NewMockNoteUnreadRepository() *MockNoteUnreadRepository {
	return &MockNoteUnreadRepository{}
}

// Upsert records an unread row. Flags are OR-merged on conflict to mirror the
// real repository's behaviour.
func (m *MockNoteUnreadRepository) Upsert(row *model.NoteUnread) error {
	for _, existing := range m.Rows {
		if existing.UserID == row.UserID && existing.NoteID == row.NoteID {
			existing.IsSpecified = existing.IsSpecified || row.IsSpecified
			existing.IsMentioned = existing.IsMentioned || row.IsMentioned
			return nil
		}
	}
	m.Rows = append(m.Rows, row)
	return nil
}

// HasAnySpecified returns whether any specified-flagged row exists for user.
func (m *MockNoteUnreadRepository) HasAnySpecified(userID string) (bool, error) {
	for _, r := range m.Rows {
		if r.UserID == userID && r.IsSpecified {
			return true, nil
		}
	}
	return false, nil
}

// HasAnyMentioned returns whether any mentioned-flagged row exists for user.
func (m *MockNoteUnreadRepository) HasAnyMentioned(userID string) (bool, error) {
	for _, r := range m.Rows {
		if r.UserID == userID && r.IsMentioned {
			return true, nil
		}
	}
	return false, nil
}

// DeleteByUserNote removes the row matching (userID, noteID), if any.
func (m *MockNoteUnreadRepository) DeleteByUserNote(userID, noteID string) error {
	out := m.Rows[:0]
	for _, r := range m.Rows {
		if r.UserID == userID && r.NoteID == noteID {
			continue
		}
		out = append(out, r)
	}
	m.Rows = out
	return nil
}

// DeleteAllByUser removes every row owned by userID.
func (m *MockNoteUnreadRepository) DeleteAllByUser(userID string) error {
	out := m.Rows[:0]
	for _, r := range m.Rows {
		if r.UserID == userID {
			continue
		}
		out = append(out, r)
	}
	m.Rows = out
	return nil
}

// MockNoteThreadMutingRepository is a test double for repository.NoteThreadMutingRepository.
type MockNoteThreadMutingRepository struct {
	Mutings map[string]*model.NoteThreadMuting // keyed by "userId:threadId"
}

func NewMockNoteThreadMutingRepository() *MockNoteThreadMutingRepository {
	return &MockNoteThreadMutingRepository{Mutings: make(map[string]*model.NoteThreadMuting)}
}

func (m *MockNoteThreadMutingRepository) Create(mut *model.NoteThreadMuting) error {
	m.Mutings[mut.UserID+":"+mut.ThreadID] = mut
	return nil
}

func (m *MockNoteThreadMutingRepository) Delete(userID, threadID string) error {
	delete(m.Mutings, userID+":"+threadID)
	return nil
}

func (m *MockNoteThreadMutingRepository) Exists(userID, threadID string) (bool, error) {
	_, ok := m.Mutings[userID+":"+threadID]
	return ok, nil
}

func (m *MockNoteThreadMutingRepository) ListMutedThreadIDs(userID string) ([]string, error) {
	var ids []string
	for _, mut := range m.Mutings {
		if mut.UserID == userID {
			ids = append(ids, mut.ThreadID)
		}
	}
	return ids, nil
}
