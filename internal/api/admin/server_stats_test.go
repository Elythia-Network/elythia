package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubIPRepo struct{}

func (s *stubIPRepo) Observe(_, _ string, _ time.Time) error              { return nil }
func (s *stubIPRepo) ListByUser(_ string, _ int) ([]*model.UserIP, error) { return nil, nil }
func (s *stubIPRepo) DeleteLastSeenBefore(_ time.Time) (int64, error)     { return 0, nil }

func TestGetIndexStats(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusOK, doPost(h.GetIndexStats, `{}`, adminUser).Code)
}

func TestGetTableStats(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusOK, doPost(h.GetTableStats, `{}`, adminUser).Code)
}

func TestGetUserIPs_NilRepo(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusOK, doPost(h.GetUserIPs, `{}`, adminUser).Code)
}

func TestGetUserIPs_InvalidParam(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetUserIPRepo(&stubIPRepo{})
	assert.Equal(t, http.StatusBadRequest, doPost(h.GetUserIPs, `{}`, adminUser).Code)
}

func TestGetUserIPs_Success(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetUserIPRepo(&stubIPRepo{})
	rec := doPost(h.GetUserIPs, `{"userId":"u1"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// admin/server-info は upstream 同様 enableServerMachineStats gate を持たず、
// 常に live な full shape を返す (機械統計が無効でも moderator には実値)。
func TestServerInfo_NoGateAlwaysLive(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta.EnableServerMachineStats = false // gate が効かないことを確認
	rec := doPost(h.ServerInfo, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	// machine は実値 (string, "?" でない)。
	machine, ok := got["machine"].(string)
	require.True(t, ok)
	assert.NotEmpty(t, machine)
	assert.NotEqual(t, "?", machine)
	// 全 admin field が present。
	for _, k := range []string{"machine", "os", "node", "psql", "redis", "cpu", "mem", "fs", "net"} {
		_, ok := got[k]
		assert.True(t, ok, k+" は present")
	}
}

// enableServerMachineStats = true で本家 server-info shape を返す。
// machine は string、os/node が埋まり、psql/redis は DB/Redis 未配線で空文字。
func TestServerInfo_EnabledShape(t *testing.T) {
	h, _, metaRepo, _ := newTestHandler(t)
	metaRepo.Meta.EnableServerMachineStats = true
	rec := doPost(h.ServerInfo, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	// machine は string (型不一致 regression guard)。
	machine, ok := got["machine"].(string)
	require.True(t, ok, "machine must be a string, got %T", got["machine"])
	assert.NotEmpty(t, machine)
	assert.NotEmpty(t, got["os"])
	assert.NotEmpty(t, got["node"])
	// psql / redis は adminDB / queueRedis 未配線なので空文字 (string 型を維持)。
	assert.Equal(t, "", got["psql"])
	assert.Equal(t, "", got["redis"])
	cpu := got["cpu"].(map[string]any)
	assert.Greater(t, cpu["cores"].(float64), float64(0))
	net := got["net"].(map[string]any)
	_, hasIface := net["interface"]
	assert.True(t, hasIface)
}

// adminDB 配線時は SHOW server_version から psql を埋める (実 PG が必要)。
func TestServerInfo_PostgresVersionFromDB(t *testing.T) {
	if integrationDB == nil {
		t.Skip("integrationDB unavailable (set TEST_DB_HOST or run with Docker daemon for testcontainer)")
	}
	h, _, _, _ := newTestHandler(t)
	h.SetAdminDB(integrationDB)
	rec := doPost(h.ServerInfo, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	psql, _ := got["psql"].(string)
	assert.NotEmpty(t, psql, "adminDB 配線時は SHOW server_version で psql が埋まる")
}

// queueRedis 配線時は redis INFO から redis_version を埋める。
func TestServerInfo_RedisVersionFromInfo(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetQueueRedis(stubQueueRedisInfo{raw: "# Server\r\nredis_version:7.2.4\r\n"})
	rec := doPost(h.ServerInfo, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "7.2.4", got["redis"])
}

// erroringQueueRedis は QueueRedisInfo が常にエラーを返す stub。
type erroringQueueRedis struct{}

func (erroringQueueRedis) QueueRedisInfo(context.Context) (string, error) {
	return "", assertError{}
}

// redis INFO 取得失敗時は redis を空文字に fallback する (best-effort)。
func TestServerInfo_RedisVersionError(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetQueueRedis(erroringQueueRedis{})
	rec := doPost(h.ServerInfo, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "", got["redis"])
}
