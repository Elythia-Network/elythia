package abuse

import (
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// UserLookups resolves the parts of UserLite that come from other tables: the
// instance of a remote user and the user's custom emojis. A nil lookup leaves
// that part out.
type UserLookups struct {
	Instances entity.InstanceLookup
	Emojis    entity.EmojiLookup
}

// WebhookPayload renders report as the body of the abuseReport and
// abuseReportResolved system webhooks. reporter, target and assignee are the
// users of the report (nil when unknown); idGen derives createdAt and may be
// nil.
//
// 本家 AbuseReportNotificationService.notifySystemWebhook は通報の行をそのまま
// 展開し、利用者 3 人を UserLite で差し込む (#3260)。行の列なので
// targetUserHost / reporterHost も入る。管理画面の API (admin/abuse-user-reports)
// とは形が違うので、あちらの packer を流用しない。
//
// createdAt は本家に無い。mk-go が前から送っていた項目で、消すと受け取る側が
// 壊れうるので、追加の項目として残す (docs/divergence.md 1-1b)。
//
// 本家の UserLite はリモートの利用者に instance を付け、emojis を絵文字の URL に
// 解決する (UserEntityService.pack)。PackUserLite だけではどちらも付かないので、
// 他の packer と同じ resolver を通す。Flag の通報者は必ずリモートなので、ここを
// 省くと Flag 経由の通報だけ本文の形が変わる。
func WebhookPayload(report *model.AbuseUserReport, reporter, target, assignee *model.User, lookups UserLookups, idGen id.Generator) map[string]any {
	users := make([]*model.User, 0, 3)
	for _, u := range []*model.User{reporter, target, assignee} {
		if u != nil {
			users = append(users, u)
		}
	}
	instances := entity.NewInstanceResolver(lookups.Instances, users...)
	// EmojiResolver は note から絵文字名を集めるので、利用者だけを持つ note を渡す
	// (PackNotifications と同じ形)。
	carriers := make([]*model.Note, 0, len(users))
	for _, u := range users {
		carriers = append(carriers, &model.Note{User: u})
	}
	emojis := entity.NewEmojiResolver(lookups.Emojis, carriers)
	packUser := func(u *model.User) any {
		if u == nil {
			return nil
		}
		lite := entity.PackUserLite(u)
		instances.FillUserLite(&lite)
		emojis.PopulateUserEmojis(u, &lite)
		return &lite
	}
	createdAt := ""
	if idGen != nil {
		if t, err := idGen.ParseTime(report.ID); err == nil {
			createdAt = t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}
	// 本家は assigneeId が無いときに assignee を null にする。
	var assigneeOut any
	if report.AssigneeID != nil {
		assigneeOut = packUser(assignee)
	}
	return map[string]any{
		"id":             report.ID,
		"createdAt":      createdAt,
		"targetUserId":   report.TargetUserID,
		"targetUser":     packUser(target),
		"targetUserHost": report.TargetUserHost,
		"reporterId":     report.ReporterID,
		"reporter":       packUser(reporter),
		"reporterHost":   report.ReporterHost,
		"assigneeId":     report.AssigneeID,
		"assignee":       assigneeOut,
		"resolved":       report.Resolved,
		"forwarded":      report.Forwarded,
		"comment":        report.Comment,
		"moderationNote": report.ModerationNote,
		"resolvedAs":     report.ResolvedAs,
	}
}
