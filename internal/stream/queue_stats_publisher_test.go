package stream

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubQueueInspector serves canned QueueStatsInfo for one queue name.
type stubQueueInspector struct {
	info *QueueStatsInfo
	err  error
	hits int
}

func (s *stubQueueInspector) GetQueueInfo(_ string) (*QueueStatsInfo, error) {
	s.hits++
	if s.err != nil {
		return nil, s.err
	}
	return s.info, nil
}

func TestQueueStatsPublisher_PublishesDeliverAndInboxDepths(t *testing.T) {
	// stub は queue 名で値を切り替えられるようにする (deliver / inbox で
	// 同じ inspector を共有するため)。
	infos := map[string]*QueueStatsInfo{
		"deliver": {Active: 3, Pending: 7, Scheduled: 2, Retry: 1},
		"inbox":   {Active: 5, Pending: 11, Scheduled: 0, Retry: 4},
	}
	insp := &perQueueInspector{infos: infos}
	pub := &capturePubSub{}
	p := NewQueueStatsPublisher(insp, pub, 30*time.Millisecond)
	p.Start()
	time.Sleep(60 * time.Millisecond)
	p.Stop()

	calls := pub.snapshot()
	require.NotEmpty(t, calls)
	for _, c := range calls {
		assert.Equal(t, "queueStats", c.channel)
		var body struct {
			Deliver struct {
				Active              int `json:"active"`
				Waiting             int `json:"waiting"`
				Delayed             int `json:"delayed"`
				ActiveSincePrevTick int `json:"activeSincePrevTick"`
			} `json:"deliver"`
			Inbox struct {
				Active              int `json:"active"`
				Waiting             int `json:"waiting"`
				Delayed             int `json:"delayed"`
				ActiveSincePrevTick int `json:"activeSincePrevTick"`
			} `json:"inbox"`
		}
		require.NoError(t, json.Unmarshal(c.payload, &body))
		assert.Equal(t, 3, body.Deliver.Active)
		assert.Equal(t, 7, body.Deliver.Waiting)
		// Bull delayed = Scheduled + Retry
		assert.Equal(t, 3, body.Deliver.Delayed)
		// inbox は #534 で専用 queue 化、#565 で worker 化されたので
		// 実数を出す (#654)。
		assert.Equal(t, 5, body.Inbox.Active)
		assert.Equal(t, 11, body.Inbox.Waiting)
		assert.Equal(t, 4, body.Inbox.Delayed)
		// stub は Completed を 0 で返し続けるので delta も 0 で安定。
		assert.Equal(t, 0, body.Inbox.ActiveSincePrevTick)
	}
}

// seqCompletedInspector は queue 名ごとに Completed 累計のシーケンスを
// 返す stub。tick を進めるたびに次の値を返す。activeSincePrevTick が
// 前 tick との差分で計算されることを決定的に検証するため (#654)。
type seqCompletedInspector struct {
	deliver, inbox []int
	deliverIdx     int
	inboxIdx       int
}

func (s *seqCompletedInspector) GetQueueInfo(qname string) (*QueueStatsInfo, error) {
	var seq []int
	var idx *int
	switch qname {
	case "deliver":
		seq, idx = s.deliver, &s.deliverIdx
	case "inbox":
		seq, idx = s.inbox, &s.inboxIdx
	default:
		return &QueueStatsInfo{}, nil
	}
	if len(seq) == 0 {
		return &QueueStatsInfo{}, nil
	}
	i := *idx
	if i >= len(seq) {
		i = len(seq) - 1 // 末尾以降は最終値で固定
	} else {
		*idx++
	}
	return &QueueStatsInfo{Completed: seq[i]}, nil
}

// TestQueueStatsPublisher_ActiveSincePrevTickIsCompletedDelta は
// activeSincePrevTick が Completed の前 tick との差分で計算され、
// Completed が retention の prune で減る挙動で
// 負値になっても 0 にクランプされることを guard する (#654 review)。
//
// tick() を直接駆動する internal test。Start()/Stop() の goroutine 経由
// だと tick タイミングが非決定的なので避ける。
func TestQueueStatsPublisher_ActiveSincePrevTickIsCompletedDelta(t *testing.T) {
	insp := &seqCompletedInspector{
		// deliver: 10 → 15 (+5) → 3 (rolling reset、負値 clamp) → 8 (+5)
		deliver: []int{10, 15, 3, 8},
		// inbox は固定 0 で「他 queue の delta が deliver のみで動く」
		// ことを示す。
		inbox: []int{0, 0, 0, 0},
	}
	pub := &capturePubSub{}
	p := NewQueueStatsPublisher(insp, pub, 0)

	// 各 tick で activeSincePrevTick の期待値を assert。
	expected := []int{
		0, // 初回: prev なし
		5, // 10 → 15
		0, // 15 → 3 (負値 clamp)
		5, // 3 → 8
	}
	for i, want := range expected {
		p.tick()
		calls := pub.snapshot()
		require.Len(t, calls, i+1, "expected one publish per tick")
		var body struct {
			Deliver struct {
				ActiveSincePrevTick int `json:"activeSincePrevTick"`
			} `json:"deliver"`
		}
		require.NoError(t, json.Unmarshal(calls[i].payload, &body))
		assert.Equalf(t, want, body.Deliver.ActiveSincePrevTick,
			"tick %d: deliver activeSincePrevTick", i)
	}
}

// Start() → Stop() → Start() の再起動で prevCompleted が reset され、
// 新 publisher の初回 tick が 0 を出すことを確認 (#654 review)。
func TestQueueStatsPublisher_RestartResetsPrevCompleted(t *testing.T) {
	insp := &seqCompletedInspector{
		deliver: []int{100, 105}, // restart 後 1 回目 = 100、2 回目 = 105
		inbox:   []int{0, 0},
	}
	pub := &capturePubSub{}
	p := NewQueueStatsPublisher(insp, pub, 0)

	// 1 サイクル目: tick で prevCompleted=100 が記録される
	p.tick()

	// prevCompleted が外から見えないので Start/Stop 経由で reset を起こす
	p.Start()
	p.Stop()

	// 2 サイクル目: restart 後の最初の tick は prev なし扱いで 0
	p.tick()
	calls := pub.snapshot()
	require.NotEmpty(t, calls)
	var body struct {
		Deliver struct {
			ActiveSincePrevTick int `json:"activeSincePrevTick"`
		} `json:"deliver"`
	}
	require.NoError(t, json.Unmarshal(calls[len(calls)-1].payload, &body))
	assert.Equal(t, 0, body.Deliver.ActiveSincePrevTick,
		"restart 後の初回 tick は prev なし扱いで 0")
}

// perQueueInspector returns a different QueueStatsInfo per queue name so
// tests can verify deliver / inbox are queried independently.
type perQueueInspector struct {
	infos map[string]*QueueStatsInfo
}

func (s *perQueueInspector) GetQueueInfo(qname string) (*QueueStatsInfo, error) {
	return s.infos[qname], nil
}

func TestQueueStatsPublisher_InspectorErrorPublishesZeros(t *testing.T) {
	pub := &capturePubSub{}
	insp := &stubQueueInspector{err: errors.New("boom")}
	p := NewQueueStatsPublisher(insp, pub, 30*time.Millisecond)
	p.Start()
	time.Sleep(50 * time.Millisecond)
	p.Stop()

	calls := pub.snapshot()
	require.NotEmpty(t, calls)
	var body struct {
		Deliver struct {
			Active int `json:"active"`
		} `json:"deliver"`
	}
	require.NoError(t, json.Unmarshal(calls[0].payload, &body))
	assert.Equal(t, 0, body.Deliver.Active)
}

func TestQueueStatsPublisher_NilInspectorNoOp(t *testing.T) {
	pub := &capturePubSub{}
	p := NewQueueStatsPublisher(nil, pub, 30*time.Millisecond)
	p.Start()
	time.Sleep(20 * time.Millisecond)
	p.Stop()
	assert.Empty(t, pub.snapshot())
}

func TestQueueStatsPublisher_DoubleStopSafe(t *testing.T) {
	pub := &capturePubSub{}
	insp := &stubQueueInspector{info: &QueueStatsInfo{}}
	p := NewQueueStatsPublisher(insp, pub, 50*time.Millisecond)
	p.Start()
	p.Stop()
	p.Stop() // no panic
}

// publisher が ring buffer に snapshot を貯めて Log() で返せること (#571 item 2)
func TestQueueStatsPublisher_LogReturnsRecentSnapshots(t *testing.T) {
	insp := &stubQueueInspector{info: &QueueStatsInfo{Active: 1, Pending: 2, Scheduled: 3}}
	pub := &capturePubSub{}
	p := NewQueueStatsPublisher(insp, pub, 10*time.Millisecond)
	p.Start()
	time.Sleep(50 * time.Millisecond)
	p.Stop()

	logs := p.Log(0)
	require.NotEmpty(t, logs, "Log should accumulate snapshots from each tick")
	for _, raw := range logs {
		var body map[string]any
		require.NoError(t, json.Unmarshal(raw, &body))
		assert.Contains(t, body, "deliver")
		assert.Contains(t, body, "inbox")
	}
}

// Log(maxLen) で件数制限
func TestQueueStatsPublisher_LogRespectsMaxLen(t *testing.T) {
	insp := &stubQueueInspector{info: &QueueStatsInfo{}}
	pub := &capturePubSub{}
	p := NewQueueStatsPublisher(insp, pub, 5*time.Millisecond)
	p.Start()
	time.Sleep(50 * time.Millisecond)
	p.Stop()

	logs := p.Log(2)
	assert.LessOrEqual(t, len(logs), 2)
}

// ring buffer は QueueStatsLogMax 件で頭打ち
func TestQueueStatsPublisher_LogCapAtMax(t *testing.T) {
	insp := &stubQueueInspector{info: &QueueStatsInfo{}}
	pub := &capturePubSub{}
	p := NewQueueStatsPublisher(insp, pub, 0)
	for i := 0; i < 250; i++ {
		p.logBuf.Append(json.RawMessage(`{}`))
	}
	assert.Equal(t, QueueStatsLogMax, len(p.Log(0)))
}

// TestQueueStatsPublisher_DefaultIntervalMatchesUpstream fixes the default
// interval to the one of Misskey's QueueStatsService (10 seconds, #3592).
func TestQueueStatsPublisher_DefaultIntervalMatchesUpstream(t *testing.T) {
	p := NewQueueStatsPublisher(&stubQueueInspector{}, &capturePubSub{}, 0)
	assert.Equal(t, 10*time.Second, p.interval)
	assert.Equal(t, 10*time.Second, DefaultQueueStatsInterval)
}
