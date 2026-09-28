package driver

import (
	"errors"
	"fmt"
	"time"
)

// ErrSkipRetry is a sentinel returned by a HandlerFunc to tell the driver
// the job must not be retried even if attempts remain. Drivers map
// this to their native skip-retry semantics (mkq.ErrUnrecoverable).
//
// Handlers typically wrap it with fmt.Errorf("%w: %w", err, driver.ErrSkipRetry)
// so callers can both inspect the underlying cause and observe the skip
// signal via errors.Is.
var ErrSkipRetry = errors.New("queue driver: skip retry")

// DelayError is returned (or wrapped) by a HandlerFunc to put the job
// back for Delay without consuming an attempt. Drivers map it to their
// native delay (mkq.Delay, i.e. BullMQ's DelayedError).
//
// 「今は送るべきでない」ときに使う (配送先が落ちていると分かっている、しばらく
// 待つよう言われた、など)。普通のエラーで返すと retry の枠を待つだけで使い切り、
// 相手が戻る前にジョブが捨てられる (#3048)。
type DelayError struct {
	Delay time.Duration
}

func (e *DelayError) Error() string {
	return fmt.Sprintf("queue driver: delayed for %s", e.Delay)
}

// Delay returns a *DelayError for d. See DelayError.
func Delay(d time.Duration) error {
	return &DelayError{Delay: d}
}

// ErrResizeNotSupported is returned by Driver.Resize when the driver has
// no worker pool object to resize at all.
//
// **「Start 前」ではなく「Server() 前」。** mkq driver がこれを返すのは
// `Driver.Server()` を一度も呼んでいないときだけで (mkqdriver/driver.go の
// `d.dServer == nil`)、`Server()` 済みで `Start()` 前なら pool map が空なので
// 返るのは `mkqdriver: Resize: unknown queue %q` のほう。production の配線は
// `newServer` が構築時に `queue.NewServer(driver)` = `Server()` を呼ぶので、
// **この sentinel には到達しない** (#2985 の敵対的レビューで実測)。
//
// したがってこれを「auto-scale が使えない driver」の判定には使わないこと。
// 以前 startAutoScale がそうしていたが、上記の理由で一度も発火しなかった。
var ErrResizeNotSupported = errors.New("queue driver: dynamic Resize not supported by this backend")
