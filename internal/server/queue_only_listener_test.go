package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/config"
	queuemetrics "github.com/shiroha-a/mk/internal/queue/metrics"
)

// queue role の最小 mux は /healthz を返す。ここが無いと
// `elythia healthcheck` (Dockerfile が使う) が必ず失敗し、コンテナの
// ヘルスチェックを外さないと運用できないノードになる (#2459)。
func TestQueueOnlyMux_ServesHealthz(t *testing.T) {
	s := &Server{config: &config.Config{}}
	rec := httptest.NewRecorder()
	s.queueOnlyMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "ok", body["status"])
	// role を返すのは、監視先を取り違えたときに応答だけで気付けるようにするため。
	assert.Equal(t, string(config.RoleQueue), body["role"])
}

// **API 面が生えないこと。** ここが漏れると配送専用ノードが実質 Web ノードに
// なり、分離の意味が消える。`s.echo` を流用しない設計の要点。
func TestQueueOnlyMux_DoesNotServeAPI(t *testing.T) {
	s := &Server{config: &config.Config{}}
	mux := s.queueOnlyMux()

	for _, path := range []string{"/api/meta", "/api/notes/create", "/", "/inbox", "/@alice"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			assert.Equal(t, http.StatusNotFound, rec.Code)
		})
	}
}

// enableMetrics=false では /metrics も生やさない (server ノードと同じ gate)。
func TestQueueOnlyMux_MetricsOffByDefault(t *testing.T) {
	s := &Server{config: &config.Config{}}
	rec := httptest.NewRecorder()
	s.queueOnlyMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// enableMetrics=true なら出す。worker の観測手段はここしか無い (admin API は
// server ノード側にしかない)。
func TestQueueOnlyMux_ServesMetricsWhenEnabled(t *testing.T) {
	s := &Server{
		config:       &config.Config{EnableMetrics: true},
		queueMetrics: queuemetrics.New(),
	}
	rec := httptest.NewRecorder()
	s.queueOnlyMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	assert.NotEmpty(t, body)
}
