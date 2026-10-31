package following_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

// injectFailure makes the given statement on table fail for rows whose
// column is value. The returned function removes it.
func (e *approvalEnv) injectFailure(name, timing, table, row, column, value string) (remove func()) {
	e.t.Helper()
	// 外す関数を返すのは、外した後の再試行を確かめるため。
	require.NoError(e.t, e.db.Exec(`CREATE OR REPLACE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF `+row+`."`+column+`" = '`+value+`' THEN RAISE EXCEPTION 'injected failure'; END IF; RETURN `+row+`; END $$`).Error)
	removed := false
	remove = func() {
		if removed {
			return
		}
		removed = true
		require.NoError(e.t, e.db.Exec(`DROP FUNCTION `+name+`() CASCADE`).Error)
	}
	e.t.Cleanup(remove)
	require.NoError(e.t, e.db.Exec(`CREATE TRIGGER `+name+` BEFORE `+timing+` ON "`+table+`" FOR EACH ROW EXECUTE FUNCTION `+name+`()`).Error)
	return remove
}

// counts returns followerID's followingCount and followeeID's followersCount.
func (e *approvalEnv) counts(followerID, followeeID string) (following, followers int) {
	e.t.Helper()
	var er, ee model.User
	require.NoError(e.t, e.db.First(&er, "id = ?", followerID).Error)
	require.NoError(e.t, e.db.First(&ee, "id = ?", followeeID).Error)
	return er.FollowingCount, ee.FollowersCount
}

// フォローの行を作った後に申請の削除やカウントが失敗しても、行だけを残さない。
// 再試行で、カウントと通知が 1 回ずつで成立する (#3911)。
func TestFollow_DB_FailureAfterInsertLeavesNothingAndRetrySucceeds(t *testing.T) {
	cases := []struct {
		name string
		// fail は失敗を仕込み、外す関数を返す。
		fail func(e *approvalEnv, follower, followee string) func()
	}{
		{"leftover request delete fails", func(e *approvalEnv, follower, _ string) func() {
			return e.injectFailure("fe_fail_request", "DELETE", "follow_request", "OLD", "followerId", follower)
		}},
		{"followers count fails", func(e *approvalEnv, _, followee string) func() {
			return e.injectFailure("fe_fail_count", "UPDATE", "user", "NEW", "id", followee)
		}},
		{"following count fails", func(e *approvalEnv, follower, _ string) func() {
			return e.injectFailure("fe_fail_following_count", "UPDATE", "user", "NEW", "id", follower)
		}},
		{"insert fails", func(e *approvalEnv, follower, _ string) func() {
			return e.injectFailure("fe_fail_insert", "INSERT", "following", "NEW", "followerId", follower)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newApprovalEnv(t)
			e.hook.follows = nil
			now := time.Now()
			followee := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour)})
			follower := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour)})
			// 期間の設定を外した後のフォローし直しのように、同じ組の申請が残っている。
			require.NoError(t, e.frRepo.Create(&model.FollowRequest{ID: e.idGen.Generate(now), FollowerID: follower, FolloweeID: followee}))
			remove := tc.fail(e, follower, followee)

			_, err := e.svc.Follow(follower, followee, following.FollowOptions{})
			require.Error(t, err)
			assert.False(t, e.isFollowing(follower, followee), "the following row must not be left behind")
			assert.True(t, e.isRequested(follower, followee), "the request is kept")
			fc, frc := e.counts(follower, followee)
			assert.Equal(t, 0, fc)
			assert.Equal(t, 0, frc)
			assert.Empty(t, e.hook.follows)

			remove()
			res, err := e.svc.Follow(follower, followee, following.FollowOptions{})
			require.NoError(t, err, "a retry must not stop at ErrAlreadyFollowing")
			require.NotNil(t, res.Following)
			assert.True(t, e.isFollowing(follower, followee))
			assert.False(t, e.isRequested(follower, followee))
			fc, frc = e.counts(follower, followee)
			assert.Equal(t, 1, fc)
			assert.Equal(t, 1, frc)
			assert.Equal(t, []string{follower + "->" + followee}, e.hook.follows)
		})
	}
}

// 承認で行の作成が失敗しても、申請だけを消さない。承認をやり直せる (#3911)。
func TestAcceptRequest_DB_FailureKeepsRequestAndRetrySucceeds(t *testing.T) {
	e := newApprovalEnv(t)
	now := time.Now()
	followee := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour), locked: true})
	follower := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour)})
	res, err := e.svc.Follow(follower, followee, following.FollowOptions{})
	require.NoError(t, err)
	require.NotNil(t, res.Request)
	remove := e.injectFailure("fe_fail_accept", "INSERT", "following", "NEW", "followerId", follower)

	require.Error(t, e.svc.AcceptRequest(followee, follower))
	assert.True(t, e.isRequested(follower, followee), "the request must survive a failed accept")
	assert.False(t, e.isFollowing(follower, followee))
	assert.Empty(t, e.hook.accepts)

	remove()
	require.NoError(t, e.svc.AcceptRequest(followee, follower))
	assert.False(t, e.isRequested(follower, followee))
	assert.True(t, e.isFollowing(follower, followee))
	fc, frc := e.counts(follower, followee)
	assert.Equal(t, 1, fc)
	assert.Equal(t, 1, frc)
	assert.Equal(t, []string{follower + "->" + followee}, e.hook.accepts)
}

// 確かめた後に申請が取り消されたら、承認しない (ErrRequestNotFound)。
func TestAcceptRequest_DB_RequestGoneAfterLookup(t *testing.T) {
	e := newApprovalEnv(t)
	now := time.Now()
	followee := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour), locked: true})
	follower := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour)})
	_, err := e.svc.Follow(follower, followee, following.FollowOptions{})
	require.NoError(t, err)

	// 探した後、消す前に取り消された形を、申請の削除を空振りさせて作る。
	require.NoError(t, e.db.Exec(`CREATE OR REPLACE FUNCTION fe_skip_request_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF OLD."followerId" = '`+follower+`' THEN RETURN NULL; END IF; RETURN OLD; END $$`).Error)
	t.Cleanup(func() { e.db.Exec(`DROP FUNCTION fe_skip_request_delete() CASCADE`) })
	require.NoError(t, e.db.Exec(`CREATE TRIGGER fe_skip_request_delete BEFORE DELETE ON "follow_request" FOR EACH ROW EXECUTE FUNCTION fe_skip_request_delete()`).Error)

	require.ErrorIs(t, e.svc.AcceptRequest(followee, follower), following.ErrRequestNotFound)
	assert.False(t, e.isFollowing(follower, followee))
	fc, frc := e.counts(follower, followee)
	assert.Equal(t, 0, fc)
	assert.Equal(t, 0, frc)
}

// 同じ申請を同時に承認しても、両方とも成功し、カウントと通知は 1 回だけ。
// 後の方は、申請の削除が先の方の行ロックを待ってから 0 件になる (#3911)。
//
// 重なりは決定的に作る。別のトランザクションで申請の行を握り、2 つの承認が
// 両方ともその行で待ちに入ってから放す。放さずに並べるだけだと、先の方が
// 終わってから後の方が申請を探し、ErrRequestNotFound になる (本家と同じ)。
func TestAcceptRequest_DB_ConcurrentAcceptBothSucceed(t *testing.T) {
	e := newApprovalEnv(t)
	now := time.Now()
	followee := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour), locked: true})
	follower := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour)})
	_, err := e.svc.Follow(follower, followee, following.FollowOptions{})
	require.NoError(t, err)
	e.hook.accepts = nil

	holder := e.db.Begin()
	require.NoError(t, holder.Error)
	defer holder.Rollback()
	var held int64
	require.NoError(t, holder.Raw(`SELECT count(*) FROM (SELECT 1 FROM "follow_request" WHERE "followerId" = ? AND "followeeId" = ? FOR UPDATE) r`, follower, followee).Scan(&held).Error)
	require.EqualValues(t, 1, held)
	var xid string
	require.NoError(t, holder.Raw(`SELECT xid(pg_current_xact_id())::text`).Scan(&xid).Error)

	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { errs <- e.svc.AcceptRequest(followee, follower) })
	}
	// 両方が申請の削除で待つまで待つ。1 つ目は holder のトランザクションを、
	// 2 つ目は 1 つ目が握った行 (tuple) のロックを待つ。
	require.Eventually(t, func() bool {
		var waiting int64
		if err := e.db.Raw(`SELECT count(DISTINCT pid) FROM pg_locks WHERE NOT granted AND (
	(locktype = 'transactionid' AND transactionid::text = ?) OR
	(locktype = 'tuple' AND relation = 'follow_request'::regclass))`, xid).Scan(&waiting).Error; err != nil {
			return false
		}
		return waiting == 2
	}, 10*time.Second, 10*time.Millisecond, "both accepts must wait on the held request")
	require.NoError(t, holder.Rollback().Error)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.True(t, e.isFollowing(follower, followee))
	assert.False(t, e.isRequested(follower, followee))
	fc, frc := e.counts(follower, followee)
	assert.Equal(t, 1, fc)
	assert.Equal(t, 1, frc)
	assert.Equal(t, []string{follower + "->" + followee}, e.hook.accepts, "the accept notification is made once")
}

// カウントはトランザクションの中で直接書くので、利用者のキャッシュを無効化
// しないと古い値を返し続ける (#3911)。
func TestFollow_DB_InvalidatesCachedUserCounts(t *testing.T) {
	e := newApprovalEnv(t)
	cached := repository.NewCachedUserRepository(repository.NewUserRepository(e.db))
	svc := following.NewService(cached, repository.NewFollowingRepository(e.db), e.frRepo, e.idGen, nil, nil, nil, nil)
	now := time.Now()
	followee := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour)})
	follower := e.user(userOpts{seenAt: now.Add(-365 * 24 * time.Hour)})
	// キャッシュに 0 の値を載せる。
	for _, id := range []string{follower, followee} {
		_, err := cached.FindByID(id)
		require.NoError(t, err)
	}

	_, err := svc.Follow(follower, followee, following.FollowOptions{})
	require.NoError(t, err)
	er, err := cached.FindByID(follower)
	require.NoError(t, err)
	ee, err := cached.FindByID(followee)
	require.NoError(t, err)
	assert.Equal(t, 1, er.FollowingCount)
	assert.Equal(t, 1, ee.FollowersCount)
}

// 起動時検査は、トランザクションで書ける following の repository と、カウントを
// 書いた利用者を無効化できる user の repository の両方を求める (#3911)。
func TestHasFollowingEstablisher(t *testing.T) {
	e := newApprovalEnv(t)
	plainUsers := repository.NewUserRepository(e.db)
	cached := repository.NewCachedUserRepository(plainUsers)
	followings := repository.NewFollowingRepository(e.db)
	cases := []struct {
		name       string
		users      repository.UserRepository
		followings repository.FollowingRepository
		want       bool
	}{
		{"production wiring", cached, followings, true},
		{"uncached users cannot be invalidated", plainUsers, followings, false},
		{"a following repository without a transaction", cached, testutil.NewMockFollowingRepository(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := following.NewService(tc.users, tc.followings, e.frRepo, e.idGen, nil, nil, nil, nil)
			assert.Equal(t, tc.want, svc.HasFollowingEstablisher())
		})
	}
}
