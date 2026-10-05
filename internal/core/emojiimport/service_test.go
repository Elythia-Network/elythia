package emojiimport_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/emojiimport"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- helpers ---

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

type zipEntry struct {
	name string
	body []byte
}

func buildZip(t *testing.T, entries []zipEntry) []byte {
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

// metaJSON returns a Misskey-compatible meta.json body for the given records.
func metaJSON(t *testing.T, records []map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"metaVersion": 2,
		"host":        nil,
		"exportedAt":  time.Now().UTC().Format(time.RFC3339),
		"emojis":      records,
	})
	require.NoError(t, err)
	return b
}

// fakeDriveReader returns a fixed body (or error) for any fileID.
type fakeDriveReader struct {
	body []byte
	err  error
}

func (f *fakeDriveReader) Fetch(_ string) (*model.DriveFile, []byte, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	return &model.DriveFile{}, f.body, nil
}

func newUploader(t *testing.T) *drive.Service {
	t.Helper()
	fileRepo := testutil.NewMockDriveFileRepository()
	folderRepo := testutil.NewMockDriveFolderRepository()
	folderRepo.FilesRef = fileRepo
	storage := drive.NewLocalStorage(t.TempDir(), "https://example.com/files")
	idGen, _ := id.NewGenerator("aidx")
	return drive.NewService(fileRepo, folderRepo, storage, idGen)
}

func newDeps(t *testing.T, body []byte) (emojiimport.Deps, *testutil.MockUserRepository, *testutil.MockEmojiRepository, *drive.Service) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	_ = userRepo.Create(&model.User{ID: "admin"})
	emojiRepo := testutil.NewMockEmojiRepository()
	uploader := newUploader(t)
	reader := &fakeDriveReader{body: body}
	idGen, _ := id.NewGenerator("aidx")
	return emojiimport.Deps{
		UserRepo:  userRepo,
		EmojiRepo: emojiRepo,
		Drive:     reader,
		Uploader:  uploader,
		IDGen:     idGen,
	}, userRepo, emojiRepo, uploader
}

// --- Run ---

func TestRun_HappyPath_MultipleEmojis(t *testing.T) {
	img := pngBytes(t)
	meta := metaJSON(t, []map[string]any{
		{
			"fileName":   "smile.png",
			"downloaded": true,
			"emoji": map[string]any{
				"name":        "smile",
				"category":    "faces",
				"aliases":     []string{"happy", "joy"},
				"license":     "CC0",
				"isSensitive": false,
				"localOnly":   false,
			},
		},
		{
			"fileName":   "wave.png",
			"downloaded": true,
			"emoji": map[string]any{
				"name":    "wave",
				"aliases": []string{},
			},
		},
	})
	body := buildZip(t, []zipEntry{
		{"meta.json", meta},
		{"smile.png", img},
		{"wave.png", img},
	})
	deps, _, repo, _ := newDeps(t, body)
	imp := emojiimport.NewImporter(deps)

	res, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 2, res.Total)
	assert.Equal(t, 2, res.Imported)
	assert.Equal(t, 0, res.Skipped)

	smile, err := repo.FindByNameAndHost("smile", nil)
	require.NoError(t, err)
	assert.Equal(t, "CC0", *smile.License)
	require.NotNil(t, smile.Category)
	assert.Equal(t, "faces", *smile.Category)
	assert.Equal(t, []string{"happy", "joy"}, []string(smile.Aliases))

	wave, err := repo.FindByNameAndHost("wave", nil)
	require.NoError(t, err)
	assert.Nil(t, wave.Category)
}

func TestRun_DeletesExistingLocalEmoji(t *testing.T) {
	img := pngBytes(t)
	meta := metaJSON(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
	})
	body := buildZip(t, []zipEntry{{"meta.json", meta}, {"smile.png", img}})
	deps, _, repo, _ := newDeps(t, body)
	// 既存ローカル smile を置いて、import 後に ID が更新されていることを確認する。
	_ = repo.Create(&model.Emoji{ID: "old", Name: "smile"})

	imp := emojiimport.NewImporter(deps)
	_, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)

	found, err := repo.FindByNameAndHost("smile", nil)
	require.NoError(t, err)
	assert.NotEqual(t, "old", found.ID)
}

func TestRun_SkipsInvalidRecords(t *testing.T) {
	img := pngBytes(t)
	meta := metaJSON(t, []map[string]any{
		// not downloaded
		{"fileName": "a.png", "downloaded": false, "emoji": map[string]any{"name": "a"}},
		// invalid file name
		{"fileName": "bad name.png", "downloaded": true, "emoji": map[string]any{"name": "bad"}},
		// invalid emoji name
		{"fileName": "c.png", "downloaded": true, "emoji": map[string]any{"name": "bad!name"}},
		// missing zip entry
		{"fileName": "missing.png", "downloaded": true, "emoji": map[string]any{"name": "miss"}},
	})
	body := buildZip(t, []zipEntry{{"meta.json", meta}, {"a.png", img}, {"c.png", img}})
	deps, _, repo, _ := newDeps(t, body)
	imp := emojiimport.NewImporter(deps)

	res, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 4, res.Total)
	assert.Equal(t, 0, res.Imported)
	assert.Equal(t, 4, res.Skipped)
	assert.Empty(t, repo.Emojis)
}

func TestRun_MissingMeta(t *testing.T) {
	body := buildZip(t, []zipEntry{{"only.png", []byte("x")}})
	deps, _, _, _ := newDeps(t, body)
	imp := emojiimport.NewImporter(deps)
	_, err := imp.Run(context.Background(), "admin", "f1")
	assert.ErrorIs(t, err, emojiimport.ErrMissingMeta)
}

func TestRun_MalformedMeta(t *testing.T) {
	body := buildZip(t, []zipEntry{{"meta.json", []byte("not json")}})
	deps, _, _, _ := newDeps(t, body)
	imp := emojiimport.NewImporter(deps)
	_, err := imp.Run(context.Background(), "admin", "f1")
	assert.ErrorIs(t, err, emojiimport.ErrMalformedMeta)
}

func TestRun_InvalidZip(t *testing.T) {
	deps, _, _, _ := newDeps(t, []byte("not a zip"))
	imp := emojiimport.NewImporter(deps)
	_, err := imp.Run(context.Background(), "admin", "f1")
	assert.ErrorIs(t, err, emojiimport.ErrInvalidZip)
}

func TestRun_DriveFetchError(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	_ = userRepo.Create(&model.User{ID: "admin"})
	idGen, _ := id.NewGenerator("aidx")
	deps := emojiimport.Deps{
		UserRepo:  userRepo,
		EmojiRepo: testutil.NewMockEmojiRepository(),
		Drive:     &fakeDriveReader{err: errors.New("boom")},
		Uploader:  newUploader(t),
		IDGen:     idGen,
	}
	imp := emojiimport.NewImporter(deps)
	_, err := imp.Run(context.Background(), "admin", "f1")
	assert.ErrorIs(t, err, emojiimport.ErrDriveFileNotFound)
}

func TestRun_UserNotFound(t *testing.T) {
	deps, _, _, _ := newDeps(t, []byte{})
	imp := emojiimport.NewImporter(deps)
	_, err := imp.Run(context.Background(), "ghost", "f1")
	assert.ErrorIs(t, err, emojiimport.ErrUserNotFound)
}

func TestRun_MissingDeps(t *testing.T) {
	imp := emojiimport.NewImporter(emojiimport.Deps{})
	_, err := imp.Run(context.Background(), "admin", "f1")
	assert.Error(t, err)
}

func TestRun_ContextCancelled(t *testing.T) {
	img := pngBytes(t)
	meta := metaJSON(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
	})
	body := buildZip(t, []zipEntry{{"meta.json", meta}, {"smile.png", img}})
	deps, _, _, _ := newDeps(t, body)
	imp := emojiimport.NewImporter(deps)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := imp.Run(ctx, "admin", "f1")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestRun_UploaderError(t *testing.T) {
	img := pngBytes(t)
	meta := metaJSON(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
	})
	body := buildZip(t, []zipEntry{{"meta.json", meta}, {"smile.png", img}})
	// broken storage causes Upload to fail (storage Put fails).
	userRepo := testutil.NewMockUserRepository()
	_ = userRepo.Create(&model.User{ID: "admin"})
	emojiRepo := testutil.NewMockEmojiRepository()
	storage := drive.NewLocalStorage("/nonexistent/\x00invalid", "")
	fileRepo := testutil.NewMockDriveFileRepository()
	folderRepo := testutil.NewMockDriveFolderRepository()
	idGen, _ := id.NewGenerator("aidx")
	uploader := drive.NewService(fileRepo, folderRepo, storage, idGen)
	deps := emojiimport.Deps{
		UserRepo:  userRepo,
		EmojiRepo: emojiRepo,
		Drive:     &fakeDriveReader{body: body},
		Uploader:  uploader,
		IDGen:     idGen,
	}
	imp := emojiimport.NewImporter(deps)
	res, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Skipped)
	assert.Equal(t, 0, res.Imported)
}

func TestSetNow(t *testing.T) {
	deps, _, _, _ := newDeps(t, []byte{})
	imp := emojiimport.NewImporter(deps)
	imp.SetNow(func() time.Time { return time.Unix(42, 0) })
	imp.SetNow(nil) // nil must be ignored
}

// --- 対象 2/3/4/5 用ヘルパー (既存 newDeps は変えずに追加) ---

// newDepsWithFileRepo は newDeps に drive file repository を足したもの。
// import で Drive に保存された実体を検査する新テストだけが使う。
func newDepsWithFileRepo(t *testing.T, body []byte) (emojiimport.Deps, *testutil.MockUserRepository, *testutil.MockEmojiRepository, *testutil.MockDriveFileRepository, *drive.Service) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	_ = userRepo.Create(&model.User{ID: "admin"})
	emojiRepo := testutil.NewMockEmojiRepository()
	fileRepo := testutil.NewMockDriveFileRepository()
	folderRepo := testutil.NewMockDriveFolderRepository()
	folderRepo.FilesRef = fileRepo
	storage := drive.NewLocalStorage(t.TempDir(), "https://example.com/files")
	idGen, _ := id.NewGenerator("aidx")
	uploader := drive.NewService(fileRepo, folderRepo, storage, idGen)
	reader := &fakeDriveReader{body: body}
	return emojiimport.Deps{
		UserRepo:  userRepo,
		EmojiRepo: emojiRepo,
		Drive:     reader,
		Uploader:  uploader,
		IDGen:     idGen,
	}, userRepo, emojiRepo, fileRepo, uploader
}

// pngBytesSize encodes a w×h RGBA image as PNG. 同名エントリを内容で区別する
// ためにサイズ違いの PNG を作れる。
func pngBytesSize(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// md5Hex returns the hex-encoded MD5 of b, matching drive.AnalyseFile's digest.
func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// --- 対象 2: zip ディレクトリ構造 / パストラバーサル ---
//
// エントリ索引は path.Base で正規化される (service.go L143) ため、ディレクトリを
// 含むエントリ名でも basename として解決される。実装はファイル書出しを一切
// 行わず (メモリ上で zip を読み Drive へ Upload するだけ)、外部パスへ
// 読み書きする経路が無いことを固定する。

func TestRun_NestedDirectoryEntry(t *testing.T) {
	img := pngBytes(t)
	meta := metaJSON(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
	})
	body := buildZip(t, []zipEntry{
		{"meta.json", meta},
		{"dir/smile.png", img},
	})
	deps, _, repo, _ := newDeps(t, body)
	imp := emojiimport.NewImporter(deps)

	res, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Imported)
	assert.Equal(t, 0, res.Skipped)
	_, err = repo.FindByNameAndHost("smile", nil)
	require.NoError(t, err)
}

func TestRun_MetaInSubdirectory(t *testing.T) {
	img := pngBytes(t)
	meta := metaJSON(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
	})
	body := buildZip(t, []zipEntry{
		{"nested/meta.json", meta},
		{"dir/smile.png", img},
	})
	deps, _, repo, _ := newDeps(t, body)
	imp := emojiimport.NewImporter(deps)

	res, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Imported)
	assert.Equal(t, 0, res.Skipped)
	_, err = repo.FindByNameAndHost("smile", nil)
	require.NoError(t, err)
}

func TestRun_PathTraversalEntry(t *testing.T) {
	// `../../evil.png` 相当のエントリ名も path.Base で `evil.png` に正規化される。
	// 実体が画像でなくても import され、ディスク上の外部パスには一切触れない
	// (in-memory で zip を読み、内容をそのまま Drive へ Upload するだけ)。
	meta := metaJSON(t, []map[string]any{
		{"fileName": "evil.png", "downloaded": true, "emoji": map[string]any{"name": "evil"}},
	})
	body := buildZip(t, []zipEntry{
		{"meta.json", meta},
		{"../../evil.png", []byte("not an image but still stored")},
	})
	deps, _, repo, _ := newDeps(t, body)
	imp := emojiimport.NewImporter(deps)

	res, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Imported)
	assert.Equal(t, 0, res.Skipped)
	_, err = repo.FindByNameAndHost("evil", nil)
	require.NoError(t, err)
}

func TestRun_DirectoryEntry(t *testing.T) {
	img := pngBytes(t)
	meta := metaJSON(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
	})
	// 末尾スラッシュの directory entry を含んでもエラーにならず、実ファイルだけ
	// import される。
	body := buildZip(t, []zipEntry{
		{"meta.json", meta},
		{"dir/", nil},
		{"smile.png", img},
	})
	deps, _, repo, _ := newDeps(t, body)
	imp := emojiimport.NewImporter(deps)

	res, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Imported)
	assert.Equal(t, 0, res.Skipped)
	_, err = repo.FindByNameAndHost("smile", nil)
	require.NoError(t, err)
}

// パストラバーサルは zip エントリ名側だけでなく、lookup キーになる meta.json の
// fileName 側も制御される。validFileName が `/` を弾くので fileName が traversal
// を含んでいれば Skipped になることを固定する (正規表現が緩められたときに検出)。
func TestRun_MetaFileNameTraversalSkipped(t *testing.T) {
	meta := metaJSON(t, []map[string]any{
		{"fileName": "../evil.png", "downloaded": true, "emoji": map[string]any{"name": "evil"}},
	})
	body := buildZip(t, []zipEntry{
		{"meta.json", meta},
		{"evil.png", []byte("not an image but still stored")},
	})
	deps, _, repo, _ := newDeps(t, body)
	imp := emojiimport.NewImporter(deps)

	res, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Total)
	assert.Equal(t, 0, res.Imported)
	assert.Equal(t, 1, res.Skipped)
	assert.Empty(t, repo.Emojis)
}

// meta.json/ のような名前のディレクトリエントリは meta.json として採用されず、
// 正しい meta.json だけが使われる (#IsDir で弾く)。
func TestRun_MetaJSONDirectoryEntryIgnored(t *testing.T) {
	img := pngBytes(t)
	meta := metaJSON(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
	})
	// `meta.json/` は path.Base で `meta.json` になるがディレクトリなので対象外。
	// これが採用されると readZipEntry が空を返して ErrMalformedMeta になる。
	body := buildZip(t, []zipEntry{
		{"meta.json", meta},
		{"meta.json/", nil},
		{"smile.png", img},
	})
	deps, _, repo, _ := newDeps(t, body)
	imp := emojiimport.NewImporter(deps)

	res, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Imported)
	assert.Equal(t, 0, res.Skipped)
	_, err = repo.FindByNameAndHost("smile", nil)
	require.NoError(t, err)
}

// --- 対象 3: 同名エントリの重複 (index は後勝ち) ---

func TestRun_DuplicateImageEntry_LastWins(t *testing.T) {
	first := pngBytesSize(t, 2, 2)
	second := pngBytesSize(t, 3, 3)
	meta := metaJSON(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
	})
	body := buildZip(t, []zipEntry{
		{"meta.json", meta},
		{"smile.png", first},
		{"smile.png", second}, // 同名エントリは index で後勝ち
	})
	deps, _, repo, fileRepo, _ := newDepsWithFileRepo(t, body)
	imp := emojiimport.NewImporter(deps)

	res, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Imported)
	assert.Equal(t, 0, res.Skipped)

	// drive file は 1 つだけ作られ、その内容 (= MD5) は 2 つ目の PNG と一致する。
	require.Len(t, fileRepo.Files, 1)
	var df *model.DriveFile
	for _, f := range fileRepo.Files {
		df = f
	}
	require.NotNil(t, df)
	assert.Equal(t, md5Hex(second), df.MD5, "後勝ちの 2 つ目が保存される")

	// emoji の originalUrl もその drive file を指す。
	smile, err := repo.FindByNameAndHost("smile", nil)
	require.NoError(t, err)
	assert.Equal(t, df.URL, smile.OriginalURL)
}

func TestRun_DuplicateMetaJSON_LastWins(t *testing.T) {
	img := pngBytes(t)
	// 先頭は不正な emoji 名を持つ meta、末尾が正しい meta。後勝ちで末尾が使われる。
	badMeta := metaJSON(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "bad!name"}},
	})
	goodMeta := metaJSON(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
	})
	body := buildZip(t, []zipEntry{
		{"meta.json", badMeta},
		{"meta.json", goodMeta},
		{"smile.png", img},
	})
	deps, _, repo, _ := newDeps(t, body)
	imp := emojiimport.NewImporter(deps)

	res, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Imported)
	assert.Equal(t, 0, res.Skipped)
	_, err = repo.FindByNameAndHost("smile", nil)
	require.NoError(t, err)
}

func TestRun_DuplicateMetaJSON_LastBrokenFails(t *testing.T) {
	img := pngBytes(t)
	goodMeta := metaJSON(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
	})
	// 末尾の meta.json が壊れていると job 全体が ErrMalformedMeta で失敗する
	// (= 先頭の正しい meta に巻き戻さない)。
	body := buildZip(t, []zipEntry{
		{"meta.json", goodMeta},
		{"meta.json", []byte("not json")},
		{"smile.png", img},
	})
	deps, _, _, _ := newDeps(t, body)
	imp := emojiimport.NewImporter(deps)

	_, err := imp.Run(context.Background(), "admin", "f1")
	assert.ErrorIs(t, err, emojiimport.ErrMalformedMeta)
}

func TestRun_SameBasenameDifferentDir(t *testing.T) {
	first := pngBytesSize(t, 2, 2)
	second := pngBytesSize(t, 3, 3)
	meta := metaJSON(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
	})
	// a/smile.png と b/smile.png は index で後勝ちに 1 エントリへ畳まれ、
	// import は 1 件になる。後勝ちなので b/smile.png の内容が採用される。
	body := buildZip(t, []zipEntry{
		{"meta.json", meta},
		{"a/smile.png", first},
		{"b/smile.png", second},
	})
	deps, _, repo, fileRepo, _ := newDepsWithFileRepo(t, body)
	imp := emojiimport.NewImporter(deps)

	res, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Imported)
	assert.Equal(t, 0, res.Skipped)

	// drive file は 1 つだけ作られ、その内容 (= MD5) は後勝ちの b/smile.png と
	// 一致する。索引を先勝ちに変えたときだけこの assert が落ちる。
	require.Len(t, fileRepo.Files, 1)
	var df *model.DriveFile
	for _, f := range fileRepo.Files {
		df = f
	}
	require.NotNil(t, df)
	assert.Equal(t, md5Hex(second), df.MD5, "後勝ちの b/smile.png が保存される")

	_, err = repo.FindByNameAndHost("smile", nil)
	require.NoError(t, err)
}

// --- 対象 4: ファイルタイプ制限 ---
//
// replaceEmoji は内容を MIME 検知して drive.Service.Upload に渡す。system 所有
// (User=nil) なので uploadableFileTypes ゲートは素通りし、upstream TS もファイル
// タイプを制限しないため、「非画像でも拒否しない」のが現行の互換挙動。

func TestRun_NonImageContent_StillImported(t *testing.T) {
	meta := metaJSON(t, []map[string]any{
		{"fileName": "smile.txt", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
	})
	body := buildZip(t, []zipEntry{
		{"meta.json", meta},
		{"smile.txt", []byte("hello world")},
	})
	deps, _, repo, _ := newDeps(t, body)
	imp := emojiimport.NewImporter(deps)

	res, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Imported)
	assert.Equal(t, 0, res.Skipped)

	// drive.Upload が内容ベースで type を決める (text/plain)。
	smile, err := repo.FindByNameAndHost("smile", nil)
	require.NoError(t, err)
	require.NotNil(t, smile.Type)
	assert.Equal(t, "text/plain", *smile.Type)
}

func TestRun_DisguisedExtension(t *testing.T) {
	img := pngBytes(t)
	meta := metaJSON(t, []map[string]any{
		{"fileName": "smile.png.txt", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
	})
	body := buildZip(t, []zipEntry{
		{"meta.json", meta},
		{"smile.png.txt", img},
	})
	deps, _, repo, _ := newDeps(t, body)
	imp := emojiimport.NewImporter(deps)

	res, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Imported)
	assert.Equal(t, 0, res.Skipped)

	// 拡張子ではなく内容で MIME が決まる (image/png)。
	smile, err := repo.FindByNameAndHost("smile", nil)
	require.NoError(t, err)
	require.NotNil(t, smile.Type)
	assert.Equal(t, "image/png", *smile.Type)
}

// --- 対象 5: system 所有 / originalUrl 不変条件の直接アサート ---

func TestRun_UploadedDriveFileIsSystemOwned(t *testing.T) {
	img := pngBytes(t)
	meta := metaJSON(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
	})
	body := buildZip(t, []zipEntry{{"meta.json", meta}, {"smile.png", img}})
	deps, _, _, fileRepo, _ := newDepsWithFileRepo(t, body)
	imp := emojiimport.NewImporter(deps)

	_, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)

	require.Len(t, fileRepo.Files, 1)
	for _, f := range fileRepo.Files {
		assert.Nil(t, f.UserID, "import された emoji 画像は system 所有 (User: nil) で保存される (#670)")
	}
}

func TestRun_EmojiOriginalURL_MatchesDriveFileURL(t *testing.T) {
	img := pngBytes(t)
	meta := metaJSON(t, []map[string]any{
		{"fileName": "smile.png", "downloaded": true, "emoji": map[string]any{"name": "smile"}},
	})
	body := buildZip(t, []zipEntry{{"meta.json", meta}, {"smile.png", img}})
	deps, _, repo, fileRepo, _ := newDepsWithFileRepo(t, body)
	imp := emojiimport.NewImporter(deps)

	_, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)

	smile, err := repo.FindByNameAndHost("smile", nil)
	require.NoError(t, err)
	require.Len(t, fileRepo.Files, 1)
	var df *model.DriveFile
	for _, f := range fileRepo.Files {
		df = f
	}
	require.NotNil(t, df)

	// 不変条件 (#722): emoji.originalUrl == drive_file.url。
	assert.Equal(t, df.URL, smile.OriginalURL)
	// imageProcessor 未配線なので webpublic は無く、publicUrl / type は本体に一致する。
	assert.Equal(t, df.URL, smile.PublicURL)
	require.NotNil(t, smile.Type)
	assert.Equal(t, df.Type, *smile.Type)
}
