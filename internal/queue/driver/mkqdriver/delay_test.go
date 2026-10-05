package mkqdriver

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/shiroha-a/mkq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// driver.DelayError は mkq.Delay に変換する (#3048)。変換しないと mkq は普通の
// 失敗として扱い、待っているだけで試行回数を消費する。
func TestNewDispatchHandler_DelayConverts(t *testing.T) {
	dispatch := newDispatchHandler(map[string]driver.HandlerFunc{
		"x": func(_ context.Context, _ driver.Task) error {
			return fmt.Errorf("held back: %w", driver.Delay(90*time.Second))
		},
	}, "deliver", nil, -1, nil)
	_, err := dispatch(context.Background(), &mkq.Job[framedPayload]{Data: framedPayload{Type: "x"}})
	var delayed *mkq.DelayedError
	require.True(t, errors.As(err, &delayed), "got %v", err)
	assert.Equal(t, 90*time.Second, delayed.Delay)
	assert.False(t, errors.Is(err, mkq.ErrUnrecoverable))
}

// 遅延が ErrSkipRetry より優先する (mkq と同じ)。
func TestNativeError_DelayWinsOverSkipRetry(t *testing.T) {
	err := nativeError(errors.Join(driver.Delay(time.Second), driver.ErrSkipRetry))
	var delayed *mkq.DelayedError
	require.True(t, errors.As(err, &delayed))
	assert.False(t, errors.Is(err, mkq.ErrUnrecoverable))
	assert.NoError(t, nativeError(nil))
	plain := errors.New("boom")
	assert.Equal(t, plain, nativeError(plain))
}

// 遅延は観測しない。失敗として数えると失敗率が上がって見え、成功として数えると
// 送っていない ms 級の処理が処理時間の分布に混ざる。
func TestDispatchHandler_DelayIsNotObservedAsFailure(t *testing.T) {
	obs := &recordingObserver{}
	dispatch := newDispatchHandler(map[string]driver.HandlerFunc{
		"t": func(_ context.Context, _ driver.Task) error { return driver.Delay(time.Minute) },
	}, "deliver", obs, -1, nil)
	_, err := dispatch(context.Background(), &mkq.Job[framedPayload]{
		Data: framedPayload{Type: "t"}, Timestamp: time.Now(),
	})
	require.Error(t, err)
	assert.Empty(t, obs.failures, "a delay is neither a failure nor a success")
	assert.Empty(t, obs.procs)
}

// 遅延で戻った job は試行回数を消費しないので AttemptsMade は 0 のまま戻る。
// 取り出された回数 (AttemptsStarted) で見て、意図して待たせた時間を混雑として
// 数えない。
func TestDispatchHandler_SkipsWaitAfterDelay(t *testing.T) {
	obs := &recordingObserver{}
	dispatch := newDispatchHandler(map[string]driver.HandlerFunc{
		"t": func(_ context.Context, _ driver.Task) error { return nil },
	}, "deliver", obs, -1, nil)
	_, err := dispatch(context.Background(), &mkq.Job[framedPayload]{
		Data:            framedPayload{Type: "t"},
		Timestamp:       time.Now().Add(-time.Hour),
		AttemptsMade:    0,
		AttemptsStarted: 2,
	})
	require.NoError(t, err)
	assert.Empty(t, obs.waits)

	_, err = dispatch(context.Background(), &mkq.Job[framedPayload]{
		Data:            framedPayload{Type: "t"},
		Timestamp:       time.Now().Add(-time.Second),
		AttemptsStarted: 1,
	})
	require.NoError(t, err)
	assert.Len(t, obs.waits, 1, "the first dispatch is still observed")
}
