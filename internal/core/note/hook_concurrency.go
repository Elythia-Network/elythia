package note

import (
	"context"
	"log/slog"
	"runtime"
	"sync"

	"github.com/elythia-network/elythia/internal/bgwork"
)

// ノート作成後のベストエフォートフックの同時実行数を絞る。
//
// **絞りが無いと応答のテールが伸びる。** 投稿ごとに 7 つのフックを goroutine へ
// 投げるので、負荷時には数百の goroutine が GOMAXPROCS 個の P を奪い合う。応答を
// 返す goroutine もその列に並ぶため、p50 は速いまま p95 だけが伸びる。
//
// 2 CPU 環境の実測 (投稿 20 VU / 30 秒、フォロワー 9 人):
//
//	実効総枠  notes/create p95
//	      2   95 ms
//	      8   105-109 ms
//	     16   159 ms
//	     56   181 ms
//	   無制限   181 ms
//
// 資源そのものは余っていた (CPU 1.0/2.0、DB 接続は 10 でも足り、DB へのアクセスは
// Misskey TS の 1/2.3)。足りないのは**実行の順番**なので、増やすのではなく絞る。
//
// 枠を 2 つに分けるのは fanout を守るため。**1 つを共有すると、他のフックが枠を
// 占有したときに fanout が飢える。** 同じ総枠でも共有だと反映が壊れた:
//
//	共有 2 枠   反映遅延 中央値 5,702 ms、25 回中 20 回が 15 秒以内に届かず
//	分離 2 枠   反映遅延 中央値 89 ms、取りこぼし 2 回
//
// 総枠を揃えた比較 (共有 8 と fanout 4 + その他 4) では応答・反映とも測定分散の
// 範囲で同等だった。ベンチには webhook もアンテナも無く、枠を長く握るフックが
// 存在しないため差が出ない。**本番にはそれがあるので、代償のない保険として分ける。**

// hookKind identifies which best-effort hook a goroutine belongs to.
type hookKind int

const (
	// hookFanout はタイムラインへの配布。**投稿が読み手に届くまでの時間**を
	// 決めるので、他のフックと枠を共有させない。
	hookFanout hookKind = iota
	// hookOther はそれ以外 (通知 / 連合 / チャンネル / 索引 / チャート / webhook)。
	hookOther
	hookKindCount
)

// defaultHookConcurrencyFactor multiplies GOMAXPROCS to get the default slot
// count per semaphore.
//
// 2 CPU の実測で総枠 8 (= GOMAXPROCS x 4) までは緩やかな劣化、16 を超えると急に
// 悪化した。枠は 2 つあるので 1 つあたり GOMAXPROCS x 2 とすると総枠が x4 に収まる。
// 8 コアなら 1 枠 16 / 総枠 32。
const defaultHookConcurrencyFactor = 2

var (
	hookSemsMu sync.RWMutex
	hookSems   [hookKindCount]chan struct{}
)

func init() { SetHookConcurrency(0) }

// SetHookConcurrency sets how many best-effort hooks of each kind may run at
// once. n <= 0 selects the default (GOMAXPROCS * defaultHookConcurrencyFactor).
//
// **起動時に 1 度だけ呼ぶこと。** 実行中に差し替えると、古い semaphore を掴んだ
// goroutine が解放先を失う。
func SetHookConcurrency(n int) {
	if n <= 0 {
		n = runtime.GOMAXPROCS(0) * defaultHookConcurrencyFactor
	}
	if n < 1 {
		n = 1
	}
	hookSemsMu.Lock()
	defer hookSemsMu.Unlock()
	for i := range hookSems {
		hookSems[i] = make(chan struct{}, n)
	}
	slog.Debug("note hook concurrency configured", "perKind", n, "kinds", int(hookKindCount))
}

func hookSem(k hookKind) chan struct{} {
	hookSemsMu.RLock()
	defer hookSemsMu.RUnlock()
	return hookSems[k]
}

// safeGo runs fn in a new goroutine of g, recovering from panics.
// ベストエフォートフックでパニックが発生してもプロセス全体が落ちないようにする。
func safeGo(g *bgwork.Group, fn func()) { safeGoKind(g, hookOther, fn) }

// safeGoKind is safeGo with an explicit hook kind, so the slot budget of one
// kind cannot be consumed by another.
//
// g is the group of the server generation (#3661). A hook still waiting for
// its slot when the generation stop cancels the context of g is dropped
// (and counted in g.Dropped) instead of run.
func safeGoKind(g *bgwork.Group, k hookKind, fn func()) {
	sem := hookSem(k)
	g.Go(func(ctx context.Context) {
		// **枠は goroutine の中で取る。** 外で取ると呼び出し元 (= 応答を返す
		// 経路) が待たされ、非同期にした意味が無くなる。
		//
		// 世代の停止の待ちが終わり近くになっても枠が取れなければ、走らせずに捨てる
		// (#3661)。ctx は待ちの 3/4 を過ぎるまで切れないので、待ちの間に枠が空けば
		// 走る。まだ何も書いていないので捨てても途中の状態は残らない。走らせると
		// 閉じる DB へ書きに行く。捨てたことは件数とログに残す (配送や通知は冪等な
		// 後追いが無いため、黙って消すと気付けない)。
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			g.RecordDropped("note hook", "no slot before the generation stop gave up")
			return
		}
		defer func() { <-sem }()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("recovered panic in best-effort hook", "panic", r)
			}
		}()
		fn()
	})
}
