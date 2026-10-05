package processors_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/emojiimport"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEmojiDriveReader feeds a fixed body to the importer regardless of id.
type fakeEmojiDriveReader struct {
	body []byte
	err  error
}

func (f *fakeEmojiDriveReader) Fetch(_ string) (*model.DriveFile, []byte, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	return &model.DriveFile{}, f.body, nil
}

func buildEmojiZip(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var imgBuf bytes.Buffer
	require.NoError(t, png.Encode(&imgBuf, img))

	meta, _ := json.Marshal(map[string]any{
		"metaVersion": 2,
		"emojis": []map[string]any{
			{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
		},
	})

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mw, err := zw.Create("meta.json")
	require.NoError(t, err)
	_, err = mw.Write(meta)
	require.NoError(t, err)
	iw, err := zw.Create("smile.png")
	require.NoError(t, err)
	_, err = iw.Write(imgBuf.Bytes())
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func newEmojiImporter(t *testing.T, reader emojiimport.DriveReader) *emojiimport.Importer {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	_ = userRepo.Create(&model.User{ID: "admin"})
	fileRepo := testutil.NewMockDriveFileRepository()
	folderRepo := testutil.NewMockDriveFolderRepository()
	folderRepo.FilesRef = fileRepo
	storage := drive.NewLocalStorage(t.TempDir(), "https://example.com/files")
	idGen, _ := id.NewGenerator("aidx")
	uploader := drive.NewService(fileRepo, folderRepo, storage, idGen)
	return emojiimport.NewImporter(emojiimport.Deps{
		UserRepo:  userRepo,
		EmojiRepo: testutil.NewMockEmojiRepository(),
		Drive:     reader,
		Uploader:  uploader,
		IDGen:     idGen,
	})
}

// --- tests ---

func TestImportCustomEmojisProcessor_NotConfigured(t *testing.T) {
	p := processors.NewImportCustomEmojisProcessor(nil)
	task := queue.NewImportCustomEmojisTask(queue.ImportCustomEmojisPayload{UserID: "admin", FileID: "f1"})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestImportCustomEmojisProcessor_BadPayload(t *testing.T) {
	imp := newEmojiImporter(t, &fakeEmojiDriveReader{body: buildEmojiZip(t)})
	p := processors.NewImportCustomEmojisProcessor(imp)
	task := driver.RawTask{TypeName: queue.TaskTypeImportCustomEmojis, Body: []byte("not json")}
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestImportCustomEmojisProcessor_MissingFields(t *testing.T) {
	imp := newEmojiImporter(t, &fakeEmojiDriveReader{body: buildEmojiZip(t)})
	p := processors.NewImportCustomEmojisProcessor(imp)

	cases := []queue.ImportCustomEmojisPayload{
		{UserID: "", FileID: "f"},
		{UserID: "u", FileID: ""},
	}
	for _, c := range cases {
		task := queue.NewImportCustomEmojisTask(c)
		err := p.Handle(context.Background(), task)
		require.Error(t, err)
		assert.ErrorIs(t, err, driver.ErrSkipRetry)
	}
}

func TestImportCustomEmojisProcessor_Success(t *testing.T) {
	imp := newEmojiImporter(t, &fakeEmojiDriveReader{body: buildEmojiZip(t)})
	p := processors.NewImportCustomEmojisProcessor(imp)
	task := queue.NewImportCustomEmojisTask(queue.ImportCustomEmojisPayload{UserID: "admin", FileID: "f1"})
	require.NoError(t, p.Handle(context.Background(), task))
}

func TestImportCustomEmojisProcessor_InvalidZip_SkipRetry(t *testing.T) {
	imp := newEmojiImporter(t, &fakeEmojiDriveReader{body: []byte("not a zip")})
	p := processors.NewImportCustomEmojisProcessor(imp)
	task := queue.NewImportCustomEmojisTask(queue.ImportCustomEmojisPayload{UserID: "admin", FileID: "f1"})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
	assert.ErrorIs(t, err, emojiimport.ErrInvalidZip)
}

func TestImportCustomEmojisProcessor_UserNotFound_SkipRetry(t *testing.T) {
	imp := newEmojiImporter(t, &fakeEmojiDriveReader{body: buildEmojiZip(t)})
	p := processors.NewImportCustomEmojisProcessor(imp)
	task := queue.NewImportCustomEmojisTask(queue.ImportCustomEmojisPayload{UserID: "ghost", FileID: "f1"})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
	assert.ErrorIs(t, err, emojiimport.ErrUserNotFound)
}

func TestImportCustomEmojisProcessor_DriveError_SkipRetry(t *testing.T) {
	imp := newEmojiImporter(t, &fakeEmojiDriveReader{err: errors.New("drive down")})
	p := processors.NewImportCustomEmojisProcessor(imp)
	task := queue.NewImportCustomEmojisTask(queue.ImportCustomEmojisPayload{UserID: "admin", FileID: "f1"})
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
	assert.ErrorIs(t, err, emojiimport.ErrDriveFileNotFound)
}

// 展開後サイズ超過は恒久エラーなので ErrSkipRetry を付ける。付けないと、
// mkq の既定 (MaxAttempts なし = リトライ無し) が変わったときに同じ zip を
// 何度も展開しようとする。
func TestImportCustomEmojisProcessor_ZipEntryTooLarge_SkipRetry(t *testing.T) {
	// meta.json の上限 (64MiB) を超えるエントリ。**ヘッダで弾かれるので
	// 展開はされない** (fixture の構築だけが 64MiB を使う)。
	oversized := bytes.Repeat([]byte("a"), 64<<20+1)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("meta.json")
	require.NoError(t, err)
	_, err = w.Write(oversized)
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	imp := newEmojiImporter(t, &fakeEmojiDriveReader{body: buf.Bytes()})
	p := processors.NewImportCustomEmojisProcessor(imp)
	task := queue.NewImportCustomEmojisTask(queue.ImportCustomEmojisPayload{UserID: "admin", FileID: "f1"})
	err = p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
	assert.ErrorIs(t, err, emojiimport.ErrZipEntryTooLarge)
}
