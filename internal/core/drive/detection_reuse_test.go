package drive

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"image"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// apngForTest turns a static PNG into an APNG by inserting an acTL chunk
// right after IHDR, so the decoder takes the APNG (default image) path.
func apngForTest(t *testing.T, base []byte) []byte {
	t.Helper()
	const ihdrEnd = 8 + 4 + 4 + 13 + 4
	require.Greater(t, len(base), ihdrEnd)
	payload := binary.BigEndian.AppendUint32(nil, 1) // num_frames
	payload = binary.BigEndian.AppendUint32(payload, 0)
	td := append([]byte("acTL"), payload...)
	chunk := binary.BigEndian.AppendUint32(nil, uint32(len(payload)))
	chunk = append(chunk, td...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(td))
	out := append([]byte{}, base[:ihdrEnd]...)
	out = append(out, chunk...)
	return append(out, base[ihdrEnd:]...)
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(path))
	require.NoError(t, err)
	return b
}

type detectionFixture struct {
	name string
	mime string
	body []byte
}

// detectionFixtures covers the input shapes whose decode has its own rules
// (EXIF orientation, ICC, animation, palette, 16bit, alpha, interlace).
func detectionFixtures(t *testing.T) []detectionFixture {
	t.Helper()
	seeds := "../../misc/imagedecode/testdata/"
	nrgba64 := image.NewNRGBA64(image.Rect(0, 0, 37, 23))
	for y := range 23 {
		for x := range 37 {
			nrgba64.SetNRGBA64(x, y, color.NRGBA64{R: uint16(x * 1771), G: uint16(y * 2843), B: 40000, A: uint16(30000 + x*900)})
		}
	}
	gray16 := image.NewGray16(image.Rect(0, 0, 31, 17))
	for y := range 17 {
		for x := range 31 {
			gray16.SetGray16(x, y, color.Gray16{Y: uint16(x*2000 + y*300)})
		}
	}
	pal := image.NewPaletted(image.Rect(0, 0, 40, 30), color.Palette{
		color.NRGBA{0, 0, 0, 0}, color.NRGBA{200, 30, 30, 255}, color.NRGBA{30, 200, 30, 128},
	})
	for y := range 30 {
		for x := range 40 {
			pal.SetColorIndex(x, y, uint8((x/5+y/3)%3))
		}
	}
	return []detectionFixture{
		{"seed jpeg", "image/jpeg", readFixture(t, seeds+"fuzz-seeds/seed.jpg")},
		{"seed png", "image/png", readFixture(t, seeds+"fuzz-seeds/seed.png")},
		{"seed gif", "image/gif", readFixture(t, seeds+"fuzz-seeds/seed.gif")},
		{"seed webp", "image/webp", readFixture(t, seeds+"fuzz-seeds/seed.webp")},
		{"seed bmp", "image/bmp", readFixture(t, seeds+"fuzz-seeds/seed.bmp")},
		{"seed tiff", "image/tiff", readFixture(t, seeds+"fuzz-seeds/seed.tiff")},
		{"seed avif", "image/avif", readFixture(t, seeds+"fuzz-seeds/seed.avif")},
		{"interlaced rgb8 png", "image/png", readFixture(t, seeds+"interlaced-rgb8.png")},
		{"interlaced rgb8 trns png", "image/png", readFixture(t, seeds+"interlaced-rgb8-trns.png")},
		{"interlaced rgba8 png", "image/png", readFixture(t, seeds+"interlaced-rgba8.png")},
		{"plain rgb8 png", "image/png", readFixture(t, seeds+"plain-rgb8.png")},
		{"lossy webp with alpha", "image/webp", readFixture(t, "../mediaproxy/testdata/lossy-alpha.webp")},
		{"jpeg exif orientation 6 over webpublic size", "image/jpeg", jpegWithOrientation(t, noisyImage(16, 2100, false), 6)},
		{"jpeg exif orientation 3", "image/jpeg", jpegWithOrientation(t, noisyImage(120, 80, false), 3)},
		{"jpeg exif orientation 8", "image/jpeg", jpegWithOrientation(t, noisyImage(90, 300, false), 8)},
		{"large jpeg", "image/jpeg", encodeJPEGForTest(t, noisyImage(2600, 1500, false))},
		{"png with alpha", "image/png", encodePNGForTest(t, noisyImage(300, 200, true))},
		{"png 16bit with alpha", "image/png", encodePNGForTest(t, nrgba64)},
		{"png gray16", "image/png", encodePNGForTest(t, gray16)},
		{"png paletted with transparency", "image/png", encodePNGForTest(t, pal)},
		{"tiny png is enlarged", "image/png", encodePNGForTest(t, noisyImage(3, 2, false))},
		{"apng", "image/apng", apngForTest(t, encodePNGForTest(t, noisyImage(50, 40, true)))},
		{"animated gif", "image/gif", animatedGIFForTest(t, 64, 48)},
		{"animated webp", "image/webp", animatedWebPFixture(t, 48, 32)},
	}
}

// The image handed to the sensitive detector must be byte-for-byte what
// NormalizeImageForDetection produced from the upload before #3995, for
// every input shape, and the other variants must be unchanged.
func TestProcessAllWithDetection_MatchesNormalizeImageForDetection(t *testing.T) {
	p := NewDefaultImageProcessor()
	for _, fx := range detectionFixtures(t) {
		t.Run(fx.name, func(t *testing.T) {
			want, err := NormalizeImageForDetection(fx.body, fx.mime)
			require.NoError(t, err)

			got := p.ProcessAllWithDetection(fx.body, fx.mime)
			require.NotNil(t, got)
			assert.True(t, bytes.Equal(want, got.Detection), "判定に渡る画像が変わった")

			plain := p.ProcessAll(fx.body, fx.mime)
			require.NotNil(t, plain)
			assert.Nil(t, plain.Detection, "判定を頼んでいないのに縮小を作っている")
			assert.Equal(t, plain.Width, got.Width)
			assert.Equal(t, plain.Height, got.Height)
			assert.Equal(t, plain.Blurhash, got.Blurhash)
			assert.Equal(t, plain.Thumbnail, got.Thumbnail)
			assert.Equal(t, plain.Webpublic, got.Webpublic)
		})
	}
}

// Undecodable input: both paths fail, and no detection image is produced.
func TestProcessAllWithDetection_Undecodable(t *testing.T) {
	p := NewDefaultImageProcessor()
	_, err := NormalizeImageForDetection([]byte("not an image"), "image/png")
	require.Error(t, err)
	assert.Nil(t, p.ProcessAllWithDetection([]byte("not an image"), "image/png"))
}

// countDecodes wraps both decoders and reports how many decodes ran.
func countDecodes(t *testing.T) func() int {
	t.Helper()
	var mu sync.Mutex
	var n int
	origAlts, origDet := decodeForAltsFunc, decodeForDetectionFunc
	t.Cleanup(func() { decodeForAltsFunc, decodeForDetectionFunc = origAlts, origDet })
	wrap := func(f func([]byte, string) (image.Image, error)) func([]byte, string) (image.Image, error) {
		return func(body []byte, mime string) (image.Image, error) {
			mu.Lock()
			n++
			mu.Unlock()
			return f(body, mime)
		}
	}
	decodeForAltsFunc = wrap(origAlts)
	decodeForDetectionFunc = wrap(origDet)
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// detectorCapture is an official sensitive-detector stub that records the
// image0 part it received.
type detectorCapture struct {
	mu    sync.Mutex
	calls int
	image []byte
}

func (d *detectorCapture) server(t *testing.T, probability float64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var b []byte
		if fhs := r.MultipartForm.File["image0"]; len(fhs) == 1 {
			f, err := fhs[0].Open()
			if err == nil {
				b, _ = io.ReadAll(f)
				_ = f.Close()
			}
		}
		d.mu.Lock()
		d.calls++
		d.image = b
		d.mu.Unlock()
		_ = json.NewEncoder(w).Encode(officialOKResponse([]Prediction{{ClassName: "Porn", Probability: probability}}))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func detectionUploadService(t *testing.T, ip ImageProcessor, apiURL string, cfg SensitiveConfig) *Service {
	t.Helper()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	s := NewService(testutil.NewMockDriveFileRepository(), testutil.NewMockDriveFolderRepository(),
		NewLocalStorage(t.TempDir(), "https://example.com/files"), idGen, nil, nil, nil, nil)
	s.SetImageProcessor(ip)
	s.SetOfficialSensitiveDetector(NewOfficialDetector(nil))
	s.SetSensitiveSettingsProvider(func() SensitiveSettings {
		return SensitiveSettings{Config: cfg, Official: OfficialDetectorSettings{APIURL: apiURL}}
	})
	return s
}

var detectionOnCfg = SensitiveConfig{Detection: "all", Sensitivity: "medium", SetFlagAutomatically: true}

// With official image detection enabled, an upload decodes the image once
// and the detector receives exactly what NormalizeImageForDetection makes.
func TestUpload_OfficialImageDetectionDecodesOnce(t *testing.T) {
	body := jpegWithOrientation(t, noisyImage(2600, 1500, false), 6)
	want, err := NormalizeImageForDetection(body, "image/jpeg")
	require.NoError(t, err)

	var d detectorCapture
	srv := d.server(t, 0.9)
	s := detectionUploadService(t, NewDefaultImageProcessor(), srv.URL, detectionOnCfg)
	decodes := countDecodes(t)

	f, err := s.Upload(context.Background(), UploadInput{User: &model.User{ID: "u1"}, Body: body, Name: "a.jpg"})
	require.NoError(t, err)
	assert.True(t, f.IsSensitive)
	assert.NotNil(t, f.ThumbnailURL)
	assert.NotNil(t, f.WebpublicURL)
	assert.Equal(t, 1, decodes(), "判定のためにもう一度デコードしている")
	require.Equal(t, 1, d.calls)
	assert.True(t, bytes.Equal(want, d.image), "判定に渡る画像が変わった")
}

// recordingDetectionProcessor is a DetectionImageProcessor that records
// which entry point Upload used.
type recordingDetectionProcessor struct {
	*DefaultImageProcessor
	plain, withDetection int
	// detection overrides ImageAlts.Detection when withDetection is used.
	detection []byte
	dropDet   bool
}

func (r *recordingDetectionProcessor) ProcessAll(body []byte, mime string) *ImageAlts {
	r.plain++
	return r.DefaultImageProcessor.ProcessAll(body, mime)
}

func (r *recordingDetectionProcessor) ProcessAllWithDetection(body []byte, mime string) *ImageAlts {
	r.withDetection++
	alts := r.DefaultImageProcessor.ProcessAllWithDetection(body, mime)
	if alts != nil && r.detection != nil {
		alts.Detection = r.detection
	}
	if alts != nil && r.dropDet {
		alts.Detection = nil
	}
	return alts
}

// The detection image is made only when the official detector will judge
// the upload as a still image; otherwise nothing changes (the decoded image
// is not kept for a detection that does not run).
func TestUpload_DetectionImageOnlyForOfficialImageRoute(t *testing.T) {
	remote := "remote.example"
	body := encodePNGForTest(t, noisyImage(40, 30, false))
	cases := []struct {
		name      string
		apiURL    bool
		cfg       SensitiveConfig
		user      *model.User
		sensitive bool
		want      bool
	}{
		{name: "official enabled", apiURL: true, cfg: detectionOnCfg, user: &model.User{ID: "u1"}, want: true},
		{name: "no detector url", apiURL: false, cfg: detectionOnCfg, user: &model.User{ID: "u1"}},
		{name: "flag not set automatically", apiURL: true, cfg: SensitiveConfig{Detection: "all", Sensitivity: "medium"}, user: &model.User{ID: "u1"}},
		{name: "detection none", apiURL: true, cfg: SensitiveConfig{Detection: "none", Sensitivity: "medium", SetFlagAutomatically: true}, user: &model.User{ID: "u1"}},
		{name: "remote user with local only", apiURL: true, cfg: SensitiveConfig{Detection: "local", Sensitivity: "medium", SetFlagAutomatically: true}, user: &model.User{ID: "u1", Host: &remote}},
		{name: "silenced host", apiURL: true, cfg: SensitiveConfig{Detection: "all", Sensitivity: "medium", SetFlagAutomatically: true, SilencedHosts: []string{remote}}, user: &model.User{ID: "u1", Host: &remote}},
		{name: "already sensitive", apiURL: true, cfg: detectionOnCfg, user: &model.User{ID: "u1"}, sensitive: true},
		{name: "system file", apiURL: true, cfg: detectionOnCfg, user: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var d detectorCapture
			srv := d.server(t, 0.1)
			url := ""
			if tc.apiURL {
				url = srv.URL
			}
			rp := &recordingDetectionProcessor{DefaultImageProcessor: NewDefaultImageProcessor()}
			s := detectionUploadService(t, rp, url, tc.cfg)
			_, err := s.Upload(context.Background(), UploadInput{User: tc.user, Body: body, Name: "a.png", IsSensitive: tc.sensitive})
			require.NoError(t, err)
			if tc.want {
				assert.Equal(t, 1, rp.withDetection)
				assert.Equal(t, 0, rp.plain)
			} else {
				assert.Equal(t, 0, rp.withDetection, "判定しないのに判定用の縮小を作っている")
				assert.Equal(t, 1, rp.plain)
				assert.Equal(t, 0, d.calls)
			}
		})
	}
}

// The detector receives the prepared image as is, and falls back to decoding
// when none was prepared.
func TestUpload_OfficialImageDetectionUsesPreparedImage(t *testing.T) {
	body := encodePNGForTest(t, noisyImage(40, 30, false))
	marker := encodePNGForTest(t, noisyImage(5, 5, false))
	want, err := NormalizeImageForDetection(body, "image/png")
	require.NoError(t, err)

	t.Run("prepared", func(t *testing.T) {
		var d detectorCapture
		srv := d.server(t, 0.1)
		rp := &recordingDetectionProcessor{DefaultImageProcessor: NewDefaultImageProcessor(), detection: marker}
		s := detectionUploadService(t, rp, srv.URL, detectionOnCfg)
		_, err := s.Upload(context.Background(), UploadInput{User: &model.User{ID: "u1"}, Body: body, Name: "a.png"})
		require.NoError(t, err)
		assert.True(t, bytes.Equal(marker, d.image), "用意した縮小を使っていない")
	})
	t.Run("not prepared falls back", func(t *testing.T) {
		var d detectorCapture
		srv := d.server(t, 0.1)
		rp := &recordingDetectionProcessor{DefaultImageProcessor: NewDefaultImageProcessor(), dropDet: true}
		s := detectionUploadService(t, rp, srv.URL, detectionOnCfg)
		decodes := countDecodes(t)
		_, err := s.Upload(context.Background(), UploadInput{User: &model.User{ID: "u1"}, Body: body, Name: "a.png"})
		require.NoError(t, err)
		assert.True(t, bytes.Equal(want, d.image))
		assert.Equal(t, 2, decodes())
	})
}

// sensitiveRouteFor keeps the branch order detectSensitive had before it
// was split out (#3995).
func TestSensitiveRouteFor(t *testing.T) {
	remote := "remote.example"
	local := &model.User{ID: "u1"}
	on := SensitiveSettings{Config: detectionOnCfg, Official: OfficialDetectorSettings{APIURL: "http://detector.example"}}
	onVideos := on
	onVideos.Config.EnableForVideos = true
	legacyOnly := SensitiveSettings{Config: detectionOnCfg}
	legacyVideos := legacyOnly
	legacyVideos.Config.EnableForVideos = true

	official := &Service{officialSensitiveDetector: NewOfficialDetector(nil)}
	legacy := &Service{sensitiveDetector: &stubLegacyDetector{}}

	cases := []struct {
		name     string
		s        *Service
		user     *model.User
		mime     string
		settings SensitiveSettings
		want     sensitiveRoute
	}{
		{"official image", official, local, "image/jpeg", on, sensitiveOfficialImage},
		{"official apng without videos", official, local, "image/apng", on, sensitiveOfficialImage},
		{"official apng with videos", official, local, "image/vnd.mozilla.apng", onVideos, sensitiveOfficialFrames},
		{"official video with videos", official, local, "video/mp4", onVideos, sensitiveOfficialFrames},
		{"official video without videos", official, local, "video/mp4", on, sensitiveSkip},
		{"official non image", official, local, "application/pdf", on, sensitiveSkip},
		{"official without url", official, local, "image/jpeg", legacyOnly, sensitiveSkip},
		{"legacy image", legacy, local, "image/jpeg", legacyOnly, sensitiveLegacy},
		{"legacy video without videos", legacy, local, "video/mp4", legacyOnly, sensitiveSkip},
		{"legacy video with videos", legacy, local, "video/mp4", legacyVideos, sensitiveLegacy},
		{"silenced", official, &model.User{ID: "u2", Host: &remote}, "image/jpeg",
			SensitiveSettings{Config: SensitiveConfig{SilencedHosts: []string{remote}}}, sensitiveSilenced},
		{"not automatic", official, local, "image/jpeg",
			SensitiveSettings{Config: SensitiveConfig{Detection: "all"}, Official: on.Official}, sensitiveSkip},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.s.sensitiveRouteFor(tc.user, tc.mime, tc.settings, false))
		})
	}
}

type stubLegacyDetector struct{}

func (stubLegacyDetector) Detect(context.Context, []byte, string) (float64, error) { return 0, nil }
