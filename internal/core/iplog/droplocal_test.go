package iplog

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// After DropLocal the same observation is written again, and the window starts
// over from the drop.
func TestService_DropLocal(t *testing.T) {
	s, repo, _, clock := newTestService(t, true)

	s.Record("u1", "192.0.2.1")
	s.Record("u2", "192.0.2.2")
	assert.Len(t, repo.calls(), 2)

	*clock = clock.Add(30 * time.Minute)
	s.DropLocal()
	s.Record("u1", "192.0.2.1")
	s.Record("u2", "192.0.2.2")
	assert.Len(t, repo.calls(), 4, "every mark is dropped")

	// 窓は捨てた時刻から数え直す。元の窓の終わりを過ぎても、捨てた後の印は残る。
	*clock = clock.Add(40 * time.Minute)
	s.Record("u1", "192.0.2.1")
	assert.Len(t, repo.calls(), 4, "the window restarts at the drop")
}
