// Package moderatoractivity implements the upstream
// CheckModeratorsActivityProcessorService (#1563): it detects when every
// moderator has been inactive past a threshold and, if so, flips the instance
// to invitation-only (disableRegistration=true), warning moderators by email /
// announcement / SystemWebhook as the deadline approaches.
package moderatoractivity

import (
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/l10n"
	"github.com/elythia-network/elythia/internal/model"
)

const (
	// inactivityLimitDays は全モデレーターが非アクティブと判定する日数閾値。
	inactivityLimitDays = 7
	// warningRemainingDays は警告通知を始める「残り日数」。
	warningRemainingDays = 2
	// warningIntervalHours は警告通知の間隔 (残り時間がこの倍数のときだけ送る)。
	warningIntervalHours = 6
)

// Narrow dependency interfaces (satisfied by the wired services/repos).
type (
	// ModeratorLister returns moderators+admins+root (core/role.Service.GetModerators).
	ModeratorLister interface {
		GetModerators() ([]*model.User, error)
	}
	// MetaService reads and updates instance meta (repository.MetaRepository).
	MetaService interface {
		Fetch() (*model.Meta, error)
		Update(fields map[string]any) error
	}
	// ProfileLookup batch-resolves user profiles for email delivery
	// (repository.UserRepository.FindProfilesByUserIDs).
	ProfileLookup interface {
		FindProfilesByUserIDs(userIDs []string) ([]*model.UserProfile, error)
	}
	// AnnouncementCreator persists an announcement. Wired to
	// core/announcement.Creator, which also publishes the realtime
	// `announcementCreated` main-stream event for per-user announcements
	// (#1606、本家 AnnouncementService.create 相当)。
	AnnouncementCreator interface {
		Create(a *model.Announcement) error
	}
	// WebhookDispatcher fans an event out to subscribed system webhooks
	// (core/webhook.Service.DispatchSystem).
	WebhookDispatcher interface {
		DispatchSystem(eventType string, body any)
	}
	// IDGenerator generates announcement ids (misc/id.Generator).
	IDGenerator interface {
		Generate(t time.Time) string
	}
)

// EmailSender sends a plaintext email. nil disables email delivery.
type EmailSender func(to, subject, body string)

// Service evaluates moderator activity and applies the invitation-only switch
// plus notifications. Any dependency may be nil to disable its effect.
type Service struct {
	moderators ModeratorLister
	meta       MetaService
	profiles   ProfileLookup
	announce   AnnouncementCreator
	webhook    WebhookDispatcher
	idGen      IDGenerator
	sendEmail  EmailSender
	now        func() time.Time
}

// NewService constructs the moderator-activity service.
func NewService(
	moderators ModeratorLister,
	meta MetaService,
	profiles ProfileLookup,
	announce AnnouncementCreator,
	webhook WebhookDispatcher,
	idGen IDGenerator,
	sendEmail EmailSender,
) *Service {
	return &Service{
		moderators: moderators,
		meta:       meta,
		profiles:   profiles,
		announce:   announce,
		webhook:    webhook,
		idGen:      idGen,
		sendEmail:  sendEmail,
		now:        time.Now,
	}
}

// Check runs one evaluation cycle. Safe to call when dependencies are missing
// (returns nil). Mirrors upstream process()/processImpl().
func (s *Service) Check() error {
	if s.moderators == nil || s.meta == nil {
		return nil
	}
	meta, err := s.meta.Fetch()
	if err != nil {
		return err
	}
	// 既に招待制なら何もしない (upstream process() の早期 return)。
	//
	// **承認制 (#2557) と「受け付けない」(#3186) のときも何もしない (mk-go 独自)。**
	// この処理の目的は「審査する人がいない間に、誰でも登録できる状態を残さない」こと。
	// 承認制は承認するモデレーターがいなければ誰も入れないので、閉じる理由が無い。
	// しかも disableRegistration だけを立てると承認制の入口 (approvalOpen) まで塞がり、
	// 承認制と招待制が重なった「どこからも登録できない」状態になる (#2565 が避けている
	// 組み合わせ)。「受け付けない」は既に全部閉じている。
	if meta.DisableRegistration || meta.ApprovalRequiredForSignup || meta.RegistrationClosed {
		return nil
	}

	mods, err := s.moderators.GetModerators()
	if err != nil {
		return err
	}
	// allMods = 通知対象 (= 全 moderator)。upstream の notify* は lastActiveDate で
	// filter せず fetchModerators() で全件に通知するため、警告 / 招待制切替の通知は
	// lastActiveDate が null の moderator (= mk-go では認証 API を叩いていないだけの
	// 現役 panel admin になりがち) にも届ける必要がある。
	// withDate = 非アクティブ判定対象 (upstream 同様 lastActiveDate==null は除外)。
	allMods := make([]*model.User, 0, len(mods))
	withDate := make([]*model.User, 0, len(mods))
	for _, m := range mods {
		if m == nil {
			continue
		}
		allMods = append(allMods, m)
		if m.LastActiveDate != nil {
			withDate = append(withDate, m)
		}
	}
	// 安全側 divergence: lastActiveDate を持つ moderator が 0 人なら何もしない。
	// upstream は空集合で isModeratorsInactive=true となり登録を無効化しうるが、
	// mk-go の lastActiveDate は認証 API request 時にしか bump されないため、
	// 全 null = 活動を計測できていないだけ とみなし、fresh/quiet instance を
	// 誤って招待制にしない (#1563)。
	if len(withDate) == 0 {
		return nil
	}

	now := s.now()
	// upstream は setDate(today-7) の calendar-day 減算 (server local TZ)。mk-go は
	// 168h の絶対減算で、UTC 運用では同値。DST observing TZ の遷移日のみ ±1h ずれる
	// が、運用上無視できる軽微差 (#1563)。
	inactivePeriod := now.Add(-inactivityLimitDays * 24 * time.Hour)
	inactiveCount := 0
	var newest time.Time
	for _, m := range withDate {
		if m.LastActiveDate.Before(inactivePeriod) {
			inactiveCount++
		}
		if m.LastActiveDate.After(newest) {
			newest = *m.LastActiveDate
		}
	}
	isModeratorsInactive := inactiveCount == len(withDate)

	if isModeratorsInactive {
		slog.Warn("checkModeratorsActivity: all moderators inactive, switching to invitation-only",
			"limitDays", inactivityLimitDays)
		if err := s.meta.Update(map[string]any{"disableRegistration": true}); err != nil {
			slog.Warn("checkModeratorsActivity: update meta failed", "err", err)
		}
		s.notifyChangeToInvitationOnly(allMods)
		return nil
	}

	// newest は inactivePeriod 以上 (= 少なくとも 1 人 active) なので非負。
	remaining := newest.Sub(inactivePeriod)
	remainingDays := int(remaining / (24 * time.Hour)) // floor (非負)
	remainingHours := int(remaining / time.Hour)       // floor (非負)
	if remainingDays <= warningRemainingDays && remainingHours%warningIntervalHours == 0 {
		s.notifyInactiveModeratorsWarning(allMods, remaining, remainingDays, remainingHours)
	}
	return nil
}

type verifiedRecipient struct {
	email string
	lang  string
}

func (s *Service) instanceLangs() []string {
	if s.meta == nil {
		return nil
	}
	meta, err := s.meta.Fetch()
	if err != nil || meta == nil {
		return nil
	}
	return l10n.LangsFromMeta(meta)
}

// notifyInactiveModeratorsWarning emails moderators and dispatches the warning
// SystemWebhook (upstream notifyInactiveModeratorsWarning).
func (s *Service) notifyInactiveModeratorsWarning(mods []*model.User, remaining time.Duration, remainingDays, remainingHours int) {
	profileByUser := s.profilesByUserID(mods)
	metaLangs := s.instanceLangs()
	if s.sendEmail != nil {
		for _, m := range mods {
			recipient := s.verifiedRecipientFor(m, profileByUser, metaLangs)
			if recipient == nil {
				continue
			}
			subject, body := l10n.ModeratorInactivityWarning(recipient.lang, remainingDays, remainingHours)
			s.sendEmail(recipient.email, subject, body)
		}
	}
	if s.webhook != nil {
		s.webhook.DispatchSystem("inactiveModeratorsWarning", map[string]any{
			// 本家の ModeratorInactivityRemainingTime と同じ 3 つ。time は残りの
			// ミリ秒 (本家は Date の getTime の差) (#3261)。
			"remainingTime": map[string]any{
				"time":    remaining.Milliseconds(),
				"asDays":  remainingDays,
				"asHours": remainingHours,
			},
		})
	}
}

// notifyChangeToInvitationOnly creates a per-moderator announcement, emails
// moderators and dispatches the invitation-only SystemWebhook (upstream
// notifyChangeToInvitationOnly).
func (s *Service) notifyChangeToInvitationOnly(mods []*model.User) {
	profileByUser := s.profilesByUserID(mods)
	metaLangs := s.instanceLangs()

	if s.announce != nil && s.idGen != nil {
		for _, m := range mods {
			lang := s.langForModerator(m, profileByUser, metaLangs)
			subject, body := l10n.ModeratorInvitationOnlyChanged(lang, inactivityLimitDays)
			uid := m.ID
			a := &model.Announcement{
				ID:                     s.idGen.Generate(s.now()),
				Title:                  subject,
				Text:                   body,
				Icon:                   "info",
				Display:                "normal",
				IsActive:               true,
				ForExistingUsers:       true,
				NeedConfirmationToRead: true,
				UserID:                 &uid,
			}
			if err := s.announce.Create(a); err != nil {
				slog.Warn("checkModeratorsActivity: create announcement failed", "userId", uid, "err", err)
			}
		}
	}
	if s.sendEmail != nil {
		for _, m := range mods {
			recipient := s.verifiedRecipientFor(m, profileByUser, metaLangs)
			if recipient == nil {
				continue
			}
			subject, body := l10n.ModeratorInvitationOnlyChanged(recipient.lang, inactivityLimitDays)
			s.sendEmail(recipient.email, subject, body)
		}
	}
	if s.webhook != nil {
		s.webhook.DispatchSystem("inactiveModeratorsInvitationOnlyChanged", map[string]any{})
	}
}

func (s *Service) profilesByUserID(mods []*model.User) map[string]*model.UserProfile {
	if s.profiles == nil {
		return nil
	}
	ids := make([]string, 0, len(mods))
	for _, m := range mods {
		ids = append(ids, m.ID)
	}
	rows, err := s.profiles.FindProfilesByUserIDs(ids)
	if err != nil {
		slog.Warn("checkModeratorsActivity: load profiles failed", "err", err)
		return nil
	}
	out := make(map[string]*model.UserProfile, len(rows))
	for _, p := range rows {
		out[p.UserID] = p
	}
	return out
}

func (s *Service) langForModerator(m *model.User, profiles map[string]*model.UserProfile, metaLangs []string) string {
	if profiles != nil {
		if p := profiles[m.ID]; p != nil {
			return l10n.Resolve(p.Lang, metaLangs)
		}
	}
	return l10n.Resolve(nil, metaLangs)
}

func (s *Service) verifiedRecipientFor(m *model.User, profiles map[string]*model.UserProfile, metaLangs []string) *verifiedRecipient {
	if profiles == nil {
		return nil
	}
	p := profiles[m.ID]
	if p == nil || !p.EmailVerified || p.Email == nil || *p.Email == "" {
		return nil
	}
	return &verifiedRecipient{
		email: *p.Email,
		lang:  l10n.Resolve(p.Lang, metaLangs),
	}
}
