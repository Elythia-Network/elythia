package repository

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestReversiRepository_DeleteOutdatedGames(t *testing.T) {
	repo := NewReversiRepository(testDB)
	u1 := insertTestUser(t, "u_revd_1", "reversidel1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "u_revd_2", "reversidel2")
	defer cleanupUser(t, u2.ID)

	mk := func(id string, started bool) *model.ReversiGame {
		return &model.ReversiGame{
			ID: id, User1ID: u1.ID, User2ID: u2.ID, IsStarted: started,
			Map: model.StringArray{"--------"}, BW: "random", TimeLimitForEachTurn: 90, Logs: datatypes.JSON("[]"),
		}
	}
	// id は時刻順ソート可能 (aidx)。"a..." < "z..." を閾値として使う。
	require.NoError(t, repo.Create(mk("aaa_old_unstarted", false))) // outdated (id < threshold, 未開始)
	require.NoError(t, repo.Create(mk("aaa_old_started", true)))    // 開始済みは消さない
	require.NoError(t, repo.Create(mk("zzz_new_unstarted", false))) // id >= threshold は消さない
	defer testDB.Exec(`DELETE FROM "reversi_game" WHERE id IN (?,?,?)`, "aaa_old_unstarted", "aaa_old_started", "zzz_new_unstarted")

	n, err := repo.DeleteOutdatedGames("bbb_threshold")
	require.NoError(t, err)
	assert.EqualValues(t, 1, n, "id < threshold かつ未開始のみ削除")
	_, err = repo.FindByID("aaa_old_unstarted")
	assert.Error(t, err, "outdated は削除済み")
	_, err = repo.FindByID("aaa_old_started")
	assert.NoError(t, err, "開始済みは残る")
	_, err = repo.FindByID("zzz_new_unstarted")
	assert.NoError(t, err, "新しい game は残る")
}

// TestReversiRepository_UpdateReadyState_Guards verifies the atomic ready
// update and its pre-start guard semantics (#1626).
func TestReversiRepository_UpdateReadyState_Guards(t *testing.T) {
	repo := NewReversiRepository(testDB)
	u1 := insertTestUser(t, "u_revg_1", "reversigrd1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "u_revg_2", "reversigrd2")
	defer cleanupUser(t, u2.ID)

	game := &model.ReversiGame{
		ID: "rev_grd_1", User1ID: u1.ID, User2ID: u2.ID,
		Map: model.StringArray{"--------"}, BW: "random", TimeLimitForEachTurn: 90, Logs: datatypes.JSON("[]"),
	}
	require.NoError(t, repo.Create(game))
	defer testDB.Exec(`DELETE FROM "reversi_game" WHERE id = ?`, game.ID)

	// 正常系: 自分のカラムだけ更新され、最新行が返る
	got, err := repo.UpdateReadyState(game.ID, true, true)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.User1Ready)
	assert.False(t, got.User2Ready)

	got, err = repo.UpdateReadyState(game.ID, false, true)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.User1Ready, "user2側の更新でuser1Readyが失われてはならない")
	assert.True(t, got.User2Ready)

	// started後のready変更はguardで無視され(nil, nil)が返る
	require.NoError(t, testDB.Exec(`UPDATE "reversi_game" SET "isStarted" = true WHERE id = ?`, game.ID).Error)
	got, err = repo.UpdateReadyState(game.ID, true, false)
	require.NoError(t, err)
	assert.Nil(t, got, "started後のready変更はguardで弾かれる")

	// 存在しない行も(nil, nil)
	got, err = repo.UpdateReadyState("rev_grd_ghost", true, true)
	require.NoError(t, err)
	assert.Nil(t, got)
}

// TestReversiRepository_UpdateReadyState_ConcurrentNoLostUpdate is the
// regression test for #1626: concurrent per-player ready updates must not
// clobber each other. full-row Saveに退行するとここが高確率で落ちる。
func TestReversiRepository_UpdateReadyState_ConcurrentNoLostUpdate(t *testing.T) {
	repo := NewReversiRepository(testDB)
	u1 := insertTestUser(t, "u_revc_1", "reversicon1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "u_revc_2", "reversicon2")
	defer cleanupUser(t, u2.ID)

	for i := 0; i < 20; i++ {
		gameID := fmt.Sprintf("rev_con_%02d", i)
		game := &model.ReversiGame{
			ID: gameID, User1ID: u1.ID, User2ID: u2.ID,
			Map: model.StringArray{"--------"}, BW: "random", TimeLimitForEachTurn: 90, Logs: datatypes.JSON("[]"),
		}
		require.NoError(t, repo.Create(game))

		var wg sync.WaitGroup
		wg.Add(2)
		errs := make([]error, 2)
		go func() {
			defer wg.Done()
			_, errs[0] = repo.UpdateReadyState(gameID, true, true)
		}()
		go func() {
			defer wg.Done()
			_, errs[1] = repo.UpdateReadyState(gameID, false, true)
		}()
		wg.Wait()
		require.NoError(t, errs[0])
		require.NoError(t, errs[1])

		got, err := repo.FindByID(gameID)
		require.NoError(t, err)
		assert.True(t, got.User1Ready, "iteration %d: user1Ready must survive concurrent update", i)
		assert.True(t, got.User2Ready, "iteration %d: user2Ready must survive concurrent update", i)
		require.NoError(t, repo.Delete(gameID))
	}
}

// TestReversiRepository_MarkStarted_SingleWinner verifies the started claim is
// won by exactly one concurrent caller (#1626).
func TestReversiRepository_MarkStarted_SingleWinner(t *testing.T) {
	repo := NewReversiRepository(testDB)
	u1 := insertTestUser(t, "u_revm_1", "reversimrk1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "u_revm_2", "reversimrk2")
	defer cleanupUser(t, u2.ID)

	for i := 0; i < 10; i++ {
		gameID := fmt.Sprintf("rev_mrk_%02d", i)
		game := &model.ReversiGame{
			ID: gameID, User1ID: u1.ID, User2ID: u2.ID,
			Map: model.StringArray{"--------"}, BW: "random", TimeLimitForEachTurn: 90, Logs: datatypes.JSON("[]"),
		}
		require.NoError(t, repo.Create(game))

		black := 1
		now := time.Now()
		crc := "12345"
		claim := &model.ReversiGame{ID: gameID, Black: &black, StartedAt: &now, CRC32: &crc}

		var wg sync.WaitGroup
		wg.Add(2)
		wins := make([]bool, 2)
		errs := make([]error, 2)
		for j := 0; j < 2; j++ {
			go func(j int) {
				defer wg.Done()
				wins[j], errs[j] = repo.MarkStarted(claim)
			}(j)
		}
		wg.Wait()
		require.NoError(t, errs[0])
		require.NoError(t, errs[1])
		winners := 0
		for _, w := range wins {
			if w {
				winners++
			}
		}
		assert.Equal(t, 1, winners, "iteration %d: exactly one caller must win the start claim", i)

		got, err := repo.FindByID(gameID)
		require.NoError(t, err)
		assert.True(t, got.IsStarted)
		require.NotNil(t, got.Black)
		assert.Equal(t, 1, *got.Black)
		require.NotNil(t, got.CRC32)
		assert.Equal(t, "12345", *got.CRC32)
		assert.NotNil(t, got.StartedAt)
		require.NoError(t, repo.Delete(gameID))
	}
}

func TestReversiRepository_CRUD(t *testing.T) {
	repo := NewReversiRepository(testDB)
	u1 := insertTestUser(t, "u_rev_1", "reversiuser1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "u_rev_2", "reversiuser2")
	defer cleanupUser(t, u2.ID)

	game := &model.ReversiGame{
		ID:                   "rev_g1",
		User1ID:              u1.ID,
		User2ID:              u2.ID,
		Map:                  model.StringArray{"--------", "--------", "--------", "---wb---", "---bw---", "--------", "--------", "--------"},
		BW:                   "random",
		TimeLimitForEachTurn: 90,
		Logs:                 datatypes.JSON("[]"),
	}
	require.NoError(t, repo.Create(game))
	defer testDB.Exec(`DELETE FROM "reversi_game" WHERE id = ?`, game.ID)

	// FindByID
	got, err := repo.FindByID(game.ID)
	require.NoError(t, err)
	assert.Equal(t, u1.ID, got.User1ID)

	// FindByID not found
	_, err = repo.FindByID("ghost")
	assert.Error(t, err)

	// Update + ListByUser
	got.BW = "1"
	require.NoError(t, repo.Update(got))
	list, err := repo.ListByUser(u1.ID, 10)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(list), 1)

	// ListActive
	active, err := repo.ListActive()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(active), 1)

	// Delete
	require.NoError(t, repo.Delete(game.ID))
	_, err = repo.FindByID(game.ID)
	assert.Error(t, err)
}

// **招待の重複排除を一覧の窓で代用しない。** 以前は `ListByUser(invitee, 20)`
// の中を線形に探していたので、**別の相手からの招待を 20 件流し込むだけで
// 重複排除が破れ**、同じ相手からの再送が新しい行を作れるようになっていた
// (1 ペアあたりの上限が消える)。条件を SQL に渡せば窓に依存しない。
func TestReversiRepository_FindPendingInvitation(t *testing.T) {
	repo := NewReversiRepository(testDB)
	invitee := insertTestUser(t, "u_revfpi_0", "revfpi0")
	defer cleanupUser(t, invitee.ID)
	inviter := insertTestUser(t, "u_revfpi_1", "revfpi1")
	defer cleanupUser(t, inviter.ID)

	var ids []string
	mk := func(id, u1, u2 string, started, ended bool) {
		g := &model.ReversiGame{
			ID: id, User1ID: u1, User2ID: u2, IsStarted: started, IsEnded: ended,
			Map: model.StringArray{"--------"}, BW: "random", TimeLimitForEachTurn: 90,
			Logs: datatypes.JSON("[]"),
		}
		require.NoError(t, repo.Create(g))
		ids = append(ids, id)
	}
	defer func() {
		for _, id := range ids {
			testDB.Exec(`DELETE FROM "reversi_game" WHERE id = ?`, id)
		}
	}()

	t.Run("招待が無ければ nil", func(t *testing.T) {
		got, err := repo.FindPendingInvitation(invitee.ID, inviter.ID)
		require.NoError(t, err)
		assert.Nil(t, got)
	})

	mk("revfpi_a_pending", inviter.ID, invitee.ID, false, false)

	t.Run("pending な招待を返す", func(t *testing.T) {
		got, err := repo.FindPendingInvitation(invitee.ID, inviter.ID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "revfpi_a_pending", got.ID)
	})

	t.Run("向きが逆の行は拾わない", func(t *testing.T) {
		got, err := repo.FindPendingInvitation(inviter.ID, invitee.ID)
		require.NoError(t, err)
		assert.Nil(t, got, "inviter と invitee を入れ替えた行を招待として扱っている")
	})

	// **窓に依存しないこと。** 無関係な相手からの招待を 30 件積む。一覧の先頭
	// 20 件を走査する実装だと、ここで元の招待が押し出されて nil になる。
	others := make([]*model.User, 0, 3)
	for i := 0; i < 3; i++ {
		u := insertTestUser(t, fmt.Sprintf("u_revfpi_o%d", i), fmt.Sprintf("revfpio%d", i))
		others = append(others, u)
		defer cleanupUser(t, u.ID)
	}
	for i := 0; i < 30; i++ {
		mk(fmt.Sprintf("revfpi_z_flood_%02d", i), others[i%len(others)].ID, invitee.ID, false, false)
	}

	t.Run("他からの招待を積まれても見失わない", func(t *testing.T) {
		got, err := repo.FindPendingInvitation(invitee.ID, inviter.ID)
		require.NoError(t, err)
		require.NotNil(t, got, "flood で重複排除が破れている (再送が新しい行を作れる)")
		assert.Equal(t, "revfpi_a_pending", got.ID)
	})

	mk("revfpi_b_started", inviter.ID, invitee.ID, true, false)
	mk("revfpi_c_ended", inviter.ID, invitee.ID, false, true)

	t.Run("開始済み / 終了済みは招待ではない", func(t *testing.T) {
		got, err := repo.FindPendingInvitation(invitee.ID, inviter.ID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "revfpi_a_pending", got.ID,
			"開始済み / 終了済みの行を pending な招待として返している")
	})
}
