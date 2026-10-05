package channels

import (
	"encoding/json"
	"testing"

	"github.com/elythia-network/elythia/internal/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubLogProvider は任意の log slice を返す test double。channel が呼び出す
// maxLen を記録する。
type stubLogProvider struct {
	entries        []json.RawMessage
	lastMaxLenSeen int
}

func (s *stubLogProvider) Log(maxLen int) []json.RawMessage {
	s.lastMaxLenSeen = maxLen
	return s.entries
}

// logger 配線済の factory 経由で historical log を返す (#571 item 2 main path)
func TestServerStatsFactory_RequestLogServesHistorical(t *testing.T) {
	logger := &stubLogProvider{
		entries: []json.RawMessage{
			json.RawMessage(`{"cpu":0.7}`),
			json.RawMessage(`{"cpu":0.6}`),
		},
	}
	factory := NewServerStatsFactory(logger)

	ctx := newCtx(nil)
	ch := factory(ctx)
	require.NoError(t, ch.Init(nil))
	ch.OnClientMessage("requestLog", json.RawMessage(`{"length":50}`))

	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "statsLog", ctx.sentType[0])
	body, ok := ctx.sentBody[0].([]json.RawMessage)
	require.True(t, ok, "expected []json.RawMessage from logger.Log")
	assert.Len(t, body, 2)
	assert.Equal(t, 50, logger.lastMaxLenSeen, "client length が provider に伝わる")
}

// requestLog 以外は無視
func TestServerStats_OnClientMessage_NonRequestLog(t *testing.T) {
	ctx := newCtx(nil)
	ch := factoryWithLogger(t, &stubLogProvider{})(ctx)
	require.NoError(t, ch.Init(nil))
	ch.OnClientMessage("noise", json.RawMessage(`{}`))
	assert.Empty(t, ctx.sentType, "unrelated message types ignored")
}

// body 不正 JSON でも panic せず length=0 (publisher default) で処理続行
func TestServerStats_RequestLog_InvalidBody(t *testing.T) {
	logger := &stubLogProvider{entries: []json.RawMessage{json.RawMessage(`{"cpu":0.1}`)}}
	ch := NewServerStatsFactory(logger)(newCtx(nil))
	require.NoError(t, ch.Init(nil))
	ch.OnClientMessage("requestLog", json.RawMessage(`not json`))
	// length=0 が provider に渡される (== publisher default 適用)
	assert.Equal(t, 0, logger.lastMaxLenSeen)
}

// 複数 channel が同 logger を共有しても干渉しない (concurrent-safe を logger
// 内部に委譲する想定の sanity check)
func TestServerStats_FactoryReturnsIndependentChannels(t *testing.T) {
	logger := &stubLogProvider{}
	factory := NewServerStatsFactory(logger)

	ctxA := newCtx(nil)
	ctxB := newCtx(nil)
	chA := factory(ctxA)
	chB := factory(ctxB)
	require.NoError(t, chA.Init(nil))
	require.NoError(t, chB.Init(nil))
	chA.Dispose()
	// Dispose した chA の影響が chB に及ばないこと
	chB.OnRedisEvent([]byte(`{"cpu":0.42}`))
	assert.Len(t, ctxB.sentType, 1)
}

// テスト helper: factory 経由 + nil 安全
func factoryWithLogger(t *testing.T, logger StatsLogProvider) stream.ChannelFactory {
	t.Helper()
	return NewServerStatsFactory(logger)
}
