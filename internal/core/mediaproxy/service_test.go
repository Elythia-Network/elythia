package mediaproxy

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- mock AllowlistChecker ---

type mockAllowlist struct {
	allowed map[string]bool
	// lookups counts IsAllowedURL calls, so tests can show a value was refused
	// without reaching the database.
	lookups int
}

func (m *mockAllowlist) IsAllowedURL(_ context.Context, url string) (bool, error) {
	m.lookups++
	return m.allowed[url], nil
}

// --- mock Storage ---

type mockStorage struct {
	files map[string][]byte
}

func (m *mockStorage) Put(_ string, _ io.Reader) (string, error) { return "", nil }
func (m *mockStorage) Delete(_ string) error                     { return nil }
func (m *mockStorage) Get(key string) (io.ReadCloser, error) {
	data, ok := m.files[key]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return io.NopCloser(io.Reader(io.NopCloser(
		&byteReadCloser{data: data, pos: 0},
	))), nil
}

type byteReadCloser struct {
	data []byte
	pos  int
}

func (b *byteReadCloser) Read(p []byte) (int, error) {
	if b.pos >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.pos:])
	b.pos += n
	return n, nil
}
func (b *byteReadCloser) Close() error { return nil }

// --- test helpers ---

// testAllowedCIDRs はテストでhttptest (127.0.0.1) への接続を許可するCIDRリスト
var testAllowedCIDRs = []string{"127.0.0.0/8", "::1/128"}

func testService(allowedURLs map[string]bool) *Service {
	return NewService(
		"https://example.com",
		"Misskey/2026.5.1 (https://example.com)",
		&mockStorage{files: map[string][]byte{}},
		&mockAllowlist{allowed: allowedURLs},
		[]byte("test-secret"),
		testAllowedCIDRs,
	)
}

func makePNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	w := &byteWriter{}
	_ = png.Encode(w, img)
	return w.data
}

// makeBadgePNG builds a 100x100 image with a horizontal gradient, so the badge
// pipeline's entropy guard (#2920) does not skip it. makePNG is a solid colour
// and is intentionally kept that way — it exercises the skip path.
func makeBadgePNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			v := uint8(x * 255 / 99)
			img.Set(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	w := &byteWriter{}
	_ = png.Encode(w, img)
	return w.data
}

type byteWriter struct {
	data []byte
}

func (w *byteWriter) Write(p []byte) (int, error) {
	w.data = append(w.data, p...)
	return len(p), nil
}

// --- tests ---

func TestAuthorize_ValidHMAC(t *testing.T) {
	s := testService(nil)
	url := "https://remote.example/avatar.png"
	sig := s.SignURL(url)

	err := s.Authorize(context.Background(), url, sig)
	assert.NoError(t, err)
}

// **期限付きの署名も受ける (#3037)。** `/url` が発行する形。
func TestAuthorize_ValidExpiringHMAC(t *testing.T) {
	s := testService(map[string]bool{})
	target := "https://evil.example/anything.png"

	sig := SignURLUntil([]byte("test-secret"), target, time.Now().Add(time.Hour))
	assert.NoError(t, s.Authorize(context.Background(), target, sig))
}

// **期限を過ぎたら allowlist に落ちる。** allowlist に無い URL なので
// `ErrUnauthorized`。
func TestAuthorize_ExpiredHMACIsUnauthorized(t *testing.T) {
	s := testService(map[string]bool{})
	target := "https://evil.example/anything.png"

	sig := SignURLUntil([]byte("test-secret"), target, time.Now().Add(-time.Second))
	assert.ErrorIs(t, s.Authorize(context.Background(), target, sig), ErrUnauthorized)
}

func TestAuthorize_InvalidHMAC_AllowlistedURL(t *testing.T) {
	s := testService(map[string]bool{
		"https://remote.example/avatar.png": true,
	})

	err := s.Authorize(context.Background(), "https://remote.example/avatar.png", "wrong-sig")
	assert.NoError(t, err)
}

func TestAuthorize_NoSig_AllowlistedURL(t *testing.T) {
	s := testService(map[string]bool{
		"https://remote.example/avatar.png": true,
	})

	err := s.Authorize(context.Background(), "https://remote.example/avatar.png", "")
	assert.NoError(t, err)
}

func TestAuthorize_NoSig_NotAllowlisted(t *testing.T) {
	s := testService(map[string]bool{})

	err := s.Authorize(context.Background(), "https://evil.example/malware.exe", "")
	assert.ErrorIs(t, err, ErrUnauthorized)
}

func TestAuthorize_InvalidHMAC_NotAllowlisted(t *testing.T) {
	s := testService(map[string]bool{})

	err := s.Authorize(context.Background(), "https://evil.example/malware.exe", "bad-sig")
	assert.ErrorIs(t, err, ErrUnauthorized)
}

func TestFetch_RemoteImage(t *testing.T) {
	imgData := makePNG()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imgData)
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/img.png": true})

	result, err := s.Fetch(context.Background(), ts.URL+"/img.png", ModeDefault, FormatWebP, true)
	require.NoError(t, err)
	defer result.Body.Close()

	assert.Equal(t, "image/png", result.ContentType)

	body, _ := io.ReadAll(result.Body)
	assert.NotEmpty(t, body)
}

func TestFetch_RemoteImage_Emoji(t *testing.T) {
	imgData := makePNG()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imgData)
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/emoji.png": true})

	result, err := s.Fetch(context.Background(), ts.URL+"/emoji.png", ModeEmoji, FormatWebP, true)
	require.NoError(t, err)
	defer result.Body.Close()

	assert.Equal(t, "image/webp", result.ContentType)
}

func TestFetch_RemoteImage_Avatar(t *testing.T) {
	imgData := makePNG()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imgData)
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/avatar.png": true})

	result, err := s.Fetch(context.Background(), ts.URL+"/avatar.png", ModeAvatar, FormatWebP, true)
	require.NoError(t, err)
	defer result.Body.Close()

	assert.Equal(t, "image/webp", result.ContentType)
}

// 型を名乗らない Content-Type は、標準の綴りでなくても中身から判定し直す。
//
// **`binary/octet-stream` は非標準だが実在する。** S3 互換ストレージの一部が
// 返しており、これを素通りさせていたせいで、該当するインスタンスのアイコン・
// バナー・カスタム絵文字が resize 経路でまとめて 404 になっていた (#2541)。
func TestFetch_RemoteImage_SniffsUnknownBinary(t *testing.T) {
	for _, contentType := range []string{
		"application/octet-stream",
		"binary/octet-stream",
		"",
	} {
		t.Run(contentType, func(t *testing.T) {
			imgData := makePNG()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if contentType != "" {
					w.Header().Set("Content-Type", contentType)
				} else {
					// Go は書き込み時に自動で sniff するので、明示的に消す。
					w.Header()["Content-Type"] = nil
				}
				_, _ = w.Write(imgData)
			}))
			defer ts.Close()

			s := testService(map[string]bool{ts.URL + "/avatar.png": true})

			result, err := s.Fetch(context.Background(), ts.URL+"/avatar.png", ModeAvatar, FormatWebP, true)
			require.NoError(t, err)
			defer result.Body.Close()

			assert.Equal(t, "image/webp", result.ContentType)
		})
	}
}

func TestIsUnknownBinary(t *testing.T) {
	for _, ct := range []string{"", "application/octet-stream", "binary/octet-stream"} {
		assert.True(t, isUnknownBinary(ct), "型を名乗っていないものを見落としている: %q", ct)
	}
	// 型を名乗っているものは尊重する。stored MIME を上書きすると、
	// DetectContentType が認識しない HEIC/AVIF/JXL が壊れる。
	for _, ct := range []string{"image/png", "image/heic", "image/jxl", "application/pdf"} {
		assert.False(t, isUnknownBinary(ct), "名乗っている型を捨てている: %q", ct)
	}
}

func TestFetch_RemoteImage_Static(t *testing.T) {
	imgData := makePNG()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imgData)
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/img.png": true})

	result, err := s.Fetch(context.Background(), ts.URL+"/img.png", ModeStatic, FormatWebP, false)
	require.NoError(t, err)
	defer result.Body.Close()

	assert.Equal(t, "image/webp", result.ContentType)
}

func TestFetch_RemoteImage_Preview(t *testing.T) {
	imgData := makePNG()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imgData)
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/img.png": true})

	result, err := s.Fetch(context.Background(), ts.URL+"/img.png", ModePreview, FormatWebP, true)
	require.NoError(t, err)
	defer result.Body.Close()

	assert.Equal(t, "image/webp", result.ContentType)
}

func TestFetch_RemoteImage_Badge(t *testing.T) {
	// **単色だと entropy guard に落ちて 404 になる (#2920)。** グラデーションで
	// 中身のある画像を使う。単色が 404 になること自体は
	// TestProcessBadge_BlankImageIs404 が固定する。
	imgData := makeBadgePNG()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(imgData)
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/img.png": true})

	result, err := s.Fetch(context.Background(), ts.URL+"/img.png", ModeBadge, FormatWebP, true)
	require.NoError(t, err)
	defer result.Body.Close()

	assert.Equal(t, "image/png", result.ContentType)
}

// アニメ GIF 等の multi-frame 形式は emoji / avatar / preview mode で
// pass-through されることを確認する (#941)。Go の image.Decode は 1 frame
// しか返さないため resize 経路に乗せると静止化してしまうため。
func TestFetch_RemoteImage_AnimatedGIF_PassThroughOnEmoji(t *testing.T) {
	gifData := []byte("GIF89a") // valid な animation でなくとも MIME type で判定される
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		_, _ = w.Write(gifData)
	}))
	defer ts.Close()
	s := testService(map[string]bool{ts.URL + "/anim.gif": true})

	for _, mode := range []ProxyMode{ModeEmoji, ModeAvatar, ModePreview} {
		result, err := s.Fetch(context.Background(), ts.URL+"/anim.gif", mode, FormatWebP, true)
		require.NoError(t, err, "mode=%v", mode)
		assert.Equal(t, "image/gif", result.ContentType, "mode=%v should preserve animated MIME", mode)
		result.Body.Close()
	}
}

func TestFetch_RemoteImage_AnimatedAPNG_PassThroughOnEmoji(t *testing.T) {
	pngData := makePNG()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/apng")
		_, _ = w.Write(pngData)
	}))
	defer ts.Close()
	s := testService(map[string]bool{ts.URL + "/anim.apng": true})

	result, err := s.Fetch(context.Background(), ts.URL+"/anim.apng", ModeEmoji, FormatWebP, true)
	require.NoError(t, err)
	defer result.Body.Close()
	assert.Equal(t, "image/apng", result.ContentType)
}

func TestIsAnimatedFormat(t *testing.T) {
	assert.True(t, isAnimatedFormat("image/gif"))
	assert.True(t, isAnimatedFormat("image/apng"))
	assert.True(t, isAnimatedFormat("image/vnd.mozilla.apng"))
	assert.False(t, isAnimatedFormat("image/png"))
	assert.False(t, isAnimatedFormat("image/jpeg"))
	assert.False(t, isAnimatedFormat("image/webp"))
	assert.False(t, isAnimatedFormat(""))
}

func TestFetch_Remote404(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/missing.png": true})

	_, err := s.Fetch(context.Background(), ts.URL+"/missing.png", ModeDefault, FormatWebP, true)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestFetch_LocalFile(t *testing.T) {
	imgData := makePNG()

	s := NewService(
		"https://example.com",
		"Misskey/2026.5.1 (https://example.com)",
		&mockStorage{files: map[string][]byte{"abc123": imgData}},
		&mockAllowlist{allowed: map[string]bool{}},
		[]byte("test-secret"),
		nil,
	)

	result, err := s.Fetch(context.Background(), "https://example.com/files/abc123", ModeDefault, FormatWebP, true)
	require.NoError(t, err)
	defer result.Body.Close()

	assert.Equal(t, "image/png", result.ContentType)
}

// favicon.ico を返すリモートサーバーは IANA 公式 MIME `image/vnd.microsoft.icon`
// を使う場合がある (#418)。古い慣例の `image/x-icon` だけで safe-list を組むと
// 多くのリモートホストの favicon が unsafe MIME 拒否で 500 になり、UI 上で
// instance ticker のアイコンが消える。両 alias とも許容することを確認する。
func TestFetch_FaviconWithIANAMIMETypeAccepted(t *testing.T) {
	imgData := []byte{0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x10, 0x10} // bogus ico header
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/vnd.microsoft.icon")
		_, _ = w.Write(imgData)
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/favicon.ico": true})

	result, err := s.Fetch(context.Background(), ts.URL+"/favicon.ico", ModeDefault, FormatWebP, true)
	require.NoError(t, err, "image/vnd.microsoft.icon must pass through")
	defer result.Body.Close()
	assert.Equal(t, "image/vnd.microsoft.icon", result.ContentType)
}

func TestFetch_ContentTypeWithParameters(t *testing.T) {
	// Content-Type に charset 等のパラメータが付いていても media type だけで
	// allowlist 照合する (#418 Devin review)。
	imgData := makePNG()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png; charset=binary")
		_, _ = w.Write(imgData)
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/img.png": true})

	result, err := s.Fetch(context.Background(), ts.URL+"/img.png", ModeDefault, FormatWebP, true)
	require.NoError(t, err, "media type should match after stripping parameters")
	defer result.Body.Close()
	assert.Equal(t, "image/png", result.ContentType)
}

func TestFetch_FaviconWithLegacyMIMETypeAccepted(t *testing.T) {
	// 古い慣例 image/x-icon も引き続き許可する (regression guard)。
	imgData := []byte{0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x10, 0x10}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		_, _ = w.Write(imgData)
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/favicon.ico": true})

	result, err := s.Fetch(context.Background(), ts.URL+"/favicon.ico", ModeDefault, FormatWebP, true)
	require.NoError(t, err)
	defer result.Body.Close()
	assert.Equal(t, "image/x-icon", result.ContentType)
}

func TestFetch_UnsafeMIME_Rejected(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		_, _ = w.Write([]byte("alert('xss')"))
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/evil.js": true})

	_, err := s.Fetch(context.Background(), ts.URL+"/evil.js", ModeDefault, FormatWebP, true)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "rejected MIME type")
}

func TestDummyPNG(t *testing.T) {
	result := DummyPNG()
	defer result.Body.Close()
	assert.Equal(t, "image/png", result.ContentType)
	data, _ := io.ReadAll(result.Body)
	assert.NotEmpty(t, data)
}

func TestIsConvertibleImage(t *testing.T) {
	tests := []struct {
		mime string
		want bool
	}{
		{"image/png", true},
		{"image/jpeg", true},
		{"image/gif", true},
		{"image/webp", true},
		{"image/bmp", true},
		{"image/tiff", true},
		// IANA 公式名 (vnd.microsoft.icon) と古い慣例 (x-icon) を両方許可 (#418)
		{"image/x-icon", true},
		{"image/vnd.microsoft.icon", true},
		// #672 Phase 1: pure Go decoder で input transcode 対応
		{"image/x-portable-bitmap", true},
		{"image/x-portable-graymap", true},
		{"image/x-portable-pixmap", true},
		{"image/x-portable-anymap", true},
		{"image/x-tga", true},
		{"image/x-targa", true},
		// #734: pure Go JPEG 2000 decoder (mrjoshuak/go-jpeg2000)。
		// JP2/J2K は完全対応、JPX (Part 2) は library 未対応のため pass-through
		// のみで convertible では false。
		{"image/jp2", true},
		{"image/jpeg2000", true},
		{"image/jpx", false},
		// #672 Phase 1 partial: JXR / MNG は decode library が無く pass-through 用
		// にのみ browsersafe 許可。convertible では無いので false。
		{"image/jxr", false},
		{"video/x-mng", false},
		{"image/svg+xml", false},
		{"video/mp4", false},
		{"text/plain", false},
	}
	for _, tt := range tests {
		t.Run(tt.mime, func(t *testing.T) {
			assert.Equal(t, tt.want, isConvertibleImage(tt.mime))
		})
	}
}

func TestBrowsersafeMIMEs(t *testing.T) {
	assert.True(t, browsersafeMIMEs["image/png"])
	assert.True(t, browsersafeMIMEs["video/mp4"])
	assert.True(t, browsersafeMIMEs["audio/mpeg"])
	// favicon.ico の MIME alias 両方を許可 (#418)
	assert.True(t, browsersafeMIMEs["image/x-icon"])
	assert.True(t, browsersafeMIMEs["image/vnd.microsoft.icon"])
	// #672 Phase 1 で追加した formats は browsersafe (decode 経路あり or
	// pass-through) で受け入れる
	assert.True(t, browsersafeMIMEs["image/x-portable-pixmap"])
	assert.True(t, browsersafeMIMEs["image/x-tga"])
	assert.True(t, browsersafeMIMEs["image/jp2"])
	assert.True(t, browsersafeMIMEs["image/jpeg2000"])
	assert.True(t, browsersafeMIMEs["image/jxr"])
	assert.True(t, browsersafeMIMEs["video/x-mng"])
	assert.False(t, browsersafeMIMEs["application/javascript"])
	assert.False(t, browsersafeMIMEs["text/html"])
}

func TestFetch_SVG_ReturnsDummyPNG(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect width="10" height="10"/></svg>`))
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/icon.svg": true})

	result, err := s.Fetch(context.Background(), ts.URL+"/icon.svg", ModeDefault, FormatWebP, true)
	require.NoError(t, err)
	defer result.Body.Close()

	// SVGはXSSリスクがあるためダミーPNGにフォールバック
	assert.Equal(t, "image/png", result.ContentType)
}

func TestFetch_LocalFile_NotFound(t *testing.T) {
	s := NewService(
		"https://example.com",
		"Misskey/2026.5.1 (https://example.com)",
		&mockStorage{files: map[string][]byte{}},
		&mockAllowlist{allowed: map[string]bool{}},
		[]byte("test-secret"),
		nil,
	)

	_, err := s.Fetch(context.Background(), "https://example.com/files/nonexistent", ModeDefault, FormatWebP, true)
	assert.Error(t, err)
}

func TestFetch_LocalFile_EmptyAccessKey(t *testing.T) {
	s := NewService(
		"https://example.com",
		"Misskey/2026.5.1 (https://example.com)",
		&mockStorage{files: map[string][]byte{}},
		&mockAllowlist{allowed: map[string]bool{}},
		[]byte("test-secret"),
		nil,
	)

	_, err := s.Fetch(context.Background(), "https://example.com/files/", ModeDefault, FormatWebP, true)
	assert.ErrorIs(t, err, ErrBadRequest)
}

func TestFetch_LocalFile_WithPathSegments(t *testing.T) {
	imgData := makePNG()
	s := NewService(
		"https://example.com",
		"Misskey/2026.5.1 (https://example.com)",
		&mockStorage{files: map[string][]byte{"abc123": imgData}},
		&mockAllowlist{allowed: map[string]bool{}},
		[]byte("test-secret"),
		nil,
	)

	// /files/abc123/extra のようなパスでもabc123だけ使う
	result, err := s.Fetch(context.Background(), "https://example.com/files/abc123/extra", ModeDefault, FormatWebP, true)
	require.NoError(t, err)
	defer result.Body.Close()
	assert.Equal(t, "image/png", result.ContentType)
}

func TestFetch_LocalFile_Emoji(t *testing.T) {
	imgData := makePNG()
	s := NewService(
		"https://example.com",
		"Misskey/2026.5.1 (https://example.com)",
		&mockStorage{files: map[string][]byte{"emoji1": imgData}},
		&mockAllowlist{allowed: map[string]bool{}},
		[]byte("test-secret"),
		nil,
	)

	result, err := s.Fetch(context.Background(), "https://example.com/files/emoji1", ModeEmoji, FormatWebP, true)
	require.NoError(t, err)
	defer result.Body.Close()
	assert.Equal(t, "image/webp", result.ContentType)
}

func TestFetch_RemoteServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/error.png": true})

	_, err := s.Fetch(context.Background(), ts.URL+"/error.png", ModeDefault, FormatWebP, true)
	assert.Error(t, err)
}

func TestFetch_RemoteGone(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/deleted.png": true})

	_, err := s.Fetch(context.Background(), ts.URL+"/deleted.png", ModeDefault, FormatWebP, true)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestFetch_RemoteNoContentType(t *testing.T) {
	imgData := makePNG()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Content-Typeヘッダなしで返す
		_, _ = w.Write(imgData)
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/img.png": true})

	result, err := s.Fetch(context.Background(), ts.URL+"/img.png", ModeDefault, FormatWebP, true)
	require.NoError(t, err)
	defer result.Body.Close()
	// auto-detected from content
	assert.Equal(t, "image/png", result.ContentType)
}

func TestProcessResize_NonConvertibleImage(t *testing.T) {
	s := testService(nil)
	data := []byte("not an image")
	result, err := s.processResize(data, "application/octet-stream", 100, 100, FormatWebP)
	require.NoError(t, err)
	defer result.Body.Close()
	// 変換できない場合はそのまま返す
	assert.Equal(t, "application/octet-stream", result.ContentType)
}

// **変換できない MIME は 404 (#2920)。** upstream は
// `requiresImageConversion && !isConvertibleImage` で `Unexpected mime` を
// 404 にする。**本番経路では Fetch 側の同じ判定が先に効く**ので挙動は変わらないが、
// processBadge 単体の契約として揃えておく。
func TestProcessBadge_NonConvertibleImage(t *testing.T) {
	s := testService(nil)
	data := []byte("not an image")
	_, err := s.processBadge(data, "text/plain")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestResizeToHeight_SmallImage(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 50, 50))
	result := resizeToHeight(img, 128)
	// 元画像が128以下なので拡大されない
	assert.Equal(t, 50, result.Bounds().Dy())
}

func TestResizeFit_SmallImage(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 50, 50))
	result := resizeFit(img, 498, 422)
	// 元画像が範囲内なのでそのまま
	assert.Equal(t, 50, result.Bounds().Dx())
	assert.Equal(t, 50, result.Bounds().Dy())
}

func TestDecodeImage_InvalidData(t *testing.T) {
	_, err := decodeImage([]byte("not an image"), "image/jpeg")
	assert.Error(t, err)
}

// TestDecodeImage_PPM は #672 Phase 1 で追加した Netpbm 系 (spakin/netpbm)
// の input decode 経路が standard image.Decode 経由で動作することを確認する。
// 単純な 2x2 PPM (P3) を decode して bounds が期待通りなら OK。
func TestDecodeImage_PPM(t *testing.T) {
	// P3 = ASCII PPM, 2x2, max=255。R G B が 4 ピクセル分。
	ppm := []byte("P3\n2 2\n255\n255 0 0 0 255 0 0 0 255 255 255 0\n")
	img, err := decodeImage(ppm, "image/x-portable-pixmap")
	require.NoError(t, err)
	assert.Equal(t, 2, img.Bounds().Dx())
	assert.Equal(t, 2, img.Bounds().Dy())
}

// TestFetch_PassThrough_JXR は #672 Phase 1 partial: JPEG XR / MNG family
// が browsersafeMIMEs に追加されたことで 415 で reject されず pass-through
// で raw bytes が配信されることを assert する。pure Go decoder 無しの
// formats が browser ネイティブ対応 (Edge legacy / viewer plugin) に委譲
// される経路の regression guard。
func TestFetch_PassThrough_JXR(t *testing.T) {
	// 適当なバイト列 (実 JXR ファイル相当の最小マジック)。decoder は通らない
	// ので中身が valid である必要は無い。
	jxrBytes := []byte("II\xbc\x01" + "fakejxrpayload")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jxr")
		_, _ = w.Write(jxrBytes)
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/img.jxr": true})
	result, err := s.Fetch(context.Background(), ts.URL+"/img.jxr", ModeDefault, FormatWebP, true)
	require.NoError(t, err)
	defer result.Body.Close()

	assert.Equal(t, "image/jxr", result.ContentType, "JXR は pass-through で content-type 維持")
	body, _ := io.ReadAll(result.Body)
	assert.Equal(t, jxrBytes, body, "raw bytes がそのまま返る (transcode しない)")
}

// TestFetch_PassThrough_MNG は MNG が同様に pass-through されることを確認。
func TestFetch_PassThrough_MNG(t *testing.T) {
	mngBytes := []byte("\x8aMNG\r\n\x1a\n" + "fakemngpayload")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/x-mng")
		_, _ = w.Write(mngBytes)
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/anim.mng": true})
	result, err := s.Fetch(context.Background(), ts.URL+"/anim.mng", ModeDefault, FormatWebP, true)
	require.NoError(t, err)
	defer result.Body.Close()

	assert.Equal(t, "video/x-mng", result.ContentType)
	body, _ := io.ReadAll(result.Body)
	assert.Equal(t, mngBytes, body)
}

// TestDecodeImage_JP2 は service_jp2_test.go に移動した (#1088)。
// 3rd party lib `mrjoshuak/go-jpeg2000` 内部の data race を踏むため
// `-race` build から除外する必要があり、build tag (`//go:build !race`)
// で separate file 化している。

// TestDecodeImage_TGA は #672 Phase 1 で追加した TGA decoder (blezek/tga)
// が contentType 経由で manual dispatch されることを確認する。
// blezek/tga は magic bytes 無し format のため image.RegisterFormat には載
// せず、decodeImage 内で MIME type 判定で dispatch する設計。
func TestDecodeImage_TGA(t *testing.T) {
	// 最小 uncompressed TGA v2: header 18 + pixel 4 + footer 26 = 48 bytes。
	// footer は v2 signature "TRUEVISION-XFILE" を含み、blezek/tga が
	// SeekEnd-26 で読みに行く (footer 無いと "negative position" エラー)。
	header := []byte{
		0,    // ID length
		0,    // color map type (none)
		2,    // image type: uncompressed true-color
		0, 0, // color map first index
		0, 0, // color map length
		0,    // color map entry size
		0, 0, // x origin
		0, 0, // y origin
		1, 0, // width = 1
		1, 0, // height = 1
		32,   // bpp
		0x28, // image descriptor: top-left origin (bit5=1) + 8 alpha bits
	}
	pixel := []byte{0x00, 0xff, 0x00, 0xff} // BGRA pixel (green opaque)
	footer := make([]byte, 26)
	// 4 bytes extension offset = 0, 4 bytes developer offset = 0
	copy(footer[8:], "TRUEVISION-XFILE.\x00")
	tgaBytes := append(append(header, pixel...), footer...)

	img, err := decodeImage(tgaBytes, "image/x-tga")
	require.NoError(t, err)
	assert.Equal(t, 1, img.Bounds().Dx())
	assert.Equal(t, 1, img.Bounds().Dy())
}

func TestEncodeWebP(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	data, err := encodeWebP(img)
	require.NoError(t, err)
	assert.NotEmpty(t, data)
}

func TestSignURL_MatchesStandalone(t *testing.T) {
	s := testService(nil)
	url := "https://example.com/test.png"
	sig := s.SignURL(url)
	assert.Equal(t, SignURL([]byte("test-secret"), url), sig)
}

func TestResizeToHeight_LargeImage(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 500, 500))
	result := resizeToHeight(img, 128)
	assert.Equal(t, 128, result.Bounds().Dy())
}

func TestResizeFit_LargeImage(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1000, 1000))
	result := resizeFit(img, 200, 200)
	assert.LessOrEqual(t, result.Bounds().Dx(), 200)
	assert.LessOrEqual(t, result.Bounds().Dy(), 200)
}

func TestFetch_Remote_InvalidURL(t *testing.T) {
	s := testService(map[string]bool{"not://valid": true})

	_, err := s.Fetch(context.Background(), "not://valid", ModeDefault, FormatWebP, true)
	assert.Error(t, err)
}

func TestProcessResize_LargeImage_WidthAndHeight(t *testing.T) {
	s := testService(nil)
	imgData := makePNG() // 100x100

	result, err := s.processResize(imgData, "image/png", 50, 50, FormatWebP)
	require.NoError(t, err)
	defer result.Body.Close()
	assert.Equal(t, "image/webp", result.ContentType)
}

func TestProcessResize_HeightOnly(t *testing.T) {
	s := testService(nil)
	imgData := makePNG() // 100x100

	result, err := s.processResize(imgData, "image/png", 0, 50, FormatWebP)
	require.NoError(t, err)
	defer result.Body.Close()
	assert.Equal(t, "image/webp", result.ContentType)
}

func TestProcessBadge_ValidImage(t *testing.T) {
	s := testService(nil)
	imgData := makeBadgePNG() // 100x100 のグラデーション

	result, err := s.processBadge(imgData, "image/png")
	require.NoError(t, err)
	defer result.Body.Close()
	assert.Equal(t, "image/png", result.ContentType)
}

// allowlist を**引けなかった**ことと「許可されていない」を分ける (#3036)。
//
// **潰すと handler が 403 + `Cache-Control: max-age=86400` で返す。**
// PostgreSQL の瞬断のあいだ、`sig` を持たないすべてのプロキシ URL が 403 に
// なり 1 日焼き付く。#2913 と同じ症状で #2792 にも反する。#3034 が直した
// 「リモート取得の失敗を 404 に潰す」より影響が広い — あちらは 1 URL ずつ
// だが、こちらは障害中の全 URL が同時に焼き付く。
func TestAuthorize_AllowlistErrorIsNotUnauthorized(t *testing.T) {
	s := NewService(
		"https://example.com",
		"Misskey/2026.5.1 (https://example.com)",
		&mockStorage{files: map[string][]byte{}},
		&errorAllowlist{},
		[]byte("test-secret"),
		nil,
	)

	err := s.Authorize(context.Background(), "https://example.com/img.png", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrAllowlistUnavailable)
	assert.NotErrorIs(t, err, ErrUnauthorized, "DB 障害を 403 に潰している")
	// **元の原因を捨てない。** 捨てるとログから障害の中身が消える。
	assert.Contains(t, err.Error(), "db connection failed")
}

// 列に入りえない URL は DB を引く前に弾く (#3036、doctrine は #3025)。
//
// **未認証の利用者が 503 とエラーログを任意に生成できてしまう。**
// `IsAllowedURL` は `?url` を無検査で 11 箇所に bind する (4 テーブルの UNION)
// ので、PostgreSQL が受け付けないバイト列はクエリごと落とす。それを
// `ErrAllowlistUnavailable` に流すと、この変更が守ろうとしている
// 「監視で本物の障害が埋もれない」を自分で壊す。
//
// 実測 (実 PostgreSQL): 0xff と孤立サロゲートは SQLSTATE 22021。NUL は
// **プロトコルで値が違う** — 本番の extended protocol では同じ 22021、
// テストハーネスの simple protocol では 08P01。
// 判定は `colfit.Storable` が NUL と不正な UTF-8 の両方を見る (以前は NUL しか
// 見ておらず、この経路だけが `utf8.ValidString` を併記していた)。
//
// **このテスト自体は PostgreSQL に触らない** — 見ているのは「引く前に弾く」
// ことだけで、上の実測は guard がなぜ要るかの根拠。
func TestAuthorize_UnstorableURLIsRejectedBeforeQuery(t *testing.T) {
	for name, raw := range map[string]string{
		// バイト列は Go のエスケープではなく []byte で組む (ソースを ASCII に保つ)。
		"NUL":            "https://example.com/a" + string([]byte{0x00}) + "b.png",
		"invalid UTF-8":  "https://example.com/a" + string([]byte{0xff}) + "b.png",
		"lone surrogate": "https://example.com/a" + string([]byte{0xed, 0xa0, 0x80}) + "b.png",
	} {
		t.Run(name, func(t *testing.T) {
			// **呼ばれたら落ちる checker を渡す。** エラーを返すだけの
			// checker だと、guard を `IsAllowedURL` の**後ろ**へ動かす変異が
			// 素通りする (レビュー 2 周目で実測)。#3025 が
			// 「guard の分岐が実際に return すること」「順序を見ること」と
			// 書いているのと同じ形。
			checker := &failingAllowlist{t: t}
			s := NewService(
				"https://example.com",
				"Misskey/2026.5.1 (https://example.com)",
				&mockStorage{files: map[string][]byte{}},
				checker,
				[]byte("test-secret"),
				nil,
			)

			err := s.Authorize(context.Background(), raw, "")
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrUnauthorized,
				"列に入りえない値を 503 に流している")
			assert.NotErrorIs(t, err, ErrAllowlistUnavailable)
			assert.False(t, checker.called, "DB を引いてしまっている")
		})
	}
}

// 署名があれば列に入りえない値でも先に通る (順序を変えていないこと)。
func TestAuthorize_HMACStillWinsOverStorableCheck(t *testing.T) {
	s := testService(nil)
	raw := "https://example.com/a" + string([]byte{0x00}) + "b.png"
	require.NoError(t, s.Authorize(context.Background(), raw, s.SignURL(raw)))
}

// 本当に許可されていないときは従来どおり ErrUnauthorized。
//
// **これが無いと「全部 503 にする」実装が緑で通る。** それをやると、
// 許可していない URL まで 5 分ごとに引き直すことになる。
func TestAuthorize_NotAllowedStaysUnauthorized(t *testing.T) {
	s := testService(map[string]bool{"https://example.com/ok.png": true})

	err := s.Authorize(context.Background(), "https://example.com/no.png", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnauthorized)
	assert.NotErrorIs(t, err, ErrAllowlistUnavailable)
}

// 利用者の離脱が DB 障害に化けないこと (#3036)。
//
// `IsAllowedURL` は ctx を取るので、離脱すると driver が
// `context.Canceled` を返す。`%w` を落とすと handler が 499 に
// 振り分けられなくなり、離脱の件数が 503 として監視に積み上がる。
func TestAuthorize_ClientCancelSurvives(t *testing.T) {
	s := NewService(
		"https://example.com",
		"Misskey/2026.5.1 (https://example.com)",
		&mockStorage{files: map[string][]byte{}},
		&ctxErrAllowlist{},
		[]byte("test-secret"),
		nil,
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.Authorize(ctx, "https://example.com/img.png", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, ErrUnauthorized)
}

// 利用者の離脱を Error ログに混ぜない (#3036)。
//
// **DB 障害の指標が汚れる。** handler は離脱を 499 に分類して何も書かないのに、
// service 側が Error を出すと、モバイル回線のスクロール離脱が
// 「allowlist lookup failed」として積み上がる。
func TestAuthorize_CancelDoesNotLogError(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	s := NewService(
		"https://example.com",
		"Misskey/2026.5.1 (https://example.com)",
		&mockStorage{files: map[string][]byte{}},
		&ctxErrAllowlist{},
		[]byte("test-secret"),
		nil,
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.Error(t, s.Authorize(ctx, "https://example.com/img.png", ""))
	assert.NotContains(t, buf.String(), "allowlist lookup failed",
		"利用者の離脱を DB 障害として記録している")

	// **DB 障害のほうは残る** (抑止しすぎていないこと)。
	buf.Reset()
	s2 := NewService(
		"https://example.com",
		"Misskey/2026.5.1 (https://example.com)",
		&mockStorage{files: map[string][]byte{}},
		&errorAllowlist{},
		[]byte("test-secret"),
		nil,
	)
	require.Error(t, s2.Authorize(context.Background(), "https://example.com/img.png", ""))
	assert.Contains(t, buf.String(), "allowlist lookup failed")
}

// failingAllowlist fails the test if the query is reached at all.
//
// **「引く前に弾く」を検査するにはこれが要る。** エラーを返すだけの
// checker では、guard を後ろへ動かしても結果の error が同じになるので
// 変異が素通りする。
type failingAllowlist struct {
	t      *testing.T
	called bool
}

func (e *failingAllowlist) IsAllowedURL(_ context.Context, url string) (bool, error) {
	e.called = true
	e.t.Errorf("IsAllowedURL が呼ばれた (引く前に弾けていない): %q", url)
	return false, nil
}

type errorAllowlist struct{}

func (e *errorAllowlist) IsAllowedURL(_ context.Context, _ string) (bool, error) {
	return false, fmt.Errorf("db connection failed")
}

// ctxErrAllowlist mirrors a driver that surfaces the caller's cancellation.
type ctxErrAllowlist struct{}

func (e *ctxErrAllowlist) IsAllowedURL(ctx context.Context, _ string) (bool, error) {
	return false, ctx.Err()
}

func TestFetch_LocalFile_TooLarge(t *testing.T) {
	// maxDownloadを超えるローカルファイル
	bigData := make([]byte, maxDownload+100)
	s := NewService(
		"https://example.com",
		"Misskey/2026.5.1 (https://example.com)",
		&mockStorage{files: map[string][]byte{"big": bigData}},
		&mockAllowlist{allowed: map[string]bool{}},
		[]byte("test-secret"),
		nil,
	)

	_, err := s.Fetch(context.Background(), "https://example.com/files/big", ModeDefault, FormatWebP, true)
	assert.ErrorIs(t, err, ErrTooLarge)
}

// sizedStorage returns bodies that report their size up front (as S3 with
// Content-Length and local files do) and records how many bytes were read.
type sizedStorage struct {
	size  int64
	read  int64
	close int
}

func (m *sizedStorage) Put(_ string, _ io.Reader) (string, error) { return "", nil }
func (m *sizedStorage) Delete(_ string) error                     { return nil }
func (m *sizedStorage) Get(_ string) (io.ReadCloser, error)       { return &sizedReadBody{s: m}, nil }

type sizedReadBody struct{ s *sizedStorage }

func (b *sizedReadBody) Size() int64 { return b.s.size }
func (b *sizedReadBody) Read(p []byte) (int, error) {
	n := len(p)
	if remaining := b.s.size - b.s.read; int64(n) > remaining {
		n = int(remaining)
	}
	if n == 0 {
		return 0, io.EOF
	}
	b.s.read += int64(n)
	return n, nil
}
func (b *sizedReadBody) Close() error { b.s.close++; return nil }

// 大きさが分かるローカルのファイルが上限を超えるなら、読む前に断る。
func TestFetch_LocalFile_KnownSizeTooLargeIsNotRead(t *testing.T) {
	st := &sizedStorage{size: maxDownload + 1}
	s := NewService("https://example.com", "ua", st, &mockAllowlist{allowed: map[string]bool{}}, []byte("test-secret"), nil)

	_, err := s.Fetch(context.Background(), "https://example.com/files/big", ModeDefault, FormatWebP, true)
	assert.ErrorIs(t, err, ErrTooLarge)
	assert.Zero(t, st.read, "the body must not be read once its size is over the limit")
	assert.Equal(t, 1, st.close)

	// 上限以下なら従来どおり読む (中身は画像ではないので結果は問わない)。
	st = &sizedStorage{size: 1024}
	s = NewService("https://example.com", "ua", st, &mockAllowlist{allowed: map[string]bool{}}, []byte("test-secret"), nil)
	_, err = s.Fetch(context.Background(), "https://example.com/files/small", ModeDefault, FormatWebP, true)
	assert.NotErrorIs(t, err, ErrTooLarge)
	assert.Equal(t, int64(1024), st.read)
}

func TestFetch_Remote_ContentLengthExceedsMax(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Content-Lengthが maxDownload を超える値を設定
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "999999999")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/huge.png": true})

	_, err := s.Fetch(context.Background(), ts.URL+"/huge.png", ModeDefault, FormatWebP, true)
	assert.ErrorIs(t, err, ErrTooLarge)
}

func TestFetch_Remote_BodyExceedsMaxNoContentLength(t *testing.T) {
	// Content-LengthなしでmaxDownloadを超えるボディを返す
	// テストではmaxDownloadの実値(32MB)を使うと遅いので、
	// 小さいサービスを作って検証する
	bigBody := make([]byte, maxDownload+100)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(bigBody)
	}))
	defer ts.Close()

	s := testService(map[string]bool{ts.URL + "/huge.png": true})

	_, err := s.Fetch(context.Background(), ts.URL+"/huge.png", ModeDefault, FormatWebP, true)
	assert.ErrorIs(t, err, ErrTooLarge)
}

// **部分的に透明な NYCbCrA を imaging に直接渡すと全画素が 0 になる (#2591)。**
//
// アルファ付きの拡張 WebP (VP8X + ALPH + VP8) を Go の webp デコーダが返す型で、
// リモート利用者のアイコンが「200 が返るのに真っ白」という壊れ方をしていた。
//
// **全面不透明なら通るので気づきにくい。** アルファを持つ画像だけが壊れる。
func TestNormalizeForResize_PartiallyTransparentNYCbCrA(t *testing.T) {
	const w, h = 260, 240

	build := func(alphaAt func(x, y int) byte) *image.NYCbCrA {
		img := image.NewNYCbCrA(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
		for y := range h {
			for x := range w {
				img.Y[img.YOffset(x, y)] = 200
				img.Cb[img.COffset(x, y)] = 100
				img.Cr[img.COffset(x, y)] = 150
				img.A[img.AOffset(x, y)] = alphaAt(x, y)
			}
		}
		return img
	}
	nonZero := func(img image.Image) int {
		b := img.Bounds()
		n := 0
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				if r, g, bl, a := img.At(x, y).RGBA(); r|g|bl|a != 0 {
					n++
				}
			}
		}
		return n
	}

	tests := []struct {
		name       string
		alphaAt    func(x, y int) byte
		wantPixels bool
	}{
		{"半分だけ不透明", func(x, _ int) byte {
			if x > w/2 {
				return 255
			}
			return 0
		}, true},
		{"全面不透明", func(int, int) byte { return 255 }, true},
		{"全面透明", func(int, int) byte { return 0 }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := build(tt.alphaAt)
			// 素の At() では中身がある = デコードは成功している。
			if tt.wantPixels {
				require.NotZero(t, nonZero(src), "元画像に中身があること")
			}

			got := nonZero(resizeToHeight(src, 60))
			if tt.wantPixels {
				assert.NotZero(t, got, "リサイズで中身が消えている (真っ白なアイコンになる)")
			} else {
				assert.Zero(t, got)
			}
		})
	}
}

// 対象を絞っていることの確認。**NRGBA 化は画素あたりのコピーを 1 回増やす**ので、
// 影響を受けない型には掛けない。
func TestNormalizeForResize_LeavesOtherTypesAlone(t *testing.T) {
	nrgba := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	assert.Same(t, nrgba, normalizeForResize(nrgba))

	ycbcr := image.NewYCbCr(image.Rect(0, 0, 4, 4), image.YCbCrSubsampleRatio420)
	assert.Same(t, ycbcr, normalizeForResize(ycbcr))

	nycbcra := image.NewNYCbCrA(image.Rect(0, 0, 4, 4), image.YCbCrSubsampleRatio420)
	converted := normalizeForResize(nycbcra)
	assert.NotSame(t, image.Image(nycbcra), converted)
	assert.IsType(t, &image.NRGBA{}, converted)
}

// **列に入らない長さの URL は allowlist を引かずに拒否する (2026-10-31 の監査)。**
// 署名の無い `?url` は 11 箇所に bind されるので、長さを見ないと未認証の
// 1 リクエストで約 1MiB × 11 のパラメータを PostgreSQL へ渡せた。
func TestAuthorize_NoSig_TooLongURLIsUnauthorizedWithoutLookup(t *testing.T) {
	long := "https://remote.example/" + strings.Repeat("a", MaxAllowlistURLChars)
	s := testService(map[string]bool{long: true})
	m := s.allowlist.(*mockAllowlist)

	err := s.Authorize(context.Background(), long, "")
	assert.ErrorIs(t, err, ErrUnauthorized)
	assert.Zero(t, m.lookups, "列に入らない長さの URL で allowlist を引いている")

	// 上限ちょうどは引く。
	exact := "https://remote.example/" + strings.Repeat("a", MaxAllowlistURLChars-len("https://remote.example/"))
	require.Len(t, exact, MaxAllowlistURLChars)
	s = testService(map[string]bool{exact: true})
	require.NoError(t, s.Authorize(context.Background(), exact, ""))
	assert.Equal(t, 1, s.allowlist.(*mockAllowlist).lookups)

	// varchar(n) は文字数なので、多バイトの文字で上限ちょうどの URL (バイト数は
	// 上限の約 3 倍) も引く (レビュー L-1)。
	wide := "https://remote.example/" + strings.Repeat("あ", MaxAllowlistURLChars-len("https://remote.example/"))
	require.Greater(t, len(wide), MaxAllowlistURLChars)
	s = testService(map[string]bool{wide: true})
	require.NoError(t, s.Authorize(context.Background(), wide, ""))
	assert.Equal(t, 1, s.allowlist.(*mockAllowlist).lookups)
}
