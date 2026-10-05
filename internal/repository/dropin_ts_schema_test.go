package repository

import (
	"sync"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Misskey TS が作った DB には存在しない列。upstream は
// 1697420555911-deleteCreatedAt で app / auth_session の createdAt を DROP
// しており、clip.notesCount はそもそも Clip entity に無い。
//
// mk-go の migration はこれらを `CREATE TABLE IF NOT EXISTS` の中でしか定義
// しておらず、TS 製の既存テーブルに対しては no-op になる。したがって
// mk-go が列を読み書きすると drop-in 環境だけ
// `column "..." of relation "..." does not exist` で落ちる (#2243)。
//
// 本テストは当該列を実際に DROP して TS 製 DB を再現し、mk-go の書き込み /
// 読み出し経路が成立することを担保する。列を足す migration ではなく
// **列への依存を外す** 方針を取ったので、regression はここで検出する。
var tsMissingColumns = []struct{ table, column string }{
	{"app", "createdAt"},
	{"auth_session", "createdAt"},
	{"clip", "notesCount"},
}

// tsDB は TS 形状の専用 schema (`internal_repository_ts`) に繋いだ handle。
//
// **shared な schema の列を DROP して戻す形はやめた** (#2756)。PostgreSQL は
// DROP した列も 1 テーブル 1600 列の上限に数えるので、実行のたびに枠が減り、
// 繰り返し回すと `tables can have at most 1600 columns` で落ちる。専用 schema を
// **一度だけ**その形に作って使い回せば増えない (`DROP COLUMN IF EXISTS` は
// 2 回目以降 no-op)。shared schema を実行中に作り変えないので、同じパッケージの
// 他テストと競合しない利点もある。
var (
	tsDB     *gorm.DB
	tsDBOnce sync.Once
	tsDBErr  error
)

// tsShapedDB returns the lazily-created TS-shaped schema handle.
func tsShapedDB(t *testing.T) *gorm.DB {
	t.Helper()
	tsDBOnce.Do(func() {
		db, err := testutil.OpenTestDBSchema("ts")
		if err != nil {
			tsDBErr = err
			return
		}
		testutil.ApplyMigrations(db)
		for _, c := range tsMissingColumns {
			if err := db.Exec(
				`ALTER TABLE "` + c.table + `" DROP COLUMN IF EXISTS "` + c.column + `"`).Error; err != nil {
				tsDBErr = err
				return
			}
		}
		tsDB = db
	})
	require.NoError(t, tsDBErr)
	require.NotNil(t, tsDB)
	// **前提を毎回確かめる。** これらのテストは「列が無くても動く」ことを見る
	// ので、列が有るままでも通ってしまう (実測で素通りした)。schema が TS 形状に
	// なっていなければ、テストは何も証明していない。
	wantDropped := map[string]int64{}
	for _, c := range tsMissingColumns {
		wantDropped[c.table]++
	}
	for _, c := range tsMissingColumns {
		var n int64
		require.NoError(t, tsDB.Raw(`
			SELECT count(*) FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`,
			c.table, c.column).Scan(&n).Error)
		require.Zero(t, n, "%s.%s が残っている = TS 形状になっていない", c.table, c.column)

		// **落ちている列は、このテストが落とした分だけ。** 増えていれば
		// 「毎回 DROP して戻す」形に逆戻りしている (実行のたびに列枠を食う、#2756)。
		var dropped int64
		require.NoError(t, tsDB.Raw(`
			SELECT count(*) FROM pg_attribute a
			JOIN pg_class c ON c.oid = a.attrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = current_schema() AND c.relname = ? AND a.attisdropped`,
			c.table).Scan(&dropped).Error)
		require.EqualValues(t, wantDropped[c.table], dropped,
			"%s の dropped column が %d 個 (このテストが落とすのは %d 個)。"+
				"実行のたびに DROP して戻す形に戻っていないか確認すること",
			c.table, dropped, wantDropped[c.table])
	}
	return tsDB
}

// insertTSUser is insertTestUser against the TS-shaped schema, with cleanup.
// 兄弟 schema は他のテストと共有しないが、行は実行をまたいで残るので消す。
func insertTSUser(t *testing.T, db *gorm.DB, id, username string) *model.User {
	t.Helper()
	u := insertTestUserOn(t, db, id, username)
	t.Cleanup(func() { db.Exec(`DELETE FROM "user" WHERE id = ?`, id) })
	return u
}

func TestDropIn_TSShapedSchema_AppAndAuthSession(t *testing.T) {
	db := tsShapedDB(t)
	repo := NewAuthSessionRepository(db)
	user := insertTSUser(t, db, "u_dropin_1", "dropinuser")

	app := &model.App{
		ID:          "app_dropin_1",
		UserID:      &user.ID,
		Secret:      "secret_dropin_1",
		Name:        "DropinApp",
		Description: "",
		Permission:  model.StringArray{"read:account"},
	}
	require.NoError(t, repo.CreateApp(app), "app INSERT must not reference createdAt")
	defer db.Exec(`DELETE FROM "app" WHERE id = ?`, app.ID)

	gotApp, err := repo.FindAppBySecret("secret_dropin_1")
	require.NoError(t, err)
	assert.Equal(t, "DropinApp", gotApp.Name)

	session := &model.AuthSession{
		ID:    "sess_dropin_1",
		Token: "token_dropin_1",
		AppID: app.ID,
	}
	require.NoError(t, repo.CreateSession(session), "auth_session INSERT must not reference createdAt")
	defer db.Exec(`DELETE FROM "auth_session" WHERE id = ?`, session.ID)

	gotSession, err := repo.FindSessionByToken("token_dropin_1")
	require.NoError(t, err)
	assert.Equal(t, app.ID, gotSession.AppID)
}

func TestDropIn_TSShapedSchema_Clip(t *testing.T) {
	db := tsShapedDB(t)
	clipRepo := NewClipRepository(db)
	clipNoteRepo := NewClipNoteRepository(db)
	user := insertTSUser(t, db, "u_dropin_2", "dropinclip")

	c := newTestClip("clp_dropin_1", user.ID, "dropin")
	require.NoError(t, clipRepo.Create(c), "clip INSERT must not reference notesCount")
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "clip_note" WHERE "clipId" = ?`, c.ID)
		db.Exec(`DELETE FROM "clip" WHERE id = ?`, c.ID)
	})

	got, err := clipRepo.FindByID(c.ID)
	require.NoError(t, err)
	assert.Equal(t, "dropin", got.Name)

	// notesCount は clip_note の実カウントで出すので、列が無くても数えられる。
	n, err := clipNoteRepo.CountByClip(c.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 0, n)

	note := insertTestNoteOn(t, db, "n_dropin_1", user.ID)
	t.Cleanup(func() { db.Exec(`DELETE FROM "note" WHERE id = ?`, note.ID) })
	require.NoError(t, clipNoteRepo.Create(&model.ClipNote{
		ID: "cn_dropin_1", ClipID: c.ID, NoteID: note.ID,
	}))
	n, err = clipNoteRepo.CountByClip(c.ID)
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)

	rows, err := clipRepo.ListByUser(user.ID, "", "", 10, 0)
	require.NoError(t, err)
	assert.Len(t, rows, 1, "clip SELECT must not reference notesCount")
}
