package server

import (
	"time"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/runtimestats"
)

// queueRuntimeAdapter implements apiadmin.QueueRuntimeProvider by combining
// the live driver worker count, the resolved auto-scale bounds and the
// short-lived runtimestats snapshot (#2277).
//
// admin UI がこれを読むことで、/metrics を公開せずとも「今 worker が何本で、
// 直近どう scale したか / どれだけ待たされているか」が分かる。
type queueRuntimeAdapter struct {
	drv        driver.Driver
	stats      *runtimestats.Recorder
	autoScale  bool
	minWorkers int
	maxWorkers int
}

// QueueRuntime implements apiadmin.QueueRuntimeProvider.
func (a *queueRuntimeAdapter) QueueRuntime(qname string) (apiadmin.QueueRuntime, bool) {
	if a == nil || a.drv == nil || qname == "" {
		return apiadmin.QueueRuntime{}, false
	}
	rt := apiadmin.QueueRuntime{
		Workers:    a.drv.WorkerCount(qname),
		MinWorkers: a.minWorkers,
		MaxWorkers: a.maxWorkers,
		AutoScale:  a.autoScale,
		// scaleEvents は nil を返すと JSON が null になる。frontend が
		// v-for で回すので必ず配列にする。
		ScaleEvents: []apiadmin.QueueScaleEvent{},
	}
	if a.stats != nil {
		snap := a.stats.Snapshot(qname)
		rt.DispatchWaitMs = toQuantiles(snap.DispatchWait)
		rt.ProcessingMs = toQuantiles(snap.Processing)
		rt.RecentFailures = snap.Failures
		for _, ev := range snap.ScaleEvents {
			rt.ScaleEvents = append(rt.ScaleEvents, apiadmin.QueueScaleEvent{
				At:        ev.At.Format(time.RFC3339),
				Direction: ev.Direction,
				From:      ev.From,
				To:        ev.To,
			})
		}
	}
	return rt, true
}

// toQuantiles converts the recorder snapshot into the API shape. Durations
// become milliseconds with 0.1ms resolution so the JSON stays readable.
func toQuantiles(s runtimestats.LatencySnapshot) apiadmin.Quantiles {
	return apiadmin.Quantiles{
		Count: s.Count,
		P50:   roundMillis(s.P50),
		P95:   roundMillis(s.P95),
		Max:   roundMillis(s.Max),
	}
}

func roundMillis(d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(d.Microseconds()) / 1000.0
}

// newQueueRuntimeProvider builds the admin-facing runtime view from the live
// driver plus the resolved auto-scale bounds. Bounds mirror startAutoScale's
// resolution so the UI shows the same numbers the controller obeys.
func (s *Server) newQueueRuntimeProvider() apiadmin.QueueRuntimeProvider {
	minWorkers, maxWorkers := resolveAutoScaleBounds(s.config)
	return &queueRuntimeAdapter{
		drv:        s.queueDriver,
		stats:      s.queueRuntimeStats,
		autoScale:  s.config.JobQueueAutoScale,
		minWorkers: minWorkers,
		maxWorkers: maxWorkers,
	}
}
