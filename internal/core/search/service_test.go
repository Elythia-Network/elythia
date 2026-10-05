package search

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingProvider counts calls and returns the configured outputs.
type recordingProvider struct {
	indexCalls   int
	unindexCalls int
	searchCalls  int
	indexErr     error
	unindexErr   error
	searchErr    error
	searchOut    []*model.Note
	lastQuery    string
	lastOpts     SearchOpts
	lastPage     Pagination
}

func (p *recordingProvider) IndexNote(_ *model.Note) error {
	p.indexCalls++
	return p.indexErr
}

func (p *recordingProvider) UnindexNote(_ *model.Note) error {
	p.unindexCalls++
	return p.unindexErr
}

func (p *recordingProvider) SearchNote(_ *model.User, q string, opts SearchOpts, page Pagination) ([]*model.Note, error) {
	p.searchCalls++
	p.lastQuery = q
	p.lastOpts = opts
	p.lastPage = page
	return p.searchOut, p.searchErr
}

func TestService_Forwards(t *testing.T) {
	provider := &recordingProvider{
		searchOut: []*model.Note{{ID: "n1"}},
	}
	svc := NewService(provider)

	require.NoError(t, svc.IndexNote(&model.Note{ID: "x"}))
	require.NoError(t, svc.UnindexNote(&model.Note{ID: "x"}))

	out, err := svc.SearchNote(nil, "hello", SearchOpts{UserID: "u1"}, Pagination{Limit: 5})
	require.NoError(t, err)
	assert.Len(t, out, 1)
	assert.Equal(t, 1, provider.indexCalls)
	assert.Equal(t, 1, provider.unindexCalls)
	assert.Equal(t, 1, provider.searchCalls)
	assert.Equal(t, "hello", provider.lastQuery)
	assert.Equal(t, "u1", provider.lastOpts.UserID)
	assert.Equal(t, 5, provider.lastPage.Limit)
}

func TestService_EmptyQueryRejected(t *testing.T) {
	provider := &recordingProvider{}
	svc := NewService(provider)
	_, err := svc.SearchNote(nil, "", SearchOpts{}, Pagination{})
	require.ErrorIs(t, err, ErrEmptyQuery)
	// Provider はクエリが空のとき呼ばれないこと。
	assert.Equal(t, 0, provider.searchCalls)
}

func TestService_PropagatesProviderError(t *testing.T) {
	boom := errors.New("boom")
	provider := &recordingProvider{searchErr: boom, indexErr: boom, unindexErr: boom}
	svc := NewService(provider)

	require.ErrorIs(t, svc.IndexNote(&model.Note{ID: "x"}), boom)
	require.ErrorIs(t, svc.UnindexNote(&model.Note{ID: "x"}), boom)
	_, err := svc.SearchNote(nil, "x", SearchOpts{}, Pagination{Limit: 1})
	require.ErrorIs(t, err, boom)
}

func TestService_NilProviderFallsBackToNoop(t *testing.T) {
	svc := NewService(nil)
	require.NoError(t, svc.IndexNote(&model.Note{ID: "x"}))
	require.NoError(t, svc.UnindexNote(&model.Note{ID: "x"}))
	// nil provider → NoopProvider (#877 で SearchNote は ErrUnavailable を
	// 返すように改修)。caller (handler) 側で 400 UNAVAILABLE に翻訳する
	// 想定。
	_, err := svc.SearchNote(nil, "x", SearchOpts{}, Pagination{Limit: 5})
	require.ErrorIs(t, err, ErrUnavailable)
}
