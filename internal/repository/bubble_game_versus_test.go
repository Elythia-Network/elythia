package repository

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func bvBase(id, u1, u2 string, startedAt time.Time) BubbleVersusMatchBase {
	return BubbleVersusMatchBase{ID: id, User1ID: u1, User2ID: u2, GameMode: "normal", Seed: "seed_" + id, StartedAt: startedAt}
}

func bvCleanup(t *testing.T, ids ...string) {
	t.Helper()
	for _, id := range ids {
		testDB.Exec(`DELETE FROM "bubble_game_versus_record" WHERE "id" = ?`, id)
	}
}

func TestBubbleVersusRepository_ReportsAndFinishMergeInAnyOrder(t *testing.T) {
	repo := NewBubbleVersusRepository(testDB)
	u1 := insertTestUser(t, "u_bvm_1", "bvmerge1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "u_bvm_2", "bvmerge2")
	defer cleanupUser(t, u2.ID)
	now := time.Now().UTC().Truncate(time.Millisecond)
	ver := 4

	// 終局の確定が先、報告が後。
	base := bvBase("bvm_a", u1.ID, u2.ID, now)
	defer bvCleanup(t, base.ID)
	winner := u2.ID
	require.NoError(t, repo.Finish(base, now.Add(time.Minute), &winner, "gameOver"))
	require.NoError(t, repo.SaveReport(base, 0, BubbleVersusReport{Score: 10, Frame: 100, Reason: "gameOver", GameVersion: &ver, Logs: datatypes.JSON(`[[1,0,5]]`)}))
	require.NoError(t, repo.SaveReport(base, 1, BubbleVersusReport{Score: 20, Frame: 120, Reason: "opponentEnded", Logs: datatypes.JSON(`[[2,0,6]]`)}))

	rec, err := repo.FindByID(base.ID)
	require.NoError(t, err)
	require.NotNil(t, rec.EndedAt, "報告が終局の列を消さない")
	assert.Equal(t, u2.ID, *rec.WinnerID)
	assert.Equal(t, "gameOver", *rec.Reason)
	require.NotNil(t, rec.User1Score)
	assert.Equal(t, 10, *rec.User1Score)
	assert.Equal(t, 4, *rec.User1GameVersion)
	assert.JSONEq(t, `[[1,0,5]]`, string(rec.User1Logs))
	require.NotNil(t, rec.User2Score, "2 人目の報告が 1 人目の列を消さない")
	assert.Equal(t, 20, *rec.User2Score)
	assert.Equal(t, "opponentEnded", *rec.User2Reason)
	assert.Nil(t, rec.User2GameVersion, "版の無い報告は NULL のまま")
	assert.JSONEq(t, `[[2,0,6]]`, string(rec.User2Logs))
	require.NotNil(t, rec.User1, "参加者を preload する")
	assert.Equal(t, u1.ID, rec.User1.ID)

	// 報告が先、終局の確定が後。報告を確定が消さない。
	base2 := bvBase("bvm_b", u1.ID, u2.ID, now)
	defer bvCleanup(t, base2.ID)
	require.NoError(t, repo.SaveReport(base2, 1, BubbleVersusReport{Score: 5, Frame: 50, Reason: "timeUp", Logs: datatypes.JSON(`[]`)}))
	rec2, err := repo.FindByID(base2.ID)
	require.NoError(t, err)
	assert.Nil(t, rec2.EndedAt, "報告だけでは終局しない")
	require.NoError(t, repo.Finish(base2, now.Add(time.Minute), nil, "timeUp"))
	rec2, err = repo.FindByID(base2.ID)
	require.NoError(t, err)
	require.NotNil(t, rec2.User2Score, "終局の確定が報告の列を消さない")
	assert.Equal(t, 5, *rec2.User2Score)
	assert.Nil(t, rec2.WinnerID, "引き分けは winnerId が NULL")
	assert.Nil(t, rec2.User1Score, "届いていない側は NULL")

	// 同じ側の報告を送り直したら上書きする。
	require.NoError(t, repo.SaveReport(base2, 1, BubbleVersusReport{Score: 7, Frame: 70, Reason: "timeUp", Logs: datatypes.JSON(`[]`)}))
	rec2, err = repo.FindByID(base2.ID)
	require.NoError(t, err)
	assert.Equal(t, 7, *rec2.User2Score)

	assert.Error(t, repo.SaveReport(base2, 2, BubbleVersusReport{}), "slot は 0 か 1 だけ")
}

func TestBubbleVersusRepository_ListByUser(t *testing.T) {
	repo := NewBubbleVersusRepository(testDB)
	u1 := insertTestUser(t, "u_bvl_1", "bvlist1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "u_bvl_2", "bvlist2")
	defer cleanupUser(t, u2.ID)
	u3 := insertTestUser(t, "u_bvl_3", "bvlist3")
	defer cleanupUser(t, u3.ID)
	now := time.Now().UTC()

	ended := func(id, a, b string) BubbleVersusMatchBase {
		base := bvBase(id, a, b, now)
		require.NoError(t, repo.Finish(base, now, nil, "timeUp"))
		return base
	}
	defer bvCleanup(t, "bvl_1", "bvl_2", "bvl_3", "bvl_4", "bvl_5")
	ended("bvl_1", u1.ID, u2.ID)                                                                     // u1 が招待
	ended("bvl_2", u2.ID, u1.ID)                                                                     // u1 が招待された
	ended("bvl_3", u2.ID, u3.ID)                                                                     // u1 は無関係
	require.NoError(t, repo.SaveReport(bvBase("bvl_4", u1.ID, u2.ID, now), 0, BubbleVersusReport{})) // 未終局
	ended("bvl_5", u1.ID, u3.ID)

	ids := func(recs []*model.BubbleGameVersusRecord) []string {
		out := make([]string, 0, len(recs))
		for _, r := range recs {
			out = append(out, r.ID)
		}
		return out
	}

	all, err := repo.ListByUser(u1.ID, false, "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"bvl_5", "bvl_2", "bvl_1"}, ids(all), "両側の参加を拾い、未終局と無関係を除き、id の降順")

	// 一覧は記録とシードを読まない (詳細の FindByID では読む)。
	require.NoError(t, repo.SaveReport(bvBase("bvl_1", u1.ID, u2.ID, now), 0, BubbleVersusReport{Score: 1, Logs: datatypes.JSON(`[[1,0,5]]`)}))
	all, err = repo.ListByUser(u1.ID, false, "", 10)
	require.NoError(t, err)
	for _, r := range all {
		assert.Empty(t, r.User1Logs, r.ID)
		assert.Empty(t, r.User2Logs, r.ID)
		assert.Empty(t, r.Seed, r.ID)
	}
	full, err := repo.FindByID("bvl_1")
	require.NoError(t, err)
	assert.JSONEq(t, `[[1,0,5]]`, string(full.User1Logs))
	assert.NotEmpty(t, full.Seed)

	page, err := repo.ListByUser(u1.ID, false, "bvl_5", 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"bvl_2"}, ids(page), "untilId と limit")

	// 公開は両者がそろったときだけ。
	_, err = repo.SetPublic("bvl_1", u1.ID, true)
	require.NoError(t, err)
	_, err = repo.SetPublic("bvl_2", u1.ID, true)
	require.NoError(t, err)
	_, err = repo.SetPublic("bvl_2", u2.ID, true)
	require.NoError(t, err)
	pub, err := repo.ListByUser(u1.ID, true, "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"bvl_2"}, ids(pub), "片方だけが公開にした対局は出ない")

	// u1 が user1 の対局も、後ろの条件 (終局・公開) で絞られていること。
	// bvl_4 (未終局) と bvl_1 / bvl_5 (非公開) がそれに当たる。
	assert.NotContains(t, ids(pub), "bvl_5")
	assert.NotContains(t, ids(all), "bvl_4")

	none, err := repo.ListByUser("u_bvl_\x00", false, "", 10)
	require.NoError(t, err)
	assert.Empty(t, none, "列に入らない値は引く前に空")
}

func TestBubbleVersusRepository_SetPublic(t *testing.T) {
	repo := NewBubbleVersusRepository(testDB)
	u1 := insertTestUser(t, "u_bvp_1", "bvpub1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "u_bvp_2", "bvpub2")
	defer cleanupUser(t, u2.ID)
	u3 := insertTestUser(t, "u_bvp_3", "bvpub3")
	defer cleanupUser(t, u3.ID)
	base := bvBase("bvp_1", u1.ID, u2.ID, time.Now())
	defer bvCleanup(t, base.ID)
	require.NoError(t, repo.Finish(base, time.Now(), nil, "timeUp"))

	ok, err := repo.SetPublic(base.ID, u2.ID, true)
	require.NoError(t, err)
	assert.True(t, ok)
	rec, err := repo.FindByID(base.ID)
	require.NoError(t, err)
	assert.False(t, rec.User1Public, "相手の側は変えない")
	assert.True(t, rec.User2Public, "自分の側を変える")

	ok, err = repo.SetPublic(base.ID, u3.ID, true)
	require.NoError(t, err)
	assert.False(t, ok, "参加していない人は変えられない")
	rec, err = repo.FindByID(base.ID)
	require.NoError(t, err)
	assert.False(t, rec.User1Public)

	ok, err = repo.SetPublic("bvp_none", u1.ID, true)
	require.NoError(t, err)
	assert.False(t, ok, "無い対局")
	ok, err = repo.SetPublic("bvp_\x00", u1.ID, true)
	require.NoError(t, err)
	assert.False(t, ok, "列に入らない値")
}

func TestBubbleVersusRepository_FindByID_NotFound(t *testing.T) {
	repo := NewBubbleVersusRepository(testDB)
	_, err := repo.FindByID("bvf_missing")
	assert.True(t, IsNotFound(err))
	_, err = repo.FindByID("bvf_\x00")
	assert.True(t, IsNotFound(err), "列に入らない値は引く前に not found")
}

func TestBubbleVersusRepository_DeleteExpired(t *testing.T) {
	repo := NewBubbleVersusRepository(testDB)
	u1 := insertTestUser(t, "u_bve_1", "bvexp1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "u_bve_2", "bvexp2")
	defer cleanupUser(t, u2.ID)
	now := time.Now().UTC()
	cutoff := now.Add(-30 * 24 * time.Hour)
	defer bvCleanup(t, "bve_old", "bve_new", "bve_stale", "bve_fresh")

	require.NoError(t, repo.Finish(bvBase("bve_old", u1.ID, u2.ID, cutoff.Add(-2*time.Hour)), cutoff.Add(-time.Hour), nil, "timeUp"))
	require.NoError(t, repo.Finish(bvBase("bve_new", u1.ID, u2.ID, cutoff.Add(-2*time.Hour)), cutoff.Add(time.Hour), nil, "timeUp"))
	require.NoError(t, repo.SaveReport(bvBase("bve_stale", u1.ID, u2.ID, cutoff.Add(-time.Hour)), 0, BubbleVersusReport{}))
	require.NoError(t, repo.SaveReport(bvBase("bve_fresh", u1.ID, u2.ID, cutoff.Add(time.Hour)), 0, BubbleVersusReport{}))

	n, err := repo.DeleteExpired(cutoff)
	require.NoError(t, err)
	assert.EqualValues(t, 2, n)
	for id, want := range map[string]bool{"bve_old": false, "bve_new": true, "bve_stale": false, "bve_fresh": true} {
		_, err := repo.FindByID(id)
		assert.Equal(t, want, err == nil, id)
	}
}

func TestBubbleVersusRepository_DeletedUserRemovesMatch(t *testing.T) {
	repo := NewBubbleVersusRepository(testDB)
	u1 := insertTestUser(t, "u_bvd_1", "bvdel1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "u_bvd_2", "bvdel2")
	defer cleanupUser(t, u2.ID)
	base := bvBase("bvd_1", u1.ID, u2.ID, time.Now())
	defer bvCleanup(t, base.ID)
	require.NoError(t, repo.Finish(base, time.Now(), nil, "timeUp"))

	cleanupUser(t, u2.ID)
	_, err := repo.FindByID(base.ID)
	assert.True(t, IsNotFound(err), "どちらかが退会したら対局も消える")
}
