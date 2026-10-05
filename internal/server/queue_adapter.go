package server

import (
	"context"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/redis/go-redis/v9"
)

// jobQueueRedisInfo adapts the job-queue go-redis client to the admin handler's
// QueueRedisInfoProvider, exposing the raw `INFO` payload for the per-queue db
// block (memory / clients / uptime)。go-redis 依存を admin package から隔離する。
type jobQueueRedisInfo struct {
	rdb *redis.Client
}

func (j jobQueueRedisInfo) QueueRedisInfo(ctx context.Context) (string, error) {
	return j.rdb.Info(ctx).Result()
}

// queueStatsInspectorAdapter adapts queue.Inspector to the minimal
// stream.QueueInspector interface needed by QueueStatsPublisher. Keeping this
// adapter in internal/server prevents internal/stream from importing
// internal/queue (which would create a circular dep via the driver types).
type queueStatsInspectorAdapter struct {
	inner *queue.Inspector
}

func (a *queueStatsInspectorAdapter) GetQueueInfo(qname string) (*stream.QueueStatsInfo, error) {
	info, err := a.inner.GetQueueInfo(qname)
	if err != nil {
		return nil, err
	}
	return &stream.QueueStatsInfo{
		Active:    info.Active,
		Pending:   info.Pending,
		Scheduled: info.Scheduled,
		Retry:     info.Retry,
		Completed: info.Completed,
	}, nil
}

// queueInspectorAdapter adapts queue.Inspector to admin.QueueInspector.
type queueInspectorAdapter struct {
	inner *queue.Inspector
}

func (a *queueInspectorAdapter) Queues() ([]string, error) {
	return a.inner.Queues()
}

func (a *queueInspectorAdapter) GetQueueInfo(qname string) (*apiadmin.QueueInfoResult, error) {
	info, err := a.inner.GetQueueInfo(qname)
	if err != nil {
		return nil, err
	}
	return &apiadmin.QueueInfoResult{
		Queue:         info.Queue,
		Size:          info.Size,
		Active:        info.Active,
		Pending:       info.Pending,
		Completed:     info.Completed,
		Failed:        info.Failed,
		Scheduled:     info.Scheduled,
		Retry:         info.Retry,
		IsPaused:      info.IsPaused,
		QualifiedName: info.QualifiedName,
	}, nil
}

func (a *queueInspectorAdapter) PauseQueue(qname string) error {
	return a.inner.PauseQueue(qname)
}

func (a *queueInspectorAdapter) UnpauseQueue(qname string) error {
	return a.inner.UnpauseQueue(qname)
}

func (a *queueInspectorAdapter) DeleteTask(qname, taskID string) error {
	return a.inner.DeleteTask(qname, taskID)
}

func (a *queueInspectorAdapter) DeleteAllPendingTasks(qname string) (int, error) {
	return a.inner.DeleteAllPendingTasks(qname)
}

func (a *queueInspectorAdapter) RunTask(qname, taskID string) error {
	return a.inner.RunTask(qname, taskID)
}

func (a *queueInspectorAdapter) ListPendingTasks(qname string, page, pageSize int) ([]*apiadmin.QueueTaskSummary, error) {
	rows, err := a.inner.ListPendingTasks(qname, page, pageSize)
	return taskSummariesToAdmin(rows), err
}

func (a *queueInspectorAdapter) ListActiveTasks(qname string, page, pageSize int) ([]*apiadmin.QueueTaskSummary, error) {
	rows, err := a.inner.ListActiveTasks(qname, page, pageSize)
	return taskSummariesToAdmin(rows), err
}

func (a *queueInspectorAdapter) ListScheduledTasks(qname string, page, pageSize int) ([]*apiadmin.QueueTaskSummary, error) {
	rows, err := a.inner.ListScheduledTasks(qname, page, pageSize)
	return taskSummariesToAdmin(rows), err
}

func (a *queueInspectorAdapter) ListCompletedTasks(qname string, page, pageSize int) ([]*apiadmin.QueueTaskSummary, error) {
	rows, err := a.inner.ListCompletedTasks(qname, page, pageSize)
	return taskSummariesToAdmin(rows), err
}

func (a *queueInspectorAdapter) ListFailedTasks(qname string, page, pageSize int) ([]*apiadmin.QueueTaskSummary, error) {
	rows, err := a.inner.ListFailedTasks(qname, page, pageSize)
	return taskSummariesToAdmin(rows), err
}

func (a *queueInspectorAdapter) ListRetryTasks(qname string, page, pageSize int) ([]*apiadmin.QueueTaskSummary, error) {
	rows, err := a.inner.ListRetryTasks(qname, page, pageSize)
	return taskSummariesToAdmin(rows), err
}

func (a *queueInspectorAdapter) ListDelayedTasks(qname string, page, pageSize int) ([]*apiadmin.QueueTaskSummary, error) {
	rows, err := a.inner.ListDelayedTasks(qname, page, pageSize)
	return taskSummariesToAdmin(rows), err
}

func (a *queueInspectorAdapter) GetTaskInfo(qname, taskID string) (*apiadmin.QueueTaskSummary, error) {
	t, err := a.inner.GetTaskInfo(qname, taskID)
	if err != nil {
		return nil, err
	}
	return taskSummaryToAdmin(t), nil
}

func (a *queueInspectorAdapter) GetTaskLogs(qname, taskID string, start, end int64) ([]string, int64, error) {
	return a.inner.GetTaskLogs(qname, taskID, start, end)
}

func (a *queueInspectorAdapter) QueueMetrics(qname, kind string) (*apiadmin.QueueMetricsResult, error) {
	m, err := a.inner.QueueMetrics(qname, kind)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, nil
	}
	return &apiadmin.QueueMetricsResult{Count: m.Count, Data: m.Data}, nil
}

func taskSummariesToAdmin(rows []*queue.TaskSummary) []*apiadmin.QueueTaskSummary {
	out := make([]*apiadmin.QueueTaskSummary, 0, len(rows))
	for _, r := range rows {
		out = append(out, taskSummaryToAdmin(r))
	}
	return out
}

func taskSummaryToAdmin(t *queue.TaskSummary) *apiadmin.QueueTaskSummary {
	if t == nil {
		return nil
	}
	return &apiadmin.QueueTaskSummary{
		ID:            t.ID,
		Queue:         t.Queue,
		Type:          t.Type,
		State:         t.State,
		Payload:       t.Payload,
		Retried:       t.Retried,
		MaxRetry:      t.MaxRetry,
		LastErr:       t.LastErr,
		LastFailedAt:  t.LastFailedAt,
		NextProcessAt: t.NextProcessAt,
		EnqueuedAt:    t.EnqueuedAt,
		ProcessedAt:   t.ProcessedAt,
		CompletedAt:   t.CompletedAt,
		ProcessedBy:   t.ProcessedBy,
		Opts:          t.Opts,
		Delay:         t.Delay,
		Stacktrace:    t.Stacktrace,
		ReturnValue:   t.ReturnValue,
		Progress:      t.Progress,
		AttemptsAt:    t.AttemptsAt,
	}
}
