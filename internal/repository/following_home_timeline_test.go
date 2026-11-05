package repository

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/elythia-network/elythia/internal/model"
)

func followingIDs(rows []*model.Following) []string {
	ids := make([]string, 0, len(rows))
	for _, f := range rows {
		ids = append(ids, f.ID)
	}
	return ids
}

// TestListHomeTimelineFollowers_OnlyLocalNonHibernated checks that the fanout
// reads only the followers it delivers to, newest first, and that the offset
// pages over those rows alone (#4021).
func TestListHomeTimelineFollowers_OnlyLocalNonHibernated(t *testing.T) {
	repo := NewFollowingRepository(testDB)
	author := insertTestUser(t, "u_htf_author", "htfauthor")
	defer cleanupUser(t, author.ID)
	other := insertTestUser(t, "u_htf_other", "htfother")
	defer cleanupUser(t, other.ID)

	remoteHost := "remote.example"
	rows := []*model.Following{
		{ID: "htf_1", FollowerID: "u_htf_l1", FolloweeID: author.ID},
		{ID: "htf_2", FollowerID: "u_htf_r1", FolloweeID: author.ID, FollowerHost: &remoteHost},
		{ID: "htf_3", FollowerID: "u_htf_l2", FolloweeID: author.ID},
		{ID: "htf_4", FollowerID: "u_htf_h1", FolloweeID: author.ID, IsFollowerHibernated: true},
		{ID: "htf_5", FollowerID: "u_htf_rh", FolloweeID: author.ID, FollowerHost: &remoteHost, IsFollowerHibernated: true},
		{ID: "htf_6", FollowerID: "u_htf_l3", FolloweeID: author.ID},
		// 別の利用者のフォロワーは混ざらない
		{ID: "htf_7", FollowerID: "u_htf_l1", FolloweeID: other.ID},
	}
	created := map[string]bool{}
	for _, f := range rows {
		if created[f.FollowerID] {
			continue
		}
		created[f.FollowerID] = true
		u := insertTestUser(t, f.FollowerID, strings.ReplaceAll(f.FollowerID, "_", ""))
		defer cleanupUser(t, u.ID)
	}
	for _, f := range rows {
		require.NoError(t, testDB.Create(f).Error)
		defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, f.ID)
	}

	all, err := repo.ListFollowers(author.ID, 10, 0)
	require.NoError(t, err)
	require.Len(t, all, 6, "前提: author のフォロー行は 6 本")

	got, err := repo.ListHomeTimelineFollowers(author.ID, 10, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"htf_6", "htf_3", "htf_1"}, followingIDs(got), "ローカルで休眠していないフォロワーだけを id の降順で読む")

	first, err := repo.ListHomeTimelineFollowers(author.ID, 2, 0)
	require.NoError(t, err)
	second, err := repo.ListHomeTimelineFollowers(author.ID, 2, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"htf_6", "htf_3"}, followingIDs(first))
	assert.Equal(t, []string{"htf_1"}, followingIDs(second), "offset は絞った後の行で数える")
}

// TestListHomeTimelineFollowers_GenericPlanUsesPartialIndex pins the SQL of
// ListHomeTimelineFollowers to the predicate of 000141 (#4021).
//
// 本番は準備した文を使い回すので、同じ文が繰り返されると generic plan に切り替わる
// (TestListFollowersToNotify_GenericPlanUsesPartialIndex と同じ)。述語が部分 index と
// 食い違うと (bind 変数にする、条件を落とすなど)、index の述語を満たすと判定できず、
// 全フォロワーの行を読む形に戻る。
//
// 共有の schema では表がほぼ空なので、planner は主キーを逆順に読んで filter する計画を
// 選ぶことがあり、部分 index を使えるかを見られない (-shuffle の順序で実際に落ちた)。
// 専用の schema で following の他の index を外し、使える index をこれだけにして確かめる。
// 使えなければ Seq Scan になる。
func TestListHomeTimelineFollowers_GenericPlanUsesPartialIndex(t *testing.T) {
	db, m := openPerfIndexSchema(t, "homeplan")
	const name = "IDX_following_followeeId_id_home"
	require.NoError(t, runMigrate(func() error { return m.Migrate(141) }))

	var others []struct {
		Index      string
		Constraint *string
	}
	require.NoError(t, db.Raw(`
		SELECT ic.relname AS index, con.conname AS constraint
		FROM pg_index x
		JOIN pg_class ic ON ic.oid = x.indexrelid
		JOIN pg_class tc ON tc.oid = x.indrelid
		LEFT JOIN pg_constraint con ON con.conindid = x.indexrelid
		WHERE tc.relnamespace = to_regnamespace(current_schema())
		  AND tc.relname = 'following' AND ic.relname <> ?`, name).Scan(&others).Error)
	require.NotEmpty(t, others, "外す index を拾えていない")
	for _, o := range others {
		stmt := `DROP INDEX "` + o.Index + `"`
		if o.Constraint != nil {
			stmt = `ALTER TABLE "following" DROP CONSTRAINT "` + *o.Constraint + `"`
		}
		require.NoError(t, db.Exec(stmt).Error, stmt)
	}

	rec := &sqlRecorder{Interface: logger.Discard}
	repo := NewFollowingRepository(db.Session(&gorm.Session{Logger: rec}))
	_, err := repo.ListHomeTimelineFollowers("u_htf_none", 200, 200)
	require.NoError(t, err)

	rec.mu.Lock()
	sql, vars := rec.sql, rec.vars
	rec.mu.Unlock()
	require.Contains(t, sql, `FROM "following"`, "ListHomeTimelineFollowers の SQL を拾えていない")
	require.NotEmpty(t, vars)

	placeholders := make([]string, len(vars))
	for i := range vars {
		placeholders[i] = "?"
	}
	var lines []string
	require.NoError(t, db.Connection(func(conn *gorm.DB) error {
		defer conn.Exec(`DEALLOCATE gp_home_followers`) // PREPARE 前に落ちたときのエラーは無視する
		tx := conn.Begin()
		require.NoError(t, tx.Error)
		defer tx.Rollback()
		for _, s := range []string{
			`SET LOCAL plan_cache_mode = force_generic_plan`,
			`SET LOCAL enable_seqscan = off`,
			`PREPARE gp_home_followers AS ` + sql,
		} {
			require.NoError(t, tx.Exec(s).Error, s)
		}
		return tx.Raw(`EXPLAIN EXECUTE gp_home_followers(`+strings.Join(placeholders, ", ")+`)`, vars...).Scan(&lines).Error
	}))
	plan := strings.Join(lines, "\n")
	assert.Contains(t, plan, name,
		"generic plan で部分 index が使われない (述語を 000141 と揃えているか):\n%s", plan)
}

// TestMigration141_FollowingHomeTimelineFollowersIndex checks that 000141
// creates a valid partial index with the fanout's predicate and that down
// removes it (#4021).
func TestMigration141_FollowingHomeTimelineFollowersIndex(t *testing.T) {
	db, m := openPerfIndexSchema(t, "migrate141")
	const name = "IDX_following_followeeId_id_home"

	require.NoError(t, runMigrate(func() error { return m.Migrate(141) }))
	state := perfIndexState(t, db)
	valid, ok := state[name]
	require.True(t, ok, "%s が無い", name)
	assert.True(t, valid, "%s が INVALID のまま", name)
	var def string
	require.NoError(t, db.Raw(`
		SELECT pg_get_indexdef(c.oid) FROM pg_class c
		WHERE c.relnamespace = to_regnamespace(current_schema()) AND c.relname = ?`, name).Scan(&def).Error)
	assert.Contains(t, def, `following USING btree ("followeeId", id) WHERE (("followerHost" IS NULL) AND (NOT "isFollowerHibernated"))`)

	require.NoError(t, runMigrate(func() error { return m.Migrate(140) }))
	assert.NotContains(t, perfIndexState(t, db), name, "down が %s を落としていない", name)
}
