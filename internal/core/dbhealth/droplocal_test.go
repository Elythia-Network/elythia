package dbhealth

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seqReads makes readFn return a report whose GeneratedAt is base plus the call
// number in seconds. Only the first read blocks until release is closed.
func seqReads(s *Service, base time.Time) (calls *atomic.Int64, entered, release chan struct{}) {
	calls = &atomic.Int64{}
	entered, release = make(chan struct{}), make(chan struct{})
	s.readFn = func(ctx context.Context, now time.Time) (Report, error) {
		n := calls.Add(1)
		if n == 1 {
			close(entered)
			<-release
		}
		return Report{GeneratedAt: base.Add(time.Duration(n) * time.Second)}, nil
	}
	return calls, entered, release
}

func TestService_DropLocal(t *testing.T) {
	base := time.Now()
	s := NewService(nil, false)
	s.SetClockForTest(func() time.Time { return base })
	calls, _, release := seqReads(s, base)
	close(release)

	_, err := s.Report(context.Background())
	require.NoError(t, err)
	_, err = s.Report(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(1), calls.Load(), "served from the cache")

	s.DropLocal()
	r, err := s.Report(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(2), calls.Load(), "the report is read again after DropLocal")
	assert.Equal(t, base.Add(2*time.Second), r.GeneratedAt)
}

// A drop while the statistics are being read keeps that report out of the
// cache, and a call after the drop does not share the reading started before it.
func TestService_DropDuringRead(t *testing.T) {
	base := time.Now()
	s := NewService(nil, false)
	s.SetClockForTest(func() time.Time { return base })
	calls, entered, release := seqReads(s, base)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = s.Report(context.Background())
	}()
	<-entered
	s.DropLocal()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := s.Report(ctx)
	require.NoError(t, err, "a call after the drop waited for the reading started before it")
	assert.Equal(t, base.Add(2*time.Second), r.GeneratedAt)

	close(release)
	wg.Wait()
	cached, err := s.Report(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(2), calls.Load())
	assert.Equal(t, base.Add(2*time.Second), cached.GeneratedAt,
		"the report read before the drop is not stored over the newer one")
}
