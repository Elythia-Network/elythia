package repository

import (
	"context"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func cleanupAntenna(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "antenna" WHERE id = ?`, id)
}

func newTestAntenna(id, ownerID, name string) *model.Antenna {
	return &model.Antenna{
		ID:              id,
		LastUsedAt:      time.Now(),
		UserID:          ownerID,
		Name:            name,
		Src:             model.AntennaSourceAll,
		Keywords:        datatypes.JSON([]byte(`[]`)),
		ExcludeKeywords: datatypes.JSON([]byte(`[]`)),
		IsActive:        true,
	}
}

func TestAntennaRepository_CreateAndFindByID(t *testing.T) {
	repo := NewAntennaRepository(testDB)
	user := insertTestUser(t, "u_ar_1", "antuser1")
	defer cleanupUser(t, user.ID)

	a := newTestAntenna("ant_cr_1", user.ID, "alpha antenna")
	require.NoError(t, repo.Create(a))
	defer cleanupAntenna(t, a.ID)

	got, err := repo.FindByID(a.ID)
	require.NoError(t, err)
	assert.Equal(t, "alpha antenna", got.Name)
	assert.Equal(t, model.AntennaSourceAll, got.Src)
}

func TestAntennaRepository_FindByID_NotFound(t *testing.T) {
	repo := NewAntennaRepository(testDB)
	_, err := repo.FindByID("missing")
	assert.Error(t, err)
}

func TestAntennaRepository_UpdateFields(t *testing.T) {
	repo := NewAntennaRepository(testDB)
	user := insertTestUser(t, "u_ar_2", "antuser2")
	defer cleanupUser(t, user.ID)

	a := newTestAntenna("ant_cr_2", user.ID, "beta")
	require.NoError(t, repo.Create(a))
	defer cleanupAntenna(t, a.ID)

	require.NoError(t, repo.UpdateFields(a.ID, map[string]any{
		"name":          "beta updated",
		"caseSensitive": true,
	}))

	got, err := repo.FindByID(a.ID)
	require.NoError(t, err)
	assert.Equal(t, "beta updated", got.Name)
	assert.True(t, got.CaseSensitive)
}

func TestAntennaRepository_UpdateFields_NoOp(t *testing.T) {
	repo := NewAntennaRepository(testDB)
	require.NoError(t, repo.UpdateFields("any", nil))
}

// UpdateFields で users に空 model.StringArray を渡しても NOT NULL 制約違反
// を起こさないこと (#896 と同 pattern)。core/antenna.Service.Update が
// model.StringArray() で wrap せずに plain []string を渡していた時、GORM が
// NULL に倒して 23502 を起こしていた regression guard。
func TestAntennaRepository_UpdateFields_EmptyUsers(t *testing.T) {
	repo := NewAntennaRepository(testDB)
	user := insertTestUser(t, "u_ar_users", "antusersuser")
	defer cleanupUser(t, user.ID)

	a := newTestAntenna("ant_cr_users", user.ID, "users")
	a.Users = model.StringArray{"u1", "u2"}
	require.NoError(t, repo.Create(a))
	defer cleanupAntenna(t, a.ID)

	// 空 users で update — model.StringArray{} なら '{}' に serialize、
	// plain []string{} だと GORM が NULL に倒す drift。
	require.NoError(t, repo.UpdateFields(a.ID, map[string]any{
		"users": model.StringArray{},
	}))

	got, err := repo.FindByID(a.ID)
	require.NoError(t, err)
	assert.NotNil(t, got.Users, "users は NULL にならず空配列で保存される")
	assert.Empty(t, got.Users)
}

func TestAntennaRepository_Delete(t *testing.T) {
	repo := NewAntennaRepository(testDB)
	user := insertTestUser(t, "u_ar_3", "antuser3")
	defer cleanupUser(t, user.ID)

	a := newTestAntenna("ant_cr_3", user.ID, "gamma")
	require.NoError(t, repo.Create(a))
	require.NoError(t, repo.Delete(a))
	_, err := repo.FindByID(a.ID)
	assert.Error(t, err)
}

func TestAntennaRepository_ListByUser(t *testing.T) {
	repo := NewAntennaRepository(testDB)
	user := insertTestUser(t, "u_ar_4", "antuser4")
	defer cleanupUser(t, user.ID)

	for _, id := range []string{"ant_lst_1", "ant_lst_2", "ant_lst_3"} {
		a := newTestAntenna(id, user.ID, id)
		require.NoError(t, repo.Create(a))
		defer cleanupAntenna(t, a.ID)
	}

	rows, err := repo.ListByUser(user.ID)
	require.NoError(t, err)
	assert.Len(t, rows, 3)

	count, err := repo.CountByUser(user.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), count)
}

func TestAntennaRepository_ListAllActive(t *testing.T) {
	repo := NewAntennaRepository(testDB)
	user := insertTestUser(t, "u_ar_5", "antuser5")
	defer cleanupUser(t, user.ID)

	active := newTestAntenna("ant_act_1", user.ID, "active")
	inactive := newTestAntenna("ant_act_2", user.ID, "inactive")
	inactive.IsActive = false
	for _, a := range []*model.Antenna{active, inactive} {
		require.NoError(t, repo.Create(a))
		defer cleanupAntenna(t, a.ID)
	}

	rows, err := repo.ListAllActive()
	require.NoError(t, err)
	// 他テストの残骸が混ざる可能性があるので "ant_act_1" が含まれるかだけ検証
	var found bool
	for _, a := range rows {
		if a.ID == "ant_act_1" {
			found = true
		}
		assert.True(t, a.IsActive)
	}
	assert.True(t, found)
}

func TestAntennaRepository_DeactivateUnusedSince(t *testing.T) {
	repo := NewAntennaRepository(testDB)
	user := insertTestUser(t, "u_ar_6", "antuser6")
	defer cleanupUser(t, user.ID)

	now := time.Now()
	// recent: cutoff より新しい (残る) / stale: cutoff より古い (deactivate) /
	// alreadyOff: 古いが既に非アクティブ (RowsAffected に数えない)。
	recent := newTestAntenna("ant_de_recent", user.ID, "recent")
	recent.LastUsedAt = now.Add(-time.Hour)
	stale := newTestAntenna("ant_de_stale", user.ID, "stale")
	stale.LastUsedAt = now.Add(-30 * 24 * time.Hour)
	alreadyOff := newTestAntenna("ant_de_off", user.ID, "off")
	alreadyOff.LastUsedAt = now.Add(-30 * 24 * time.Hour)
	alreadyOff.IsActive = false
	for _, a := range []*model.Antenna{recent, stale, alreadyOff} {
		require.NoError(t, repo.Create(a))
		defer cleanupAntenna(t, a.ID)
	}

	cutoff := now.Add(-7 * 24 * time.Hour)
	// DeactivateUnusedSince は全ユーザー横断のグローバル操作なので、共有 testDB の
	// 他テスト残骸も拾いうる。正確な件数ではなく「stale が含まれる (>=1)」と
	// per-antenna の状態で検証する。
	n, err := repo.DeactivateUnusedSince(cutoff)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(1), "cutoff より古い active antenna が deactivate される")

	gotRecent, err := repo.FindByID(recent.ID)
	require.NoError(t, err)
	assert.True(t, gotRecent.IsActive, "cutoff より新しい antenna は active のまま")
	gotStale, err := repo.FindByID(stale.ID)
	require.NoError(t, err)
	assert.False(t, gotStale.IsActive, "cutoff より古い antenna は deactivate")
	gotOff, err := repo.FindByID(alreadyOff.ID)
	require.NoError(t, err)
	assert.False(t, gotOff.IsActive, "既に非アクティブな antenna はそのまま")

	// 2回目は stale-active 行が残っていないので 0。isActive=true ガードにより
	// 既に非アクティブな行 (alreadyOff 含む) を再カウント・再書き込みしないことの検証。
	n2, err := repo.DeactivateUnusedSince(cutoff)
	require.NoError(t, err)
	assert.EqualValues(t, 0, n2, "既に非アクティブな行は再カウントしない")
}

func TestAntennaRepository_QueryErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewAntennaRepository(db)
	_, err := repo.ListByUser("any")
	assert.Error(t, err)
	_, err = repo.ListAllActive()
	assert.Error(t, err)
	_, err = repo.CountByUser("any")
	assert.Error(t, err)
}
