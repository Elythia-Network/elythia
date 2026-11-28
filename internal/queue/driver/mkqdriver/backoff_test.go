package mkqdriver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// httpRelatedBackoff が Misskey TS の式 (2^n-1)*60s を jitter=0 で再現する
// ことを確認する。rng を 0 固定にして jitter 項を消す (#1406, mkq#67)。
func TestHTTPRelatedBackoff_Formula(t *testing.T) {
	zero := func() float64 { return 0 }
	cases := []struct {
		attemptsMade int
		want         time.Duration
	}{
		{1, time.Minute},      // (2^1-1)*60s = 60s
		{2, 3 * time.Minute},  // (2^2-1)*60s = 180s
		{3, 7 * time.Minute},  // (2^3-1)*60s = 420s
		{4, 15 * time.Minute}, // (2^4-1)*60s = 900s
	}
	for _, c := range cases {
		got := httpRelatedBackoff(c.attemptsMade, zero)
		assert.Equal(t, c.want, got, "attemptsMade=%d", c.attemptsMade)
	}
}

// 大きい attemptsMade では 8h cap に丸められる (jitter=0)。
func TestHTTPRelatedBackoff_Cap(t *testing.T) {
	zero := func() float64 { return 0 }
	// 2^20 * 60s は 8h を遥かに超えるので cap される。
	got := httpRelatedBackoff(20, zero)
	assert.Equal(t, httpRelatedBackoffCap, got)
}

// jitter は base に対して最大 +20% 上乗せされる。rng=1 で上限を確認する。
func TestHTTPRelatedBackoff_JitterUpperBound(t *testing.T) {
	one := func() float64 { return 1 }
	// attemptsMade=1: base=60s, +20% = 72s
	got := httpRelatedBackoff(1, one)
	assert.Equal(t, 72*time.Second, got)
}

// jitter=0..1 の範囲で結果が [base, base*1.2] に収まる。
func TestHTTPRelatedBackoff_JitterRange(t *testing.T) {
	base := 3 * time.Minute // attemptsMade=2
	lo := httpRelatedBackoff(2, func() float64 { return 0 })
	hi := httpRelatedBackoff(2, func() float64 { return 1 })
	assert.Equal(t, base, lo)
	assert.Equal(t, base+time.Duration(float64(base)*0.2), hi)
	assert.True(t, hi > lo)
}

// FederationMaxRetryDelay is the delay before the last try with the jitter at
// its upper bound, and stops growing at the cap.
func TestFederationMaxRetryDelay(t *testing.T) {
	// 既定の 8 回では最後の待ち (7 回目の失敗の後) が 127 分、jitter の上限で
	// 152 分 24 秒。
	assert.Equal(t, 152*time.Minute+24*time.Second, FederationMaxRetryDelay(8))
	assert.Equal(t, 72*time.Second, FederationMaxRetryDelay(2))
	for _, n := range []int{-1, 0, 1} {
		assert.Zero(t, FederationMaxRetryDelay(n), "maxAttempts=%d has no retry", n)
	}
	// 上限 8 時間に jitter の上限を足した 9 時間 36 分で止まる。
	for _, n := range []int{11, 12, 20, 1000, math.MaxInt} {
		assert.Equal(t, 9*time.Hour+36*time.Minute, FederationMaxRetryDelay(n), "maxAttempts=%d", n)
	}
}

// Every delay the workers' strategy can pick for a job with maxAttempts tries
// is within FederationMaxRetryDelay(maxAttempts).
func TestFederationBackoffStrategyWithinMaxRetryDelay(t *testing.T) {
	for maxAttempts := 2; maxAttempts <= 14; maxAttempts++ {
		limit := FederationMaxRetryDelay(maxAttempts)
		for attemptsMade := 1; attemptsMade < maxAttempts; attemptsMade++ {
			for range 200 {
				d := federationBackoffStrategy(attemptsMade)
				require.LessOrEqual(t, d, limit, "maxAttempts=%d attemptsMade=%d", maxAttempts, attemptsMade)
				require.GreaterOrEqual(t, d, httpRelatedBackoff(attemptsMade, func() float64 { return 0 }))
			}
		}
	}
}

// The workers register federationBackoffStrategy, the function
// FederationMaxRetryDelay is checked against.
func TestWorkersRegisterFederationBackoffStrategy(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "server.go", nil, 0)
	require.NoError(t, err)
	var args []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "WithBackoffStrategy" {
			return true
		}
		for _, a := range call.Args {
			if id, ok := a.(*ast.Ident); ok {
				args = append(args, id.Name)
			} else {
				args = append(args, "<not an identifier>")
			}
		}
		return true
	})
	assert.Equal(t, []string{"federationBackoffStrategy"}, args)
}
