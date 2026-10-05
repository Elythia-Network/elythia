package emojiimport_test

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/emojiimport"
	"github.com/elythia-network/elythia/internal/core/transfer"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 実 PostgreSQL + 実 repository で emoji import-zip のフルパスを検証する。
// fsck_test.go の newTestDB パターンを踏襲し、自 package schema に閉じた
// 無条件削除を行う (#2450)。

func newIntegrationDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)
	// emoji → drive_file → user の順で FK 安全に削除する。
	for _, q := range []string{
		`DELETE FROM "emoji"`,
		`DELETE FROM "drive_file"`,
		`DELETE FROM "user"`,
	} {
		require.NoError(t, db.Exec(q).Error)
	}
	return db
}

func seedIntegrationUser(t *testing.T, db *gorm.DB, id string) {
	t.Helper()
	require.NoError(t, db.Exec(
		`INSERT INTO "user" (id, username, "usernameLower", "avatarDecorations") VALUES (?, ?, ?, '[]')`,
		id, id, id).Error)
}

// newIntegrationImporter wires the same components as router.go:609-638:
// real repositories + drive.Service + transfer.RepoBackedDriveReader.
func newIntegrationImporter(t *testing.T, db *gorm.DB) (*emojiimport.Importer, *drive.Service) {
	t.Helper()
	userRepo := repository.NewUserRepository(db)
	emojiRepo := repository.NewEmojiRepository(db)
	fileRepo := repository.NewDriveFileRepository(db)
	folderRepo := repository.NewDriveFolderRepository(db)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	storage := drive.NewLocalStorage(t.TempDir(), "https://example.com/files")
	driveService := drive.NewService(fileRepo, folderRepo, storage, idGen)
	driveReader := transfer.NewRepoBackedDriveReader(fileRepo, storage)
	imp := emojiimport.NewImporter(emojiimport.Deps{
		UserRepo:  userRepo,
		EmojiRepo: emojiRepo,
		Drive:     driveReader,
		Uploader:  driveService,
		IDGen:     idGen,
	})
	return imp, driveService
}

// uploadZip stores body as a system-owned drive_file (User: nil) and returns
// the created row, mirroring the admin uploads before calling import-zip.
func uploadZip(t *testing.T, driveService *drive.Service, body []byte) *model.DriveFile {
	t.Helper()
	df, err := driveService.Upload(context.Background(), drive.UploadInput{
		Body:  body,
		Name:  "emojis.zip",
		Force: true,
	})
	require.NoError(t, err)
	return df
}

func TestImport_Integration_HappyPath(t *testing.T) {
	db := newIntegrationDB(t)
	seedIntegrationUser(t, db, "admin")
	imp, driveService := newIntegrationImporter(t, db)

	img := pngBytes(t)
	meta := metaJSON(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "smile", "category": "faces"}},
		{"fileName": "wave.png", "downloaded": true, "emoji": map[string]any{"name": "wave"}},
	})
	zipBody := buildZip(t, []zipEntry{{"meta.json", meta}, {"smile.png", img}, {"wave.png", img}})
	zipFile := uploadZip(t, driveService, zipBody)

	res, err := imp.Run(context.Background(), "admin", zipFile.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Total)
	assert.Equal(t, 2, res.Imported)
	assert.Equal(t, 0, res.Skipped)

	var count int64
	require.NoError(t, db.Raw(`SELECT COUNT(*) FROM "emoji"`).Scan(&count).Error)
	assert.EqualValues(t, 2, count)

	var category string
	require.NoError(t, db.Raw(`SELECT category FROM "emoji" WHERE name = 'smile'`).Scan(&category).Error)
	assert.Equal(t, "faces", category)
}

func TestImport_Integration_SystemOwnedAndOriginalUrl(t *testing.T) {
	db := newIntegrationDB(t)
	seedIntegrationUser(t, db, "admin")
	imp, driveService := newIntegrationImporter(t, db)

	img := pngBytes(t)
	meta := metaJSON(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
	})
	zipBody := buildZip(t, []zipEntry{{"meta.json", meta}, {"smile.png", img}})
	zipFile := uploadZip(t, driveService, zipBody)

	_, err := imp.Run(context.Background(), "admin", zipFile.ID)
	require.NoError(t, err)

	// emoji と対応 drive_file を DB から直接読んで不変条件をアサートする。
	var em struct {
		Name        string
		OriginalURL string
		PublicURL   string
		Type        *string
	}
	require.NoError(t, db.Raw(
		`SELECT name, "originalUrl", "publicUrl", type FROM "emoji" WHERE name = 'smile'`).Scan(&em).Error)

	var df struct {
		UserID *string
		URL    string
		Type   string
	}
	require.NoError(t, db.Raw(
		`SELECT "userId", url, type FROM "drive_file" WHERE url = ?`, em.OriginalURL).Scan(&df).Error)

	// 画像は system 所有 (userId NULL) で保存される (#670)。
	assert.Nil(t, df.UserID, "import された emoji 画像は system 所有")
	// 不変条件 (#722): emoji.originalUrl == drive_file.url。
	assert.Equal(t, df.URL, em.OriginalURL)
	// imageProcessor 未配線なので webpublic は無く、publicUrl / type は本体に一致する。
	assert.Equal(t, df.URL, em.PublicURL)
	require.NotNil(t, em.Type)
	assert.Equal(t, df.Type, *em.Type)
}

func TestImport_Integration_MissingMeta(t *testing.T) {
	db := newIntegrationDB(t)
	seedIntegrationUser(t, db, "admin")
	imp, driveService := newIntegrationImporter(t, db)

	zipBody := buildZip(t, []zipEntry{{"only.png", pngBytes(t)}})
	zipFile := uploadZip(t, driveService, zipBody)

	_, err := imp.Run(context.Background(), "admin", zipFile.ID)
	assert.ErrorIs(t, err, emojiimport.ErrMissingMeta)

	var count int64
	require.NoError(t, db.Raw(`SELECT COUNT(*) FROM "emoji"`).Scan(&count).Error)
	assert.EqualValues(t, 0, count)
}
