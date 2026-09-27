package abuse_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/core/abuse"
	"github.com/shiroha-a/mk/internal/core/notification"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubMods struct {
	users []*model.User
	err   error
}

func (s *stubMods) GetModerators() ([]*model.User, error) { return s.users, s.err }

type recordingNotifier struct {
	inputs []notification.CreateInput
	opts   []notification.Coalesce
	err    error
}

func (r *recordingNotifier) CreateCoalesced(_ context.Context, in notification.CreateInput, opt notification.Coalesce) (*notification.Notification, error) {
	r.inputs = append(r.inputs, in)
	r.opts = append(r.opts, opt)
	return nil, r.err
}

// quiet reports the Quiet flag of each call.
func (r *recordingNotifier) quiet() []bool {
	out := make([]bool, len(r.opts))
	for i, o := range r.opts {
		out[i] = o.Quiet
	}
	return out
}

type failingLookup struct {
	*testutil.MockAbuseReportRepository
	pairErr, hostErr, stateErr error
}

func (f *failingLookup) HasOlderUnresolvedByPair(a, b, c string) (bool, error) {
	if f.pairErr != nil {
		return true, f.pairErr
	}
	return f.MockAbuseReportRepository.HasOlderUnresolvedByPair(a, b, c)
}

func (f *failingLookup) HasOlderUnresolvedFromHost(h, c string) (bool, error) {
	if f.hostErr != nil {
		return true, f.hostErr
	}
	return f.MockAbuseReportRepository.HasOlderUnresolvedFromHost(h, c)
}

func (f *failingLookup) FindStatesByIDs(ids []string) (map[string]model.AbuseReportState, error) {
	if f.stateErr != nil {
		return nil, f.stateErr
	}
	return f.MockAbuseReportRepository.FindStatesByIDs(ids)
}

func mods(ids ...string) *stubMods {
	s := &stubMods{}
	for _, id := range ids {
		s.users = append(s.users, &model.User{ID: id})
	}
	return s
}

func strp(s string) *string { return &s }

func TestInAppNotifier_NotifiesEveryModerator(t *testing.T) {
	repo := testutil.NewMockAbuseReportRepository()
	rec := &recordingNotifier{}
	n := abuse.NewInAppNotifier(mods("m1", "m2"), rec, repo)

	r := &model.AbuseUserReport{ID: "r1", ReporterID: "u", TargetUserID: "t", Comment: "secret"}
	require.NoError(t, repo.Create(r))
	n.NotifyNewReport(context.Background(), r)

	require.Len(t, rec.inputs, 2)
	assert.Equal(t, "m1", rec.inputs[0].NotifieeID)
	assert.Equal(t, "m2", rec.inputs[1].NotifieeID)
	for i, in := range rec.inputs {
		assert.Equal(t, time.Duration(10)*time.Minute, rec.opts[i].Window)
		assert.False(t, rec.opts[i].Quiet)
		assert.Equal(t, notification.TypeAbuseReport, in.Type)
		assert.Equal(t, "u", in.NotifierID)
		assert.Equal(t, map[string]any{"reportId": "r1", "targetUserId": "t"}, in.Extra, "comment must not be copied into Redis")
	}
}

// TestInAppNotifier_RepeatedPairIsQuiet: 同じ組を未解決のまま重ねた通報は既存の通知を
// 作り直さない (Quiet)。最古の 1 件だけは作り直せる (並行投入で両方が黙らないため)。
func TestInAppNotifier_RepeatedPairIsQuiet(t *testing.T) {
	repo := testutil.NewMockAbuseReportRepository()
	rec := &recordingNotifier{}
	n := abuse.NewInAppNotifier(mods("m1"), rec, repo)

	first := &model.AbuseUserReport{ID: "r1", ReporterID: "u", TargetUserID: "t"}
	second := &model.AbuseUserReport{ID: "r2", ReporterID: "u", TargetUserID: "t"}
	require.NoError(t, repo.Create(first))
	require.NoError(t, repo.Create(second))

	n.NotifyNewReport(context.Background(), second)
	n.NotifyNewReport(context.Background(), first)
	other := &model.AbuseUserReport{ID: "r3", ReporterID: "u", TargetUserID: "t2"}
	require.NoError(t, repo.Create(other))
	n.NotifyNewReport(context.Background(), other)
	// 既存が無いモデレーターには Quiet でも作る (CreateCoalesced の責務) ので、
	// 呼び出し自体は毎回行う。
	assert.Equal(t, []bool{true, false, false}, rec.quiet(), "repeat / oldest / different target")
}

func TestInAppNotifier_RemoteHostWithUnresolvedReportIsQuiet(t *testing.T) {
	repo := testutil.NewMockAbuseReportRepository()
	rec := &recordingNotifier{}
	n := abuse.NewInAppNotifier(mods("m1"), rec, repo)

	first := &model.AbuseUserReport{ID: "r1", ReporterID: "a", TargetUserID: "t1", ReporterHost: strp("flood.example")}
	second := &model.AbuseUserReport{ID: "r2", ReporterID: "b", TargetUserID: "t2", ReporterHost: strp("flood.example")}
	elsewhere := &model.AbuseUserReport{ID: "r3", ReporterID: "c", TargetUserID: "t3", ReporterHost: strp("other.example")}
	for _, r := range []*model.AbuseUserReport{first, second, elsewhere} {
		require.NoError(t, repo.Create(r))
	}

	n.NotifyNewReport(context.Background(), second)
	n.NotifyNewReport(context.Background(), elsewhere)
	require.NoError(t, repo.UpdateFields("r1", map[string]any{"resolved": true}))
	n.NotifyNewReport(context.Background(), second)
	assert.Equal(t, []bool{true, false, false}, rec.quiet(), "same host / other host / after the older one is resolved")
}

// TestInAppNotifier_LocalReportsAreNotThrottledByHost: ローカルは reporterHost が
// NULL。ホストで絞ると別々の利用者の通報まで 1 件目以降が消える。
func TestInAppNotifier_LocalReportsAreNotThrottledByHost(t *testing.T) {
	repo := testutil.NewMockAbuseReportRepository()
	rec := &recordingNotifier{}
	n := abuse.NewInAppNotifier(mods("m1"), rec, repo)

	first := &model.AbuseUserReport{ID: "r1", ReporterID: "a", TargetUserID: "t1"}
	second := &model.AbuseUserReport{ID: "r2", ReporterID: "b", TargetUserID: "t2"}
	require.NoError(t, repo.Create(first))
	require.NoError(t, repo.Create(second))

	n.NotifyNewReport(context.Background(), second)
	assert.Equal(t, []bool{false}, rec.quiet())
}

func TestInAppNotifier_AliveChecksReportExists(t *testing.T) {
	repo := testutil.NewMockAbuseReportRepository()
	rec := &recordingNotifier{}
	n := abuse.NewInAppNotifier(mods("m1"), rec, repo)

	r := &model.AbuseUserReport{ID: "r1", ReporterID: "u", TargetUserID: "t"}
	require.NoError(t, repo.Create(r))
	n.NotifyNewReport(context.Background(), r)
	require.Len(t, rec.opts, 1)
	alive := rec.opts[0].Alive
	require.NotNil(t, alive, "without alive, notifications of deleted reports would suppress new ones")

	withID := func(id string) *notification.Notification {
		return &notification.Notification{Extra: map[string]any{"reportId": id}}
	}
	assert.True(t, alive(withID("r1")))
	assert.False(t, alive(withID("gone")))
	assert.False(t, alive(&notification.Notification{}))
}

// TestInAppNotifier_LookupErrorsLeanTowardNotifying: DB エラーで見送ると、
// 通報が届いているのに誰も気付けない。
func TestInAppNotifier_LookupErrorsLeanTowardNotifying(t *testing.T) {
	boom := errors.New("boom")
	repo := &failingLookup{MockAbuseReportRepository: testutil.NewMockAbuseReportRepository(), pairErr: boom, hostErr: boom, stateErr: boom}
	rec := &recordingNotifier{}
	n := abuse.NewInAppNotifier(mods("m1"), rec, repo)

	n.NotifyNewReport(context.Background(), &model.AbuseUserReport{ID: "r1", ReporterID: "u", TargetUserID: "t", ReporterHost: strp("x.example")})
	require.Len(t, rec.inputs, 1)
	assert.Equal(t, []bool{false}, rec.quiet(), "a failed check must not keep the stale notification")
	assert.False(t, rec.opts[0].Alive(&notification.Notification{Extra: map[string]any{"reportId": "r1"}}))
}

func TestInAppNotifier_NoOps(t *testing.T) {
	repo := testutil.NewMockAbuseReportRepository()
	rec := &recordingNotifier{}
	r := &model.AbuseUserReport{ID: "r1", ReporterID: "u", TargetUserID: "t"}

	var nilNotifier *abuse.InAppNotifier
	nilNotifier.NotifyNewReport(context.Background(), r)
	abuse.NewInAppNotifier(nil, rec, repo).NotifyNewReport(context.Background(), r)
	abuse.NewInAppNotifier(mods("m1"), nil, repo).NotifyNewReport(context.Background(), r)
	abuse.NewInAppNotifier(mods("m1"), rec, nil).NotifyNewReport(context.Background(), r)
	abuse.NewInAppNotifier(mods("m1"), rec, repo).NotifyNewReport(context.Background(), nil)
	abuse.NewInAppNotifier(&stubMods{err: errors.New("boom")}, rec, repo).NotifyNewReport(context.Background(), r)
	assert.Empty(t, rec.inputs)

	// 作成失敗は握り潰す (通報は保存済み)。
	failing := &recordingNotifier{err: errors.New("boom")}
	abuse.NewInAppNotifier(mods("m1", "m2"), failing, repo).NotifyNewReport(context.Background(), r)
	assert.Len(t, failing.inputs, 2, "a failure for one moderator must not stop the others")
}

func TestInAppNotifier_DoesNotInheritCancel(t *testing.T) {
	repo := testutil.NewMockAbuseReportRepository()
	var seen context.Context
	n := abuse.NewInAppNotifier(mods("m1"), notifierFunc(func(ctx context.Context) { seen = ctx }), repo)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n.NotifyNewReport(ctx, &model.AbuseUserReport{ID: "r1", ReporterID: "u", TargetUserID: "t"})
	require.NotNil(t, seen)
	assert.NoError(t, seen.Err())
}

type notifierFunc func(ctx context.Context)

func (f notifierFunc) CreateCoalesced(ctx context.Context, _ notification.CreateInput, _ notification.Coalesce) (*notification.Notification, error) {
	f(ctx)
	return nil, nil
}
