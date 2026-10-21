package driveusage

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seqRepo returns a different size on each call (10, 20, ...) and blocks only
// the first call until release is closed.
type seqRepo struct {
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
}

func newSeqRepo() *seqRepo {
	return &seqRepo{entered: make(chan struct{}), release: make(chan struct{})}
}

func (f *seqRepo) Breakdown(int) (*repository.DriveUsageBreakdown, error) {
	n := f.calls.Add(1)
	if n == 1 {
		close(f.entered)
		<-f.release
	}
	return &repository.DriveUsageBreakdown{
		ByKind: []repository.DriveUsageKindRow{{
			Kind:             repository.DriveUsageKindAttachment,
			Origin:           repository.DriveUsageOriginLocal,
			DriveUsageBucket: repository.DriveUsageBucket{Count: 1, Size: n * 10},
		}},
	}, nil
}

func TestService_DropLocal(t *testing.T) {
	repo := &fakeRepo{}
	s, _ := newTestService(repo, time.Hour)
	_, err := s.Breakdown(false)
	require.NoError(t, err)
	r, err := s.Breakdown(false)
	require.NoError(t, err)
	require.True(t, r.Cached)

	s.DropLocal()
	r, err = s.Breakdown(false)
	require.NoError(t, err)
	assert.False(t, r.Cached, "the snapshot is recomputed after DropLocal")
	assert.Equal(t, int64(2), repo.calls.Load())
}

// A drop while the aggregation runs keeps its result out of the cache, and a
// call after the drop does not share the aggregation started before it.
func TestService_DropDuringAggregation(t *testing.T) {
	repo := newSeqRepo()
	s, _ := newTestService(repo, time.Hour)

	var wg sync.WaitGroup
	var before *Result
	wg.Add(1)
	go func() {
		defer wg.Done()
		before, _ = s.Breakdown(false)
	}()
	<-repo.entered
	s.DropLocal()

	after := make(chan *Result, 1)
	go func() {
		r, _ := s.Breakdown(false)
		after <- r
	}()
	var r *Result
	select {
	case r = <-after:
	case <-time.After(5 * time.Second):
		close(repo.release)
		t.Fatal("a call after the drop waited for the aggregation started before it")
	}
	assert.Equal(t, int64(20), r.Breakdown.Total().Size)

	close(repo.release)
	wg.Wait()
	require.NotNil(t, before)
	assert.Equal(t, int64(10), before.Breakdown.Total().Size)

	cached, err := s.Breakdown(false)
	require.NoError(t, err)
	assert.True(t, cached.Cached)
	assert.Equal(t, int64(20), cached.Breakdown.Total().Size,
		"the aggregation started before the drop is not stored over the newer one")
}
