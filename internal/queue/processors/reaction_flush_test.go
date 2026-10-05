package processors

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/core/reaction"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/require"
)

func TestReactionFlushProcessor_Handle(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	writer := reaction.NewDirectWriter(noteRepo)
	p := NewReactionFlushProcessor(writer)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{TypeName: "test"}))
}
