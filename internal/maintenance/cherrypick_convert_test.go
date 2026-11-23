package maintenance

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
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
		require.Equal(t, 5, inspect(t, db).Unprocessed())
		// リモートの予約削除は「残す」の対象にしない。
		require.NoError(t, db.Exec(`INSERT INTO note (id, "userId", "userHost", visibility, "deleteAt")
			VALUES ('n8', 'a1', 'remote.example', 'public', ?)`, cherryPickNow.Add(time.Hour)).Error)

		require.NoError(t, ApplyCherryPickSQL(ctx, db, CherryPickPlan{
			PendingUserIDs: []string{"p1", "p2"}, PendingAction: CherryPickSuspend,
			QuarantinedHosts: []string{"existing.example", "q.example"}, QuarantineAction: CherryPickBlock,
			KeepLaterDeletes: true, DeleteResolvers: true,
			RepositoryURL: "https://github.com/kokonect-link/cherrypick", ElythiaRepositoryURL: "https://elythia.example/repo",
			FeedbackURL: "https://github.com/kokonect-link/cherrypick/issues/new", ElythiaFeedbackURL: "https://elythia.example/repo/issues/new",
			Now: cherryPickNow,
		}))

		r := inspect(t, db)
		assert.Empty(t, r.PendingUsers)
		assert.Zero(t, r.AbuseReportResolvers)
		assert.Empty(t, r.QuarantinedHosts)
		assert.Empty(t, r.RepositoryURL)
		assert.Empty(t, r.FeedbackURL)
		assert.Zero(t, r.ScheduledDeletesLater)
		assert.EqualValues(t, 1, r.ScheduledDeletesDue, "期限切れは本体の削除で消すので、ここでは残る")
		assert.Equal(t, []string{"Existing.example", "q.example"}, blockedHosts(t, db), "大文字小文字が違うだけのものは足さない")

		var suspended int64
		require.NoError(t, db.Raw(`SELECT count(*) FROM "user" WHERE "isSuspended" AND id IN ('p1', 'p2')`).Scan(&suspended).Error)
		assert.EqualValues(t, 2, suspended)
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
			QuarantinedHosts: []string{"q.example"}, QuarantineAction: CherryPickLift,
			Now: cherryPickNow,
		}))
		r := inspect(t, db)
		require.Len(t, r.PendingUsers, 1)
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
			PendingUserIDs: []string{"p1", "p2"}, PendingAction: CherryPickDelete, Now: cherryPickNow,
		}))
		assert.Len(t, inspect(t, db).PendingUsers, 2)
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
