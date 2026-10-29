package repository

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"gorm.io/plugin/dbresolver"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// TestExistingNoteIDsOnPrimary_ReadsPrimaryNotReplica は #2719 の要を固定する。
//
// antenna の宙吊り ID 除去は「DB に無い」を根拠に破壊的な操作をする。通常の
// SELECT はレプリカに振られるので、複製前の行を「無い」と誤判定すると生きて
// いる note の ID が恒久的に消える。この経路だけは primary を見なければ
// ならない。
//
// **レプリカ遅延を schema で模す。** 同じ DB 内に replica 相当の空 schema を
// 作り、そこを dbresolver の Replicas として登録する。primary にだけ行がある
// 状態で、素の SELECT が空振りし ExistingNoteIDsOnPrimary が引けることを見る。
func TestExistingNoteIDsOnPrimary_ReadsPrimaryNotReplica(t *testing.T) {
	replicaSchema := "repo_primary_probe_replica"
	require.NoError(t, testDB.Exec("CREATE SCHEMA IF NOT EXISTS "+replicaSchema).Error)
	t.Cleanup(func() { testDB.Exec("DROP SCHEMA IF EXISTS " + replicaSchema + " CASCADE") })
	// replica 側には note テーブルだけ作り、行は入れない (= 複製がまだ届いて
	// いない状態)。
	require.NoError(t, testDB.Exec(fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s."note" (LIKE "note" INCLUDING ALL)`, replicaSchema)).Error)

	// testDB は呼び出し元パッケージ専用 schema に pin されている (#2450)。
	// primary 側はそれと同じ schema を見なければならない。
	var primarySchema string
	require.NoError(t, testDB.Raw("SELECT current_schema()").Scan(&primarySchema).Error)
	gdb := openWithReplicaSchema(t, primarySchema, replicaSchema)
	repo := &noteRepository{db: gdb}

	testDB.Exec(`DELETE FROM "user" WHERE id = ?`, "primprobeu01")
	u := insertTestUser(t, "primprobeu01", "primprobeu01")
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "user" WHERE id = ?`, u.ID) })
	id := "9primaryprobe0000001"
	require.NoError(t, testDB.Create(&model.Note{
		ID: id, UserID: u.ID, Visibility: model.NoteVisibilityPublic,
	}).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "note" WHERE id = ?`, id) })

	// 素の SELECT は replica に振られるので引けない (= 複製遅延の再現)。
	var replicaSide []*model.Note
	require.NoError(t, gdb.Where("id IN ?", []string{id}).Find(&replicaSide).Error)
	require.Empty(t, replicaSide, "テストの前提: 通常の SELECT は replica を見て空振りする")

	// primary 固定なら引ける。
	got, err := repo.ExistingNoteIDsOnPrimary([]string{id})
	require.NoError(t, err)
	assert.Equal(t, []string{id}, got, "primary に在る行を「無い」と誤判定しない")
}

func TestNotePrimaryReader_UsesCurrentNoteAndAuthorValues(t *testing.T) {
	replicaSchema := "repo_note_policy_replica"
	require.NoError(t, testDB.Exec("CREATE SCHEMA IF NOT EXISTS "+replicaSchema).Error)
	t.Cleanup(func() { testDB.Exec("DROP SCHEMA IF EXISTS " + replicaSchema + " CASCADE") })
	var primarySchema string
	require.NoError(t, testDB.Raw("SELECT current_schema()").Scan(&primarySchema).Error)
	for _, table := range []string{"user", "note", "poll"} {
		require.NoError(t, testDB.Exec(fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s."%s" (LIKE "%s"."%s" INCLUDING ALL)`,
			replicaSchema, table, primarySchema, table)).Error)
	}

	user := insertTestUser(t, "upolpri1", "policyprimary")
	t.Cleanup(func() { cleanupUser(t, user.ID) })
	require.NoError(t, testDB.Model(&model.User{}).Where("id = ?", user.ID).
		Update("requireSigninToViewContents", true).Error)
	note := insertTestNote(t, "npolpri1", user.ID)
	t.Cleanup(func() { cleanupNote(t, note.ID) })
	require.NoError(t, testDB.Model(&model.Note{}).Where("id = ?", note.ID).
		Update("visibility", model.NoteVisibilityFollowers).Error)

	// Seed the replica with an intentionally stale, more permissive snapshot.
	require.NoError(t, testDB.Exec(fmt.Sprintf(
		`INSERT INTO %s."user" SELECT * FROM "%s"."user" WHERE id = ?`,
		replicaSchema, primarySchema), user.ID).Error)
	require.NoError(t, testDB.Exec(fmt.Sprintf(
		`INSERT INTO %s."note" SELECT * FROM "%s"."note" WHERE id = ?`,
		replicaSchema, primarySchema), note.ID).Error)
	require.NoError(t, testDB.Exec(fmt.Sprintf(
		`UPDATE %s."user" SET "requireSigninToViewContents" = false WHERE id = ?`,
		replicaSchema), user.ID).Error)
	require.NoError(t, testDB.Exec(fmt.Sprintf(
		`UPDATE %s."note" SET visibility = 'public' WHERE id = ?`,
		replicaSchema), note.ID).Error)

	gdb := openWithReplicaSchema(t, primarySchema, replicaSchema)
	repo := &noteRepository{db: gdb}

	stale, err := repo.FindByIDWithRelations(note.ID)
	require.NoError(t, err)
	require.NotNil(t, stale.User)
	assert.Equal(t, model.NoteVisibilityPublic, stale.Visibility,
		"test premise: ordinary reads observe the stale replica")
	assert.False(t, stale.User.RequireSigninToViewContents,
		"test premise: author preferences are stale on the replica")

	current, err := repo.FindByIDWithRelationsOnPrimary(note.ID)
	require.NoError(t, err)
	require.NotNil(t, current.User)
	assert.Equal(t, model.NoteVisibilityFollowers, current.Visibility)
	assert.True(t, current.User.RequireSigninToViewContents)

	currentWithUser, err := repo.FindByIDWithUserOnPrimary(note.ID)
	require.NoError(t, err)
	require.NotNil(t, currentWithUser.User)
	assert.Equal(t, model.NoteVisibilityFollowers, currentWithUser.Visibility)
	assert.True(t, currentWithUser.User.RequireSigninToViewContents)

	many, err := repo.FindManyByIDsWithUserOnPrimary([]string{note.ID})
	require.NoError(t, err)
	require.Len(t, many, 1)
	require.NotNil(t, many[0].User)
	assert.Equal(t, model.NoteVisibilityFollowers, many[0].Visibility)
	assert.True(t, many[0].User.RequireSigninToViewContents)

}

func TestFindManyLocalReferenceMetadataByIDs_UsesPrimaryAndPreservesInputOrder(t *testing.T) {
	replicaSchema := "repo_note_ref_metadata_replica"
	require.NoError(t, testDB.Exec("CREATE SCHEMA IF NOT EXISTS "+replicaSchema).Error)
	t.Cleanup(func() { testDB.Exec("DROP SCHEMA IF EXISTS " + replicaSchema + " CASCADE") })
	var primarySchema string
	require.NoError(t, testDB.Raw("SELECT current_schema()").Scan(&primarySchema).Error)
	require.NoError(t, testDB.Exec(fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s."note" (LIKE "%s"."note" INCLUDING ALL)`,
		replicaSchema, primarySchema)).Error)

	user := insertTestUser(t, "urefmeta1", "refmetadata")
	t.Cleanup(func() { cleanupUser(t, user.ID) })
	note := insertTestNote(t, "nrefmeta1", user.ID)
	t.Cleanup(func() { cleanupNote(t, note.ID) })
	require.NoError(t, testDB.Model(&model.Note{}).Where("id = ?", note.ID).Update("localOnly", true).Error)
	remoteHost := "remote.example"
	remoteNote := &model.Note{ID: "nrefmeta2", UserID: user.ID, UserHost: &remoteHost, Visibility: model.NoteVisibilityPublic}
	require.NoError(t, testDB.Create(remoteNote).Error)
	t.Cleanup(func() { cleanupNote(t, remoteNote.ID) })
	// レプリカには制限の緩い古い行を置く。
	require.NoError(t, testDB.Exec(fmt.Sprintf(
		`INSERT INTO %s."note" SELECT * FROM "%s"."note" WHERE id = ?`,
		replicaSchema, primarySchema), note.ID).Error)
	require.NoError(t, testDB.Exec(fmt.Sprintf(
		`UPDATE %s."note" SET "localOnly" = false WHERE id = ?`, replicaSchema), note.ID).Error)
	require.NoError(t, testDB.Exec(fmt.Sprintf(
		`INSERT INTO %s."note" SELECT * FROM "%s"."note" WHERE id = ?`,
		replicaSchema, primarySchema), remoteNote.ID).Error)

	gdb := openWithReplicaSchema(t, primarySchema, replicaSchema)
	repo := &noteRepository{db: gdb}
	missingID := "nrefmetagone"
	var stale model.Note
	require.NoError(t, gdb.Where("id = ?", note.ID).First(&stale).Error)
	assert.False(t, stale.LocalOnly, "通常SELECTでは古いレプリカ行が見える")
	got, err := repo.FindManyLocalReferenceMetadataByIDs([]string{remoteNote.ID, missingID, note.ID})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []string{remoteNote.ID, note.ID}, []string{got[0].ID, got[1].ID}, "missing IDs are omitted while result order follows input")
	assert.Equal(t, user.ID, got[0].UserID)
	assert.Equal(t, &remoteHost, got[0].UserHost)
	assert.False(t, got[0].LocalOnly)
	assert.Equal(t, user.ID, got[1].UserID)
	assert.Nil(t, got[1].UserHost)
	assert.True(t, got[1].LocalOnly, "privacy metadata must come from the primary")

	got, err = repo.FindManyLocalReferenceMetadataByIDs(nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestExistingNoteIDsOnPrimary_Subset は返す集合を固定する。
func TestExistingNoteIDsOnPrimary_Subset(t *testing.T) {
	repo := NewNoteRepository(testDB)
	testDB.Exec(`DELETE FROM "user" WHERE id = ?`, "primsubsetu1")
	u := insertTestUser(t, "primsubsetu1", "primsubsetu1")
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "user" WHERE id = ?`, u.ID) })
	id := "9primarysubset000001"
	require.NoError(t, testDB.Create(&model.Note{
		ID: id, UserID: u.ID, Visibility: model.NoteVisibilityPublic,
	}).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "note" WHERE id = ?`, id) })

	got, err := repo.ExistingNoteIDsOnPrimary([]string{id, "9primarysubsetgone01"})
	require.NoError(t, err)
	assert.Equal(t, []string{id}, got, "在るものだけ返す")

	got, err = repo.ExistingNoteIDsOnPrimary(nil)
	require.NoError(t, err)
	assert.Empty(t, got, "空入力ではクエリを撃たない")
}

// openWithReplicaSchema は primary = 既定 schema、replica = 指定 schema として
// dbresolver を登録した *gorm.DB を返す。
func openWithReplicaSchema(t *testing.T, primarySchema, replicaSchema string) *gorm.DB {
	t.Helper()
	primaryDSN := testutil.TestDSNForSchema(primarySchema)
	replicaDSN := testutil.TestDSNForSchema(replicaSchema)

	// testutil.openDSN と同じ設定にする。PreferSimpleProtocol は #1089
	// (`cached plan must not change result type`) 対策で、同じ DB に
	// ApplyMigrations が additive DDL を回すこの環境では外す理由が無い。
	gdb, err := gorm.Open(postgres.New(postgres.Config{
		DSN: primaryDSN, PreferSimpleProtocol: true,
	}), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, gdb.Use(dbresolver.Register(dbresolver.Config{
		Replicas: []gorm.Dialector{postgres.New(postgres.Config{
			DSN: replicaDSN, PreferSimpleProtocol: true,
		})},
		Policy: dbresolver.RandomPolicy{},
	})))
	return gdb
}
