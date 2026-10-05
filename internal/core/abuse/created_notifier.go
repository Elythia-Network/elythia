package abuse

import (
	"context"
	"log/slog"

	corewebhook "github.com/elythia-network/elythia/internal/core/webhook"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// ReportInAppNotifier leaves a new report in the moderators' notification
// list. 実装は InAppNotifier。
type ReportInAppNotifier interface {
	NotifyNewReport(ctx context.Context, report *model.AbuseUserReport)
}

// AdminEventPublisher publishes an event to one user's admin stream.
// 実装は stream.AdminStreamPublisher。
type AdminEventPublisher interface {
	PublishAdminEvent(userID, eventType string, body any)
}

// SystemWebhookDispatcher dispatches a system webhook event, skipping the
// webhooks listed in excludes. 実装は core/webhook.Service。
type SystemWebhookDispatcher interface {
	DispatchSystemExcluding(eventType string, body any, excludes []string)
}

// RecipientLister lists the abuse report notification recipients.
type RecipientLister interface {
	List() ([]*model.AbuseReportNotificationRecipient, error)
}

// ProfileFinder resolves the profiles (email addresses) of the mail
// recipients.
type ProfileFinder interface {
	FindProfilesByUserIDs(ids []string) ([]*model.UserProfile, error)
}

// MetaFetcher reads the instance meta (meta.email).
type MetaFetcher interface {
	Fetch() (*model.Meta, error)
}

// MailSender sends a plain-text email. 実装は smtp.SubjectBodySenderFromMeta
// で、SMTP が未設定なら何もしない。
type MailSender func(to, subject, body string)

// CreatedNotifier tells moderators about a newly created abuse report through
// every channel: the in-app notification, the admin stream, the
// `abuseReport` system webhook (#3256) and email (#3265).
//
// **通報の入口ごとに通知を書かない。** ローカルの `users/report-abuse` と、連合
// 経由で受ける `Flag` は、どちらも保存した後にこれを 1 回呼ぶ。入口ごとに書いて
// いたので `Flag` の経路だけ Webhook と admin stream が抜けていた (#3256)。本家も
// 両方が `AbuseReportService.report` に合流し、そこから admin stream / system
// webhook / メールを出す。
//
// どの経路も best-effort で、未配線の経路は何もしない。通報そのものは保存済み
// なので、通知の失敗で通報を失敗させない。
type CreatedNotifier struct {
	inApp ReportInAppNotifier

	mods  ModeratorLister
	admin AdminEventPublisher

	webhook    SystemWebhookDispatcher
	recipients RecipientLister
	lookups    UserLookups
	idGen      id.Generator

	mail           MailSender
	mailRecipients RecipientLister
	profiles       ProfileFinder
	meta           MetaFetcher
}

// NewCreatedNotifier constructs a CreatedNotifier. Any nil dependency
// disables that channel.
func NewCreatedNotifier(inApp ReportInAppNotifier, mods ModeratorLister, admin AdminEventPublisher) *CreatedNotifier {
	return &CreatedNotifier{inApp: inApp, mods: mods, admin: admin}
}

// SetWebhook wires the system webhook channel.
//
// **コンストラクタの引数にしない。** router では通報の通知先の repository
// (無効にした通知先を除外に使う) が、通報の入口 (users の handler と
// federation の processor) を組み立てた後でしか揃わない。同じ notifier を
// 先に両方へ渡し、Webhook は後から足す。
func (n *CreatedNotifier) SetWebhook(d SystemWebhookDispatcher, recipients RecipientLister, lookups UserLookups, idGen id.Generator) {
	n.webhook = d
	n.recipients = recipients
	n.lookups = lookups
	n.idGen = idGen
}

// SetMail wires the email channel (#3265): 本家 notifyMail と同じく、有効な
// メール方式の通知先と meta.email へ送る。
func (n *CreatedNotifier) SetMail(send MailSender, recipients RecipientLister, profiles ProfileFinder, meta MetaFetcher) {
	n.mail = send
	// webhook 側 (SetWebhook) の recipients とは別に持つ。片方の配線を変えた
	// ときに、もう片方が黙って変わらないようにする。
	n.mailRecipients = recipients
	n.profiles = profiles
	n.meta = meta
}

// NotifyCreated notifies about report. reporter and target are the users of
// the report as already resolved by the caller (nil when unknown).
func (n *CreatedNotifier) NotifyCreated(ctx context.Context, report *model.AbuseUserReport, reporter, target *model.User) {
	if n == nil || report == nil {
		return
	}
	if n.inApp != nil {
		n.inApp.NotifyNewReport(ctx, report)
	}
	n.publishAdminStream(report)
	n.dispatchWebhook(report, reporter, target)
	if n.mail != nil {
		// SMTP は数秒かかりうるので、通報の API や inbox の処理を待たせない。
		go n.sendMail(report)
	}
}

// abuseReportMailSubject is upstream notifyMail の件名 (英語の固定)。
const abuseReportMailSubject = "New Abuse Report"

// sendMail emails report to every mail recipient (upstream notifyMail)。
func (n *CreatedNotifier) sendMail(report *model.AbuseUserReport) {
	for _, to := range n.mailAddresses() {
		// 本家は件名を英語の固定、本文を通報のコメントにする (HTML 版と text 版の
		// 両方に sanitize-html を通したコメント)。mk-go は text だけで送るので、
		// コメントをそのまま入れる (HTML として解釈されないので無害化は要らない)。
		n.mail(to, abuseReportMailSubject, report.Comment)
	}
}

// mailAddresses returns where to send the report mail, mirroring upstream
// notifyMail / fetchEMailRecipients:
//   - 有効 (isActive) なメール方式の通知先で、利用者がモデレーター (管理者を
//     含む) のもの。本家はモデレーターでなくなった利用者の通知先を DB から
//     消すが、ここでは送らないだけにする (通報のたびに管理設定を書き換えない)
//   - その利用者のメールアドレスが確認済みのもの
//   - meta.email (設定されていれば)
func (n *CreatedNotifier) mailAddresses() []string {
	var out []string
	if n.mailRecipients != nil && n.profiles != nil && n.mods != nil {
		out = append(out, n.recipientAddresses()...)
	}
	if n.meta != nil {
		if m, err := n.meta.Fetch(); err != nil {
			slog.Warn("abuse: fetch meta for report mail failed", "err", err)
		} else if m != nil && m.Email != nil && *m.Email != "" {
			out = append(out, *m.Email)
		}
	}
	return out
}

func (n *CreatedNotifier) recipientAddresses() []string {
	recipients, err := n.mailRecipients.List()
	if err != nil {
		slog.Warn("abuse: list report mail recipients failed", "err", err)
		return nil
	}
	var userIDs []string
	for _, r := range recipients {
		if r.Method == "email" && r.IsActive && r.UserID != nil && *r.UserID != "" {
			userIDs = append(userIDs, *r.UserID)
		}
	}
	if len(userIDs) == 0 {
		return nil
	}
	mods, err := n.mods.GetModerators()
	if err != nil {
		slog.Warn("abuse: list moderators for report mail failed", "err", err)
		return nil
	}
	isMod := make(map[string]bool, len(mods))
	for _, m := range mods {
		isMod[m.ID] = true
	}
	profiles, err := n.profiles.FindProfilesByUserIDs(userIDs)
	if err != nil {
		slog.Warn("abuse: find profiles for report mail failed", "err", err)
		return nil
	}
	byID := make(map[string]*model.UserProfile, len(profiles))
	for _, p := range profiles {
		byID[p.UserID] = p
	}
	var out []string
	for _, id := range userIDs {
		p := byID[id]
		if !isMod[id] || p == nil || !p.EmailVerified || p.Email == nil || *p.Email == "" {
			continue
		}
		out = append(out, *p.Email)
	}
	return out
}

// publishAdminStream sends newAbuseUserReport to every moderator's admin
// stream (#1549)。本家 notifyAdminStream と同じく {id, targetUserId,
// reporterId, comment} だけを送る (misskey-js の newAbuseUserReport 型も同じ 4 つ)。
func (n *CreatedNotifier) publishAdminStream(report *model.AbuseUserReport) {
	if n.mods == nil || n.admin == nil {
		return
	}
	mods, err := n.mods.GetModerators()
	if err != nil {
		slog.Warn("abuse: list moderators failed", "err", err)
		return
	}
	body := map[string]any{
		"id":           report.ID,
		"targetUserId": report.TargetUserID,
		"reporterId":   report.ReporterID,
		"comment":      report.Comment,
	}
	for _, m := range mods {
		n.admin.PublishAdminEvent(m.ID, "newAbuseUserReport", body)
	}
}

// dispatchWebhook fires the abuseReport system webhook (#1542)。本文は
// WebhookPayload (本家と同じ形、#3260)。作成時点なので担当者は居ない。
func (n *CreatedNotifier) dispatchWebhook(report *model.AbuseUserReport, reporter, target *model.User) {
	if n.webhook == nil {
		return
	}
	body := WebhookPayload(report, reporter, target, nil, n.lookups, n.idGen)
	n.webhook.DispatchSystemExcluding(corewebhook.SystemEventAbuseReport, body, n.inactiveWebhookIDs())
}

// inactiveWebhookIDs returns the systemWebhookId of inactive recipients
// (method=webhook)。本家 notifySystemWebhook の withoutWebhookIds 相当 (#1542)。
// 取れなければ nil (除外せずに送る)。
func (n *CreatedNotifier) inactiveWebhookIDs() []string {
	if n.recipients == nil {
		return nil
	}
	recipients, err := n.recipients.List()
	if err != nil {
		return nil
	}
	var excludes []string
	for _, r := range recipients {
		if r.Method == "webhook" && !r.IsActive && r.SystemWebhookID != nil {
			excludes = append(excludes, *r.SystemWebhookID)
		}
	}
	return excludes
}
