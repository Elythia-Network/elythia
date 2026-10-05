package drive_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/safehttp"
	"github.com/elythia-network/elythia/internal/testutil"
)

// pngBody makes bytes that AnalyseFile types as image/png, so the copy keeps a
// realistic name / MIME instead of falling back to text/plain.
func pngBody(tail string) []byte { return []byte("\x89PNG\r\n\x1a\n" + tail) }

// **複製は system 所有になること (#2966 / #2990)。** ここが利用者付きに戻ると、
// 承認した絵文字が申請者の drive ファイルに依存し続けるという直したはずのバグが
// そのまま復活する。
func TestCopyToSystemFile(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	uid := "u1"
	body := pngBody("emoji-bytes")
	src, err := svc.Upload(context.Background(), drive.UploadInput{
		User: &model.User{ID: uid, Username: "alice"}, Body: body, Name: "orig.png",
	})
	require.NoError(t, err)
	require.NotNil(t, src.UserID, "元ファイルが利用者所有でないと前提が崩れる")

	copied, err := svc.CopyToSystemFile(context.Background(), src, "sushi", false, 1<<20)
	require.NoError(t, err)
	require.Nil(t, copied.UserID, "複製が利用者に紐付いている")
	require.Nil(t, copied.UserHost, "複製に userHost が付いている")
	require.NotEqual(t, src.ID, copied.ID, "複製ではなく元ファイルを返している")
	require.NotEqual(t, src.URL, copied.URL, "複製が元ファイルと同じ URL を指している")
	require.Equal(t, "sushi.png", copied.Name, "渡した名前が使われていない")

	got, err := svc.ReadFileBody(copied, 1<<20)
	require.NoError(t, err)
	require.Equal(t, body, got, "複製の実体が元と違う")

	// **元のファイルは触らない。** ノートの添付やプロフィールで使われている
	// 可能性がある。
	stored, err := fileRepo.FindByID(src.ID)
	require.NoError(t, err)
	require.NotNil(t, stored.UserID, "元のファイルの所有者を奪っている")
	require.Equal(t, uid, *stored.UserID)
	require.Equal(t, src.URL, stored.URL, "元のファイルの URL が変わっている")
}

// name が空なら元ファイルの名前を引き継ぐ。
func TestCopyToSystemFileFallsBackToSourceName(t *testing.T) {
	svc, _, _ := newSvc(t)
	src, err := svc.Upload(context.Background(), drive.UploadInput{Body: pngBody("x"), Name: "orig.png"})
	require.NoError(t, err)

	copied, err := svc.CopyToSystemFile(context.Background(), src, "", false, 1<<20)
	require.NoError(t, err)
	require.Equal(t, "orig.png", copied.Name)
}

// **センシティブは元ファイルと呼び出し側の指定の OR。** 片方だけを見ると、
// 申請で sensitive を付けたのに複製に印が残らない / 元に印があるのに落ちる。
func TestCopyToSystemFileCarriesSensitive(t *testing.T) {
	for _, tc := range []struct {
		name     string
		srcFlag  bool
		argFlag  bool
		wantFlag bool
	}{
		{"どちらも無し", false, false, false},
		{"元ファイルに印", true, false, true},
		{"呼び出し側の指定", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, fileRepo, _ := newSvc(t)
			// Upload は user==nil のとき sensitive 検出を skip するので、入力が
			// そのまま入る。
			src, err := svc.Upload(context.Background(), drive.UploadInput{
				Body: pngBody("x"), Name: "o.png", IsSensitive: tc.srcFlag,
			})
			require.NoError(t, err)
			require.Equal(t, tc.srcFlag, src.IsSensitive)

			copied, err := svc.CopyToSystemFile(context.Background(), src, "e", tc.argFlag, 1<<20)
			require.NoError(t, err)
			require.Equal(t, tc.wantFlag, copied.IsSensitive)

			stored, err := fileRepo.FindByID(copied.ID)
			require.NoError(t, err)
			require.Equal(t, tc.wantFlag, stored.IsSensitive, "行に保存されていない")
		})
	}
}

// 上限超過は safehttp.ErrResponseTooLarge のまま伝わること。
// **潰すと呼び出し側が種別で分けられなくなる** — 大きすぎるのは却下すべき申請、
// ストレージや DB の障害は 5xx、と扱いが違う (#2792)。
func TestCopyToSystemFileRejectsOversized(t *testing.T) {
	svc, _, _ := newSvc(t)
	src, err := svc.Upload(context.Background(), drive.UploadInput{Body: pngBody("0123456789"), Name: "o.png"})
	require.NoError(t, err)

	_, err = svc.CopyToSystemFile(context.Background(), src, "e", false, 4)
	require.ErrorIs(t, err, safehttp.ErrResponseTooLarge)
}

// 実体を持たない行は ErrObjectNotFound のまま伝わること (#2990 のバッチは
// これを「元画像を復元できない」側に分類する)。
func TestCopyToSystemFileMissingBody(t *testing.T) {
	svc, _, _ := newSvc(t)
	key := "gone"
	_, err := svc.CopyToSystemFile(context.Background(), &model.DriveFile{AccessKey: &key, URL: "u"}, "e", false, 1<<20)
	require.ErrorIs(t, err, drive.ErrObjectNotFound)
}

// nil を渡したら何も作らずに落ちること。
func TestCopyToSystemFileRejectsNilSource(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	before := len(fileRepo.Files)
	_, err := svc.CopyToSystemFile(context.Background(), nil, "e", false, 1<<20)
	require.Error(t, err)
	require.Len(t, fileRepo.Files, before, "nil を渡したのにファイルを作っている")
}

// **`storedInternal` の行はローカルから読むこと。** オブジェクトストレージを
// 有効にした構成でも、移行前に保存された元ファイルを複製できる必要がある
// (#2990 の本番データは storedInternal = false に偏っているので、この経路は
// テストでしか通らない)。
func TestCopyToSystemFileReadsStoredInternalFromLocal(t *testing.T) {
	fileRepo := testutil.NewMockDriveFileRepository()
	folderRepo := testutil.NewMockDriveFolderRepository()
	folderRepo.FilesRef = fileRepo
	idGen, _ := id.NewGenerator("aidx")

	// primary (= オブジェクトストレージのつもり) と local を別物にする。
	primary := drive.NewLocalStorage(t.TempDir(), "https://s3.example/files")
	local := drive.NewLocalStorage(t.TempDir(), "https://example.com/files")
	svc := drive.NewService(fileRepo, folderRepo, primary, idGen)
	svc.SetLocalStorage(local)

	body := pngBody("legacy-local-bytes")
	key := "legacy-key"
	_, err := local.Put(key, bytes.NewReader(body))
	require.NoError(t, err)
	uid := "u1"
	src := &model.DriveFile{
		ID: "f1", UserID: &uid, Name: "legacy.png", Type: "image/png",
		AccessKey: &key, StoredInternal: true, URL: "https://example.com/files/" + key,
	}
	require.NoError(t, fileRepo.Create(src))

	copied, err := svc.CopyToSystemFile(context.Background(), src, "legacy", false, 1<<20)
	require.NoError(t, err)
	require.Nil(t, copied.UserID)
	// 複製は primary 側へ書かれる (= 現在の保存先。元が local だったことは
	// 引き継がない)。
	rc, err := primary.Get(*copied.AccessKey)
	require.NoError(t, err)
	defer rc.Close()
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, body, got, "複製が primary へ書かれていない")
}

// upstream `webpublicUrl ?? url` / `webpublicType ?? type`。
func TestPreferWebpublic(t *testing.T) {
	s := func(v string) *string { return &v }
	for _, tc := range []struct {
		name     string
		f        *model.DriveFile
		wantURL  string
		wantType *string
	}{
		{"nil", nil, "", nil},
		{"webpublic あり", &model.DriveFile{
			URL: "https://e/o.png", Type: "image/png",
			WebpublicURL: s("https://e/w.webp"), WebpublicType: s("image/webp"),
		}, "https://e/w.webp", s("image/webp")},
		{"webpublic が空文字", &model.DriveFile{
			URL: "https://e/o.png", Type: "image/png",
			WebpublicURL: s(""), WebpublicType: s(""),
		}, "https://e/o.png", s("image/png")},
		{"webpublic なし", &model.DriveFile{URL: "https://e/o.png", Type: "image/png"},
			"https://e/o.png", s("image/png")},
		{"type も空", &model.DriveFile{URL: "https://e/o.png"}, "https://e/o.png", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.wantURL, drive.PreferWebpublicURL(tc.f))
			got := drive.PreferWebpublicType(tc.f)
			if tc.wantType == nil {
				require.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			require.Equal(t, *tc.wantType, *got)
		})
	}
}

// system 所有の判定は孤児 cleanup の guard (`orphanWhere`) と同じ条件でなければ
// ならない。`admin/emoji/add` (#2999) / `admin/emoji/update` (#3014) と後始末
// バッチ (#2990) がこれで「複製が要るか」を決めるので、片方だけ見ると守られない行を
// 「複製済み」と判定する。
func TestIsSystemOwned(t *testing.T) {
	s := func(v string) *string { return &v }
	for _, tc := range []struct {
		name string
		f    *model.DriveFile
		want bool
	}{
		{"nil", nil, false},
		{"所有者なし", &model.DriveFile{ID: "a"}, true},
		{"利用者所有", &model.DriveFile{ID: "a", UserID: s("u1")}, false},
		// リモートキャッシュは admin/drive/clean-remote-files で消えるので
		// system 所有ではない (`userId` は NULL でも守られない、#2717)。
		{"リモート由来 (userId NULL)", &model.DriveFile{ID: "a", UserHost: s("remote.example")}, false},
		{"リモート利用者所有", &model.DriveFile{ID: "a", UserID: s("u1"), UserHost: s("remote.example")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, drive.IsSystemOwned(tc.f))
		})
	}
}

// failingPutStorage reads through to inner but refuses every write.
type failingPutStorage struct {
	inner drive.Storage
	err   error
}

func (s failingPutStorage) Put(string, io.Reader) (string, error) { return "", s.err }
func (s failingPutStorage) Get(k string) (io.ReadCloser, error)   { return s.inner.Get(k) }
func (s failingPutStorage) Delete(k string) error                 { return s.inner.Delete(k) }

// 書き込みの失敗は握り潰さず、元のエラーを辿れる形で返すこと。
// **バッチ (#2990) がここで種別を分ける** — 読めない (= 元画像が無い) のと
// 書けない (= ストレージ障害) では運用の対処が違う。
func TestCopyToSystemFileReportsUploadFailure(t *testing.T) {
	fileRepo := testutil.NewMockDriveFileRepository()
	folderRepo := testutil.NewMockDriveFolderRepository()
	folderRepo.FilesRef = fileRepo
	idGen, _ := id.NewGenerator("aidx")
	local := drive.NewLocalStorage(t.TempDir(), "https://example.com/files")

	// まず素の service で元ファイルを作る。
	seed := drive.NewService(fileRepo, folderRepo, local, idGen)
	src, err := seed.Upload(context.Background(), drive.UploadInput{Body: pngBody("x"), Name: "o.png"})
	require.NoError(t, err)

	boom := errors.New("storage is down")
	svc := drive.NewService(fileRepo, folderRepo, failingPutStorage{inner: local, err: boom}, idGen)
	_, err = svc.CopyToSystemFile(context.Background(), src, "e", false, 1<<20)
	require.Error(t, err)
	require.ErrorIs(t, err, boom, "元のエラーが辿れない")
	require.NotErrorIs(t, err, drive.ErrObjectNotFound, "書き込みの失敗を not-found に丸めている")
}
