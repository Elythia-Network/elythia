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
// Delayed は Bull 用語で、未来実行予定と失敗後の再試行待ちの合計 (#3962)。
//
// activeSincePrevTick (= tick 間に処理された数) は、ProcessedTotal (終わった
// ジョブの累計) の前 tick との差分で出す (#4220)。driver が累計を持たない
// (HasProcessedTotal が false) ときだけ、Completed の差分で代える (#654)。
// Completed は保持数で頭打ちになるので、代わりの値は保持数に達するまでしか
// 正しくない。前の tick で累計を読めていたのに false になったときは、読み出しの
// 一時的な失敗として0を出し、前の累計を残す。
type QueueStatsInfo struct {
	Active            int
	Pending           int
	Delayed           int
	Completed         int
	ProcessedTotal    int64
	HasProcessedTotal bool
}

// processedMark is the counter an earlier tick saw for one queue.
type processedMark struct {
	value int64
	// total は value が ProcessedTotal か (false なら Completed)。種類が
	// 変わった tick は差分を取らない。
	total bool
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

	// prevProcessed は前 tick 時点の処理数の累計を queue 名別に保持
	// する。次 tick で current - prev を activeSincePrevTick として
	// 送信し、Misskey TS の Bull `active` event count 互換にする (#654)。
	// loop goroutine 単独で書き込むので mutex 不要。
	prevProcessed map[string]processedMark
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
		prevProcessed: make(map[string]processedMark),
	}
}

// Start begins the collection loop.
//
// Stop()→Start() による再起動時は前 publisher の prevProcessed を捨て、
// 新規 loop の初回 tick で activeSincePrevTick=0 を出す (再起動またぎで
// delta を取っても意味のある値にならない)。
func (p *QueueStatsPublisher) Start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started || p.pub == nil || p.inspector == nil {
		return
	}
	p.prevProcessed = make(map[string]processedMark)
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
	// 同等APIがないため、前 tick から終わったジョブの数を出す (#4220。本家
	// ほど厳密でなくてもグラフは描画できる)。
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
// 初回 tick (prevProcessed に該当 queue の値が無い) は 0 を出して、
// 次 tick から実数の delta を出す。
func (p *QueueStatsPublisher) entryForQueue(qname string) queueStatsEntry {
	info, err := p.inspector.GetQueueInfo(qname)
	if err != nil || info == nil {
		return queueStatsEntry{}
	}
	cur := processedMark{value: int64(info.Completed)}
	if info.HasProcessedTotal {
		cur = processedMark{value: info.ProcessedTotal, total: true}
	}
	prev, ok := p.prevProcessed[qname]
	if ok && prev.total && !cur.total {
		// 累計を読めたのに、この tick だけ読めなかった (Redis の一時的な
		// エラー)。前の累計を残し、読めた次の tick で、この間の分もまとめて
		// 出す。上書きすると、この tick と次の tick の両方が 0 になる。
		return queueStatsEntry{
			Active:  info.Active,
			Waiting: info.Pending,
			Delayed: info.Delayed,
		}
	}
	var processed int
	if ok && prev.total == cur.total {
		// Completed は retention の prune でも減り、累計も metrics のキーを
		// 消せば戻るので、負値になり得る。負値は 0 にクランプ。
		if d := cur.value - prev.value; d > 0 {
			processed = int(d)
		}
	}
	p.prevProcessed[qname] = cur
	return queueStatsEntry{
		ActiveSincePrevTick: processed,
		Active:              info.Active,
		Waiting:             info.Pending,
		Delayed:             info.Delayed,
	}
}
