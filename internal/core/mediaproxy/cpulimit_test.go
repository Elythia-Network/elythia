package mediaproxy

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withShortCPUAcquireTimeout shortens the saturation wait for the duration of
// a test so a genuinely exhausted pool can be observed without sleeping for
// the production value.
//
// **`cpuAcquireTimeout` が var なのはこのため。** #2849 の argon2 枠と同じ形。
func withShortCPUAcquireTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	orig := cpuAcquireTimeout
	cpuAcquireTimeout = d
	t.Cleanup(func() { cpuAcquireTimeout = orig })
}

// TestProxyModeStringIsHumanReadable fixes the log-facing names.
//
// **無いと shed のログが `"mode":2` になる。** operator に手掛かりを
// 残すのが `slog.Warn` の目的なので、数字だけでは意味が無い
// (#2849 が `Scheme.String()` で直したのと同型)。
func TestProxyModeStringIsHumanReadable(t *testing.T) {
	for mode, want := range map[ProxyMode]string{
		ModeDefault: "default",
		ModeEmoji:   "emoji",
		ModeAvatar:  "avatar",
		ModeStatic:  "static",
		ModePreview: "preview",
		ModeBadge:   "badge",
	} {
		assert.Equal(t, want, mode.String())
	}
	// 未知の値でも数字だけにはしない。
	assert.Equal(t, "ProxyMode(99)", ProxyMode(99).String())
}

// TestCPUAcquireTimeout pins the production wait.
//
// **値そのものをテストで固定する。** ここを短くすると、枠が守ろうとして
// いるまさにその輻輳で正当な要求を落とすようになり、しかも症状は「たまに
// 画像が出ない」なので原因に辿り着きにくい (#2849 が実際に踏んだ形)。
// 逆に長くすると、枠待ちの goroutine が入力バッファを抱えたまま積み上がり、
// 枠で抑えたはずのピークメモリが行列側に戻る。
func TestCPUAcquireTimeout(t *testing.T) {
	assert.Equal(t, 5*time.Second, cpuAcquireTimeout)
}

// TestDefaultCPUConcurrencyIsHalfOfGOMAXPROCS fixes the default.
//
// **GOMAXPROCS 全部にしてはいけない** — 実測で無制限と peak RSS が区別できず
// (995MB vs 959MB)、枠を持つ意味が無くなる。1 まで絞るのも駄目で、
// スループットが半分以下 (30.7 rps vs 71.6) になる。
func TestDefaultCPUConcurrencyIsHalfOfGOMAXPROCS(t *testing.T) {
	want := runtime.GOMAXPROCS(0) / 2
	if want < 1 {
		want = 1
	}
	assert.Equal(t, want, defaultCPUConcurrency())
	assert.GreaterOrEqual(t, defaultCPUConcurrency(), 1, "枠が 0 だと画像処理が一切通らない")
	assert.Less(t, defaultCPUConcurrency(), runtime.GOMAXPROCS(0)+1)
}

// TestNewServiceInstallsCPULimit asserts that a Service built through the
// normal constructor is limited, not just one that called SetCPUConcurrency.
//
// **既定が無制限だと、明示的に設定していない構成が全部無防備になる。**
func TestNewServiceInstallsCPULimit(t *testing.T) {
	s := testService(nil)
	require.NotNil(t, s.cpuSlots, "NewService が枠を張っていない")
	assert.Equal(t, defaultCPUConcurrency(), s.cpuConcurrency)
}

// TestSetCPUConcurrencyRestoresDefaultForNonPositive covers the config path
// where 0 means "unset".
func TestSetCPUConcurrencyRestoresDefaultForNonPositive(t *testing.T) {
	for _, n := range []int{0, -1, -1000} {
		s := testService(nil)
		s.SetCPUConcurrency(n)
		assert.Equal(t, defaultCPUConcurrency(), s.cpuConcurrency, "n=%d", n)
		require.NotNil(t, s.cpuSlots)
	}
}

// TestAcquireCPUNeverExceedsLimit is the core invariant: no more goroutines
// hold the pipeline than the configured cap, whatever the interleaving.
func TestAcquireCPUNeverExceedsLimit(t *testing.T) {
	const limit = 2
	const workers = 16

	s := testService(nil)
	s.SetCPUConcurrency(limit)

	var inFlight, maxSeen int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := s.acquireCPU(context.Background())
			if err != nil {
				return
			}
			defer release()
			cur := atomic.AddInt64(&inFlight, 1)
			for {
				old := atomic.LoadInt64(&maxSeen)
				if cur <= old || atomic.CompareAndSwapInt64(&maxSeen, old, cur) {
					break
				}
			}
			// 枠を握ったまま少し留まらないと、実装が壊れていても
			// 重なりが観測できない。
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt64(&inFlight, -1)
		}()
	}
	wg.Wait()

	assert.LessOrEqual(t, atomic.LoadInt64(&maxSeen), int64(limit),
		"同時に画像処理へ入った本数が上限を超えた")
	assert.Greater(t, atomic.LoadInt64(&maxSeen), int64(0), "誰も枠を取れていない")
}

// TestAcquireCPUShedsWhenSaturated asserts the pool sheds instead of queueing
// forever once the wait is exceeded.
func TestAcquireCPUShedsWhenSaturated(t *testing.T) {
	withShortCPUAcquireTimeout(t, 20*time.Millisecond)

	s := testService(nil)
	s.SetCPUConcurrency(1)

	release, err := s.acquireCPU(context.Background())
	require.NoError(t, err)
	defer release()

	_, err = s.acquireCPU(context.Background())
	require.ErrorIs(t, err, ErrOverloaded)
}

// TestAcquireCPUDistinguishesClientCancel asserts that a caller-side cancel is
// not reported as overload.
//
// **混ぜると監視で嘘をつく。** 利用者が接続を切っただけの件数が 503 に
// 積み上がると、サーバーが過負荷だと読めてしまう。
func TestAcquireCPUDistinguishesClientCancel(t *testing.T) {
	withShortCPUAcquireTimeout(t, time.Second)

	s := testService(nil)
	s.SetCPUConcurrency(1)

	release, err := s.acquireCPU(context.Background())
	require.NoError(t, err)
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.acquireCPU(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, ErrOverloaded)
}

// TestAcquireCPUReleaseIsIdempotent guards against a double Release panicking
// the process.
//
// `semaphore.Weighted.Release` は容量を超えて返すと panic する。defer と
// 明示呼び出しが両方走る書き方はいずれ出るので、release 側で吸収する。
func TestAcquireCPUReleaseIsIdempotent(t *testing.T) {
	s := testService(nil)
	s.SetCPUConcurrency(1)

	release, err := s.acquireCPU(context.Background())
	require.NoError(t, err)
	require.NotPanics(t, func() {
		release()
		release()
	})

	// 枠が壊れていないこと (2 回目の Release が容量を水増ししていない)。
	r2, err := s.acquireCPU(context.Background())
	require.NoError(t, err)
	defer r2()
	withShortCPUAcquireTimeout(t, 20*time.Millisecond)
	_, err = s.acquireCPU(context.Background())
	require.ErrorIs(t, err, ErrOverloaded)
}

// TestAcquireCPUReleaseDoesNotTouchAReplacedPool asserts the release path
// returns the permit to the semaphore it took it from.
//
// **`SetCPUConcurrency` は起動時専用** (goroutine-safe ではない。詳細は
// そちらの GoDoc) なので、この並びは production では起きない。それでも
// 固定するのは、破ったときの代償が**プロセスのクラッシュ**だから —
// 解放時に `s.cpuSlots` を読み直す実装だと、acquire したのとは別の
// `semaphore.Weighted` へ Release が入り
// `semaphore: released more than held` で panic する。
//
// **このテストは逐次呼び出しで、並行の張り替えは踏まない** (そちらは
// データ競合であり、防いでいるのは panic だけ)。
func TestAcquireCPUReleaseDoesNotTouchAReplacedPool(t *testing.T) {
	s := testService(nil)
	s.SetCPUConcurrency(1)

	release, err := s.acquireCPU(context.Background())
	require.NoError(t, err)

	// 枠を張り替える (起動時専用の API なので、これは契約違反の再現)。
	s.SetCPUConcurrency(2)

	require.NotPanics(t, release, "差し替え後の Release が panic した")

	// 新しい枠が水増しされていないこと (容量 2 のまま)。
	withShortCPUAcquireTimeout(t, 20*time.Millisecond)
	r1, err := s.acquireCPU(context.Background())
	require.NoError(t, err)
	defer r1()
	r2, err := s.acquireCPU(context.Background())
	require.NoError(t, err)
	defer r2()
	_, err = s.acquireCPU(context.Background())
	require.ErrorIs(t, err, ErrOverloaded, "張り替え後の枠の容量が増えている")
}

// --- 配線 (Fetch から実際に枠を取っているか) ---

// makeLargePNG builds a PNG whose decode+resize+encode is slow enough that a
// held slot is observable.
//
// ノイズを入れるのは、単色だと PNG が極端に小さくなって decode が速すぎる
// ため (それだと上のテストが空虚になる)。
//
// **`BestSpeed` で書くのは fixture の生成が本体より重かったから** —
// 2000x2000 を既定圧縮で作ると `png.Encode` だけで 8.6 秒かかり、
// `-race` のテスト時間の大半を占めていた (実測 38.8s → 26.8s の差)。
// 800x800 + BestSpeed なら生成 0.6 秒で済む。**以下はすべて `-race` 時の
// 実測** — decode 単体で 313ms、`processResize` 全体で 2.16s かかるので、
// 待ち上限 5ms に対する余裕は 60 倍以上。`-race` 無しでも decode 18.6ms /
// 全体 219ms で、パイプライン全体なら 44 倍あり判定は変わらない
// (`-race -count=10` / `-count=10` / `GOMAXPROCS=1,2` のいずれでも
// shed=7 で一定)。
func makeLargePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(12345)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			seed = seed*1664525 + 1013904223
			n := uint8(seed >> 24)
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: n, A: 255})
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	require.NoError(t, enc.Encode(&buf, img))
	return buf.Bytes()
}

// newImageOrigin serves a PNG so Fetch reaches the image pipeline.
func newImageOrigin(t *testing.T) *httptest.Server {
	t.Helper()
	png := makePNG()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestFetchResizeModesTakeACPUSlot asserts the limiter is actually on the
// resize path, for every mode that decodes.
//
// **「枠を持っている」ことと「その枠を使っている」ことは別。** acquireCPU の
// 単体テストだけだと、processAndReturn から呼ぶのを外しても緑のまま通る。
func TestFetchResizeModesTakeACPUSlot(t *testing.T) {
	withShortCPUAcquireTimeout(t, 20*time.Millisecond)

	origin := newImageOrigin(t)
	for _, mode := range []ProxyMode{ModeEmoji, ModeAvatar, ModeStatic, ModePreview, ModeBadge} {
		s := testService(map[string]bool{origin.URL: true})
		s.SetCPUConcurrency(1)

		// テスト側で唯一の枠を握ったままにする。
		release, err := s.acquireCPU(context.Background())
		require.NoError(t, err)

		_, err = s.Fetch(context.Background(), origin.URL, mode, FormatWebP, true)
		release()
		require.ErrorIs(t, err, ErrOverloaded, "mode=%d が枠を取っていない", mode)
	}
}

// TestFetchHoldsTheSlotForTheWholePipeline is the invariant this whole change
// exists for: the slot must be held **while the work runs**, not merely taken
// and handed back.
//
// **これが無いと `defer release()` を `release()` に変えるだけで全テストが
// 緑のまま通る (レビューで実測)。** その形は acquire こそするが仕事の前に
// 返すので同時デコード数を一切縛らず、peak RSS は無制限時に戻る。
// docs/gates.md の 2026-09-17 の経緯が blocker として挙げている「`storableIDs(ids)` と
// 書いて戻り値を捨てる」と同型。
//
// 判定は「枠 1 で N 本同時に投げたとき、待ちきれずに shed されるものが
// 出るか」。仕事の間保持していれば必ず出るし、即返していれば 1 件も出ない。
func TestFetchHoldsTheSlotForTheWholePipeline(t *testing.T) {
	// デコードが待ち上限より確実に長くなる大きさにする。小さい画像だと
	// 正しい実装でも shed が出ず、テストが空虚になる。
	withShortCPUAcquireTimeout(t, 5*time.Millisecond)

	big := makeLargePNG(t, 800, 800)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(big)
	}))
	defer origin.Close()

	s := testService(map[string]bool{origin.URL: true})
	s.SetCPUConcurrency(1)

	const workers = 8
	var shed, ok int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := s.Fetch(context.Background(), origin.URL, ModeAvatar, FormatWebP, true)
			switch {
			case err == nil:
				res.Body.Close()
				atomic.AddInt64(&ok, 1)
			case errors.Is(err, ErrOverloaded):
				atomic.AddInt64(&shed, 1)
			}
		}()
	}
	close(start)
	wg.Wait()

	t.Logf("shed=%d ok=%d / %d", shed, ok, workers)
	assert.Greater(t, atomic.LoadInt64(&shed), int64(0),
		"1 本も shed されていない = 枠がデコード中に保持されていない")
	assert.Greater(t, atomic.LoadInt64(&ok), int64(0),
		"1 本も通っていない = 枠そのものが壊れている")
}

// TestFetchPassThroughDoesNotTakeACPUSlot is the other half of the pair: the
// cheap path must stay usable while the pipeline is saturated.
//
// **これが無いと「Fetch 全体を枠で囲む」実装が緑で通る。** それをやると
// リモート fetch の待ちで枠を占有し、同じ枠数でスループットだけが落ちる。
func TestFetchPassThroughDoesNotTakeACPUSlot(t *testing.T) {
	withShortCPUAcquireTimeout(t, 20*time.Millisecond)

	origin := newImageOrigin(t)
	s := testService(map[string]bool{origin.URL: true})
	s.SetCPUConcurrency(1)

	release, err := s.acquireCPU(context.Background())
	require.NoError(t, err)
	defer release()

	res, err := s.Fetch(context.Background(), origin.URL, ModeDefault, FormatWebP, true)
	require.NoError(t, err, "pass-through が画像処理の枠を待たされている")
	defer res.Body.Close()
	assert.Equal(t, "image/png", res.ContentType)
}
