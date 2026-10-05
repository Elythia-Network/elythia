package mkqdriver

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/stretchr/testify/assert"
)

// backoff option が指定されたとき toMkqAddOptions が mkq.AddOption を 1 つ
// 追加で emit することを確認する (#1405 / #1406)。mkq.AddOption は不透明な
// 関数なので、適用後の addConfig を覗けない代わりに emit 件数で検証する。
func TestToMkqAddOptions_Backoff(t *testing.T) {
	base := toMkqAddOptions(driver.EnqueueOptions{}, "deliver", nil)

	// custom は delay 不要で emit される (federation 既定)。
	withCustom := toMkqAddOptions(driver.EnqueueOptions{
		BackoffType: driver.BackoffCustom,
	}, "deliver", nil)
	assert.Len(t, withCustom, len(base)+1, "custom backoff で 1 option 追加される")

	withExp := toMkqAddOptions(driver.EnqueueOptions{
		BackoffType:  driver.BackoffExponential,
		BackoffDelay: time.Second,
	}, "deliver", nil)
	assert.Len(t, withExp, len(base)+1, "exponential backoff で 1 option 追加される")

	withFixed := toMkqAddOptions(driver.EnqueueOptions{
		BackoffType:  driver.BackoffFixed,
		BackoffDelay: time.Second,
	}, "deliver", nil)
	assert.Len(t, withFixed, len(base)+1, "fixed backoff で 1 option 追加される")

	// built-in で delay 0 のときは emit しない (custom は除く)。
	noDelay := toMkqAddOptions(driver.EnqueueOptions{BackoffType: driver.BackoffExponential}, "deliver", nil)
	assert.Len(t, noDelay, len(base), "built-in は delay 0 では emit しない")

	unknown := toMkqAddOptions(driver.EnqueueOptions{
		BackoffType:  "bogus",
		BackoffDelay: time.Second,
	}, "deliver", nil)
	assert.Len(t, unknown, len(base), "未知の type では emit しない")
}
