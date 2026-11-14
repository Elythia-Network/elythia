package repository

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestMigration142_DriveFileUserFolderIDIndex checks that 000142 creates the
// upstream (userId, folderId, id) index under the upstream name and that down
// removes it (#4164).
func TestMigration142_DriveFileUserFolderIDIndex(t *testing.T) {
	db, m := openPerfIndexSchema(t, "migrate142")
	const name = "IDX_55720b33a61a7c806a8215b825"

	require.NoError(t, runMigrate(func() error { return m.Migrate(142) }))
	state := perfIndexState(t, db)
	valid, ok := state[name]
	require.True(t, ok, "%s が無い", name)
	assert.True(t, valid, "%s が INVALID のまま", name)
	var def string
	require.NoError(t, db.Raw(`
		SELECT pg_get_indexdef(c.oid) FROM pg_class c
		WHERE c.relnamespace = to_regnamespace(current_schema()) AND c.relname = ?`, name).Scan(&def).Error)
	assert.Contains(t, def, `drive_file USING btree ("userId", "folderId", id)`)

	require.NoError(t, runMigrate(func() error { return m.Migrate(141) }))
	_, ok = perfIndexState(t, db)[name]
	assert.False(t, ok, "down で %s が消えない", name)
}

// TestDriveFileListByUser_RootReadsUserFolderIndexInOrder: the root listing
// (`"folderId" IS NULL`) orders by folderId too, so that it reads the
// (userId, folderId, id) index in order without sorting (#4164). Ordered by id
// alone, IS NULL is not taken as an equality and the index order cannot be
// used (upstream 2026.11.0 #18129).
//
// 他の index と主キーを外し、使える index をこれだけにして計画を見る (#4021 の
// テストと同じ理由。共有の schema では表がほぼ空で、planner の選び方が揺れる)。
func TestDriveFileListByUser_RootReadsUserFolderIndexInOrder(t *testing.T) {
	db, m := openPerfIndexSchema(t, "driveplan")
	const name = "IDX_55720b33a61a7c806a8215b825"
	require.NoError(t, runMigrate(func() error { return m.Migrate(142) }))

	var others []struct {
		Index      string
		Constraint *string
	}
	require.NoError(t, db.Raw(`
		SELECT ic.relname AS index, con.conname AS constraint
		FROM pg_index x
		JOIN pg_class ic ON ic.oid = x.indexrelid
		JOIN pg_class tc ON tc.oid = x.indrelid
		LEFT JOIN pg_constraint con ON con.conindid = x.indexrelid AND con.conrelid = tc.oid
		WHERE tc.relnamespace = to_regnamespace(current_schema())
		  AND tc.relname = 'drive_file' AND ic.relname <> ?`, name).Scan(&others).Error)
	require.NotEmpty(t, others, "外す index を拾えていない")
	for _, o := range others {
		stmt := `DROP INDEX "` + o.Index + `"`
		if o.Constraint != nil {
			stmt = `ALTER TABLE "drive_file" DROP CONSTRAINT "` + *o.Constraint + `" CASCADE`
		}
		require.NoError(t, db.Exec(stmt).Error, stmt)
	}

	for _, sort := range []string{"", "+createdAt", "-createdAt"} {
		t.Run("sort="+sort, func(t *testing.T) {
			rec := &sqlRecorder{Interface: logger.Discard}
			repo := NewDriveFileRepository(db.Session(&gorm.Session{Logger: rec}))
			_, err := repo.ListByUser("u_drive_none", nil, false, "", sort, "", "", 30)
			require.NoError(t, err)

			rec.mu.Lock()
			sql, vars := rec.sql, rec.vars
			rec.mu.Unlock()
			require.Contains(t, sql, `FROM "drive_file"`, "ListByUser の SQL を拾えていない")

			var lines []string
			require.NoError(t, db.Connection(func(conn *gorm.DB) error {
				tx := conn.Begin()
				require.NoError(t, tx.Error)
				defer tx.Rollback()
				require.NoError(t, tx.Exec(`SET LOCAL enable_seqscan = off`).Error)
				return tx.Raw(`EXPLAIN `+sql, vars...).Scan(&lines).Error
			}))
			plan := strings.Join(lines, "\n")
			assert.Contains(t, plan, name, "ルートの一覧が (userId, folderId, id) の index を使わない:\n%s", plan)
			assert.NotContains(t, plan, "Sort", "ルートの一覧が index の並びを使えず並べ替えている:\n%s", plan)
		})
	}
}
