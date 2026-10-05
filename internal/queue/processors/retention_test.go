package processors_test

import (
	"context"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubAggregator struct {
	called int
	err    error
}

func (s *stubAggregator) Aggregate(_ context.Context) error {
	s.called++
	return s.err
}

func TestRetentionAggregateProcessor_Handle_Success(t *testing.T) {
	agg := &stubAggregator{}
	p := processors.NewRetentionAggregateProcessor(agg)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{TypeName: "x"}))
	assert.Equal(t, 1, agg.called)
}

// 失敗時も nil を返してジョブとしては success 扱いにする契約を確認する。
// MaxRetry(0) と組み合わさって dead queue を膨らませない方針 (#421)。
func TestRetentionAggregateProcessor_Handle_SwallowsError(t *testing.T) {
	wantErr := errors.New("db boom")
	agg := &stubAggregator{err: wantErr}
	p := processors.NewRetentionAggregateProcessor(agg)
	err := p.Handle(context.Background(), driver.RawTask{TypeName: "x"})
	assert.NoError(t, err, "aggregation failures must not surface as job errors")
	assert.Equal(t, 1, agg.called)
}

// Nil aggregator → no-op without panic. Callers wire the processor before
// the service is ready in some bootstrap orderings.
func TestRetentionAggregateProcessor_Handle_NilSvcIsNoop(t *testing.T) {
	p := processors.NewRetentionAggregateProcessor(nil)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{TypeName: "x"}))
}
