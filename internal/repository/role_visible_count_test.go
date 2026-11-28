package repository

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/elythia-network/elythia/internal/model"
)

const hiddenUserIndex = "IDX_user_hidden_id"

// TestCountVisibleByRole_MatchesListVisibleByRole checks, over every
// combination of the user states the two conditions look at, that
// CountVisibleByRole counts exactly the assignments ListVisibleByRole returns
// (#4263).
//
// 一覧は visibleAssigneeCond、人数は hiddenUserCond の引き算で、2 つの条件は
// 別の定数になっている。片方だけを直すと roles/users の一覧と usersCount の数が
// ずれるので、凍結・削除済み・host (NULL / 空文字 / リモート)・期限 (なし /
// 先 / 切れ) の組み合わせを全部置き、両方の数と、Go で書いた期待の数を比べる。
func TestCountVisibleByRole_MatchesListVisibleByRole(t *testing.T) {
	roleRepo := NewRoleRepository(testDB)
	assignRepo := NewRoleAssignmentRepository(testDB)

	now := time.Now()
	role := &model.Role{
		ID: "role_vcm_test", UpdatedAt: now, LastUsedAt: now, Name: "VCM",
		Target: model.RoleTargetManual, Policies: datatypes.JSON([]byte("{}")),
		CondFormula: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, roleRepo.Create(role))
	defer cleanupRole(t, role.ID)

	future, past := now.Add(time.Hour), now.Add(-time.Hour)
	hosts := []struct {
		name, sql string
		remote    bool
	}{
		{"null", `NULL`, false},
		{"empty", `''`, false},
		{"remote", `'remote.example'`, true},
	}
	expiries := []struct {
		name   string
		at     *time.Time
		active bool
	}{
		{"none", nil, true},
		{"future", &future, true},
		{"past", &past, false},
	}
	want, n := 0, 0
	for _, suspended := range []bool{false, true} {
		for _, deleted := range []bool{false, true} {
			for _, h := range hosts {
				for _, e := range expiries {
					n++
					uid := fmt.Sprintf("vcm_u%02d", n)
					createTestUser(t, uid)
					require.NoError(t, testDB.Exec(
						`UPDATE "user" SET "isSuspended" = ?, "isDeleted" = ?, host = `+h.sql+` WHERE id = ?`,
						suspended, deleted, uid).Error)
					require.NoError(t, assignRepo.Create(&model.RoleAssignment{
						ID: fmt.Sprintf("vcm_a%02d", n), UserID: uid, RoleID: role.ID, ExpiresAt: e.at,
					}), "%s: suspended=%v deleted=%v host=%s expiry=%s", uid, suspended, deleted, h.name, e.name)
					if e.active && !suspended && (h.remote || !deleted) {
						want++
					}
				}
			}
		}
	}
	require.Equal(t, 36, n, "組み合わせを全部置けていない")
	require.Equal(t, 8, want, "期待の数の数え方が変わった")

	listed, err := assignRepo.ListVisibleByRole(role.ID, "", "", 0)
	require.NoError(t, err)
	counted, err := assignRepo.CountVisibleByRole(role.ID)
	require.NoError(t, err)
	assert.Len(t, listed, want, "ListVisibleByRole (visibleAssigneeCond) が期待とずれる")
	assert.Equal(t, want, counted, "CountVisibleByRole (hiddenUserCond) が期待とずれる")
	assert.Equal(t, len(listed), counted, "一覧の件数と人数がずれる")
}

// TestCountVisibleByRole_UsesHiddenUserIndex checks that the hidden side of
// CountVisibleByRole reads "user" through IDX_user_hidden_id, also under a
// generic plan (#4263).
//
// hiddenUserCond の形が 000150 の述語とずれると、planner は部分 index を使えず、
// 数えるたびに "user" の全行を読む形に戻る。共有の schema では表がほぼ空で、
// planner が主キーから引く計画を選びうるので、専用の schema で "user" の他の index
// (主キーを含む) を外し、使える index をこれだけにして確かめる。使えなければ
// Seq Scan on "user" になる。
func TestCountVisibleByRole_UsesHiddenUserIndex(t *testing.T) {
	db, m := openPerfIndexSchema(t, "rolehidden")
	require.NoError(t, runMigrate(func() error { return m.Migrate(150) }))

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
		  AND tc.relname = 'user' AND ic.relname <> ?`, hiddenUserIndex).Scan(&others).Error)
	require.NotEmpty(t, others, "外す index を拾えていない")
	for _, o := range others {
		stmt := `DROP INDEX "` + o.Index + `"`
		if o.Constraint != nil {
			// 主キーは他の表の FK が参照しているので、FK ごと外す (専用の schema なので構わない)。
			stmt = `ALTER TABLE "user" DROP CONSTRAINT "` + *o.Constraint + `" CASCADE`
		}
		require.NoError(t, db.Exec(stmt).Error, stmt)
	}

	rec := &sqlRecorder{Interface: logger.Discard}
	repo := NewRoleAssignmentRepository(db.Session(&gorm.Session{Logger: rec}))
	n, err := repo.CountVisibleByRole("role_hidden_none")
	require.NoError(t, err)
	assert.Zero(t, n)

	rec.mu.Lock()
	sql, vars := rec.sql, rec.vars
	rec.mu.Unlock()
	require.Contains(t, sql, `JOIN "user" u`, "CountVisibleByRole の SQL を拾えていない")
	require.NotEmpty(t, vars)

	placeholders := make([]string, len(vars))
	for i := range vars {
		placeholders[i] = "?"
	}
	var lines []string
	require.NoError(t, db.Connection(func(conn *gorm.DB) error {
		defer conn.Exec(`DEALLOCATE gp_role_visible_count`) // PREPARE 前に落ちたときのエラーは無視する
		tx := conn.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		for _, s := range []string{
			`SET LOCAL plan_cache_mode = force_generic_plan`,
			`SET LOCAL enable_seqscan = off`,
			`PREPARE gp_role_visible_count AS ` + sql,
		} {
			require.NoError(t, tx.Exec(s).Error, s)
		}
		return tx.Raw(`EXPLAIN EXECUTE gp_role_visible_count(`+strings.Join(placeholders, ", ")+`)`, vars...).Scan(&lines).Error
	}))
	plan := strings.Join(lines, "\n")
	assert.Contains(t, plan, hiddenUserIndex,
		"generic plan で部分 index が使われない (hiddenUserCond を 000150 の述語と揃えているか):\n%s", plan)
	assert.NotContains(t, plan, `Seq Scan on "user"`, "数える文が \"user\" を全部読む:\n%s", plan)
}

// TestMigration150_UserHiddenIndex checks that 000150 creates a valid partial
// index with hiddenUserCond's predicate and that down removes it (#4263).
func TestMigration150_UserHiddenIndex(t *testing.T) {
	db, m := openPerfIndexSchema(t, "migrate150")

	require.NoError(t, runMigrate(func() error { return m.Migrate(150) }))
	state := perfIndexState(t, db)
	valid, ok := state[hiddenUserIndex]
	require.True(t, ok, "%s が無い", hiddenUserIndex)
	assert.True(t, valid, "%s が INVALID のまま", hiddenUserIndex)
	var def string
	require.NoError(t, db.Raw(`
		SELECT pg_get_indexdef(c.oid) FROM pg_class c
		WHERE c.relnamespace = to_regnamespace(current_schema()) AND c.relname = ?`, hiddenUserIndex).Scan(&def).Error)
	assert.Contains(t, def, `"user" USING btree (id) WHERE ("isSuspended" OR ("isDeleted" AND ((COALESCE(host, ''::character varying))::text = ''::text)))`)

	require.NoError(t, runMigrate(func() error { return m.Migrate(149) }))
	assert.NotContains(t, perfIndexState(t, db), hiddenUserIndex, "down が %s を落としていない", hiddenUserIndex)

	require.NoError(t, runMigrate(func() error { return m.Migrate(150) }))
	assert.True(t, perfIndexState(t, db)[hiddenUserIndex], "up → down → up で %s が戻らない", hiddenUserIndex)
}
