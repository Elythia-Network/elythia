package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func officialOKResponse(predictions ...[]Prediction) map[string]any {
	items := make([]map[string]any, 0, len(predictions))
	for _, p := range predictions {
		if p == nil {
			items = append(items, map[string]any{"success": false, "error": map[string]any{"code": "IMAGE_DECODE_FAILED", "message": "x"}})
		} else {
			items = append(items, map[string]any{"success": true, "predictions": p})
		}
	}
	return map[string]any{"success": true, "result": map[string]any{"results": items}}
}

func TestOfficialDetector_DetectMany_Success(t *testing.T) {
	var gotAuth string
	var gotFields []string
	var gotPartTypes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		require.NoError(t, r.ParseMultipartForm(16<<20))
		for i := 0; ; i++ {
			field := fmt.Sprintf("image%d", i)
			fhs := r.MultipartForm.File[field]
			if len(fhs) == 0 {
				break
			}
			gotFields = append(gotFields, field)
			gotPartTypes = append(gotPartTypes, fhs[0].Header.Get("Content-Type"))
		}
		_ = json.NewEncoder(w).Encode(officialOKResponse(
			[]Prediction{{ClassName: "Porn", Probability: 0.9}},
			[]Prediction{{ClassName: "Neutral", Probability: 0.99}},
		))
	}))
	t.Cleanup(srv.Close)

	d := NewOfficialDetector(srv.Client())
	res := d.DetectMany(context.Background(), [][]byte{[]byte("a"), []byte("b")}, OfficialDetectorSettings{
		APIURL: srv.URL, APIKey: "sekrit", TimeoutMs: 5000, MaxImagesPerRequest: 4,
	})
	require.Len(t, res, 2)
	require.NotNil(t, res[0])
	assert.Equal(t, "Porn", res[0][0].ClassName)
	require.NotNil(t, res[1])
	assert.Equal(t, "Bearer sekrit", gotAuth)
	assert.Equal(t, []string{"image0", "image1"}, gotFields)
	assert.Equal(t, []string{"image/png", "image/png"}, gotPartTypes)
}

func TestOfficialDetector_DetectMany_Chunking(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		require.NoError(t, r.ParseMultipartForm(16<<20))
		n := len(r.MultipartForm.File)
		preds := make([][]Prediction, n)
		for i := range preds {
			preds[i] = []Prediction{{ClassName: "Neutral", Probability: 1}}
		}
		_ = json.NewEncoder(w).Encode(officialOKResponse(preds...))
	}))
	t.Cleanup(srv.Close)

	d := NewOfficialDetector(srv.Client())
	res := d.DetectMany(context.Background(), [][]byte{[]byte("a"), []byte("b"), []byte("c")}, OfficialDetectorSettings{
		APIURL: srv.URL, MaxImagesPerRequest: 2,
	})
	require.Len(t, res, 3)
	for i, p := range res {
		assert.NotNil(t, p, "image %d should have predictions", i)
	}
	assert.EqualValues(t, 2, requests.Load(), "3 images / maxImagesPerRequest=2 → 2 requests")
}

func TestOfficialDetector_DetectMany_FailOpen(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"http 500", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) }},
		{"invalid json", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("nope")) }},
		{"success false", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": map[string]any{"code": "AUTHENTICATION_REQUIRED", "message": "x"}})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			t.Cleanup(srv.Close)
			d := NewOfficialDetector(srv.Client())
			res := d.DetectMany(context.Background(), [][]byte{[]byte("a")}, OfficialDetectorSettings{APIURL: srv.URL})
			require.Len(t, res, 1)
			assert.Nil(t, res[0], "failure must fail open (nil predictions)")
		})
	}
}

func TestOfficialDetector_DetectMany_PerItemFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(officialOKResponse(
			nil, // decode 失敗 item
			[]Prediction{{ClassName: "Sexy", Probability: 0.8}},
		))
	}))
	t.Cleanup(srv.Close)
	d := NewOfficialDetector(srv.Client())
	res := d.DetectMany(context.Background(), [][]byte{[]byte("a"), []byte("b")}, OfficialDetectorSettings{APIURL: srv.URL, MaxImagesPerRequest: 4})
	require.Len(t, res, 2)
	assert.Nil(t, res[0])
	assert.NotNil(t, res[1])
}

func TestOfficialDetector_DetectMany_EmptyAndInvalidURL(t *testing.T) {
	d := NewOfficialDetector(nil)
	assert.Empty(t, d.DetectMany(context.Background(), nil, OfficialDetectorSettings{APIURL: "http://x"}))
	res := d.DetectMany(context.Background(), [][]byte{[]byte("a")}, OfficialDetectorSettings{APIURL: ""})
	require.Len(t, res, 1)
	assert.Nil(t, res[0])
	res = d.DetectMany(context.Background(), [][]byte{[]byte("a")}, OfficialDetectorSettings{APIURL: "://bad"})
	require.Len(t, res, 1)
	assert.Nil(t, res[0])
}

func TestOfficialSensitivityThreshold(t *testing.T) {
	// upstream DriveService.ts の三項演算子と同値 (legacy とは異なる)。
	assert.Equal(t, 0.1, OfficialSensitivityThreshold("veryHigh"))
	assert.Equal(t, 0.3, OfficialSensitivityThreshold("high"))
	assert.Equal(t, 0.5, OfficialSensitivityThreshold("medium"))
	assert.Equal(t, 0.7, OfficialSensitivityThreshold("low"))
	assert.Equal(t, 0.9, OfficialSensitivityThreshold("veryLow"))
	assert.Equal(t, 0.5, OfficialSensitivityThreshold("unknown"))
}

func TestJudgePredictions(t *testing.T) {
	sensitive, porn := JudgePredictions([]Prediction{{ClassName: "Neutral", Probability: 0.99}}, 0.5)
	assert.False(t, sensitive)
	assert.False(t, porn)

	sensitive, porn = JudgePredictions([]Prediction{{ClassName: "Sexy", Probability: 0.6}}, 0.5)
	assert.True(t, sensitive)
	assert.False(t, porn)

	sensitive, porn = JudgePredictions([]Prediction{{ClassName: "Porn", Probability: 0.8}}, 0.5)
	assert.True(t, sensitive)
	assert.True(t, porn, "Porn > 0.75 で porn 判定")

	// 境界: > threshold (>= ではない、upstream 準拠)。
	sensitive, _ = JudgePredictions([]Prediction{{ClassName: "Hentai", Probability: 0.5}}, 0.5)
	assert.False(t, sensitive)
}

func TestAggregateFrameJudgements(t *testing.T) {
	// 0 件 → false (upstream 2026.7.0 の全動画センシティブ化バグ修正)。
	assert.False(t, AggregateFrameJudgements(nil, 0.5))
	// 2/3 >= ceil(3*0.5)=2 → true。
	assert.True(t, AggregateFrameJudgements([]bool{true, true, false}, 0.5))
	// 1/3 < 2 → false。
	assert.False(t, AggregateFrameJudgements([]bool{true, false, false}, 0.5))
}

// makeDetectionTestPNG returns an all-transparent (or colored) PNG of the given size.
func makeDetectionTestPNG(t *testing.T, w, h int, c color.NRGBA) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func TestNormalizeImageForDetection(t *testing.T) {
	// 100x50 の透過画像 → 299x299 PNG、透過部分は 18% グレー (119,119,119)。
	body := makeDetectionTestPNG(t, 100, 50, color.NRGBA{R: 0, G: 0, B: 0, A: 0})
	out, err := NormalizeImageForDetection(body, "image/png")
	require.NoError(t, err)

	img, err := png.Decode(bytes.NewReader(out))
	require.NoError(t, err)
	assert.Equal(t, 299, img.Bounds().Dx())
	assert.Equal(t, 299, img.Bounds().Dy())
	r, g, b, a := img.At(150, 150).RGBA()
	assert.EqualValues(t, 119, r>>8)
	assert.EqualValues(t, 119, g>>8)
	assert.EqualValues(t, 119, b>>8)
	assert.EqualValues(t, 255, a>>8)
}

func TestNormalizeImageForDetection_InvalidInput(t *testing.T) {
	_, err := NormalizeImageForDetection([]byte("not an image"), "image/png")
	assert.Error(t, err)
}

// stubFrameExtractor implements VideoProcessor + DetectionFrameExtractor.
type stubFrameExtractor struct {
	frames [][]byte
	err    error
}

func (s *stubFrameExtractor) GenerateThumbnail(_ context.Context, _ []byte, _ string) (*ProcessedImage, error) {
	return nil, nil
}
func (s *stubFrameExtractor) ExtractDetectionFrames(_ context.Context, _ []byte) ([][]byte, error) {
	return s.frames, s.err
}

func officialTestService(t *testing.T, detectorURL string, cfg SensitiveConfig, official OfficialDetectorSettings) *Service {
	t.Helper()
	s := &Service{}
	s.SetOfficialSensitiveDetector(NewOfficialDetector(nil))
	official.APIURL = detectorURL
	s.SetSensitiveSettingsProvider(func() SensitiveSettings {
		return SensitiveSettings{Config: cfg, Official: official}
	})
	return s
}

func TestDetectSensitiveOfficial_ImagePath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 正規化済み 299x299 PNG が送られてくること (part のデコード検証)。
		require.NoError(t, r.ParseMultipartForm(16<<20))
		fhs := r.MultipartForm.File["image0"]
		require.Len(t, fhs, 1)
		f, err := fhs[0].Open()
		require.NoError(t, err)
		defer f.Close()
		img, err := png.Decode(f)
		require.NoError(t, err)
		require.Equal(t, 299, img.Bounds().Dx())
		_ = json.NewEncoder(w).Encode(officialOKResponse([]Prediction{{ClassName: "Porn", Probability: 0.9}}))
	}))
	t.Cleanup(srv.Close)

	s := officialTestService(t, srv.URL, SensitiveConfig{
		Detection: "all", Sensitivity: "medium", SetFlagAutomatically: true,
	}, OfficialDetectorSettings{})
	body := makeDetectionTestPNG(t, 40, 40, color.NRGBA{R: 200, G: 10, B: 10, A: 255})
	assert.True(t, s.detectSensitive(context.Background(), &model.User{}, body, "image/png"))
}

func TestDetectSensitiveOfficial_ImageBelowThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(officialOKResponse([]Prediction{{ClassName: "Porn", Probability: 0.2}}))
	}))
	t.Cleanup(srv.Close)
	s := officialTestService(t, srv.URL, SensitiveConfig{
		Detection: "all", Sensitivity: "medium", SetFlagAutomatically: true,
	}, OfficialDetectorSettings{})
	body := makeDetectionTestPNG(t, 40, 40, color.NRGBA{A: 255})
	assert.False(t, s.detectSensitive(context.Background(), &model.User{}, body, "image/png"))
}

func TestDetectSensitiveOfficial_DetectorDown_FailOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	s := officialTestService(t, srv.URL, SensitiveConfig{
		Detection: "all", Sensitivity: "medium", SetFlagAutomatically: true,
	}, OfficialDetectorSettings{})
	body := makeDetectionTestPNG(t, 40, 40, color.NRGBA{A: 255})
	assert.False(t, s.detectSensitive(context.Background(), &model.User{}, body, "image/png"))
}

func TestDetectSensitiveOfficial_VideoFrames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseMultipartForm(16<<20))
		n := len(r.MultipartForm.File)
		preds := make([][]Prediction, n)
		for i := range preds {
			preds[i] = []Prediction{{ClassName: "Hentai", Probability: 0.9}}
		}
		_ = json.NewEncoder(w).Encode(officialOKResponse(preds...))
	}))
	t.Cleanup(srv.Close)

	s := officialTestService(t, srv.URL, SensitiveConfig{
		Detection: "all", Sensitivity: "medium", SetFlagAutomatically: true, EnableForVideos: true,
	}, OfficialDetectorSettings{})
	s.videoProcessor = &stubFrameExtractor{frames: [][]byte{[]byte("f1"), []byte("f2")}}
	assert.True(t, s.detectSensitive(context.Background(), &model.User{}, []byte("video-bytes"), "video/mp4"))
}

func TestDetectSensitiveOfficial_VideoDisabled(t *testing.T) {
	s := officialTestService(t, "http://unused.invalid", SensitiveConfig{
		Detection: "all", Sensitivity: "medium", SetFlagAutomatically: true, EnableForVideos: false,
	}, OfficialDetectorSettings{})
	assert.False(t, s.detectSensitive(context.Background(), &model.User{}, []byte("video"), "video/mp4"))
}

func TestDetectSensitiveOfficial_VideoWithoutExtractor(t *testing.T) {
	s := officialTestService(t, "http://unused.invalid", SensitiveConfig{
		Detection: "all", Sensitivity: "medium", SetFlagAutomatically: true, EnableForVideos: true,
	}, OfficialDetectorSettings{})
	// videoProcessor 未設定 (DetectionFrameExtractor 非対応) → fail-open false。
	assert.False(t, s.detectSensitive(context.Background(), &model.User{}, []byte("video"), "video/mp4"))
}

func TestDetectSensitiveOfficial_NonImageMime(t *testing.T) {
	s := officialTestService(t, "http://unused.invalid", SensitiveConfig{
		Detection: "all", Sensitivity: "medium", SetFlagAutomatically: true,
	}, OfficialDetectorSettings{})
	assert.False(t, s.detectSensitive(context.Background(), &model.User{}, []byte("%PDF-1.4"), "application/pdf"))
}

// 公式 URL が未設定なら legacy detector に fallback する。
func TestDetectSensitive_OfficialUnsetFallsBackToLegacy(t *testing.T) {
	s := &Service{}
	s.SetSensitiveDetection(stubDetector{score: 0.9}, SensitiveConfig{})
	s.SetOfficialSensitiveDetector(NewOfficialDetector(nil))
	s.SetSensitiveSettingsProvider(func() SensitiveSettings {
		return SensitiveSettings{
			Config:   SensitiveConfig{Detection: "all", Sensitivity: "medium", SetFlagAutomatically: true},
			Official: OfficialDetectorSettings{APIURL: ""},
		}
	})
	assert.True(t, s.detectSensitive(context.Background(), &model.User{}, []byte("data"), "image/png"))
}
