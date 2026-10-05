package queue_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// payloadRecordingClient captures the payload as well as the options, which
// recordingDriverClient (queue_keepfailed_test.go) deliberately drops.
type payloadRecordingClient struct {
	lastTaskType string
	lastPayload  []byte
	lastOpts     driver.EnqueueOptions
	calls        int
	err          error
}

func (r *payloadRecordingClient) Enqueue(_ context.Context, taskType string, payload []byte, opts ...driver.EnqueueOption) error {
	r.calls++
	if r.err != nil {
		return r.err
	}
	r.lastTaskType = taskType
	r.lastPayload = payload
	r.lastOpts = driver.ApplyEnqueueOptions(opts)
	return nil
}

func (r *payloadRecordingClient) Close() error { return nil }

func TestClient_EnqueueDeleteObjectStorageFile(t *testing.T) {
	rec := &payloadRecordingClient{}
	c := queue.NewClient(&stubDriver{client: rec})
	defer func() { _ = c.Close() }()

	require.NoError(t, c.EnqueueDeleteObjectStorageFile("abc123.webp"))

	assert.Equal(t, queue.TaskTypeObjectStorageDeleteFile, rec.lastTaskType)
	assert.Equal(t, queue.ObjectStorageQueueName, rec.lastOpts.Queue)

	var got queue.ObjectStorageDeleteFilePayload
	require.NoError(t, json.Unmarshal(rec.lastPayload, &got))
	assert.Equal(t, "abc123.webp", got.Key)

	// storage の一時障害を待てるよう、指数バックオフ付きで再試行させる。
	assert.Positive(t, rec.lastOpts.MaxRetry)
	assert.Equal(t, driver.BackoffExponential, rec.lastOpts.BackoffType)
	assert.Equal(t, time.Minute, rec.lastOpts.BackoffDelay)
}

func TestClient_EnqueueDeleteObjectStorageFile_RetentionPolicyApplied(t *testing.T) {
	rec := &payloadRecordingClient{}
	c := queue.NewClient(&stubDriver{client: rec})
	defer func() { _ = c.Close() }()

	c.SetPolicy(queue.ObjectStorageQueueName, queue.Policy{KeepFailed: 42, KeepCompleted: 7})
	require.NoError(t, c.EnqueueDeleteObjectStorageFile("k"))

	assert.True(t, rec.lastOpts.KeepFailedSet)
	assert.Equal(t, 42, rec.lastOpts.KeepFailed)
	assert.True(t, rec.lastOpts.KeepCompletedSet)
	assert.Equal(t, 7, rec.lastOpts.KeepCompleted)
}

func TestClient_EnqueueDeleteObjectStorageFile_DriverError(t *testing.T) {
	rec := &payloadRecordingClient{err: errors.New("redis down")}
	c := queue.NewClient(&stubDriver{client: rec})
	defer func() { _ = c.Close() }()

	assert.Error(t, c.EnqueueDeleteObjectStorageFile("k"))
}

func TestClient_EnqueueCleanRemoteFiles(t *testing.T) {
	rec := &payloadRecordingClient{}
	c := queue.NewClient(&stubDriver{client: rec})
	defer func() { _ = c.Close() }()

	require.NoError(t, c.EnqueueCleanRemoteFiles())

	assert.Equal(t, queue.TaskTypeCleanRemoteFiles, rec.lastTaskType)
	assert.Equal(t, queue.ObjectStorageQueueName, rec.lastOpts.Queue)
	assert.Nil(t, rec.lastPayload)
	// 一括削除は job 1 本で回しきる。失敗しても再実行すればよいので retry せず、
	// 連打は unique window で吸収する。
	assert.Equal(t, 0, rec.lastOpts.MaxRetry)
	assert.Positive(t, rec.lastOpts.UniqueTTL)
}

func TestClient_EnqueueCleanRemoteFiles_DriverError(t *testing.T) {
	rec := &payloadRecordingClient{err: errors.New("redis down")}
	c := queue.NewClient(&stubDriver{client: rec})
	defer func() { _ = c.Close() }()

	assert.Error(t, c.EnqueueCleanRemoteFiles())
}

// EnqueueUnfollowBulkDelayed は ProcessIn を driver まで通す (#2420)。
// これが落ちると、アカウント移行の「24 時間後に旧アカウントの following を
// 解除する」が即時実行になり、移行直後に Undo(Follow) が殺到する。
func TestClient_EnqueueUnfollowBulkDelayed(t *testing.T) {
	rec := &payloadRecordingClient{}
	c := queue.NewClient(&stubDriver{client: rec})
	defer func() { _ = c.Close() }()

	payloads := []queue.UnfollowPayload{
		{FollowerID: "src", FolloweeID: "a"},
		{FollowerID: "src", FolloweeID: "b"},
	}
	require.NoError(t, c.EnqueueUnfollowBulkDelayed(payloads, 24*time.Hour))

	assert.Equal(t, 2, rec.calls, "payload ごとに 1 job")
	assert.Equal(t, queue.TaskTypeUnfollow, rec.lastTaskType)
	assert.Equal(t, queue.RelationshipQueueName, rec.lastOpts.Queue,
		"遅延指定で queue 振り分けが変わってはいけない")
	assert.Equal(t, 24*time.Hour, rec.lastOpts.ProcessIn)
}

// delay <= 0 なら即時 enqueue。EnqueueUnfollowBulk と同じ挙動になる。
func TestClient_EnqueueUnfollowBulkDelayed_ZeroDelayIsImmediate(t *testing.T) {
	for _, delay := range []time.Duration{0, -time.Second} {
		rec := &payloadRecordingClient{}
		c := queue.NewClient(&stubDriver{client: rec})

		require.NoError(t, c.EnqueueUnfollowBulkDelayed(
			[]queue.UnfollowPayload{{FollowerID: "src", FolloweeID: "a"}}, delay))

		assert.Zero(t, rec.lastOpts.ProcessIn, "delay=%v は即時", delay)
		_ = c.Close()
	}
}

// enqueue が失敗しても残りの payload を試み、まとめてエラーを返す。
// 1 件の失敗で残り全部が積まれないと、その分のフォロー行が永久に残る。
func TestClient_EnqueueUnfollowBulkDelayed_AggregatesErrors(t *testing.T) {
	rec := &payloadRecordingClient{err: assert.AnError}
	c := queue.NewClient(&stubDriver{client: rec})
	defer func() { _ = c.Close() }()

	err := c.EnqueueUnfollowBulkDelayed([]queue.UnfollowPayload{
		{FollowerID: "src", FolloweeID: "a"},
		{FollowerID: "src", FolloweeID: "b"},
	}, time.Hour)

	require.Error(t, err)
	assert.Equal(t, 2, rec.calls, "1 件失敗しても残りを試みる")
}
