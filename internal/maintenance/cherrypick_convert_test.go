package maintenance

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"gorm.io/plugin/dbresolver"

	"github.com/elythia-network/elythia/internal/testutil"
)

// seedConvert puts one of each kind of item into a CherryPick database.
func seedConvert(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, sql := range []string{
		`INSERT INTO meta (id, "approvalRequiredForSignup", "repositoryUrl", "blockedHosts")
			VALUES ('x', true, 'https://github.com/kokonect-link/cherrypick', '{Existing.example}')`,
		`INSERT INTO "user" (id, username, "usernameLower", approved) VALUES ('a1', 'alice', 'alice', true)`,
		`INSERT INTO "user" (id, username, "usernameLower", approved, "signupReason") VALUES
			('p1', 'carol', 'carol', false, 'hi'), ('p2', 'dave', 'dave', false, 'hello')`,
		`INSERT INTO "user" (id, username, "usernameLower", approved) VALUES
			('m1', 'erin', 'erin', false), ('m2', 'frank', 'frank', false)`,
		`INSERT INTO abuse_report_resolver (id, "updatedAt", name, "reportContentPattern", "expiresAt", forward)
			VALUES ('r1', now(), 'spam', 'buy now', '1week', true)`,
		`INSERT INTO instance (id, "firstRetrievedAt", host, "quarantineLimited") VALUES
			('i1', now(), 'q.example', true), ('i2', now(), 'existing.example', true), ('i3', now(), 'ok.example', false)`,
	} {
		require.NoError(t, db.Exec(sql).Error, sql)
	}
	past, future := cherryPickNow.Add(-time.Hour), cherryPickNow.Add(time.Hour)
	for _, n := range []struct {
		id string
		at *time.Time
	}{{"n1", &past}, {"n2", &future}, {"n3", nil}} {
		require.NoError(t, db.Exec(`INSERT INTO note (id, "userId", visibility, "deleteAt") VALUES (?, 'a1', 'public', ?)`, n.id, n.at).Error)
	}
}

func blockedHosts(t *testing.T, db *gorm.DB) []string {
	t.Helper()
	var hosts []string
	require.NoError(t, db.Raw(`SELECT unnest("blockedHosts") FROM meta`).Scan(&hosts).Error)
	return hosts
}

func TestApplyCherryPickSQL(t *testing.T) {
	ctx := context.Background()
	t.Run("suspend, block, keep, resolvers, repositoryUrl", func(t *testing.T) {
		db := openCherryPickDB(t)
		seedConvert(t, db)
		require.Equal(t, 6, inspect(t, db).Unprocessed())
		// リモートの予約削除は「残す」の対象にしない。
		require.NoError(t, db.Exec(`INSERT INTO note (id, "userId", "userHost", visibility, "deleteAt")
			VALUES ('n8', 'a1', 'remote.example', 'public', ?)`, cherryPickNow.Add(time.Hour)).Error)

		require.NoError(t, ApplyCherryPickSQL(ctx, db, CherryPickPlan{
			PendingUserIDs: []string{"p1", "p2"}, PendingAction: CherryPickSuspend,
			UnapprovedAction: CherryPickSuspend,
			QuarantinedHosts: []string{"existing.example", "q.example"}, QuarantineAction: CherryPickBlock,
			KeepLaterDeletes: true, DeleteResolvers: true,
			RepositoryURL: "https://github.com/kokonect-link/cherrypick", ElythiaRepositoryURL: "https://elythia.example/repo",
			FeedbackURL: "https://github.com/kokonect-link/cherrypick/issues/new", ElythiaFeedbackURL: "https://elythia.example/repo/issues/new",
			Now: cherryPickNow,
		}))

		r := inspect(t, db)
		assert.Empty(t, r.PendingUsers)
		assert.Zero(t, r.UnapprovedMembers)
		assert.Equal(t, 1, r.Unprocessed(), "残るのは期限切れの予約削除だけ")
		assert.Zero(t, r.AbuseReportResolvers)
		assert.Empty(t, r.QuarantinedHosts)
		assert.Empty(t, r.RepositoryURL)
		assert.Empty(t, r.FeedbackURL)
		assert.Zero(t, r.ScheduledDeletesLater)
		assert.EqualValues(t, 1, r.ScheduledDeletesDue, "期限切れは本体の削除で消すので、ここでは残る")
		assert.Equal(t, []string{"Existing.example", "q.example"}, blockedHosts(t, db), "大文字小文字が違うだけのものは足さない")

		var suspended int64
		require.NoError(t, db.Raw(`SELECT count(*) FROM "user" WHERE "isSuspended" AND id IN ('p1', 'p2', 'm1', 'm2')`).Scan(&suspended).Error)
		assert.EqualValues(t, 4, suspended)
		var aliceSuspended bool
		require.NoError(t, db.Raw(`SELECT "isSuspended" FROM "user" WHERE id = 'a1'`).Scan(&aliceSuspended).Error)
		assert.False(t, aliceSuspended, "承認済みの利用者には触らない")
		assert.EqualValues(t, 1, r.RemoteScheduledDeletes, "リモートの deleteAt は残る")
		var url string
		require.NoError(t, db.Raw(`SELECT "repositoryUrl" FROM meta`).Scan(&url).Error)
		assert.Equal(t, "https://elythia.example/repo", url)
		var feedback string
		require.NoError(t, db.Raw(`SELECT "feedbackUrl" FROM meta`).Scan(&feedback).Error)
		assert.Equal(t, "https://elythia.example/repo/issues/new", feedback)
		var ok int64
		require.NoError(t, db.Raw(`SELECT count(*) FROM instance WHERE NOT "quarantineLimited"`).Scan(&ok).Error)
		assert.EqualValues(t, 3, ok)
	})
	t.Run("approve and lift", func(t *testing.T) {
		db := openCherryPickDB(t)
		seedConvert(t, db)
		require.NoError(t, ApplyCherryPickSQL(ctx, db, CherryPickPlan{
			PendingUserIDs: []string{"p1"}, PendingAction: CherryPickApprove,
			UnapprovedAction: CherryPickApprove,
			QuarantinedHosts: []string{"q.example"}, QuarantineAction: CherryPickLift,
			Now: cherryPickNow,
		}))
		r := inspect(t, db)
		assert.Zero(t, r.UnapprovedMembers, "理由の無い利用者は承認された")
		require.Len(t, r.PendingUsers, 1, "理由のある p2 は承認されない")
		assert.Equal(t, "p2", r.PendingUsers[0].ID)
		assert.Equal(t, []string{"existing.example"}, r.QuarantinedHosts)
		assert.Equal(t, []string{"Existing.example"}, blockedHosts(t, db), "外すときはブロックへ足さない")
		assert.EqualValues(t, 1, r.AbuseReportResolvers)
		assert.EqualValues(t, 1, r.ScheduledDeletesLater)
		assert.NotEmpty(t, r.RepositoryURL)
	})
	t.Run("delete leaves the users to the server", func(t *testing.T) {
		db := openCherryPickDB(t)
		seedConvert(t, db)
		require.NoError(t, ApplyCherryPickSQL(ctx, db, CherryPickPlan{
			PendingUserIDs: []string{"p1", "p2"}, PendingAction: CherryPickDelete,
			UnapprovedAction: CherryPickDelete, Now: cherryPickNow,
		}))
		r := inspect(t, db)
		assert.Len(t, r.PendingUsers, 2)
		assert.EqualValues(t, 2, r.UnapprovedMembers)
		ids, err := CherryPickUnapprovedMemberIDs(ctx, db)
		require.NoError(t, err)
		assert.Equal(t, []string{"m1", "m2"}, ids, "理由のある申請者と承認済みの利用者は含めない")
	})
	t.Run("unknown actions roll back", func(t *testing.T) {
		db := openCherryPickDB(t)
		seedConvert(t, db)
		assert.Error(t, ApplyCherryPickSQL(ctx, db, CherryPickPlan{
			PendingUserIDs: []string{"p1"}, PendingAction: "ban", Now: cherryPickNow,
		}))
		assert.Error(t, ApplyCherryPickSQL(ctx, db, CherryPickPlan{
			PendingUserIDs: []string{"p1"}, PendingAction: CherryPickSuspend,
			QuarantinedHosts: []string{"q.example"}, QuarantineAction: "ignore", Now: cherryPickNow,
		}))
		assert.Error(t, ApplyCherryPickSQL(ctx, db, CherryPickPlan{
			PendingUserIDs: []string{"p1"}, PendingAction: CherryPickSuspend,
			UnapprovedAction: "ban", Now: cherryPickNow,
		}))
		assert.Len(t, inspect(t, db).PendingUsers, 2, "同じトランザクションの凍結も戻る")
	})
	t.Run("block without a meta row", func(t *testing.T) {
		db := openCherryPickDB(t)
		assert.Error(t, ApplyCherryPickSQL(ctx, db, CherryPickPlan{
			QuarantinedHosts: []string{"q.example"}, QuarantineAction: CherryPickBlock, Now: cherryPickNow,
		}))
	})
}

func TestCherryPickResolversAndScheduledDeletes(t *testing.T) {
	ctx := context.Background()
	db := openCherryPickDB(t)
	seedConvert(t, db)
	// リモートの予約削除は含めない。
	require.NoError(t, db.Exec(`INSERT INTO note (id, "userId", "userHost", visibility, "deleteAt")
		VALUES ('n9', 'a1', 'remote.example', 'public', ?)`, cherryPickNow.Add(-time.Hour)).Error)

	rows, err := CherryPickResolvers(ctx, db)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "spam", rows[0].Name)
	assert.Equal(t, "buy now", *rows[0].ReportContentPattern)
	assert.Nil(t, rows[0].TargetUserPattern)
	assert.Equal(t, "1week", rows[0].ExpiresAt)
	assert.True(t, rows[0].Forward)

	due, later, err := CherryPickScheduledDeletes(ctx, db, cherryPickNow)
	require.NoError(t, err)
	assert.Equal(t, []string{"n1"}, due)
	assert.Equal(t, []string{"n2"}, later)

	require.NoError(t, db.Exec(`DROP TABLE abuse_report_resolver`).Error)
	require.NoError(t, db.Exec(`ALTER TABLE note DROP COLUMN "deleteAt"`).Error)
	rows, err = CherryPickResolvers(ctx, db)
	require.NoError(t, err)
	assert.Empty(t, rows)
	due, later, err = CherryPickScheduledDeletes(ctx, db, cherryPickNow)
	require.NoError(t, err)
	assert.Empty(t, due)
	assert.Empty(t, later)
}

func TestCherryPickConvertedMarker(t *testing.T) {
	ctx := context.Background()
	db := openCherryPickDB(t)
	converted, err := CherryPickConverted(ctx, db)
	require.NoError(t, err)
	assert.False(t, converted)

	require.NoError(t, MarkCherryPickConverted(ctx, db))
	require.NoError(t, MarkCherryPickConverted(ctx, db))
	converted, err = CherryPickConverted(ctx, db)
	require.NoError(t, err)
	assert.True(t, converted)
	var n int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM migrations WHERE name = ?`, cherryPickConvertedMarker).Scan(&n).Error)
	assert.EqualValues(t, 1, n, "2 回付けても 1 行")
	assert.Equal(t, CherryPickSupported, inspect(t, db).Status, "印の記録は版の判定に掛からない")

	migrated, err := ElythiaMigrated(ctx, db)
	require.NoError(t, err)
	assert.False(t, migrated)
	require.NoError(t, db.Exec(`CREATE TABLE schema_migrations (version bigint PRIMARY KEY, dirty boolean NOT NULL)`).Error)
	require.NoError(t, db.Exec(`INSERT INTO schema_migrations VALUES (1, false)`).Error)
	migrated, err = ElythiaMigrated(ctx, db)
	require.NoError(t, err)
	assert.True(t, migrated)

	require.NoError(t, db.Exec(`DROP TABLE migrations`).Error)
	converted, err = CherryPickConverted(ctx, db)
	require.NoError(t, err)
	assert.False(t, converted)
}

func TestCherryPickLeftovers(t *testing.T) {
	ctx := context.Background()
	db := openCherryPickDB(t)
	left, err := CherryPickLeftovers(ctx, db)
	require.NoError(t, err)
	var names []string
	for _, l := range left {
		names = append(names, l.String())
	}
	assert.Equal(t, []string{"user.approved", "instance.quarantineLimited", "abuse_report_resolver", "note.deleteAt"}, names)

	require.NoError(t, DropCherryPickLeftovers(ctx, db))
	left, err = CherryPickLeftovers(ctx, db)
	require.NoError(t, err)
	assert.Empty(t, left)
	var types int64
	require.NoError(t, db.Raw(`SELECT count(*) FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname = current_schema() AND t.typname = 'abuse_report_resolver_expiresat_enum'`).Scan(&types).Error)
	assert.Zero(t, types, "表の enum 型も消す")
	// 消さないもの
	ok, err := hasColumns(db, "user", "signupReason")
	require.NoError(t, err)
	assert.True(t, ok)
	ok, err = hasTable(db, "note_history")
	require.NoError(t, err)
	assert.True(t, ok, "読まれなくなるだけのデータは残す")

	require.NoError(t, DropCherryPickLeftovers(ctx, db), "流し直しても通る")
	r := inspect(t, db)
	assert.Equal(t, CherryPickSupported, r.Status)
	assert.Zero(t, r.Unprocessed(), "消した後も check は通る")
}

// 素の Elythia の schema には、cleanup が消すものが無い。Elythia が同じ名前の列や表を
// 使い始めたら (本家の追従など)、cleanup が Elythia のものを消してしまうので、ここで落とす。
func TestCherryPickLeftovers_NotInElythia(t *testing.T) {
	left, err := CherryPickLeftovers(context.Background(), testDB)
	require.NoError(t, err)
	assert.Empty(t, left)
	var types int64
	require.NoError(t, testDB.Raw(`SELECT count(*) FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname = current_schema() AND t.typname = 'abuse_report_resolver_expiresat_enum'`).Scan(&types).Error)
	assert.Zero(t, types, "cleanup は型も無条件に消す")
	var n int64
	require.NoError(t, testDB.Raw(`SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = 'note'`).Scan(&n).Error)
	require.EqualValues(t, 1, n, "testDB は Elythia の migration を流した schema")
}

// serve が DB を開いたときの確認。CherryPick の DB で convert の印が無ければ true、印を付けた
// 後と CherryPick ではない DB では false。
func TestCherryPickNeedsConvert(t *testing.T) {
	ctx := context.Background()
	db := openCherryPickDB(t)
	needs, err := CherryPickNeedsConvert(ctx, db)
	require.NoError(t, err)
	assert.True(t, needs, "変換していない CherryPick の DB")

	require.NoError(t, MarkCherryPickConverted(ctx, db))
	needs, err = CherryPickNeedsConvert(ctx, db)
	require.NoError(t, err)
	assert.False(t, needs, "変換した後")

	require.NoError(t, db.Exec(`DELETE FROM migrations`).Error)
	needs, err = CherryPickNeedsConvert(ctx, db)
	require.NoError(t, err)
	assert.False(t, needs, "CherryPick の記録が無い DB")

}

// CherryPickNeedsConvert reads the primary even when the pool routes SELECTs
// to a replica. A lagging replica that does not have the CherryPick records
// yet must not let serve start on an unconverted database.
func TestCherryPickNeedsConvert_ReadsPrimary(t *testing.T) {
	ctx := context.Background()
	db := openCherryPickDB(t)
	var primarySchema string
	require.NoError(t, db.Raw("SELECT current_schema()").Scan(&primarySchema).Error)
	// レプリカの遅れを空の schema で模す (migrations の表がまだ届いていない)。
	replicaSchema := primarySchema + "_replica"
	require.NoError(t, db.Exec(`DROP SCHEMA IF EXISTS "`+replicaSchema+`" CASCADE`).Error)
	require.NoError(t, db.Exec(`CREATE SCHEMA "`+replicaSchema+`"`).Error)
	t.Cleanup(func() { db.Exec(`DROP SCHEMA IF EXISTS "` + replicaSchema + `" CASCADE`) })

	gdb, err := gorm.Open(postgres.New(postgres.Config{
		DSN: testutil.TestDSNForSchema(primarySchema), PreferSimpleProtocol: true,
	}), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	require.NoError(t, err)
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, gdb.Use(dbresolver.Register(dbresolver.Config{
		Replicas: []gorm.Dialector{postgres.New(postgres.Config{
			DSN: testutil.TestDSNForSchema(replicaSchema), PreferSimpleProtocol: true,
		})},
		Policy: dbresolver.RandomPolicy{},
	})))

	// 前提: 素の SELECT はレプリカを読み、migrations の表が見えない。
	var tables int64
	require.NoError(t, gdb.Raw(`SELECT count(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = 'migrations'`).Scan(&tables).Error)
	require.Zero(t, tables, "the plain SELECT reads the replica")

	needs, err := CherryPickNeedsConvert(ctx, gdb)
	require.NoError(t, err)
	assert.True(t, needs, "the primary holds the unconverted CherryPick database")

	require.NoError(t, MarkCherryPickConverted(ctx, db))
	needs, err = CherryPickNeedsConvert(ctx, gdb)
	require.NoError(t, err)
	assert.False(t, needs, "the marker on the primary is seen at once")
}
