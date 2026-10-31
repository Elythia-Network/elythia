package repository

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/model"
)

type pageFK struct {
	Name       string `gorm:"column:conname"`
	DeleteRule string `gorm:"column:confdeltype"`
	RefTable   string `gorm:"column:reftable"`
}

// pageEyeCatchingFKs lists the foreign keys of page.eyeCatchingImageId in the
// current schema. pg_constraint は全 schema の行を返すので、pg_namespace で今の
// schema に絞る (#2777)。
func pageEyeCatchingFKs(t *testing.T) []pageFK {
	t.Helper()
	var rows []pageFK
	require.NoError(t, testDB.Raw(`
		SELECT c.conname, c.confdeltype::text AS confdeltype, f.relname AS reftable
		FROM pg_constraint c
		JOIN pg_class r ON r.oid = c.conrelid
		JOIN pg_class f ON f.oid = c.confrelid
		JOIN pg_namespace n ON n.oid = r.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
		WHERE n.nspname = current_schema()
		  AND r.relname = 'page'
		  AND a.attname = 'eyeCatchingImageId'
		  AND c.contype = 'f'
		ORDER BY c.conname`).Scan(&rows).Error)
	return rows
}

// #3369: page.eyeCatchingImageId には、本家と同じ名前の drive_file への外部キー
// (ON DELETE SET NULL) がある。張る前に、どの drive file も指していない id と
// 空文字を NULL にする。
//
// **migration をテストの中で流し直す。** テスト用の schema は実行をまたいで残る
// ので、適用済みの状態を見るだけだと、migration の中身を変えても緑のまま通る。
// down で外部キーの無い状態に戻してから up を流し、その結果を見る。
func TestMigration000139_PageEyeCatchingImageFKLikeUpstream(t *testing.T) {
	up := migrationSQL(t, "000139_page_eye_catching_image_fk.up.sql")
	down := migrationSQL(t, "000139_page_eye_catching_image_fk.down.sql")
	want := []pageFK{{"FK_a9ca79ad939bf06066b81c9d3aa", "n", "drive_file"}}

	require.NoError(t, testDB.Exec(down).Error)
	t.Cleanup(func() {
		if err := testDB.Exec(up).Error; err != nil {
			t.Errorf("000139 の up で schema を戻せなかった: %v", err)
		}
	})
	assert.Empty(t, pageEyeCatchingFKs(t), "down は外部キーを落とす")

	user := insertTestUser(t, "u_pgfk_1", "pgfkuser1")
	defer cleanupUser(t, user.ID)
	uid := user.ID
	file := &model.DriveFile{
		ID: "f_pgfk_1", UserID: &uid, MD5: "x", Name: "x", Type: "image/png", Size: 1,
		StoredInternal: true, URL: "u", Properties: datatypes.JSON([]byte("{}")),
		RequestHeaders: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, testDB.Create(file).Error)
	defer cleanupDriveFile(t, file.ID)

	// 外部キーが無い間に書けた値。
	pages := map[string]*string{"pg_pgfk_ok": &file.ID, "pg_pgfk_ghost": ptrString("f_pgfk_ghost"), "pg_pgfk_empty": ptrString(""), "pg_pgfk_none": nil}
	for id, img := range pages {
		p := newTestPage(id, user.ID, id)
		p.EyeCatchingImageID = img
		require.NoError(t, testDB.Create(p).Error)
		defer cleanupPage(t, id)
	}

	require.NoError(t, testDB.Exec(up).Error)
	assert.Equal(t, want, pageEyeCatchingFKs(t))

	imageOf := func(id string) *string {
		var p model.Page
		require.NoError(t, testDB.First(&p, "id = ?", id).Error)
		return p.EyeCatchingImageID
	}
	require.NotNil(t, imageOf("pg_pgfk_ok"), "drive file を指す id は残す")
	assert.Equal(t, file.ID, *imageOf("pg_pgfk_ok"))
	assert.Nil(t, imageOf("pg_pgfk_ghost"), "どの drive file も指していない id は NULL にする")
	assert.Nil(t, imageOf("pg_pgfk_empty"), "空文字は NULL にする")
	assert.Nil(t, imageOf("pg_pgfk_none"))

	// TS 版から引き継いだ DB: 本家の制約が既にある。流しても失敗せず、重複も作らない。
	require.NoError(t, testDB.Exec(up).Error)
	assert.Equal(t, want, pageEyeCatchingFKs(t))

	// 本家の NonCascadingPageEyeCatching より前の TS 版の DB: 同じ名前の CASCADE と、
	// さらに古い名前の CASCADE。どちらも落として SET NULL の 1 本にする。
	for _, name := range []string{"FK_a9ca79ad939bf06066b81c9d3aa", "FK_3126dd7c502c9e4d7597ef7ef10"} {
		require.NoError(t, testDB.Exec(down).Error)
		require.NoError(t, testDB.Exec(`ALTER TABLE "page" ADD CONSTRAINT "`+name+
			`" FOREIGN KEY ("eyeCatchingImageId") REFERENCES "drive_file"("id") ON DELETE CASCADE ON UPDATE NO ACTION`).Error)
		require.NoError(t, testDB.Exec(up).Error)
		assert.Equal(t, want, pageEyeCatchingFKs(t), "CASCADE の %s は SET NULL に張り直す", name)
	}

	// 存在しない drive file は書けない。
	bad := newTestPage("pg_pgfk_bad", user.ID, "pg_pgfk_bad")
	bad.EyeCatchingImageID = ptrString("f_pgfk_ghost")
	assert.Error(t, testDB.Create(bad).Error)
	defer cleanupPage(t, bad.ID)

	// drive file を消すと、本家と同じく Page は残って画像が外れる。
	require.NoError(t, testDB.Exec(`DELETE FROM "drive_file" WHERE id = ?`, file.ID).Error)
	assert.Nil(t, imageOf("pg_pgfk_ok"))
}
