package repository

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/testutil"
)

// migration 000116 の書き換えを実 PostgreSQL で固定する (#3394)。
//
// **通常の migration 適用では一度も実行されない。** `ApplyMigrations` が流すのは
// meta 行の無い fresh schema なので、UPDATE が当たる行が無い。書き換える行と
// 触らない行を自分で用意して、up と down を流す。
//
// **`ApplyMigrations` は呼ばない。** 必要なのは meta の 2 列だけで、テーブルごと
// 作り直す (列を DROP すると 1600 列の枠が減る。#2756)。
func TestMigration000116_RewritesOnlyPreviousDefaults(t *testing.T) {
	db, err := testutil.OpenTestDBSchema("repourlmig")
	require.NoError(t, err)
	// 呼ぶたびに新しい接続プールが開くので、使い終わったら閉じる。開いたままだと
	// パッケージの残りのテストの間ずっと接続を持ち、並行に走る他のテストの接続枠を食う。
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	require.NoError(t, db.Exec(`DROP TABLE IF EXISTS "meta"`).Error)
	require.NoError(t, db.Exec(`CREATE TABLE "meta" ("id" varchar PRIMARY KEY, "repositoryUrl" varchar, "feedbackUrl" varchar)`).Error)

	const (
		oldRepo     = "https://github.com/shiroha-a/mk"
		oldFeedback = "https://github.com/shiroha-a/mk/issues/new"
		newRepo     = "https://github.com/Elythia-Network/elythia"
		newFeedback = "https://github.com/Elythia-Network/elythia/issues/new"
	)
	rows := []struct {
		id, repo, feedback any
	}{
		{"default", oldRepo, oldFeedback},
		{"custom", "https://git.example/fork", "https://git.example/fork/issues"},
		{"upstream", "https://github.com/misskey-dev/misskey", "https://github.com/misskey-dev/misskey/issues/new"},
		{"null", nil, nil},
		// 完全一致だけを書き換える。末尾の `/` が付いた値は operator が入れたもの。
		{"slash", oldRepo + "/", oldFeedback + "/"},
	}
	for _, r := range rows {
		require.NoError(t, db.Exec(`INSERT INTO "meta" ("id","repositoryUrl","feedbackUrl") VALUES (?,?,?)`, r.id, r.repo, r.feedback).Error)
	}

	type got struct {
		ID            string
		RepositoryURL *string `gorm:"column:repositoryUrl"`
		FeedbackURL   *string `gorm:"column:feedbackUrl"`
	}
	read := func() map[string]got {
		var out []got
		require.NoError(t, db.Raw(`SELECT "id","repositoryUrl","feedbackUrl" FROM "meta"`).Scan(&out).Error)
		require.Len(t, out, len(rows))
		m := map[string]got{}
		for _, g := range out {
			m[g.ID] = g
		}
		return m
	}
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}

	up := migrationSQL(t, "000116_transferred_repository_url.up.sql")
	require.Contains(t, up, "UPDATE", "000116 の up が読めていない")
	require.NoError(t, db.Exec(up).Error)
	after := read()
	assert.Equal(t, newRepo, str(after["default"].RepositoryURL))
	assert.Equal(t, newFeedback, str(after["default"].FeedbackURL))
	assert.Equal(t, "https://git.example/fork", str(after["custom"].RepositoryURL))
	assert.Equal(t, "https://git.example/fork/issues", str(after["custom"].FeedbackURL))
	assert.Equal(t, "https://github.com/misskey-dev/misskey", str(after["upstream"].RepositoryURL))
	assert.Equal(t, "https://github.com/misskey-dev/misskey/issues/new", str(after["upstream"].FeedbackURL))
	assert.Equal(t, "<nil>", str(after["null"].RepositoryURL))
	assert.Equal(t, oldRepo+"/", str(after["slash"].RepositoryURL))
	assert.Equal(t, oldFeedback+"/", str(after["slash"].FeedbackURL))

	// 流し直しても変わらない (golang-migrate が dirty から再実行する場合)。
	require.NoError(t, db.Exec(up).Error)
	assert.Equal(t, after, read())

	down := migrationSQL(t, "000116_transferred_repository_url.down.sql")
	require.Contains(t, down, "UPDATE", "000116 の down が読めていない")
	require.NoError(t, db.Exec(down).Error)
	back := read()
	assert.Equal(t, oldRepo, str(back["default"].RepositoryURL))
	assert.Equal(t, oldFeedback, str(back["default"].FeedbackURL))
	assert.Equal(t, "https://git.example/fork", str(back["custom"].RepositoryURL))
	assert.Equal(t, oldRepo+"/", str(back["slash"].RepositoryURL))
}
