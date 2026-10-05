// Package search provides a backend-agnostic search service for notes.
//
// The package wraps a Provider interface so that callers (HTTP handlers and
// note hooks) do not have to know whether the underlying backend is SQL ILIKE,
// Meilisearch, or a no-op stub.  Provider selection is performed once during
// router setup based on configuration; callers always interact with the
// generic Service facade.
package search

import (
	"errors"

	"github.com/elythia-network/elythia/internal/model"
)

// ErrEmptyQuery is returned by Service / Provider when the caller passes a
// blank search query. ハンドラ層で 400 BadRequest に変換する想定。
var ErrEmptyQuery = errors.New("search query is empty")

// ErrUnavailable is returned by NoopProvider.SearchNote when the operator has
// explicitly disabled note search (= fulltextSearch.provider="none")。
// upstream Misskey TS と同 semantics で、ハンドラ層で 400 UNAVAILABLE
// (id: 0b44998d-77aa-4427-80d0-d2c9b8523011) に変換する想定 (#877)。
var ErrUnavailable = errors.New("search backend is not available")

// NoteDocument is the projection of a model.Note that gets stored in an
// external full-text search index (typically Meilisearch). SQL backends do not
// use this type — they query the live `note` table directly.
type NoteDocument struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"createdAt"` // unix milliseconds
	UserID    string `json:"userId"`
	// UserHost is stored as null for local authors, like upstream. Older
	// mk-go indexes omitted the attribute; buildFilter matches both.
	UserHost  *string  `json:"userHost"`
	ChannelID *string  `json:"channelId,omitempty"`
	CW        *string  `json:"cw,omitempty"`
	Text      *string  `json:"text,omitempty"`
	Tags      []string `json:"tags,omitempty"`
}

// SearchOpts mirrors the optional filters supported by Misskey の searchNote.
// 各文字列フィールドは zero value (空文字) でフィルタ無効を意味する。
// Host == "." はローカル限定 (userHost IS NULL) を表す。
type SearchOpts struct {
	UserID    string
	ChannelID string
	Host      string
	// Offset is the OFFSET-based pagination offset (upstream notes/search.ts:47
	// `offset`). 0 = no offset (#1783)。
	Offset int
	// RangeStartAt / RangeEndAt は投稿日時範囲 (epoch ms、upstream #16119 #2069)。
	// nil = 無制限。sinceId/untilId cursor とは独立に AND される。
	RangeStartAt *int64
	RangeEndAt   *int64
	// LocalUsersOnly restricts results to notes whose own author is local
	// (`userHost IS NULL`), independent of Host. The notes/search handler sets
	// it for anonymous visitors under meta.ugcVisibilityForVisitor=local
	// (upstream SearchService).
	LocalUsersOnly bool
}

// Pagination wraps the keyset pagination parameters used by SearchNote.
type Pagination struct {
	UntilID string
	SinceID string
	Limit   int
}

// Provider is the contract every search backend must satisfy.
//
// IndexNote / UnindexNote are best-effort: callers (note hooks) should not
// surface errors to end users — they exist mainly so that backends can log
// failures and so that tests can verify the call paths.
type Provider interface {
	IndexNote(note *model.Note) error
	UnindexNote(note *model.Note) error
	SearchNote(viewer *model.User, query string, opts SearchOpts, page Pagination) ([]*model.Note, error)
}
