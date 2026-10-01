package abuse

import (
	"context"
	"log/slog"

	corewebhook "github.com/shiroha-a/mk/internal/core/webhook"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
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

// ProfileFinder resolves user profiles for packing users into the webhook.
type ProfileFinder interface {
	FindProfilesByUserIDs(ids []string) ([]*model.UserProfile, error)
}

// CreatedNotifier tells moderators about a newly created abuse report through
// every channel: the in-app notification, the admin stream and the
// `abuseReport` system webhook (#3256).
//
// **通報の入口ごとに通知を書かない。** ローカルの `users/report-abuse` と、連合
// 経由で受ける `Flag` は、どちらも保存した後にこれを 1 回呼ぶ。入口ごとに書いて
// いたので `Flag` の経路だけ Webhook と admin stream が抜けていた (#3256)。本家も
// 両方が `AbuseReportService.report` に合流し、そこから admin stream / system
// webhook / メールを出す (メールは mk-go に無い)。
//
// どの経路も best-effort で、未配線の経路は何もしない。通報そのものは保存済み
// なので、通知の失敗で通報を失敗させない。
type CreatedNotifier struct {
	inApp ReportInAppNotifier

	mods  ModeratorLister
	admin AdminEventPublisher

	webhook    SystemWebhookDispatcher
	recipients RecipientLister
	profiles   ProfileFinder
	idGen      id.Generator
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
func (n *CreatedNotifier) SetWebhook(d SystemWebhookDispatcher, recipients RecipientLister, profiles ProfileFinder, idGen id.Generator) {
	n.webhook = d
	n.recipients = recipients
	n.profiles = profiles
	n.idGen = idGen
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
// admin/abuse-user-reports の packedAbuseReport と同じ形。
func (n *CreatedNotifier) dispatchWebhook(report *model.AbuseUserReport, reporter, target *model.User) {
	if n.webhook == nil {
		return
	}
	var profByID map[string]*model.UserProfile
	if n.profiles != nil {
		ids := make([]string, 0, 2)
		for _, u := range []*model.User{reporter, target} {
			if u != nil {
				ids = append(ids, u.ID)
			}
		}
		if profiles, err := n.profiles.FindProfilesByUserIDs(ids); err == nil {
			profByID = make(map[string]*model.UserProfile, len(profiles))
			for _, p := range profiles {
				profByID[p.UserID] = p
			}
		}
	}
	packUser := func(u *model.User) any {
		if u == nil {
			return nil
		}
		var d entity.UserDetailed
		if n.idGen != nil {
			d = entity.PackUserDetailed(u, profByID[u.ID], n.idGen)
		} else {
			d = entity.PackUserDetailed(u, profByID[u.ID])
		}
		return &d
	}
	createdAt := ""
	if n.idGen != nil {
		if t, err := n.idGen.ParseTime(report.ID); err == nil {
			createdAt = t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}
	// assignee / resolvedAs は作成時点では常に空。forwarded も作成時点では false。
	body := map[string]any{
		"id":             report.ID,
		"createdAt":      createdAt,
		"comment":        report.Comment,
		"resolved":       false,
		"reporterId":     report.ReporterID,
		"targetUserId":   report.TargetUserID,
		"assigneeId":     nil,
		"reporter":       packUser(reporter),
		"targetUser":     packUser(target),
		"assignee":       nil,
		"forwarded":      false,
		"resolvedAs":     nil,
		"moderationNote": "",
	}
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
