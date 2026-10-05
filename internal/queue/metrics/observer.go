package metrics

import (
	"time"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// Observer adapts driver observations into the Prometheus histograms
// declared by New(). It exists so the driver stays free of a Prometheus
// dependency (#2277).
//
// DispatchWaitSeconds / ProcessingSeconds はこれまで declare だけで hook が
// 無く、scrape しても常に 0 だった。本 adapter が唯一の書き込み経路になる。
type Observer struct {
	m *Metrics
}

// NewObserver returns an Observer writing into m. Returns nil when m is nil
// so callers can wire unconditionally.
func NewObserver(m *Metrics) *Observer {
	if m == nil {
		return nil
	}
	return &Observer{m: m}
}

// ObserveDispatchWait implements driver.Observer.
func (o *Observer) ObserveDispatchWait(queue string, d time.Duration) {
	if o == nil || o.m == nil {
		return
	}
	o.m.DispatchWaitSeconds.WithLabelValues(queue).Observe(d.Seconds())
}

// ObserveProcessing implements driver.Observer.
func (o *Observer) ObserveProcessing(queue string, d time.Duration, failed bool) {
	if o == nil || o.m == nil {
		return
	}
	status := "success"
	if failed {
		status = "failure"
	}
	o.m.ProcessingSeconds.WithLabelValues(queue, status).Observe(d.Seconds())
}

// MultiObserver fans one observation out to several observers. Used to feed
// both Prometheus and the admin-facing runtimestats recorder from the single
// driver hook.
type MultiObserver []driver.Observer

// ObserveDispatchWait implements driver.Observer.
func (m MultiObserver) ObserveDispatchWait(queue string, d time.Duration) {
	for _, o := range m {
		if o != nil {
			o.ObserveDispatchWait(queue, d)
		}
	}
}

// ObserveProcessing implements driver.Observer.
func (m MultiObserver) ObserveProcessing(queue string, d time.Duration, failed bool) {
	for _, o := range m {
		if o != nil {
			o.ObserveProcessing(queue, d, failed)
		}
	}
}
