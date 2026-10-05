// Package charthook adapts the chart wrappers in
// internal/core/chart/charts to the per-service ChartHook interfaces
// declared by NoteCreateService, FollowingService, ReactionService,
// DriveService and the federation resolver / deliver service / inbox
// handler. One Hooks instance is constructed in router.go and shared
// across every service so a single chart event source can fan out to
// any number of charts without each service needing to know about the
// chart graph.
//
// All adapter methods are best-effort: chart errors are intentionally
// swallowed because the upstream services treat hook failures as
// non-fatal (matching Misskey TS).
//
// The hooks also drive the instance notesCount / usersCount counters through
// InstanceCounter, because upstream updates those counters at the same call
// sites and under the same meta.enableStatsForFederatedInstances branch as the
// instance chart.
package charthook

import (
	"time"

	"github.com/elythia-network/elythia/internal/core/chart"
	"github.com/elythia-network/elythia/internal/core/chart/charts"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// Hooks bundles every chart wrapper plus the id generator needed to
// resolve user creation timestamps for ActiveUsersChart.
type Hooks struct {
	Notes            *charts.NotesChart
	Users            *charts.UsersChart
	Drive            *charts.DriveChart
	Federation       *charts.FederationChart
	Instance         *charts.InstanceChart
	ApRequest        *charts.ApRequestChart
	ActiveUsers      *charts.ActiveUsersChart
	PerUserNotes     *charts.PerUserNotesChart
	PerUserDrive     *charts.PerUserDriveChart
	PerUserFollowing *charts.PerUserFollowingChart
	PerUserPv        *charts.PerUserPvChart
	PerUserReaction  *charts.PerUserReactionChart

	// IDGen is used by the activeUsers chart to compute the user's
	// signup time from its id (matching `IdService.parse(user.id).date`
	// upstream). Set to nil to disable activeUsers tracking.
	IDGen id.Generator

	// Meta flags — false のとき対応するチャート生成をスキップする。
	// デフォルトは true (Misskey のデフォルトと一致)。
	ChartsForRemoteUser    bool
	ChartsForFederatedInst bool
	// StatsForFederatedInst mirrors meta.enableStatsForFederatedInstances.
	// Upstream nests the note / user / follow-related instance chart updates
	// inside this flag, so those instance charts need both flags.
	StatsForFederatedInst bool

	// InstanceCounter receives the instance notesCount / usersCount
	// increments. nil disables them.
	InstanceCounter InstanceCounter

	// metaSource, when set, is read on every event so meta flag changes take
	// effect without a restart. The three fields above are the fallback.
	metaSource func() (*model.Meta, error)
}

// InstanceCounter accumulates increments of an instance counter column
// (notesCount / usersCount). Implemented by instance.CounterBuffer.
type InstanceCounter interface {
	Add(host, column string, delta int)
}

// Instance counter column names passed to InstanceCounter.Add.
const (
	instanceNotesCount = "notesCount"
	instanceUsersCount = "usersCount"
)

// SetMetaSource makes the hooks read the meta flags from fetch on every
// event instead of the snapshot fields. A failed fetch falls back to the
// fields.
func (h *Hooks) SetMetaSource(fetch func() (*model.Meta, error)) {
	h.metaSource = fetch
}

// flagSet is the effective meta flags for one event.
type flagSet struct {
	remoteUser bool
	fedCharts  bool
	fedStats   bool
}

// flags returns the meta flags to apply to the current event.
//
// 本家は meta をその場で読むので、admin で切り替えると次のイベントから効く。
// 起動時に一度だけ写した値で判定すると、切り替えが再起動まで反映されない。
// metaSource は cachedMeta の Fetch で、イベントごとの DB 往復は無い。
func (h *Hooks) flags() flagSet {
	f := flagSet{
		remoteUser: h.ChartsForRemoteUser,
		fedCharts:  h.ChartsForFederatedInst,
		fedStats:   h.StatsForFederatedInst,
	}
	if h.metaSource == nil {
		return f
	}
	m, err := h.metaSource()
	if err != nil || m == nil {
		return f
	}
	return flagSet{
		remoteUser: m.EnableChartsForRemoteUser,
		fedCharts:  m.EnableChartsForFederatedInstances,
		fedStats:   m.EnableStatsForFederatedInstances,
	}
}

// addInstanceCount forwards one counter increment when a counter is wired.
func (h *Hooks) addInstanceCount(host, column string, delta int) {
	if h.InstanceCounter != nil {
		h.InstanceCounter.Add(host, column, delta)
	}
}

// Config bundles every chart pointer alongside the id generator. The
// constructor wraps each pointer in its typed wrapper exactly once so
// callers do not have to repeat the boilerplate at the wiring site.
type Config struct {
	Notes            *chart.Chart
	Users            *chart.Chart
	Drive            *chart.Chart
	Federation       *chart.Chart
	Instance         *chart.Chart
	ApRequest        *chart.Chart
	ActiveUsers      *chart.Chart
	PerUserNotes     *chart.Chart
	PerUserDrive     *chart.Chart
	PerUserFollowing *chart.Chart
	PerUserPv        *chart.Chart
	PerUserReaction  *chart.Chart
	IDGen            id.Generator
	Clock            chart.Clock
}

// New constructs a Hooks adapter from the engine pointers. Any nil
// pointer becomes a no-op for the corresponding event.
func New(cfg Config) *Hooks {
	mk := func(c *chart.Chart, build func(*chart.Chart) any) any {
		if c == nil {
			return nil
		}
		return build(c)
	}
	h := &Hooks{
		IDGen:                  cfg.IDGen,
		ChartsForRemoteUser:    true,
		ChartsForFederatedInst: true,
		StatsForFederatedInst:  true,
	}
	if v := mk(cfg.Notes, func(c *chart.Chart) any { return charts.NewNotesChart(c) }); v != nil {
		h.Notes = v.(*charts.NotesChart)
	}
	if v := mk(cfg.Users, func(c *chart.Chart) any { return charts.NewUsersChart(c) }); v != nil {
		h.Users = v.(*charts.UsersChart)
	}
	if v := mk(cfg.Drive, func(c *chart.Chart) any { return charts.NewDriveChart(c) }); v != nil {
		h.Drive = v.(*charts.DriveChart)
	}
	if v := mk(cfg.Federation, func(c *chart.Chart) any { return charts.NewFederationChart(c) }); v != nil {
		h.Federation = v.(*charts.FederationChart)
	}
	if v := mk(cfg.Instance, func(c *chart.Chart) any { return charts.NewInstanceChart(c) }); v != nil {
		h.Instance = v.(*charts.InstanceChart)
	}
	if v := mk(cfg.ApRequest, func(c *chart.Chart) any { return charts.NewApRequestChart(c) }); v != nil {
		h.ApRequest = v.(*charts.ApRequestChart)
	}
	if cfg.ActiveUsers != nil {
		h.ActiveUsers = charts.NewActiveUsersChart(cfg.ActiveUsers, cfg.Clock)
	}
	if v := mk(cfg.PerUserNotes, func(c *chart.Chart) any { return charts.NewPerUserNotesChart(c) }); v != nil {
		h.PerUserNotes = v.(*charts.PerUserNotesChart)
	}
	if v := mk(cfg.PerUserDrive, func(c *chart.Chart) any { return charts.NewPerUserDriveChart(c) }); v != nil {
		h.PerUserDrive = v.(*charts.PerUserDriveChart)
	}
	if v := mk(cfg.PerUserFollowing, func(c *chart.Chart) any { return charts.NewPerUserFollowingChart(c) }); v != nil {
		h.PerUserFollowing = v.(*charts.PerUserFollowingChart)
	}
	if v := mk(cfg.PerUserPv, func(c *chart.Chart) any { return charts.NewPerUserPvChart(c) }); v != nil {
		h.PerUserPv = v.(*charts.PerUserPvChart)
	}
	if v := mk(cfg.PerUserReaction, func(c *chart.Chart) any { return charts.NewPerUserReactionChart(c) }); v != nil {
		h.PerUserReaction = v.(*charts.PerUserReactionChart)
	}
	return h
}

// --- note hook ---------------------------------------------------------------

// OnNoteCreated fires NotesChart, PerUserNotesChart, ActiveUsersChart
// (Write side) and InstanceChart (when remote) for a freshly persisted
// note.
func (h *Hooks) OnNoteCreated(note *model.Note) {
	if h == nil || note == nil {
		return
	}
	f := h.flags()
	if h.Notes != nil {
		_ = h.Notes.Update(note, true)
	}
	noteIsRemote := note.UserHost != nil && *note.UserHost != ""
	if h.PerUserNotes != nil && note.UserID != "" && (f.remoteUser || !noteIsRemote) {
		_ = h.PerUserNotes.Update(note.UserID, note, true)
	}
	// 本家 NoteCreateService.postNoteCreated は instance の notesCount と
	// instanceChart.updateNote を enableStatsForFederatedInstances の分岐の
	// 中でだけ行い、chart はさらに enableChartsForFederatedInstances を見る (#3330)。
	if noteIsRemote && f.fedStats {
		h.addInstanceCount(*note.UserHost, instanceNotesCount, 1)
		if h.Instance != nil && f.fedCharts {
			_ = h.Instance.UpdateNote(*note.UserHost, note, true)
		}
	}
	// 書き込みアクティブユーザー: ローカルユーザーのみカウントする。
	if h.ActiveUsers != nil && note.UserID != "" && (note.UserHost == nil || *note.UserHost == "") {
		_ = h.ActiveUsers.Write(note.UserID, time.Time{})
	}
}

// OnNoteDeleted fires NotesChart, PerUserNotesChart and InstanceChart
// for a removed note. activeUsers is not touched on delete (mirrors
// upstream which only commits write events on create).
func (h *Hooks) OnNoteDeleted(note *model.Note) {
	if h == nil || note == nil {
		return
	}
	f := h.flags()
	if h.Notes != nil {
		_ = h.Notes.Update(note, false)
	}
	noteIsRemote := note.UserHost != nil && *note.UserHost != ""
	if h.PerUserNotes != nil && note.UserID != "" && (f.remoteUser || !noteIsRemote) {
		_ = h.PerUserNotes.Update(note.UserID, note, false)
	}
	// 本家 NoteDeleteService.delete も作成と同じ入れ子 (#3330)。
	if noteIsRemote && f.fedStats {
		h.addInstanceCount(*note.UserHost, instanceNotesCount, -1)
		if h.Instance != nil && f.fedCharts {
			_ = h.Instance.UpdateNote(*note.UserHost, note, false)
		}
	}
}

// --- following hook ----------------------------------------------------------

// OnFollow fires PerUserFollowingChart and InstanceChart (when one side
// is remote) for a successful follow event.
func (h *Hooks) OnFollow(follower, followee *model.User) {
	h.commitFollow(follower, followee, true)
}

// OnUnfollow is the inverse of OnFollow.
func (h *Hooks) OnUnfollow(follower, followee *model.User) {
	h.commitFollow(follower, followee, false)
}

func (h *Hooks) commitFollow(follower, followee *model.User, isFollow bool) {
	if h == nil || follower == nil || followee == nil {
		return
	}
	f := h.flags()
	if h.PerUserFollowing != nil {
		// リモートユーザーのチャートは ChartsForRemoteUser で制御
		followerRemote := isRemote(follower)
		followeeRemote := isRemote(followee)
		if f.remoteUser || (!followerRemote && !followeeRemote) {
			_ = h.PerUserFollowing.Update(follower, followee, isFollow)
		}
	}
	// 本家 UserFollowingService は instanceChart の更新を
	// enableStatsForFederatedInstances の分岐の中でだけ行い、向きは instance の
	// 集計列と同じ「その host の側から見た」数 (#3330)。remote → local は
	// その host の利用者がフォローしている側なので updateFollowing、
	// local → remote はフォローされている側なので updateFollowers。
	if h.Instance != nil && f.fedStats && f.fedCharts {
		if isRemote(follower) && !isRemote(followee) {
			_ = h.Instance.UpdateFollowing(*follower.Host, isFollow)
		}
		if !isRemote(follower) && isRemote(followee) {
			_ = h.Instance.UpdateFollowers(*followee.Host, isFollow)
		}
	}
}

// OnFollowersMovedAway records the chart side of upstream
// AccountMoveService.adjustFollowingCounts: the local followers of oldAccount
// each lose one following, and when oldAccount is remote its instance loses
// followers. The instance followersCount column itself is adjusted by the
// move service.
//
// 本家は perUserFollowingChart をフォロワーごとに 1 回ずつ減らし、
// instanceChart.updateFollowers(host, false) は**人数に関わらず 1 回だけ**
// 呼ぶ (集計列のほうは人数ぶん減らす)。chart の値が人数とずれるのも本家の
// とおりに写している (#3330)。instance chart は他のフォロー系と同じく
// enableStatsForFederatedInstances の分岐の中。
func (h *Hooks) OnFollowersMovedAway(oldAccount *model.User, localFollowerIDs []string) {
	if h == nil || oldAccount == nil || len(localFollowerIDs) == 0 {
		return
	}
	f := h.flags()
	oldRemote := isRemote(oldAccount)
	if h.PerUserFollowing != nil && (f.remoteUser || !oldRemote) {
		for _, followerID := range localFollowerIDs {
			if followerID == "" {
				continue
			}
			_ = h.PerUserFollowing.Update(&model.User{ID: followerID}, oldAccount, false)
		}
	}
	if h.Instance != nil && oldRemote && f.fedStats && f.fedCharts {
		_ = h.Instance.UpdateFollowers(*oldAccount.Host, false)
	}
}

// --- reaction hook -----------------------------------------------------------

// OnReactionCreated fires PerUserReactionChart for one reaction event.
// The reactor's host decides the local/remote split; the recipient
// (note owner) is the chart group.
func (h *Hooks) OnReactionCreated(reactor *model.User, note *model.Note) {
	if h == nil || reactor == nil || note == nil {
		return
	}
	if h.PerUserReaction != nil {
		_ = h.PerUserReaction.Update(reactor, note)
	}
}

// --- drive hook --------------------------------------------------------------

// OnFileUploaded fires DriveChart, PerUserDriveChart and InstanceChart
// (when remote) for a freshly persisted drive file.
func (h *Hooks) OnFileUploaded(file *model.DriveFile) {
	h.commitDrive(file, true)
}

// OnFileDeleted is the inverse of OnFileUploaded.
func (h *Hooks) OnFileDeleted(file *model.DriveFile) {
	h.commitDrive(file, false)
}

func (h *Hooks) commitDrive(file *model.DriveFile, isAdditional bool) {
	if h == nil || file == nil {
		return
	}
	f := h.flags()
	if h.Drive != nil {
		_ = h.Drive.Update(file, isAdditional)
	}
	fileIsRemote := file.UserHost != nil && *file.UserHost != ""
	if h.PerUserDrive != nil && file.UserID != nil && *file.UserID != "" && (f.remoteUser || !fileIsRemote) {
		_ = h.PerUserDrive.Update(file, isAdditional)
	}
	// 本家 DriveService は instanceChart.updateDrive を
	// enableChartsForFederatedInstances だけで判定する (stats の分岐の外)。
	// instance に drive の集計列は無い。
	if h.Instance != nil && fileIsRemote && f.fedCharts {
		_ = h.Instance.UpdateDrive(file, isAdditional)
	}
}

// --- federation hooks --------------------------------------------------------

// OnRemoteUserCreated fires UsersChart and InstanceChart.NewUser for a
// remote user observed for the first time by the federation resolver.
func (h *Hooks) OnRemoteUserCreated(user *model.User) {
	if h == nil || user == nil {
		return
	}
	if h.Users != nil {
		_ = h.Users.Update(user, true)
	}
	// 本家 ApPersonService.createPerson は instance の usersCount の加算と
	// instanceChart.newUser を enableStatsForFederatedInstances の分岐の中で
	// 行い、chart はさらに enableChartsForFederatedInstances を見る (#3330)。
	if !isRemote(user) {
		return
	}
	f := h.flags()
	if f.fedStats {
		h.addInstanceCount(*user.Host, instanceUsersCount, 1)
		if h.Instance != nil && f.fedCharts {
			_ = h.Instance.NewUser(*user.Host)
		}
	}
}

// OnInboxReceived fires ApRequestChart, FederationChart and
// InstanceChart for one inbox event from the given remote host.
func (h *Hooks) OnInboxReceived(host string) {
	if h == nil {
		return
	}
	if h.ApRequest != nil {
		_ = h.ApRequest.Inbox()
	}
	if h.Federation != nil && host != "" {
		_ = h.Federation.Inbox(host)
	}
	// 本家 InboxProcessorService は instanceChart.requestReceived を
	// enableChartsForFederatedInstances の下でだけ呼ぶ (#3330)。
	if h.Instance != nil && host != "" && h.flags().fedCharts {
		_ = h.Instance.RequestReceived(host)
	}
}

// OnDelivered fires ApRequestChart, FederationChart and InstanceChart
// for one outbound delivery attempt.
func (h *Hooks) OnDelivered(host string, succeeded bool) {
	if h == nil {
		return
	}
	if h.ApRequest != nil {
		if succeeded {
			_ = h.ApRequest.DeliverSucceeded()
		} else {
			_ = h.ApRequest.DeliverFailed()
		}
	}
	if h.Federation != nil && host != "" {
		_ = h.Federation.Delivered(host, succeeded)
	}
	// 本家 DeliverProcessorService も instanceChart.requestSent を
	// enableChartsForFederatedInstances の下でだけ呼ぶ (#3330)。
	if h.Instance != nil && host != "" && h.flags().fedCharts {
		_ = h.Instance.RequestSent(host, succeeded)
	}
}

// --- pv / active-user read hooks ---------------------------------------------

// OnUserShow records a profile view in PerUserPvChart and counts the
// viewing user against ActiveUsersChart.Read when authenticated.
//
// `viewerID` may be empty for anonymous viewers; in that case
// PerUserPvChart records the visitor key (typically a session id) and
// ActiveUsersChart is not touched.
func (h *Hooks) OnUserShow(ownerID, viewerID, visitorKey string) {
	if h == nil {
		return
	}
	if h.PerUserPv != nil && ownerID != "" {
		if viewerID != "" {
			_ = h.PerUserPv.CommitByUser(ownerID, viewerID)
		} else if visitorKey != "" {
			_ = h.PerUserPv.CommitByVisitor(ownerID, visitorKey)
		}
	}
	if h.ActiveUsers != nil && viewerID != "" && h.IDGen != nil {
		if t, err := h.IDGen.ParseTime(viewerID); err == nil {
			_ = h.ActiveUsers.Read(viewerID, t)
		}
	}
}

// isRemote reports whether the user has a non-empty host.
func isRemote(u *model.User) bool {
	return u != nil && u.Host != nil && *u.Host != ""
}
