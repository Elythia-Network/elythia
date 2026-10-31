package repository

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// establishFixture is a follower / followee pair for CreateEstablished.
type establishFixture struct {
	t        *testing.T
	follower string
	followee string
}

func newEstablishFixture(t *testing.T, prefix string) *establishFixture {
	t.Helper()
	fx := &establishFixture{t: t, follower: prefix + "_er", followee: prefix + "_ee"}
	insertTestUser(t, fx.follower, prefix+"er")
	insertTestUser(t, fx.followee, prefix+"ee")
	t.Cleanup(func() {
		testDB.Exec(`DELETE FROM "following" WHERE "followerId" = ?`, fx.follower)
		testDB.Exec(`DELETE FROM "follow_request" WHERE "followerId" = ?`, fx.follower)
		cleanupUser(t, fx.follower)
		cleanupUser(t, fx.followee)
	})
	return fx
}

// newEstablisher returns the repository from NewFollowingRepository as a
// FollowingEstablisher, failing the test when it does not implement it.
func newEstablisher(t *testing.T) FollowingEstablisher {
	t.Helper()
	// following.Service が型アサーションで使う形と同じ。
	est, ok := NewFollowingRepository(testDB).(FollowingEstablisher)
	require.True(t, ok, "NewFollowingRepository must implement FollowingEstablisher")
	return est
}

func (fx *establishFixture) following(id string) *model.Following {
	return &model.Following{ID: id, FollowerID: fx.follower, FolloweeID: fx.followee}
}

func (fx *establishFixture) request(id string) {
	fx.t.Helper()
	require.NoError(fx.t, testDB.Create(&model.FollowRequest{ID: id, FollowerID: fx.follower, FolloweeID: fx.followee}).Error)
}

// state returns the following rows, follow request rows, the follower's
// followingCount and the followee's followersCount of the pair.
func (fx *establishFixture) state() (followings, requests int64, followingCount, followersCount int) {
	fx.t.Helper()
	require.NoError(fx.t, testDB.Model(&model.Following{}).Where(`"followerId" = ? AND "followeeId" = ?`, fx.follower, fx.followee).Count(&followings).Error)
	require.NoError(fx.t, testDB.Model(&model.FollowRequest{}).Where(`"followerId" = ? AND "followeeId" = ?`, fx.follower, fx.followee).Count(&requests).Error)
	var er, ee model.User
	require.NoError(fx.t, testDB.First(&er, "id = ?", fx.follower).Error)
	require.NoError(fx.t, testDB.First(&ee, "id = ?", fx.followee).Error)
	return followings, requests, er.FollowingCount, ee.FollowersCount
}

// failOn makes the given statement on table fail for rows whose column is
// value, until the test ends.
func failOn(t *testing.T, name, timing, table, row, column, value string) {
	t.Helper()
	require.NoError(t, testDB.Exec(`CREATE OR REPLACE FUNCTION `+name+`() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF `+row+`."`+column+`" = '`+value+`' THEN RAISE EXCEPTION 'injected failure'; END IF; RETURN `+row+`; END $$`).Error)
	t.Cleanup(func() {
		require.NoError(t, testDB.Exec(`DROP FUNCTION `+name+`() CASCADE`).Error)
	})
	require.NoError(t, testDB.Exec(`CREATE TRIGGER `+name+` BEFORE `+timing+` ON "`+table+`" FOR EACH ROW EXECUTE FUNCTION `+name+`()`).Error)
}

func TestCreateEstablishedDeletesRequestAndCounts(t *testing.T) {
	fx := newEstablishFixture(t, "est_ok")
	fx.request("est_ok_req")
	deleted, err := newEstablisher(t).CreateEstablished(fx.following("est_ok_f"), EstablishFollowingOpts{IncrementCounts: true})
	require.NoError(t, err)
	assert.True(t, deleted)
	followings, requests, following, followers := fx.state()
	assert.EqualValues(t, 1, followings)
	assert.EqualValues(t, 0, requests)
	assert.Equal(t, 1, following)
	assert.Equal(t, 1, followers)
}

func TestCreateEstablishedWithoutRequestOrCounts(t *testing.T) {
	fx := newEstablishFixture(t, "est_plain")
	deleted, err := newEstablisher(t).CreateEstablished(fx.following("est_plain_f"), EstablishFollowingOpts{})
	require.NoError(t, err)
	assert.False(t, deleted, "there was no request to delete")
	followings, _, following, followers := fx.state()
	assert.EqualValues(t, 1, followings)
	assert.Equal(t, 0, following, "counts are left to the caller when IncrementCounts is off (moved accounts)")
	assert.Equal(t, 0, followers)
}

func TestCreateEstablishedRequireRequestWritesNothingWithoutRequest(t *testing.T) {
	fx := newEstablishFixture(t, "est_req")
	_, err := newEstablisher(t).CreateEstablished(fx.following("est_req_f"), EstablishFollowingOpts{IncrementCounts: true, RequireRequest: true})
	require.ErrorIs(t, err, ErrNotFound)
	followings, _, following, followers := fx.state()
	assert.EqualValues(t, 0, followings)
	assert.Equal(t, 0, following)
	assert.Equal(t, 0, followers)
}

// 途中のどの書き込みが失敗しても、何も残さない (#3911)。
func TestCreateEstablishedRollsBackOnFailure(t *testing.T) {
	cases := []struct {
		name string
		fail func(t *testing.T, fx *establishFixture)
	}{
		{"request delete fails", func(t *testing.T, fx *establishFixture) {
			failOn(t, "est_fail_request", "DELETE", "follow_request", "OLD", "followerId", fx.follower)
		}},
		{"following count fails", func(t *testing.T, fx *establishFixture) {
			failOn(t, "est_fail_following_count", "UPDATE", "user", "NEW", "id", fx.follower)
		}},
		{"followers count fails", func(t *testing.T, fx *establishFixture) {
			failOn(t, "est_fail_followers_count", "UPDATE", "user", "NEW", "id", fx.followee)
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prefix := []string{"est_rb_a", "est_rb_b", "est_rb_c"}[i]
			fx := newEstablishFixture(t, prefix)
			fx.request(prefix + "_req")
			tc.fail(t, fx)
			_, err := newEstablisher(t).CreateEstablished(fx.following(prefix+"_f"), EstablishFollowingOpts{IncrementCounts: true})
			require.Error(t, err)
			followings, requests, following, followers := fx.state()
			assert.EqualValues(t, 0, followings, "the following row is rolled back")
			assert.EqualValues(t, 1, requests, "the request is kept")
			assert.Equal(t, 0, following)
			assert.Equal(t, 0, followers)
		})
	}
}

// 申請が無くフォローが成立済みなら、承認は ErrFollowingAlreadyExists で何も書かない。
func TestCreateEstablishedRequireRequestReportsExistingFollowing(t *testing.T) {
	fx := newEstablishFixture(t, "est_exist")
	insertFollowing(t, "est_exist_old", fx.follower, fx.followee)
	_, err := newEstablisher(t).CreateEstablished(fx.following("est_exist_f"), EstablishFollowingOpts{IncrementCounts: true, RequireRequest: true})
	require.ErrorIs(t, err, ErrFollowingAlreadyExists)
	followings, _, following, followers := fx.state()
	assert.EqualValues(t, 1, followings)
	assert.Equal(t, 0, following)
	assert.Equal(t, 0, followers)
}

func TestCreateEstablishedUniqueViolationKeepsRequest(t *testing.T) {
	fx := newEstablishFixture(t, "est_dup")
	insertFollowing(t, "est_dup_old", fx.follower, fx.followee)
	fx.request("est_dup_req")
	_, err := newEstablisher(t).CreateEstablished(fx.following("est_dup_f"), EstablishFollowingOpts{IncrementCounts: true})
	require.True(t, IsUniqueViolation(err), "got %v", err)
	followings, requests, following, followers := fx.state()
	assert.EqualValues(t, 1, followings)
	assert.EqualValues(t, 1, requests, "the request delete is rolled back with the insert")
	assert.Equal(t, 0, following)
	assert.Equal(t, 0, followers)
}

// A→B と B→A を同時に成立させても、デッドロックにならない (#3911)。
//
// user の行を書いた順にロックすると、A→B は A→B の順、B→A は B→A の順で
// 取るので互いを待つ。重なりは決定的に作る: id の小さい方の行を別の
// トランザクションで握り、両方がその行で待ちに入ってから放す。順序を
// 揃えていなければ、先に握った方が残りの行で相手を待ち、およそ半分の回で
// 40P01 になる。
func TestCreateEstablishedMutualFollowDoesNotDeadlock(t *testing.T) {
	for round := range 5 {
		prefix := "est_mut" + string(rune('a'+round))
		a, b := prefix+"_a", prefix+"_b"
		insertTestUser(t, a, prefix+"a")
		insertTestUser(t, b, prefix+"b")
		t.Cleanup(func() {
			testDB.Exec(`DELETE FROM "following" WHERE "followerId" IN (?, ?)`, a, b)
			cleanupUser(t, a)
			cleanupUser(t, b)
		})
		low := min(a, b)

		holder := testDB.Begin()
		require.NoError(t, holder.Error)
		// 途中で落ちても行を握ったまま残さない (待っている側と後片付けが止まる)。
		defer holder.Rollback()
		var held int64
		require.NoError(t, holder.Raw(`SELECT count(*) FROM (SELECT 1 FROM "user" WHERE id = ? FOR UPDATE) u`, low).Scan(&held).Error)
		require.EqualValues(t, 1, held)
		var xid string
		require.NoError(t, holder.Raw(`SELECT xid(pg_current_xact_id())::text`).Scan(&xid).Error)

		errs := make(chan error, 2)
		var wg sync.WaitGroup
		est := newEstablisher(t)
		for i, pair := range [][2]string{{a, b}, {b, a}} {
			wg.Go(func() {
				_, err := est.CreateEstablished(&model.Following{ID: fmt.Sprintf("%s_f%d", prefix, i), FollowerID: pair[0], FolloweeID: pair[1]}, EstablishFollowingOpts{IncrementCounts: true})
				errs <- err
			})
		}
		require.Eventually(t, func() bool {
			var waiting int64
			if err := testDB.Raw(`SELECT count(DISTINCT pid) FROM pg_locks WHERE NOT granted AND (
	(locktype = 'transactionid' AND transactionid::text = ?) OR
	(locktype = 'tuple' AND relation = '"user"'::regclass))`, xid).Scan(&waiting).Error; err != nil {
				return false
			}
			return waiting == 2
		}, 10*time.Second, 10*time.Millisecond, "both must wait on the held user row")
		require.NoError(t, holder.Rollback().Error)
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err, "round %d", round)
		}
		var ua, ub model.User
		require.NoError(t, testDB.First(&ua, "id = ?", a).Error)
		require.NoError(t, testDB.First(&ub, "id = ?", b).Error)
		assert.Equal(t, [2]int{1, 1}, [2]int{ua.FollowingCount, ua.FollowersCount})
		assert.Equal(t, [2]int{1, 1}, [2]int{ub.FollowingCount, ub.FollowersCount})
	}
}

func TestDeleteEstablishedDeletesAndDecrements(t *testing.T) {
	fx := newEstablishFixture(t, "del_ok")
	f := fx.following("del_ok_f")
	_, err := newEstablisher(t).CreateEstablished(f, EstablishFollowingOpts{IncrementCounts: true})
	require.NoError(t, err)
	require.NoError(t, newEstablisher(t).DeleteEstablished(f, true))
	followings, _, following, followers := fx.state()
	assert.EqualValues(t, 0, followings)
	assert.Equal(t, 0, following)
	assert.Equal(t, 0, followers)
}

func TestDeleteEstablishedWithoutCounts(t *testing.T) {
	fx := newEstablishFixture(t, "del_plain")
	f := fx.following("del_plain_f")
	_, err := newEstablisher(t).CreateEstablished(f, EstablishFollowingOpts{IncrementCounts: true})
	require.NoError(t, err)
	require.NoError(t, newEstablisher(t).DeleteEstablished(f, false))
	followings, _, following, followers := fx.state()
	assert.EqualValues(t, 0, followings)
	assert.Equal(t, 1, following, "counts are left to the caller when decrementCounts is off (moved accounts)")
	assert.Equal(t, 1, followers)
}

// 消す行が無ければ、カウントを減らさずに ErrNotFound を返す。同じ組の解除が
// 同時に走ったとき、後の方がカウントを二重に減らさないため (#3369)。
func TestDeleteEstablishedGoneRowWritesNothing(t *testing.T) {
	fx := newEstablishFixture(t, "del_gone")
	f := fx.following("del_gone_f")
	_, err := newEstablisher(t).CreateEstablished(f, EstablishFollowingOpts{IncrementCounts: true})
	require.NoError(t, err)
	require.NoError(t, newEstablisher(t).DeleteEstablished(f, true))
	require.ErrorIs(t, newEstablisher(t).DeleteEstablished(f, true), ErrNotFound)
	_, _, following, followers := fx.state()
	assert.Equal(t, 0, following)
	assert.Equal(t, 0, followers)
}

// カウントの減算が失敗しても、行を消したままにしない (#3369)。
func TestDeleteEstablishedRollsBackOnFailure(t *testing.T) {
	for i, side := range []string{"follower", "followee"} {
		t.Run(side+" count fails", func(t *testing.T) {
			prefix := []string{"del_rb_a", "del_rb_b"}[i]
			fx := newEstablishFixture(t, prefix)
			f := fx.following(prefix + "_f")
			_, err := newEstablisher(t).CreateEstablished(f, EstablishFollowingOpts{IncrementCounts: true})
			require.NoError(t, err)
			failed := fx.follower
			if side == "followee" {
				failed = fx.followee
			}
			failOn(t, prefix+"_trg", "UPDATE", "user", "NEW", "id", failed)
			require.Error(t, newEstablisher(t).DeleteEstablished(f, true))
			followings, _, following, followers := fx.state()
			assert.EqualValues(t, 1, followings, "the following row is rolled back")
			assert.Equal(t, 1, following)
			assert.Equal(t, 1, followers)
		})
	}
}

// A→B と B→A を同時に解除しても、デッドロックにならない (#3369)。重なりの
// 作り方は TestCreateEstablishedMutualFollowDoesNotDeadlock と同じ。
func TestDeleteEstablishedMutualUnfollowDoesNotDeadlock(t *testing.T) {
	for round := range 5 {
		prefix := "del_mut" + string(rune('a'+round))
		a, b := prefix+"_a", prefix+"_b"
		insertTestUser(t, a, prefix+"a")
		insertTestUser(t, b, prefix+"b")
		t.Cleanup(func() {
			testDB.Exec(`DELETE FROM "following" WHERE "followerId" IN (?, ?)`, a, b)
			cleanupUser(t, a)
			cleanupUser(t, b)
		})
		est := newEstablisher(t)
		fs := []*model.Following{
			{ID: prefix + "_f0", FollowerID: a, FolloweeID: b},
			{ID: prefix + "_f1", FollowerID: b, FolloweeID: a},
		}
		for _, f := range fs {
			_, err := est.CreateEstablished(f, EstablishFollowingOpts{IncrementCounts: true})
			require.NoError(t, err)
		}

		holder := testDB.Begin()
		require.NoError(t, holder.Error)
		// 途中で落ちても行を握ったまま残さない (待っている側と後片付けが止まる)。
		defer holder.Rollback()
		var held int64
		require.NoError(t, holder.Raw(`SELECT count(*) FROM (SELECT 1 FROM "user" WHERE id = ? FOR UPDATE) u`, min(a, b)).Scan(&held).Error)
		require.EqualValues(t, 1, held)
		var xid string
		require.NoError(t, holder.Raw(`SELECT xid(pg_current_xact_id())::text`).Scan(&xid).Error)

		errs := make(chan error, 2)
		var wg sync.WaitGroup
		for _, f := range fs {
			wg.Go(func() { errs <- est.DeleteEstablished(f, true) })
		}
		require.Eventually(t, func() bool {
			var waiting int64
			if err := testDB.Raw(`SELECT count(DISTINCT pid) FROM pg_locks WHERE NOT granted AND (
	(locktype = 'transactionid' AND transactionid::text = ?) OR
	(locktype = 'tuple' AND relation = '"user"'::regclass))`, xid).Scan(&waiting).Error; err != nil {
				return false
			}
			return waiting == 2
		}, 10*time.Second, 10*time.Millisecond, "both must wait on the held user row")
		require.NoError(t, holder.Rollback().Error)
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err, "round %d", round)
		}
		var ua, ub model.User
		require.NoError(t, testDB.First(&ua, "id = ?", a).Error)
		require.NoError(t, testDB.First(&ub, "id = ?", b).Error)
		assert.Equal(t, [2]int{0, 0}, [2]int{ua.FollowingCount, ua.FollowersCount})
		assert.Equal(t, [2]int{0, 0}, [2]int{ub.FollowingCount, ub.FollowersCount})
	}
}

// 作成・削除のどちらも、コミットした後に follower のフォローの集合のキャッシュを
// 無効化する。無効化しないと、解除した相手のフォロワー限定の投稿が TTL の間
// 見え続ける (#3369)。
func TestEstablishInvalidatesRelationCache(t *testing.T) {
	fx := newEstablishFixture(t, "est_rc")
	rc := NewRelationCache(testDB)
	est, ok := NewFollowingRepositoryWithRelationCache(testDB, rc).(FollowingEstablisher)
	require.True(t, ok)
	followees := func() []string {
		t.Helper()
		ids, err := rc.FolloweeIDs(fx.follower)
		require.NoError(t, err)
		return ids
	}
	require.Empty(t, followees(), "warm the cache with no followees")

	f := fx.following("est_rc_f")
	_, err := est.CreateEstablished(f, EstablishFollowingOpts{IncrementCounts: true})
	require.NoError(t, err)
	assert.Equal(t, []string{fx.followee}, followees(), "a follow is visible right after CreateEstablished")

	require.NoError(t, est.DeleteEstablished(f, true))
	assert.Empty(t, followees(), "an unfollow is visible right after DeleteEstablished")
}
