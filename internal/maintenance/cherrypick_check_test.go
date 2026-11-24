package maintenance

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/testutil"
)

// openCherryPickDB recreates the sibling schema `internal_maintenance_cherrypick`
// and loads the CherryPick 4.18.0-beta.1 schema into it.
//
// 毎回作り直す。前の実行の行や、列を落としたテストの形が残ると、別の理由で落ちる。
func openCherryPickDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := testutil.OpenTestDBSchema("cherrypick")
	require.NoError(t, err)
	var schema string
	require.NoError(t, db.Raw("SELECT current_schema()").Scan(&schema).Error)
	// 名前そのものを要求する。search_path が効いていないと current_schema() が
	// public を返し、直後の DROP SCHEMA が共有の public を落とす。
	require.Equal(t, "internal_maintenance_cherrypick", schema)
	require.NoError(t, db.Exec(`DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`).Error)
	require.NoError(t, db.Exec(`CREATE SCHEMA "`+schema+`"`).Error)
	require.NoError(t, testutil.LoadCherryPickSchema(db))
	return db
}

var cherryPickNow = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

// requireApproval turns on CherryPick's approval for signup. CherryPick は
// meta の行を起動したときに作る (fixture には行が無い)。
//
// repositoryURL が CherryPick のものなら、feedbackUrl も CherryPick の既定のまま残す。
// そうでなければ feedbackUrl を空にして、リンクの項目を数えないようにする。
func requireApproval(t *testing.T, db *gorm.DB, repositoryURL string) {
	t.Helper()
	require.NoError(t, db.Exec(`INSERT INTO meta (id, "approvalRequiredForSignup", "repositoryUrl") VALUES ('x', true, ?)`, repositoryURL).Error)
	if !isCherryPickRepository(repositoryURL) {
		require.NoError(t, db.Exec(`UPDATE meta SET "feedbackUrl" = NULL`).Error)
	}
}

func inspect(t *testing.T, db *gorm.DB) CherryPickReport {
	t.Helper()
	r, err := InspectCherryPick(context.Background(), db, cherryPickNow)
	require.NoError(t, err)
	return r
}

func TestInspectCherryPick_FreshDatabase(t *testing.T) {
	r := inspect(t, openCherryPickDB(t))
	assert.Equal(t, CherryPickSupported, r.Status, r.Reason)
	assert.Zero(t, r.Unprocessed())
	assert.Empty(t, r.PendingUsers)
	assert.Empty(t, r.QuarantinedHosts)
	assert.Empty(t, r.RepositoryURL)
}

func TestInspectCherryPick_CountsWhatElythiaDrops(t *testing.T) {
	db := openCherryPickDB(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		require.NoError(t, db.Exec(sql, args...).Error)
	}
	user := func(id, name string, host *string, approved, suspended, deleted bool, reason *string) {
		t.Helper()
		exec(`INSERT INTO "user" (id, username, "usernameLower", host, approved, "isSuspended", "isDeleted", "signupReason")
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, name, name, host, approved, suspended, deleted, reason)
	}
	reason := "参加したいです"
	remote := "remote.example"
	user("u1", "carol", nil, false, false, false, &reason)
	user("u2", "dave", nil, false, false, false, nil)
	// 数えないもの: 承認済み・凍結済み・削除済み・リモート・システムアカウント
	user("u3", "alice", nil, true, false, false, nil)
	user("u4", "erin", nil, false, true, false, nil)
	user("u5", "frank", nil, false, false, true, nil)
	user("u6", "grace", &remote, false, false, false, nil)
	user("u7", "instance.actor", nil, false, false, false, nil)
	exec(`INSERT INTO system_account (id, "userId", type) VALUES ('s1', 'u7', 'actor')`)
	exec(`UPDATE "user" SET "autoDeleteNotesAfterDays" = 30 WHERE id IN ('u1', 'u6')`)

	exec(`INSERT INTO abuse_report_resolver (id, "updatedAt", name, "expiresAt", forward)
		VALUES ('r1', now(), 'spam', 'indefinitely', false)`)
	exec(`INSERT INTO instance (id, "firstRetrievedAt", host, "quarantineLimited") VALUES
		('i1', now(), 'b.example', true), ('i2', now(), 'a.example', true), ('i3', now(), 'c.example', false)`)
	note := func(id string, host *string, deleteAt *time.Time) {
		t.Helper()
		exec(`INSERT INTO note (id, "userId", "userHost", visibility, "deleteAt") VALUES (?, 'u3', ?, 'public', ?)`, id, host, deleteAt)
	}
	past, future := cherryPickNow.Add(-time.Hour), cherryPickNow.Add(time.Hour)
	note("n1", nil, &past)
	note("n2", nil, &cherryPickNow)
	note("n3", nil, &future)
	note("n4", nil, nil)
	note("n5", &remote, &past)
	exec(`INSERT INTO note_draft (id, "userId", visibility, "pollMultiple", "deleteAt") VALUES
		('d1', 'u3', 'public', false, now()), ('d2', 'u3', 'public', false, NULL)`)
	exec(`INSERT INTO note_history (id, "noteId", "updatedAt", "userId", visibility, "createdAt")
		VALUES ('h1', 'n4', now(), 'u3', 'public', now())`)
	exec(`INSERT INTO event ("noteId", start, title, "noteVisibility", "userId")
		VALUES ('n4', now(), 'tea', 'public', 'u3')`)
	requireApproval(t, db, "https://github.com/kokonect-link/cherrypick")

	r := inspect(t, db)
	require.Equal(t, CherryPickSupported, r.Status, r.Reason)
	assert.Equal(t, []CherryPickPendingUser{{ID: "u1", Username: "carol", SignupReason: &reason}}, r.PendingUsers,
		"理由の無い dave は申請した人ではない")
	assert.EqualValues(t, 1, r.UnapprovedMembers, "dave")
	assert.EqualValues(t, 1, r.AbuseReportResolvers)
	assert.Equal(t, []string{"a.example", "b.example"}, r.QuarantinedHosts)
	assert.EqualValues(t, 2, r.ScheduledDeletesDue, "期限ちょうどは期限切れに数える。リモートは数えない")
	assert.EqualValues(t, 1, r.ScheduledDeletesLater)
	assert.Equal(t, "https://github.com/kokonect-link/cherrypick", r.RepositoryURL)
	assert.Equal(t, "https://github.com/kokonect-link/cherrypick/issues/new", r.FeedbackURL, "CherryPick の列の既定値")
	assert.EqualValues(t, 1, r.NoteHistories)
	assert.EqualValues(t, 1, r.Events)
	assert.EqualValues(t, 1, r.RemoteScheduledDeletes)
	assert.EqualValues(t, 1, r.AutoDeleteUsers, "ローカルだけ")
	assert.EqualValues(t, 1, r.DraftScheduledDeletes)
	assert.Equal(t, 6, r.Unprocessed(), "理由の無い dave も要対応")
}

// 承認制が無効なら、approved が false でも CherryPick はサインインさせる (その場で
// true に書き換える)。既存の利用者は false のまま残っているので、数えてはいけない。
func TestInspectCherryPick_ApprovalOff(t *testing.T) {
	db := openCherryPickDB(t)
	require.NoError(t, db.Exec(`INSERT INTO "user" (id, username, "usernameLower") VALUES ('u1', 'alice', 'alice')`).Error)
	require.NoError(t, db.Exec(`INSERT INTO "user" (id, username, "usernameLower", "signupReason") VALUES ('u2', 'carol', 'carol', 'hi')`).Error)
	for _, step := range []string{"meta の行が無い", "承認制が既定の無効"} {
		r := inspect(t, db)
		assert.Empty(t, r.PendingUsers, step)
		assert.Zero(t, r.UnapprovedMembers, step)
		require.NoError(t, db.Exec(`INSERT INTO meta (id) VALUES ('x') ON CONFLICT DO NOTHING`).Error)
	}
	require.NoError(t, db.Exec(`UPDATE meta SET "approvalRequiredForSignup" = true`).Error)
	r := inspect(t, db)
	require.Len(t, r.PendingUsers, 1, "approved の既定は false")
	assert.Equal(t, "carol", r.PendingUsers[0].Username)
	assert.EqualValues(t, 1, r.UnapprovedMembers, "承認制の前からの alice")
}

// convert が行の状態を変えれば、その項目は数えられなくなる。
func TestInspectCherryPick_ResolvedRowsAreNotCounted(t *testing.T) {
	db := openCherryPickDB(t)
	require.NoError(t, db.Exec(`INSERT INTO "user" (id, username, "usernameLower", approved, "signupReason") VALUES ('u1', 'carol', 'carol', false, 'hi')`).Error)
	requireApproval(t, db, "https://github.com/elythia-network/elythia")
	require.Equal(t, 1, inspect(t, db).Unprocessed())

	require.NoError(t, db.Exec(`UPDATE "user" SET "isSuspended" = true`).Error)
	r := inspect(t, db)
	assert.Empty(t, r.PendingUsers)
	assert.Empty(t, r.RepositoryURL)
	assert.Empty(t, r.FeedbackURL)
	assert.Zero(t, r.Unprocessed())
}

// feedbackUrl だけが CherryPick を指していても、リンクの項目として数える。
func TestInspectCherryPick_FeedbackURLOnly(t *testing.T) {
	db := openCherryPickDB(t)
	require.NoError(t, db.Exec(`INSERT INTO meta (id, "repositoryUrl") VALUES ('x', 'https://github.com/elythia-network/elythia')`).Error)
	r := inspect(t, db)
	assert.Empty(t, r.RepositoryURL)
	assert.Equal(t, "https://github.com/kokonect-link/cherrypick/issues/new", r.FeedbackURL)
	assert.Equal(t, 1, r.Unprocessed())
}

func TestIsCherryPickFeedback(t *testing.T) {
	for u, want := range map[string]bool{
		"https://github.com/kokonect-link/cherrypick/issues/new":     true,
		"https://github.com/kokonect-link/cherrypick/issues/new/":    true,
		"https://GitHub.com/Kokonect-Link/CherryPick/issues/new":     true,
		"https://github.com/kokonect-link/cherrypick":                false,
		"https://github.com/Elythia-Network/elythia/issues/new":      false,
		"https://gitlab.example/kokonect-link/cherrypick/issues/new": false,
		"": false,
	} {
		assert.Equal(t, want, isCherryPickFeedback(u), u)
	}
}

// 後始末 (cleanup) で列と表を消した後も、判定が通って 0 件になる。
func TestInspectCherryPick_AfterCleanup(t *testing.T) {
	db := openCherryPickDB(t)
	for _, sql := range []string{
		`ALTER TABLE "user" DROP COLUMN approved`,
		`ALTER TABLE "user" DROP COLUMN "signupReason"`,
		`ALTER TABLE instance DROP COLUMN "quarantineLimited"`,
		`ALTER TABLE note DROP COLUMN "deleteAt"`,
		`ALTER TABLE note_draft DROP COLUMN "deleteAt"`,
		`ALTER TABLE "user" DROP COLUMN "autoDeleteNotesAfterDays"`,
		`ALTER TABLE meta DROP COLUMN "approvalRequiredForSignup"`,
		`DROP TABLE abuse_report_resolver`,
		`DROP TABLE note_history`,
		`DROP TABLE event`,
	} {
		require.NoError(t, db.Exec(sql).Error, sql)
	}
	r := inspect(t, db)
	assert.Equal(t, CherryPickSupported, r.Status, r.Reason)
	assert.Zero(t, r.Unprocessed())
}

// 申請の理由の列が無ければ、申請した人と見分けられないので、承認されていない
// 利用者として数える (要対応)。
func TestInspectCherryPick_WithoutSignupReason(t *testing.T) {
	db := openCherryPickDB(t)
	require.NoError(t, db.Exec(`ALTER TABLE "user" DROP COLUMN "signupReason"`).Error)
	require.NoError(t, db.Exec(`INSERT INTO "user" (id, username, "usernameLower", approved) VALUES ('u9', 'carol', 'carol', false)`).Error)
	requireApproval(t, db, "")
	r := inspect(t, db)
	assert.Empty(t, r.PendingUsers)
	assert.EqualValues(t, 1, r.UnapprovedMembers)
	assert.Equal(t, 1, r.Unprocessed())
}

// 理由の無い承認されていない利用者 (承認制にした後でメールの確認を済ませた申請者を
// 含む) は、Elythia ではサインインできてしまうので要対応に数える。選ぶまでは、
// migrate の前の確認が変換済みの印を自分で付けない。
func TestInspectCherryPick_UnapprovedMembersNeedAction(t *testing.T) {
	ctx := context.Background()
	db := openCherryPickDB(t)
	require.NoError(t, db.Exec(`INSERT INTO "user" (id, username, "usernameLower", approved) VALUES ('u1', 'erin', 'erin', false)`).Error)
	requireApproval(t, db, "")
	require.NoError(t, db.Exec(`CREATE TABLE schema_migrations (version bigint PRIMARY KEY, dirty boolean NOT NULL)`).Error)
	require.NoError(t, db.Exec(`INSERT INTO schema_migrations VALUES (1, false)`).Error)

	r, err := InspectCherryPickBeforeMigrate(ctx, db, cherryPickNow)
	require.NoError(t, err)
	assert.EqualValues(t, 1, r.UnapprovedMembers)
	assert.Equal(t, 1, r.Unprocessed())
	assert.False(t, r.Converted, "選ぶまでは印を付けない")
	converted, err := CherryPickConverted(ctx, db)
	require.NoError(t, err)
	assert.False(t, converted)
}

// 期限前の予約削除だけでも、要対応に数える。
func TestInspectCherryPick_OnlyLaterScheduledDeletes(t *testing.T) {
	db := openCherryPickDB(t)
	require.NoError(t, db.Exec(`INSERT INTO "user" (id, username, "usernameLower", approved) VALUES ('u1', 'alice', 'alice', true)`).Error)
	require.NoError(t, db.Exec(`INSERT INTO note (id, "userId", visibility, "deleteAt") VALUES ('n1', 'u1', 'public', ?)`,
		cherryPickNow.Add(time.Hour)).Error)
	r := inspect(t, db)
	assert.Zero(t, r.ScheduledDeletesDue)
	assert.EqualValues(t, 1, r.ScheduledDeletesLater)
	assert.Equal(t, 1, r.Unprocessed())
}

// 版を確かめるところで失敗したら、CherryPickStatusError を返す。
func TestInspectCherryPick_StatusError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := openCherryPickDB(t)
	for _, f := range []func(context.Context, *gorm.DB, time.Time) (CherryPickReport, error){InspectCherryPick, InspectCherryPickBeforeMigrate} {
		_, err := f(ctx, db, cherryPickNow)
		var statusErr *CherryPickStatusError
		require.ErrorAs(t, err, &statusErr)
		assert.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, statusErr.Err.Error(), err.Error())
	}
}

func TestInspectCherryPick_Versions(t *testing.T) {
	t.Run("older CherryPick", func(t *testing.T) {
		db := openCherryPickDB(t)
		require.NoError(t, db.Exec(`DELETE FROM migrations WHERE name = 'ApprovalSignup1768893580518'`).Error)
		r := inspect(t, db)
		assert.Equal(t, CherryPickUnsupported, r.Status)
		assert.Contains(t, r.Reason, "47 of the 48")
		assert.Equal(t, 1, r.Unprocessed())
	})
	t.Run("newer CherryPick", func(t *testing.T) {
		db := openCherryPickDB(t)
		require.NoError(t, db.Exec(`INSERT INTO migrations ("timestamp", name) VALUES (1770000000000, 'Newer1770000000000')`).Error)
		r := inspect(t, db)
		assert.Equal(t, CherryPickUnsupported, r.Status)
		assert.Contains(t, r.Reason, "newer than CherryPick 4.18.0-beta.1")
	})
	t.Run("after Elythia migrated it", func(t *testing.T) {
		// Elythia は本家の新しい記録を足すので、流した後は最新の記録を見ない。
		db := openCherryPickDB(t)
		require.NoError(t, db.Exec(`INSERT INTO migrations ("timestamp", name) VALUES (1794385801836, 'FixMetaMigratedConfigColumnDefaults1794385801836')`).Error)
		require.NoError(t, db.Exec(`CREATE TABLE schema_migrations (version bigint PRIMARY KEY, dirty boolean NOT NULL)`).Error)
		require.NoError(t, db.Exec(`INSERT INTO schema_migrations VALUES (1, false)`).Error)
		assert.Equal(t, CherryPickSupported, inspect(t, db).Status)
	})
	t.Run("newer CherryPick with an empty schema_migrations", func(t *testing.T) {
		// -force や down を先に当てると、golang-migrate は空の表だけを作る。流したことに
		// ならないので、版はまだ見分けられる。
		db := openCherryPickDB(t)
		require.NoError(t, db.Exec(`INSERT INTO migrations ("timestamp", name) VALUES (1770000000000, 'Newer1770000000000')`).Error)
		require.NoError(t, db.Exec(`CREATE TABLE schema_migrations (version bigint PRIMARY KEY, dirty boolean NOT NULL)`).Error)
		assert.Equal(t, CherryPickUnsupported, inspect(t, db).Status)
	})
}

func TestInspectCherryPick_NotCherryPick(t *testing.T) {
	t.Run("Elythia", func(t *testing.T) {
		// testDB は Elythia の migration を流した schema。本家の記録を持つ。
		var n int64
		require.NoError(t, testDB.Raw(`SELECT count(*) FROM migrations`).Scan(&n).Error)
		require.Positive(t, n)
		r := inspect(t, testDB)
		assert.Equal(t, NotCherryPick, r.Status)
		assert.Zero(t, r.Unprocessed())
	})
	t.Run("Misskey", func(t *testing.T) {
		// TypeORM が作った本家の DB の形: 本家の記録だけを持ち、schema_migrations が無い。
		db := openCherryPickDB(t)
		require.NoError(t, db.Exec(`DELETE FROM migrations WHERE name IN ?`, cherryPickMigrations).Error)
		assert.Equal(t, NotCherryPick, inspect(t, db).Status)
	})
	t.Run("no migrations table", func(t *testing.T) {
		db := openCherryPickDB(t)
		require.NoError(t, db.Exec(`DROP TABLE migrations`).Error)
		assert.Equal(t, NotCherryPick, inspect(t, db).Status)
	})
}

func TestIsCherryPickRepository(t *testing.T) {
	for u, want := range map[string]bool{
		"https://github.com/kokonect-link/cherrypick":      true,
		"https://github.com/kokonect-link/cherrypick/":     true,
		"https://github.com/kokonect-link/cherrypick.git":  true,
		"https://GitHub.com/Kokonect-Link/CherryPick":      true,
		" https://github.com/kokonect-link/cherrypick ":    true,
		"https://github.com/elythia-network/elythia":       false,
		"https://github.com/kokonect-link/cherrypick-fork": false,
		"https://gitlab.example/kokonect-link/cherrypick":  false,
		"://broken": false,
		"":          false,
	} {
		assert.Equal(t, want, isCherryPickRepository(u), u)
	}
}

// migrate の前の確認は、Elythia が流した後も convert が済むまで数え、済んだら
// 数えない (#4248)。
func TestInspectCherryPickBeforeMigrate(t *testing.T) {
	db := openCherryPickDB(t)
	require.NoError(t, db.Exec(`INSERT INTO "user" (id, username, "usernameLower", approved, "signupReason") VALUES ('u1', 'carol', 'carol', false, 'hi')`).Error)
	requireApproval(t, db, "")

	r, err := InspectCherryPickBeforeMigrate(context.Background(), db, cherryPickNow)
	require.NoError(t, err)
	assert.False(t, r.Converted)
	assert.False(t, r.Migrated)
	assert.Len(t, r.PendingUsers, 1)

	require.NoError(t, db.Exec(`CREATE TABLE schema_migrations (version bigint PRIMARY KEY, dirty boolean NOT NULL)`).Error)
	require.NoError(t, db.Exec(`INSERT INTO schema_migrations VALUES (1, false)`).Error)
	r, err = InspectCherryPickBeforeMigrate(context.Background(), db, cherryPickNow)
	require.NoError(t, err)
	assert.False(t, r.Converted)
	assert.True(t, r.Migrated)
	assert.Len(t, r.PendingUsers, 1, "流した後も、convert が済むまでは数える")

	require.NoError(t, MarkCherryPickConverted(context.Background(), db))
	r, err = InspectCherryPickBeforeMigrate(context.Background(), db, cherryPickNow)
	require.NoError(t, err)
	assert.Equal(t, CherryPickReport{Converted: true}, r)
	full := inspect(t, db)
	assert.True(t, full.Converted)
	assert.Len(t, full.PendingUsers, 1, "check は済んだ後も数える")
}

// 接続文字列から繋いで数える経路。繋げなければ CherryPickStatusError を返す。
func TestInspectCherryPickDSN(t *testing.T) {
	db := openCherryPickDB(t)
	require.NoError(t, db.Exec(`INSERT INTO "user" (id, username, "usernameLower", approved, "signupReason") VALUES ('u1', 'carol', 'carol', false, 'hi')`).Error)
	requireApproval(t, db, "")

	r, err := InspectCherryPickDSN(testutil.TestDSNForSchema("internal_maintenance_cherrypick"), cherryPickNow, InspectCherryPick)
	require.NoError(t, err)
	assert.Equal(t, CherryPickSupported, r.Status, r.Reason)
	require.Len(t, r.PendingUsers, 1)

	_, err = InspectCherryPickDSN("host=127.0.0.1 port=1 user=x dbname=x sslmode=disable connect_timeout=1", cherryPickNow, InspectCherryPick)
	var statusErr *CherryPickStatusError
	assert.ErrorAs(t, err, &statusErr)
}

// Elythia が流した後で要対応が無ければ、migrate の前の確認が印を付ける。流す前は付けない。
func TestInspectCherryPickBeforeMigrate_MarksWhenNothingToDo(t *testing.T) {
	ctx := context.Background()
	db := openCherryPickDB(t)
	r, err := InspectCherryPickBeforeMigrate(ctx, db, cherryPickNow)
	require.NoError(t, err)
	assert.False(t, r.Converted, "流す前は付けない")
	converted, err := CherryPickConverted(ctx, db)
	require.NoError(t, err)
	assert.False(t, converted)

	require.NoError(t, db.Exec(`CREATE TABLE schema_migrations (version bigint PRIMARY KEY, dirty boolean NOT NULL)`).Error)
	require.NoError(t, db.Exec(`INSERT INTO schema_migrations VALUES (1, false)`).Error)
	r, err = InspectCherryPickBeforeMigrate(ctx, db, cherryPickNow)
	require.NoError(t, err)
	assert.True(t, r.Converted)
	converted, err = CherryPickConverted(ctx, db)
	require.NoError(t, err)
	assert.True(t, converted)
}
