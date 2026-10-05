package queue

import (
	"github.com/elythia-network/elythia/internal/queue/driver"
)

// InspectorInfo aliases driver.InspectorInfo so callers depend on
// the queue package without pulling driver into their imports.
type InspectorInfo = driver.InspectorInfo

// TaskSummary aliases driver.TaskSummary for the same reason.
type TaskSummary = driver.TaskSummary

// MetricsResult aliases driver.MetricsResult.
type MetricsResult = driver.MetricsResult

// MetricsKindCompleted / MetricsKindFailed re-export driver constants
// so callers can avoid importing the driver package directly.
const (
	MetricsKindCompleted = driver.MetricsKindCompleted
	MetricsKindFailed    = driver.MetricsKindFailed
)

// Inspector is the driver-neutral facade over driver.Inspector.
type Inspector struct {
	inner driver.Inspector
}

// NewInspector wraps the driver's inspector.
func NewInspector(d driver.Driver) *Inspector {
	return &Inspector{inner: d.Inspector()}
}

// Queues returns the list of queue names known to the inspector.
func (i *Inspector) Queues() ([]string, error) { return i.inner.Queues() }

// GetQueueInfo returns aggregated counters for the named queue.
func (i *Inspector) GetQueueInfo(qname string) (*InspectorInfo, error) {
	return i.inner.GetQueueInfo(qname)
}

// DispatchableCount returns how many jobs a worker could dequeue from the
// named queue right now (0 while paused), skipping the rest of the summary
// (#2605, #3166).
func (i *Inspector) DispatchableCount(qname string) (int, error) {
	return i.inner.DispatchableCount(qname)
}

// PauseQueue pauses the named queue (#17436)。
func (i *Inspector) PauseQueue(qname string) error {
	return i.inner.PauseQueue(qname)
}

// UnpauseQueue resumes the named queue (#17436)。
func (i *Inspector) UnpauseQueue(qname string) error {
	return i.inner.UnpauseQueue(qname)
}

// DeleteTask deletes a task by queue and ID.
func (i *Inspector) DeleteTask(qname, taskID string) error {
	return i.inner.DeleteTask(qname, taskID)
}

// DeleteAllPendingTasks deletes all pending tasks in a queue.
func (i *Inspector) DeleteAllPendingTasks(qname string) (int, error) {
	return i.inner.DeleteAllPendingTasks(qname)
}

// RunTask promotes a scheduled / retry task to run immediately.
func (i *Inspector) RunTask(qname, taskID string) error {
	return i.inner.RunTask(qname, taskID)
}

// Close releases the underlying inspector.
func (i *Inspector) Close() error { return i.inner.Close() }

// ListPendingTasks returns up to pageSize pending tasks in qname,
// starting at the given page (1-indexed).
func (i *Inspector) ListPendingTasks(qname string, page, pageSize int) ([]*TaskSummary, error) {
	return i.inner.ListPendingTasks(qname, page, pageSize)
}

// ListActiveTasks returns up to pageSize active tasks.
func (i *Inspector) ListActiveTasks(qname string, page, pageSize int) ([]*TaskSummary, error) {
	return i.inner.ListActiveTasks(qname, page, pageSize)
}

// ListCompletedTasks returns up to pageSize completed (finished) tasks.
func (i *Inspector) ListCompletedTasks(qname string, page, pageSize int) ([]*TaskSummary, error) {
	return i.inner.ListCompletedTasks(qname, page, pageSize)
}

// ListFailedTasks returns up to pageSize failed (finished) tasks.
func (i *Inspector) ListFailedTasks(qname string, page, pageSize int) ([]*TaskSummary, error) {
	return i.inner.ListFailedTasks(qname, page, pageSize)
}

// ListScheduledTasks returns up to pageSize scheduled tasks.
func (i *Inspector) ListScheduledTasks(qname string, page, pageSize int) ([]*TaskSummary, error) {
	return i.inner.ListScheduledTasks(qname, page, pageSize)
}

// ListRetryTasks returns up to pageSize retry tasks.
func (i *Inspector) ListRetryTasks(qname string, page, pageSize int) ([]*TaskSummary, error) {
	return i.inner.ListRetryTasks(qname, page, pageSize)
}

// ListDelayedTasks returns up to pageSize entries of the whole delayed
// bucket, latest fire time first.
func (i *Inspector) ListDelayedTasks(qname string, page, pageSize int) ([]*TaskSummary, error) {
	return i.inner.ListDelayedTasks(qname, page, pageSize)
}

// GetTaskInfo returns full info for a single task.
func (i *Inspector) GetTaskInfo(qname, taskID string) (*TaskSummary, error) {
	return i.inner.GetTaskInfo(qname, taskID)
}

// GetTaskLogs returns the log lines recorded against a task.
// See driver.Inspector.GetTaskLogs.
func (i *Inspector) GetTaskLogs(qname, taskID string, start, end int64) ([]string, int64, error) {
	return i.inner.GetTaskLogs(qname, taskID, start, end)
}

// QueueMetrics returns BullMQ-spec per-minute completed / failed
// history for the given queue. See driver.Inspector.QueueMetrics.
func (i *Inspector) QueueMetrics(qname, kind string) (*MetricsResult, error) {
	return i.inner.QueueMetrics(qname, kind)
}
