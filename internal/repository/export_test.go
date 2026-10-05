package repository

import (
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// SharedTestDB exposes the package's test DB (opened by TestMain)
// to the external repository_test package.
func SharedTestDB() *gorm.DB { return testDB }

// EmojiKeyWaiters reports how many callers are waiting on the in-flight fetch
// of k (0 when none is in flight).
func EmojiKeyWaiters(c *CachedEmojiRepository, k model.EmojiKey) int {
	c.keys.mu.Lock()
	defer c.keys.mu.Unlock()
	if f, ok := c.keys.inflight[k]; ok {
		return f.waiters
	}
	return 0
}

// EmojiKeyByIDCount reports the size of the id → key index.
func EmojiKeyByIDCount(c *CachedEmojiRepository) int {
	c.keys.mu.Lock()
	defer c.keys.mu.Unlock()
	return len(c.keys.byID)
}

// SetEmojiInvalidationDrainTimeout overrides the queue's drain timeout.
func SetEmojiInvalidationDrainTimeout(q *EmojiInvalidationQueue, d time.Duration) {
	q.drainTimeout = d
}
