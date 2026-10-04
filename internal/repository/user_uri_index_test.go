package repository

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// upstreamUserURIIndex is the index name upstream's Init migration creates for
// MiUser.uri (`@Index()`).
const upstreamUserURIIndex = "IDX_be623adaa4c566baf5d29ce0c8"

// userURIIndexDefs returns the definitions of every index on "user" whose key
// is exactly ("uri"), scoped to the package schema.
func userURIIndexDefs(t *testing.T) map[string]string {
	t.Helper()
	var rows []struct {
		Indexname string
		Indexdef  string
	}
	require.NoError(t, testDB.Raw(`
		SELECT indexname, indexdef FROM pg_indexes
		WHERE schemaname = current_schema() AND tablename = 'user'
		  AND indexdef LIKE '%USING btree (uri)%'`).Scan(&rows).Error)
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Indexname] = r.Indexdef
	}
	return out
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "migration", name))
	require.NoError(t, err)
	return string(b)
}

// TestUserURIIndex_MatchesUpstream pins migration 000108 (#3330): mk-go-born
// DBs get the same user.uri index upstream has, under upstream's name, and
// re-applying it to a DB that already has it (a TS-born DB) creates nothing.
func TestUserURIIndex_MatchesUpstream(t *testing.T) {
	defs := userURIIndexDefs(t)
	require.Len(t, defs, 1, "user(uri) の index はちょうど 1 本: %v", defs)
	def, ok := defs[upstreamUserURIIndex]
	require.True(t, ok, "upstream と同じ名前で作る: %v", defs)
	// partial / unique にすると TS 製 DB の index と定義が変わる。
	assert.NotContains(t, def, "UNIQUE")
	assert.NotContains(t, def, "WHERE")

	var valid bool
	require.NoError(t, testDB.Raw(`
		SELECT i.indisvalid FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema() AND c.relname = ?`, upstreamUserURIIndex).Scan(&valid).Error)
	assert.True(t, valid, "CONCURRENTLY の構築が INVALID のまま残っていない")

	up := readMigration(t, "000108_user_uri_index.up.sql")
	down := readMigration(t, "000108_user_uri_index.down.sql")

	// TS 製 DB 相当 (同名の index が既にある) に up を流しても増えない。
	require.NoError(t, testDB.Exec(up).Error)
	assert.Len(t, userURIIndexDefs(t), 1, "IF NOT EXISTS が名前で効いて二重化しない")

	// down は消し、up は作り直す。終わったら元の状態に戻っている。
	require.NoError(t, testDB.Exec(down).Error)
	assert.Empty(t, userURIIndexDefs(t), "down が index を落とす")
	require.NoError(t, testDB.Exec(up).Error)
	assert.Contains(t, userURIIndexDefs(t), upstreamUserURIIndex)
}
