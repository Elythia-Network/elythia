package middleware

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/role"
	"github.com/shiroha-a/mk/internal/safemath"
)

// EndpointLimit defines rate limit parameters for an API endpoint.
type EndpointLimit struct {
	Duration    time.Duration // ウィンドウ幅（例: 1時間）
	Max         int           // ウィンドウ内最大リクエスト数
	MinInterval time.Duration // 連続リクエスト最小間隔（0なら無効）
	// RejectResponse overrides the default RATE_LIMIT_EXCEEDED body returned on
	// 429 for this endpoint. nil なら汎用 RateLimitExceeded。signin 系は
	// TOO_MANY_AUTHENTICATION_FAILURES を返すために使う (#1829)。
	RejectResponse func() map[string]any
	// UserBucketOnly drops the IP bucket for this endpoint (#3106).
	//
	// **未認証のリクエストで管理者を締め出せる経路を作らないため。** limiter は
	// route の権限検査より前に走るので、403 になるリクエストでも bucket を消費する。
	// 認証済みの利用者は user と IP の**両方**を消費するので、同じ出口 IP
	// (CGNAT・社内 NAT・`trustProxy` の誤設定) から未認証で叩き続けられると、
	// **正当なモデレーターの照会が窓のあいだ 429 になる**。
	//
	// IP 照会の上限が守りたいのは「認証済みの利用者による濫用」で、そちらは
	// user bucket が押さえる。未認証は権限検査が 403 で落として何も開示しないので、
	// IP bucket はこの endpoint 群では**守るものが無く、締め出しだけを生む**。
	//
	// 既定 (false) では今までどおり両方を見る — auth 系のように**未認証の試行
	// そのもの**を抑えたい endpoint では IP bucket が本体なので、一律には外さない。
	UserBucketOnly bool
}

// LimitInfo holds the result of a rate limit check.
type LimitInfo struct {
	Remaining int   // 残りリクエスト数
	ResetMs   int64 // リセット時刻 (Unix milliseconds)
}

// RateLimitStore abstracts the backing store for rate limit counters.
type RateLimitStore interface {
	// Check records a request and returns the current limit status.
	// key is the rate limit bucket identifier, duration is the window
	// size, and max is the maximum number of requests allowed.
	Check(ctx context.Context, key string, duration time.Duration, max int) (LimitInfo, error)
}

// RedisRateLimitStore implements RateLimitStore using Redis sorted sets.
// The algorithm mirrors the TS ratelimiter (visionmedia/node-ratelimiter):
// each request is a ZADD with a microsecond timestamp as both score and
// member, old entries are pruned with ZREMRANGEBYSCORE, and ZCARD gives
// the current count within the sliding window.
type RedisRateLimitStore struct {
	rdb *redis.Client
}

// NewRedisRateLimitStore creates a store backed by the given Redis client.
func NewRedisRateLimitStore(rdb *redis.Client) *RedisRateLimitStore {
	return &RedisRateLimitStore{rdb: rdb}
}

// Check implements RateLimitStore using a sliding-window sorted set.
func (s *RedisRateLimitStore) Check(ctx context.Context, key string, duration time.Duration, max int) (LimitInfo, error) {
	now := time.Now().UnixMicro()
	windowStart := now - duration.Microseconds()
	rkey := "limit:" + key

	pipe := s.rdb.Pipeline()
	pipe.ZRemRangeByScore(ctx, rkey, "0", fmt.Sprintf("%d", windowStart))
	zcardCmd := pipe.ZCard(ctx, rkey)
	pipe.ZAdd(ctx, rkey, redis.Z{Score: float64(now), Member: now})
	zrangeOldestCmd := pipe.ZRange(ctx, rkey, 0, 0)
	zrangeAtMaxCmd := pipe.ZRange(ctx, rkey, int64(-max), int64(-max))
	pipe.ZRemRangeByRank(ctx, rkey, 0, int64(-(max + 1)))
	pipe.PExpire(ctx, rkey, duration)

	_, err := pipe.Exec(ctx)
	if err != nil {
		return LimitInfo{}, fmt.Errorf("rate limit redis pipeline: %w", err)
	}

	count := int(zcardCmd.Val())
	remaining := 0
	if count < max {
		remaining = max - count
	}

	// リセット時刻の計算: TS版と同じロジック
	// -max位置のエントリがあればそのタイムスタンプ、なければ最古のエントリ
	var resetMicro int64
	atMax := zrangeAtMaxCmd.Val()
	oldest := zrangeOldestCmd.Val()
	if len(atMax) > 0 {
		resetMicro = parseIntFromString(atMax[0]) + duration.Microseconds()
	} else if len(oldest) > 0 {
		resetMicro = parseIntFromString(oldest[0]) + duration.Microseconds()
	} else {
		resetMicro = now + duration.Microseconds()
	}

	return LimitInfo{
		Remaining: remaining,
		ResetMs:   resetMicro / 1000,
	}, nil
}

// parseIntFromString は文字列を int64 にパースする。
// Redis の ZRANGE は member を文字列で返すため。
func parseIntFromString(s string) int64 {
	var v int64
	// 読めない値は 0 を返す。呼び出し側は Redis のカウンタを読む経路で、
	// 0 と「未設定」を区別していない。
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil {
		return 0
	}
	return v
}

// RateLimiter provides per-endpoint rate limiting as Echo middleware.
type RateLimiter struct {
	store             RateLimitStore
	enableIPRateLimit bool
	limits            map[string]*EndpointLimit
	policyProvider    role.PolicyProvider // optional, nil なら factor=1 固定
	disabled          bool
}

// NewRateLimiter creates a RateLimiter with the given store and config.
func NewRateLimiter(store RateLimitStore, enableIPRateLimit bool, limits map[string]*EndpointLimit) *RateLimiter {
	return &RateLimiter{
		store:             store,
		enableIPRateLimit: enableIPRateLimit,
		limits:            limits,
	}
}

// NewRedisRateLimiter creates a RateLimiter backed by Redis.
func NewRedisRateLimiter(rdb *redis.Client, enableIPRateLimit bool, limits map[string]*EndpointLimit) *RateLimiter {
	return NewRateLimiter(NewRedisRateLimitStore(rdb), enableIPRateLimit, limits)
}

// Disable turns the limiter into a no-op.
//
// 本家 RateLimiterService は constructor で NODE_ENV !== 'production' なら
// disabled を立て、開発・テスト環境ではレート制限を一切かけない。mk-go に
// NODE_ENV に相当する概念は無いので、本番では必ず false になる TestMode を
// 同じ用途で使う (呼び出しは router.go 側)。
func (rl *RateLimiter) Disable() {
	rl.disabled = true
}

// SetPolicyProvider wires a PolicyProvider so authenticated user の
// rateLimitFactor が反映される。
//
// **`rateLimitFactor` は除数 (#3037 レビュー 2 周目で実測)。** `scaledMax` は
// `base / factor` なので、**値が大きいほど実効 Max は小さくなる** — trusted
// user を緩めるなら 0.5 のような 1 未満の値を割り当てる。以前ここには
// 「factor=2.0 で実効 Max を 2 倍にする」と書いてあったが逆で、そのとおりに
// 設定すると半分に締まる (倒れる向きは安全側だが、設定の効き方の記述が
// 実装と食い違っていた)。upstream `RateLimiterService.limit()` の
// `max: limitation.max / factor` と同じ。
func (rl *RateLimiter) SetPolicyProvider(p role.PolicyProvider) {
	rl.policyProvider = p
}

// Middleware returns an Echo middleware that enforces rate limiting.
// リミット定義のないエンドポイントはそのまま通過する。
// Redisエラー時はfail-open（ログだけ出してリクエストを通す）。
//
// 認証済みリクエストでは userID と IP の両 bucket を OR で評価する
// (#606 item 2): 攻撃者が認証して別アカウントを brute-force するケースで
// userID bucket に逃がされず、IP bucket でも捕捉する。X-RateLimit-Remaining
// は最も逼迫した bucket (最小値) を返す。
func (rl *RateLimiter) Middleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if rl.disabled {
				return next(c)
			}
			endpoint := strings.TrimPrefix(c.Path(), "/api/")
			limit, ok := rl.limits[endpoint]
			if !ok {
				return next(c)
			}

			actors := rl.resolveActors(c)
			if limit.UserBucketOnly {
				actors = dropIPActors(actors)
			}
			if len(actors) == 0 {
				return next(c)
			}

			ctx := c.Request().Context()
			minRemaining := -1

			for _, a := range actors {
				effectiveMax := scaledMax(limit.Max, a.factor)

				// minInterval チェック。#2106 N28: upstream RateLimiterService は
				// `duration: minInterval * factor` で window に factor を乗算する
				// (旧 mk-go は factor 未適用で乖離していた)。
				if limit.MinInterval > 0 {
					minInterval := scaledMinInterval(limit.MinInterval, a.factor)
					info, err := rl.store.Check(ctx, a.key+":"+endpoint+":min", minInterval, 1)
					if err != nil {
						slog.Warn("rate limit store error (minInterval)", "endpoint", endpoint, "err", err)
						continue
					}
					if info.Remaining == 0 {
						return rl.rejectRequest(c, info, limit)
					}
				}

				// duration/max チェック (factor 適用済みの effectiveMax で評価)
				if limit.Duration > 0 && effectiveMax > 0 {
					info, err := rl.store.Check(ctx, a.key+":"+endpoint, limit.Duration, effectiveMax)
					if err != nil {
						slog.Warn("rate limit store error", "endpoint", endpoint, "err", err)
						continue
					}
					if info.Remaining == 0 {
						return rl.rejectRequest(c, info, limit)
					}
					if minRemaining < 0 || info.Remaining < minRemaining {
						minRemaining = info.Remaining
					}
				}
			}
			if minRemaining >= 0 {
				c.Response().Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", minRemaining))
			}

			return next(c)
		}
	}
}

// rateLimitActor は単一 bucket を識別する key + factor のペア。
// 認証済 user では userID 由来 actor (factor=user policy) と IP 由来 actor
// (factor=1.0) が並ぶ。未認証では IP 由来 actor のみ。
type rateLimitActor struct {
	key    string  // bucket 識別子 (userID そのまま / "ip-<hash>" 等)
	factor float64 // Max scaling 係数 (>0、1.0 で base、>1 で緩和、<1 で厳格)
}

// resolveActors returns all bucket actors that should be checked for this
// request. 認証済 user は userID + IP の両方、未認証は IP のみを返す。
// EnableIPRateLimit=false かつ未認証なら空 (= 制限スキップ、既存挙動維持)。
func (rl *RateLimiter) resolveActors(c echo.Context) []rateLimitActor {
	var actors []rateLimitActor
	if user := GetUser(c); user != nil {
		// user.ID は ULID で `ip-` prefix と衝突しないので prefix なしで
		// そのまま bucket key にする (既存 Redis state との互換性維持)。
		actors = append(actors, rateLimitActor{
			key:    user.ID,
			factor: rl.userFactor(user.ID),
		})
		// 認証済でも IP bucket を併用 (#606 item 2)。NAT 配下の正当ユーザーは
		// 同一 IP で複数 user として扱われるが、auth 系 endpoint は 1h/60 程度
		// と緩いので実害は小さい (個別 user の rate-limit は userID bucket で
		// 守られる)。
		// **プロセス内の呼び出しは IP バケットを使わない。** プラグインの
		// `AsUser` は実在しない `127.0.0.1` を名乗るので、そのままだと全利用者が
		// 単一のバケットを取り合い、1 人の操作で他の全員が 429 になる。
		if rl.enableIPRateLimit && !IsInternalCall(c.Request().Context()) {
			actors = append(actors, rateLimitActor{
				key:    ipHash(c.RealIP()),
				factor: 1.0, // IP 由来 bucket は user 単位の factor を適用しない
			})
		}
		return actors
	}
	if !rl.enableIPRateLimit || IsInternalCall(c.Request().Context()) {
		// 未認証のプロセス内呼び出し (プラグインの `Anonymous()`) も同じ理由で
		// 共有バケットから外す。利用者単位のバケットが無いので、ここを外すと
		// 該当経路は事実上無制限になるが、呼び出せるのは自分が同梱した
		// プラグインだけ (外部からの入力は元のリクエストの側で既に制限を受ける)。
		return nil
	}
	return []rateLimitActor{{key: ipHash(c.RealIP()), factor: 1.0}}
}

// dropIPActors removes IP-derived buckets, leaving only user buckets.
//
// **`ipHash` が付ける `ip-` prefix で判別する。** user bucket は ULID をそのまま
// key にしており (`resolveActors` の注記)、`ip-` で始まらないことが保証されている。
func dropIPActors(actors []rateLimitActor) []rateLimitActor {
	out := actors[:0:0]
	for _, a := range actors {
		if strings.HasPrefix(a.key, ipBucketPrefix) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// userFactor returns the rateLimitFactor from the user's role policies.
// 未配線 / 値不正 / 0 以下なら 1.0 (= 影響なし) で返す。
func (rl *RateLimiter) userFactor(userID string) float64 {
	if rl.policyProvider == nil {
		return 1.0
	}
	policies := rl.policyProvider.GetUserPolicies(userID)
	raw, ok := policies["rateLimitFactor"]
	if !ok {
		return 1.0
	}
	switch v := raw.(type) {
	case float64:
		if v > 0 {
			return v
		}
	case int:
		if v > 0 {
			return float64(v)
		}
	}
	return 1.0
}

// scaledMax divides the base Max by factor, mirroring upstream
// RateLimiterService.limit() の `max: limitation.max / factor` (#2106 N28)。
// rateLimitFactor は**除数**で、値が大きいほど実効 max が小さくなる
// (= role policy で締める) — 例 factor=3(300%) で 1/3 に厳格化、factor=0.3(30%) で
// 約 3.3x に緩和。factor=1.0 / 不正値 (<=0またはNaN) は base そのまま。結果は最低 1 に
// クランプ (factor 過大の事故で全 user が瞬時に 429 を食らうのを防ぐ)。
func scaledMax(base int, factor float64) int {
	if factor <= 0 || factor == 1.0 || math.IsNaN(factor) {
		return base
	}
	scaled := safemath.Float64ToInt(float64(base) / factor)
	if scaled < 1 {
		return 1
	}
	return scaled
}

// scaledMinInterval multiplies the base minInterval window by factor, mirroring
// upstream RateLimiterService の `duration: limitation.minInterval * factor`
// (#2106 N28)。rateLimitFactor は乗数で、factor が大きいほど window が長くなり
// (= 連投間隔が厳しくなる)、小さいほど緩む。factor=1.0 / 不正値は base そのまま。
// 正方向の duration overflow は実質的な無期限拒否を避けるため base へ戻す。
func scaledMinInterval(base time.Duration, factor float64) time.Duration {
	if factor <= 0 || factor == 1.0 || math.IsNaN(factor) {
		return base
	}
	if factor >= float64(math.MaxInt64)/float64(base) {
		return base
	}
	return time.Duration(float64(base) * factor)
}

// rejectRequest returns a 429 response with appropriate headers. limit may
// carry a per-endpoint RejectResponse override (e.g. signin returns
// TOO_MANY_AUTHENTICATION_FAILURES instead of the generic RATE_LIMIT_EXCEEDED、
// #1829)。
func (rl *RateLimiter) rejectRequest(c echo.Context, info LimitInfo, limit *EndpointLimit) error {
	retryAfterSec := (info.ResetMs - time.Now().UnixMilli() + 999) / 1000
	if retryAfterSec < 0 {
		retryAfterSec = 0
	}
	c.Response().Header().Set("Retry-After", fmt.Sprintf("%d", retryAfterSec))
	if limit != nil && limit.RejectResponse != nil {
		return c.JSON(http.StatusTooManyRequests, limit.RejectResponse())
	}
	return c.JSON(http.StatusTooManyRequests, apierr.RateLimitExceeded())
}

// ipHash computes a rate-limit key for an IP address.
// TS版 getIpHash 互換: IPv6は/64マスク、IPv4はフルアドレスをハッシュ。
// IPHash exposes the bucket key derivation to callers outside this package.
//
// **同じ丸め方を使うため。** プラグインの peer 経路 (#2537) も IP を key に
// するが、生のアドレスを使うと IPv6 では /64 の中でアドレスを回すだけで
// いくらでも新しい枠が取れる。
func IPHash(ipStr string) string { return ipHash(ipStr) }

// ipBucketPrefix marks IP-derived bucket keys. user bucket は ULID をそのまま
// key にするのでこの prefix と衝突しない (`resolveActors` の注記)。
const ipBucketPrefix = "ip-"

func ipHash(ipStr string) string {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		// パース不能な場合はSHA256フォールバック
		h := sha256.Sum256([]byte(ipStr))
		n := new(big.Int).SetBytes(h[:8])
		return ipBucketPrefix + n.Text(36)
	}

	if ip4 := ip.To4(); ip4 != nil {
		// IPv4: フルアドレスをそのまま使用
		n := new(big.Int).SetBytes(ip4)
		return ipBucketPrefix + n.Text(36)
	}

	// IPv6: /64マスク（先頭8バイトのみ）
	ip16 := ip.To16()
	n := new(big.Int).SetBytes(ip16[:8])
	return ipBucketPrefix + n.Text(36)
}
