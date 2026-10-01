package repository

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #3293: 000107 は、それより前に作ったページが参照するノートの pageCount を
// ページの content から数え直して埋める。増やす向きにしか直さない。
//
// **migration をテストの中で流す。** 適用済みの schema を見るだけでは、
// migration の中身を変えても緑のまま通る。
func TestMigration000107_BackfillsNotePageCount(t *testing.T) {
	up := migrationSQL(t, "000107_note_page_count_backfill.up.sql")
	user := insertTestUser(t, "u_m107", "m107user")
	defer cleanupUser(t, user.ID)
	for _, id := range []string{"n_m107_a", "n_m107_b", "n_m107_c", "n_m107_d", "n_m107_e", "3"} {
		insertTestNote(t, id, user.ID)
	}
	// repository を通さずに作る (#3293 より前のページと同じく pageCount は 0 のまま)。
	require.NoError(t, testDB.Exec(`INSERT INTO "page" (id, title, name, "userId", content) VALUES
		('pg_m107_1', 't', 'm1', ?, '[{"type":"note","note":"n_m107_a"},{"type":"note","note":"n_m107_a"},{"type":"section","children":[{"type":"note","note":"n_m107_b"}]},{"type":"section","children":{"type":"note","note":"n_m107_c"}}]'),
		('pg_m107_2', 't', 'm2', ?, '[{"type":"note","note":"n_m107_a"},{"type":"note","note":"n_m107_e"},{"type":"note","note":3}]')`, user.ID, user.ID).Error)
	defer cleanupPage(t, "pg_m107_1")
	defer cleanupPage(t, "pg_m107_2")
	// d はページに参照されていないが値が残っている (TS 由来で古い行)。減らさない。
	require.NoError(t, testDB.Exec(`UPDATE "note" SET "pageCount" = 5 WHERE id = 'n_m107_d'`).Error)
	// e は 1 ページから参照されているが、保存値の方が大きい。参照されている行でも
	// 下げない (下げるのは fsck の -fix の役目)。
	require.NoError(t, testDB.Exec(`UPDATE "note" SET "pageCount" = 7 WHERE id = 'n_m107_e'`).Error)

	require.NoError(t, testDB.Exec(up).Error)

	count := func(id string) int16 {
		var v int16
		require.NoError(t, testDB.Raw(`SELECT "pageCount" FROM "note" WHERE id = ?`, id).Scan(&v).Error)
		return v
	}
	assert.EqualValues(t, 2, count("n_m107_a"), "2 つのページから参照 (同じページ内の重複は 1 回)")
	assert.EqualValues(t, 1, count("n_m107_b"), "section の children もたどる")
	assert.EqualValues(t, 0, count("n_m107_c"), "children が配列でない section はたどらない")
	assert.EqualValues(t, 5, count("n_m107_d"), "参照されていない行は触らない")
	assert.EqualValues(t, 7, count("n_m107_e"), "増やす向きにしか直さない")
	assert.EqualValues(t, 0, count("3"), "文字列でない note は id \"3\" のノートと取り違えない")

	// 2 回流しても変わらない。
	require.NoError(t, testDB.Exec(up).Error)
	assert.EqualValues(t, 2, count("n_m107_a"))
}
