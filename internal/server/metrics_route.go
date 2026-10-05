package server

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/elythia-network/elythia/internal/queue/driver"
	queuemetrics "github.com/elythia-network/elythia/internal/queue/metrics"
)

// newQueueMetrics constructs a Metrics bundle and binds it to the given
// driver for pull-mode gauges. The instance is **not** registered yet —
// caller must invoke wireMetricsEndpoint (which registers + mounts the
// HTTP handler). Separating construction and registration lets the
// auto-scale runner (#1125) share the same Metrics instance so its
// ScaleEventsTotal counter feeds into the /metrics output.
func newQueueMetrics(d driver.Driver) *queuemetrics.Metrics {
	m := queuemetrics.New()
	m.BindDriver(d)
	return m
}

// wireMetricsEndpoint registers `metrics` against a fresh prometheus
// Registry and mounts /metrics on e. Extracted so router.setupRoutes
// and the smoke test (`metrics_route_test.go`) share a single
// configuration path — if you change the endpoint path or the
// handler options, the test covers the regression.
//
// Returns an error if metrics registration fails (= duplicate
// registration bug). Caller is responsible for the EnableMetrics gate
// so this helper can be reused from contexts that always want the
// endpoint (e.g. tests).
func wireMetricsEndpoint(e *echo.Echo, metrics *queuemetrics.Metrics) error {
	handler, err := buildMetricsHandler(metrics)
	if err != nil {
		return err
	}
	e.GET("/metrics", echo.WrapHandler(handler))
	return nil
}

// buildMetricsHandler registers `metrics` against a fresh registry and
// wraps the result in promhttp.HandlerFor. Exposed separately from
// wireMetricsEndpoint so tests can drive the handler directly
// (httptest) without an Echo instance.
func buildMetricsHandler(metrics *queuemetrics.Metrics) (http.Handler, error) {
	registry := prometheus.NewRegistry()
	if err := metrics.Register(registry); err != nil {
		return nil, err
	}
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}), nil
}
