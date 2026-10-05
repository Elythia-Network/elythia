package admin_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/procstats"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeMetrics(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

// Deps 未配線でも 200 を返す。最小構成のデプロイでダッシュボードが 500 に
// ならないための保証。
func TestServerMetrics_Unwired(t *testing.T) {
	h, _, _, _ := newTestHandler(t)

	rec := doPost(h.ServerMetrics, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	out := decodeMetrics(t, rec.Body.Bytes())
	goStats, ok := out["go"].(map[string]any)
	require.True(t, ok)
	assert.Positive(t, goStats["goroutines"])
	version, ok := out["version"].(map[string]any)
	require.True(t, ok)
	assert.NotEmpty(t, version["misskey"])
	assert.NotEmpty(t, version["mkGo"])
	assert.Zero(t, out["uptimeMs"], "起動時刻未配線なら 0")
}

func TestServerMetrics_Uptime(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetProcStatsDeps(procstats.Deps{StartedAt: time.Now().Add(-2 * time.Minute)})

	rec := doPost(h.ServerMetrics, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	out := decodeMetrics(t, rec.Body.Bytes())
	assert.Positive(t, out["uptimeMs"])
}

// 単位を field 名に出しているので、frontend が ns と ms を取り違えないことを
// レスポンス shape のレベルで固定する。
func TestServerMetrics_UnitSuffixedFields(t *testing.T) {
	h, _, _, _ := newTestHandler(t)

	rec := doPost(h.ServerMetrics, `{}`, adminUser)
	out := decodeMetrics(t, rec.Body.Bytes())

	goStats := out["go"].(map[string]any)
	for _, key := range []string{"heapAllocBytes", "heapSysBytes", "lastGcPauseNs"} {
		assert.Contains(t, goStats, key)
	}
	assert.Contains(t, out, "uptimeMs")
}

// DB / Redis の接続プールは出さない (#2395 レビューで UI ごと落とした)。
// 復活させるなら UI と併せて入れる前提なので、レスポンスに漏れ出さないことを固定する。
func TestServerMetrics_NoPoolSections(t *testing.T) {
	h, _, _, _ := newTestHandler(t)

	rec := doPost(h.ServerMetrics, `{}`, adminUser)
	out := decodeMetrics(t, rec.Body.Bytes())

	assert.NotContains(t, out, "db")
	assert.NotContains(t, out, "redis")
}
