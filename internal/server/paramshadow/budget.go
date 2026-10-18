package paramshadow

import (
	"runtime"
	"sync"
	"time"
)

// DefaultBudgetBurst is how much validation time may accumulate before the
// budget stops validating: 100ms.
//
// 普通のリクエストの検証は1件で数十µs(notes/createで約16µs、i/updateで約50µs)
// なので、100msは数千件の続けての検証にあたる。普通の負荷の山では当たらない。
const DefaultBudgetBurst = 100 * time.Millisecond

// minBudgetCores is the least validation time per second the budget allows,
// in cores: 0.1 core (100ms per second).
const minBudgetCores = 0.1

// DefaultBudgetCores is the validation time per second the budget allows,
// in cores: GOMAXPROCS/16, and at least 0.1.
//
// **検証が使うCPUを、全体の16分の1(約6%)の目安に抑える。** 同時実行の枠は
// 「同時に何件か」を抑えるだけで、1件が軽ければ1つの枠が1コアを使い続けられた
// (#3677のレビュー2周目M-1で、2コアの機械に約0.85コアを足せた)。下限の0.1コアは、
// 1〜2コアの機械でも普通の負荷が予算に当たらないための値で、1コアでは10%、
// 2コアでは6.25%(GOMAXPROCS/16 = 0.125の方が大きい)になる。
func DefaultBudgetCores() float64 {
	return max(minBudgetCores, float64(runtime.GOMAXPROCS(0))/16)
}

// budget is a leaky bucket of validation time: each validation adds the
// time it took, and the debt drains at cores seconds per second. While the
// debt is at or over burst, admit refuses.
type budget struct {
	mu    sync.Mutex
	cores float64
	burst time.Duration
	debt  time.Duration
	last  time.Time
}

func newBudget(cores float64, burst time.Duration) *budget {
	return &budget{cores: cores, burst: burst}
}

// drain lowers the debt by what drained since the last call. Call it with mu
// held.
func (b *budget) drain(now time.Time) {
	if !b.last.IsZero() {
		if elapsed := now.Sub(b.last); elapsed > 0 {
			b.debt -= time.Duration(float64(elapsed) * b.cores)
			b.debt = max(b.debt, 0)
		}
	}
	// 時計が戻ったときは、lastを進めずに減らさない(次の呼び出しで取り戻す)。
	if now.After(b.last) {
		b.last = now
	}
}

// admit reports whether a validation may start now.
func (b *budget) admit(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.drain(now)
	return b.debt < b.burst
}

// spend adds the time a validation took.
func (b *budget) spend(now time.Time, took time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.drain(now)
	b.debt += max(took, 0)
}
