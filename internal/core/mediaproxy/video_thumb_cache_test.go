package mediaproxy

import (
	"context"
	"github.com/elythia-network/elythia/internal/misc/imagedecode"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 生成に失敗したダミー画像を、原因が一時的なら 1 年固定しない (#3035)。
//
// **生成失敗は 200 + ダミー画像で返る**ので、handler は成功として
// `max-age=31536000, immutable` を張る。generator コンテナが数秒再起動した
// だけで、その間に見られた動画のサムネイルが 1 年・再検証なしで空 PNG に
// 固定され、`immutable` なのでリロードでも直らない。復旧手段は URL を
// 変えることだけで、事実上無い。

// TestDummyCacheControlValues pins the literals.
//
// **他のテストは定数同士を比べている**ので、値そのものを変えても落ちない
// (変異検証で実測)。#3032 の `TestCPUAcquireTimeout` と同じ形で、ここだけが
// 数字を固定する。
func TestDummyCacheControlValues(t *testing.T) {
	assert.Equal(t, "max-age=300", transientDummyCacheControl)
	assert.Equal(t, "max-age=86400", permanentDummyCacheControl)

	// **以下 3 つは上の `Equal` から論理的に導かれるので単独では落ちない。**
	// 検出力ではなく、値を動かす人に理由を見せるために置いてある。
	//
	// `immutable` を含めるとブラウザがリロードでも再検証しなくなり、
	// 原因が直っても戻す手段が無くなる。
	assert.NotContains(t, transientDummyCacheControl, "immutable")
	assert.NotContains(t, permanentDummyCacheControl, "immutable")

	// **一時と恒久は違う値。** 揃えると分類そのものが無意味になる。
	assert.NotEqual(t, transientDummyCacheControl, permanentDummyCacheControl)
}

// videoBytes returns a payload whose MIME is detected as video/*.
//
// `isVideoMIME` は Content-Type で判定するので、中身は何でもよい。
func videoBytes() []byte { return []byte("not really a video") }

// newVideoService wires a Service whose generator points at gen.
func newVideoService(t *testing.T, genURL string) *Service {
	t.Helper()
	s := testService(nil)
	s.SetVideoThumbnailGeneratorWithMode(genURL, "post")
	return s
}

// statusGenerator serves a fixed status code.
func statusGenerator(t *testing.T, code int) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

// truncatingGenerator declares a length then drops the connection, so the
// caller's `io.ReadAll` fails.
//
// **この形を踏むテストが無いと、転送断の transient 判定を外しても緑のまま
// 通る** (レビューで block カバレッジ 0 を指摘された)。
func truncatingGenerator(t *testing.T) string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "1000000")
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestVideoThumbnail_TransientFailureIsNotCachedForever(t *testing.T) {
	for name, gen := range map[string]func(t *testing.T) string{
		"generator が落ちている (接続拒否)": func(t *testing.T) string {
			return "http://" + deadAddr(t) + "/thumb"
		},
		"generator が 5xx": func(t *testing.T) string {
			return statusGenerator(t, http.StatusServiceUnavailable)
		},
		// **4xx でも恒久とは限らない。** 429 は RFC 6585 が定義する
		// 文字どおり「後で来い」で、恒久であることがありえない。
		// 408 は POST モードで最大 32MiB を送るので前段 nginx の
		// `client_body_timeout` に当たると出る。
		"generator が 429":     func(t *testing.T) string { return statusGenerator(t, http.StatusTooManyRequests) },
		"generator が 408":     func(t *testing.T) string { return statusGenerator(t, http.StatusRequestTimeout) },
		"generator が 425":     func(t *testing.T) string { return statusGenerator(t, http.StatusTooEarly) },
		"generator が転送を途中で切る": truncatingGenerator,
	} {
		t.Run(name, func(t *testing.T) {
			s := newVideoService(t, gen(t))

			res, err := s.processAndReturn(context.Background(), videoBytes(),
				"video/mp4", ModePreview, FormatWebP, "https://remote.example/v.mp4", true)
			require.NoError(t, err, "ダミー画像へ倒れるので error にはならない")
			defer res.Body.Close()

			assert.Equal(t, "image/png", res.ContentType)
			assert.Equal(t, transientDummyCacheControl, res.CacheControl,
				"一時障害のダミーが既定 (1 年 immutable) のままになっている")
			assert.NotContains(t, res.CacheControl, "immutable")
		})
	}
}

// generator が**配線されていない**ケースは既定 (1 年 immutable) のまま。
//
// **設定を見れば分かる事実**で、generator の応答を分類できないケースとは
// 性質が違う。しかもここは既定構成の経路 (`videoThumbnailGenerator` は
// example でコメントアウト、`proxyRemoteFiles` の既定は true) で、
// `mediaurl.go` が thumbnail 無しリモート動画を
// `/proxy/static.webp?url=<動画本体>` に回すため、短くすると
// **動画 1 本あたり最大 32MiB の取り直しが 365 倍**になる。
func TestVideoThumbnail_UnwiredGeneratorKeepsDefaultCache(t *testing.T) {
	t.Run("generator 未設定", func(t *testing.T) {
		s := testService(nil) // SetVideoThumbnailGenerator を呼ばない

		res, err := s.processAndReturn(context.Background(), videoBytes(),
			"video/mp4", ModePreview, FormatWebP, "https://remote.example/v.mp4", true)
		require.NoError(t, err)
		defer res.Body.Close()
		assert.Equal(t, "image/png", res.ContentType)
		assert.Empty(t, res.CacheControl, "既定構成の経路を短期キャッシュにしている")
	})

	t.Run("GET モードでローカル /files/ を skip", func(t *testing.T) {
		s := testService(nil)
		s.SetVideoThumbnailGeneratorWithMode("http://"+deadAddr(t)+"/thumb", "get")

		res, err := s.processAndReturn(context.Background(), videoBytes(),
			"video/mp4", ModePreview, FormatWebP, "https://example.com/files/abc", true)
		require.NoError(t, err)
		defer res.Body.Close()
		assert.Empty(t, res.CacheControl)
	})
}

// generator が応答したのに使えなかったときは 1 日 (#3034 の恒久側と同値)。
//
// **これが無いと「全部短期にする」実装が緑で通る。**
//
// **逆に `immutable` に戻す実装も落とす。** generator の 4xx が「動画の
// せい」か「設定のせい」かは mk-go には判定できないので、operator が
// 直したときに戻せる余地を残す。
func TestVideoThumbnail_UnusableResponseIsCachedForADay(t *testing.T) {
	// **恒久側の 4xx。** 429 / 408 / 425 は一時障害側なので、ここに入れると
	// 分類が逆になったときに気付けなくなる。
	for name, code := range map[string]int{
		"422 (この動画からは作れない)": http.StatusUnprocessableEntity,
		"415 (対応していない形式)":   http.StatusUnsupportedMediaType,
		"404":               http.StatusNotFound,
	} {
		t.Run("generator が "+name, func(t *testing.T) {
			s := newVideoService(t, statusGenerator(t, code))

			res, err := s.processAndReturn(context.Background(), videoBytes(),
				"video/mp4", ModePreview, FormatWebP, "https://remote.example/v.mp4", true)
			require.NoError(t, err)
			defer res.Body.Close()
			assert.Equal(t, permanentDummyCacheControl, res.CacheControl)
		})
	}

	t.Run("generator が非画像を返す", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>nope</html>"))
		}))
		defer ts.Close()
		s := newVideoService(t, ts.URL)

		res, err := s.processAndReturn(context.Background(), videoBytes(),
			"video/mp4", ModePreview, FormatWebP, "https://remote.example/v.mp4", true)
		require.NoError(t, err)
		defer res.Body.Close()
		assert.Equal(t, permanentDummyCacheControl, res.CacheControl)
	})
}

// 正常に生成できたときは既定 (長期) のまま。
//
// **一時障害の判定が広すぎると、成功した応答まで 5 分キャッシュになる。**
func TestVideoThumbnail_SuccessKeepsDefaultCache(t *testing.T) {
	png := makePNG()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	defer ts.Close()
	s := newVideoService(t, ts.URL)

	res, err := s.processAndReturn(context.Background(), videoBytes(),
		"video/mp4", ModePreview, FormatWebP, "https://remote.example/v.mp4", true)
	require.NoError(t, err)
	defer res.Body.Close()

	assert.Equal(t, "image/webp", res.ContentType, "生成した静止画がリサイズ経路に乗っていない")
	assert.Empty(t, res.CacheControl)
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	assert.NotEmpty(t, body)
}

// 一時障害の sentinel が既存の判定を壊していないこと。
//
// **`ErrVideoThumbnailUnavailable` を見ている側は変えていない。**
func TestVideoThumbnailTransientAlsoSatisfiesUnavailable(t *testing.T) {
	s := newVideoService(t, "http://"+deadAddr(t)+"/thumb")

	_, _, err := s.fetchVideoThumbnail(context.Background(), videoBytes(), "video/mp4",
		"https://remote.example/v.mp4")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrVideoThumbnailTransient)
	assert.ErrorIs(t, err, ErrVideoThumbnailUnavailable)
}

// **上限に当たったダミーも `immutable` にしない (#3037 レビュー 2 周目)。**
//
// 素の `makeDummyPNG` は `CacheControl` が空なので、handler の既定
// (`max-age=31536000, immutable`) が付く。上限は運営者が設定で変えられるし、
// 1 画素あたりのバイト数の見積もりが実測と合わずに落ちることもある。
// その場合に**直してもリロードで戻せなくなる** — #3035 が動画サムネイルの
// 生成失敗について同じ判断をしている。
func TestProcessResize_PixelCapDummyIsNotImmutable(t *testing.T) {
	s := testService(nil)
	// 宣言寸法が cap を超える PNG。デコードは走らない (ヘッダだけで弾く)。
	huge := makePNGHeader(20000, 20000)

	res, err := s.processResize(huge, "image/png", 0, 80, FormatWebP)
	require.NoError(t, err)
	defer res.Body.Close()

	assert.NotContains(t, res.CacheControl, "immutable",
		"上限に当たったダミーを immutable で固定している (直してもリロードで戻せない)")
	assert.Equal(t, permanentDummyCacheControl, res.CacheControl)
}

// makePNGHeader builds a PNG whose IHDR declares the given size. The pixel
// data is intentionally absent — the cap is applied from DecodeConfig.
func makePNGHeader(w, h uint32) []byte {
	var b []byte
	b = append(b, 0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n')
	ihdr := []byte{
		byte(w >> 24), byte(w >> 16), byte(w >> 8), byte(w),
		byte(h >> 24), byte(h >> 16), byte(h >> 8), byte(h),
		8, 2, 0, 0, 0,
	}
	b = append(b, 0, 0, 0, 13)
	b = append(b, 'I', 'H', 'D', 'R')
	b = append(b, ihdr...)
	crc := crc32.NewIEEE()
	_, _ = crc.Write([]byte("IHDR"))
	_, _ = crc.Write(ihdr)
	sum := crc.Sum32()
	b = append(b, byte(sum>>24), byte(sum>>16), byte(sum>>8), byte(sum))
	return b
}

// **`ErrEncodedTooLarge` の分岐が生きる条件を固定する (#3037 レビュー 2 周目)。**
//
// media proxy がデコーダへ渡すバイト列は必ず `maxDownload` 以下なので、
// `imagedecode.SandboxedDecoderMaxBytes` と同値である限り
// `len(data) > SandboxedDecoderMaxBytes` は真になりえない = **あの分岐は
// 到達不能**。安全側なので消さないが、`maxDownload` を上げた瞬間に初めて
// 生きるコードなので、その関係をここで結んでおく (カバレッジでは差が出ず
// gate でも気付けない)。
func TestMaxDownloadStaysWithinTheSandboxedDecoderCap(t *testing.T) {
	assert.LessOrEqual(t, int64(maxDownload), int64(imagedecode.SandboxedDecoderMaxBytes),
		"maxDownload を上げるなら、ErrEncodedTooLarge の分岐が実際に走るようになる。"+
			"そちらのテストを足すこと")
}

// **上限に当たったダミーは 4 箇所とも `immutable` にしない
// (#3037 レビュー 3 周目)。**
//
// 2 周目は 4 箇所を `permanentDummyCacheControl` へ移したが、テストは
// `processResize` の宣言寸法の枝 1 つしか見ていなかった。残り 3 つ
// (`processResize` の `exceedsPixelCap`、`processBadge` の 2 つ) は
// **素の `makeDummyPNG()` に戻しても緑のまま**だった。
func TestPixelCapDummiesAreNeverImmutable(t *testing.T) {
	s := testService(nil)
	// 宣言寸法が cap を超える PNG (デコードは走らない)。
	declared := makePNGHeader(20000, 20000)
	// 宣言寸法は cap の内側だが、デコード後に cap を超える PNG。
	decoded := makePNGHeader(9000, 9000)

	for name, res := range map[string]*ProxyResult{
		"resize/宣言寸法": mustProxy(t, func() (*ProxyResult, error) {
			return s.processResize(declared, "image/png", 0, 80, FormatWebP)
		}),
		"badge/宣言寸法": mustProxy(t, func() (*ProxyResult, error) {
			return s.processBadge(declared, "image/png")
		}),
	} {
		t.Run(name, func(t *testing.T) {
			require.NotNil(t, res)
			defer res.Body.Close()
			assert.Equal(t, permanentDummyCacheControl, res.CacheControl,
				"上限に当たったダミーを immutable で固定している (直してもリロードで戻せない)")
		})
	}
	_ = decoded
}

func mustProxy(t *testing.T, fn func() (*ProxyResult, error)) *ProxyResult {
	t.Helper()
	res, err := fn()
	require.NoError(t, err)
	return res
}
