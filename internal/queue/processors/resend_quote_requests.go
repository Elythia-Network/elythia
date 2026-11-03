package processors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// QuoteRequestResender sends again the QuoteRequests that are still pending
// and due (#3238). core/federation.QuoteOutbox satisfies it.
type QuoteRequestResender interface {
	ResendPending(now time.Time) (int, error)
	ScheduleUpcomingResends(now time.Time) (int, error)
}

// ResendRequeuer puts the job of a resend slot again (#3994). queue.Client
// satisfies it.
type ResendRequeuer interface {
	RequeueQuoteRequestResend(noteID string, at time.Time) error
}

// maxEarlyRequeueAhead bounds how far ahead a slot of an early job may be to be
// put again: the longest gap between slots (10 分後の枠は最初の送信から 10 分) plus
// slack.
const maxEarlyRequeueAhead = 15 * time.Minute

// ResendQuoteRequestsProcessor runs the resend of pending QuoteRequests
// (FEP-044f、#3238)。送り直しの予定の時刻に積んだ遅延ジョブと、起動時と、2 分ごとの
// 保険の cron で流す (#3994。以前は毎分の cron)。
type ResendQuoteRequestsProcessor struct {
	resender QuoteRequestResender
	requeuer ResendRequeuer
	now      func() time.Time
}

// SetRequeuer sets where a slot job that ran before its due time is put again
// (#3994). nil disables it.
func (p *ResendQuoteRequestsProcessor) SetRequeuer(r ResendRequeuer) {
	p.requeuer = r
}

// NewResendQuoteRequestsProcessor constructs a processor.
func NewResendQuoteRequestsProcessor(r QuoteRequestResender) *ResendQuoteRequestsProcessor {
	return &ResendQuoteRequestsProcessor{resender: r, now: time.Now}
}

// Handle implements the driver handler contract. The job the startup puts
// (payload atStartup) runs RunAtStartup.
//
// 期限の来た行を探せなかったときは error で返し、queue に試し直させる (#3994。
// 毎分の cron だった頃は次の回が拾った)。送る 1 件ごとの失敗は ResendPending の中で
// 扱い、ここには返ってこない。
func (p *ResendQuoteRequestsProcessor) Handle(ctx context.Context, t driver.Task) error {
	if atStartup(t) {
		return p.RunAtStartup(ctx)
	}
	p.requeueIfEarly(t)
	return p.resendDue()
}

// requeueIfEarly puts the job of the slot again at its due time when t ran
// before it (管理画面の promote で前倒しされたとき、#3994 の敵対的レビュー)。
// 期限の前に走ったジョブは、その枠に何もできない。そのまま終えると、枠は 2 分ごとの
// 保険の cron まで処理されず、3 分の許容を過ぎて送らずに使い切られうる。期限の
// 来ている別の行は、続く resendDue がそのまま処理する。
func (p *ResendQuoteRequestsProcessor) requeueIfEarly(t driver.Task) {
	if p.requeuer == nil || t == nil || len(t.Payload()) == 0 {
		return
	}
	var slot struct {
		NoteID string `json:"noteId"`
		At     int64  `json:"at"`
	}
	if json.Unmarshal(t.Payload(), &slot) != nil || slot.NoteID == "" || slot.At == 0 {
		return
	}
	at := time.UnixMilli(slot.At)
	now := p.now()
	if !now.Before(at) {
		return
	}
	// 枠は最初の送信から 10 分の間にしか無いので、それより先を指す payload は壊れて
	// いる。積み直すと、遅延の計算次第ですぐ走って回り続けうるので、保険の cron に
	// 任せる (#3994 の敵対的レビュー)。
	if at.After(now.Add(maxEarlyRequeueAhead)) {
		return
	}
	if err := p.requeuer.RequeueQuoteRequestResend(slot.NoteID, at); err != nil {
		slog.Warn("resendQuoteRequests: requeue an early job", "noteId", slot.NoteID, "err", err)
	}
}

func (p *ResendQuoteRequestsProcessor) resendDue() error {
	n, err := p.resender.ResendPending(p.now())
	if err != nil {
		return fmt.Errorf("resendQuoteRequests: %w", err)
	}
	if n > 0 {
		slog.Info("resendQuoteRequests: sent", "count", n)
	}
	return nil
}

// RunAtStartup sends the resends that came due while no job ran, and
// schedules again the jobs of the resends not yet due (#3994).
//
// 止まっていた間の分は、予定から 3 分以内なら送り、それより遅れたものは送らずに
// 枠を使い切る (ResendPending の許容)。複数のプロセスが同時に流しても、行の
// 取り分けで一度しか送らない。
//
// 期限の来た分が失敗しても、まだ来ていない予定の積み直しは流す (#3994 の敵対的
// レビュー)。毎回取り分けに失敗する行が 1 つあるだけで積み直しが止まらないように、
// 両方を流してから両方の失敗を返す。
func (p *ResendQuoteRequestsProcessor) RunAtStartup(_ context.Context) error {
	dueErr := p.resendDue()
	var upErr error
	if _, err := p.resender.ScheduleUpcomingResends(p.now()); err != nil {
		upErr = fmt.Errorf("resendQuoteRequests: %w", err)
	}
	return errors.Join(dueErr, upErr)
}
