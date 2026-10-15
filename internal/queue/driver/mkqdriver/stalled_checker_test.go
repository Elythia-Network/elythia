package mkqdriver

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func checkerHandle(seq uint64, clk *fakeClock, busyFor time.Duration) *workerHandle {
	h := handleBusyFor(seq, clk, busyFor)
	h.stalledChecker = true
	return h
}

// TestSplitForRemoval_KeepsTheStalledChecker checks that a scale-down stops
// the Worker scanning for stalled jobs only when it stops every Worker
// (#3592), even when it is the trailing idle one.
func TestSplitForRemoval_KeepsTheStalledChecker(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	ws := []*workerHandle{
		handleBusyFor(0, clk, time.Second),
		handleBusyFor(1, clk, 0),
		checkerHandle(2, clk, 0),
	}

	kept, removed := splitForRemoval(ws, 1, 1, true)
	assert.Equal(t, []uint64{1}, seqs(removed), "the idle non-scanner goes first")
	assert.Equal(t, []uint64{0, 2}, seqs(kept))

	kept, removed = splitForRemoval(ws, 2, 2, true)
	assert.Equal(t, []uint64{0, 1}, seqs(removed), "a busy worker goes before the scanner")
	assert.Equal(t, []uint64{2}, seqs(kept))

	// 処理中を止められない (予算 0) ときも、役持ちを代わりに止めない。
	kept, removed = splitForRemoval(ws, 2, 0, true)
	assert.Equal(t, []uint64{1}, seqs(removed))
	assert.Equal(t, []uint64{0, 2}, seqs(kept))

	kept, removed = splitForRemoval(ws, 3, 3, true)
	assert.Empty(t, kept)
	assert.Equal(t, []uint64{0, 1, 2}, seqs(removed), "emptying the roster stops the scanner too")
}

// TestSplitForRemoval_OnlyTheFirstCheckerIsKept checks that a second
// scanner (a quarantined one that came back) is not kept on its own.
func TestSplitForRemoval_OnlyTheFirstCheckerIsKept(t *testing.T) {
	clk := &fakeClock{n: int64(time.Hour)}
	ws := []*workerHandle{
		checkerHandle(0, clk, 0),
		handleBusyFor(1, clk, time.Second),
		checkerHandle(2, clk, 0),
	}
	kept, removed := splitForRemoval(ws, 1, 1, true)
	assert.Equal(t, []uint64{2}, seqs(removed))
	assert.Equal(t, []uint64{0, 1}, seqs(kept))
}

func TestHasStalledChecker(t *testing.T) {
	clk := &fakeClock{}
	assert.False(t, hasStalledChecker(nil))
	assert.False(t, hasStalledChecker([]*workerHandle{handleBusyFor(0, clk, 0)}))
	assert.True(t, hasStalledChecker([]*workerHandle{handleBusyFor(0, clk, 0), checkerHandle(1, clk, 0)}))
}
