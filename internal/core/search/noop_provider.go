package search

import "github.com/elythia-network/elythia/internal/model"

// NoopProvider is the Provider used when the operator opts into TS-strict
// drop-in compat (= fulltextSearch.provider="none")。upstream Misskey TS は
// meilisearch / sonic 等の external search backend を必須とし、未配備時は
// notes/search が 400 UNAVAILABLE を返す。NoopProvider はその挙動を mk-go
// で再現するために IndexNote / UnindexNote は no-op で受け、SearchNote は
// ErrUnavailable を返す (#877)。
type NoopProvider struct{}

// NewNoopProvider returns a Provider whose operations all succeed without
// touching any external system. SearchNote だけは ErrUnavailable を返し、
// ハンドラ層で 400 UNAVAILABLE に翻訳される (#877)。
func NewNoopProvider() *NoopProvider { return &NoopProvider{} }

// IndexNote is a no-op.
func (p *NoopProvider) IndexNote(_ *model.Note) error { return nil }

// UnindexNote is a no-op.
func (p *NoopProvider) UnindexNote(_ *model.Note) error { return nil }

// SearchNote returns ErrUnavailable so the handler can translate it to a
// 400 UNAVAILABLE response (= upstream TS と同 shape、#877)。
func (p *NoopProvider) SearchNote(_ *model.User, _ string, _ SearchOpts, _ Pagination) ([]*model.Note, error) {
	return nil, ErrUnavailable
}
