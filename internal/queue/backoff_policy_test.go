package queue

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/stretchr/testify/assert"
)

func TestBackoffOptFromPolicy(t *testing.T) {
	// 未指定なら既定の custom backoff (= Misskey TS httpRelatedBackoff)。
	def := driver.ApplyEnqueueOptions([]driver.EnqueueOption{backoffOptFromPolicy(Policy{})})
	assert.Equal(t, driver.BackoffCustom, def.BackoffType)

	// custom を明示しても override 成立 (delay 不要)。
	custom := driver.ApplyEnqueueOptions([]driver.EnqueueOption{
		backoffOptFromPolicy(Policy{BackoffType: driver.BackoffCustom}),
	})
	assert.Equal(t, driver.BackoffCustom, custom.BackoffType)

	// built-in は delay とセットで上書きできる。
	override := driver.ApplyEnqueueOptions([]driver.EnqueueOption{
		backoffOptFromPolicy(Policy{BackoffType: driver.BackoffFixed, BackoffDelay: 10 * time.Second}),
	})
	assert.Equal(t, driver.BackoffFixed, override.BackoffType)
	assert.Equal(t, 10*time.Second, override.BackoffDelay)

	// built-in で delay 0 は不完全な指定として既定 (custom) にフォールバックする。
	partial := driver.ApplyEnqueueOptions([]driver.EnqueueOption{
		backoffOptFromPolicy(Policy{BackoffType: driver.BackoffFixed}),
	})
	assert.Equal(t, driver.BackoffCustom, partial.BackoffType)
}
