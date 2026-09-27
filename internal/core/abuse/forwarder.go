// Package abuse implements the side effects of abuse reports: Forwarder
// handles the "notify the remote origin of a reported user" flow triggered by
// admin/forward-abuse-user-report, and InAppNotifier leaves new reports in the
// moderators' notification list.
package abuse

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/model"
)

// ReportStore is the narrow subset of repository.AbuseReportRepository that
// Forwarder needs.
type ReportStore interface {
	FindByID(id string) (*model.AbuseUserReport, error)
	UpdateFields(id string, fields map[string]any) error
}

// SystemActorFetcher returns the instance's system actor used to sign the
// Flag activity. Matches core/systemaccount.Service.Fetch.
type SystemActorFetcher interface {
	Fetch(kind string) (*model.User, error)
}

// FlagRenderer builds the Flag activity payload.
type FlagRenderer interface {
	RenderFlag(actor *model.User, targetURI, content string) *activitypub.Flag
}

// Deliverer enqueues a signed activity to the given inboxes.
type Deliverer interface {
	DeliverActivity(signerUserID string, body []byte, inboxes []string) error
}

// Forwarder implements admin.AbuseForwarder using the activitypub renderer
// and federation delivery pipeline.
type Forwarder struct {
	reports   ReportStore
	systemSvc SystemActorFetcher
	renderer  FlagRenderer
	deliverer Deliverer
}

// NewForwarder wires a Forwarder. kind はシステムアクタの種別で、本家互換
// のため "instance" を想定する。テストで差し替えられるよう interface を
// すべて受け取る。
func NewForwarder(reports ReportStore, systemSvc SystemActorFetcher, renderer FlagRenderer, deliverer Deliverer) *Forwarder {
	return &Forwarder{
		reports:   reports,
		systemSvc: systemSvc,
		renderer:  renderer,
		deliverer: deliverer,
	}
}

// SystemActorKind is the system account kind used as the actor of the Flag
// activity. Exposed so tests and the router can agree on the identifier
// without hard-coding the string.
const SystemActorKind = "instance"

// ForwardReport renders a Flag activity for the report and delivers it to
// the target user's origin inbox. ローカル通報 (target.Host nil / Inbox nil)
// は配送スキップで forwarded フラグだけ立てる。
func (f *Forwarder) ForwardReport(reportID string) error {
	report, err := f.reports.FindByID(reportID)
	if err != nil {
		return fmt.Errorf("find abuse report: %w", err)
	}
	// DB フラグは常に立てておく (本家も "forward された扱い" を無条件に true
	// にするため)。
	if err := f.reports.UpdateFields(reportID, map[string]any{"forwarded": true}); err != nil {
		return fmt.Errorf("mark report forwarded: %w", err)
	}
	target := report.TargetUser
	if target == nil || target.IsLocal() {
		return nil
	}
	if target.URI == nil || *target.URI == "" {
		return nil
	}
	inbox := preferredInbox(target)
	if inbox == "" {
		return nil
	}

	actor, err := f.systemSvc.Fetch(SystemActorKind)
	if err != nil {
		return fmt.Errorf("fetch system actor: %w", err)
	}
	if actor == nil {
		return errors.New("abuse forwarder: system actor is nil")
	}

	flag := f.renderer.RenderFlag(actor, *target.URI, report.Comment)
	body, err := json.Marshal(flag)
	if err != nil {
		return fmt.Errorf("marshal flag: %w", err)
	}
	return f.deliverer.DeliverActivity(actor.ID, body, []string{inbox})
}

// preferredInbox prefers the individual inbox over sharedInbox, matching the
// unexported helper in core/federation/deliver_service.go (理由はそちらの
// GoDoc)。小さい 5 行の重複なので、deliver_service.go 側を export するより
// inline する方が patch surface が狭い。**片方だけ直さないこと。**
func preferredInbox(u *model.User) string {
	if u.Inbox != nil && *u.Inbox != "" {
		return *u.Inbox
	}
	if u.SharedInbox != nil && *u.SharedInbox != "" {
		return *u.SharedInbox
	}
	return ""
}
