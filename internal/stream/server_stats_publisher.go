package stream

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
)

// ServerStatsPublisher periodically publishes host-level metrics (CPU, memory,
// disk, network) to the `serverStats` PubSub topic that the client-side
// "serverStats" channel and its widgets subscribe to.
//
// 本家Misskey ServerStatsService相当。収集間隔は2秒で、frontend グラフが
// 滑らかに動く粒度。gopsutilで OS 依存差を吸収する。
//
// requestLog (frontend が初期表示で historical 値を取得する) のため、各 tick
// の snapshot を ring buffer に保持して Log(maxLen) で返す。本家 TS は最大
// 200 件保持なので同じ defaults。
type ServerStatsPublisher struct {
	pub      PubSubPublisher
	interval time.Duration
	// idleRecheck は、無効の間に有効になったかを見直す間隔。切り替えは Reevaluate
	// で届くので、これは metaUpdated を取りこぼしたときの上限としてだけ置く。
	idleRecheck time.Duration
	stopCh      chan struct{}
	// wakeCh は Reevaluate の知らせ。容量 1 で、送れなければ既に届いている。
	wakeCh  chan struct{}
	wg      sync.WaitGroup
	started bool
	mu      sync.Mutex

	// 前回tickのnet counter値。差分を秒速に変換するため保持する。
	prevNetRx uint64
	prevNetTx uint64
	prevTick  time.Time

	// 前回tickのdisk I/O counter値。同じく差分で秒速を計算する。
	prevDiskRead  uint64
	prevDiskWrite uint64

	// logBuf は直近 ServerStatsLogMax 件分の snapshot を新しい順に保持する
	// O(1) append の circular buffer。TS の Array.unshift + length>200 で pop
	// と同じ semantics を head pointer 方式で実装。
	logBuf *statsRingBuffer

	// enabled は meta の enableServerMachineStats を返す。本家は無効なら
	// ServerStatsService が何もしないので、それに揃えて無効のあいだは集めも
	// 流しもしない。nil も無効として扱う。構築子の必須の引数で受ける (#3827)。
	enabled func() bool
}

// ServerStatsLogMax は requestLog 応答で返す最大件数。TS 本家と同じ 200。
const ServerStatsLogMax = 200

// serverStatsIdleRecheck is how often a disabled publisher looks at the gate on
// its own. 無効の間も 2 秒ごとに起きて meta を見ていた (2026-11-26 の監査で毎分
// 30 回、meta のキャッシュが切れた回は SELECT も流れた)。切り替えは meta の更新で
// Reevaluate が知らせるので、これは知らせを取りこぼしたとき (metaUpdated の
// publish の失敗など) に、再起動を待たずに追いつくための上限 (#4276)。
const serverStatsIdleRecheck = time.Minute

// NewServerStatsPublisher constructs a ServerStatsPublisher. interval<=0 は
// デフォルト (2秒) を使用する。
//
// enabled decides on each tick whether host metrics are collected and
// published. nil publishes nothing. 以前は setter で入れ、入れ忘れ (管理画面で
// 有効にしても値が流れない) を起動時の検査 (criticalWiring) が見ていた。渡し忘れを
// コンパイルで落とすため、構築子の必須の引数で受ける (#3827)。
func NewServerStatsPublisher(pub PubSubPublisher, interval time.Duration, enabled func() bool) *ServerStatsPublisher {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return &ServerStatsPublisher{
		pub:         pub,
		interval:    interval,
		idleRecheck: serverStatsIdleRecheck,
		stopCh:      make(chan struct{}),
		wakeCh:      make(chan struct{}, 1),
		logBuf:      newStatsRingBuffer(ServerStatsLogMax),
		enabled:     enabled,
	}
}

// Reevaluate makes the loop look at the gate now. The router calls it after
// the meta row cache was dropped (a local update or `metaUpdated` from another
// process), so enabling the stats in the control panel starts publishing
// without a restart, and disabling them stops it at once. It never blocks.
func (p *ServerStatsPublisher) Reevaluate() {
	select {
	case p.wakeCh <- struct{}{}:
	default:
	}
}

func (p *ServerStatsPublisher) isEnabled() bool {
	p.mu.Lock()
	fn := p.enabled
	p.mu.Unlock()
	return fn != nil && fn()
}

// Start begins the collection loop in a goroutine. 二重呼び出しはno-op。
func (p *ServerStatsPublisher) Start() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started || p.pub == nil {
		return
	}
	p.started = true
	p.wg.Add(1)
	go p.loop()
}

// Stop signals the loop to exit and waits for it to finish.
func (p *ServerStatsPublisher) Stop() {
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

// loop ticks every interval while the gate is open. While it is closed, it
// only wakes on Reevaluate or after idleRecheck.
func (p *ServerStatsPublisher) loop() {
	defer p.wg.Done()
	// 初回tickは即時実行して接続直後のウィジェットに早く値を届ける。
	enabled := p.tick()
	var ticker *time.Ticker
	idle := time.NewTimer(p.idleRecheck)
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
		idle.Stop()
	}()
	for {
		var next <-chan time.Time
		if enabled {
			idle.Stop()
			if ticker == nil {
				ticker = time.NewTicker(p.interval)
			}
			next = ticker.C
		} else {
			if ticker != nil {
				ticker.Stop()
				ticker = nil
			}
			idle.Reset(p.idleRecheck)
			next = idle.C
		}
		select {
		case <-p.stopCh:
			return
		case <-next:
			enabled = p.tick()
		case <-p.wakeCh:
			// 切り替わったときだけ tick する。有効のままの meta の更新で tick すると、
			// 2 秒の間隔の途中で 1 回余分に流れる。有効になったときは、起動のときと
			// 同じく最初の値をすぐに届ける。
			if p.isEnabled() != enabled {
				enabled = p.tick()
			}
		}
	}
}

// serverStatsSnapshot shapes the JSON body to match the upstream
// ServerStatsService output ({ cpu, mem{used,active}, net{rx,tx}, fs{r,w} }).
// frontend MkServerMetric widget がこのキー名を直接読む。
type serverStatsSnapshot struct {
	CPU float64 `json:"cpu"`
	Mem struct {
		Used   uint64 `json:"used"`
		Active uint64 `json:"active"`
	} `json:"mem"`
	Net struct {
		Rx uint64 `json:"rx"`
		Tx uint64 `json:"tx"`
	} `json:"net"`
	Fs struct {
		R uint64 `json:"r"`
		W uint64 `json:"w"`
	} `json:"fs"`
}

// tick collects and publishes one snapshot when the gate is open. It reports
// whether the gate was open.
func (p *ServerStatsPublisher) tick() bool {
	if !p.isEnabled() {
		// 管理画面で無効にした後は、有効だったあいだの履歴も requestLog で
		// 返さない。差分の起点も捨て、再び有効にした最初の tick を初回として扱う。
		p.logBuf.Reset()
		p.prevTick = time.Time{}
		return false
	}
	snap := p.collect()
	body, err := json.Marshal(snap)
	if err != nil {
		slog.Warn("server stats: marshal failed", "err", err)
		return true
	}
	raw := json.RawMessage(body)
	p.logBuf.Append(raw)
	if err := p.pub.Publish(context.Background(), "serverStats", raw); err != nil {
		slog.Warn("server stats: publish failed", "err", err)
	}
	return true
}

// Log returns the most recent up to maxLen snapshots, newest first. maxLen<=0
// または ServerStatsLogMax を超える値は ServerStatsLogMax に丸める。frontend
// 起動直後の requestLog で historical な timeline を埋めるために使う。
func (p *ServerStatsPublisher) Log(maxLen int) []json.RawMessage {
	// 無効にした直後、次の tick が履歴を捨てるまでの間も返さない。
	if !p.isEnabled() {
		return []json.RawMessage{}
	}
	return p.logBuf.Log(maxLen)
}

func (p *ServerStatsPublisher) collect() serverStatsSnapshot {
	var snap serverStatsSnapshot
	// CPU全体使用率 (非block: interval=0は前回計測との差分を返す)。
	// frontend widget (pie.vue等) は 0-1 範囲の fraction を期待し `value * 100`
	// で%表示するため、gopsutil の 0-100 パーセント値を 100 で割って合わせる。
	if cpus, err := cpu.Percent(0, false); err == nil && len(cpus) > 0 {
		snap.CPU = cpus[0] / 100
	}
	if vm, err := mem.VirtualMemory(); err == nil {
		snap.Mem.Used = vm.Total - vm.Available
		snap.Mem.Active = vm.Active
	}
	// 秒速換算するために前回tickからの経過秒数を使う。初回tickでは
	// prevTickがゼロ値なので差分計算せず0にする (次回tickから値が出る)。
	now := time.Now()
	elapsed := now.Sub(p.prevTick).Seconds()
	firstTick := p.prevTick.IsZero()
	p.prevTick = now

	if ios, err := net.IOCounters(false); err == nil && len(ios) > 0 {
		rx, tx := ios[0].BytesRecv, ios[0].BytesSent
		if !firstTick && elapsed > 0 {
			snap.Net.Rx = diffPerSec(rx, p.prevNetRx, elapsed)
			snap.Net.Tx = diffPerSec(tx, p.prevNetTx, elapsed)
		}
		p.prevNetRx = rx
		p.prevNetTx = tx
	}
	if ios, err := disk.IOCounters(); err == nil {
		var r, w uint64
		for _, c := range ios {
			r += c.ReadBytes
			w += c.WriteBytes
		}
		if !firstTick && elapsed > 0 {
			snap.Fs.R = diffPerSec(r, p.prevDiskRead, elapsed)
			snap.Fs.W = diffPerSec(w, p.prevDiskWrite, elapsed)
		}
		p.prevDiskRead = r
		p.prevDiskWrite = w
	}
	return snap
}

// diffPerSec は cumulative counter の差分を秒速に換算する。counter reset
// (OS再起動など) では curr < prev になり得るので、アンダーフローを0で
// クランプする。
func diffPerSec(curr, prev uint64, elapsedSec float64) uint64 {
	if curr < prev || elapsedSec <= 0 {
		return 0
	}
	return uint64(float64(curr-prev) / elapsedSec)
}
