package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubQueueRedisInfo struct{ raw string }

func (s stubQueueRedisInfo) QueueRedisInfo(_ context.Context) (string, error) {
	return s.raw, nil
}

// QueueQueueStats が job-queue Redis INFO 由来の db (memory / clients / uptime)
// を埋めることを handler 経由で確認する (#1393)。従来は 0 固定 stub だった。
func TestQueueQueueStats_PopulatesDBFromRedisInfo(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		info: map[string]*apiadmin.QueueInfoResult{
			"deliver": {Queue: "deliver", Active: 1},
		},
	}
	h.SetQueueInspector(insp)
	h.SetQueueRedis(stubQueueRedisInfo{raw: "# Memory\n" +
		"used_memory:2048\n" +
		"used_memory_peak:4096\n" +
		"# Clients\n" +
		"connected_clients:3\n" +
		"blocked_clients:0\n" +
		"# Server\n" +
		"uptime_in_seconds:99\n" +
		"tcp_port:6379\n"})

	rec := doPost(h.QueueQueueStats, `{"queue":"deliver"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))

	db, ok := got["db"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 99, db["uptime"])
	assert.EqualValues(t, 6379, db["port"])
	mem := db["memory"].(map[string]any)
	assert.EqualValues(t, 2048, mem["used"])
	assert.EqualValues(t, 4096, mem["peak"])
	clients := db["clients"].(map[string]any)
	assert.EqualValues(t, 3, clients["connected"])
}

// provider 未配線時は db が 0-stub のまま (= 従来挙動、200 維持)。
func TestQueueQueueStats_DBStubWhenNoRedis(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		info: map[string]*apiadmin.QueueInfoResult{"deliver": {Queue: "deliver"}},
	}
	h.SetQueueInspector(insp)
	// SetQueueRedis を呼ばない。

	rec := doPost(h.QueueQueueStats, `{"queue":"deliver"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	db := got["db"].(map[string]any)
	assert.EqualValues(t, 0, db["uptime"])
	mem := db["memory"].(map[string]any)
	assert.EqualValues(t, 0, mem["used"])
}
