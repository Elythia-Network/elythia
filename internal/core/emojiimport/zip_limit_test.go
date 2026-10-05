package emojiimport

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"regexp"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildZipLocal は 1 エントリの zip を組む。**外部テストパッケージ
// (emojiimport_test) の buildZip は internal test からは見えない**ので、
// unexported な readZipEntry を直接叩くこのファイル用に別途持つ。
func buildZipLocal(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	require.NoError(t, err)
	_, err = w.Write(body)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// openEntry は zip バイト列から名前でエントリを引く。
func openEntry(t *testing.T, raw []byte, name string) *zip.File {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	require.NoError(t, err)
	for _, f := range zr.File {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("entry %q not found", name)
	return nil
}

// 展開後サイズが上限内なら読める。
func TestReadZipEntry_WithinLimit(t *testing.T) {
	raw := buildZipLocal(t, "a.png", bytes.Repeat([]byte("x"), 100))
	got, err := readZipEntry(openEntry(t, raw, "a.png"), 1024)
	require.NoError(t, err)
	assert.Len(t, got, 100)
}

// **ヘッダの UncompressedSize64 で拒否する。** 高圧縮率のエントリを、
// 展開せずに (= メモリを確保せずに) 弾けることが要点。
func TestReadZipEntry_RejectsByHeaderSize(t *testing.T) {
	// 1MiB のゼロ埋めは deflate で 1KiB 程度まで縮む。上限 4KiB に対し
	// **圧縮後サイズは収まるが展開後は超える**ので、ヘッダを見ないと通ってしまう。
	body := make([]byte, 1<<20)
	raw := buildZipLocal(t, "bomb.png", body)
	assert.Less(t, len(raw), 1<<20, "圧縮が効いていないと前提が崩れる")

	f := openEntry(t, raw, "bomb.png")
	require.Equal(t, uint64(1<<20), f.UncompressedSize64)

	_, err := readZipEntry(f, 4<<10)
	assert.ErrorIs(t, err, ErrZipEntryTooLarge)
}

// **ヘッダを偽装しても巨大な読み込みにはならない。** ZIP のサイズ欄は書き手が
// 自由に詰められるので「ヘッダ検査だけで安全」とは言えないが、Go の
// `archive/zip` は宣言サイズと実データの不一致を**読み出しの時点で**弾く。
//
// このテストが固定するのは「偽装しても上限を超えて読まない」ことであって、
// `ErrZipEntryTooLarge` が返ることではない (実測では stdlib の
// `zip: not a valid zip file` が先に返り、**0 バイトも読めない**)。実装の
// LimitReader は stdlib のこの挙動に依存しないための保険。
func TestReadZipEntry_ForgedHeaderDoesNotReadBeyondLimit(t *testing.T) {
	const limit = 4 << 10
	body := make([]byte, 1<<20)
	raw := buildZipLocal(t, "liar.png", body)

	// central directory / local header の uncompressed size を 1 に書き換える。
	forged := forgeUncompressedSize(t, raw, uint32(1<<20), 1)
	f := openEntry(t, forged, "liar.png")
	require.Equal(t, uint64(1), f.UncompressedSize64, "ヘッダ偽装が効いていない")

	got, err := readZipEntry(f, limit)
	require.Error(t, err, "偽装 zip が素通りしている")
	assert.LessOrEqual(t, len(got), limit, "上限を超えて読んでいる")
}

// forgeUncompressedSize は zip バイト列中の uncompressed size 欄 (32bit LE) を
// すべて書き換える。local file header と central directory の両方に現れる。
func forgeUncompressedSize(t *testing.T, raw []byte, from, to uint32) []byte {
	t.Helper()
	out := append([]byte(nil), raw...)
	var want [4]byte
	binary.LittleEndian.PutUint32(want[:], from)
	var repl [4]byte
	binary.LittleEndian.PutUint32(repl[:], to)

	n := 0
	for i := 0; i+4 <= len(out); i++ {
		if bytes.Equal(out[i:i+4], want[:]) {
			copy(out[i:i+4], repl[:])
			n++
		}
	}
	require.Greater(t, n, 0, "uncompressed size 欄が見つからない")
	return out
}

// --- 上限が呼び出し側に配線されているか ---

type stubDriveReader struct{ body []byte }

func (r *stubDriveReader) Fetch(string) (*model.DriveFile, []byte, error) {
	return &model.DriveFile{ID: "f1"}, r.body, nil
}

func newLimitDeps(t *testing.T, body []byte) Deps {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "admin"}))
	fileRepo := testutil.NewMockDriveFileRepository()
	folderRepo := testutil.NewMockDriveFolderRepository()
	folderRepo.FilesRef = fileRepo
	idGen, _ := id.NewGenerator("aidx")
	return Deps{
		UserRepo:  userRepo,
		EmojiRepo: testutil.NewMockEmojiRepository(),
		Drive:     &stubDriveReader{body: body},
		Uploader:  drive.NewService(fileRepo, folderRepo, drive.NewLocalStorage(t.TempDir(), "https://example.com/files"), idGen),
		IDGen:     idGen,
	}
}

// withCaps は上限を一時的に下げる。**64MiB の fixture を作らずに配線を検証する**
// ための仕掛けで、`maxMetaJSONBytes` / `maxEmojiImageBytes` が var なのはこのため。
func withCaps(t *testing.T, meta, image int64) {
	t.Helper()
	om, oi := maxMetaJSONBytes, maxEmojiImageBytes
	maxMetaJSONBytes, maxEmojiImageBytes = meta, image
	t.Cleanup(func() { maxMetaJSONBytes, maxEmojiImageBytes = om, oi })
}

// meta.json 側の上限が Run に配線されていること。
func TestRun_RejectsOversizedMetaJSON(t *testing.T) {
	withCaps(t, 64, 32<<20)
	// 64 バイトを超える meta.json (中身は妥当な JSON)。
	meta := []byte(`{"metaVersion":2,"host":null,"exportedAt":"2026-09-07T00:00:00Z","emojis":[]}`)
	require.Greater(t, len(meta), 64)

	body := buildZipLocal(t, "meta.json", meta)
	imp := NewImporter(newLimitDeps(t, body))
	_, err := imp.Run(context.Background(), "admin", "f1")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrZipEntryTooLarge, "meta.json の上限が配線されていない")
}

// 画像側の上限が Run に配線されていること。上限超過の画像は skip され、
// **import は 0 件**になる (meta.json 自体は読める)。
func TestRun_SkipsOversizedImage(t *testing.T) {
	withCaps(t, 64<<20, 64)
	meta := []byte(`{"metaVersion":2,"host":null,"exportedAt":"2026-09-07T00:00:00Z","emojis":[{"fileName":"big.png","downloaded":true,"emoji":{"name":"big"}}]}`)
	img := bytes.Repeat([]byte("x"), 4096)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range []struct {
		name string
		body []byte
	}{{"meta.json", meta}, {"big.png", img}} {
		w, err := zw.Create(e.name)
		require.NoError(t, err)
		_, err = w.Write(e.body)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())

	imp := NewImporter(newLimitDeps(t, buf.Bytes()))
	res, err := imp.Run(context.Background(), "admin", "f1")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Total)
	assert.Equal(t, 0, res.Imported, "上限超過の画像が取り込まれている")
	assert.Equal(t, 1, res.Skipped)
}

// **出荷される既定値を固定する。** 上限を var にしたことで、`Run` を通るテストは
// withCaps が、readZipEntry 直叩きのテストはリテラルが上限を与えるので、
// **本番値を参照するアサーションが 1 つも無くなっていた**。桁を打ち間違えても
// (実測: 64<<40 / 32<<40 にしても) 全テストが緑のままになる。
func TestZipCaps_ProductionDefaults(t *testing.T) {
	assert.Equal(t, int64(64<<20), maxMetaJSONBytes, "meta.json の上限は upstream zip.ts と同じ 64MiB")
	assert.Equal(t, int64(32<<20), maxEmojiImageBytes, "画像の上限は upstream zip.ts と同じ 32MiB")
}

// **ヘッダを大きく偽装したケース。** 既存の偽装テストはサイズを小さく詰めており、
// その場合は stdlib が不一致を検出するので `UncompressedSize64` 検査を消しても
// 落ちなかった。宣言だけ大きくすると、ヘッダ検査だけが分岐点になる。
func TestReadZipEntry_RejectsWhenHeaderClaimsHuge(t *testing.T) {
	// Store 方式 (無圧縮) で 100 バイト。圧縮すると size 欄の探索が当たらない。
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "small.png", Method: zip.Store})
	require.NoError(t, err)
	_, err = w.Write(bytes.Repeat([]byte("x"), 100))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	// uncompressed size 欄 100 -> 16MiB。
	forged := forgeUncompressedSize(t, buf.Bytes(), 100, 1<<24)
	f := openEntry(t, forged, "small.png")
	require.Equal(t, uint64(1<<24), f.UncompressedSize64, "ヘッダ偽装が効いていない")

	_, err = readZipEntry(f, 4<<10)
	assert.ErrorIs(t, err, ErrZipEntryTooLarge,
		"ヘッダが上限超過を宣言しているのに読みに行っている")
}

// --- upstream 2026.9.0 の isValidName (長さ + パターン) ---

func TestValidImportName(t *testing.T) {
	// **リテラルで書く。** maxImportNameLength を参照すると、定数を変える変異に
	// テスト側が追随してしまい境界がずれても落ちない (実測で 256 にする変異が
	// 生き残った)。upstream の MAX_NAME_LENGTH は 255。
	assert.Equal(t, 255, maxImportNameLength, "upstream MAX_NAME_LENGTH と揃える")
	long := bytes.Repeat([]byte("a"), 255)
	tooLong := bytes.Repeat([]byte("a"), 256)

	cases := []struct {
		name    string
		value   string
		pattern *regexp.Regexp
		ok      bool
	}{
		{"plain file name", "smile.png", validFileName, true},
		{"multi extension", "a.b.c", validFileName, true},
		{"no extension", "smile", validFileName, true},
		// 2026.9.0 で厳格化。旧パターン `^[a-zA-Z0-9_]+?([a-zA-Z0-9.]+)?$` は通していた。
		{"double dot", "a..png", validFileName, false},
		{"trailing dot", "a.", validFileName, false},
		{"leading dot", ".png", validFileName, false},
		{"path separator", "sub/a.png", validFileName, false},
		{"exactly 255", string(long), validFileName, true},
		{"256 chars", string(tooLong), validFileName, false},
		{"emoji name", "smile", validEmojiName, true},
		{"emoji name with dot", "smi.le", validEmojiName, false},
		{"emoji name 256 chars", string(tooLong), validEmojiName, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.ok, validImportName(tc.value, tc.pattern))
		})
	}
}

// upstream の truncateForLog 相当。**上限を移植したなら log 側も要る** —
// meta.json は 64MiB まで許すので、名前をそのまま出すと 1 行が数十 MiB になる。
func TestTruncateForLog(t *testing.T) {
	assert.Equal(t, "short", truncateForLog("short"))
	exact := string(bytes.Repeat([]byte("a"), 255))
	assert.Equal(t, exact, truncateForLog(exact), "255 はそのまま")
	over := string(bytes.Repeat([]byte("a"), 256))
	got := truncateForLog(over)
	assert.Equal(t, 255+3, len(got), "255 で切って ... を足す")
	assert.True(t, strings.HasSuffix(got, "..."))
}
