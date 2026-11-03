package processors

import (
	"context"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// orderedGamePruner records when the bulk delete runs relative to the
// federated cancel.
type orderedGamePruner struct {
	log          *[]string
	gotThreshold string
}

func (f *orderedGamePruner) DeleteOutdatedGames(threshold string) (int64, error) {
	*f.log = append(*f.log, "bulk")
	f.gotThreshold = threshold
	return 0, nil
}

// fakeFedCanceler returns listed[i] for the i-th call (the last value repeats)
// and reports notRemoved fewer games as removed.
type fakeFedCanceler struct {
	log        *[]string
	listed     []int
	notRemoved int
	err        error
	errAt      int
	calls      int
	thresholds []string
	limits     []int
}

func (f *fakeFedCanceler) CancelOutdatedFederatedGames(_ context.Context, thresholdID string, limit int) (int, int, error) {
	*f.log = append(*f.log, "fed")
	f.thresholds = append(f.thresholds, thresholdID)
	f.limits = append(f.limits, limit)
	i := f.calls
	f.calls++
	if f.err != nil && i == f.errAt {
		// 失敗したbatchも満杯だったとして返し、続けるかどうかをerrだけで決めさせる。
		return reversiFederatedCancelBatch, 0, f.err
	}
	n := f.listed[len(f.listed)-1]
	if i < len(f.listed) {
		n = f.listed[i]
	}
	return n, n - f.notRemoved, nil
}

func runCleanWithFed(t *testing.T, c *fakeFedCanceler, log *[]string) *orderedGamePruner {
	t.Helper()
	game := &orderedGamePruner{log: log}
	proc := NewCleanProcessor(nil, nil, game, &fakeCleanIDGen{}, nil, 0, nil)
	if c != nil {
		proc.SetOutdatedFederatedGameCanceler(c)
	}
	require.NoError(t, proc.Handle(context.Background(), driver.RawTask{TypeName: "test"}))
	return game
}

// The federated games are notified and removed before the bulk delete, with
// the same threshold, so the bulk delete cannot remove them first (#3956).
func TestClean_CancelsFederatedReversiGamesBeforeBulkDelete(t *testing.T) {
	var log []string
	c := &fakeFedCanceler{log: &log, listed: []int{3}}
	game := runCleanWithFed(t, c, &log)

	assert.Equal(t, []string{"fed", "bulk"}, log)
	assert.Equal(t, []string{"threshold-id"}, c.thresholds)
	assert.Equal(t, "threshold-id", game.gotThreshold)
	assert.Equal(t, []int{reversiFederatedCancelBatch}, c.limits)
}

func TestClean_FederatedReversiCancelBatches(t *testing.T) {
	t.Run("keeps going while batches are full", func(t *testing.T) {
		var log []string
		c := &fakeFedCanceler{log: &log, listed: []int{reversiFederatedCancelBatch, reversiFederatedCancelBatch, 5}}
		runCleanWithFed(t, c, &log)
		assert.Equal(t, 3, c.calls)
		assert.Equal(t, "bulk", log[len(log)-1])
	})

	t.Run("a full batch with games started meanwhile still continues", func(t *testing.T) {
		// 読んだ件数で続けるかを決める。消せた件数で決めると、読んでから消すまでに
		// 始まった対局が1件でもあるだけで、残りを知らせずに一括削除へ回してしまう。
		var log []string
		c := &fakeFedCanceler{log: &log, listed: []int{reversiFederatedCancelBatch, 5}, notRemoved: 1}
		runCleanWithFed(t, c, &log)
		assert.Equal(t, 2, c.calls)
	})

	t.Run("stops at the per-run cap", func(t *testing.T) {
		var log []string
		c := &fakeFedCanceler{log: &log, listed: []int{reversiFederatedCancelBatch}}
		runCleanWithFed(t, c, &log)
		assert.Equal(t, reversiFederatedCancelMaxBatches, c.calls)
		assert.Equal(t, "bulk", log[len(log)-1], "the rest is still removed by the bulk delete")
	})

	t.Run("an error stops the batches but not the bulk delete", func(t *testing.T) {
		var log []string
		c := &fakeFedCanceler{log: &log, listed: []int{reversiFederatedCancelBatch}, err: errors.New("db down"), errAt: 1}
		runCleanWithFed(t, c, &log)
		assert.Equal(t, 2, c.calls)
		assert.Equal(t, []string{"fed", "fed", "bulk"}, log)
	})

	t.Run("unwired canceler keeps the bulk delete only", func(t *testing.T) {
		var log []string
		runCleanWithFed(t, nil, &log)
		assert.Equal(t, []string{"bulk"}, log)
	})
}
