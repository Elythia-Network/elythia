package fsck

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/testutil"
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

// notesCount は増えるだけの値なので、保存値が実件数より小さい (増やすのに
// 失敗した) ときだけを報告する (#3927)。
func TestRun_DetectsLostNotesCountIncrements(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	seedNote(t, db, "n1", "u1")
	seedNote(t, db, "n2", "u1")
	// seedNote はカウンタを増やさないので、u1 は 0 のまま (2 回の増加を失った状態)。

	rep, err := Run(context.Background(), db, Options{})
	require.NoError(t, err)
	require.Len(t, rep.Drifts, 1)
	assert.Equal(t, "notesCount", rep.Drifts[0].Column)
	assert.EqualValues(t, 0, rep.Drifts[0].Stored)
	assert.EqualValues(t, 2, rep.Drifts[0].Actual)
	assert.Zero(t, rep.Repaired, "既定では書き込まない")
}

// 本家もElythiaも投稿を消しても notesCount を減らさないので、保存値が実件数より
// 大きいのは正常。報告せず、-fix でも下げない (#3927)。
func TestRun_NotesCountAboveRowCountIsNotDrift(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	seedNote(t, db, "n1", "u1")
	// 3 件投稿して 2 件消した利用者。
	require.NoError(t, db.Exec(`UPDATE "user" SET "notesCount" = 3 WHERE id = 'u1'`).Error)

	rep, err := Run(context.Background(), db, Options{Fix: true})
	require.NoError(t, err)
	assert.True(t, rep.OK())
	assert.Zero(t, rep.Repaired)

	var got int64
	require.NoError(t, db.Raw(`SELECT "notesCount" FROM "user" WHERE id = 'u1'`).Scan(&got).Error)
	assert.EqualValues(t, 3, got, "-fix でも下げない")
}

// リモートの利用者も検査する。#3928 より前に取り込んだ投稿の分は数えていない
// ので、-fix で取り込んだ件数まで上がる (#3927)。
func TestRun_NotesCountChecksRemoteUsers(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.Exec(
		`INSERT INTO "user" (id, username, "usernameLower", host, "avatarDecorations") VALUES ('r1', 'r1', 'r1', 'remote.example', '[]')`).Error)
	seedNote(t, db, "n1", "r1")

	rep, err := Run(context.Background(), db, Options{Fix: true})
	require.NoError(t, err)
	require.Len(t, rep.Drifts, 1, "リモートの利用者も報告する")
	assert.Equal(t, "r1", rep.Drifts[0].ID)
	var got int64
	require.NoError(t, db.Raw(`SELECT "notesCount" FROM "user" WHERE id = 'r1'`).Scan(&got).Error)
	assert.EqualValues(t, 1, got)
}

// **既定は読み取り専用。** 検査しただけで値が変わってはいけない。
func TestRun_DoesNotWriteWithoutFix(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	seedNote(t, db, "n1", "u1")

	rep, err := Run(context.Background(), db, Options{})
	require.NoError(t, err)
	require.Len(t, rep.Drifts, 1, "ずれはある")

	var got int64
	require.NoError(t, db.Raw(`SELECT "notesCount" FROM "user" WHERE id = 'u1'`).Scan(&got).Error)
	assert.EqualValues(t, 0, got, "値が変わっていない")
}

func TestRun_FixRepairsCounters(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	seedNote(t, db, "n1", "u1")
	seedNote(t, db, "n2", "u1")
	require.NoError(t, db.Exec(`UPDATE "user" SET "notesCount" = 1 WHERE id = 'u1'`).Error)

	rep, err := Run(context.Background(), db, Options{Fix: true})
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Repaired)

	var got int64
	require.NoError(t, db.Raw(`SELECT "notesCount" FROM "user" WHERE id = 'u1'`).Scan(&got).Error)
	assert.EqualValues(t, 2, got)

	// 修正後は clean。
	rep, err = Run(context.Background(), db, Options{})
	require.NoError(t, err)
	assert.True(t, rep.OK())
}

// 増えるだけのカウンタの書き戻しは、検査の後に増えた値を下げない。検査から
// 書き戻しまでの間にもサーバーは投稿を受け付けている (#3927)。
func TestRepair_RaiseOnlyNeverLowers(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	// 検査の時点では 0 → 実際 1 だったが、書き戻す前に 5 まで増えた。
	require.NoError(t, db.Exec(`UPDATE "user" SET "notesCount" = 5 WHERE id = 'u1'`).Error)

	require.NoError(t, repair(context.Background(), db,
		Drift{Table: "user", Column: "notesCount", ID: "u1", Stored: 0, Actual: 1}, true))
	var got int64
	require.NoError(t, db.Raw(`SELECT "notesCount" FROM "user" WHERE id = 'u1'`).Scan(&got).Error)
	assert.EqualValues(t, 5, got, "増えた値を下げない")

	// raiseOnly でないカウンタは実件数で上書きする。
	require.NoError(t, db.Exec(`UPDATE "user" SET "followersCount" = 7 WHERE id = 'u1'`).Error)
	require.NoError(t, repair(context.Background(), db,
		Drift{Table: "user", Column: "followersCount", ID: "u1", Stored: 7, Actual: 2}, false))
	require.NoError(t, db.Raw(`SELECT "followersCount" FROM "user" WHERE id = 'u1'`).Scan(&got).Error)
	assert.EqualValues(t, 2, got)
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

// 移行したアカウントが絡むフォローは、本家もElythiaもカウントに入れない。
// 実件数は相手が移行済みでない行だけを数え、移行済みの利用者自身は検査しない
// (本家 decrementFollowing の数え直しと同じ式)。
func TestRun_FollowCountsExcludeMovedAccounts(t *testing.T) {
	db := newTestDB(t)
	for _, id := range []string{"u1", "u2", "m1"} {
		seedUser(t, db, id)
	}
	require.NoError(t, db.Exec(`UPDATE "user" SET "movedToUri" = 'https://remote.example/users/new' WHERE id = 'm1'`).Error)
	require.NoError(t, db.Exec(`INSERT INTO "following" (id, "followerId", "followeeId") VALUES
		('f1', 'u1', 'm1'), ('f2', 'm1', 'u2'), ('f3', 'u1', 'u2')`).Error)
	// u1 は u2 だけを、u2 は u1 だけを数える。m1 の値は移行のときに 0 にした後、
	// 数え直されない。
	require.NoError(t, db.Exec(`UPDATE "user" SET "followingCount" = 1 WHERE id = 'u1'`).Error)
	require.NoError(t, db.Exec(`UPDATE "user" SET "followersCount" = 1 WHERE id = 'u2'`).Error)
	require.NoError(t, db.Exec(`UPDATE "user" SET "followersCount" = 5, "followingCount" = 5 WHERE id = 'm1'`).Error)

	rep, err := Run(context.Background(), db, Options{})
	require.NoError(t, err)
	assert.True(t, rep.OK(), "%+v", rep.Drifts)

	// 移行の前の値 (移行済みの相手を含めた値) が残っていれば、下げる向きに報告する。
	require.NoError(t, db.Exec(`UPDATE "user" SET "followingCount" = 2 WHERE id = 'u1'`).Error)
	require.NoError(t, db.Exec(`UPDATE "user" SET "followersCount" = 2 WHERE id = 'u2'`).Error)
	rep, err = Run(context.Background(), db, Options{})
	require.NoError(t, err)
	got := map[string]Drift{}
	for _, d := range rep.Drifts {
		got[d.Column+"/"+d.ID] = d
	}
	require.Len(t, got, 2, "%+v", rep.Drifts)
	assert.EqualValues(t, 1, got["followingCount/u1"].Actual)
	assert.EqualValues(t, 1, got["followersCount/u2"].Actual)
}

func TestRun_DetectsNoteCounters(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	seedUser(t, db, "u2")
	seedNote(t, db, "parent", "u1")
	require.NoError(t, db.Exec(
		`INSERT INTO "note" (id, "userId", visibility, "replyId") VALUES ('r1', 'u1', 'public', 'parent')`).Error)
	seedRenote(t, db, "rn1", "u2", "parent", nil)
	require.NoError(t, db.Exec(
		`UPDATE "note" SET "repliesCount" = 9, "renoteCount" = 0 WHERE id = 'parent'`).Error)

	_, err := Run(context.Background(), db, Options{Fix: true})
	require.NoError(t, err)

	var replies, renotes int64
	require.NoError(t, db.Raw(`SELECT "repliesCount" FROM "note" WHERE id = 'parent'`).Scan(&replies).Error)
	require.NoError(t, db.Raw(`SELECT "renoteCount" FROM "note" WHERE id = 'parent'`).Scan(&renotes).Error)
	assert.EqualValues(t, 1, replies)
	assert.EqualValues(t, 1, renotes)
}

// seedRenote inserts a renote of target by userID. A non-nil text makes it a
// quote.
func seedRenote(t *testing.T, db *gorm.DB, id, userID, target string, text *string) {
	t.Helper()
	require.NoError(t, db.Exec(
		`INSERT INTO "note" (id, "userId", visibility, "renoteId", "renoteUserId", text)
		 VALUES (?, ?, 'public', ?, (SELECT "userId" FROM "note" WHERE id = ?), ?)`,
		id, userID, target, target, text).Error)
}

// renoteDrifts returns only the renoteCount drifts.
func renoteDrifts(rep Report) []Drift {
	var out []Drift
	for _, d := range rep.Drifts {
		if d.Column == "renoteCount" {
			out = append(out, d)
		}
	}
	return out
}

// 本家は自分のノートのリノートと bot のリノートを renoteCount に数えない
// (`data.renote.userId !== user.id && !user.isBot`)。引用は数える。fsck も
// 同じ条件で数え、記録が小さいときだけを報告する (#3927)。
func TestRun_RenoteCountExcludesSelfAndBotRenotes(t *testing.T) {
	db := newTestDB(t)
	for _, id := range []string{"u1", "u2", "u3", "b1"} {
		seedUser(t, db, id)
	}
	require.NoError(t, db.Exec(`UPDATE "user" SET "isBot" = true WHERE id = 'b1'`).Error)
	seedNote(t, db, "parent", "u1")
	quote := "quote"
	seedRenote(t, db, "self", "u1", "parent", nil)
	seedRenote(t, db, "bot", "b1", "parent", nil)
	seedRenote(t, db, "pure", "u2", "parent", nil)
	seedRenote(t, db, "quote", "u3", "parent", &quote)
	// renoteUserId が空の行 (通常の経路では作られない) は、自分のリノートかを
	// 判定できないので数えない。
	require.NoError(t, db.Exec(
		`INSERT INTO "note" (id, "userId", visibility, "renoteId") VALUES ('unknown', 'u2', 'public', 'parent')`).Error)
	require.NoError(t, db.Exec(`UPDATE "note" SET "renoteCount" = 2 WHERE id = 'parent'`).Error)

	rep, err := Run(context.Background(), db, Options{})
	require.NoError(t, err)
	assert.Empty(t, renoteDrifts(rep), "自分と bot のリノートは数えない")

	// 1 回の増加を失った状態は報告し、-fix で数えた件数まで上げる。
	require.NoError(t, db.Exec(`UPDATE "note" SET "renoteCount" = 1 WHERE id = 'parent'`).Error)
	rep, err = Run(context.Background(), db, Options{Fix: true})
	require.NoError(t, err)
	drifts := renoteDrifts(rep)
	require.Len(t, drifts, 1)
	assert.Equal(t, "parent", drifts[0].ID)
	assert.EqualValues(t, 1, drifts[0].Stored)
	assert.EqualValues(t, 2, drifts[0].Actual)

	var got int64
	require.NoError(t, db.Raw(`SELECT "renoteCount" FROM "note" WHERE id = 'parent'`).Scan(&got).Error)
	assert.EqualValues(t, 2, got)
}

// リノートを消しても本家は renoteCount を減らさないので、記録が数えた件数より
// 大きいのは正常。報告せず、-fix でも下げない (#3927)。
func TestRun_RenoteCountAboveCountedRenotesIsNotDrift(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	seedUser(t, db, "u2")
	seedNote(t, db, "parent", "u1")
	seedRenote(t, db, "rn1", "u2", "parent", nil)
	// 3 回リノートされて 2 件が消えたノート。
	require.NoError(t, db.Exec(`UPDATE "note" SET "renoteCount" = 3 WHERE id = 'parent'`).Error)

	rep, err := Run(context.Background(), db, Options{Fix: true})
	require.NoError(t, err)
	assert.Empty(t, renoteDrifts(rep))

	var got int64
	require.NoError(t, db.Raw(`SELECT "renoteCount" FROM "note" WHERE id = 'parent'`).Scan(&got).Error)
	assert.EqualValues(t, 3, got, "-fix でも下げない")
}

// renoteCount は smallint なので、数えた件数が上限を超えるノートは 32767 で
// 頭打ちにして書き戻す。頭打ちが無いと -fix の UPDATE が範囲外で失敗し続ける。
func TestRun_RenoteCountActualIsCappedAtSmallintMax(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	seedUser(t, db, "u2")
	seedNote(t, db, "parent", "u1")
	require.NoError(t, db.Exec(`INSERT INTO "note" (id, "userId", visibility, "renoteId", "renoteUserId")
		SELECT 'rn' || g, 'u2', 'public', 'parent', 'u1' FROM generate_series(1, 32768) g`).Error)
	analyze(t, db, "note")

	rep, err := Run(context.Background(), db, Options{Fix: true})
	require.NoError(t, err, "-fix が範囲外で失敗しない")
	drifts := renoteDrifts(rep)
	require.Len(t, drifts, 1)
	assert.EqualValues(t, 32767, drifts[0].Actual)

	var got int64
	require.NoError(t, db.Raw(`SELECT "renoteCount" FROM "note" WHERE id = 'parent'`).Scan(&got).Error)
	assert.EqualValues(t, 32767, got)

	rep, err = Run(context.Background(), db, Options{})
	require.NoError(t, err)
	assert.Empty(t, renoteDrifts(rep), "頭打ちの値に直した後は報告しない")
}

// repliesCount も smallint なので、32767 で頭打ちにして書き戻す (#3927)。
// 頭打ちが無いと -fix の UPDATE が範囲外で失敗し、後ろの検査も走らない。
func TestRun_RepliesCountActualIsCappedAtSmallintMax(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	seedNote(t, db, "parent", "u1")
	require.NoError(t, db.Exec(`INSERT INTO "note" (id, "userId", visibility, "replyId")
		SELECT 'rp' || g, 'u1', 'public', 'parent' FROM generate_series(1, 32768) g`).Error)
	analyze(t, db, "note")

	rep, err := Run(context.Background(), db, Options{Fix: true})
	require.NoError(t, err, "-fix が範囲外で失敗しない")
	var drifts []Drift
	for _, d := range rep.Drifts {
		if d.Column == "repliesCount" {
			drifts = append(drifts, d)
		}
	}
	require.Len(t, drifts, 1)
	assert.EqualValues(t, 32767, drifts[0].Actual)

	var got int64
	require.NoError(t, db.Raw(`SELECT "repliesCount" FROM "note" WHERE id = 'parent'`).Scan(&got).Error)
	assert.EqualValues(t, 32767, got)

	rep, err = Run(context.Background(), db, Options{})
	require.NoError(t, err)
	for _, d := range rep.Drifts {
		assert.NotEqual(t, "repliesCount", d.Column, "頭打ちの値に直した後は報告しない: %+v", d)
	}
}

// analyze refreshes the planner statistics of table after a bulk insert.
//
// 直前のテストが行を消した後の統計 (行がほぼ無い) のまま数万行を数えると、
// planner が集計の副問い合わせを外側の行ごとに数え直す nested loop を選び、
// 検査が何分も返らない。本番は autovacuum が統計を保つ。
func analyze(t *testing.T, db *gorm.DB, table string) {
	t.Helper()
	require.NoError(t, db.Exec(`ANALYZE "`+table+`"`).Error)
}

// 増えるだけのカウンタ (消した行の分が残る) だけが raiseOnly で、-fix で下げない。
// 指定が外れても、競合が無ければ上書きと結果が同じなので、他のテストでは
// 気付けない。
func TestCounterChecks_RaiseOnly(t *testing.T) {
	got := map[string]bool{}
	for _, c := range counterChecks {
		got[c.table+"."+c.column] = c.raiseOnly
	}
	assert.Equal(t, map[string]bool{
		"user.followersCount": false,
		"user.followingCount": false,
		"user.notesCount":     true,
		"note.repliesCount":   false,
		"note.renoteCount":    true,
		"note.clippedCount":   false,
		"note.pageCount":      false,
	}, got)
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

// 上限を超えても壊れない (大規模インスタンスで全件を持たない)。集めるのは
// 上限までだが、種類ごとの総数は上限と別に数える (#3927)。
func TestRun_RespectsLimit(t *testing.T) {
	db := newTestDB(t)
	for _, id := range []string{"a3", "a1", "a2"} {
		seedUser(t, db, id)
		seedNote(t, db, "n"+id, id)
	}

	rep, err := Run(context.Background(), db, Options{Limit: 2})
	require.NoError(t, err)
	require.Len(t, rep.Drifts, 2)
	assert.Equal(t, "a1", rep.Drifts[0].ID, "id の順に集める")
	assert.Equal(t, "a2", rep.Drifts[1].ID)
	assert.Equal(t, []DriftTotal{{Table: "user", Column: "notesCount", Count: 3}}, rep.Totals)
	assert.EqualValues(t, 3, rep.DriftTotal())

	// -fix は集めた分だけ直し、残りは次の実行で直る。
	rep, err = Run(context.Background(), db, Options{Limit: 2, Fix: true})
	require.NoError(t, err)
	assert.Equal(t, 2, rep.Repaired)
	rep, err = Run(context.Background(), db, Options{Limit: 2})
	require.NoError(t, err)
	require.Len(t, rep.Drifts, 1)
	assert.Equal(t, "a3", rep.Drifts[0].ID)
	assert.EqualValues(t, 1, rep.DriftTotal())
}

// ずれの無い種類は Totals に出さない。
func TestRun_TotalsOmitCleanCounters(t *testing.T) {
	db := newTestDB(t)
	seedUser(t, db, "u1")
	rep, err := Run(context.Background(), db, Options{})
	require.NoError(t, err)
	assert.Empty(t, rep.Totals)
	assert.Zero(t, rep.DriftTotal())
}

func TestReport_DriftTotal(t *testing.T) {
	r := Report{Totals: []DriftTotal{{Count: 1500}, {Count: 3}}}
	assert.EqualValues(t, 1503, r.DriftTotal())
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
		Drift{Table: "user", Column: "notesCount", ID: "u1", Actual: 1}, true)
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
	analyze(t, db, "clip_note")

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
