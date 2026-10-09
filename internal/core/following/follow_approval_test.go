package following_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

// 本家 should-require-follow-approval.ts の単体テストと同じ境界: 期間に達した
// 時点で止めなくなる。相手の出どころでローカル用・リモート用を選ぶ。
func TestRequiresFollowApproval_Boundaries(t *testing.T) {
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	now := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	profile := &model.UserProfile{FollowApprovalLocalSeconds: new(7 * 86400), FollowApprovalRemoteSeconds: new(14 * 86400)}
	user := func(created time.Time, remote bool) *model.User {
		u := &model.User{ID: gen.Generate(created)}
		if remote {
			h := "remote.example"
			u.Host = &h
		}
		return u
	}

	// 10 日前に作られた人: ローカル用 (7 日) には足り、リモート用 (14 日) には足りない。
	assert.False(t, following.RequiresFollowApproval(profile, user(now.Add(-10*day), false), gen, now))
	assert.True(t, following.RequiresFollowApproval(profile, user(now.Add(-10*day), true), gen, now))

	for _, remote := range []bool{false, true} {
		days := time.Duration(7)
		if remote {
			days = 14
		}
		// aidx は ms 精度なので、境界の前後 1ms で見る。
		assert.True(t, following.RequiresFollowApproval(profile, user(now.Add(-days*day+time.Millisecond), remote), gen, now), "remote=%v: 1ms 足りない", remote)
		assert.False(t, following.RequiresFollowApproval(profile, user(now.Add(-days*day), remote), gen, now), "remote=%v: ちょうど", remote)
		assert.False(t, following.RequiresFollowApproval(profile, user(now.Add(-days*day-time.Millisecond), remote), gen, now), "remote=%v: 過ぎた", remote)
	}
}

// 0 と未設定は止めない。profile や follower が無いとき、ID を読めないときも
// 止めない。
func TestRequiresFollowApproval_Disabled(t *testing.T) {
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	now := time.Now()
	fresh := &model.User{ID: gen.Generate(now)}
	for name, p := range map[string]*model.UserProfile{
		"nil":  {},
		"zero": {FollowApprovalLocalSeconds: new(0), FollowApprovalRemoteSeconds: new(0)},
	} {
		assert.False(t, following.RequiresFollowApproval(p, fresh, gen, now), name)
	}
	on := &model.UserProfile{FollowApprovalLocalSeconds: new(3600)}
	assert.True(t, following.RequiresFollowApproval(on, fresh, gen, now), "前提: 設定があれば止める")
	assert.False(t, following.RequiresFollowApproval(nil, fresh, gen, now))
	assert.False(t, following.RequiresFollowApproval(on, nil, gen, now))
	assert.False(t, following.RequiresFollowApproval(on, &model.User{ID: "!"}, gen, now), "ID を読めなければ止めない")
	assert.False(t, following.RequiresFollowApproval(on, fresh, nil, now), "ID の日時を出せなければ止めない")

	// 0 は止めない。作成日時が少し未来 (時計のずれとして受け付ける範囲) でも同じ。
	h := "remote.example"
	ahead := now.Add(time.Minute)
	skewed := &model.User{ID: gen.Generate(now), Host: &h, AccountCreatedAt: &ahead}
	assert.False(t, following.RequiresFollowApproval(&model.UserProfile{FollowApprovalRemoteSeconds: new(0)}, skewed, gen, now))
}

// 期間の判定で profile を読めない障害は、止めずに通すのではなくエラーにする
// (DB 障害を not-found に丸めない、#2799)。profile が無いだけなら通す。
func TestFollow_ProfileLookupFailure(t *testing.T) {
	svc, userRepo, fRepo, _ := newSvc(t)
	addUser(t, userRepo, "alice", false)
	addUser(t, userRepo, "bob", false)

	res, err := svc.Follow("alice", "bob", following.FollowOptions{})
	require.NoError(t, err, "profile が無いだけなら通す")
	require.NotNil(t, res.Following)
	assert.Len(t, fRepo.Followings, 1)

	addUser(t, userRepo, "carol", false)
	userRepo.FindProfileErr = errors.New("db down")
	_, err = svc.Follow("alice", "carol", following.FollowOptions{})
	require.Error(t, err)
	assert.Len(t, fRepo.Followings, 1, "障害のときにフォローを成立させない")
}

// 一括承認でも、profile を読めない障害と、リクエストの一覧を読めない障害は
// エラーにする。
func TestAcceptAllRequests_Failures(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	frRepo := &failingListRequestRepo{MockFollowRequestRepository: testutil.NewMockFollowRequestRepository()}
	svc := newSvcWith(userRepo, testutil.NewMockFollowingRepository(), frRepo)
	addUser(t, userRepo, "alice", true)
	addUser(t, userRepo, "bob", false)
	_, err := svc.Follow("bob", "alice", following.FollowOptions{})
	require.NoError(t, err)

	userRepo.FindProfileErr = errors.New("db down")
	require.Error(t, svc.AcceptAllRequests("alice"))
	userRepo.FindProfileErr = nil

	frRepo.err = errors.New("db down")
	require.Error(t, svc.AcceptAllRequests("alice"))
	frRepo.err = nil

	// profile が無いだけなら期間の設定も無いので、全て承認する。
	require.NoError(t, svc.AcceptAllRequests("alice"))
	ok, err := frRepo.Exists("bob", "alice")
	require.NoError(t, err)
	assert.False(t, ok)
}

// 未配線なら記録せず、一覧は空。配線の有無は起動時検査が見る。
func TestSilentFollowRepo_Unwired(t *testing.T) {
	svc, _, _, _ := newSvc(t)
	assert.False(t, svc.HasSilentFollowRepo())
	rows, err := svc.ListSilentFollows("alice", 10, "", "")
	require.NoError(t, err)
	assert.Empty(t, rows)
	svc.SetSilentFollowRepo(nopSilentFollowRepo{})
	assert.True(t, svc.HasSilentFollowRepo())
}

// failingListRequestRepo fails ListReceived while err is set.
type failingListRequestRepo struct {
	*testutil.MockFollowRequestRepository
	err error
}

func (r *failingListRequestRepo) ListReceived(userID string, limit int, sinceID, untilID string) ([]*model.FollowRequest, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.MockFollowRequestRepository.ListReceived(userID, limit, sinceID, untilID)
}

type nopSilentFollowRepo struct{}

func (nopSilentFollowRepo) Record(*model.SilentFollow) error { return nil }
func (nopSilentFollowRepo) ListByFollowee(string, int, string, string) ([]*model.SilentFollow, error) {
	return nil, nil
}

var _ repository.SilentFollowRepository = nopSilentFollowRepo{}

type failingSilentFollowRepo struct{ nopSilentFollowRepo }

func (failingSilentFollowRepo) Record(*model.SilentFollow) error { return errors.New("db down") }

// 記録に失敗しても、成立したフォローは取り消さない (記録は付随するもの)。
// 通知も出さないまま。
func TestFollow_SilentFollowRecordFailureKeepsFollow(t *testing.T) {
	svc, userRepo, fRepo, _ := newSvc(t)
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	followee := gen.Generate(time.Now().Add(-24 * time.Hour))
	follower := gen.Generate(time.Now())
	addUser(t, userRepo, followee, false)
	addUser(t, userRepo, follower, false)
	userRepo.Profiles[followee] = &model.UserProfile{UserID: followee, FollowApprovalLocalSeconds: new(3600), FollowApprovalAction: model.FollowApprovalActionSilentFollow}
	hook := &recordingHook{}
	svc.SetNotificationHook(hook)
	svc.SetSilentFollowRepo(failingSilentFollowRepo{})

	res, err := svc.Follow(follower, followee, following.FollowOptions{})
	require.NoError(t, err)
	require.NotNil(t, res.Following)
	assert.Len(t, fRepo.Followings, 1)
	assert.Empty(t, hook.follows)
}
