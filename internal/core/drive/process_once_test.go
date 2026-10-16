package drive

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"math/rand"
	"testing"

	"github.com/kovidgoyal/imaging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/misc/imagedecode"
)

// noisyImage returns a photo-like gradient with noise so that resampling
// differences actually show up in the pixels.
//
// `-race` 下でも速いように、Pix へ直接書き、乱数は整数のハッシュで作る。
func noisyImage(w, h int, alpha bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		row := img.Pix[y*img.Stride:]
		for x := range w {
			n := int((uint32(x)*2654435761 ^ uint32(y)*2246822519) >> 27) // 0..31
			p := row[x*4 : x*4+4 : x*4+4]
			p[0] = uint8((x*255/w + n) % 256)
			p[1] = uint8((y*255/h + n) % 256)
			p[2] = uint8(((x+y)*128/(w+h) + n) % 256)
			p[3] = 255
			if alpha {
				p[3] = uint8((x*255/w + n) % 256)
			}
		}
	}
	return img
}

func encodeJPEGForTest(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}))
	return buf.Bytes()
}

func encodePNGForTest(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// jpegWithOrientation inserts an EXIF APP1 segment carrying the given
// Orientation tag right after SOI.
func jpegWithOrientation(t *testing.T, img image.Image, orientation uint16) []byte {
	t.Helper()
	base := encodeJPEGForTest(t, img)
	tiff := []byte("MM\x00\x2a\x00\x00\x00\x08")
	ifd := make([]byte, 2+12+4)
	binary.BigEndian.PutUint16(ifd[0:2], 1)
	binary.BigEndian.PutUint16(ifd[2:4], 0x0112) // Orientation
	binary.BigEndian.PutUint16(ifd[4:6], 3)      // SHORT
	binary.BigEndian.PutUint32(ifd[6:10], 1)
	binary.BigEndian.PutUint16(ifd[10:12], orientation)
	payload := append([]byte("Exif\x00\x00"), append(tiff, ifd...)...)
	seg := []byte{0xFF, 0xE1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:4], uint16(len(payload)+2))
	seg = append(seg, payload...)
	out := append([]byte{}, base[:2]...)
	out = append(out, seg...)
	return append(out, base[2:]...)
}

func animatedGIFForTest(t *testing.T, w, h int) []byte {
	t.Helper()
	pal := color.Palette{color.RGBA{255, 0, 0, 255}, color.RGBA{0, 0, 255, 255}}
	g := &gif.GIF{}
	for i := range 2 {
		frame := image.NewPaletted(image.Rect(0, 0, w, h), pal)
		for y := range h {
			for x := range w {
				frame.SetColorIndex(x, y, uint8((x/7+y/5+i)%2))
			}
		}
		g.Image = append(g.Image, frame)
		g.Delay = append(g.Delay, 10)
	}
	var buf bytes.Buffer
	require.NoError(t, gif.EncodeAll(&buf, g))
	return buf.Bytes()
}

// psnr returns the PSNR (dB) between two images of the same size over RGBA.
func psnr(t *testing.T, a, b image.Image) float64 {
	t.Helper()
	require.Equal(t, a.Bounds().Size(), b.Bounds().Size())
	na, nb := imaging.Clone(a), imaging.Clone(b)
	var se float64
	for i := range na.Pix {
		d := float64(na.Pix[i]) - float64(nb.Pix[i])
		se += d * d
	}
	mse := se / float64(len(na.Pix))
	if mse == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(255*255/mse)
}

// The single-decode path must produce the same outputs as the four
// per-step methods it replaces (#3609).
//
// 寸法・blurhash・webpublic はバイト単位で一致させる。サムネイルは、webpublic を
// 縮小して作る入力 (2048 超) では 2048 の縮小から作るので画素が少し変わる。
// そこは寸法と形式を一致させ、画素は PSNR で近いことを見る。それ以外の入力では
// サムネイルもバイト単位で一致させる。
func TestProcessAll_MatchesPerStepMethods(t *testing.T) {
	cases := []struct {
		name string
		mime string
		// body は親で直列に作る (下の warm-up のコメント)。
		body func(t *testing.T) []byte
		// wantW/H は向きを補正した後の寸法。
		wantW, wantH  int
		wantWebpublic string // "" なら作られない
		wantThumbW    int
		wantThumbH    int
		// thumbFromIntermediate は 2048 の縮小からサムネイルを作るケース。
		thumbFromIntermediate bool
	}{
		{
			name: "small jpeg without exif", mime: "image/jpeg",
			body:  func(t *testing.T) []byte { return encodeJPEGForTest(t, noisyImage(640, 480, false)) },
			wantW: 640, wantH: 480, wantThumbW: 498, wantThumbH: 373,
		},
		{
			name: "small jpeg with exif orientation 6", mime: "image/jpeg",
			body:  func(t *testing.T) []byte { return jpegWithOrientation(t, noisyImage(480, 360, false), 6) },
			wantW: 360, wantH: 480, wantWebpublic: "image/webp", wantThumbW: 316, wantThumbH: 422,
		},
		{
			name: "large jpeg with exif orientation 8", mime: "image/jpeg",
			// 向きを補正すると 2062x150。寸法は、2048 の縮小へ imaging.Fit を掛けると
			// 1px ずれるものを選んだ (直接なら 498x36、縮小からだと 498x35)。
			body:  func(t *testing.T) []byte { return jpegWithOrientation(t, noisyImage(150, 2062, false), 8) },
			wantW: 2062, wantH: 150, wantWebpublic: "image/webp", wantThumbW: 498, wantThumbH: 36,
			thumbFromIntermediate: true,
		},
		{
			name: "large png with alpha", mime: "image/png",
			body:  func(t *testing.T) []byte { return encodePNGForTest(t, noisyImage(2064, 150, true)) },
			wantW: 2064, wantH: 150, wantWebpublic: "image/png", wantThumbW: 498, wantThumbH: 36,
			thumbFromIntermediate: true,
		},
		{
			// 縦横比の切り捨てで 1px ずれやすい細長い画像。
			name: "very wide png", mime: "image/png",
			body:  func(t *testing.T) []byte { return encodePNGForTest(t, noisyImage(4099, 7, false)) },
			wantW: 4099, wantH: 7, wantWebpublic: "image/png", wantThumbW: 498, wantThumbH: 1,
			thumbFromIntermediate: true,
		},
		{
			// アニメーションはメタデータが無ければ 2048 超でも webpublic を作らない。
			name: "large animated gif", mime: "image/gif",
			body:  func(t *testing.T) []byte { return animatedGIFForTest(t, 2100, 120) },
			wantW: 2100, wantH: 120, wantThumbW: 374, wantThumbH: 21,
		},
		{
			name: "animated webp", mime: "image/webp",
			body:  func(t *testing.T) []byte { return animatedWebPFixture(t, 64, 48) },
			wantW: 64, wantH: 48, wantThumbW: 64, wantThumbH: 48,
		},
		{
			// **GPS を公開側へ出さない。** 2048 以下でもメタデータがあれば webpublic を
			// 作る。JPEG 以外 (PNG の eXIf、TIFF) も対象 (#3037)。
			name: "small png with exif chunk", mime: "image/png",
			body:  func(t *testing.T) []byte { return pngWithChunk(t, "eXIf", []byte("II\x2a\x00")) },
			wantW: 4, wantH: 4, wantWebpublic: "image/png", wantThumbW: 4, wantThumbH: 4,
		},
		{
			// アニメーションでも、メタデータがあれば webpublic を作る (upstream より厳しい側)。
			// サムネイルは webpublic を作ってもアニメーション用の箱 (374x317) に収める。
			// 400x300 は 498x422 の箱なら縮めずに収まるので、箱の取り違えが寸法に出る。
			name: "apng with exif chunk", mime: "image/apng",
			body: func(t *testing.T) []byte {
				return insertPNGChunkForTest(t, encodePNGForTest(t, noisyImage(400, 300, false)), "eXIf", []byte("II\x2a\x00"))
			},
			wantW: 400, wantH: 300, wantWebpublic: "image/webp", wantThumbW: 374, wantThumbH: 280,
		},
		{
			// TIFF は IFD そのものがメタデータなので、小さくても常に作る。
			name: "small tiff", mime: "image/tiff",
			body:  func(t *testing.T) []byte { return makeTestTIFF(64, 48) },
			wantW: 64, wantH: 48, wantWebpublic: "image/webp", wantThumbW: 64, wantThumbH: 48,
		},
		{
			// 2048 超でも、メタデータの無いアニメーション WebP は 1 コマへ潰さない
			// (webpublic を作らない、#3128)。WebP は MIME では判定できない。
			name: "large animated webp", mime: "image/webp",
			body:  func(t *testing.T) []byte { return animatedWebPFixture(t, 3000, 8) },
			wantW: 3000, wantH: 8, wantThumbW: 498, wantThumbH: 1,
		},
		{
			name: "small avif", mime: "image/avif",
			body:  func(t *testing.T) []byte { return makeTestAVIF(t, 64, 48) },
			wantW: 64, wantH: 48, wantWebpublic: "image/webp", wantThumbW: 64, wantThumbH: 48,
		},
	}

	p := NewDefaultImageProcessor()

	// **fixture と wasm のコーデックの初期化は親で直列に済ませる。** wazero の
	// runtime をプロセスで初めて作る処理が、webp と avif で同時に走ると
	// パッケージ変数 (wazero の version) の data race になる。`-run` でこの
	// テストだけを `-race` で流すと必ず落ちていた (#3609 のレビュー)。
	// エンコーダは fixture を作るときに、デコーダは下の decodeImage で初期化する。
	bodies := make([][]byte, len(cases))
	for i, tc := range cases {
		bodies[i] = tc.body(t)
		_, err := decodeImage(bodies[i], tc.mime)
		require.NoError(t, err, tc.name)
	}

	for i, tc := range cases {
		body := bodies[i]
		t.Run(tc.name, func(t *testing.T) {
			// `-race` 下では 1 件が数秒かかるので並べる。パッケージ変数は触らない。
			t.Parallel()
			// 個別のメソッドの結果 (= 直す前の processImage の出力)。
			w, h, err := p.GetDimensions(body, tc.mime)
			require.NoError(t, err)
			hash, err := p.CalculateBlurhash(body, tc.mime)
			require.NoError(t, err)
			thumb, err := p.GenerateThumbnail(body, tc.mime)
			require.NoError(t, err)
			require.NotNil(t, thumb)
			wp, err := p.GenerateWebpublic(body, tc.mime)
			require.NoError(t, err)

			got := p.ProcessAll(body, tc.mime)
			require.NotNil(t, got)

			assert.Equal(t, tc.wantW, w)
			assert.Equal(t, tc.wantH, h)
			assert.Equal(t, w, got.Width)
			assert.Equal(t, h, got.Height)
			assert.Equal(t, hash, got.Blurhash)

			if tc.wantWebpublic == "" {
				assert.Nil(t, wp)
				assert.Nil(t, got.Webpublic)
			} else {
				require.NotNil(t, wp)
				require.NotNil(t, got.Webpublic)
				assert.Equal(t, tc.wantWebpublic, got.Webpublic.MimeType)
				assert.Equal(t, wp.MimeType, got.Webpublic.MimeType)
				assert.True(t, bytes.Equal(wp.Data, got.Webpublic.Data), "webpublic bytes differ")
				// webpublic は他人に見せる側なので、メタデータ (GPS など) を持たない。
				assert.False(t, hasStrippableMetadata(got.Webpublic.Data, got.Webpublic.MimeType),
					"webpublic still carries metadata")
			}

			require.NotNil(t, got.Thumbnail)
			assert.Equal(t, "image/webp", got.Thumbnail.MimeType)
			assert.Equal(t, thumb.MimeType, got.Thumbnail.MimeType)
			oldImg, err := imagedecode.Decode(thumb.Data)
			require.NoError(t, err)
			newImg, err := imagedecode.Decode(got.Thumbnail.Data)
			require.NoError(t, err)
			assert.Equal(t, image.Pt(tc.wantThumbW, tc.wantThumbH), oldImg.Bounds().Size())
			assert.Equal(t, oldImg.Bounds().Size(), newImg.Bounds().Size())
			if tc.thumbFromIntermediate {
				// 同じ画質 (q77) の WebP で、縮小の経路だけが違う。
				v := psnr(t, oldImg, newImg)
				t.Logf("thumbnail PSNR old vs new: %.1f dB", v)
				assert.Greater(t, v, 30.0)

				// **使う像を固定する。** 2048 の縮小から作ったサムネイルとバイト単位で
				// 一致し、元の像から作ったもの (直す前) とは一致しないこと。PSNR だけだと
				// 元の像から作る形に戻しても通る。
				decoded, err := decodeImage(body, tc.mime)
				require.NoError(t, err)
				intermediate := resizeFit(decoded, webpublicMax, webpublicMax)
				want, err := encodeThumbnail(imaging.Resize(intermediate, tc.wantThumbW, tc.wantThumbH, imaging.Lanczos))
				require.NoError(t, err)
				assert.True(t, bytes.Equal(want.Data, got.Thumbnail.Data), "thumbnail is not derived from the 2048 intermediate")
				assert.False(t, bytes.Equal(thumb.Data, got.Thumbnail.Data), "thumbnail is derived from the original image")
			} else {
				assert.True(t, bytes.Equal(thumb.Data, got.Thumbnail.Data), "thumbnail bytes differ")
			}
		})
	}
}

// Undecodable input yields no alternates; it must not be treated as a
// rejection of the upload, and the pixel cap must still apply.
func TestProcessAll_UndecodableReturnsNil(t *testing.T) {
	p := NewDefaultImageProcessor()
	assert.Nil(t, p.ProcessAll([]byte("not an image"), "image/png"))
	assert.Nil(t, p.ProcessAll(makeTestPNG(4, 4), "application/octet-stream"))
}

// ProcessAll decodes exactly once, through decodeImage, so the pixel cap
// (`imagedecode.UpstreamMaxPixels`) applies to the single-decode path too.
func TestProcessAll_DecodesOnceThroughPixelCap(t *testing.T) {
	var calls int
	var errs []error
	orig := decodeForAltsFunc
	t.Cleanup(func() { decodeForAltsFunc = orig })
	decodeForAltsFunc = func(body []byte, mimeType string) (image.Image, error) {
		calls++
		img, err := orig(body, mimeType)
		errs = append(errs, err)
		return img, err
	}

	p := NewDefaultImageProcessor()

	// webpublic もサムネイルも作る入力で、デコードは 1 回。
	got := p.ProcessAll(jpegWithOrientation(t, noisyImage(16, 2100, false), 6), "image/jpeg")
	require.NotNil(t, got)
	require.NotNil(t, got.Webpublic)
	require.NotNil(t, got.Thumbnail)
	assert.Equal(t, 1, calls)

	// 54 バイトで 8.6GB を要求するヘッダは cap の sentinel で止まる。
	calls, errs = 0, nil
	assert.Nil(t, p.ProcessAll(bmpHeaderInternalForTest(46341, 46341), "image/bmp"))
	require.Equal(t, 1, calls)
	assert.ErrorIs(t, errs[0], imagedecode.ErrTooManyPixels)
}

// The single-decode path uses the upstream pixel cap (0x3FFF^2), not the
// media proxy's 64MP one.
//
// **cap の値まで固定する。** 64MP の cap を当てると 64〜268MP の実写真が
// webpublic を失い、EXIF の GPS が公開側へ出る (`decodeImage` のコメント)。
// IDAT の無い PNG (IHDR + IEND だけ) を使うので、ヘッダの判定を通っても
// ラスタは確保されずにデコードが失敗する。
func TestProcessAll_UsesUpstreamPixelCap(t *testing.T) {
	var errs []error
	orig := decodeForAltsFunc
	t.Cleanup(func() { decodeForAltsFunc = orig })
	decodeForAltsFunc = func(body []byte, mimeType string) (image.Image, error) {
		img, err := orig(body, mimeType)
		errs = append(errs, err)
		return img, err
	}

	p := NewDefaultImageProcessor()
	cases := []struct {
		w, h    uint32
		overCap bool
	}{
		{10000, 10000, false}, // 100MP: 64MP の cap なら止まる
		{16383, 16383, false}, // UpstreamMaxPixels ちょうど
		{16384, 16384, true},
	}
	for _, tc := range cases {
		errs = nil
		assert.Nil(t, p.ProcessAll(pngHeaderOnlyForTest(t, tc.w, tc.h), "image/png"))
		require.Len(t, errs, 1)
		require.Error(t, errs[0])
		if tc.overCap {
			assert.ErrorIs(t, errs[0], imagedecode.ErrTooManyPixels, "%dx%d", tc.w, tc.h)
		} else {
			assert.NotErrorIs(t, errs[0], imagedecode.ErrTooManyPixels, "%dx%d", tc.w, tc.h)
		}
	}
}

// insertPNGChunkForTest inserts a chunk right before IEND.
func insertPNGChunkForTest(t *testing.T, base []byte, typ string, payload []byte) []byte {
	t.Helper()
	iend := bytes.LastIndex(base, []byte("IEND"))
	require.Positive(t, iend)
	td := append([]byte(typ), payload...)
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(payload)))
	chunk = append(chunk, td...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(td))
	out := append([]byte{}, base[:iend-4]...)
	out = append(out, chunk...)
	return append(out, base[iend-4:]...)
}

// pngHeaderOnlyForTest returns a PNG with only IHDR and IEND (no pixel data)
// declaring w x h RGBA 8bit.
func pngHeaderOnlyForTest(t *testing.T, w, h uint32) []byte {
	t.Helper()
	chunk := func(typ string, data []byte) []byte {
		out := make([]byte, 4, 12+len(data))
		binary.BigEndian.PutUint32(out, uint32(len(data)))
		td := append([]byte(typ), data...)
		out = append(out, td...)
		return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(td))
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], w)
	binary.BigEndian.PutUint32(ihdr[4:8], h)
	ihdr[8], ihdr[9] = 8, 6 // 8bit RGBA
	out := []byte("\x89PNG\r\n\x1a\n")
	out = append(out, chunk("IHDR", ihdr)...)
	return append(out, chunk("IEND", nil)...)
}

// A failing webpublic encode must not drop the other outputs (best-effort,
// like the per-step methods).
func TestProcessAll_WebpublicEncodeErrorKeepsOtherOutputs(t *testing.T) {
	// PNG の webpublic は PNG で出すので、PNG のエンコーダだけを落とせば
	// サムネイル (WebP) は作れる。
	restore := SetPNGEncoderForTest(func(image.Image) ([]byte, error) { return nil, errors.New("boom") })
	t.Cleanup(restore)

	p := NewDefaultImageProcessor()
	got := p.ProcessAll(encodePNGForTest(t, noisyImage(4099, 7, false)), "image/png")
	require.NotNil(t, got)
	assert.Nil(t, got.Webpublic)
	assert.Equal(t, 4099, got.Width)
	assert.Equal(t, 7, got.Height)
	assert.NotEmpty(t, got.Blurhash)
	require.NotNil(t, got.Thumbnail)
	assert.Equal(t, "image/webp", got.Thumbnail.MimeType)
}

// bmpHeaderInternalForTest is a 54-byte BMP header declaring w x h pixels.
func bmpHeaderInternalForTest(w, h int32) []byte {
	var b bytes.Buffer
	b.WriteString("BM")
	for _, v := range []any{uint32(54), uint32(0), uint32(54), uint32(40), w, h,
		uint16(1), uint16(24), uint32(0), uint32(0), int32(2835), int32(2835), uint32(0), uint32(0)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	return b.Bytes()
}

// fitDims must reproduce imaging.Fit's output size exactly, including the
// truncation and the zero-height fallback inside imaging.Resize.
func TestFitDims_MatchesImagingFit(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	check := func(srcW, srcH, maxW, maxH int) {
		if srcW <= maxW && srcH <= maxH {
			return
		}
		src := image.NewGray(image.Rect(0, 0, srcW, srcH))
		want := imaging.Fit(src, maxW, maxH, imaging.Lanczos).Bounds().Size()
		w, h := fitDims(srcW, srcH, maxW, maxH)
		assert.Equal(t, want, image.Pt(w, h), "src=%dx%d box=%dx%d", srcW, srcH, maxW, maxH)
	}
	for range 150 {
		check(1+r.Intn(120), 1+r.Intn(120), 1+r.Intn(30), 1+r.Intn(30))
	}
	check(4099, 7, thumbnailWidth, thumbnailHeight)
	check(5000, 1, thumbnailWidth, thumbnailHeight)
	check(1, 5000, thumbnailWidth, thumbnailHeight)
	check(2049, 350, thumbnailWidth, thumbnailHeight)
	check(2048, 349, thumbnailWidth, thumbnailHeight)
	check(3000, 1, animThumbWidth, animThumbHeight)
}

// fakeBatchProcessor fails the test if the per-step methods are used.
type fakeBatchProcessor struct {
	t     *testing.T
	alts  *ImageAlts
	calls int
}

func (f *fakeBatchProcessor) GenerateThumbnail([]byte, string) (*ProcessedImage, error) {
	f.t.Error("per-step GenerateThumbnail called")
	return nil, nil
}

func (f *fakeBatchProcessor) GenerateWebpublic([]byte, string) (*ProcessedImage, error) {
	f.t.Error("per-step GenerateWebpublic called")
	return nil, nil
}

func (f *fakeBatchProcessor) GetDimensions([]byte, string) (int, int, error) {
	f.t.Error("per-step GetDimensions called")
	return 0, 0, nil
}

func (f *fakeBatchProcessor) CalculateBlurhash([]byte, string) (string, error) {
	f.t.Error("per-step CalculateBlurhash called")
	return "", nil
}

func (f *fakeBatchProcessor) ProcessAll([]byte, string) *ImageAlts {
	f.calls++
	return f.alts
}

// processImage must use the single-decode path when available and map its
// result onto the stored columns exactly like the per-step path.
func TestProcessImage_UsesSingleDecode(t *testing.T) {
	thumb := &ProcessedImage{Data: []byte("t"), MimeType: "image/webp"}
	wp := &ProcessedImage{Data: []byte("w"), MimeType: "image/png"}
	f := &fakeBatchProcessor{t: t, alts: &ImageAlts{Width: 30, Height: 20, Blurhash: "LKO2", Thumbnail: thumb, Webpublic: wp}}
	s := &Service{imageProcessor: f}

	got := s.processImage([]byte("x"), "image/png")
	assert.Equal(t, 1, f.calls)
	assert.JSONEq(t, `{"width":30,"height":20}`, string(got.properties))
	require.NotNil(t, got.blurhash)
	assert.Equal(t, "LKO2", *got.blurhash)
	assert.Same(t, thumb, got.thumbnail)
	assert.Same(t, wp, got.webpublic)

	// 読めない画像: 何も作らないが、結果は空で返す (アップロードは続ける)。
	f.alts = nil
	got = s.processImage([]byte("x"), "image/png")
	require.NotNil(t, got)
	assert.Nil(t, got.properties)
	assert.Nil(t, got.blurhash)
	assert.Nil(t, got.thumbnail)
	assert.Nil(t, got.webpublic)
}
