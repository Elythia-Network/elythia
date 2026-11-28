package repository

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// meta は primary から読む (#4276)。CachedMetaRepository は更新で捨てた直後に読み
// 直して TTL の間持つので、複製の前のレプリカを読むと古い行を持ち続ける。レプリカに
// 古い行を置いた schema を Replicas に登録し、primary の行が返ることを見る。
func TestMetaRepository_FetchReadsPrimary(t *testing.T) {
	replicaSchema := "repo_meta_primary_replica"
	require.NoError(t, testDB.Exec("CREATE SCHEMA IF NOT EXISTS "+replicaSchema).Error)
	t.Cleanup(func() { testDB.Exec("DROP SCHEMA IF EXISTS " + replicaSchema + " CASCADE") })
	var primarySchema string
	require.NoError(t, testDB.Raw("SELECT current_schema()").Scan(&primarySchema).Error)
	require.NoError(t, NewMetaRepository(testDB).EnsureInitial("x"))
	require.NoError(t, testDB.Exec(fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s."meta" (LIKE %q."meta" INCLUDING ALL)`, replicaSchema, primarySchema)).Error)
	require.NoError(t, testDB.Exec(fmt.Sprintf(
		`INSERT INTO %s."meta" SELECT * FROM %q."meta"`, replicaSchema, primarySchema)).Error)

	var saved bool
	require.NoError(t, testDB.Model(&model.Meta{}).Select(`"enableServerMachineStats"`).Take(&saved).Error)
	t.Cleanup(func() { testDB.Exec(`UPDATE "meta" SET "enableServerMachineStats" = ?`, saved) })
	// primary だけが有効になった (= 複製がまだ届いていない) 状態。
	require.NoError(t, testDB.Exec(`UPDATE "meta" SET "enableServerMachineStats" = true`).Error)
	require.NoError(t, testDB.Exec(fmt.Sprintf(`UPDATE %s."meta" SET "enableServerMachineStats" = false`, replicaSchema)).Error)

	gdb := openWithReplicaSchema(t, primarySchema, replicaSchema)
	var replicaSide model.Meta
	require.NoError(t, gdb.First(&replicaSide).Error)
	require.False(t, replicaSide.EnableServerMachineStats, "テストの前提: 素の SELECT は replica を見る")

	m, err := NewMetaRepository(gdb).Fetch()
	require.NoError(t, err)
	assert.True(t, m.EnableServerMachineStats, "meta must be read from the primary")
}

// 行が無いときに作った後の読み直しも primary を読む。レプリカには作った直後の行が
// まだ無いので、レプリカを読むと「無い」で失敗する。
func TestMetaRepository_FetchRecreatesOnPrimary(t *testing.T) {
	replicaSchema := "repo_meta_recreate_replica"
	require.NoError(t, testDB.Exec("CREATE SCHEMA IF NOT EXISTS "+replicaSchema).Error)
	t.Cleanup(func() { testDB.Exec("DROP SCHEMA IF EXISTS " + replicaSchema + " CASCADE") })
	var primarySchema string
	require.NoError(t, testDB.Raw("SELECT current_schema()").Scan(&primarySchema).Error)
	require.NoError(t, NewMetaRepository(testDB).EnsureInitial("x"))
	// レプリカは空の表 (複製がまだ届いていない)。
	require.NoError(t, testDB.Exec(fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s."meta" (LIKE %q."meta" INCLUDING ALL)`, replicaSchema, primarySchema)).Error)
	// primary の行を退避して消し、終わったら戻す。
	require.NoError(t, testDB.Exec(fmt.Sprintf(`CREATE TABLE %s."meta_saved" AS SELECT * FROM %q."meta"`, replicaSchema, primarySchema)).Error)
	t.Cleanup(func() {
		require.NoError(t, testDB.Exec(`DELETE FROM "meta"`).Error)
		require.NoError(t, testDB.Exec(fmt.Sprintf(`INSERT INTO "meta" SELECT * FROM %s."meta_saved"`, replicaSchema)).Error)
	})
	require.NoError(t, testDB.Exec(`DELETE FROM "meta"`).Error)

	gdb := openWithReplicaSchema(t, primarySchema, replicaSchema)
	m, err := NewMetaRepository(gdb).Fetch()
	require.NoError(t, err, "the row created on the primary must be read back from the primary")
	require.NotNil(t, m)
	var n int64
	require.NoError(t, testDB.Model(&model.Meta{}).Count(&n).Error)
	assert.Equal(t, int64(1), n)
}
