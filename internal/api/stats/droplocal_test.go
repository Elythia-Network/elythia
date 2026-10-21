package stats

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestStats_DropLocal(t *testing.T) {
	resetStatsTables(t)
	t.Cleanup(func() { resetStatsTables(t) })

	h := NewHandler(testDB, nil, nil)
	instances, reactions := h.countDB()
	require.EqualValues(t, 0, instances)
	require.EqualValues(t, 0, reactions)

	seedReaction(t, "g")
	instances, _ = h.countDB()
	require.EqualValues(t, 0, instances, "served from the cache")

	h.DropLocal()
	instances, reactions = h.countDB()
	assert.EqualValues(t, 1, instances, "the totals are counted again after DropLocal")
	assert.EqualValues(t, 1, reactions)
}

// A drop while the totals are being counted keeps them out of the cache, and a
// call after the drop does not share the count started before it.
func TestStats_DropDuringCount(t *testing.T) {
	resetStatsTables(t)
	t.Cleanup(func() { resetStatsTables(t) })
	seedReaction(t, "h")

	sqlDB, err := testDB.DB()
	require.NoError(t, err)
	db, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{Logger: logger.Discard})
	require.NoError(t, err)
	var reactionCounts atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("stats_test:hold_first", func(tx *gorm.DB) {
		if tx.Statement.Table == "note_reaction" && reactionCounts.Add(1) == 1 {
			close(entered)
			<-release
		}
	}))
	h := NewHandler(db, nil, nil)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.countDB()
	}()
	<-entered
	h.DropLocal()
	seedReaction(t, "i")

	type pair struct{ instances, reactions int64 }
	after := make(chan pair, 1)
	go func() {
		i, r := h.countDB()
		after <- pair{i, r}
	}()
	select {
	case p := <-after:
		assert.Equal(t, pair{2, 2}, p)
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("a call after the drop waited for the count started before it")
	}

	close(release)
	wg.Wait()
	instances, reactions := h.countDB()
	assert.EqualValues(t, 2, instances, "the count started before the drop is not stored over the newer one")
	assert.EqualValues(t, 2, reactions)
	assert.EqualValues(t, 2, reactionCounts.Load())
}
