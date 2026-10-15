package stream

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

// QueueInspector is the minimal subset of queue.Inspector needed for emitting
// streaming stats. interfaceで受け取ることでテストでstub化できる。
type QueueInspector interface {
	GetQueueInfo(qname string) (*QueueStatsInfo, error)
}

// QueueStatsInfo mirrors the relevant fields from queue.InspectorInfo so that
// this package does not have to import internal/queue. 循環依存防止。
// Delayed は Bull 用語で、driver の Scheduled (未来実行予定) + Retry
// (失敗後の再試行待ち) 合計に対応するので両方の field を保持する。
//
// Completed は「累計の処理完了数」。前 tick との差分で
// activeSincePrevTick (= tick 間に処理された数) を計算するために保持する
// (#654)。
type QueueStatsInfo struct {
	Active    int
	Pending   int
	Scheduled int
	Retry     int
	Completed int
}

// QueueStatsPublisher periodically queries queue depths and publishes
// them to the `queueStats` PubSub topic for the admin dashboard / widget.
//
// 本家Misskey QueueStatsServiceは `deliver` と `inbox` の2キーを出す。
// mk-go では #534 で inbox を専用 queue 化、#565 で HTTP handler を
// verify-in-worker 化したため、`deliver` と同じく実数を出力する。
// 他の queue (push / webhook等) は出さない (frontendが見ていない)。
type QueueStatsPublisher struct {
	inspector QueueInspector
	pub       PubSubPublisher
	interval  time.Duration
	stopCh    chan struct{}
	wg        sync.WaitGroup
	started   bool
	mu        sync.Mutex

	// logBuf: server_stats と同等の O(1) circular buffer (新しい順、
	// 最大 QueueStatsLogMax 件)。requestLog 応答用。
	logBuf *statsRingBuffer

	// prevCompleted は前 tick 時点の Completed 累計を queue 名別に保持
	// する。次 tick で current - prev を activeSincePrevTick として
	// 送信し、Misskey TS の Bull `active` event count 互換にする (#654)。
	// loop goroutine 単独で書き込むので mutex 不要。
	prevCompleted map[string]int
}

// QueueStatsLogMax は requestLog 応答で返す最大件数。TS 本家と同じ 200。
const QueueStatsLogMax = 200

// DefaultQueueStatsInterval is how often the queue stats are collected and
// published when NewQueueStatsPublisher is given no interval. It is the
// interval of Misskey's QueueStatsService.
//
// 以前は 3 秒で、待機中の Redis の往復の約 2 割を占めていた (#3592)。
// requestLog の 200 件が覆う時間も本家と同じ約 33 分になる。
const DefaultQueueStatsInterval = 10 * time.Second

// NewQueueStatsPublisher constructs a QueueStatsPublisher. interval<=0 は
// DefaultQueueStatsInterval を使用する。
func NewQueueStatsPublisher(inspector QueueInspector, pub PubSubPublisher, interval time.Duration) *QueueStatsPublisher {
	if interval <= 0 {
		interval = DefaultQueueStatsInterval
	}
	return &QueueStatsPublisher{
		inspector:     inspector,
		pub:           pub,
		interval:      interval,
		stopCh:        make(chan struct{}),
		logBuf:        newStatsRingBuffer(QueueStatsLogMax),
		prevCompleted: make(map[string]int),
	}
}

// Start begins the collection loop.
//
// Stop()→Start() による再起動時は前 publisher の prevCompleted を捨て、
// 新規 loop の初回 tick で activeSincePrevTick=0 を出す (再起動またぎで
// delta を取っても意味のある値にならない)。
func (p *QueueStatsPublisher) Start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started || p.pub == nil || p.inspector == nil {
		return
	}
	p.prevCompleted = make(map[string]int)
	p.started = true
	p.wg.Add(1)
	go p.loop()
}

// Stop signals the loop to exit and waits for completion.
func (p *QueueStatsPublisher) Stop() {
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return
	}
	close(p.stopCh)
	p.started = false
	p.mu.Unlock()
	p.wg.Wait()
}

func (p *QueueStatsPublisher) loop() {
	defer p.wg.Done()
	p.tick()
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-t.C:
			p.tick()
		}
	}
}

// queueStatsBody matches the upstream { deliver, inbox } schema so that the
// frontend admin/overview.queue chart accepts our payload directly.
type queueStatsBody struct {
	Deliver queueStatsEntry `json:"deliver"`
	Inbox   queueStatsEntry `json:"inbox"`
}

type queueStatsEntry struct {
	// ActiveSincePrevTick はBullのactive eventカウントに相当。driver には
	// 同等APIがないため、前 tick からの completed 差分を出す (本家ほど厳密で
	// なくてもグラフは描画できる)。
	ActiveSincePrevTick int `json:"activeSincePrevTick"`
	Active              int `json:"active"`
	Waiting             int `json:"waiting"`
	Delayed             int `json:"delayed"`
}

func (p *QueueStatsPublisher) tick() {
	body := queueStatsBody{
		Deliver: p.entryForQueue(deliverQueueName),
		Inbox:   p.entryForQueue(inboxQueueName),
	}
	raw, err := json.Marshal(body)
	if err != nil {
		slog.Warn("queue stats: marshal failed", "err", err)
		return
	}
	rawMsg := json.RawMessage(raw)
	p.logBuf.Append(rawMsg)
	if err := p.pub.Publish(context.Background(), "queueStats", rawMsg); err != nil {
		slog.Warn("queue stats: publish failed", "err", err)
	}
}

// Log returns the most recent up to maxLen snapshots, newest first. server_stats
// の Log と同じ semantics。
func (p *QueueStatsPublisher) Log(maxLen int) []json.RawMessage {
	return p.logBuf.Log(maxLen)
}

// deliverQueueName / inboxQueueName は本番の queue 名。テストで差し
// 替えられるように変数化する。internal/queue の InboxQueueName / queue
// 定義と一致させること。
var (
	deliverQueueName = "deliver"
	inboxQueueName   = "inbox"
)

// entryForQueue queries one queue and translates its depth into the
// Bull-shaped queueStatsEntry the frontend WidgetJobQueue consumes.
//
// activeSincePrevTick は前 tick 以降に処理完了したジョブ数の delta で、
// Bull の active event count (= worker が pick up した瞬間に発火する
// counter) と意味的には完全互換ではないが、widget の Process 列が
// 「前 tick から処理された量」を表示する仕様には合致する。snapshot 時
// の Active 数より低トラフィック環境で観測しやすい。
//
// 初回 tick (prevCompleted に該当 queue の値が無い) は 0 を出して、
// 次 tick から実数の delta を出す。
func (p *QueueStatsPublisher) entryForQueue(qname string) queueStatsEntry {
	info, err := p.inspector.GetQueueInfo(qname)
	if err != nil || info == nil {
		return queueStatsEntry{}
	}
	var processed int
	if prev, ok := p.prevCompleted[qname]; ok {
		// Completed は retention の prune でも減るので負値になり得る。
		// 負値は 0 にクランプ。
		if d := info.Completed - prev; d > 0 {
			processed = d
		}
	}
	p.prevCompleted[qname] = info.Completed
	return queueStatsEntry{
		ActiveSincePrevTick: processed,
		Active:              info.Active,
		Waiting:             info.Pending,
		// Bull の delayed は Scheduled (未来実行予定) と Retry
		// (失敗後再試行待ち) の両方を含む。
		Delayed: info.Scheduled + info.Retry,
	}
}
