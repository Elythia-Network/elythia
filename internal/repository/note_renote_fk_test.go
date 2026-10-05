package repository

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// migrationSQL reads a migration file so the tests exercise the SQL that
// actually ships. 手で書き写すとファイルを変更してもテストが追随しない。
func migrationSQL(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "migration", name))
	require.NoError(t, err, "migration %s を読めない", name)
	return string(b)
}

// #2629: 000001 が note の自己参照 FK を張らないこと。
//
// 張ると、upstream 2025.8.0 (migration 1753868431598 / misskey-dev#16332) 以降の
// TS 製 DB を引き継げなくなる。upstream は同版で自己参照 FK を削除しており、
// 以後はノートを削除しても参照する側の renoteId / replyId が**正常に残る**。
// `ADD CONSTRAINT` は既存行を全件検証するので、そういう行が 1 つでもあると
// 23503 で失敗し、golang-migrate は version 1 で dirty のまま停止する。
//
// 000080 で drop する以上ここで張る意味も無い。誰かが戻したときに気付けるよう
// ファイルの内容そのものを検査する。
func TestMigration000001_DoesNotAddNoteSelfFK(t *testing.T) {
	sql := migrationSQL(t, "000001_initial.up.sql")
	assert.NotContains(t, sql, `ADD CONSTRAINT "FK_note_replyId"`,
		"000001 で自己参照 FK を張ると TS 製 DB の migration が version 1 で止まる")
	assert.NotContains(t, sql, `ADD CONSTRAINT "FK_note_renoteId"`,
		"000001 で自己参照 FK を張ると TS 製 DB の migration が version 1 で止まる")
}

// #2623: note の自己参照 FK (renoteId / replyId) を落としたので、元ノートが
// 削除されてもリノート / 返信側の参照は NULL 化されない。
//
// FK が `ON DELETE SET NULL` だった頃は renoteId ごと消え、renoteUserId だけが
// 残る「本文も引用先も無いノート」になっていた。frontend は renoteId が残って
// いれば「削除されたノート」として描画できる (MkNote.vue) ので、upstream と
// 同じく参照を残すのが正しい。
func TestNoteRepository_SelfReferenceSurvivesTargetDeletion(t *testing.T) {
	user := insertTestUser(t, "rnfk_u", "rnfkuser")
	defer cleanupUser(t, user.ID)

	targetID := "rnfk_target"
	userID := user.ID

	require.NoError(t, testDB.Create(&model.Note{ID: targetID, UserID: user.ID, Visibility: "public"}).Error)
	require.NoError(t, testDB.Create(&model.Note{
		ID: "rnfk_renote", UserID: user.ID, Visibility: "public",
		RenoteID: &targetID, RenoteUserID: &userID,
	}).Error)
	require.NoError(t, testDB.Create(&model.Note{
		ID: "rnfk_reply", UserID: user.ID, Visibility: "public",
		ReplyID: &targetID, ReplyUserID: &userID,
	}).Error)

	// 対象ノートを消す。FK が残っていればここで renoteId / replyId が NULL 化される。
	require.NoError(t, testDB.Exec(`DELETE FROM "note" WHERE id = ?`, targetID).Error)

	var gotRenote model.Note
	require.NoError(t, testDB.First(&gotRenote, "id = ?", "rnfk_renote").Error)
	require.NotNil(t, gotRenote.RenoteID, "FK を落としたので renoteId は NULL 化されない")
	assert.Equal(t, targetID, *gotRenote.RenoteID)

	var gotReply model.Note
	require.NoError(t, testDB.First(&gotReply, "id = ?", "rnfk_reply").Error)
	require.NotNil(t, gotReply.ReplyID, "FK を落としたので replyId は NULL 化されない")
	assert.Equal(t, targetID, *gotReply.ReplyID)
}

// #2623: 孤児行を後始末する migration (000081) を**ファイルから読んで実行し**、
// 削除する行と残す行の境界を固定する。
//
// SQL を手で書き写すとファイルを変更してもテストが通ってしまうので、
// migration そのものを流す。
func TestMigration000081_OrphanRepairScope(t *testing.T) {
	user := insertTestUser(t, "orph_u", "orphuser")
	defer cleanupUser(t, user.ID)

	userID := user.ID
	liveTarget := "orph_live_target"
	text := "残す"
	cw := "cw"

	notes := map[string]*model.Note{
		// 消えるべき: 元が pure renote、対象が消えていて、誰も触っていない。
		"orphan": {ID: "orph_orphan", UserID: user.ID, Visibility: "public", RenoteUserID: &userID},
		// 残すべき (中身がある)。
		"quote":    {ID: "orph_quote", UserID: user.ID, Visibility: "public", RenoteUserID: &userID, Text: &text},
		"withCW":   {ID: "orph_cw", UserID: user.ID, Visibility: "public", RenoteUserID: &userID, CW: &cw},
		"withFile": {ID: "orph_file", UserID: user.ID, Visibility: "public", RenoteUserID: &userID, FileIDs: []string{"f1"}},
		"withPoll": {ID: "orph_poll", UserID: user.ID, Visibility: "public", RenoteUserID: &userID, HasPoll: true},
		// 残すべき (他の利用者が触っている)。消すと note への CASCADE で
		// リアクション等の記録まで道連れになる。
		"reacted": {ID: "orph_reacted", UserID: user.ID, Visibility: "public", RenoteUserID: &userID, Reactions: []byte(`{"👍": 1}`)},
		"replied": {ID: "orph_replied", UserID: user.ID, Visibility: "public", RenoteUserID: &userID, RepliesCount: 1},
		"renoted": {ID: "orph_renoted", UserID: user.ID, Visibility: "public", RenoteUserID: &userID, RenoteCount: 1},
		"clipped": {ID: "orph_clipped", UserID: user.ID, Visibility: "public", RenoteUserID: &userID, ClippedCount: 1},
		// そもそもリノートではない空ノート。
		"plain": {ID: "orph_plain", UserID: user.ID, Visibility: "public"},
		// 参照が生きているリノート。
		"intact": {ID: "orph_intact", UserID: user.ID, Visibility: "public", RenoteID: &liveTarget, RenoteUserID: &userID},
		"target": {ID: liveTarget, UserID: user.ID, Visibility: "public", Text: &text},
	}
	for _, n := range notes {
		require.NoError(t, testDB.Create(n).Error)
	}

	// migration ファイルをそのまま実行する。
	require.NoError(t, testDB.Exec(migrationSQL(t, "000081_repair_orphan_renote_rows.up.sql")).Error)

	exists := func(id string) bool {
		var n int64
		require.NoError(t, testDB.Raw(`SELECT count(*) FROM "note" WHERE id = ?`, id).Scan(&n).Error)
		return n > 0
	}

	assert.False(t, exists(notes["orphan"].ID), "対象が消えていて誰も触っていない pure renote は削除される")
	for _, k := range []string{"quote", "withCW", "withFile", "withPoll"} {
		assert.True(t, exists(notes[k].ID), "%s: 中身を持つノートは残す", k)
	}
	for _, k := range []string{"reacted", "replied", "renoted", "clipped"} {
		assert.True(t, exists(notes[k].ID), "%s: 他利用者の操作がある行は残す", k)
	}
	assert.True(t, exists(notes["plain"].ID), "リノートでない空ノートは巻き込まない")
	assert.True(t, exists(notes["intact"].ID), "参照が生きているリノートは残す")
	assert.True(t, exists(notes["target"].ID), "参照先は残す")

	// 残した孤児行からは、切れたリンクの痕跡が消えていること。
	// ここが残ると mute / block / instance-mute の判定に効き続ける。
	var quote model.Note
	require.NoError(t, testDB.First(&quote, "id = ?", notes["quote"].ID).Error)
	assert.Nil(t, quote.RenoteUserID, "renoteId が無い行の renoteUserId は消す")
	assert.Nil(t, quote.RenoteUserHost, "renoteUserHost も消す")

	// 参照が生きている行は触らない。
	var intact model.Note
	require.NoError(t, testDB.First(&intact, "id = ?", notes["intact"].ID).Error)
	require.NotNil(t, intact.RenoteID)
	assert.NotNil(t, intact.RenoteUserID, "生きているリノートの renoteUserId は残す")
}

// #2623: down migration が、参照先の無い値を持つ DB でも成功すること。
//
// 通常の ADD CONSTRAINT は既存行を全件検証するので 23503 で落ちる。NOT VALID を
// 付けて既存行を検証しない形にしてあり、**この性質が壊れると本番で巻き戻せなく
// なる**。実際に dangling な行を置いた状態で down SQL を流して確かめる。
func TestMigration000080_DownSucceedsWithDanglingReferences(t *testing.T) {
	user := insertTestUser(t, "dang_u", "danguser")
	defer cleanupUser(t, user.ID)

	missing := "dang_missing_note"
	require.NoError(t, testDB.Create(&model.Note{
		ID: "dang_note", UserID: user.ID, Visibility: "public",
		RenoteID: &missing, ReplyID: &missing,
	}).Error)

	// down を流したら必ず元に戻す (同パッケージの他テストへ FK を残さない)。
	t.Cleanup(func() {
		testDB.Exec(migrationSQL(t, "000080_drop_note_self_fk.up.sql"))
	})

	err := testDB.Exec(migrationSQL(t, "000080_drop_note_self_fk.down.sql")).Error
	require.NoError(t, err, "dangling 参照があっても down は成功しなければならない")

	// FK が復元されていること。
	var n int64
	require.NoError(t, testDB.Raw(`
		SELECT count(*) FROM pg_constraint
		WHERE conrelid = '"note"'::regclass AND conname IN ('FK_note_renoteId','FK_note_replyId')`).Scan(&n).Error)
	assert.EqualValues(t, 2, n, "down で FK が 2 本とも復元される")

	// dangling な行はそのまま残っていること (NULL 化して壊さない)。
	var got model.Note
	require.NoError(t, testDB.First(&got, "id = ?", "dang_note").Error)
	require.NotNil(t, got.RenoteID, "down は参照先の無い renoteId を NULL 化しない")
	assert.Equal(t, missing, *got.RenoteID)
}
