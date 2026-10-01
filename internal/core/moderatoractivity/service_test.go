package moderatoractivity

import (
	"errors"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fakes ------------------------------------------------------------------

type fakeModerators struct {
	users []*model.User
	err   error
}

func (f *fakeModerators) GetModerators() ([]*model.User, error) { return f.users, f.err }

type fakeMeta struct {
	meta      *model.Meta
	fetchErr  error
	updated   map[string]any
	updateErr error
}

func (f *fakeMeta) Fetch() (*model.Meta, error) { return f.meta, f.fetchErr }
func (f *fakeMeta) Update(fields map[string]any) error {
	f.updated = fields
	return f.updateErr
}

type fakeProfiles struct{ byID map[string]*model.UserProfile }

func (f *fakeProfiles) FindProfilesByUserIDs(ids []string) ([]*model.UserProfile, error) {
	var out []*model.UserProfile
	for _, id := range ids {
		if p, ok := f.byID[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

type fakeAnnounce struct{ created []*model.Announcement }

func (f *fakeAnnounce) Create(a *model.Announcement) error {
	f.created = append(f.created, a)
	return nil
}

type fakeWebhook struct {
	events []string
	bodies []any
}

func (f *fakeWebhook) DispatchSystem(eventType string, body any) {
	f.events = append(f.events, eventType)
	f.bodies = append(f.bodies, body)
}

type fakeIDGen struct{ n int }

func (f *fakeIDGen) Generate(_ time.Time) string { f.n++; return "ann" }

func ptr[T any](v T) *T { return &v }

// newSvc wires a service with a fixed clock and capturing fakes.
func newSvc(now time.Time, mods *fakeModerators, meta *fakeMeta) (*Service, *fakeAnnounce, *fakeWebhook, *[]string) {
	ann := &fakeAnnounce{}
	wh := &fakeWebhook{}
	var sent []string
	profiles := &fakeProfiles{byID: map[string]*model.UserProfile{}}
	for _, u := range mods.users {
		profiles.byID[u.ID] = &model.UserProfile{UserID: u.ID, Email: ptr(u.ID + "@x.test"), EmailVerified: true}
	}
	s := NewService(mods, meta, profiles, ann, wh, &fakeIDGen{}, func(to, _, _ string) {
		sent = append(sent, to)
	})
	s.now = func() time.Time { return now }
	return s, ann, wh, &sent
}

func userWithActive(id string, last *time.Time) *model.User {
	return &model.User{ID: id, LastActiveDate: last}
}

// --- tests ------------------------------------------------------------------

func TestCheck_AlreadyInvitationOnly_NoOp(t *testing.T) {
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	meta := &fakeMeta{meta: &model.Meta{DisableRegistration: true}}
	s, _, _, _ := newSvc(now, &fakeModerators{}, meta)
	require.NoError(t, s.Check())
	assert.Nil(t, meta.updated, "既に招待制なら meta を触らない")
}

// 承認制と「受け付けない」のときは、モデレーターが全員非アクティブでも触らない
// (#3186)。承認制で disableRegistration だけを立てると、承認制の入口まで塞がる。
func TestCheck_ApprovalOrClosed_NoOp(t *testing.T) {
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-10 * 24 * time.Hour)
	for name, m := range map[string]*model.Meta{
		"approval": {ApprovalRequiredForSignup: true},
		"closed":   {RegistrationClosed: true},
	} {
		t.Run(name, func(t *testing.T) {
			meta := &fakeMeta{meta: m}
			mods := &fakeModerators{users: []*model.User{userWithActive("a", &old)}}
			s, ann, wh, _ := newSvc(now, mods, meta)
			require.NoError(t, s.Check())
			assert.Nil(t, meta.updated)
			assert.Empty(t, ann.created)
			assert.Empty(t, wh.events)
		})
	}
}

func TestCheck_NoModeratorsWithLastActive_NoOp(t *testing.T) {
	// lastActiveDate を持つ moderator が 0 人 → 安全側で no-op (登録を無効化しない)。
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	meta := &fakeMeta{meta: &model.Meta{DisableRegistration: false}}
	mods := &fakeModerators{users: []*model.User{userWithActive("a", nil)}}
	s, _, wh, _ := newSvc(now, mods, meta)
	require.NoError(t, s.Check())
	assert.Nil(t, meta.updated, "全 null は計測不能として登録を無効化しない")
	assert.Empty(t, wh.events)
}

func TestCheck_AllInactive_SwitchesToInvitationOnly(t *testing.T) {
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-10 * 24 * time.Hour) // 10 日前 = 閾値 (7日) より古い
	meta := &fakeMeta{meta: &model.Meta{DisableRegistration: false}}
	mods := &fakeModerators{users: []*model.User{
		userWithActive("a", &old),
		userWithActive("b", &old),
	}}
	s, ann, wh, sent := newSvc(now, mods, meta)
	require.NoError(t, s.Check())
	assert.Equal(t, map[string]any{"disableRegistration": true}, meta.updated)
	assert.Len(t, ann.created, 2, "moderator ごとに announcement を作る")
	for _, a := range ann.created {
		assert.True(t, a.ForExistingUsers)
		assert.True(t, a.NeedConfirmationToRead)
		require.NotNil(t, a.UserID)
	}
	assert.Equal(t, []string{"inactiveModeratorsInvitationOnlyChanged"}, wh.events)
	assert.Len(t, *sent, 2, "verified email を持つ moderator に送る")
}

func TestCheck_AllInactive_NotifiesEvenNullLastActiveModerator(t *testing.T) {
	// 全 (lastActiveDate を持つ) moderator が inactive で招待制に切替わるとき、
	// lastActiveDate==null の moderator (= 認証 API を叩いていない現役 panel admin)
	// にも announcement / email が届くこと (#1563 review fix)。
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-10 * 24 * time.Hour)
	meta := &fakeMeta{meta: &model.Meta{DisableRegistration: false}}
	mods := &fakeModerators{users: []*model.User{
		userWithActive("a", &old), // inactive (判定対象)
		userWithActive("b", nil),  // lastActiveDate null (判定対象外だが通知対象)
	}}
	s, ann, _, sent := newSvc(now, mods, meta)
	require.NoError(t, s.Check())
	assert.Equal(t, map[string]any{"disableRegistration": true}, meta.updated)
	// announcement / email は a と b の両方に届く。
	targets := map[string]bool{}
	for _, a := range ann.created {
		require.NotNil(t, a.UserID)
		targets[*a.UserID] = true
	}
	assert.True(t, targets["a"])
	assert.True(t, targets["b"], "lastActiveDate null の moderator にも announcement を作る")
	assert.Len(t, *sent, 2, "両方の verified email に送る")
}

func TestCheck_OneStillActive_NoSwitch(t *testing.T) {
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-10 * 24 * time.Hour)
	fresh := now.Add(-1 * time.Hour) // 直近 = active
	meta := &fakeMeta{meta: &model.Meta{DisableRegistration: false}}
	mods := &fakeModerators{users: []*model.User{
		userWithActive("a", &old),
		userWithActive("b", &fresh),
	}}
	s, _, wh, _ := newSvc(now, mods, meta)
	require.NoError(t, s.Check())
	assert.Nil(t, meta.updated, "1 人でも active なら招待制にしない")
	assert.Empty(t, wh.events, "remaining が大きいので警告も出ない")
}

func TestCheck_WarningWindow_SendsWarning(t *testing.T) {
	// newest lastActive を inactivePeriod + (2日 ちょうど) に置き、remainingDays=2,
	// remainingHours=48 (48%6==0) になるよう調整して警告が出ることを確認。
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	inactivePeriod := now.Add(-inactivityLimitDays * 24 * time.Hour)
	newest := inactivePeriod.Add(2 * 24 * time.Hour) // remaining = 2 日 = 48 時間
	oldEnough := now.Add(-20 * 24 * time.Hour)
	meta := &fakeMeta{meta: &model.Meta{DisableRegistration: false}}
	mods := &fakeModerators{users: []*model.User{
		userWithActive("a", &oldEnough), // inactive
		userWithActive("b", &newest),    // active (= newest)
	}}
	s, _, wh, sent := newSvc(now, mods, meta)
	require.NoError(t, s.Check())
	assert.Nil(t, meta.updated, "まだ全員 inactive ではないので切替なし")
	assert.Equal(t, []string{"inactiveModeratorsWarning"}, wh.events)
	assert.NotEmpty(t, *sent)
	// 本家の ModeratorInactivityRemainingTime と同じ 3 つを送る (#3261)。
	assert.Equal(t, map[string]any{"remainingTime": map[string]any{
		"time":    int64(48 * time.Hour / time.Millisecond),
		"asDays":  2,
		"asHours": 48,
	}}, wh.bodies[0])
}

func TestCheck_WarningWindow_NotOnSixHourBoundary_NoWarning(t *testing.T) {
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	inactivePeriod := now.Add(-inactivityLimitDays * 24 * time.Hour)
	// remaining = 1 日 + 1 時間 = 25 時間 (25%6 != 0) → 警告なし。
	newest := inactivePeriod.Add(25 * time.Hour)
	oldEnough := now.Add(-20 * 24 * time.Hour)
	meta := &fakeMeta{meta: &model.Meta{DisableRegistration: false}}
	mods := &fakeModerators{users: []*model.User{
		userWithActive("a", &oldEnough),
		userWithActive("b", &newest),
	}}
	s, _, wh, _ := newSvc(now, mods, meta)
	require.NoError(t, s.Check())
	assert.Empty(t, wh.events, "6 時間境界でないので警告を送らない")
}

func TestCheck_FetchError_Propagates(t *testing.T) {
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	meta := &fakeMeta{fetchErr: errors.New("db down")}
	s, _, _, _ := newSvc(now, &fakeModerators{}, meta)
	assert.Error(t, s.Check())
}

func TestCheck_NilDeps_NoOp(t *testing.T) {
	s := NewService(nil, nil, nil, nil, nil, nil, nil)
	require.NoError(t, s.Check())
}

func TestCheck_WarningWindow_HoursVariant(t *testing.T) {
	// remaining = 6 時間 → remainingDays=0, remainingHours=6 (6%6==0)。
	// formatRemaining の "hours" 分岐 (英日両方) をカバーする。
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	inactivePeriod := now.Add(-inactivityLimitDays * 24 * time.Hour)
	newest := inactivePeriod.Add(6 * time.Hour)
	oldEnough := now.Add(-20 * 24 * time.Hour)
	meta := &fakeMeta{meta: &model.Meta{DisableRegistration: false}}
	mods := &fakeModerators{users: []*model.User{
		userWithActive("a", &oldEnough),
		userWithActive("b", &newest),
	}}
	s, _, wh, sent := newSvc(now, mods, meta)
	require.NoError(t, s.Check())
	assert.Equal(t, []string{"inactiveModeratorsWarning"}, wh.events)
	assert.NotEmpty(t, *sent)
	assert.Equal(t, map[string]any{"remainingTime": map[string]any{
		"time":    int64(6 * time.Hour / time.Millisecond),
		"asDays":  0,
		"asHours": 6,
	}}, wh.bodies[0])
}

func TestCheck_GetModeratorsError_Propagates(t *testing.T) {
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	meta := &fakeMeta{meta: &model.Meta{DisableRegistration: false}}
	mods := &fakeModerators{err: errors.New("roles down")}
	s, _, _, _ := newSvc(now, mods, meta)
	assert.Error(t, s.Check())
}

func TestNotify_NilProfilesAndEmail_NoPanic(t *testing.T) {
	// profiles / sendEmail / announce / webhook が nil でも panic せず切替だけ行う。
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-10 * 24 * time.Hour)
	meta := &fakeMeta{meta: &model.Meta{DisableRegistration: false}}
	mods := &fakeModerators{users: []*model.User{userWithActive("a", &old)}}
	s := NewService(mods, meta, nil, nil, nil, nil, nil)
	s.now = func() time.Time { return now }
	require.NoError(t, s.Check())
	assert.Equal(t, map[string]any{"disableRegistration": true}, meta.updated)
}

type erroringProfiles struct{}

func (erroringProfiles) FindProfilesByUserIDs(_ []string) ([]*model.UserProfile, error) {
	return nil, errors.New("profile db down")
}

func TestVerifiedEmails_LoadError_NoEmail(t *testing.T) {
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	old := now.Add(-10 * 24 * time.Hour)
	meta := &fakeMeta{meta: &model.Meta{DisableRegistration: false}}
	mods := &fakeModerators{users: []*model.User{userWithActive("a", &old)}}
	var sent []string
	s := NewService(mods, meta, erroringProfiles{}, &fakeAnnounce{}, &fakeWebhook{}, &fakeIDGen{},
		func(to, _, _ string) { sent = append(sent, to) })
	s.now = func() time.Time { return now }
	require.NoError(t, s.Check())
	assert.Empty(t, sent, "profile 取得失敗時はメールを送らない")
}

// time は時間や日に丸める前の残りのミリ秒 (#3261)。asHours から逆算しない。
func TestCheck_WarningWindow_TimeIsUnroundedMilliseconds(t *testing.T) {
	now := time.Date(2026, 1, 30, 12, 0, 0, 0, time.UTC)
	inactivePeriod := now.Add(-inactivityLimitDays * 24 * time.Hour)
	remaining := 48*time.Hour + 25*time.Minute + 1234*time.Millisecond
	newest := inactivePeriod.Add(remaining)
	oldEnough := now.Add(-20 * 24 * time.Hour)
	meta := &fakeMeta{meta: &model.Meta{DisableRegistration: false}}
	mods := &fakeModerators{users: []*model.User{
		userWithActive("a", &oldEnough),
		userWithActive("b", &newest),
	}}
	s, _, wh, _ := newSvc(now, mods, meta)
	require.NoError(t, s.Check())
	require.Len(t, wh.bodies, 1)
	rt := wh.bodies[0].(map[string]any)["remainingTime"].(map[string]any)
	assert.Equal(t, remaining.Milliseconds(), rt["time"])
	assert.Equal(t, 48, rt["asHours"])
	assert.Equal(t, 2, rt["asDays"])
}
