package deliveryhealth

import (
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/elythia-network/elythia/internal/misc/jsdate"
)

// latencyBucketsMs are the upper bounds (inclusive) of the latency histogram,
// in milliseconds. 最後の +Inf は暗黙 (len(latencyBucketsMs) 番目)。
//
// 正確な分位数は取らない。「遅い」が分かればよい用途に、reservoir や t-digest の
// コストは見合わない。境界は「速い / 普通 / 遅い / 諦めた」が判別できる粗さ。
var latencyBucketsMs = [...]int64{50, 100, 250, 500, 1000, 2500, 5000, 10000}

// latencyBucketCount includes the implicit +Inf bucket.
const latencyBucketCount = len(latencyBucketsMs) + 1

// latencyBucketIndex maps a duration to its histogram bucket.
func latencyBucketIndex(d time.Duration) int {
	ms := d.Milliseconds()
	for i, bound := range latencyBucketsMs {
		if ms <= bound {
			return i
		}
	}
	return len(latencyBucketsMs) // +Inf
}

// hostCounters accumulates one host's outcomes between flushes.
type hostCounters struct {
	byClass map[OutcomeClass]int64
	latency [latencyBucketCount]int64
	last    *LastError
}

// LastError is the most recent failing attempt for a host.
type LastError struct {
	At      jsdate.Time  `json:"at"`
	Class   OutcomeClass `json:"class"`
	Status  int          `json:"status"`
	Message string       `json:"message"`
}

// Aggregator accumulates delivery outcomes in memory so the hot path never
// touches Redis.
//
// 配送は高頻度なので 1 件ごとに Redis を叩くと無視できない負荷になる。ここで
// 足し込み、Drain() でまとめて外へ出す。
//
// ホスト数は LRU で上限を設ける。配送先は自分の連合グラフに縛られるので通常は
// 収まるが、上限が無いと想定外の入力でメモリが伸びる余地を残すことになる。
// evict されたホストの計数はその周期分だけ失われる (観測が目的なので許容する)。
type Aggregator struct {
	mu    sync.Mutex
	hosts *lru.Cache[string, *hostCounters]
	clock func() time.Time
	// succeeded は「成功として数える分類か」の判定。送信と受信で違うので
	// 注入する (方向ごとに別実装を起こすと集計ロジックが二重になる)。
	succeeded func(OutcomeClass) bool
	// evicted は上限超過で捨てたホスト数の累計。運用者が上限の妥当性を
	// 判断できるよう外へ出す。
	evicted int64
}

// DefaultMaxHosts bounds the in-memory host table.
const DefaultMaxHosts = 2048

// NewAggregator constructs an Aggregator. maxHosts <= 0 uses DefaultMaxHosts.
func NewAggregator(maxHosts int) *Aggregator {
	if maxHosts <= 0 {
		maxHosts = DefaultMaxHosts
	}
	a := &Aggregator{clock: time.Now, succeeded: func(c OutcomeClass) bool { return c.Succeeded() }}
	// lru.New はサイズが正なら error を返さない。上で正に正規化済み。
	cache, _ := lru.NewWithEvict[string, *hostCounters](maxHosts, func(string, *hostCounters) {
		a.evicted++
	})
	a.hosts = cache
	return a
}

// SetClockForTest overrides the time source.
func (a *Aggregator) SetClockForTest(fn func() time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.clock = fn
}

// Record adds one attempt. 空 host は捨てる (inbox URL の parse 失敗時に来る)。
func (a *Aggregator) Record(host string, o Outcome) {
	if host == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	c, ok := a.hosts.Get(host)
	if !ok {
		c = &hostCounters{byClass: make(map[OutcomeClass]int64, len(AllClasses))}
		a.hosts.Add(host, c)
	}
	c.byClass[o.Class]++
	c.latency[latencyBucketIndex(o.Latency)]++
	if !a.succeeded(o.Class) {
		c.last = &LastError{
			At:      jsdate.NewTime(a.clock().UTC()),
			Class:   o.Class,
			Status:  o.Status,
			Message: truncateErr(o.Err),
		}
	}
}

// Delta is one host's accumulated counts since the previous Drain.
type Delta struct {
	Host      string
	ByClass   map[OutcomeClass]int64
	Latency   [latencyBucketCount]int64
	LastError *LastError
}

// Drain returns the accumulated deltas and resets the table.
//
// **戻り値は呼び出し側の所有物**にする (内部の map を貸し出さない)。flush が
// 非同期に走る前提なので、共有すると次の Record と競合する。
func (a *Aggregator) Drain() []Delta {
	a.mu.Lock()
	defer a.mu.Unlock()

	keys := a.hosts.Keys()
	out := make([]Delta, 0, len(keys))
	for _, host := range keys {
		c, ok := a.hosts.Peek(host)
		if !ok {
			continue
		}
		d := Delta{Host: host, ByClass: make(map[OutcomeClass]int64, len(c.byClass)), LastError: c.last}
		for k, v := range c.byClass {
			d.ByClass[k] = v
		}
		d.Latency = c.latency
		out = append(out, d)
	}
	a.hosts.Purge()
	return out
}

// SetSucceeded overrides the success predicate (受信側で使う、#2471)。
func (a *Aggregator) SetSucceeded(fn func(OutcomeClass) bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if fn != nil {
		a.succeeded = fn
	}
}

// EvictedHosts returns how many hosts were dropped for exceeding the cap.
func (a *Aggregator) EvictedHosts() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.evicted
}
