package testutil

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gorm.io/gorm"
)

// cherryPickFixture is the schema and the migrations records CherryPick
// 4.18.0-beta.1 leaves after its migrations, written by
// tests/cherrypick/gen-fixture.sh (#4247).
const cherryPickFixture = "internal/testutil/testdata/cherrypick-4.18.0-beta.1.sql.gz"

// LoadCherryPickSchema creates, in the current schema of db, the tables,
// types, indexes and migrations records of a CherryPick 4.18.0-beta.1
// database. It holds no other rows. Call it on an empty schema
// (OpenTestDBSchema, then DROP / CREATE SCHEMA).
//
// 中身は `public.` の修飾を落としてあるので、search_path の schema に作られる。
func LoadCherryPickSchema(db *gorm.DB) error {
	f, err := os.Open(filepath.Join(projectRoot(), cherryPickFixture))
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("read %s: %w", cherryPickFixture, err)
	}
	sql, err := io.ReadAll(zr)
	if err != nil {
		return fmt.Errorf("read %s: %w", cherryPickFixture, err)
	}
	// 引数の無い Exec は単純プロトコルで送られるので、複数の文をまとめて流せる。
	if err := db.Exec(string(sql)).Error; err != nil {
		return fmt.Errorf("load %s: %w", cherryPickFixture, err)
	}
	return nil
}
