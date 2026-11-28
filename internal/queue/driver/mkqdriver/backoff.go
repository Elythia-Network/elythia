package mkqdriver

import (
	"math"
	"math/rand/v2"
	"time"
)

// httpRelatedBackoffBaseDelay / httpRelatedBackoffCap / httpRelatedBackoffJitter
// mirror Misskey TS QueueProcessorService.httpRelatedBackoff verbatim:
//
//	const baseDelay = 60 * 1000;            // 1min
//	const maxBackoff = 8 * 60 * 60 * 1000;  // 8hours
//	let backoff = (2 ** attemptsMade - 1) * baseDelay;
//	backoff = Math.min(backoff, maxBackoff);
//	backoff += Math.round(backoff * Math.random() * 0.2);
//
// deliver / inbox の retry を TS と drop-in 一致させるため (#1406, mkq#67)。
const (
	httpRelatedBackoffBaseDelay = time.Minute
	httpRelatedBackoffCap       = 8 * time.Hour
	httpRelatedBackoffJitter    = 0.2
)

// httpRelatedBackoff reproduces Misskey TS's httpRelatedBackoff. attemptsMade
// is the post-bump attempt count (BullMQ settings.backoffStrategy semantics).
// rng はテストで決定的に差し替えられるよう引数で受け取る (nil = math/rand/v2)。
func httpRelatedBackoff(attemptsMade int, rng func() float64) time.Duration {
	if rng == nil {
		rng = rand.Float64
	}
	// (2^attemptsMade - 1) * baseDelay。attemptsMade が大きいと 2^n が
	// float64 でも巨大になるが、直後に cap で 8h に丸めるので overflow は
	// 問題にならない (cap 比較は time.Duration の範囲内)。
	mult := math.Pow(2, float64(attemptsMade)) - 1
	backoff := time.Duration(mult * float64(httpRelatedBackoffBaseDelay))
	if backoff > httpRelatedBackoffCap || backoff < 0 {
		backoff = httpRelatedBackoffCap
	}
	// +0..20% jitter。TS は Math.round するが、ns 単位の丸め差は無視できる
	// ため float のまま加算する。
	backoff += time.Duration(float64(backoff) * rng() * httpRelatedBackoffJitter)
	return backoff
}

// federationBackoffStrategy is the retry delay the workers register for jobs
// enqueued with the federation backoff (driver.WithFederationBackoff).
// FederationMaxRetryDelay bounds it with the same formula.
func federationBackoffStrategy(attemptsMade int) time.Duration {
	return httpRelatedBackoff(attemptsMade, nil)
}

// FederationMaxRetryDelay returns the longest single retry delay of a job
// enqueued with the federation backoff and maxAttempts total tries: the delay
// before its last try, with the jitter at its upper bound. maxAttempts of 1 or
// less has no retry and returns 0.
//
// 待ちは回数とともに伸びて上限で止まるので、最後の試行の前の待ちが最大になる。
// worker と同じ httpRelatedBackoff を jitter の上限で呼ぶ。上限への丸めは
// httpRelatedBackoff の中で jitter を足す前に行われるので、起点や上限を変えても
// この値は worker の待ちの最大と一致する。
func FederationMaxRetryDelay(maxAttempts int) time.Duration {
	if maxAttempts <= 1 {
		return 0
	}
	return httpRelatedBackoff(maxAttempts-1, func() float64 { return 1 })
}
