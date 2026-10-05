package driver_test

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/stretchr/testify/assert"
)

func TestWithBackoff(t *testing.T) {
	o := driver.ApplyEnqueueOptions([]driver.EnqueueOption{
		driver.WithBackoff(driver.BackoffFixed, 5*time.Second),
	})
	assert.Equal(t, driver.BackoffFixed, o.BackoffType)
	assert.Equal(t, 5*time.Second, o.BackoffDelay)
}

func TestWithFederationBackoff(t *testing.T) {
	o := driver.ApplyEnqueueOptions([]driver.EnqueueOption{
		driver.WithFederationBackoff(),
	})
	// federation backoff は BullMQ custom strategy。delay は worker 側で
	// 算出するので enqueue option には乗らない (#1406, mkq#67)。
	assert.Equal(t, driver.BackoffCustom, o.BackoffType)
	assert.Zero(t, o.BackoffDelay)
}

// 未設定なら backoff フィールドは空のまま (= mkq は即時 retry)。
func TestNoBackoffByDefault(t *testing.T) {
	o := driver.ApplyEnqueueOptions(nil)
	assert.Empty(t, o.BackoffType)
	assert.Zero(t, o.BackoffDelay)
}
