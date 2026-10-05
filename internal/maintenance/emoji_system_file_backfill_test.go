package maintenance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/emojiapplication"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/pgarray"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

// --- ストレージ構成 ---

// memStorage stands in for object storage: `StorageIsLocal` is false for it, so
// uploads land with `storedInternal = false` exactly like an S3 backend.
//
// **ローカル構成だけで確かめると片側しか通らない。** 本番のデータは
// `storedInternal = false` に偏っており (#2990 の実測)、ローカル側の経路は
// テストでしか通らない。逆にテストをローカルだけにすると、本番で実際に使う側が
// 一度も動かない。
type memStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
	baseURL string
}

func newMemStorage(baseURL string) *memStorage {
	return &memStorage{objects: map[string][]byte{}, baseURL: baseURL}
}

func (s *memStorage) Put(accessKey string, body io.Reader) (string, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[accessKey] = b
	return s.baseURL + "/" + accessKey, nil
}

func (s *memStorage) Get(accessKey string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[accessKey]
	if !ok {
		return nil, drive.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (s *memStorage) Delete(accessKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, accessKey)
	return nil
}

// --- ハーネス ---

type emojiFixture struct {
	svc     *drive.Service
	storage drive.Storage
}

func newDriveSvc(t *testing.T, storage drive.Storage) *emojiFixture {
	t.Helper()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	svc := drive.NewService(
		repository.NewDriveFileRepository(testDB),
		repository.NewDriveFolderRepository(testDB),
		storage,
		idGen,
	)
	svc.SetImageProcessor(drive.NewDefaultImageProcessor())
	return &emojiFixture{svc: svc, storage: storage}
}

func localFixture(t *testing.T) *emojiFixture {
	t.Helper()
	return newDriveSvc(t, drive.NewLocalStorage(t.TempDir(), "https://example.com/files"))
}

func objectFixture(t *testing.T) *emojiFixture {
	t.Helper()
	return newDriveSvc(t, newMemStorage("https://s3.example/files"))
}

func pngBody(tail string) []byte { return []byte("\x89PNG\r\n\x1a\n" + tail) }

// fixedNow pins emoji.updatedAt so the write can be distinguished from GORM's
// own autoUpdateTime behaviour.
var fixedNow = time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)

// seedUserToken keeps tokens inside `user.token` (char(16)) regardless of how
// long the test's suffix is.
var seedUserToken atomic.Int64

func seedUser(t *testing.T, uid string) *model.User {
	t.Helper()
	token := fmt.Sprintf("tok%013d", seedUserToken.Add(1))
	u := &model.User{
		ID: uid, Username: uid, UsernameLower: uid, Token: &token,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, testDB.Create(u).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "user" WHERE id = ?`, uid) })
	return u
}

// seedApprovedOwn builds the exact pre-#2966 shape: an approved `kind = own`
// application whose emoji points straight at the applicant's drive file.
func seedApprovedOwn(t *testing.T, fx *emojiFixture, suffix string) (*model.EmojiApplication, *model.Emoji, *model.DriveFile) {
	t.Helper()
	return seedApprovedOwnWithBody(t, fx, suffix, pngBody("emoji-"+suffix))
}

func seedApprovedOwnWithBody(t *testing.T, fx *emojiFixture, suffix string, body []byte) (*model.EmojiApplication, *model.Emoji, *model.DriveFile) {
	t.Helper()
	uid := "u_" + suffix
	u := seedUser(t, uid)
	src, err := fx.svc.Upload(context.Background(), drive.UploadInput{
		User: u, Body: body, Name: suffix + ".png",
	})
	require.NoError(t, err)
	require.NotNil(t, src.UserID, "元ファイルが利用者所有でないと前提が崩れる")
	t.Cleanup(func() { testDB.Exec(`DELETE FROM drive_file WHERE "userId" = ? OR "userId" IS NULL`, uid) })

	e := &model.Emoji{
		ID:          "e_" + suffix,
		Name:        "emoji_" + suffix,
		OriginalURL: src.URL,
		PublicURL:   drive.PreferWebpublicURL(src),
		Type:        drive.PreferWebpublicType(src),
		Aliases:     model.StringArray{},
	}
	require.NoError(t, testDB.Create(e).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM emoji WHERE id = ?`, e.ID) })

	app := &model.EmojiApplication{
		ID: "a_" + suffix, UserID: uid,
		Kind: model.EmojiApplicationKindOwn, Status: model.EmojiApplicationApproved,
		Name: e.Name, License: "CC0", Aliases: pgarray.StringArray{},
		FileID: &src.ID, EmojiID: &e.ID,
	}
	require.NoError(t, testDB.Create(app).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM emoji_application WHERE id = ?`, app.ID) })

	return app, e, src
}

func reloadEmoji(t *testing.T, id string) *model.Emoji {
	t.Helper()
	var e model.Emoji
	require.NoError(t, testDB.Where("id = ?", id).Take(&e).Error)
	return &e
}

func reloadFile(t *testing.T, id string) *model.DriveFile {
	t.Helper()
	var f model.DriveFile
	require.NoError(t, testDB.Where("id = ?", id).Take(&f).Error)
	return &f
}

// requireNoSystemFiles asserts the package schema holds no system-owned drive
// file at all.
//
// **テーブル全体を数えるのは、seed の後片付け (`seedApprovedOwnWithBody`) が
// `"userId" IS NULL` を丸ごと消しており、`internal/maintenance` の他のテストは
// system 所有の drive ファイルを作らないから。** id で絞ると「作ってから消した」
// のか「そもそも作らなかった」のか区別できるが、**別の行を作る**変異は捕まらない。
func requireNoSystemFiles(t *testing.T, msg string) {
	t.Helper()
	requireSystemFileCount(t, 0, msg)
}

func requireSystemFileCount(t *testing.T, want int64, msg string) {
	t.Helper()
	var n int64
	require.NoError(t, testDB.Model(&model.DriveFile{}).Where(`"userId" IS NULL`).Count(&n).Error)
	require.Equal(t, want, n, msg)
}

func run(t *testing.T, copier SystemFileCopier, opts EmojiSystemFileBackfillOptions) EmojiSystemFileBackfillResult {
	t.Helper()
	res, err := BackfillEmojiSystemFiles(context.Background(), testDB, copier, opts)
	require.NoError(t, err)
	return res
}

func entryFor(t *testing.T, res EmojiSystemFileBackfillResult, appID string) EmojiSystemFileEntry {
	t.Helper()
	for _, e := range res.Entries {
		if e.ApplicationID == appID {
			return e
		}
	}
	t.Fatalf("application %s が結果に含まれていない: %+v", appID, res.Entries)
	return EmojiSystemFileEntry{}
}

// --- 本体 ---

// **この修正の中核。** 承認済みの絵文字が system 所有の複製を指すようになり、
// 申請者の元ファイルは一切変わらないこと。ローカルとオブジェクトストレージの
// 両構成で通す。
func TestBackfillEmojiSystemFiles(t *testing.T) {
	for _, tc := range []struct {
		name               string
		key                string
		fixture            func(*testing.T) *emojiFixture
		wantStoredInternal bool
	}{
		{"ローカルストレージ構成", "local", localFixture, true},
		{"オブジェクトストレージ構成", "s3", objectFixture, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := tc.fixture(t)
			app, e, src := seedApprovedOwn(t, fx, tc.key)

			res := run(t, fx.svc, EmojiSystemFileBackfillOptions{
				Apply: true, Now: func() time.Time { return fixedNow },
			})
			entry := entryFor(t, res, app.ID)
			require.Equal(t, EmojiSystemFileCopied, entry.Outcome, entry.Reason)
			require.NotEmpty(t, entry.CopiedFileID)
			require.False(t, res.NeedsAttention())

			copied := reloadFile(t, entry.CopiedFileID)
			require.Nil(t, copied.UserID, "複製が利用者所有になっている")
			require.Nil(t, copied.UserHost, "複製に userHost が付いている")
			require.Equal(t, tc.wantStoredInternal, copied.StoredInternal,
				"複製が現在の保存先に書かれていない")

			got := reloadEmoji(t, e.ID)
			require.Equal(t, copied.URL, got.OriginalURL,
				"originalUrl が複製の url と一致していない (孤児 cleanup の参照保護が外れる)")
			require.Equal(t, copied.URL, got.PublicURL,
				"webpublic を持たない複製で publicUrl が url と違う")
			require.NotNil(t, got.Type)
			require.Equal(t, copied.Type, *got.Type)
			// **`updatedAt` を実際に書いていること。** 既存行にも入っているうえ
			// GORM は map に無くても `Updates` で埋めるので、値を固定して比べないと
			// map から落としても気付けない。
			require.NotNil(t, got.UpdatedAt)
			require.WithinDuration(t, fixedNow, *got.UpdatedAt, time.Second,
				"updatedAt に渡した時刻が入っていない")

			// **申請者の元ファイルは変更・移動・削除しない。**
			stored := reloadFile(t, src.ID)
			require.NotNil(t, stored.UserID, "元ファイルの所有者を奪っている")
			require.Equal(t, *src.UserID, *stored.UserID)
			require.Equal(t, src.URL, stored.URL, "元ファイルの url が変わっている")
			body, err := fx.svc.ReadFileBody(stored, 1<<20)
			require.NoError(t, err, "元ファイルの実体が消えている")
			require.NotEmpty(t, body)

			// **`emoji_application.fileId` は書き換えない** (申請時に利用者が
			// 提出したファイル、という意味を保つ)。
			var reloaded model.EmojiApplication
			require.NoError(t, testDB.Where("id = ?", app.ID).Take(&reloaded).Error)
			require.NotNil(t, reloaded.FileID)
			require.Equal(t, src.ID, *reloaded.FileID, "申請の fileId を書き換えている")
		})
	}
}

// **`originalUrl` は複製の `url` と一致させ、webpublic を入れない。**
// drive の孤児 cleanup は `emoji.originalUrl = drive_file.url` または
// `publicUrl = url` を参照保護の条件にしている。`originalUrl` に webpublic を
// 入れると、この複製は「どの絵文字からも参照されていない」と判定されて消される —
// つまりバッチが直したはずの絵文字が、今度は cleanup で壊れる。
//
// webpublic variant を実際に持つ複製を作らないと、この違いはテストに現れない
// (webpublic が無ければ `PreferWebpublicURL` は `url` と同じ値を返す)。
func TestBackfillEmojiSystemFilesKeepsCanonicalURLInOriginalURL(t *testing.T) {
	fx := localFixture(t)
	// webpublic は長辺が 2048 を超えたときだけ作られる。
	// JPEG にするのは、webpublic の MIME が原本と変わる (image/webp) から。
	// PNG は webpublic も image/png なので、`type` の導出を間違えても気付けない。
	app, e, _ := seedApprovedOwnWithBody(t, fx, "webpub", wideJPEG(t, 2100, 4))

	res := run(t, fx.svc, EmojiSystemFileBackfillOptions{Apply: true})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileCopied, entry.Outcome, entry.Reason)

	copied := reloadFile(t, entry.CopiedFileID)
	require.NotNil(t, copied.WebpublicURL, "webpublic variant が作られていない (この検査が空振りする)")
	require.NotEqual(t, copied.URL, *copied.WebpublicURL, "url と webpublic が同じで区別が付かない")

	got := reloadEmoji(t, e.ID)
	require.Equal(t, copied.URL, got.OriginalURL,
		"originalUrl に webpublic が入っている (孤児 cleanup が複製を消す)")
	require.Equal(t, *copied.WebpublicURL, got.PublicURL, "publicUrl が webpublic を優先していない")
	require.NotEqual(t, copied.URL, got.PublicURL, "publicUrl が canonical のままになっている")
	require.NotNil(t, got.Type)
	require.NotNil(t, copied.WebpublicType)
	require.Equal(t, *copied.WebpublicType, *got.Type, "type が webpublic を優先していない")
}

// wideJPEG encodes a JPEG wider than webpublicMax so Upload generates a
// webpublic variant with its own access key (and therefore its own URL and,
// since webpublic of a non-PNG is WebP, its own MIME).
func wideJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 251), G: uint8(y * 37 % 251), B: 0x20, A: 0xff})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, nil))
	return buf.Bytes()
}

// **冪等。** 2 回目は何も作らない。二重に複製すると、孤児 cleanup が誰からも
// 参照されなくなった 1 回目の複製を回収する。
func TestBackfillEmojiSystemFilesIsIdempotent(t *testing.T) {
	fx := localFixture(t)
	app, e, _ := seedApprovedOwn(t, fx, "idem")

	first := run(t, fx.svc, EmojiSystemFileBackfillOptions{Apply: true})
	require.Equal(t, 1, first.Copied)
	firstCopy := entryFor(t, first, app.ID).CopiedFileID
	afterFirst := reloadEmoji(t, e.ID).OriginalURL

	second := run(t, fx.svc, EmojiSystemFileBackfillOptions{Apply: true})
	entry := entryFor(t, second, app.ID)
	require.Equal(t, EmojiSystemFileAlready, entry.Outcome, entry.Reason)
	require.Equal(t, 0, second.Copied)
	require.Equal(t, 1, second.Already)
	require.Empty(t, entry.CopiedFileID)
	require.Equal(t, afterFirst, reloadEmoji(t, e.ID).OriginalURL, "2 回目が絵文字を書き換えている")

	var extra int64
	require.NoError(t, testDB.Model(&model.DriveFile{}).
		Where(`"userId" IS NULL AND id <> ?`, firstCopy).Count(&extra).Error)
	require.Zero(t, extra, "2 回目が複製をもう 1 つ作っている")
}

// **書き込みには `Apply` が要る。** options のゼロ値は何も書かない側にしてある —
// `DryRun bool` だと `Options{}` が「全部書く」を意味し、呼び出し側が 1 箇所で
// 否定を書き忘れただけで既定が反転する。
func TestBackfillEmojiSystemFilesDoesNotWriteWithoutApply(t *testing.T) {
	fx := localFixture(t)
	app, e, _ := seedApprovedOwn(t, fx, "dry")
	before := reloadEmoji(t, e.ID).OriginalURL

	res := run(t, fx.svc, EmojiSystemFileBackfillOptions{})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileCopied, entry.Outcome)
	require.Empty(t, entry.CopiedFileID, "dry-run なのに複製の id を返している")
	require.Equal(t, before, reloadEmoji(t, e.ID).OriginalURL, "dry-run なのに絵文字を書き換えた")
	requireNoSystemFiles(t, "dry-run なのに複製を作った")
}

// **構造的に「複製できない」行は dry-run でも出す。** 実体を読まないと分からない
// もの (ストレージからの欠落、上限超過) は本実行まで分からないが、行の形から確定
// するものまで `copied` と報告すると、dry-run が exit 0 で終わって本実行で初めて
// 赤くなる。
//
// **`size` は見ない。** あの列は実体の権威ではないので、過大な値が入っていると
// 健全な絵文字に「消すか差し替えろ」と案内してしまう
// (`TestBackfillEmojiSystemFilesIgnoresOverstatedSize` が固定している)。
func TestBackfillEmojiSystemFilesDryRunDetectsRowLevelBlockers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sql    string
		args   []any
		reason string
	}{
		{"リンク行", `UPDATE drive_file SET "isLink" = true WHERE id = ?`, nil, "リンク行"},
		{"accessKey なし", `UPDATE drive_file SET "accessKey" = NULL WHERE id = ?`, nil, "accessKey"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := localFixture(t)
			app, _, src := seedApprovedOwn(t, fx, "row"+tc.name)
			require.NoError(t, testDB.Exec(tc.sql, src.ID).Error)

			res := run(t, fx.svc, EmojiSystemFileBackfillOptions{MaxCopyBytes: 1024})
			entry := entryFor(t, res, app.ID)
			require.Equal(t, EmojiSystemFileUnrepairable, entry.Outcome, entry.Reason)
			require.Contains(t, entry.Reason, tc.reason)
			require.True(t, res.NeedsAttention(), "dry-run が exit 0 で終わっている")
		})
	}
}

// **元ファイルが消えている対象は一覧に出して非ゼロ終了。** 元画像を復元できない
// ので自動修復はしない。絵文字にも申請にも触らない。
func TestBackfillEmojiSystemFilesReportsMissingSourceFile(t *testing.T) {
	fx := localFixture(t)
	app, e, src := seedApprovedOwn(t, fx, "gone")
	before := reloadEmoji(t, e.ID).OriginalURL
	require.NoError(t, testDB.Exec(`DELETE FROM drive_file WHERE id = ?`, src.ID).Error)

	res := run(t, fx.svc, EmojiSystemFileBackfillOptions{Apply: true})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileUnrepairable, entry.Outcome)
	require.Contains(t, entry.Reason, "削除")
	require.Equal(t, e.Name, entry.EmojiName, "一覧に絵文字名が出ていない")
	require.Equal(t, 1, res.Unrepairable)
	require.True(t, res.NeedsAttention(), "exit code が非ゼロにならない")
	require.Equal(t, before, reloadEmoji(t, e.ID).OriginalURL, "修復できないのに絵文字を書き換えた")
}

// **実体がストレージから消えている対象も同じ扱い。** 行は残っているが読めない。
func TestBackfillEmojiSystemFilesReportsMissingObject(t *testing.T) {
	fx := objectFixture(t)
	app, _, src := seedApprovedOwn(t, fx, "noobj")
	require.NotNil(t, src.AccessKey)
	require.NoError(t, fx.storage.Delete(*src.AccessKey))

	res := run(t, fx.svc, EmojiSystemFileBackfillOptions{Apply: true})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileUnrepairable, entry.Outcome)
	require.Contains(t, entry.Reason, "ストレージ")
	require.True(t, res.NeedsAttention())
}

// **複製の上限を超える画像は `failed` にしない。** 待っても縮まないので、
// 再実行を促すのは誤った案内になる (人が別の画像へ差し替えるしかない)。
func TestBackfillEmojiSystemFilesReportsOversized(t *testing.T) {
	fx := localFixture(t)
	app, _, _ := seedApprovedOwn(t, fx, "toobig")

	res := run(t, fx.svc, EmojiSystemFileBackfillOptions{Apply: true, MaxCopyBytes: 4})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileUnrepairable, entry.Outcome, entry.Reason)
	require.Contains(t, entry.Reason, "上限")
	require.Equal(t, 0, res.Failed, "再実行で直らないものを failed にしている")
	require.True(t, res.NeedsAttention())
	requireNoSystemFiles(t, "上限に当たったのに複製が残っている")
}

// **上限の既定は申請側と同じ定数。** 別々に持つと「申請はできたのに承認だけが
// 恒久的に失敗する」帯が生まれる、という #2966 の判断がバッチでも効いていること。
func TestBackfillEmojiSystemFilesDefaultsToSharedCopyLimit(t *testing.T) {
	fx := localFixture(t)
	app, _, src := seedApprovedOwn(t, fx, "deflimit")
	// 既定の上限ちょうどまでは通る、を裏返して「既定より 1 バイト大きい実体は
	// 落ちる」で見る (実体を 32 MiB 作らずに済ませるため、行側の宣言ではなく
	// 実際の読み出し上限が共有定数から来ていることを確かめる)。
	require.Less(t, int64(src.Size), emojiapplication.MaxEmojiCopyBytes)

	res := run(t, fx.svc, EmojiSystemFileBackfillOptions{Apply: true})
	require.Equal(t, EmojiSystemFileCopied, entryFor(t, res, app.ID).Outcome,
		"既定の上限が共有定数より小さくなっている")
}

// 承認後にモデレーターが絵文字を消した形。守るべき絵文字が無いので複製しない。
// **exit code は非ゼロにしない** — ふつうに起きるので、毎回赤くすると本物の
// 要対応が埋もれる。
func TestBackfillEmojiSystemFilesSkipsDeletedEmoji(t *testing.T) {
	fx := localFixture(t)
	app, e, _ := seedApprovedOwn(t, fx, "delemoji")
	require.NoError(t, testDB.Exec(`DELETE FROM emoji WHERE id = ?`, e.ID).Error)

	res := run(t, fx.svc, EmojiSystemFileBackfillOptions{Apply: true})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileSkipped, entry.Outcome)
	require.Equal(t, 1, res.Skipped)
	require.False(t, res.NeedsAttention(), "ふつうに起きる状態で exit code を非ゼロにしている")

	var systemFiles int64
	require.NoError(t, testDB.Model(&model.DriveFile{}).Where(`"userId" IS NULL`).Count(&systemFiles).Error)
	require.Zero(t, systemFiles, "絵文字が無いのに複製を作った")
}

// モデレーターが `admin/emoji/update` で別の画像に差し替えた形。申請ファイルで
// 上書きするとその差し替えを巻き戻すので触らない。
//
// **ただし「済み」でもない。** ここに来た絵文字は system 所有のファイルを参照して
// いないことが確定しているので、今も誰かの drive 操作で壊れうる。黙って skip すると
// 運用者が「対象は全部片付いた」と読む。
func TestBackfillEmojiSystemFilesReportsRepointedEmoji(t *testing.T) {
	fx := localFixture(t)
	app, e, _ := seedApprovedOwn(t, fx, "repoint")
	const other = "https://example.com/files/replaced-by-moderator"
	require.NoError(t, testDB.Exec(`UPDATE emoji SET "originalUrl" = ? WHERE id = ?`, other, e.ID).Error)

	res := run(t, fx.svc, EmojiSystemFileBackfillOptions{Apply: true})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileNeedsReview, entry.Outcome)
	require.Contains(t, entry.Reason, "参照していない")
	require.Equal(t, other, reloadEmoji(t, e.ID).OriginalURL, "差し替えを巻き戻している")
	require.Equal(t, 1, res.NeedsReview)
	require.True(t, res.NeedsAttention(), "まだ壊れうる絵文字を exit code に出していない")
	requireNoSystemFiles(t, "差し替えられた絵文字のために複製を作った")
}

// 申請ファイルが既に system 所有なら複製しない。二重に複製すると、孤児 cleanup が
// 元の複製 (誰からも参照されなくなる) を回収する。
func TestBackfillEmojiSystemFilesSkipsSystemOwnedSource(t *testing.T) {
	fx := localFixture(t)
	app, e, src := seedApprovedOwn(t, fx, "alreadysys")
	require.NoError(t, testDB.Exec(`UPDATE drive_file SET "userId" = NULL WHERE id = ?`, src.ID).Error)

	// 絵文字も同じファイルを指しているので `already`。
	res := run(t, fx.svc, EmojiSystemFileBackfillOptions{Apply: true})
	require.Equal(t, EmojiSystemFileAlready, entryFor(t, res, app.ID).Outcome)
	// 1 = 申請ファイルそのもの (system 所有に書き換えたもの)。複製は作らない。
	requireSystemFileCount(t, 1, "既に system 所有なのに複製した")

	// **絵文字だけ別の場所を指していたら、申請ファイルの所有者に関係なく要対応。**
	// 絵文字が system 所有を参照していない以上まだ壊れうるので、申請ファイル側を
	// 見て skip すると「無関係なファイルの所有者で絵文字の分類が決まる」形になる。
	require.NoError(t, testDB.Exec(`UPDATE emoji SET "originalUrl" = 'https://example.com/files/elsewhere' WHERE id = ?`, e.ID).Error)
	res = run(t, fx.svc, EmojiSystemFileBackfillOptions{Apply: true})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileNeedsReview, entry.Outcome, entry.Reason)
	require.True(t, res.NeedsAttention())
	requireSystemFileCount(t, 1, "要対応と判定したのに複製した")
}

// **`userHost` 付きの行を「system 所有」と言わない。** `userId IS NULL` でも
// `userHost` があるのはリモートの添付で、`admin/federation/delete-all-files` で
// 消える。守れていない状態を「済み」と言うと、そのまま放置される。
func TestBackfillEmojiSystemFilesTreatsRemoteAttachmentAsUnprotected(t *testing.T) {
	fx := localFixture(t)
	app, e, src := seedApprovedOwn(t, fx, "remoteattach")
	require.NoError(t, testDB.Exec(
		`UPDATE drive_file SET "userId" = NULL, "userHost" = 'example.com' WHERE id = ?`, src.ID).Error)

	res := run(t, fx.svc, EmojiSystemFileBackfillOptions{Apply: true})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileCopied, entry.Outcome, entry.Reason)

	copied := reloadFile(t, entry.CopiedFileID)
	require.Nil(t, copied.UserID)
	require.Nil(t, copied.UserHost)
	require.Equal(t, copied.URL, reloadEmoji(t, e.ID).OriginalURL)
}

// 絵文字として許可されない MIME は複製しない (承認経路と同じ allowlist)。
func TestBackfillEmojiSystemFilesRejectsDisallowedType(t *testing.T) {
	fx := localFixture(t)
	app, _, src := seedApprovedOwn(t, fx, "svg")
	require.NoError(t, testDB.Exec(`UPDATE drive_file SET type = 'image/svg+xml' WHERE id = ?`, src.ID).Error)

	res := run(t, fx.svc, EmojiSystemFileBackfillOptions{Apply: true})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileUnrepairable, entry.Outcome)
	require.Contains(t, entry.Reason, "MIME")
	require.True(t, res.NeedsAttention())
	// **直せないと判定したなら何も作っていないこと。** 作ってから捨てるのでは
	// なく、そもそも読まずに落とす (孤児が出る窓を開けない)。
	requireNoSystemFiles(t, "修復できないと判定したのに複製を作った")
}

// --- 補償処理 ---

// hookedCopier lets a test observe / fail the steps around the copy.
type hookedCopier struct {
	inner      SystemFileCopier
	copyErr    error
	nilCopy    bool
	afterCopy  func(copied *model.DriveFile)
	deleteErr  error
	deleteFile []string
}

func (c *hookedCopier) CopyToSystemFile(ctx context.Context, src *model.DriveFile, name string, sensitive bool, max int64) (*model.DriveFile, error) {
	if c.copyErr != nil {
		return nil, c.copyErr
	}
	if c.nilCopy {
		return nil, nil
	}
	copied, err := c.inner.CopyToSystemFile(ctx, src, name, sensitive, max)
	if err == nil && c.afterCopy != nil {
		c.afterCopy(copied)
	}
	return copied, err
}

func (c *hookedCopier) DeleteSystemFile(fileID string) error {
	c.deleteFile = append(c.deleteFile, fileID)
	if c.deleteErr != nil {
		return c.deleteErr
	}
	return c.inner.DeleteSystemFile(fileID)
}

// 複製そのものが障害で失敗したら `failed`。not-found に丸めない (#2792)。
func TestBackfillEmojiSystemFilesCopyFailure(t *testing.T) {
	fx := localFixture(t)
	app, e, _ := seedApprovedOwn(t, fx, "copyfail")
	before := reloadEmoji(t, e.ID).OriginalURL
	c := &hookedCopier{inner: fx.svc, copyErr: errors.New("storage is down")}

	res := run(t, c, EmojiSystemFileBackfillOptions{Apply: true})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileFailed, entry.Outcome)
	require.Contains(t, entry.Reason, "storage is down")
	require.Equal(t, 1, res.Failed)
	require.True(t, res.NeedsAttention())
	require.Equal(t, before, reloadEmoji(t, e.ID).OriginalURL)
	require.Empty(t, c.deleteFile, "作っていない複製を消そうとしている")
}

// **走っている間に絵文字が変わったら上書きしない。** 条件付き更新が 0 行なら
// 作った複製を消して要対応にする。残すと誰からも参照されない孤児になる。
//
// **`failed` に入れる** — 再実行すれば `already` か `needs-review` に落ち着くので、
// 「原因を取り除いて再実行する」側が正しい案内になる。
func TestBackfillEmojiSystemFilesConflict(t *testing.T) {
	fx := localFixture(t)
	app, e, _ := seedApprovedOwn(t, fx, "conflict")
	const other = "https://example.com/files/won-the-race"
	c := &hookedCopier{inner: fx.svc, afterCopy: func(*model.DriveFile) {
		require.NoError(t, testDB.Exec(`UPDATE emoji SET "originalUrl" = ? WHERE id = ?`, other, e.ID).Error)
	}}

	res := run(t, c, EmojiSystemFileBackfillOptions{Apply: true})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileFailed, entry.Outcome)
	require.Contains(t, entry.Reason, "競合")
	require.True(t, res.NeedsAttention())
	require.Equal(t, other, reloadEmoji(t, e.ID).OriginalURL, "他の書き込みを上書きした")
	require.Len(t, c.deleteFile, 1, "競合で作った複製を片付けていない")
	requireNoSystemFiles(t, "競合で作った複製の行が残っている")
}

// 後始末に失敗しても分類は変えず、残った孤児を追える理由を出す。
func TestBackfillEmojiSystemFilesCleanupFailureIsReported(t *testing.T) {
	fx := localFixture(t)
	app, e, _ := seedApprovedOwn(t, fx, "cleanupfail")
	c := &hookedCopier{
		inner:     fx.svc,
		deleteErr: errors.New("delete refused"),
		afterCopy: func(*model.DriveFile) {
			require.NoError(t, testDB.Exec(`UPDATE emoji SET "originalUrl" = 'x' WHERE id = ?`, e.ID).Error)
		},
	}

	res := run(t, c, EmojiSystemFileBackfillOptions{Apply: true})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileFailed, entry.Outcome, "後始末の失敗で分類が変わっている")
	require.Contains(t, entry.Reason, "後始末に失敗")
	require.Contains(t, entry.Reason, "競合", "元の理由が消えている")
}

// 複製した実体の MIME が許可外なら、複製を消して要対応にする。`Upload` は
// バイト列から型を引き直すので、行の宣言と実体がずれていると起きる。
func TestBackfillEmojiSystemFilesRejectsCopiedType(t *testing.T) {
	fx := localFixture(t)
	app, _, _ := seedApprovedOwn(t, fx, "badcopytype")
	c := &hookedCopier{inner: fx.svc, afterCopy: func(copied *model.DriveFile) {
		copied.Type = "application/zip"
	}}

	res := run(t, c, EmojiSystemFileBackfillOptions{Apply: true})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileUnrepairable, entry.Outcome)
	require.Contains(t, entry.Reason, "複製した実体")
	require.Len(t, c.deleteFile, 1, "許可外の複製を片付けていない")
	requireNoSystemFiles(t, "許可外の複製の行が残っている")
}

// storedEmojiURL returns a URL of exactly n runes (保存先が返す形を模す)。
//
// **長さは定数から作る。** マジックナンバー (600 文字など) で作ると、上限を
// **締める**向きの変異 (512 → 511) が素通りする — 正常な複製を `unrepairable` に
// 落として消す側に倒れる変異なので、そちらのほうが危険 (レビュー M2 で実測)。
func storedEmojiURL(n int) string {
	const prefix = "https://s3.example/"
	return prefix + strings.Repeat("a", n-len(prefix))
}

// **複製の URL が `emoji` の列に入らないなら、複製を消して `unrepairable` にする (#3023)。**
// `drive_file.url` は varchar(1024) なので複製自体は作れるが、
// `emoji.originalUrl` / `publicUrl` は varchar(512) なので更新が SQLSTATE 22001 で
// 落ちる。**再実行しても同じ**ので `failed` (= 原因を取り除いて再実行) ではない。
// 保存先の prefix が長いオブジェクトストレージ構成で起こりうる。
func TestBackfillEmojiSystemFilesRejectsCopyURLTooLongForEmoji(t *testing.T) {
	over := storedEmojiURL(emojiURLMaxRunes + 1)
	fits := storedEmojiURL(emojiURLMaxRunes)
	for _, tc := range []struct {
		name string
		// suffix は行の id に入るので短くする (`emoji_application.id` は varchar(32))。
		suffix  string
		breakIt func(*model.DriveFile)
	}{
		{"url が 1 文字超過", "lu1", func(copied *model.DriveFile) { copied.URL = over }},
		// **`url` 側だけが超過する形も要る。** `PreferWebpublicURL` は
		// `webpublicUrl ?? url` なので、webpublic が空のケースだけだと publicUrl 側の
		// 判定が url 側を包含し、url 側の条件を外した変異が素通りする (レビュー M1)。
		{"url だけ超過 (webpublic は収まる)", "lu2", func(copied *model.DriveFile) {
			copied.URL = over
			copied.WebpublicURL = &fits
		}},
		// **webpublic 側も見る。** 本番では `drive_file.webpublicUrl` も
		// varchar(512) なので複製の INSERT が先に落ちるが、`emoji.publicUrl` に
		// 入るのはこちらの値なので、導出を変えたときに素通りさせない。
		{"webpublic が 1 文字超過", "lu3", func(copied *model.DriveFile) {
			copied.WebpublicURL = &over
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := localFixture(t)
			app, e, _ := seedApprovedOwn(t, fx, tc.suffix)
			before := reloadEmoji(t, e.ID).OriginalURL
			c := &hookedCopier{inner: fx.svc, afterCopy: tc.breakIt}

			res := run(t, c, EmojiSystemFileBackfillOptions{Apply: true})
			entry := entryFor(t, res, app.ID)
			require.Equal(t, EmojiSystemFileUnrepairable, entry.Outcome, entry.Reason)
			require.Contains(t, entry.Reason, "varchar(512)")
			require.True(t, res.NeedsAttention())
			require.Len(t, c.deleteFile, 1, "入らない複製を片付けていない")
			require.Equal(t, before, reloadEmoji(t, e.ID).OriginalURL, "絵文字が変わっている")
			requireNoSystemFiles(t, "入らない複製の行が残っている")
		})
	}
}

// **上限ちょうどは通す。** 締める向きの off-by-one で、正常な複製を消したうえで
// 「削除か差し替え」を運用者に案内する形に倒れる (レビュー M2)。
// 512 文字の URL が実際に `emoji.originalUrl` へ書けることもここで確かめている
// (テスト DB は本物の DDL を適用してある)。
func TestBackfillEmojiSystemFilesAcceptsCopyURLAtLimit(t *testing.T) {
	fx := localFixture(t)
	app, e, _ := seedApprovedOwn(t, fx, "atlimit")
	url := storedEmojiURL(emojiURLMaxRunes)
	c := &hookedCopier{inner: fx.svc, afterCopy: func(copied *model.DriveFile) {
		copied.URL = url
	}}

	res := run(t, c, EmojiSystemFileBackfillOptions{Apply: true})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileCopied, entry.Outcome, entry.Reason)
	require.Empty(t, c.deleteFile, "入るはずの複製を片付けている")
	require.Equal(t, url, reloadEmoji(t, e.ID).OriginalURL)
}

// 上限の値そのものを固定する (#3023)。
//
// **上の 2 つは URL を `emojiURLMaxRunes` から作るので、締める向きは自分では
// 捕まえられない** (511 にすると URL も 511/512 になって自己整合する。実測で
// 落ちるのはこのテストだけ)。広げる向き (513) は `AcceptsCopyURLAtLimit` が
// 513 文字を実 DDL へ書こうとして落ちる。**だからこの固定が要る。**
// DDL との突き合わせは `internal/repository/emoji_column_limits_test.go`。
func TestEmojiURLMaxRunesMatchesColumn(t *testing.T) {
	require.Equal(t, 512, emojiURLMaxRunes, "emoji.originalUrl / publicUrl は varchar(512)")
}

// --- 対象の絞り込み ---

// 申請経由でない絵文字や、承認されていない申請は対象にしない。
func TestBackfillEmojiSystemFilesScopesTargets(t *testing.T) {
	fx := localFixture(t)
	app, _, src := seedApprovedOwn(t, fx, "scope")

	for _, tc := range []struct {
		name string
		sql  string
		args []any
	}{
		{"pending", `UPDATE emoji_application SET status = 'pending' WHERE id = ?`, []any{app.ID}},
		{"rejected", `UPDATE emoji_application SET status = 'rejected' WHERE id = ?`, []any{app.ID}},
		{"canceled", `UPDATE emoji_application SET status = 'canceled' WHERE id = ?`, []any{app.ID}},
		{"remote", `UPDATE emoji_application SET kind = 'remote' WHERE id = ?`, []any{app.ID}},
		{"emojiId が NULL", `UPDATE emoji_application SET "emojiId" = NULL WHERE id = ?`, []any{app.ID}},
		{"fileId が NULL", `UPDATE emoji_application SET "fileId" = NULL WHERE id = ?`, []any{app.ID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, testDB.Exec(tc.sql, tc.args...).Error)
			t.Cleanup(func() {
				require.NoError(t, testDB.Exec(
					`UPDATE emoji_application SET status = 'approved', kind = 'own', "emojiId" = ?, "fileId" = ? WHERE id = ?`,
					app.EmojiID, src.ID, app.ID).Error)
			})
			res := run(t, fx.svc, EmojiSystemFileBackfillOptions{})
			for _, e := range res.Entries {
				require.NotEqual(t, app.ID, e.ApplicationID, "対象外のはずの申請を拾っている")
			}
		})
	}
}

// -limit で 1 回に見る件数を絞れること。
func TestBackfillEmojiSystemFilesLimit(t *testing.T) {
	fx := localFixture(t)
	seedApprovedOwn(t, fx, "lim1")
	seedApprovedOwn(t, fx, "lim2")

	all := run(t, fx.svc, EmojiSystemFileBackfillOptions{})
	// **件数を決め打たない。** schema は実行をまたいで残るので、前の実行が
	// 途中で死んで残した行があると無関係に赤くなる (#2756)。
	require.GreaterOrEqual(t, all.Scanned, 2)

	one := run(t, fx.svc, EmojiSystemFileBackfillOptions{Limit: 1})
	require.Equal(t, 1, one.Scanned)
	require.Less(t, one.Scanned, all.Scanned, "-limit が効いていない")
}

// **列挙そのものに ctx を効かせる。** 中断済みの ctx で呼ばれたら、1 行も読まずに
// 落ちること (エラーが列挙の段で出ることで区別する)。
func TestBackfillEmojiSystemFilesHonorsContext(t *testing.T) {
	fx := localFixture(t)
	seedApprovedOwn(t, fx, "ctxdone")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := BackfillEmojiSystemFiles(ctx, testDB, fx.svc, EmojiSystemFileBackfillOptions{})
	require.Error(t, err, "中断した ctx のまま問い合わせが走っている")
	require.ErrorContains(t, err, "list approved own applications",
		"列挙に ctx が効いておらず、ループまで進んでから落ちている")
}

// **走っている途中で中断されたら、そこで止める。** ctx を見ないと残りの行が
// lookup の失敗で `failed` になり、要対応として大量に報告される。
func TestBackfillEmojiSystemFilesStopsMidRunOnCancel(t *testing.T) {
	fx := localFixture(t)
	seedApprovedOwn(t, fx, "cancel1")
	seedApprovedOwn(t, fx, "cancel2")
	ctx, cancel := context.WithCancel(context.Background())
	c := &hookedCopier{inner: fx.svc, afterCopy: func(*model.DriveFile) { cancel() }}

	res, err := BackfillEmojiSystemFiles(ctx, testDB, c, EmojiSystemFileBackfillOptions{Apply: true})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, res.Scanned, "中断後も残りの行を処理している")
}

// 未配線で黙って成功しない。
func TestBackfillEmojiSystemFilesRequiresWiring(t *testing.T) {
	fx := localFixture(t)
	_, err := BackfillEmojiSystemFiles(context.Background(), nil, fx.svc, EmojiSystemFileBackfillOptions{})
	require.Error(t, err)
	_, err = BackfillEmojiSystemFiles(context.Background(), testDB, nil, EmojiSystemFileBackfillOptions{})
	require.Error(t, err)
}

// **承認経路と同じ複製を通していること。** `*drive.Service` が
// `SystemFileCopier` を満たさなくなったら、バッチ側に同じ処理をもう 1 つ書く
// 誘惑が生まれる (= 片方だけ直る形)。
var _ SystemFileCopier = (*drive.Service)(nil)

// **絵文字の更新が障害で失敗したら、作った複製を片付けて `failed` にする。**
// 残すと誰からも参照されない孤児になり、しかも申請は直っていない。
//
// 失敗は UPDATE の手前で注入する。**列幅で落とす形は使えない** — #3023 で
// 長すぎる URL は更新の前に `unrepairable` として弾くようになったので、
// ここを通らない (それを固定するのが
// `TestBackfillEmojiSystemFilesRejectsCopyURLTooLongForEmoji`)。
func TestBackfillEmojiSystemFilesUpdateFailure(t *testing.T) {
	fx := localFixture(t)
	app, e, _ := seedApprovedOwn(t, fx, "updfail")
	before := reloadEmoji(t, e.ID).OriginalURL

	db := failingUpdateDB(t)
	c := &hookedCopier{inner: fx.svc}
	res, err := BackfillEmojiSystemFiles(context.Background(), db, c, EmojiSystemFileBackfillOptions{Apply: true})
	require.NoError(t, err)
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileFailed, entry.Outcome, entry.Reason)
	require.Contains(t, entry.Reason, "絵文字の更新に失敗")
	require.True(t, res.NeedsAttention())
	require.Len(t, c.deleteFile, 1, "更新に失敗したのに複製を片付けていない")
	require.Equal(t, before, reloadEmoji(t, e.ID).OriginalURL, "失敗したのに絵文字が変わっている")

	var orphans int64
	require.NoError(t, testDB.Model(&model.DriveFile{}).Where(`"userId" IS NULL`).Count(&orphans).Error)
	require.Zero(t, orphans, "孤児の複製が残っている")
}

// DB 障害を not-found に丸めない (#2792)。参照先の読み出しが落ちたら `failed`。
func TestBackfillEmojiSystemFilesReportsLookupErrors(t *testing.T) {
	fx := localFixture(t)
	app, _, _ := seedApprovedOwn(t, fx, "dberr")

	for _, tc := range []struct {
		name    string
		table   string
		failNth int
		want    string
	}{
		{"絵文字", "emoji", 1, "絵文字の読み出しに失敗"},
		{"参照先ファイル", "drive_file", 1, "参照先ファイルの読み出しに失敗"},
		{"申請ファイル", "drive_file", 2, "申請ファイルの読み出しに失敗"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := failingQueryDB(t, tc.table, tc.failNth)
			res, err := BackfillEmojiSystemFiles(context.Background(), db, fx.svc, EmojiSystemFileBackfillOptions{Apply: true})
			require.NoError(t, err)
			entry := entryFor(t, res, app.ID)
			require.Equal(t, EmojiSystemFileFailed, entry.Outcome)
			require.Contains(t, entry.Reason, tc.want)
			require.True(t, res.NeedsAttention())
		})
	}
}

// 列挙そのものが落ちたら、部分的な結果を返さずエラーにする。
func TestBackfillEmojiSystemFilesListError(t *testing.T) {
	fx := localFixture(t)
	db := failingQueryDB(t, "emoji_application", 1)
	_, err := BackfillEmojiSystemFiles(context.Background(), db, fx.svc, EmojiSystemFileBackfillOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "list approved own applications")
}

// failingQueryDB returns a handle whose nth SELECT against table fails.
//
// **別ハンドルを開くのが要点。** callback は `*gorm.DB` の Config に載るので、
// 共有している testDB に登録すると後続のテストまで巻き込む (#2795)。
func failingQueryDB(t *testing.T, table string, nth int) *gorm.DB {
	t.Helper()
	db, err := testutil.OpenTestDB()
	require.NoError(t, err)
	// **プールを閉じる。** `OpenTestDB` は呼ぶたびに `gorm.Open` するので、
	// 閉じないとプロセス終了までコネクションが残る。
	t.Cleanup(func() {
		if sqlDB, derr := db.DB(); derr == nil {
			_ = sqlDB.Close()
		}
	})
	var seen int
	require.NoError(t, db.Callback().Query().Before("gorm:query").
		Register("maintenance_test_fail", func(tx *gorm.DB) {
			if tx.Statement.Table != table {
				return
			}
			seen++
			if seen == nth {
				// 障害注入なので戻り値は見ない。
				_ = tx.AddError(errors.New("injected db failure"))
			}
		}))
	return db
}

// 引数が空 / 実在しない id のときは「無い」を返す (黙って別の行を拾わない)。
func TestEmojiBackfillLookupHelpers(t *testing.T) {
	require.Equal(t, "", derefString(nil))
	v := "x"
	require.Equal(t, "x", derefString(&v))

	for _, id := range []string{"", "does-not-exist"} {
		e, err := findEmojiByID(testDB, id)
		require.NoError(t, err)
		require.Nil(t, e)

		f, err := findDriveFileByID(testDB, id)
		require.NoError(t, err)
		require.Nil(t, f)

		rows, err := findDriveFilesByURL(testDB, id)
		require.NoError(t, err)
		require.Empty(t, rows)
	}

	// 作っていない複製は消しにいかない。
	c := &hookedCopier{inner: nil}
	entry := EmojiSystemFileEntry{}
	deleteCopy(c, "", &entry)
	require.Empty(t, c.deleteFile)
	require.Empty(t, entry.Reason)
}

// **この修正の本丸。** 複製が drive の孤児 cleanup に回収されないこと。
//
// cleanup は `emoji.originalUrl = drive_file.url` または `publicUrl = url` を
// 参照保護の条件にしている。`originalUrl` に webpublic の URL を入れると複製の
// canonical url がどちらにも一致せず、**バッチが直したはずの絵文字が今度は
// cleanup で壊れる**。webpublic variant を持つ複製でないとこの違いは現れない。
func TestBackfillEmojiSystemFilesSurvivesOrphanCleanup(t *testing.T) {
	fx := localFixture(t)
	app, e, _ := seedApprovedOwnWithBody(t, fx, "orphan", wideJPEG(t, 2100, 4))

	res := run(t, fx.svc, EmojiSystemFileBackfillOptions{Apply: true})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileCopied, entry.Outcome, entry.Reason)
	copied := reloadFile(t, entry.CopiedFileID)
	require.NotNil(t, copied.WebpublicURL, "webpublic が無いと検査が空振りする")

	// **cleanup が実際に何かを消したことを確かめる (positive control)。**
	// この瞬間 schema に孤児が 1 つも無いと、「回収を免れた」のか「そもそも
	// 走っていない」のか区別が付かない。捨てて構わない孤児を 1 つ置く。
	bait := "bait_" + app.ID
	baitKey := "baitkey_" + app.ID
	require.NoError(t, testDB.Create(&model.DriveFile{
		ID: bait, Name: "bait.png", Type: "image/png", MD5: "b41d8", Size: 1,
		URL: "https://example.com/files/" + baitKey, AccessKey: &baitKey,
	}).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM drive_file WHERE id = ?`, bait) })

	// 本物の cleanup をそのまま流す (`admin/drive/cleanup` が呼ぶもの)。
	removed, err := repository.NewDriveFileRepository(testDB).DeleteOrphans()
	require.NoError(t, err)
	require.Positive(t, removed, "孤児 cleanup が 1 行も見ていない (検査が空振りする)")
	var baitLeft int64
	require.NoError(t, testDB.Model(&model.DriveFile{}).Where("id = ?", bait).Count(&baitLeft).Error)
	require.Zero(t, baitLeft, "参照されていない system ファイルが回収されていない")

	var n int64
	require.NoError(t, testDB.Model(&model.DriveFile{}).Where("id = ?", copied.ID).Count(&n).Error)
	require.Equal(t, int64(1), n, "複製が孤児 cleanup に回収された (絵文字の画像が消える)")
	require.Equal(t, copied.URL, reloadEmoji(t, e.ID).OriginalURL)
}

// **エラーが返っても適用されていることがある。** PostgreSQL は COMMIT を送った
// 後・ack の前に接続が切れると、サーバー側は commit 済みなのにクライアントは
// エラーを受け取る。そこで複製を消すと絵文字が存在しないファイルを指し、以後の
// 実行は `needs-review` に落ちて自動では直らない。
func TestBackfillEmojiSystemFilesKeepsCopyWhenUpdateActuallyLanded(t *testing.T) {
	fx := localFixture(t)
	app, e, _ := seedApprovedOwn(t, fx, "landed")
	db := failingUpdateAfterExecDB(t)
	c := &hookedCopier{inner: fx.svc}

	res, err := BackfillEmojiSystemFiles(context.Background(), db, c, EmojiSystemFileBackfillOptions{Apply: true})
	require.NoError(t, err)
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileCopied, entry.Outcome, entry.Reason)
	require.Contains(t, entry.Reason, "適用されていた")
	require.Empty(t, c.deleteFile, "適用済みの複製を消した (絵文字が 404 になる)")

	copied := reloadFile(t, entry.CopiedFileID)
	require.Equal(t, copied.URL, reloadEmoji(t, e.ID).OriginalURL)
}

// 載ったかどうかを確認できないときは複製を消さない。参照されている複製を消すほうが、
// 参照されない複製を残すより悪い (後者は孤児 cleanup が回収する)。
func TestBackfillEmojiSystemFilesKeepsCopyWhenLandingIsUnknown(t *testing.T) {
	fx := localFixture(t)
	app, _, _ := seedApprovedOwn(t, fx, "unknownland")
	db := failingUpdateAfterExecDB(t)
	// 更新後の読み直しだけを落とす (列挙と最初の lookup は通す)。
	failEmojiQueryAfter(t, db, 2)
	c := &hookedCopier{inner: fx.svc}

	res, err := BackfillEmojiSystemFiles(context.Background(), db, c, EmojiSystemFileBackfillOptions{Apply: true})
	require.NoError(t, err)
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileFailed, entry.Outcome)
	require.Contains(t, entry.Reason, "確認できない")
	require.Empty(t, c.deleteFile, "載ったか分からないのに複製を消した")
}

// failingUpdateAfterExecDB returns a handle whose UPDATE statements run and then
// report an error, reproducing "committed but the client saw a failure".
func failingUpdateAfterExecDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := testutil.OpenTestDB()
	require.NoError(t, err)
	t.Cleanup(func() {
		if sqlDB, derr := db.DB(); derr == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, db.Callback().Update().After("gorm:update").
		Register("maintenance_test_update_ack_lost", func(tx *gorm.DB) {
			// 障害注入なので戻り値は見ない。
			_ = tx.AddError(errors.New("injected: connection reset after commit"))
		}))
	return db
}

// failingUpdateDB returns a handle whose UPDATE statements fail before they are
// sent, reproducing "the write never landed".
//
// GORM の組み込み callback は先頭で `db.Error` を見て早期 return するので、
// `Before` で足したエラーは UPDATE そのものを実行させない。
func failingUpdateDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := testutil.OpenTestDB()
	require.NoError(t, err)
	t.Cleanup(func() {
		if sqlDB, derr := db.DB(); derr == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, db.Callback().Update().Before("gorm:update").
		Register("maintenance_test_update_never_ran", func(tx *gorm.DB) {
			// 障害注入なので戻り値は見ない。
			_ = tx.AddError(errors.New("injected: update failed"))
		}))
	return db
}

// failEmojiQueryAfter makes the nth (1-indexed) SELECT against `emoji` fail on db.
func failEmojiQueryAfter(t *testing.T, db *gorm.DB, nth int) {
	t.Helper()
	var seen int
	require.NoError(t, db.Callback().Query().Before("gorm:query").
		Register("maintenance_test_fail_emoji_reread", func(tx *gorm.DB) {
			if tx.Statement.Table != "emoji" {
				return
			}
			seen++
			if seen >= nth {
				_ = tx.AddError(errors.New("injected: emoji re-read failed"))
			}
		}))
}

// **`drive_file.url` に一意制約は無い。** 同じ url の行が複数あるとき「1 行だけ
// 採る」形だと、system 所有の行を引けば偽の「複製済み」、利用者所有の行を引けば
// 偽の「要確認」になる。system 所有が 1 つでもあるかを見ること。
func TestBackfillEmojiSystemFilesHandlesDuplicateURLs(t *testing.T) {
	fx := localFixture(t)
	app, e, src := seedApprovedOwn(t, fx, "duperr")

	// 同じ url を持つ system 所有の行を、id が後ろに来るように足す
	// (id 昇順で 1 行だけ採る実装だと利用者所有の行を引いてしまう並び)。
	dupKey := "dupkey_" + app.ID
	dup := &model.DriveFile{
		ID: "zzz_dup_" + app.ID, Name: "dup.png", Type: "image/png", MD5: "d41d8",
		Size: 1, URL: src.URL, AccessKey: &dupKey,
	}
	require.NoError(t, testDB.Create(dup).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM drive_file WHERE id = ?`, dup.ID) })

	res := run(t, fx.svc, EmojiSystemFileBackfillOptions{Apply: true})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileAlready, entry.Outcome, entry.Reason)
	require.Equal(t, src.URL, reloadEmoji(t, e.ID).OriginalURL, "複製済みなのに書き換えた")
}

// **行の `size` は権威ではない。** 宣言より実体が大きいと、読み出しの上限に
// 当たって初めて分かる。この経路を `failed` (= 再実行で直る) に倒さないこと。
func TestBackfillEmojiSystemFilesReportsOversizedBody(t *testing.T) {
	fx := localFixture(t)
	app, _, src := seedApprovedOwn(t, fx, "understated")
	// 行だけ見れば 1 バイト。実体は数十バイトある。
	require.NoError(t, testDB.Exec(`UPDATE drive_file SET size = 1 WHERE id = ?`, src.ID).Error)

	res := run(t, fx.svc, EmojiSystemFileBackfillOptions{Apply: true, MaxCopyBytes: 4})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileUnrepairable, entry.Outcome, entry.Reason)
	require.Contains(t, entry.Reason, "上限")
	require.Equal(t, 0, res.Failed, "再実行で直らないものを failed にしている")
	requireNoSystemFiles(t, "上限に当たったのに複製が残っている")
}

// copier が `(nil, nil)` を返しても panic せず `failed` にすること。
// **nil のまま進むと `copied.Type` で落ちる** — バッチが途中で死ぬと、そこまでに
// 作った複製の後始末も残りの行の処理も止まる。
func TestBackfillEmojiSystemFilesHandlesNilCopy(t *testing.T) {
	fx := localFixture(t)
	app, e, _ := seedApprovedOwn(t, fx, "nilcopy")
	before := reloadEmoji(t, e.ID).OriginalURL
	c := &hookedCopier{inner: fx.svc, nilCopy: true}

	res := run(t, c, EmojiSystemFileBackfillOptions{Apply: true})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileFailed, entry.Outcome)
	require.Equal(t, before, reloadEmoji(t, e.ID).OriginalURL)
}

// **行の `size` が過大でも「直せない」と言わない。** `drive_file.size` は行に
// 書いてあるだけで実体の権威ではない (TS 由来の行や手で直した行ではずれうる)。
// そこで `unrepairable` に倒すと、**健全な絵文字にモデレーターが「消すか差し替えろ」**
// と案内されることになる。大きすぎるかどうかは実際に読んだ結果だけで決める。
func TestBackfillEmojiSystemFilesIgnoresOverstatedSize(t *testing.T) {
	fx := localFixture(t)
	app, _, src := seedApprovedOwn(t, fx, "fatsize")
	// 行だけ見れば上限超過。実体は数十バイトしかない。
	require.NoError(t, testDB.Exec(`UPDATE drive_file SET size = 99999999 WHERE id = ?`, src.ID).Error)

	dry := run(t, fx.svc, EmojiSystemFileBackfillOptions{MaxCopyBytes: 1024})
	require.Equal(t, EmojiSystemFileCopied, entryFor(t, dry, app.ID).Outcome,
		"行の size を見て直せないと判定している")
	require.False(t, dry.NeedsAttention())

	applied := run(t, fx.svc, EmojiSystemFileBackfillOptions{Apply: true, MaxCopyBytes: 1024})
	entry := entryFor(t, applied, app.ID)
	require.Equal(t, EmojiSystemFileCopied, entry.Outcome, entry.Reason)
	require.NotEmpty(t, entry.CopiedFileID)
}

// **`Limit` を付けると冪等でなくなる。** system 所有の行が枠の外に落ちると、
// 実行のたびに新しい複製を作る。id の昇順で最後に来る形で固定する。
func TestBackfillEmojiSystemFilesSeesSystemRowBeyondTheFirstFew(t *testing.T) {
	fx := localFixture(t)
	app, e, src := seedApprovedOwn(t, fx, "manydup")

	// 同じ url の行を 3 つ足し、system 所有のものを id の最後に置く。
	for i, spec := range []struct {
		id     string
		userID *string
	}{
		{"m1_" + app.ID, src.UserID},
		{"m2_" + app.ID, src.UserID},
		{"zzz_" + app.ID, nil},
	} {
		key := fmt.Sprintf("mdup%d_%s", i, app.ID)
		row := &model.DriveFile{
			ID: spec.id, UserID: spec.userID, Name: "dup.png", Type: "image/png",
			MD5: "d41d8", Size: 1, URL: src.URL, AccessKey: &key,
		}
		require.NoError(t, testDB.Create(row).Error)
		t.Cleanup(func() { testDB.Exec(`DELETE FROM drive_file WHERE id = ?`, row.ID) })
	}

	res := run(t, fx.svc, EmojiSystemFileBackfillOptions{Apply: true})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileAlready, entry.Outcome, entry.Reason)
	require.Equal(t, src.URL, reloadEmoji(t, e.ID).OriginalURL, "複製済みなのに書き換えた")
}

// **url が空の行は複製しない。** 絵文字側の `originalUrl` も空だと「参照先を引く」も
// 「URL が一致するか」も判定にならず、上の `already` を素通りして**実行のたびに
// 新しい複製を作る**。
func TestBackfillEmojiSystemFilesRejectsEmptyURL(t *testing.T) {
	fx := localFixture(t)
	app, e, src := seedApprovedOwn(t, fx, "emptyurl")
	require.NoError(t, testDB.Exec(`UPDATE drive_file SET url = '' WHERE id = ?`, src.ID).Error)
	require.NoError(t, testDB.Exec(`UPDATE emoji SET "originalUrl" = '' WHERE id = ?`, e.ID).Error)

	res := run(t, fx.svc, EmojiSystemFileBackfillOptions{Apply: true})
	entry := entryFor(t, res, app.ID)
	require.Equal(t, EmojiSystemFileUnrepairable, entry.Outcome, entry.Reason)
	require.Contains(t, entry.Reason, "url")
	requireNoSystemFiles(t, "url が空の行を複製した")
}
