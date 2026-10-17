package server

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/bgwork"
)

// TestPluginContext_GoRunsInTheGenerationGroup checks the plugin side of
// #3661: GoContext receives a context that stopping the generation cancels,
// the stop waits for Go and GoContext, and after the stop neither starts.
func TestPluginContext_GoRunsInTheGenerationGroup(t *testing.T) {
	g := bgwork.New()
	c := &pluginContext{name: "bg-plugin", logger: slog.Default(), background: g}

	ctxs := make(chan context.Context, 1)
	c.GoContext(func(ctx context.Context) {
		ctxs <- ctx
		<-ctx.Done()
	})
	ctx := recvWithin(t, ctxs, "GoContext が走らない")
	require.NoError(t, ctx.Err())

	var slowDone atomic.Bool
	c.Go(func() {
		time.Sleep(100 * time.Millisecond)
		slowDone.Store(true)
	})

	require.Zero(t, g.Stop(context.Background()), "世代の停止が plugin の goroutine を待てなかった")
	assert.Error(t, ctx.Err(), "世代を止めても GoContext の ctx が切れない")
	assert.True(t, slowDone.Load(), "世代の停止が ctx.Go の goroutine を待たなかった")

	var ran atomic.Bool
	c.Go(func() { ran.Store(true) })
	c.GoContext(func(context.Context) { ran.Store(true) })
	time.Sleep(50 * time.Millisecond)
	assert.False(t, ran.Load(), "世代を止めた後に plugin の goroutine が走った")
	assert.EqualValues(t, 2, g.Dropped())
}
