package emojiimport_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/core/drive"
	"github.com/shiroha-a/mk/internal/core/emojiimport"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// replaceEmoji 内で既存 local 絵文字が存在する経路をカバーする。
// 既存絵文字を先に Create しておくと、Import 実行時に
// FindByNameAndHost が当たり Delete されてから新規行が作られる。
func TestRun_OverwritesExistingEmoji(t *testing.T) {
	img := pngBytesBoost(t)
	meta := metaJSONBoost(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
	})
	body := buildZipBoost(t, []zipEntryBoost{
		{"meta.json", meta},
		{"smile.png", img},
	})

	userRepo := testutil.NewMockUserRepository()
	_ = userRepo.Create(&model.User{ID: "admin"})
	emojiRepo := testutil.NewMockEmojiRepository()
	// 既存 local 絵文字 smile を事前に登録する。
	_ = emojiRepo.Create(&model.Emoji{ID: "oldid", Name: "smile"})

	fileRepo := testutil.NewMockDriveFileRepository()
	folderRepo := testutil.NewMockDriveFolderRepository()
	folderRepo.FilesRef = fileRepo
	storage := drive.NewLocalStorage(t.TempDir(), "https://example.com/files")
	idGen, _ := id.NewGenerator("aidx")
	uploader := drive.NewService(fileRepo, folderRepo, storage, idGen)

	imp := emojiimport.NewImporter(emojiimport.Deps{
		UserRepo:  userRepo,
		EmojiRepo: emojiRepo,
		Drive:     &fakeDriveReaderBoost{body: body},
		Uploader:  uploader,
		IDGen:     idGen,
	})

	res, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Imported)
	// 新しい ID の絵文字が登録されているはず
	found, err := emojiRepo.FindByNameAndHost("smile", nil)
	require.NoError(t, err)
	assert.NotEqual(t, "oldid", found.ID)
}

// EmojiRepo.Create がエラーを返すと Skipped としてカウントされる。
func TestRun_EmojiRepoCreateError(t *testing.T) {
	img := pngBytesBoost(t)
	meta := metaJSONBoost(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
	})
	body := buildZipBoost(t, []zipEntryBoost{
		{"meta.json", meta},
		{"smile.png", img},
	})

	userRepo := testutil.NewMockUserRepository()
	_ = userRepo.Create(&model.User{ID: "admin"})
	fileRepo := testutil.NewMockDriveFileRepository()
	folderRepo := testutil.NewMockDriveFolderRepository()
	folderRepo.FilesRef = fileRepo
	storage := drive.NewLocalStorage(t.TempDir(), "https://example.com/files")
	idGen, _ := id.NewGenerator("aidx")
	uploader := drive.NewService(fileRepo, folderRepo, storage, idGen)

	imp := emojiimport.NewImporter(emojiimport.Deps{
		UserRepo:  userRepo,
		EmojiRepo: &failingEmojiRepo{inner: testutil.NewMockEmojiRepository()},
		Drive:     &fakeDriveReaderBoost{body: body},
		Uploader:  uploader,
		IDGen:     idGen,
	})

	res, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Skipped)
	assert.Equal(t, 0, res.Imported)
}

// --- helpers duplicated from service_test.go with different names ---
//
// 本ファイルは _test.go として service_test.go と同じパッケージに属するが、
// service_test.go 内で宣言されたヘルパーを再利用すると import サイクルにならない
// ものの、意図的にスコープを絞るためローカル別名で複製している。

type zipEntryBoost struct {
	name string
	body []byte
}

func buildZipBoost(t *testing.T, entries []zipEntryBoost) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.Create(e.name)
		require.NoError(t, err)
		_, err = w.Write(e.body)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func pngBytesBoost(t *testing.T) []byte {
	t.Helper()
	// reuse the service_test.go helper indirectly by re-encoding a tiny PNG.
	return pngBytes(t)
}

func metaJSONBoost(t *testing.T, records []map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"metaVersion": 2,
		"emojis":      records,
	})
	require.NoError(t, err)
	return b
}

type fakeDriveReaderBoost struct {
	body []byte
	err  error
}

func (f *fakeDriveReaderBoost) Fetch(_ string) (*model.DriveFile, []byte, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	return &model.DriveFile{}, f.body, nil
}

// --- failingEmojiRepo makes Create fail while delegating other calls. ---

type failingEmojiRepo struct {
	inner *testutil.MockEmojiRepository
}

func (r *failingEmojiRepo) Create(_ *model.Emoji) error {
	return errors.New("create boom")
}

func (r *failingEmojiRepo) FindByNameAndHost(name string, host *string) (*model.Emoji, error) {
	return r.inner.FindByNameAndHost(name, host)
}

func (r *failingEmojiRepo) FindByID(id string) (*model.Emoji, error) {
	return r.inner.FindByID(id)
}

func (r *failingEmojiRepo) UpdateFields(id string, fields map[string]any) error {
	return r.inner.UpdateFields(id, fields)
}

func (r *failingEmojiRepo) Delete(id string) error { return r.inner.Delete(id) }

func (r *failingEmojiRepo) ListWithFilter(q, c string, l bool, sinceID, untilID string, lim, off int) ([]*model.Emoji, error) {
	return r.inner.ListWithFilter(q, c, l, sinceID, untilID, lim, off)
}

func (r *failingEmojiRepo) ListLocal() ([]*model.Emoji, error) { return r.inner.ListLocal() }

func (r *failingEmojiRepo) FindManyByIDs(ids []string) ([]*model.Emoji, error) {
	return r.inner.FindManyByIDs(ids)
}

func (r *failingEmojiRepo) UpdateFieldsMany(ids []string, fields map[string]any) error {
	return r.inner.UpdateFieldsMany(ids, fields)
}

func (r *failingEmojiRepo) DeleteMany(ids []string) error {
	return r.inner.DeleteMany(ids)
}

func (r *failingEmojiRepo) ListRemoteWithFilter(q, host, sinceID, untilID string, limit, offset int) ([]*model.Emoji, error) {
	return r.inner.ListRemoteWithFilter(q, host, sinceID, untilID, limit, offset)
}

func (r *failingEmojiRepo) ListV2(filter model.EmojiV2Filter) ([]*model.Emoji, error) {
	return r.inner.ListV2(filter)
}

func (r *failingEmojiRepo) CountV2(filter model.EmojiV2Filter) (int64, error) {
	return r.inner.CountV2(filter)
}

func (r *failingEmojiRepo) FindManyByNamesAndHost(names []string, host *string) ([]*model.Emoji, error) {
	return r.inner.FindManyByNamesAndHost(names, host)
}

func (r *failingEmojiRepo) FindManyByKeys(keys []model.EmojiKey) ([]*model.Emoji, error) {
	return r.inner.FindManyByKeys(keys)
}

// sanity: time import stay
var _ = time.Now
