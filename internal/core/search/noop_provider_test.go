package search

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNoopProvider_IndexUnindexAreNoOps(t *testing.T) {
	p := NewNoopProvider()
	require.NoError(t, p.IndexNote(&model.Note{ID: "x"}))
	require.NoError(t, p.UnindexNote(&model.Note{ID: "x"}))
}

// SearchNote on NoopProvider must return ErrUnavailable so the API handler can
// translate it to upstream Misskey TS-compatible 400 UNAVAILABLE (#877)。
func TestNoopProvider_SearchNoteReturnsUnavailable(t *testing.T) {
	p := NewNoopProvider()
	out, err := p.SearchNote(nil, "anything", SearchOpts{UserID: "u1"}, Pagination{Limit: 10})
	require.ErrorIs(t, err, ErrUnavailable)
	assert.Nil(t, out)
}
