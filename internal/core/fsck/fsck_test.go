package fsck

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/shiroha-a/mk/internal/testutil"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)
	// 各テストは自分のパッケージ schema に閉じているので無条件削除でよい (#2450)。
	for _, q := range []string{
		`DELETE FROM "following"`, `DELETE FROM "note"`,
		`DELETE FROM "drive_file"`, `DELETE FROM "user"`,
	} {
		require.NoError(t, db.Exec(q).Error)
	}
	return db
}

func seedUser(t *testing.T, db *gorm.DB, id string) {
	t.Helper()
	require.NoError(t, db.Exec(
		`INSERT INTO "user" (id, username, "usernameLower", "avatarDecorations") VALUES (?, ?, ?, '[]')`,
		id, id, id).Error)
}

func seedNote(t *testing.T, db *gorm.DB, id, userID string) {
	t.Helper()
	require.NoError(t, db.Exec(
		`INSERT INTO "note" (id, "userId", visibility) VALUES (?, ?, 'public')`, id, userID).Error)
}

// ずれが無ければ何も報告しない。
func TestRun_CleanDatabase(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")

	rep, err := Run(context.Background(), db, Options{})
	require.NoError(t, err)
	assert.True(t, rep.OK())
	assert.Empty(t, rep.Drifts)
}

// notesCount のずれを検出すること。増減はベストエフォートなので実際にずれる。
func TestRun_DetectsNotesCountDrift(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	seedNote(t, db, "n1", "u1")
	seedNote(t, db, "n2", "u1")
	// カウンタだけを壊す (INSERT はカウンタを更新しないので、明示的にずらす)。
	require.NoError(t, db.Exec(`UPDATE "user" SET "notesCount" = 99 WHERE id = 'u1'`).Error)

	rep, err := Run(context.Background(), db, Options{})
	require.NoError(t, err)
	require.Len(t, rep.Drifts, 1)
	assert.Equal(t, "notesCount", rep.Drifts[0].Column)
	assert.EqualValues(t, 99, rep.Drifts[0].Stored)
	assert.EqualValues(t, 2, rep.Drifts[0].Actual)
	assert.Zero(t, rep.Repaired, "既定では書き込まない")
}

// **既定は読み取り専用。** 検査しただけで値が変わってはいけない。
func TestRun_DoesNotWriteWithoutFix(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	require.NoError(t, db.Exec(`UPDATE "user" SET "notesCount" = 42 WHERE id = 'u1'`).Error)

	_, err := Run(context.Background(), db, Options{})
	require.NoError(t, err)

	var got int64
	require.NoError(t, db.Raw(`SELECT "notesCount" FROM "user" WHERE id = 'u1'`).Scan(&got).Error)
	assert.EqualValues(t, 42, got, "値が変わっていない")
}

func TestRun_FixRepairsCounters(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	seedNote(t, db, "n1", "u1")
	require.NoError(t, db.Exec(`UPDATE "user" SET "notesCount" = 99 WHERE id = 'u1'`).Error)

	rep, err := Run(context.Background(), db, Options{Fix: true})
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Repaired)

	var got int64
	require.NoError(t, db.Raw(`SELECT "notesCount" FROM "user" WHERE id = 'u1'`).Scan(&got).Error)
	assert.EqualValues(t, 1, got)

	// 修正後は clean。
	rep, err = Run(context.Background(), db, Options{})
	require.NoError(t, err)
	assert.True(t, rep.OK())
}

func TestRun_DetectsFollowCounters(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	seedUser(t, db, "u2")
	require.NoError(t, db.Exec(
		`INSERT INTO "following" (id, "followerId", "followeeId") VALUES ('f1', 'u1', 'u2')`).Error)
	require.NoError(t, db.Exec(`UPDATE "user" SET "followingCount" = 5 WHERE id = 'u1'`).Error)
	require.NoError(t, db.Exec(`UPDATE "user" SET "followersCount" = 7 WHERE id = 'u2'`).Error)

	rep, err := Run(context.Background(), db, Options{Fix: true})
	require.NoError(t, err)
	assert.Equal(t, 2, rep.Repaired)

	var following, followers int64
	require.NoError(t, db.Raw(`SELECT "followingCount" FROM "user" WHERE id = 'u1'`).Scan(&following).Error)
	require.NoError(t, db.Raw(`SELECT "followersCount" FROM "user" WHERE id = 'u2'`).Scan(&followers).Error)
	assert.EqualValues(t, 1, following)
	assert.EqualValues(t, 1, followers)
}

func TestRun_DetectsNoteCounters(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	seedNote(t, db, "parent", "u1")
	require.NoError(t, db.Exec(
		`INSERT INTO "note" (id, "userId", visibility, "replyId") VALUES ('r1', 'u1', 'public', 'parent')`).Error)
	require.NoError(t, db.Exec(
		`INSERT INTO "note" (id, "userId", visibility, "renoteId") VALUES ('rn1', 'u1', 'public', 'parent')`).Error)
	require.NoError(t, db.Exec(
		`UPDATE "note" SET "repliesCount" = 9, "renoteCount" = 9 WHERE id = 'parent'`).Error)

	_, err := Run(context.Background(), db, Options{Fix: true})
	require.NoError(t, err)

	var replies, renotes int64
	require.NoError(t, db.Raw(`SELECT "repliesCount" FROM "note" WHERE id = 'parent'`).Scan(&replies).Error)
	require.NoError(t, db.Raw(`SELECT "renoteCount" FROM "note" WHERE id = 'parent'`).Scan(&renotes).Error)
	assert.EqualValues(t, 1, replies)
	assert.EqualValues(t, 1, renotes)
}

// pageCount は #3293 から維持しているが、それより前に作ったページが参照する
// ノートは 0 のまま残っている。ページの content から数え直して直す。
func TestRun_DetectsPageCountDrift(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	for _, id := range []string{"n1", "n2", "n3", "3"} {
		seedNote(t, db, id, "u1")
	}
	// p1 は n1 を 2 回 (1 回と数える)、section の中で n2 を参照する。p2 も n1 を参照する。
	// p3 は content が配列でない (何も参照しない)。n3 は -1 になっている。
	// children が配列でない section (本家の pages/create は各要素を object としか
	// 検証しないので作れる) と、文字列でない note (id が "3" のノートと取り違えない)
	// は数えず、検査全体も落とさない。
	require.NoError(t, db.Exec(`INSERT INTO "page" (id, title, name, "userId", content) VALUES
		('p1', 't', 'a', 'u1', '[{"type":"note","note":"n1"},{"type":"note","note":"n1"},{"type":"section","children":[{"type":"note","note":"n2"},{"type":"text","note":"n3"}]},{"type":"section","children":{"type":"note","note":"n3"}}]'),
		('p2', 't', 'b', 'u1', '[{"type":"note","note":"n1"},{"type":"note","note":3}]'),
		('p3', 't', 'c', 'u1', '{"type":"note","note":"n3"}')`).Error)
	require.NoError(t, db.Exec(`UPDATE "note" SET "pageCount" = -1 WHERE id = 'n3'`).Error)

	rep, err := Run(context.Background(), db, Options{})
	require.NoError(t, err)
	got := map[string]Drift{}
	for _, d := range rep.Drifts {
		if d.Column == "pageCount" {
			got[d.ID] = d
		}
	}
	require.Len(t, got, 3)
	assert.EqualValues(t, 2, got["n1"].Actual)
	assert.EqualValues(t, 1, got["n2"].Actual)
	assert.EqualValues(t, 0, got["n3"].Actual)

	_, err = Run(context.Background(), db, Options{Fix: true})
	require.NoError(t, err)
	rep, err = Run(context.Background(), db, Options{})
	require.NoError(t, err)
	for _, d := range rep.Drifts {
		assert.NotEqual(t, "pageCount", d.Column, "修正後は pageCount のずれが残らない")
	}
}

// clippedCount は #1768 から維持しているが、それより前にクリップした行は 0 の
// まま残っている (#3291)。実件数 (clip_note) と突き合わせて検出し、-fix で直す。
func TestRun_DetectsClippedCountDrift(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	seedNote(t, db, "n1", "u1")
	seedNote(t, db, "n2", "u1")
	require.NoError(t, db.Exec(`INSERT INTO "clip" (id, "userId", name) VALUES ('c1', 'u1', 'a'), ('c2', 'u1', 'b')`).Error)
	// n1 は 2 つのクリップに入っているのにカウンタが 0 (#1768 より前の行)。
	// n2 はクリップに入っていないのに -1 (0 のまま外して負になった行)。
	require.NoError(t, db.Exec(`INSERT INTO "clip_note" (id, "noteId", "clipId") VALUES ('cn1', 'n1', 'c1'), ('cn2', 'n1', 'c2')`).Error)
	require.NoError(t, db.Exec(`UPDATE "note" SET "clippedCount" = -1 WHERE id = 'n2'`).Error)

	rep, err := Run(context.Background(), db, Options{})
	require.NoError(t, err)
	got := map[string]Drift{}
	for _, d := range rep.Drifts {
		if d.Column == "clippedCount" {
			got[d.ID] = d
		}
	}
	require.Len(t, got, 2)
	assert.EqualValues(t, 0, got["n1"].Stored)
	assert.EqualValues(t, 2, got["n1"].Actual)
	assert.EqualValues(t, -1, got["n2"].Stored)
	assert.EqualValues(t, 0, got["n2"].Actual)

	_, err = Run(context.Background(), db, Options{Fix: true})
	require.NoError(t, err)
	var n1, n2 int64
	require.NoError(t, db.Raw(`SELECT "clippedCount" FROM "note" WHERE id = 'n1'`).Scan(&n1).Error)
	require.NoError(t, db.Raw(`SELECT "clippedCount" FROM "note" WHERE id = 'n2'`).Scan(&n2).Error)
	assert.EqualValues(t, 2, n1)
	assert.EqualValues(t, 0, n2)

	rep, err = Run(context.Background(), db, Options{})
	require.NoError(t, err)
	assert.True(t, rep.OK(), "修正後は clean")
}

// 孤児は報告するが**削除しない**。カウンタは元データから導けるが、削除した行は
// 復元できない。
func TestRun_ReportsOrphansWithoutDeleting(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	seedNote(t, db, "n1", "u1")
	// FK を外して親を消し、孤児を作る。**制約名は実測したもの**
	// (note.userId は ON DELETE CASCADE なので、外さないと note ごと消えて
	// 孤児が作れない)。検査後に戻す。
	require.NoError(t, db.Exec(`ALTER TABLE "note" DROP CONSTRAINT "note_userId_fkey"`).Error)
	t.Cleanup(func() {
		_ = db.Exec(`DELETE FROM "note"`).Error
		_ = db.Exec(`ALTER TABLE "note" ADD CONSTRAINT "note_userId_fkey"
		             FOREIGN KEY ("userId") REFERENCES "user"(id) ON DELETE CASCADE`).Error
	})
	require.NoError(t, db.Exec(`DELETE FROM "user" WHERE id = 'u1'`).Error)

	rep, err := Run(context.Background(), db, Options{Fix: true})
	require.NoError(t, err)

	var found bool
	for _, o := range rep.Orphans {
		if o.Table == "note" {
			found = true
			assert.EqualValues(t, 1, o.Count)
		}
	}
	assert.True(t, found, "孤児として報告される")

	var n int64
	require.NoError(t, db.Raw(`SELECT COUNT(*) FROM "note"`).Scan(&n).Error)
	assert.EqualValues(t, 1, n, "-fix でも孤児は消さない")
}

// 上限を超えても壊れない (大規模インスタンスで全件を持たない)。
func TestRun_RespectsLimit(t *testing.T) {
	db := newTestDB(t)
	for _, id := range []string{"a1", "a2", "a3"} {
		seedUser(t, db, id)
	}
	require.NoError(t, db.Exec(`UPDATE "user" SET "notesCount" = 1`).Error)

	rep, err := Run(context.Background(), db, Options{Limit: 2})
	require.NoError(t, err)
	assert.Len(t, rep.Drifts, 2)
}

func TestReport_OK(t *testing.T) {
	assert.True(t, Report{}.OK())
	assert.False(t, Report{Drifts: []Drift{{}}}.OK())
	assert.False(t, Report{Orphans: []Orphan{{}}}.OK())
}

// closedDB returns a connection whose socket is already closed, so every
// query fails. エラー経路を通すため。
func closedDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := testutil.MustOpenTestDB()
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, sqlDB.Close())
	return db
}

// 検査クエリが失敗したらエラーを返す。**握り潰さない** — 「ずれ無し」と
// 「検査できなかった」を同じ結果にすると、壊れているのに clean に見える。
func TestRun_ReturnsErrorWhenCheckFails(t *testing.T) {
	_, err := Run(context.Background(), closedDB(t), Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fsck:")
}

// 修正の書き込みが失敗したらエラーを返す。
func TestRepair_ReturnsErrorOnWriteFailure(t *testing.T) {
	err := repair(context.Background(), closedDB(t),
		Drift{Table: "user", Column: "notesCount", ID: "u1", Actual: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "notesCount")
	assert.Contains(t, err.Error(), "u1", "どの行で失敗したかが分かる")
}

// 孤児検査の失敗もエラーになる (カウンタ検査を通過した後の経路)。
func TestRun_ReturnsErrorWhenOrphanCheckFails(t *testing.T) {
	db := newTestDB(t)
	// カウンタ検査は通る状態にしてから、孤児検査で参照するテーブルを消す。
	require.NoError(t, db.Exec(`ALTER TABLE "drive_file" RENAME TO "drive_file_tmp"`).Error)
	t.Cleanup(func() {
		_ = db.Exec(`ALTER TABLE "drive_file_tmp" RENAME TO "drive_file"`).Error
	})

	_, err := Run(context.Background(), db, Options{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "孤児検査")
}

// クリップを消しても clippedCount は減らない (本家と同じ)。保存値が実件数より
// 大きいずれは平常時にも出るので、fsck が報告し -fix で下げられること。
func TestRun_ClippedCountDriftAfterClipDeletion(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	seedNote(t, db, "n1", "u1")
	require.NoError(t, db.Exec(`INSERT INTO "clip" (id, "userId", name) VALUES ('c1', 'u1', 'a')`).Error)
	require.NoError(t, db.Exec(`INSERT INTO "clip_note" (id, "noteId", "clipId") VALUES ('cn1', 'n1', 'c1')`).Error)
	require.NoError(t, db.Exec(`UPDATE "note" SET "clippedCount" = 1 WHERE id = 'n1'`).Error)
	// clip の削除は clip_note を CASCADE で消すが、カウンタには触れない。
	require.NoError(t, db.Exec(`DELETE FROM "clip" WHERE id = 'c1'`).Error)

	rep, err := Run(context.Background(), db, Options{Fix: true})
	require.NoError(t, err)
	drifts := clippedCountDrifts(rep)
	require.Len(t, drifts, 1)
	assert.EqualValues(t, 1, drifts[0].Stored)
	assert.EqualValues(t, 0, drifts[0].Actual)

	var got int64
	require.NoError(t, db.Raw(`SELECT "clippedCount" FROM "note" WHERE id = 'n1'`).Scan(&got).Error)
	assert.EqualValues(t, 0, got)
}

// clippedCount は smallint なので、実件数が上限を超えるノートは 32767 で
// 頭打ちにして書き戻す。頭打ちが無いと -fix の UPDATE が範囲外で失敗し続ける。
func TestRun_ClippedCountActualIsCappedAtSmallintMax(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	seedNote(t, db, "n1", "u1")
	require.NoError(t, db.Exec(`INSERT INTO "clip" (id, "userId", name)
		SELECT 'c' || g, 'u1', 'c' || g FROM generate_series(1, 32768) g`).Error)
	require.NoError(t, db.Exec(`INSERT INTO "clip_note" (id, "noteId", "clipId")
		SELECT 'cn' || g, 'n1', 'c' || g FROM generate_series(1, 32768) g`).Error)

	rep, err := Run(context.Background(), db, Options{Fix: true})
	require.NoError(t, err, "-fix が範囲外で失敗しない")
	drifts := clippedCountDrifts(rep)
	require.Len(t, drifts, 1)
	assert.EqualValues(t, 32767, drifts[0].Actual)

	rep, err = Run(context.Background(), db, Options{})
	require.NoError(t, err)
	assert.True(t, rep.OK(), "頭打ちの値に直した後は clean")
}

// clippedCountDrifts returns only the clippedCount drifts. seedNote は
// notesCount を更新しないので、他の列のずれも一緒に報告される。
func clippedCountDrifts(rep Report) []Drift {
	var out []Drift
	for _, d := range rep.Drifts {
		if d.Column == "clippedCount" {
			out = append(out, d)
		}
	}
	return out
}
