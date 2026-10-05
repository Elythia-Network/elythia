package search

import (
	"time"

	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// rangeStartID / rangeEndID convert a date-range bound (epoch ms) to an aidx
// prefix id boundary, mirroring upstream `idService.gen(rangeStartAt-1)` /
// `gen(rangeEndAt+1)` (#16119 #2069)。±1ms で範囲を inclusive にする。
func rangeStartID(at *int64) string {
	if at == nil {
		return ""
	}
	return id.AidxCutoffPrefix(time.UnixMilli(*at - 1))
}

func rangeEndID(at *int64) string {
	if at == nil {
		return ""
	}
	return id.AidxCutoffPrefix(time.UnixMilli(*at + 1))
}

// SQLLikeProvider is the default backend that delegates to
// `noteRepo.SearchByFilter`. It performs a case-insensitive substring match
// over public/home notes and supports the same filter options as the
// Meilisearch backend (userId / channelId / host).
//
// When pgroonga is true the underlying query uses PGroonga's `&@~` operator
// instead of lower() + LIKE — upstream Misskey TS keeps both providers in the same
// code path and only swaps the WHERE clause.
//
// Index / Unindex are no-ops because the canonical store is the database
// itself; nothing extra has to happen on note creation / deletion.
type SQLLikeProvider struct {
	noteRepo      repository.NoteRepository
	followingRepo repository.FollowingRepository
	pgroonga      bool
}

// NewSQLLikeProvider returns a Provider backed by SearchByFilter on the
// supplied note repository. followingRepo は visibility 後フィルタ
// (note.CanSeeNote) で followers 可視性のチェックに使う。nil 可。
func NewSQLLikeProvider(noteRepo repository.NoteRepository, followingRepo repository.FollowingRepository) *SQLLikeProvider {
	return &SQLLikeProvider{noteRepo: noteRepo, followingRepo: followingRepo}
}

// NewSQLPgroongaProvider returns a Provider that uses the PGroonga `&@~`
// match operator instead of lower() + LIKE. The PGroonga extension must be installed
// on the target database and a pgroonga index must exist on note.text;
// extension installation is the operator's responsibility.
func NewSQLPgroongaProvider(noteRepo repository.NoteRepository, followingRepo repository.FollowingRepository) *SQLLikeProvider {
	return &SQLLikeProvider{noteRepo: noteRepo, followingRepo: followingRepo, pgroonga: true}
}

// IndexNote is a no-op for the SQL backend (the database is the index).
func (p *SQLLikeProvider) IndexNote(_ *model.Note) error { return nil }

// UnindexNote is a no-op for the SQL backend.
func (p *SQLLikeProvider) UnindexNote(_ *model.Note) error { return nil }

// SearchNote runs the configured SQL backend search (lower() + LIKE substring match
// by default, or PGroonga `&@~` when the provider was constructed via
// NewSQLPgroongaProvider) and post-filters the result for viewer visibility.
func (p *SQLLikeProvider) SearchNote(viewer *model.User, query string, opts SearchOpts, page Pagination) ([]*model.Note, error) {
	if query == "" {
		return nil, ErrEmptyQuery
	}
	limit := page.Limit
	if limit <= 0 {
		limit = 10
	}
	// viewer 自身の followers/specified/visibleUserIds note も検索対象に含める
	// よう viewerID を渡す (#1554)。匿名は空文字 → public/home のみ。
	viewerID := ""
	if viewer != nil {
		viewerID = viewer.ID
	}
	rows, err := p.noteRepo.SearchByFilter(model.NoteSearchFilter{
		Query:        query,
		UserID:       opts.UserID,
		ChannelID:    opts.ChannelID,
		Host:         opts.Host,
		UntilID:      page.UntilID,
		SinceID:      page.SinceID,
		RangeStartID: rangeStartID(opts.RangeStartAt),
		RangeEndID:   rangeEndID(opts.RangeEndAt),
		Limit:        limit,
		Offset:       opts.Offset,
		ViewerID:     viewerID,
		Pgroonga:     p.pgroonga,
		// LIMIT の前に絞らないと、ページがリモートのノートで埋まって件数が欠ける。
		LocalUsersOnly: opts.LocalUsersOnly,
	})
	if err != nil {
		return nil, err
	}
	out := make([]*model.Note, 0, len(rows))
	for _, n := range rows {
		if note.CanSeeNote(viewer, n, p.followingRepo) {
			out = append(out, n)
		}
	}
	return out, nil
}
