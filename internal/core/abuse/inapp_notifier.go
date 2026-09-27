package abuse

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/shiroha-a/mk/internal/core/notification"
	"github.com/shiroha-a/mk/internal/model"
)

// ModeratorLister lists moderator/administrator users. 実装は core/role.Service。
type ModeratorLister interface {
	GetModerators() ([]*model.User, error)
}

// CoalescingNotifier keeps at most one notification of a type per recipient.
// 実装は core/notification.Service。
type CoalescingNotifier interface {
	CreateCoalesced(ctx context.Context, in notification.CreateInput, opt notification.Coalesce) (*notification.Notification, error)
}

// notificationWindow is the minimum interval between two abuseReport
// notifications for one moderator (#3200).
//
// 通知欄に残るのは常に 1 件 (新しい通報で作り直す) なので、これが絞るのは
// realtime のポップアップと、一覧の先頭へ上がってくる頻度。短すぎると連打で
// ポップアップが鳴り続け、長すぎると別件の通報が先頭に上がってこない。
const notificationWindow = 10 * time.Minute

// ReportLookup is the narrow subset of repository.AbuseReportRepository that
// InAppNotifier needs.
type ReportLookup interface {
	HasOlderUnresolvedByPair(reporterID, targetUserID, beforeID string) (bool, error)
	HasOlderUnresolvedFromHost(host, beforeID string) (bool, error)
	FindStatesByIDs(ids []string) (map[string]model.AbuseReportState, error)
}

// InAppNotifier leaves new abuse reports in the moderators' notification list
// (#2868) without letting a flood of reports bury everything else (#3200).
//
// **upstream には無い通知。** あちらは通報を email / system webhook / admin
// stream でしか流さない。ローカルの users/report-abuse と連合経由の Flag の
// 両方がここを通る (片方だけだと通報の出どころで通知の有無が変わる)。
//
// 絞っているのは通知だけで、通報そのものは呼び出し側で保存済み。管理画面の
// 一覧・admin stream・system webhook には従来どおり 1 件ずつ出る。
type InAppNotifier struct {
	mods     ModeratorLister
	notifier CoalescingNotifier
	reports  ReportLookup
}

// NewInAppNotifier builds an InAppNotifier. Any nil dependency turns
// NotifyNewReport into a no-op.
func NewInAppNotifier(mods ModeratorLister, notifier CoalescingNotifier, reports ReportLookup) *InAppNotifier {
	return &InAppNotifier{mods: mods, notifier: notifier, reports: reports}
}

// NotifyNewReport notifies every moderator of a report that has already been
// persisted. best-effort: failures are logged, never returned.
//
// 通知欄に残す通報の通知はモデレーターごとに 1 件で、新しい通報で作り直す
// (#3200)。作り直さずに既存を残すのは次のとき:
//   - 前回の作成から notificationWindow が経っていない
//   - 同じ通報者が同じ相手を、未解決のまま前にも通報している
//   - リモートからの通報で、同じホストから未解決の通報が前にも来ている
//
// **既存が無ければ必ず作る。** 残した通知には read 時に未対応の件数
// (unresolvedCount) が載るので、作り直さなかった通報もそこから見える。
// 逆に既存が無い (押し出された / 参照先が消えた) ときに見送ると、見える
// 場所がどこにも無くなる。
func (n *InAppNotifier) NotifyNewReport(ctx context.Context, report *model.AbuseUserReport) {
	if n == nil || n.mods == nil || n.notifier == nil || n.reports == nil || report == nil {
		return
	}
	repeat := n.isRepeat(report)
	// **リクエストの cancel を引き継がない。** 通報は永続化済みで、通知はそれに
	// 付随する副作用。クライアント切断で欠けるべきではない。
	ctx = context.WithoutCancel(ctx)
	mods, err := n.mods.GetModerators()
	if err != nil {
		slog.Warn("abuse report: list moderators failed", "err", err)
		return
	}
	for _, m := range mods {
		// 通報者自身がモデレーターなら notifier == notifiee になり
		// ErrSelfNotification で弾かれる。自分の通報が自分の通知欄に出ないのは
		// 正しいので、警告として出さない。ErrCoalesced も意図した抑止。
		_, nerr := n.notifier.CreateCoalesced(ctx, notification.CreateInput{
			NotifieeID: m.ID,
			NotifierID: report.ReporterID,
			Type:       notification.TypeAbuseReport,
			// **comment は入れない (#2868)。** 通報コメントは定型フォームの全文が
			// 入るので通知欄に出しても読めず、出さない以上 Redis に通報本文の
			// 複製を残す理由が無い (権限を失った元モデレーターに読まれる面も減る)。
			Extra: map[string]any{
				"reportId":     report.ID,
				"targetUserId": report.TargetUserID,
			},
		}, notification.Coalesce{Window: notificationWindow, Quiet: repeat, Alive: n.reportAlive})
		if nerr != nil && !errors.Is(nerr, notification.ErrSelfNotification) && !errors.Is(nerr, notification.ErrCoalesced) {
			slog.Warn("abuse report: in-app notification failed", "moderator", m.ID, "err", nerr)
		}
	}
}

// isRepeat reports whether an older unresolved report already covers this one.
//
// **DB エラーは「繰り返しではない」に倒す。** 繰り返し扱いにすると既存の通知を
// 作り直さないので、新しい通報が一覧の先頭に上がってこない。通報の保存は直前に
// 成功しているので、一時的な失敗で作り直しが増える量は限られる (Window もある)。
func (n *InAppNotifier) isRepeat(report *model.AbuseUserReport) bool {
	dup, err := n.reports.HasOlderUnresolvedByPair(report.ReporterID, report.TargetUserID, report.ID)
	if err != nil {
		slog.Warn("abuse report: duplicate check failed", "report", report.ID, "err", err)
	} else if dup {
		return true
	}
	// ローカルの通報は reporterHost が NULL。ホスト単位で絞るのはリモートだけ
	// (ローカルは全員が同じ「ホスト」なので、絞ると 1 件目以降が全部消える)。
	if report.ReporterHost == nil {
		return false
	}
	fromHost, err := n.reports.HasOlderUnresolvedFromHost(*report.ReporterHost, report.ID)
	if err != nil {
		slog.Warn("abuse report: host check failed", "report", report.ID, "err", err)
		return false
	}
	return fromHost
}

// reportAlive reports whether an unread notification still points at an
// existing report. 通報が消えた通知は read 時に drop されるので、それを根拠に
// 抑止すると通知欄に何も見えないまま新しい通報が隠れる。lookup が失敗したら
// 「生きていない」に倒して作る側へ寄せる (isRepeat と同じ理由)。
func (n *InAppNotifier) reportAlive(existing *notification.Notification) bool {
	reportID, _ := existing.Extra["reportId"].(string)
	if reportID == "" {
		return false
	}
	states, err := n.reports.FindStatesByIDs([]string{reportID})
	if err != nil {
		slog.Warn("abuse report: state lookup failed", "report", reportID, "err", err)
		return false
	}
	_, ok := states[reportID]
	return ok
}
