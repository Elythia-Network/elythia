package imagedecode

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/blezek/tga"
	"github.com/gen2brain/avif"
	"github.com/gen2brain/jpegxl"
	gwebp "github.com/gen2brain/webp"
	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
)

// fuzzSeedMaxBytes keeps external seeds small so that each mutation stays
// fast; large seeds made the first audit run manage only a few hundred
// executions in 150 seconds.
const fuzzSeedMaxBytes = 96 << 10

// fuzzDecodeTimeout flags inputs that take unreasonably long to decode
// (decompression bombs, quadratic paths). Go's fuzzer only reports panics
// and explicit failures, so the time has to be checked here.
const fuzzDecodeTimeout = 5 * time.Second

// timeCheckEnabled reports whether the decode-time check applies: when the
// binary runs under -fuzz, or when IMAGEDECODE_FUZZ_TIMECHECK=1 is set so that
// a slow input recorded under testdata/fuzz can be reproduced with the
// `go test -run` command Go prints (that command has no -fuzz).
// 種だけを走らせる `make test` では見ない。-race 付きだと WASM のデコーダの
// 初回に 2 秒ほどかかり、CI のように他の package と並列だと 5 秒を超える
// (make check で実際に落ちた、#3480)。
func timeCheckEnabled() bool {
	if os.Getenv("IMAGEDECODE_FUZZ_TIMECHECK") == "1" {
		return true
	}
	fl := flag.Lookup("test.fuzz")
	return fl != nil && fl.Value.String() != ""
}

// checkDecodeTime fails the input when the time check is enabled and the
// decode exceeded fuzzDecodeTimeout.
func checkDecodeTime(t *testing.T, start time.Time, what string, n int) {
	if !timeCheckEnabled() {
		return
	}
	if d := time.Since(start); d > fuzzDecodeTimeout {
		t.Fatalf("%s took %v for %d bytes", what, d, n)
	}
}

// fuzzSeedImage returns a small image with distinct pixels and alpha so that
// every encoder produces a non-trivial file.
func fuzzSeedImage() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 60), G: uint8(y * 60), B: uint8((x + y) * 30), A: uint8(255 - x*40)})
		}
	}
	return img
}

// fuzzSeedEncoders is the single list that both generatedSeeds and the
// presence check in TestWriteFuzzSeeds walk, so a new encoder cannot be added
// to one without the other.
var fuzzSeedEncoders = []struct {
	ext string
	enc func(w io.Writer, m image.Image) error
}{
	{".png", func(w io.Writer, m image.Image) error { return png.Encode(w, m) }},
	{".gif", func(w io.Writer, m image.Image) error { return gif.Encode(w, m, nil) }},
	{".jpg", func(w io.Writer, m image.Image) error { return jpeg.Encode(w, m, nil) }},
	{".webp", func(w io.Writer, m image.Image) error { return gwebp.Encode(w, m, gwebp.Options{Lossless: true}) }},
	{".avif", func(w io.Writer, m image.Image) error { return avif.Encode(w, m) }},
	{".jxl", func(w io.Writer, m image.Image) error { return jpegxl.Encode(w, m) }},
	{".tga", func(w io.Writer, m image.Image) error { return tga.Encode(w, m) }},
	{".bmp", func(w io.Writer, m image.Image) error { return bmp.Encode(w, m) }},
	{".tiff", func(w io.Writer, m image.Image) error { return tiff.Encode(w, m, nil) }},
}

var (
	fuzzSeedOnce  sync.Once
	fuzzSeedCache map[string][]byte
	fuzzSeedErr   error
)

// fuzzSeedDir holds the committed seeds (one 4x4 image per encoder) that a
// plain `go test` run feeds to every fuzz function. They are written by
// TestWriteFuzzSeeds; see generatedSeeds for why they are not encoded at
// test time.
const fuzzSeedDir = "testdata/fuzz-seeds"

// generatedSeeds encodes fuzzSeedImage with each encoder reachable from the
// decoders' dependencies (stdlib PNG / GIF / JPEG, x/image BMP / TIFF,
// gen2brain WebP / AVIF / JPEG XL, blezek TGA), keyed by extension. HEIC には
// Go のエンコーダが無いので、その経路は外部の種 (IMAGEDECODE_FUZZ_SEEDS) を
// 渡したときだけ通る。
//
// テストのたびに作らずファイルにしてコミットするのは、WASM のエンコーダ
// (AVIF / JPEG XL / WebP) が -race 付きだと 3 つで 16 秒かかり、`make test` が
// その分遅くなるため (#3480)。デコードの方は長い種でも 2 秒ほど。
func generatedSeeds(t testing.TB) map[string][]byte {
	fuzzSeedOnce.Do(func() {
		img := fuzzSeedImage()
		out := map[string][]byte{}
		for _, e := range fuzzSeedEncoders {
			var b bytes.Buffer
			if err := e.enc(&b, img); err != nil {
				fuzzSeedErr = fmt.Errorf("encode %s seed: %w", e.ext, err)
				return
			}
			out[e.ext] = b.Bytes()
		}
		fuzzSeedCache = out
	})
	if fuzzSeedErr != nil {
		t.Fatal(fuzzSeedErr)
	}
	return fuzzSeedCache
}

// TestWriteFuzzSeeds regenerates testdata/fuzz-seeds when
// IMAGEDECODE_WRITE_FUZZ_SEEDS=1 is set; otherwise it only checks that every
// encoder's seed is present so a new encoder cannot be forgotten.
func TestWriteFuzzSeeds(t *testing.T) {
	write := os.Getenv("IMAGEDECODE_WRITE_FUZZ_SEEDS") == "1"
	if !write {
		for _, e := range fuzzSeedEncoders {
			if _, err := os.Stat(filepath.Join(fuzzSeedDir, "seed"+e.ext)); err != nil {
				t.Errorf("missing committed seed for %s (run with IMAGEDECODE_WRITE_FUZZ_SEEDS=1): %v", e.ext, err)
			}
		}
		return
	}
	if err := os.MkdirAll(fuzzSeedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 一覧から外した形式の古いファイルは消す。
	stale, _ := filepath.Glob(filepath.Join(fuzzSeedDir, "seed.*"))
	for _, path := range stale {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	for ext, data := range generatedSeeds(t) {
		if err := os.WriteFile(filepath.Join(fuzzSeedDir, "seed"+ext), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// addSeeds adds the committed seeds, the PNG fixtures under testdata, and
// every file under the directories listed in IMAGEDECODE_FUZZ_SEEDS (colon
// separated) whose extension is in exts. 外部の種は module cache の各ライブラリの
// testdata を想定している (`make fuzz-imagedecode` が組み立てる)。
func addSeeds(f *testing.F, exts ...string) {
	want := map[string]bool{}
	for _, e := range exts {
		want[e] = true
	}
	addDir := func(dir string) int {
		n := 0
		_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !want[strings.ToLower(filepath.Ext(path))] {
				return nil
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil || len(data) > fuzzSeedMaxBytes {
				return nil
			}
			f.Add(data)
			n++
			return nil
		})
		return n
	}
	if addDir("testdata") == 0 {
		f.Fatalf("no committed seeds under testdata for %v", exts)
	}
	// 外部の種は、無いディレクトリや 1 件も拾えないディレクトリを黙って
	// 飛ばさない (Section 4「何も拾えなかったら落とす」)。typo や go.work の
	// 不調で種が空のまま回るのを防ぐ。拡張子の違いで 0 件になるのは正常
	// (TGA の fuzz に x/image の testdata を渡したときなど)。
	for _, dir := range strings.Split(os.Getenv("IMAGEDECODE_FUZZ_SEEDS"), ":") {
		if dir == "" {
			continue
		}
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			f.Fatalf("IMAGEDECODE_FUZZ_SEEDS: %q is not a directory: %v", dir, err)
		}
		addDir(dir)
	}
}

// fuzzImageExts lists every container the DecodeWithPixelCap pipeline can
// reach: the formats imaging registers (including netpbm) plus the sandboxed
// decoders.
var fuzzImageExts = []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".apng", ".avif", ".avifs", ".heic", ".heif", ".jxl", ".tiff", ".tif", ".bmp", ".pam", ".pbm", ".pgm", ".ppm"}

// fuzzPixelCap returns the pixel cap the decode fuzz runs with. The default
// is MaxPixels (8192 squared) so that one raster at the declared size is
// 256MB; paths that convert the image (colour profile, orientation, animation
// canvas) hold a second raster, so budget 512MB or more per worker. With
// IMAGEDECODE_FUZZ_PIXELCAP=upstream it uses UpstreamMaxPixels, the cap drive
// applies to uploads (0x3FFF squared; one raster is 1GiB regardless of bit
// depth because of the byte budget in DecodeWithPixelCap), so that headers
// declaring between the two caps are decoded as well.
func fuzzPixelCap() int64 {
	if os.Getenv("IMAGEDECODE_FUZZ_PIXELCAP") == "upstream" {
		return UpstreamMaxPixels
	}
	return MaxPixels
}

// FuzzDecodeWithPixelCap fuzzes the entry point drive uses for uploads and
// remote attachments.
func FuzzDecodeWithPixelCap(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("\x89PNG\r\n\x1a\n"))
	addSeeds(f, fuzzImageExts...)
	pixelCap := fuzzPixelCap()
	f.Fuzz(func(t *testing.T, data []byte) {
		start := time.Now()
		_, _ = DecodeWithPixelCap(data, pixelCap)
		checkDecodeTime(t, start, "decode", len(data))
	})
}

func FuzzDecodeTGAWithPixelCap(f *testing.F) {
	f.Add([]byte{})
	addSeeds(f, ".tga")
	pixelCap := fuzzPixelCap()
	f.Fuzz(func(t *testing.T, data []byte) {
		start := time.Now()
		_, _ = DecodeTGAWithPixelCap(data, pixelCap)
		checkDecodeTime(t, start, "tga decode", len(data))
	})
}

// FuzzProbes covers the cheap classifiers that run before any decoder and the
// embedded metadata (ICC / EXIF) checks.
func FuzzProbes(f *testing.F) {
	f.Add([]byte{})
	addSeeds(f, fuzzImageExts...)
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = IsAnimatedWebP(data)
		_ = IsBrokenInterlacedPNG(data)
		_ = ExceedsSandboxedDecoderSize(data)
		_ = checkEmbeddedMetadata(data)
	})
}
