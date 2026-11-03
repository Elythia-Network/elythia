package queue

import (
	"context"
	"errors"
	"time"

	"github.com/elythia-network/elythia/internal/queue/driver"
)

// ArchiveAnnouncementPayload is the payload of the delayed
// maintenance:archiveAnnouncements job scheduled for one announcement
// (#3994). The processor does not trust it: it archives whatever is due when
// it runs. The fields make the dedup key of each (announcement, due time)
// distinct and show in the admin job list what the job was for.
type ArchiveAnnouncementPayload struct {
	AnnouncementID string `json:"announcementId,omitempty"`
	// AutoArchiveAt is the due time in Unix milliseconds (本家の
	// ArchiveAnnouncementJobData と同じ単位)。
	AutoArchiveAt int64 `json:"autoArchiveAt,omitempty"`
	// AtStartup marks the job EnqueueDueJobsAtStartup puts: besides archiving
	// what is due, it schedules again the jobs of the announcements not yet
	// due.
	AtStartup bool `json:"atStartup,omitempty"`
}

// ResendQuoteRequestPayload is the payload of the delayed
// maintenance:resendQuoteRequests job scheduled for one resend slot of a
// pending QuoteRequest (#3994). Like ArchiveAnnouncementPayload, the processor
// re-reads the rows instead of trusting it.
type ResendQuoteRequestPayload struct {
	NoteID string `json:"noteId,omitempty"`
	// At is the due time of the slot in Unix milliseconds.
	At int64 `json:"at,omitempty"`
	// AtStartup marks the job EnqueueDueJobsAtStartup puts: besides the
	// resends that are due, it schedules again the jobs of those not yet due.
	AtStartup bool `json:"atStartup,omitempty"`
	// Requeued marks a job put again by RequeueQuoteRequestResend because the
	// job for the slot ran before its due time.
	Requeued bool `json:"requeued,omitempty"`
}

// dueJobMargin is added to the delay of a job that must run at or after a due
// time.
//
// 期限より前に走らせないために要るのは、遅延をミリ秒に切り上げることだけ。mkq は
// 積んだ側の time.Now() に遅延を足して走る時刻 (= 期限) を決め、それを過ぎたかは
// worker 側の時計で見て、処理も同じ worker の時計で期限と比べるので、ホスト間の
// 時計のずれは効かない (#3994 の敵対的レビュー)。1 秒の余裕は、切り上げの計算を
// 誤ったときの保険として置く。遅れてもどちらの処理でも結果は変わらない (お知らせは
// 読むときにも期限を見ている。送り直しは 3 分の遅れまで許す)。
const dueJobMargin = time.Second

// maxDueJobDelay caps the delay of a due job. お知らせの期限は JS の Date の上限
// (約 27 万年先) まで受け付けるので、遅延の計算があふれない値で抑える。抑えた
// ジョブが早く走っても、処理は保存されている期限と比べて何もしない。
const maxDueJobDelay = 100 * 365 * 24 * time.Hour

// dueJobDelay returns how long to wait before a job for at may run, measured
// from now: at - now rounded up to a millisecond, plus dueJobMargin. A due
// time already passed gives 0 (run now).
func dueJobDelay(at, now time.Time) time.Duration {
	d := at.Sub(now)
	if d <= 0 {
		return 0
	}
	// 切り上げの前に上限で抑える。time.Time.Sub は約 292 年で飽和し、その値を
	// 切り上げると符号があふれて負 (= すぐ走る) になる (#3994 の敵対的レビュー)。
	d = min(d, maxDueJobDelay)
	if r := d % time.Millisecond; r != 0 {
		d += time.Millisecond - r
	}
	return d + dueJobMargin
}

// enqueueDueJob puts taskType on the maintenance queue to run once at (or
// right after) at.
//
// **同じ payload の重複は、走る時刻まで積まない。** 起動時の積み直しや、複数の
// プロセスが同じ予定を積むときに、走る前のジョブが既にあれば足さない。dedup の
// 期限を遅延と同じにするのは、mkq が期限付きの dedup の印を完了しても消さないため
// (期限を長くすると、走り終えた後に同じ予定を積み直せなくなる)。すぐ走るジョブ
// (期限を過ぎていたもの) は dedup しない。重なっても、処理は行の今の値を確かめて
// 一度しか効かない。
func (c *Client) enqueueDueJob(taskType string, payload any, at time.Time, opts ...driver.EnqueueOption) error {
	base := []driver.EnqueueOption{
		driver.WithQueue(MaintenanceQueueName),
		driver.WithKeepCompletedAge(ScheduledJobRetention),
		driver.WithKeepFailedAge(ScheduledJobRetention),
	}
	if d := dueJobDelay(at, time.Now()); d > 0 {
		base = append(base, driver.WithProcessIn(d), driver.WithUnique(d))
	}
	base = append(base, opts...)
	return c.inner.Enqueue(context.Background(), taskType, mustMarshal(payload), base...)
}

// archiveAnnouncementOpts, resendQuoteRequestOpts and startupJobOpts are the
// retry options of the due jobs.
//
// お知らせは、失敗した回を 1 分・2 分・4 分後に試し直す (DB の一時的な失敗で、次の
// 起動か日次の保険まで落ちないままになるのを避ける)。送り直しは 30 秒ごとに 5 回、
// 予定から 3 分の許容いっぱいまで (下の ResendSlotRetries。それより長い障害は 2 分
// ごとの保険の cron が拾う)。
//
// 起動時のジョブは 1・2・4・8 分後に試し直す (#3994 の敵対的レビュー)。起動の直後は
// DB の準備が済んでいないことがあり、まだ来ていない予定の積み直しはこのジョブにしか
// できない。
func archiveAnnouncementOpts() []driver.EnqueueOption {
	return []driver.EnqueueOption{driver.WithMaxRetry(3), driver.WithBackoff(driver.BackoffExponential, time.Minute)}
}

func resendQuoteRequestOpts() []driver.EnqueueOption {
	return []driver.EnqueueOption{driver.WithMaxRetry(ResendSlotRetries), driver.WithBackoff(driver.BackoffFixed, ResendSlotRetryInterval)}
}

// ResendSlotRetries and ResendSlotRetryInterval are the retries of the job of
// a resend slot: the last try runs ResendSlotLastTry after the slot.
//
// 送り直しは予定から 3 分 (core/federation の QuoteRequestResendTolerance) を超えて
// 遅れると送らずに枠を使い切るので、試し直しはその許容いっぱいまで続ける (#3994 の
// 敵対的レビュー)。最後の試行が許容の内側にあることは internal/server のテストが
// 確かめる。ResendSlotLastTry は試行がすぐに失敗したときの値で、mkq は前の試行が
// 失敗した時刻から間隔を数えるので、DB が応答しない形 (接続のタイムアウトまで待つ) の
// 障害では試行が後ろへずれ、最後の回は許容を過ぎて空振りしうる。
const (
	ResendSlotRetries       = 5
	ResendSlotRetryInterval = 30 * time.Second
)

// ResendSlotLastTry returns how long after its slot the last try of a slot job
// runs (the margin plus every retry interval).
func ResendSlotLastTry() time.Duration {
	return dueJobMargin + ResendSlotRetries*ResendSlotRetryInterval
}

func startupJobOpts() []driver.EnqueueOption {
	return []driver.EnqueueOption{driver.WithMaxRetry(4), driver.WithBackoff(driver.BackoffExponential, time.Minute)}
}

// EnqueueDueJobsAtStartup puts the startup run of the archive of the
// announcements and of the resend of the QuoteRequests (#3994): each handles
// what came due while no job ran (the process was down, or a job was lost or
// never scheduled) and schedules again the jobs not yet due, like upstream
// AnnouncementService.onModuleInit.
//
// プロセスの中で直接流さず、queue に積んで worker に処理させる。世代を止めるときに
// queue の worker と一緒に止まり、DB のプールを閉じた後に書きに行かない。複数の
// プロセスが同時に積んでも、処理は行の更新と取り分けで一度しか効かない。
func (c *Client) EnqueueDueJobsAtStartup() error {
	now := time.Now()
	return errors.Join(
		c.enqueueDueJob(TaskTypeArchiveAnnouncements, ArchiveAnnouncementPayload{AtStartup: true}, now, startupJobOpts()...),
		c.enqueueDueJob(TaskTypeResendQuoteRequests, ResendQuoteRequestPayload{AtStartup: true}, now, startupJobOpts()...),
	)
}

// ScheduleAnnouncementArchive schedules the archive of the announcement id at
// its autoArchiveAt (#3994), like upstream QueueService
// .scheduleAnnouncementArchive.
//
// 古い予定のジョブは取り消さない (本家は取り消す)。処理は走った時点で保存されて
// いる期限と比べるので、期限を変えた・消した・お知らせを消した後に古いジョブが
// 走っても、その行には何もしない。
func (c *Client) ScheduleAnnouncementArchive(id string, at time.Time) error {
	return c.enqueueDueJob(TaskTypeArchiveAnnouncements,
		ArchiveAnnouncementPayload{AnnouncementID: id, AutoArchiveAt: at.UnixMilli()}, at,
		archiveAnnouncementOpts()...)
}

// ScheduleQuoteRequestResend schedules the resend slot at of the pending
// QuoteRequest of noteID (#3994).
func (c *Client) ScheduleQuoteRequestResend(noteID string, at time.Time) error {
	return c.enqueueDueJob(TaskTypeResendQuoteRequests,
		ResendQuoteRequestPayload{NoteID: noteID, At: at.UnixMilli()}, at,
		resendQuoteRequestOpts()...)
}

// RequeueQuoteRequestResend puts the job for the resend slot at of noteID
// again, for a job that ran before its due time (#3994 の敵対的レビュー)。
//
// 管理画面の promote は遅延ジョブを前倒しで走らせる。期限の前なので何もせずに
// 終わると、その枠は保険の cron まで処理されず、その頃には 3 分の許容を過ぎて
// 送らずに使い切られうる。同じ予定は、走る時刻まで残る dedup の印に弾かれて
// 積み直せないので、payload を変えて (requeued) dedup せずに積む。重なっても、
// 処理は行の取り分けで一度しか効かない。
func (c *Client) RequeueQuoteRequestResend(noteID string, at time.Time) error {
	base := []driver.EnqueueOption{
		driver.WithQueue(MaintenanceQueueName),
		driver.WithKeepCompletedAge(ScheduledJobRetention),
		driver.WithKeepFailedAge(ScheduledJobRetention),
	}
	if d := dueJobDelay(at, time.Now()); d > 0 {
		base = append(base, driver.WithProcessIn(d))
	}
	base = append(base, resendQuoteRequestOpts()...)
	return c.inner.Enqueue(context.Background(), TaskTypeResendQuoteRequests,
		mustMarshal(ResendQuoteRequestPayload{NoteID: noteID, At: at.UnixMilli(), Requeued: true}), base...)
}
