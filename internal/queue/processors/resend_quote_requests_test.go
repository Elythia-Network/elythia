package processors

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

type fakeQuoteResender struct {
	at  []time.Time
	n   int
	err error
}

func (f *fakeQuoteResender) ResendPending(now time.Time) (int, error) {
	f.at = append(f.at, now)
	return f.n, f.err
}

// 毎分の送り直し (#3238) は今の時刻で呼び、失敗しても retry させない (次の回が拾う)。
func TestResendQuoteRequests(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		n   int
		err error
	}{{n: 0}, {n: 2}, {err: errors.New("db down")}} {
		f := &fakeQuoteResender{n: tc.n, err: tc.err}
		p := NewResendQuoteRequestsProcessor(f)
		p.now = func() time.Time { return now }
		require.NoError(t, p.Handle(context.Background(), driver.RawTask{TypeName: "test"}))
		assert.Equal(t, []time.Time{now}, f.at)
	}
}
