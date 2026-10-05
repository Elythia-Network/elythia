package search

import (
	"fmt"
	"sort"
	"strings"

	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// IndexScope determines which notes are sent to the Meilisearch index. The
// shape mirrors Misskey 本家 の `meilisearch.scope` 設定。
type IndexScope string

const (
	// IndexScopeLocal indexes only notes authored by local users (userHost
	// is NULL).  This is the Misskey default.
	IndexScopeLocal IndexScope = "local"
	// IndexScopeGlobal indexes notes from any host, local or remote.
	IndexScopeGlobal IndexScope = "global"
)

// MeilisearchProvider is the production search backend.  It pushes notes
// into a Meilisearch index on creation, removes them on deletion, and
// translates SearchNote calls into a Meilisearch search request followed by
// a database round-trip to fetch the full note rows.
type MeilisearchProvider struct {
	index         MeilisearchIndex
	noteRepo      repository.NoteRepository
	followingRepo repository.FollowingRepository
	idGen         id.Generator
	scope         IndexScope
}

// NewMeilisearchProvider builds a Meilisearch-backed Provider.
//
// idGen は note ID から作成日時 (unix milli) を導出するために使う。
// scope が空文字の場合は IndexScopeLocal にフォールバックする。
func NewMeilisearchProvider(
	index MeilisearchIndex,
	noteRepo repository.NoteRepository,
	followingRepo repository.FollowingRepository,
	idGen id.Generator,
	scope IndexScope,
) *MeilisearchProvider {
	if scope == "" {
		scope = IndexScopeLocal
	}
	return &MeilisearchProvider{
		index:         index,
		noteRepo:      noteRepo,
		followingRepo: followingRepo,
		idGen:         idGen,
		scope:         scope,
	}
}

// ApplyDefaultSettings pushes our preferred index settings to Meilisearch.
// router 起動時に best-effort で呼ぶ想定 (失敗しても起動は続行)。
func (p *MeilisearchProvider) ApplyDefaultSettings() error {
	return p.index.UpdateSettings(IndexSettings{
		SearchableAttributes: []string{"text", "cw"},
		SortableAttributes:   []string{"createdAt"},
		FilterableAttributes: []string{"createdAt", "userId", "userHost", "channelId", "tags"},
		TypoToleranceEnabled: false,
		PaginationMaxHits:    10000,
	})
}

// IndexNote pushes the note into the Meilisearch index when it satisfies the
// configured visibility / scope rules.
func (p *MeilisearchProvider) IndexNote(n *model.Note) error {
	if n == nil {
		return nil
	}
	if !p.shouldIndex(n) {
		return nil
	}
	doc := NoteDocument{
		ID:        n.ID,
		CreatedAt: p.timestampOf(n),
		UserID:    n.UserID,
		UserHost:  n.UserHost,
		ChannelID: n.ChannelID,
		CW:        n.CW,
		Text:      n.Text,
		Tags:      []string(n.Tags),
	}
	return p.index.AddDocuments([]NoteDocument{doc})
}

// UnindexNote removes the note from the Meilisearch index.  We only attempt
// removal for notes whose visibility could plausibly have been indexed
// (public/home) — otherwise the call is a no-op.
func (p *MeilisearchProvider) UnindexNote(n *model.Note) error {
	if n == nil {
		return nil
	}
	if n.Visibility != model.NoteVisibilityPublic && n.Visibility != model.NoteVisibilityHome {
		return nil
	}
	return p.index.DeleteDocument(n.ID)
}

// SearchNote queries the index, then loads the matching notes from the
// database and post-filters by viewer visibility.
func (p *MeilisearchProvider) SearchNote(viewer *model.User, query string, opts SearchOpts, page Pagination) ([]*model.Note, error) {
	if query == "" {
		return nil, ErrEmptyQuery
	}
	limit := page.Limit
	if limit <= 0 {
		limit = 10
	}

	filter := p.buildFilter(opts, page)
	res, err := p.index.Search(query, IndexSearchRequest{
		Filter:               filter,
		Sort:                 []string{"createdAt:desc"},
		Limit:                int64(limit),
		Offset:               int64(opts.Offset),
		AttributesToRetrieve: []string{"id", "createdAt"},
		MatchingStrategy:     "all",
	})
	if err != nil {
		return nil, err
	}
	if res == nil || len(res.Hits) == 0 {
		return nil, nil
	}

	ids := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		ids = append(ids, h.ID)
	}
	rows, err := p.noteRepo.FindManyByIDsWithUser(ids)
	if err != nil {
		return nil, err
	}

	out := make([]*model.Note, 0, len(rows))
	for _, n := range rows {
		if note.CanSeeNote(viewer, n, p.followingRepo) {
			out = append(out, n)
		}
	}
	// FindManyByIDsWithUser preserves the input order (= score order),
	// but visibility filtering may leave gaps; sort id desc to match the
	// SQL backend's ordering for consistency.
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

// shouldIndex applies the visibility / scope / content rules.
func (p *MeilisearchProvider) shouldIndex(n *model.Note) bool {
	if n.Text == nil && n.CW == nil {
		return false
	}
	if n.Visibility != model.NoteVisibilityPublic && n.Visibility != model.NoteVisibilityHome {
		return false
	}
	switch p.scope {
	case IndexScopeGlobal:
		return true
	case IndexScopeLocal:
		return n.UserHost == nil
	default:
		// 未知のスコープ値はローカル限定にフォールバックする (安全側)。
		return n.UserHost == nil
	}
}

// timestampOf returns the unix-millisecond timestamp encoded in the note's
// id, or 0 when the id generator does not support reverse parsing.
func (p *MeilisearchProvider) timestampOf(n *model.Note) int64 {
	if p.idGen == nil {
		return 0
	}
	t, err := p.idGen.ParseTime(n.ID)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// meiliLocalUserClause matches documents authored by local users.
//
// Meilisearch の `IS NULL` は「属性があって値が null」の文書にしか一致せず、
// 属性そのものが無い文書には一致しない (v1.53.1 で実測)。以前の mk-go は
// userHost を omitempty で索引していたので、ローカルの文書は属性を持たない。
// 再索引しなくても既存の文書と TS 版が作った文書 (null で保存) の両方に効く
// よう、`NOT EXISTS` と `IS NULL` の両方を見る。
const meiliLocalUserClause = "(userHost NOT EXISTS OR userHost IS NULL)"

// buildFilter renders a Meilisearch filter expression from the search opts
// and pagination cursors.  Returns an empty string when no filters apply.
func (p *MeilisearchProvider) buildFilter(opts SearchOpts, page Pagination) string {
	var clauses []string
	if t := p.timestampOfID(page.UntilID); t != 0 {
		clauses = append(clauses, fmt.Sprintf("(createdAt < %d)", t))
	}
	if t := p.timestampOfID(page.SinceID); t != 0 {
		clauses = append(clauses, fmt.Sprintf("(createdAt > %d)", t))
	}
	// 投稿日時範囲 (upstream #16119、#2069)。createdAt は epoch ms で index 済なので
	// rangeStartAt/rangeEndAt (epoch ms) と直接比較する。cursor とは独立に AND。
	if opts.RangeStartAt != nil {
		clauses = append(clauses, fmt.Sprintf("(createdAt >= %d)", *opts.RangeStartAt))
	}
	if opts.RangeEndAt != nil {
		clauses = append(clauses, fmt.Sprintf("(createdAt <= %d)", *opts.RangeEndAt))
	}
	if opts.UserID != "" {
		clauses = append(clauses, fmt.Sprintf("(userId = %s)", quoteValue(opts.UserID)))
	}
	if opts.ChannelID != "" {
		clauses = append(clauses, fmt.Sprintf("(channelId = %s)", quoteValue(opts.ChannelID)))
	}
	if opts.Host != "" {
		if opts.Host == "." {
			clauses = append(clauses, meiliLocalUserClause)
		} else {
			clauses = append(clauses, fmt.Sprintf("(userHost = %s)", quoteValue(opts.Host)))
		}
	}
	// upstream searchNoteByMeilisearch は ugcVisibilityForVisitor=local の未ログイン
	// の閲覧者に `userHost IS NULL` を足す。Host 指定とは独立に AND する。
	if opts.LocalUsersOnly {
		clauses = append(clauses, meiliLocalUserClause)
	}
	if len(clauses) == 0 {
		return ""
	}
	return strings.Join(clauses, " AND ")
}

// timestampOfID parses an id back into its unix-millisecond timestamp.
// Returns 0 when the id is empty or cannot be parsed.
func (p *MeilisearchProvider) timestampOfID(id string) int64 {
	if id == "" || p.idGen == nil {
		return 0
	}
	t, err := p.idGen.ParseTime(id)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// quoteValue renders a string value as a single-quoted Meilisearch filter
// literal, escaping backslashes and single quotes with a backslash the same way
// upstream SearchService.compileValue does (misskey-dev/misskey#17991).
func quoteValue(v string) string {
	// Meilisearch のフィルタ構文は SQL と違い `''` を引用符のエスケープとして
	// 解釈しないので、upstream と同じくバックスラッシュでエスケープする。
	// バックスラッシュを先に置換しないと、後で足した `\'` の `\` まで二重化
	// してしまうので順序に意味がある。
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "'", `\'`)
	return "'" + v + "'"
}
