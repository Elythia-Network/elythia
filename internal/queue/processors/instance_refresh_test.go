package processors_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubFetcher records Fetch calls so the test can assert which hosts were
// refreshed. err controls the error returned from Fetch.
type stubFetcher struct {
	calls []string
	err   error
}

func (s *stubFetcher) Fetch(host string) error {
	s.calls = append(s.calls, host)
	return s.err
}

func TestInstanceRefreshProcessor_Handle_WalksStaleHosts(t *testing.T) {
	now := time.Date(2026, 4, 22, 3, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	repo := testutil.NewMockInstanceRepository()
	// stale (never fetched)
	repo.Instances["a.example"] = &model.Instance{Host: "a.example"}
	// stale (48h old)
	repo.Instances["b.example"] = &model.Instance{Host: "b.example", InfoUpdatedAt: &old}
	// fresh (1h old) — skip
	repo.Instances["c.example"] = &model.Instance{Host: "c.example", InfoUpdatedAt: &recent}
	// suspended — skip
	repo.Instances["d.example"] = &model.Instance{Host: "d.example", SuspensionState: model.SuspensionStateManuallySuspended}
	// not responding — skip
	repo.Instances["e.example"] = &model.Instance{Host: "e.example", IsNotResponding: true}

	fetcher := &stubFetcher{}
	p := processors.NewInstanceRefreshProcessor(repo, fetcher, processors.InstanceRefreshConfig{
		StaleAfter: 24 * time.Hour,
		BatchLimit: 100,
		FetchDelay: 1, // near-zero sleep so the test is fast
	})
	p.SetClock(func() time.Time { return now })

	require.NoError(t, p.Handle(context.Background(), driver.RawTask{}))
	// 2 hosts fetched (a and b, in infoUpdatedAt ASC NULLS FIRST order)
	assert.Len(t, fetcher.calls, 2)
	assert.Contains(t, fetcher.calls, "a.example")
	assert.Contains(t, fetcher.calls, "b.example")
}

func TestInstanceRefreshProcessor_Handle_FetchErrorDoesNotAbort(t *testing.T) {
	now := time.Date(2026, 4, 22, 3, 0, 0, 0, time.UTC)
	repo := testutil.NewMockInstanceRepository()
	repo.Instances["a.example"] = &model.Instance{Host: "a.example"}
	repo.Instances["b.example"] = &model.Instance{Host: "b.example"}

	// err を常に返す fetcher でも job は成功扱い (failures は log 止まり)。
	fetcher := &stubFetcher{err: errors.New("boom")}
	p := processors.NewInstanceRefreshProcessor(repo, fetcher, processors.InstanceRefreshConfig{
		FetchDelay: 1,
	})
	p.SetClock(func() time.Time { return now })
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{}))
	assert.Len(t, fetcher.calls, 2)
}

func TestInstanceRefreshProcessor_Handle_NilArgsNoOp(t *testing.T) {
	p := processors.NewInstanceRefreshProcessor(nil, nil, processors.InstanceRefreshConfig{})
	assert.NoError(t, p.Handle(context.Background(), driver.RawTask{}))
}

func TestInstanceRefreshProcessor_Handle_ContextCancellationInterrupts(t *testing.T) {
	now := time.Date(2026, 4, 22, 3, 0, 0, 0, time.UTC)
	repo := testutil.NewMockInstanceRepository()
	for _, h := range []string{"a.example", "b.example", "c.example"} {
		repo.Instances[h] = &model.Instance{Host: h}
	}
	fetcher := &stubFetcher{}
	p := processors.NewInstanceRefreshProcessor(repo, fetcher, processors.InstanceRefreshConfig{
		FetchDelay: 100 * time.Millisecond,
	})
	p.SetClock(func() time.Time { return now })

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 即座に cancel → ctx.Err() でループを抜ける
	err := p.Handle(ctx, driver.RawTask{})
	assert.ErrorIs(t, err, context.Canceled)
}
