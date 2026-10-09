package following_test

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

// approvalEnv is a following.Service over the real repositories (#3466).
type approvalEnv struct {
	t      *testing.T
	db     *gorm.DB
	idGen  id.Generator
	svc    *following.Service
	hook   *recordingHook
	frRepo repository.FollowRequestRepository
}

func newApprovalEnv(t *testing.T) *approvalEnv {
	t.Helper()
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	frRepo := repository.NewFollowRequestRepository(db)
	svc := following.NewService(repository.NewUserRepository(db), repository.NewFollowingRepository(db), frRepo, idGen)
	hook := &recordingHook{}
	svc.SetNotificationHook(hook)
	svc.SetSilentFollowRepo(repository.NewSilentFollowRepository(db))
	return &approvalEnv{t: t, db: db, idGen: idGen, svc: svc, hook: hook, frRepo: frRepo}
}

// userOpts describes a user row made by approvalEnv.user.
type userOpts struct {
	// seenAt は ID に埋める日時 (ローカルなら登録日、リモートならこのサーバーが
	// 初めて知った日時)。
	seenAt time.Time
	// host が空ならローカル。
	host             string
	accountCreatedAt *time.Time
	locked           bool
}

// user inserts a user and its profile, and returns the ID.
func (e *approvalEnv) user(o userOpts) string {
	e.t.Helper()
	uid := e.idGen.Generate(o.seenAt)
	name := "fa" + strings.ToLower(uid)
	var host *string
	if o.host != "" {
		host = &o.host
	}
	require.NoError(e.t, e.db.Exec(
		`INSERT INTO "user" (id, "updatedAt", username, "usernameLower", host, "isLocked", "accountCreatedAt") VALUES (?, NOW(), ?, ?, ?, ?, ?)`,
		uid, name, name, host, o.locked, o.accountCreatedAt,
	).Error)
	require.NoError(e.t, e.db.Exec(`INSERT INTO "user_profile" ("userId") VALUES (?)`, uid).Error)
	e.t.Cleanup(func() {
		e.db.Exec(`DELETE FROM "silent_follow" WHERE "followeeId" = ? OR "followerId" = ?`, uid, uid)
		e.db.Exec(`DELETE FROM "following" WHERE "followeeId" = ? OR "followerId" = ?`, uid, uid)
		e.db.Exec(`DELETE FROM "follow_request" WHERE "followeeId" = ? OR "followerId" = ?`, uid, uid)
		e.db.Exec(`DELETE FROM "user_profile" WHERE "userId" = ?`, uid)
		e.db.Exec(`DELETE FROM "user" WHERE id = ?`, uid)
	})
	return uid
}

// setProfile writes followApproval* / autoAcceptFollowed of userID.
func (e *approvalEnv) setProfile(userID string, fields map[string]any) {
	e.t.Helper()
	require.NoError(e.t, e.db.Model(&model.UserProfile{}).Where(`"userId" = ?`, userID).Updates(fields).Error)
}

func (e *approvalEnv) isFollowing(followerID, followeeID string) bool {
	e.t.Helper()
	var n int64
	require.NoError(e.t, e.db.Model(&model.Following{}).Where(`"followerId" = ? AND "followeeId" = ?`, followerID, followeeID).Count(&n).Error)
	return n > 0
}

func (e *approvalEnv) isRequested(followerID, followeeID string) bool {
	e.t.Helper()
	ok, err := e.frRepo.Exists(followerID, followeeID)
	require.NoError(e.t, err)
	return ok
}

func (e *approvalEnv) silentRows(followeeID string) []*model.SilentFollow {
	e.t.Helper()
	var rows []*model.SilentFollow
	require.NoError(e.t, e.db.Where(`"followeeId" = ?`, followeeID).Order("id").Find(&rows).Error)
	return rows
}

func ptrTime(t time.Time) *time.Time { return &t }

const hour = 3600

// ローカル・リモートのそれぞれで、設定した期間より新しい人からのフォローは
// リクエストになり (既定の止め方 = 本家と同じく通知付き)、古い人からのフォローは
// そのまま通る。期間は相手の出どころごとの設定だけを見る (#3466)。
func TestFollow_DB_AccountAgeThresholds(t *testing.T) {
	e := newApprovalEnv(t)
	now := time.Now()
	cases := []struct {
		name        string
		local       *int
		remote      *int
		follower    userOpts
		wantRequest bool
	}{
		{name: "local younger than the local setting", local: new(hour), follower: userOpts{seenAt: now.Add(-30 * time.Minute)}, wantRequest: true},
		{name: "local older than the local setting", local: new(hour), follower: userOpts{seenAt: now.Add(-2 * time.Hour)}},
		{name: "local follower ignores the remote setting", remote: new(hour), follower: userOpts{seenAt: now}},
		{name: "remote follower ignores the local setting", local: new(hour), follower: userOpts{seenAt: now, host: "fa-remote.example"}},
		{name: "remote younger than the remote setting", remote: new(hour), follower: userOpts{seenAt: now.Add(-30 * time.Minute), host: "fa-remote.example"}, wantRequest: true},
		{name: "remote older than the remote setting", remote: new(hour), follower: userOpts{seenAt: now.Add(-2 * time.Hour), host: "fa-remote.example"}},
		{name: "zero turns the setting off", local: new(0), follower: userOpts{seenAt: now}},
		{name: "null turns the setting off", follower: userOpts{seenAt: now}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e.hook.follows, e.hook.requests = nil, nil
			followee := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour)})
			e.setProfile(followee, map[string]any{"followApprovalLocalSeconds": intOrNil(tc.local), "followApprovalRemoteSeconds": intOrNil(tc.remote)})
			follower := e.user(tc.follower)

			res, err := e.svc.Follow(follower, followee, following.FollowOptions{})
			require.NoError(t, err)
			if tc.wantRequest {
				require.NotNil(t, res.Request)
				assert.True(t, e.isRequested(follower, followee))
				assert.False(t, e.isFollowing(follower, followee))
				assert.Equal(t, []string{follower + "->" + followee}, e.hook.requests, "既定の止め方は通知を出す")
				assert.Empty(t, e.hook.follows)
			} else {
				require.NotNil(t, res.Following)
				assert.True(t, e.isFollowing(follower, followee))
				assert.Equal(t, []string{follower + "->" + followee}, e.hook.follows)
				assert.Empty(t, e.hook.requests)
			}
		})
	}
}

func intOrNil(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// リモートの人は、作成日時 (#3465) があればそれで数え、無ければ ID の日時
// (このサーバーが初めて知った日時) で数える。本家は常に ID の日時 (#3466)。
func TestFollow_DB_RemoteAgeUsesAccountCreatedAt(t *testing.T) {
	e := newApprovalEnv(t)
	now := time.Now()
	cases := []struct {
		name        string
		follower    userOpts
		wantRequest bool
	}{
		// 初めて知ったのは今だが、何年も前に作られた人は止めない (本家なら止まる)。
		{name: "old account first seen just now", follower: userOpts{seenAt: now, host: "fa-old.example", accountCreatedAt: ptrTime(now.Add(-3 * 365 * 24 * time.Hour))}},
		// 作成日時が無ければ本家と同じく ID の日時で数える。
		{name: "unknown creation time falls back to the ID", follower: userOpts{seenAt: now, host: "fa-old.example"}, wantRequest: true},
		// 作成日時を ID の日時より優先する (前から知っている人でも、作られたのが
		// 最近なら止める)。
		{name: "creation time wins over an older ID", follower: userOpts{seenAt: now.Add(-10 * 24 * time.Hour), host: "fa-old.example", accountCreatedAt: ptrTime(now.Add(-time.Hour))}, wantRequest: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			followee := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour)})
			e.setProfile(followee, map[string]any{"followApprovalRemoteSeconds": 24 * hour})
			follower := e.user(tc.follower)

			res, err := e.svc.Follow(follower, followee, following.FollowOptions{})
			require.NoError(t, err)
			assert.Equal(t, tc.wantRequest, res.Request != nil)
			assert.Equal(t, !tc.wantRequest, e.isFollowing(follower, followee))
		})
	}
}

// 3 つの止め方で、フォローの状態・通知の有無・一覧への記録が issue の表の
// とおりになる (#3466)。
func TestFollow_DB_Actions(t *testing.T) {
	e := newApprovalEnv(t)
	now := time.Now()
	cases := []struct {
		action       string
		wantRequest  bool
		wantNotified bool
		wantSilent   bool
	}{
		{action: model.FollowApprovalActionRequest, wantRequest: true, wantNotified: true},
		{action: model.FollowApprovalActionSilentRequest, wantRequest: true},
		{action: model.FollowApprovalActionSilentFollow, wantSilent: true},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			e.hook.follows, e.hook.requests = nil, nil
			followee := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour)})
			e.setProfile(followee, map[string]any{"followApprovalLocalSeconds": hour, "followApprovalAction": tc.action})
			follower := e.user(userOpts{seenAt: now})

			res, err := e.svc.Follow(follower, followee, following.FollowOptions{})
			require.NoError(t, err)
			assert.Equal(t, tc.wantRequest, res.Request != nil)
			assert.Equal(t, tc.wantRequest, e.isRequested(follower, followee))
			assert.Equal(t, !tc.wantRequest, e.isFollowing(follower, followee))
			notified := len(e.hook.requests) + len(e.hook.follows)
			if tc.wantNotified {
				assert.Equal(t, 1, notified)
			} else {
				assert.Zero(t, notified, "通知を出さない")
			}
			rows := e.silentRows(followee)
			if tc.wantSilent {
				require.Len(t, rows, 1)
				assert.Equal(t, follower, rows[0].FollowerID)
				listed, err := e.svc.ListSilentFollows(followee, 10, "", "")
				require.NoError(t, err)
				require.Len(t, listed, 1)
				assert.Equal(t, rows[0].ID, listed[0].ID)
			} else {
				assert.Empty(t, rows)
			}
		})
	}
}

// 静かに成立させたフォローは、解除してまた来ても 1 行のまま、最後に来た日時に
// なる。期間を過ぎた人のフォローは記録しない。
func TestFollow_DB_SilentFollowRefollow(t *testing.T) {
	e := newApprovalEnv(t)
	now := time.Now()
	followee := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour)})
	e.setProfile(followee, map[string]any{"followApprovalLocalSeconds": hour, "followApprovalAction": model.FollowApprovalActionSilentFollow})
	follower := e.user(userOpts{seenAt: now})
	old := e.user(userOpts{seenAt: now.Add(-2 * time.Hour)})

	_, err := e.svc.Follow(follower, followee, following.FollowOptions{})
	require.NoError(t, err)
	first := e.silentRows(followee)
	require.Len(t, first, 1)
	require.NoError(t, e.svc.Unfollow(follower, followee))
	time.Sleep(2 * time.Millisecond)
	_, err = e.svc.Follow(follower, followee, following.FollowOptions{})
	require.NoError(t, err)
	_, err = e.svc.Follow(old, followee, following.FollowOptions{})
	require.NoError(t, err)

	again := e.silentRows(followee)
	require.Len(t, again, 1, "同じ人は 1 行にまとめ、期間を過ぎた人は記録しない")
	assert.Greater(t, again[0].ID, first[0].ID, "最後に来た日時へ書き換える")
	assert.Equal(t, []string{old + "->" + followee}, e.hook.follows, "期間を過ぎた人は通常どおり通知する")
}

// 自分がフォローしていて autoAcceptFollowed が有効な相手は、期間に関係なく
// 通常どおり (通知付きで) 成立する。鍵アカウントは止め方に関係なく通知付きの
// リクエストになる (本家どおり)。
func TestFollow_DB_AutoAcceptAndLockedOverrideAction(t *testing.T) {
	e := newApprovalEnv(t)
	now := time.Now()
	for _, action := range []string{model.FollowApprovalActionRequest, model.FollowApprovalActionSilentRequest, model.FollowApprovalActionSilentFollow} {
		t.Run("mutual/"+action, func(t *testing.T) {
			e.hook.follows, e.hook.requests = nil, nil
			followee := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour)})
			e.setProfile(followee, map[string]any{"followApprovalLocalSeconds": hour, "followApprovalAction": action, "autoAcceptFollowed": true})
			follower := e.user(userOpts{seenAt: now})
			_, err := e.svc.Follow(followee, follower, following.FollowOptions{})
			require.NoError(t, err)
			e.hook.follows = nil

			res, err := e.svc.Follow(follower, followee, following.FollowOptions{})
			require.NoError(t, err)
			require.NotNil(t, res.Following)
			assert.Equal(t, []string{follower + "->" + followee}, e.hook.follows)
			assert.Empty(t, e.silentRows(followee))
		})
		t.Run("locked/"+action, func(t *testing.T) {
			e.hook.follows, e.hook.requests = nil, nil
			followee := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour), locked: true})
			e.setProfile(followee, map[string]any{"followApprovalLocalSeconds": hour, "followApprovalAction": action, "autoAcceptFollowed": false})
			follower := e.user(userOpts{seenAt: now})

			res, err := e.svc.Follow(follower, followee, following.FollowOptions{})
			require.NoError(t, err)
			require.NotNil(t, res.Request)
			assert.Equal(t, []string{follower + "->" + followee}, e.hook.requests)
			assert.Empty(t, e.silentRows(followee))
		})
	}
}

// cancellingRequestRepo deletes the request from cancelFollowerID right after
// listing, like a follower cancelling while the bulk acceptance runs.
type cancellingRequestRepo struct {
	repository.FollowRequestRepository
	db               *gorm.DB
	cancelFollowerID string
}

func (r *cancellingRequestRepo) ListReceived(userID string, limit int, sinceID, untilID string) ([]*model.FollowRequest, error) {
	rows, err := r.FollowRequestRepository.ListReceived(userID, limit, sinceID, untilID)
	if err == nil && r.cancelFollowerID != "" {
		r.db.Exec(`DELETE FROM "follow_request" WHERE "followerId" = ? AND "followeeId" = ?`, r.cancelFollowerID, userID)
	}
	return rows, err
}

// 鍵を外したときの一括承認は本家と同じ条件 (#3466): 期間の条件に当たる人は
// 残し、自分がフォローしていて autoAcceptFollowed が有効なら承認する。止め方が
// silentFollow でも、残したリクエストを静かなフォローに変えない。処理中に
// 取り消されたリクエストは飛ばして残りを続ける。
func TestAcceptAllRequests_DB(t *testing.T) {
	for _, tc := range []struct {
		action     string
		autoAccept bool
	}{
		{model.FollowApprovalActionRequest, true},
		{model.FollowApprovalActionRequest, false},
		{model.FollowApprovalActionSilentFollow, true},
	} {
		t.Run(tc.action+map[bool]string{true: "/autoAccept", false: "/noAutoAccept"}[tc.autoAccept], func(t *testing.T) {
			e := newApprovalEnv(t)
			now := time.Now()
			followee := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour), locked: true})
			e.setProfile(followee, map[string]any{"followApprovalLocalSeconds": hour, "followApprovalAction": tc.action, "autoAcceptFollowed": tc.autoAccept})
			oldUser := e.user(userOpts{seenAt: now.Add(-2 * time.Hour)})
			newUser := e.user(userOpts{seenAt: now})
			newFollowed := e.user(userOpts{seenAt: now})
			cancelled := e.user(userOpts{seenAt: now.Add(-3 * time.Hour)})
			for _, f := range []string{oldUser, newUser, newFollowed, cancelled} {
				_, err := e.svc.Follow(f, followee, following.FollowOptions{})
				require.NoError(t, err)
				require.True(t, e.isRequested(f, followee))
			}
			// 鍵アカウントでも autoAcceptFollowed なら、後からフォローし返した
			// 相手のリクエストは残っている (フォローし返す前に来たため)。
			_, err := e.svc.Follow(followee, newFollowed, following.FollowOptions{})
			require.NoError(t, err)
			require.NoError(t, e.db.Exec(`UPDATE "user" SET "isLocked" = false WHERE id = ?`, followee).Error)

			svc := following.NewService(repository.NewUserRepository(e.db), repository.NewFollowingRepository(e.db),
				&cancellingRequestRepo{FollowRequestRepository: e.frRepo, db: e.db, cancelFollowerID: cancelled}, e.idGen)
			svc.SetSilentFollowRepo(repository.NewSilentFollowRepository(e.db))
			require.NoError(t, svc.AcceptAllRequests(followee))

			assert.True(t, e.isFollowing(oldUser, followee), "期間を過ぎた人は承認する")
			assert.False(t, e.isRequested(oldUser, followee))
			assert.False(t, e.isFollowing(newUser, followee), "期間の条件に当たる人は残す")
			assert.True(t, e.isRequested(newUser, followee))
			assert.Equal(t, tc.autoAccept, e.isFollowing(newFollowed, followee), "フォローしていて autoAcceptFollowed なら承認する")
			assert.Equal(t, !tc.autoAccept, e.isRequested(newFollowed, followee))
			assert.False(t, e.isFollowing(cancelled, followee), "取り消されたリクエストは飛ばす")
			assert.Empty(t, e.silentRows(followee), "一括承認で静かなフォローに変えない")
		})
	}
}

// 期間の設定が無ければ、溜まっていたリクエストを全て承認する (従来の本家と
// 同じ)。リクエストが無ければ何もしない。
func TestAcceptAllRequests_DB_NoSetting(t *testing.T) {
	e := newApprovalEnv(t)
	now := time.Now()
	followee := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour), locked: true})
	require.NoError(t, e.svc.AcceptAllRequests(followee))

	followers := []string{e.user(userOpts{seenAt: now}), e.user(userOpts{seenAt: now, host: "fa-remote.example"})}
	for _, f := range followers {
		_, err := e.svc.Follow(f, followee, following.FollowOptions{})
		require.NoError(t, err)
	}
	require.NoError(t, e.svc.AcceptAllRequests(followee))
	for _, f := range followers {
		assert.True(t, e.isFollowing(f, followee))
		assert.False(t, e.isRequested(f, followee))
	}
}

// 期間で申請中になった後に設定を外してフォローし直すと、即フォローが成立し、
// 残っていた申請は消える (本家 insertFollowingDoc と同じ)。「フォロー済みかつ
// 申請中」の行を作らない (#3466)。
func TestFollow_DB_RefollowAfterSettingChangeLeavesNoOrphan(t *testing.T) {
	e := newApprovalEnv(t)
	now := time.Now()
	followee := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour)})
	e.setProfile(followee, map[string]any{"followApprovalLocalSeconds": hour})
	follower := e.user(userOpts{seenAt: now})

	res, err := e.svc.Follow(follower, followee, following.FollowOptions{})
	require.NoError(t, err)
	require.NotNil(t, res.Request)

	e.setProfile(followee, map[string]any{"followApprovalLocalSeconds": nil})
	res, err = e.svc.Follow(follower, followee, following.FollowOptions{})
	require.NoError(t, err)
	require.NotNil(t, res.Following)
	assert.True(t, e.isFollowing(follower, followee))
	assert.False(t, e.isRequested(follower, followee), "残っていた申請を消す")
}

// 過去に作られた「フォロー済みかつ申請中」の行があっても、承認は申請を消すだけで
// 成功し、鍵を外したときの一括承認は止まらずに残りを承認する (#3466)。
func TestAcceptRequests_DB_OrphanedRequest(t *testing.T) {
	e := newApprovalEnv(t)
	now := time.Now()
	followee := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour), locked: true})
	orphans := []string{e.user(userOpts{seenAt: now.Add(-2 * time.Hour)}), e.user(userOpts{seenAt: now.Add(-3 * time.Hour)})}
	others := []string{e.user(userOpts{seenAt: now.Add(-4 * time.Hour)}), e.user(userOpts{seenAt: now.Add(-time.Hour)})}
	for _, f := range append(append([]string{}, orphans...), others...) {
		_, err := e.svc.Follow(f, followee, following.FollowOptions{})
		require.NoError(t, err)
	}
	// 修正前のコードが作った形を直接置く: following があり、申請も残っている。
	for _, o := range orphans {
		require.NoError(t, e.db.Exec(`INSERT INTO "following" (id, "followerId", "followeeId") VALUES (?, ?, ?)`, e.idGen.Generate(time.Now()), o, followee).Error)
		require.True(t, e.isRequested(o, followee))
	}

	require.NoError(t, e.svc.AcceptRequest(followee, orphans[0]), "既にフォロー済みなら申請を消して成功")
	assert.False(t, e.isRequested(orphans[0], followee))

	require.NoError(t, e.db.Exec(`UPDATE "user" SET "isLocked" = false WHERE id = ?`, followee).Error)
	require.NoError(t, e.svc.AcceptAllRequests(followee))
	for _, f := range append(append([]string{}, orphans...), others...) {
		assert.True(t, e.isFollowing(f, followee), f)
		assert.False(t, e.isRequested(f, followee), f)
	}
	var n int64
	require.NoError(t, e.db.Model(&model.Following{}).Where(`"followeeId" = ?`, followee).Count(&n).Error)
	assert.EqualValues(t, 4, n, "フォローを二重に作らない")
}

// 同じ申請を同時に承認しても (二度押し、鍵の解除の同時実行)、両方が成功し、
// フォローは 1 行だけになる。片方は following の一意制約に当たるが、もう片方が
// 成立させているので成功として扱う (#3466)。
func TestAcceptRequest_DB_Concurrent(t *testing.T) {
	e := newApprovalEnv(t)
	now := time.Now()
	for round := 0; round < 20; round++ {
		followee := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour), locked: true})
		follower := e.user(userOpts{seenAt: now.Add(-time.Hour)})
		_, err := e.svc.Follow(follower, followee, following.FollowOptions{})
		require.NoError(t, err)

		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				errs[i] = e.svc.AcceptRequest(followee, follower)
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			// 申請を先に消した側の後に来た方は ErrRequestNotFound になりうる
			// (UI では「既に承認済み」)。500 になる一意制約違反は出さない。
			if err != nil {
				assert.ErrorIs(t, err, following.ErrRequestNotFound, "round %d goroutine %d", round, i)
			}
		}
		var n int64
		require.NoError(t, e.db.Model(&model.Following{}).Where(`"followerId" = ? AND "followeeId" = ?`, follower, followee).Count(&n).Error)
		assert.EqualValues(t, 1, n, "round %d", round)
		assert.False(t, e.isRequested(follower, followee))
	}
}
