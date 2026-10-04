package federation

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The failure memory never grows past inboundMentionFailureMaxEntries: a full
// map drops expired entries first, and everything if none expired.
func TestMentionFetchGuard_FailureMemoryIsBounded(t *testing.T) {
	g := newMentionFetchGuard(1)
	now := time.Now()
	for i := range inboundMentionFailureMaxEntries {
		g.recordFailure(fmt.Sprintf("u%d", i), "h", false, now)
	}
	assert.Len(t, g.uris, inboundMentionFailureMaxEntries)

	// 期限切れが無ければ全部捨てて入れる。
	g.recordFailure("new", "h", true, now)
	assert.Len(t, g.uris, 1)
	assert.True(t, g.failedRecently("new", "x", now))
	assert.True(t, g.failedRecently("other", "h", now))
	assert.False(t, g.failedRecently("other", "h", now.Add(inboundMentionFailureTTL)))

	// 期限切れがあればそれだけを掃除する。
	g = newMentionFetchGuard(1)
	for i := range inboundMentionFailureMaxEntries - 1 {
		g.recordFailure(fmt.Sprintf("old%d", i), "h", false, now)
	}
	later := now.Add(inboundMentionFailureTTL / 2)
	g.recordFailure("fresh", "h", false, later)
	g.recordFailure("newer", "h", false, now.Add(inboundMentionFailureTTL))
	assert.Len(t, g.uris, 2)
	assert.True(t, g.failedRecently("fresh", "x", now.Add(inboundMentionFailureTTL)))
	// 既にある鍵の上書きは掃除しない。
	g.recordFailure("fresh", "h", false, later)
	assert.Len(t, g.uris, 2)
}

// tryAcquire never waits and hands out at most the configured number of slots.
func TestMentionFetchGuard_Slots(t *testing.T) {
	g := newMentionFetchGuard(2)
	r1, ok := g.tryAcquire()
	assert.True(t, ok)
	r2, ok := g.tryAcquire()
	assert.True(t, ok)
	_, ok = g.tryAcquire()
	assert.False(t, ok)
	r1()
	r3, ok := g.tryAcquire()
	assert.True(t, ok)
	r2()
	r3()
}
