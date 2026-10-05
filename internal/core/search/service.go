package search

import "github.com/elythia-network/elythia/internal/model"

// Service is the facade that callers (handlers / hooks) interact with.
// It delegates to a single Provider configured at construction time.
type Service struct {
	provider Provider
}

// NewService wires a Service backed by the given Provider. nil provider は
// NoopProvider にフォールバックする (構築側のミスで search 機能がクラッシュ
// するのを防ぐ目的)。
func NewService(provider Provider) *Service {
	if provider == nil {
		provider = NewNoopProvider()
	}
	return &Service{provider: provider}
}

// IndexNote forwards to the underlying provider.
func (s *Service) IndexNote(n *model.Note) error {
	return s.provider.IndexNote(n)
}

// UnindexNote forwards to the underlying provider.
func (s *Service) UnindexNote(n *model.Note) error {
	return s.provider.UnindexNote(n)
}

// SearchNote forwards to the underlying provider after rejecting blank queries.
func (s *Service) SearchNote(viewer *model.User, query string, opts SearchOpts, page Pagination) ([]*model.Note, error) {
	if query == "" {
		return nil, ErrEmptyQuery
	}
	return s.provider.SearchNote(viewer, query, opts, page)
}
