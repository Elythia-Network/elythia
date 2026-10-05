package repository

import (
	"context"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanupMuting(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "muting" WHERE id = ?`, id)
}

func cleanupRenoteMuting(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "renote_muting" WHERE id = ?`, id)
}

func TestMutingRepository_CRUDExpiry(t *testing.T) {
	repo := NewMutingRepository(testDB)
	u1 := insertTestUser(t, "u_mt_1", "mt1")
	u2 := insertTestUser(t, "u_mt_2", "mt2")
	u3 := insertTestUser(t, "u_mt_3", "mt3")
	defer cleanupUser(t, u1.ID)
	defer cleanupUser(t, u2.ID)
	defer cleanupUser(t, u3.ID)

	// active mute (no expiry)
	active := &model.Muting{ID: "m_active", MuterID: u1.ID, MuteeID: u2.ID}
	require.NoError(t, repo.Create(active))
	defer cleanupMuting(t, active.ID)

	// expired mute
	past := time.Now().Add(-1 * time.Hour)
	expired := &model.Muting{ID: "m_expired", MuterID: u1.ID, MuteeID: u3.ID, ExpiresAt: &past}
	require.NoError(t, repo.Create(expired))
	defer cleanupMuting(t, expired.ID)

	exists, err := repo.Exists(u1.ID, u2.ID)
	require.NoError(t, err)
	assert.True(t, exists)

	// 期限切れはExistsで除外
	exists, err = repo.Exists(u1.ID, u3.ID)
	require.NoError(t, err)
	assert.False(t, exists)

	found, err := repo.FindByPair(u1.ID, u2.ID)
	require.NoError(t, err)
	assert.Equal(t, "m_active", found.ID)

	rows, err := repo.ListByMuter(u1.ID, "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	require.NoError(t, repo.Delete(active))
	_, err = repo.FindByPair(u1.ID, u2.ID)
	assert.Error(t, err)
}

func TestMutingRepository_DeleteExpired(t *testing.T) {
	repo := NewMutingRepository(testDB)
	u1 := insertTestUser(t, "u_mde_1", "mde1")
	u2 := insertTestUser(t, "u_mde_2", "mde2")
	u3 := insertTestUser(t, "u_mde_3", "mde3")
	defer cleanupUser(t, u1.ID)
	defer cleanupUser(t, u2.ID)
	defer cleanupUser(t, u3.ID)

	active := &model.Muting{ID: "mde_active", MuterID: u1.ID, MuteeID: u2.ID}
	require.NoError(t, repo.Create(active))
	defer cleanupMuting(t, active.ID)
	future := time.Now().Add(time.Hour)
	notYet := &model.Muting{ID: "mde_future", MuterID: u1.ID, MuteeID: u3.ID, ExpiresAt: &future}
	require.NoError(t, repo.Create(notYet))
	defer cleanupMuting(t, notYet.ID)
	past := time.Now().Add(-time.Hour)
	expired := &model.Muting{ID: "mde_expired", MuterID: u2.ID, MuteeID: u3.ID, ExpiresAt: &past}
	require.NoError(t, repo.Create(expired))
	defer cleanupMuting(t, expired.ID)

	muterIDs, err := repo.DeleteExpired(time.Now())
	require.NoError(t, err)
	// 削除した行の muterId が返る。streaming の snapshot を取り直す宛先になるので、
	// 件数だけでなく「誰の」mute が消えたかが要る (#2453)。
	assert.Equal(t, []string{u2.ID}, muterIDs, "期限切れ 1 件だけ削除され、その muterId が返る")
	// active / future は残る、expired は消える。
	_, err = repo.FindByPair(u1.ID, u2.ID)
	assert.NoError(t, err)
	_, err = repo.FindByPair(u2.ID, u3.ID)
	assert.Error(t, err)
}

// 同一 muter の mute が複数同時に失効したとき、行数ぶんの muterId が返ること。
// distinct にすると呼び出し側の prune 件数ログが実際の行数とずれる。
func TestMutingRepository_DeleteExpired_ReturnsOneEntryPerRow(t *testing.T) {
	repo := NewMutingRepository(testDB)
	u1 := insertTestUser(t, "u_mdr_1", "mdr1")
	u2 := insertTestUser(t, "u_mdr_2", "mdr2")
	u3 := insertTestUser(t, "u_mdr_3", "mdr3")
	defer cleanupUser(t, u1.ID)
	defer cleanupUser(t, u2.ID)
	defer cleanupUser(t, u3.ID)

	past := time.Now().Add(-time.Hour)
	a := &model.Muting{ID: "mdr_a", MuterID: u1.ID, MuteeID: u2.ID, ExpiresAt: &past}
	require.NoError(t, repo.Create(a))
	defer cleanupMuting(t, a.ID)
	b := &model.Muting{ID: "mdr_b", MuterID: u1.ID, MuteeID: u3.ID, ExpiresAt: &past}
	require.NoError(t, repo.Create(b))
	defer cleanupMuting(t, b.ID)

	muterIDs, err := repo.DeleteExpired(time.Now())
	require.NoError(t, err)
	assert.Equal(t, []string{u1.ID, u1.ID}, muterIDs)
}

// 期限切れが 1 件も無ければ空を返す (nil でも空でもよいが len 0 であること)。
func TestMutingRepository_DeleteExpired_NoRows(t *testing.T) {
	repo := NewMutingRepository(testDB)
	muterIDs, err := repo.DeleteExpired(time.Now().Add(-100 * 365 * 24 * time.Hour))
	require.NoError(t, err)
	assert.Empty(t, muterIDs)
}

func TestMutingRepository_QueryErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewMutingRepository(testDB.WithContext(ctx))

	err := repo.Create(&model.Muting{ID: "x", MuterID: "a", MuteeID: "b"})
	assert.Error(t, err)
	_, err = repo.FindByPair("a", "b")
	assert.Error(t, err)
	_, err = repo.Exists("a", "b")
	assert.Error(t, err)
	_, err = repo.ListByMuter("a", "", "", 10, 0)
	assert.Error(t, err)
	_, err = repo.ListMuteeIDs("a")
	assert.Error(t, err)
	_, err = repo.ListAllMuteeIDs("a")
	assert.Error(t, err)
	// DeleteExpired の失敗は nil slice + error で返す。ここで空 slice を返すと
	// 呼び出し側が「失効 0 件」と区別できず、reload の取りこぼしに気付けない。
	muterIDs, err := repo.DeleteExpired(time.Now())
	assert.Error(t, err)
	assert.Nil(t, muterIDs)
}

// ListMuteeIDs は active (非 expired) な mute だけを返すこと、空 muterID は
// nil を返すことを確認する (#874 timeline filter 用)。
func TestMutingRepository_ListMuteeIDs(t *testing.T) {
	repo := NewMutingRepository(testDB)
	u1 := insertTestUser(t, "u_mlmid_1", "mlmid1")
	u2 := insertTestUser(t, "u_mlmid_2", "mlmid2")
	u3 := insertTestUser(t, "u_mlmid_3", "mlmid3")
	u4 := insertTestUser(t, "u_mlmid_4", "mlmid4")
	defer cleanupUser(t, u1.ID)
	defer cleanupUser(t, u2.ID)
	defer cleanupUser(t, u3.ID)
	defer cleanupUser(t, u4.ID)

	// active (no expiry)
	active := &model.Muting{ID: "mlm_active", MuterID: u1.ID, MuteeID: u2.ID}
	require.NoError(t, repo.Create(active))
	defer cleanupMuting(t, active.ID)

	// active (future expiry)
	future := time.Now().Add(1 * time.Hour)
	activeFuture := &model.Muting{ID: "mlm_active_future", MuterID: u1.ID, MuteeID: u3.ID, ExpiresAt: &future}
	require.NoError(t, repo.Create(activeFuture))
	defer cleanupMuting(t, activeFuture.ID)

	// expired
	past := time.Now().Add(-1 * time.Hour)
	expired := &model.Muting{ID: "mlm_expired", MuterID: u1.ID, MuteeID: u4.ID, ExpiresAt: &past}
	require.NoError(t, repo.Create(expired))
	defer cleanupMuting(t, expired.ID)

	ids, err := repo.ListMuteeIDs(u1.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{u2.ID, u3.ID}, ids)

	// muterID 空は nil 返却 (#874 viewer==nil の short-circuit と整合)
	ids, err = repo.ListMuteeIDs("")
	require.NoError(t, err)
	assert.Nil(t, ids)

	// ListAllMuteeIDs は expiry filter なし → expired (u4) も含めた全件 (#1555)。
	all, err := repo.ListAllMuteeIDs(u1.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{u2.ID, u3.ID, u4.ID}, all)

	all, err = repo.ListAllMuteeIDs("")
	require.NoError(t, err)
	assert.Nil(t, all)
}

func TestRenoteMutingRepository_CRUD(t *testing.T) {
	repo := NewRenoteMutingRepository(testDB)
	u1 := insertTestUser(t, "u_rmt_1", "rmt1")
	u2 := insertTestUser(t, "u_rmt_2", "rmt2")
	defer cleanupUser(t, u1.ID)
	defer cleanupUser(t, u2.ID)

	rec := &model.RenoteMuting{ID: "rm_1", MuterID: u1.ID, MuteeID: u2.ID}
	require.NoError(t, repo.Create(rec))
	defer cleanupRenoteMuting(t, rec.ID)

	found, err := repo.FindByPair(u1.ID, u2.ID)
	require.NoError(t, err)
	assert.Equal(t, "rm_1", found.ID)

	exists, err := repo.Exists(u1.ID, u2.ID)
	require.NoError(t, err)
	assert.True(t, exists)

	rows, err := repo.ListByMuter(u1.ID, "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1)

	require.NoError(t, repo.Delete(rec))
	_, err = repo.FindByPair(u1.ID, u2.ID)
	assert.Error(t, err)
}

func TestRenoteMutingRepository_QueryErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewRenoteMutingRepository(testDB.WithContext(ctx))

	err := repo.Create(&model.RenoteMuting{ID: "x", MuterID: "a", MuteeID: "b"})
	assert.Error(t, err)
	_, err = repo.FindByPair("a", "b")
	assert.Error(t, err)
	_, err = repo.Exists("a", "b")
	assert.Error(t, err)
	_, err = repo.ListByMuter("a", "", "", 10, 0)
	assert.Error(t, err)
	_, err = repo.ListMuteeIDs("a")
	assert.Error(t, err)
}

// ListMuteeIDs は muterID の renote-mute 全 muteeID を返す (#903 timeline
// filter 用)。renote_muting には expiresAt が無いので active filter は不要、
// 空 muterID は nil を返す。
func TestRenoteMutingRepository_ListMuteeIDs(t *testing.T) {
	repo := NewRenoteMutingRepository(testDB)
	u1 := insertTestUser(t, "u_rmlm_1", "rmlm1")
	u2 := insertTestUser(t, "u_rmlm_2", "rmlm2")
	u3 := insertTestUser(t, "u_rmlm_3", "rmlm3")
	defer cleanupUser(t, u1.ID)
	defer cleanupUser(t, u2.ID)
	defer cleanupUser(t, u3.ID)

	rec1 := &model.RenoteMuting{ID: "rmlm_1", MuterID: u1.ID, MuteeID: u2.ID}
	require.NoError(t, repo.Create(rec1))
	defer cleanupRenoteMuting(t, rec1.ID)
	rec2 := &model.RenoteMuting{ID: "rmlm_2", MuterID: u1.ID, MuteeID: u3.ID}
	require.NoError(t, repo.Create(rec2))
	defer cleanupRenoteMuting(t, rec2.ID)

	ids, err := repo.ListMuteeIDs(u1.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{u2.ID, u3.ID}, ids)

	// muterID 空は nil 返却 (viewer==nil の short-circuit と整合)
	ids, err = repo.ListMuteeIDs("")
	require.NoError(t, err)
	assert.Nil(t, ids)
}

// ListByMutee は muteeId 起点で有効なミュート行を返す (#2419 アカウント移行の
// ミュート引き継ぎ用)。期限切れを除外し、expiresAt をそのまま返すことを固定する。
func TestMutingRepository_ListByMutee(t *testing.T) {
	repo := NewMutingRepository(testDB)
	target := insertTestUser(t, "u_mlbm_t", "mlbmt")
	defer cleanupUser(t, target.ID)
	other := insertTestUser(t, "u_mlbm_o", "mlbmo")
	defer cleanupUser(t, other.ID)
	muterA := insertTestUser(t, "u_mlbm_a", "mlbma")
	defer cleanupUser(t, muterA.ID)
	muterB := insertTestUser(t, "u_mlbm_b", "mlbmb")
	defer cleanupUser(t, muterB.ID)
	muterC := insertTestUser(t, "u_mlbm_c", "mlbmc")
	defer cleanupUser(t, muterC.ID)

	indefinite := &model.Muting{ID: "mlbm_indef", MuterID: muterA.ID, MuteeID: target.ID}
	require.NoError(t, repo.Create(indefinite))
	defer cleanupMuting(t, indefinite.ID)

	future := time.Now().Add(1 * time.Hour)
	timed := &model.Muting{ID: "mlbm_timed", MuterID: muterB.ID, MuteeID: target.ID, ExpiresAt: &future}
	require.NoError(t, repo.Create(timed))
	defer cleanupMuting(t, timed.ID)

	// 期限切れは対象外。
	past := time.Now().Add(-1 * time.Hour)
	expired := &model.Muting{ID: "mlbm_expired", MuterID: muterC.ID, MuteeID: target.ID, ExpiresAt: &past}
	require.NoError(t, repo.Create(expired))
	defer cleanupMuting(t, expired.ID)

	// 別ユーザーへのミュートも対象外。
	unrelated := &model.Muting{ID: "mlbm_other", MuterID: muterA.ID, MuteeID: other.ID}
	require.NoError(t, repo.Create(unrelated))
	defer cleanupMuting(t, unrelated.ID)

	rows, err := repo.ListByMutee(target.ID)
	require.NoError(t, err)
	require.Len(t, rows, 2)

	byMuter := map[string]*model.Muting{}
	for _, r := range rows {
		byMuter[r.MuterID] = r
	}
	require.Contains(t, byMuter, muterA.ID)
	assert.Nil(t, byMuter[muterA.ID].ExpiresAt, "indefinite mute keeps a nil expiry")
	require.Contains(t, byMuter, muterB.ID)
	require.NotNil(t, byMuter[muterB.ID].ExpiresAt)
	assert.WithinDuration(t, future, *byMuter[muterB.ID].ExpiresAt, time.Second,
		"expiresAt is returned as-is so the carry-over can preserve it")

	// muteeID 空は nil 返却。
	rows, err = repo.ListByMutee("")
	require.NoError(t, err)
	assert.Nil(t, rows)
}
