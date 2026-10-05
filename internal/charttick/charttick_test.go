package charttick_test

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/charttick"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// skipIfNoTestDB skips when the test DB is not reachable, mirroring the
// pattern used by internal/db tests so local runs without a postgres
// container do not spuriously fail.
func skipIfNoTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := testutil.OpenTestDB()
	if err != nil {
		t.Skipf("test DB not available: %v", err)
	}
	testutil.ApplyMigrations(db)
	return db
}

// truncateChartTables clears the user/note/following/instance rows so each
// test starts from a known state. We DELETE rather than TRUNCATE because
// some FKs (instance.host) are referenced from other rows.
func truncateChartTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM "following"`,
		`DELETE FROM "instance"`,
		`DELETE FROM "note"`,
		`DELETE FROM "user"`,
	} {
		require.NoError(t, db.Exec(q).Error, q)
	}
}

func TestUsers_TickMinorIsNoop(t *testing.T) {
	db := skipIfNoTestDB(t)
	tick := charttick.Users(db)
	got, err := tick(context.Background(), "", false)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestUsers_TickMajorCountsLocalRemote(t *testing.T) {
	db := skipIfNoTestDB(t)
	truncateChartTables(t, db)

	// 2 local, 3 remote
	exec := func(q string) {
		require.NoError(t, db.Exec(q).Error, q)
	}
	exec(`INSERT INTO "user" ("id","username","usernameLower") VALUES ('u1', 'a','a')`)
	exec(`INSERT INTO "user" ("id","username","usernameLower") VALUES ('u2', 'b','b')`)
	exec(`INSERT INTO "user" ("id","username","usernameLower","host") VALUES ('u3', 'c','c','remote.example')`)
	exec(`INSERT INTO "user" ("id","username","usernameLower","host") VALUES ('u4', 'd','d','remote.example')`)
	exec(`INSERT INTO "user" ("id","username","usernameLower","host") VALUES ('u5', 'e','e','other.example')`)

	tick := charttick.Users(db)
	got, err := tick(context.Background(), "", true)
	require.NoError(t, err)
	assert.Equal(t, int64(2), got["local.total"])
	assert.Equal(t, int64(3), got["remote.total"])
}

func TestNotes_TickMajorCountsLocalRemote(t *testing.T) {
	db := skipIfNoTestDB(t)
	truncateChartTables(t, db)

	exec := func(q string) { require.NoError(t, db.Exec(q).Error, q) }
	// users for FK
	exec(`INSERT INTO "user" ("id","username","usernameLower") VALUES ('u1', 'a','a')`)
	exec(`INSERT INTO "user" ("id","username","usernameLower","host") VALUES ('u2', 'b','b','remote.example')`)
	// 1 local note, 2 remote
	exec(`INSERT INTO "note" ("id","userId","visibility") VALUES ('n1', 'u1', 'public')`)
	exec(`INSERT INTO "note" ("id","userId","userHost","visibility") VALUES ('n2', 'u2', 'remote.example', 'public')`)
	exec(`INSERT INTO "note" ("id","userId","userHost","visibility") VALUES ('n3', 'u2', 'remote.example', 'public')`)

	tick := charttick.Notes(db)
	got, err := tick(context.Background(), "", true)
	require.NoError(t, err)
	assert.Equal(t, int64(1), got["local.total"])
	assert.Equal(t, int64(2), got["remote.total"])
}

func TestNotes_TickMinorIsNoop(t *testing.T) {
	db := skipIfNoTestDB(t)
	tick := charttick.Notes(db)
	got, err := tick(context.Background(), "", false)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestFederation_TickMajorIsNoop(t *testing.T) {
	db := skipIfNoTestDB(t)
	tick := charttick.Federation(db)
	got, err := tick(context.Background(), "", true)
	require.NoError(t, err)
	assert.Nil(t, got)
}

// closedDB returns a *gorm.DB whose underlying SQL connection is closed,
// so any subsequent query returns an error. Useful for exercising the
// error branches of the TickFunc closures.
func closedDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := skipIfNoTestDB(t)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	return db
}

func TestUsers_TickMajor_DBErrorBubbles(t *testing.T) {
	db := closedDB(t)
	tick := charttick.Users(db)
	_, err := tick(context.Background(), "", true)
	assert.Error(t, err)
}

func TestNotes_TickMajor_DBErrorBubbles(t *testing.T) {
	db := closedDB(t)
	tick := charttick.Notes(db)
	_, err := tick(context.Background(), "", true)
	assert.Error(t, err)
}

func TestFederation_TickMinor_DBErrorBubbles(t *testing.T) {
	db := closedDB(t)
	tick := charttick.Federation(db)
	_, err := tick(context.Background(), "", false)
	assert.Error(t, err)
}

func TestFederation_TickMinorAggregates(t *testing.T) {
	db := skipIfNoTestDB(t)
	truncateChartTables(t, db)

	exec := func(q string) { require.NoError(t, db.Exec(q).Error, q) }
	// users
	exec(`INSERT INTO "user" ("id","username","usernameLower") VALUES ('me', 'me','me')`)
	exec(`INSERT INTO "user" ("id","username","usernameLower","host") VALUES ('a', 'a','a','a.example')`)
	exec(`INSERT INTO "user" ("id","username","usernameLower","host") VALUES ('b', 'b','b','b.example')`)
	exec(`INSERT INTO "user" ("id","username","usernameLower","host") VALUES ('c', 'c','c','c.example')`)

	// followings:
	//   me -> a (followee=a.example)  -> sub host: a.example
	//   me -> b (followee=b.example)  -> sub host: b.example
	//   c  -> me (follower=c.example) -> pub host: c.example
	//   a  -> me (follower=a.example) -> pub host: a.example  (これで a.example は pubsub)
	exec(`INSERT INTO "following" ("id","followerId","followeeId","followerHost","followeeHost") VALUES ('f1','me','a',NULL,'a.example')`)
	exec(`INSERT INTO "following" ("id","followerId","followeeId","followerHost","followeeHost") VALUES ('f2','me','b',NULL,'b.example')`)
	exec(`INSERT INTO "following" ("id","followerId","followeeId","followerHost","followeeHost") VALUES ('f3','c','me','c.example',NULL)`)
	exec(`INSERT INTO "following" ("id","followerId","followeeId","followerHost","followeeHost") VALUES ('f4','a','me','a.example',NULL)`)

	// b.example を suspend
	exec(`INSERT INTO "instance" ("id","host","firstRetrievedAt","suspensionState") VALUES ('i1','b.example', NOW(), 'manuallySuspended')`)

	tick := charttick.Federation(db)
	got, err := tick(context.Background(), "", false)
	require.NoError(t, err)

	// sub: a.example, b.example -> 2
	assert.Equal(t, int64(2), got["sub"])
	// pub: c.example, a.example -> 2
	assert.Equal(t, int64(2), got["pub"])
	// pubsub: a.example のみ -> 1
	assert.Equal(t, int64(1), got["pubsub"])
	// subActive: a.example (b.example suspended) -> 1
	assert.Equal(t, int64(1), got["subActive"])
	// pubActive: a.example, c.example (どちらも non-suspended) -> 2
	assert.Equal(t, int64(2), got["pubActive"])
}
