// Package passwordguard limits how often a signed-in account's current
// password can be checked and found wrong.
//
// `i/*` の endpoint のうち現在のパスワードを照合するもの (パスワード変更・
// token 再生成・アカウント削除・2FA の設定変更など) は、token を持つ相手に
// とってパスワードの総当たりのオラクルになる。当たればパスワードそのものが
// 手に入り、token を失効させても signin し直せる。
//
// **数えるのは照合に失敗した回数だけ。** key は 2 段で、(アカウント, 接続元の
// 範囲) ごとに DefaultMaxFailures、アカウント全体で DefaultMaxAccountFailures。
// route ごとのレート制限 (limiter) で絞ると、limiter は権限検査より前に走って
// 成否に関係なく消費するので、token を持つだけの第三者が被害者の
// `i/regenerate-token` を使い切れる (= token 漏洩時の唯一の対処を止められる)。
// endpoint ごとの bucket では合計の試行回数も endpoint の数だけ増える。
//
// **アカウント単位だけで数えると、token を盗んだ攻撃者が被害者の
// `i/regenerate-token` を止め続けられる。** わざと 10 回間違えるだけで枠が
// 尽き、token 漏洩時の唯一の対処が 429 になる。接続元の範囲 (IPv4 /24、
// IPv6 /64) ごとの枠を主にし、アカウント全体の枠は大きく取ることで、妨害には
// 攻撃者が接続元の範囲を 10 以上用意する必要がある形にする。総当たりは
// アカウント全体の枠で 1 時間あたり DefaultMaxAccountFailures 回に止まる。
package passwordguard

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// Default budget within DefaultWindow: DefaultMaxFailures wrong passwords per
// (account, client range) and DefaultMaxAccountFailures per account.
const (
	DefaultMaxFailures        = 10
	DefaultMaxAccountFailures = 100
	DefaultWindow             = time.Hour
)

// ErrTooManyFailures is returned (wrapped in *LimitedError) when the account
// has used up its failure budget.
var ErrTooManyFailures = errors.New("passwordguard: too many incorrect passwords")

// LimitedError reports when the next check will be allowed.
type LimitedError struct {
	RetryAfter time.Duration
}

func (e *LimitedError) Error() string { return ErrTooManyFailures.Error() }

// Unwrap lets errors.Is match ErrTooManyFailures.
func (e *LimitedError) Unwrap() error { return ErrTooManyFailures }

// Attempt is one reserved password check.
//
// 予約はそのまま「失敗 1 回」として残る。照合に成功したとき、または照合
// しなかったとき (検証枠が取れなかった等) にだけ Release する。
type Attempt interface {
	// Release drops the reservation so it does not count as a failure.
	Release(ctx context.Context)
}

// Guard reserves password checks against a per-account failure budget.
type Guard interface {
	// Begin reserves one check for userID made from clientIP. It returns an
	// error wrapping ErrTooManyFailures when a budget is exhausted; the caller
	// must not compare the password then.
	Begin(ctx context.Context, userID, clientIP string) (Attempt, error)
}

// RedisGuard implements Guard with a sliding-window sorted set per account.
type RedisGuard struct {
	rdb                redis.Cmdable
	maxFailures        int
	maxAccountFailures int
	window             time.Duration
	now                func() time.Time
}

// NewRedisGuard returns a Guard with the default budgets.
func NewRedisGuard(rdb redis.Cmdable) *RedisGuard {
	return &RedisGuard{
		rdb:                rdb,
		maxFailures:        DefaultMaxFailures,
		maxAccountFailures: DefaultMaxAccountFailures,
		window:             DefaultWindow,
		now:                time.Now,
	}
}

func failureKey(userID string) string {
	return "passwordguard:failures:" + userID
}

func clientFailureKey(userID, clientIP string) string {
	return "passwordguard:failures:" + userID + ":" + ClientRange(clientIP)
}

// ClientRange returns the bucket for clientIP: the /24 of an IPv4 address,
// the /64 of an IPv6 address, or the raw string when it does not parse.
//
// 1 台の端末や 1 回線が持つ範囲でまとめる。IPv6 は利用者 1 人に /64 が
// 割り当てられるのが普通なので、アドレス単位だと範囲内で回して枠を増やせる。
func ClientRange(clientIP string) string {
	ip := net.ParseIP(clientIP)
	if ip == nil {
		return clientIP
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.Mask(net.CIDRMask(24, 32)).String() + "/24"
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// Begin implements Guard.
//
// **照合の前に予約する。** 「失敗したら記録する」形だと、bcrypt の照合中
// (~100ms) に並行で投げたリクエストがすべて残り枠を見て通り、上限を何倍にも
// 超えられる。先に ZADD してから数えるので、並行でも照合に進めるのは
// 上限までに限られる。上限を超えた予約はその場で取り消す (照合していない
// ので失敗として数えない。数えると 429 を無視して叩き続けるだけで窓が
// 押し戻され続ける)。
func (g *RedisGuard) Begin(ctx context.Context, userID, clientIP string) (Attempt, error) {
	if g == nil || g.rdb == nil {
		return noopAttempt{}, nil
	}
	now := g.now()
	nowMicro := now.UnixMicro()
	member, err := newMember(nowMicro)
	if err != nil {
		return nil, err
	}
	keys := []string{clientFailureKey(userID, clientIP), failureKey(userID)}
	limits := []int{g.maxFailures, g.maxAccountFailures}
	cutoff := strconv.FormatInt(nowMicro-g.window.Microseconds(), 10)
	pipe := g.rdb.TxPipeline()
	cards := make([]*redis.IntCmd, len(keys))
	oldest := make([]*redis.ZSliceCmd, len(keys))
	for i, key := range keys {
		pipe.ZRemRangeByScore(ctx, key, "-inf", cutoff)
		pipe.ZAdd(ctx, key, redis.Z{Score: float64(nowMicro), Member: member})
		cards[i] = pipe.ZCard(ctx, key)
		oldest[i] = pipe.ZRangeWithScores(ctx, key, 0, 0)
		pipe.PExpire(ctx, key, g.window)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("passwordguard: reserve: %w", err)
	}
	a := &redisAttempt{rdb: g.rdb, keys: keys, member: member}
	var retry time.Duration
	limited := false
	for i := range keys {
		if int(cards[i].Val()) <= limits[i] {
			continue
		}
		limited = true
		r := g.window
		if o := oldest[i].Val(); len(o) > 0 {
			r = time.Duration(int64(o[0].Score)+g.window.Microseconds()-nowMicro) * time.Microsecond
		}
		if r > retry {
			retry = r
		}
	}
	if !limited {
		return a, nil
	}
	a.Release(ctx)
	if retry < 0 {
		retry = 0
	}
	return nil, &LimitedError{RetryAfter: retry}
}

func newMember(nowMicro int64) (string, error) {
	var b [8]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return "", fmt.Errorf("passwordguard: member: %w", err)
	}
	return strconv.FormatInt(nowMicro, 10) + "-" + hex.EncodeToString(b[:]), nil
}

type redisAttempt struct {
	rdb    redis.Cmdable
	keys   []string
	member string
}

// Release implements Attempt.
func (a *redisAttempt) Release(ctx context.Context) {
	// 取り消しに失敗しても失敗 1 回として残るだけで、安全側に倒れる。
	for _, key := range a.keys {
		_ = a.rdb.ZRem(ctx, key, a.member).Err()
	}
}

type noopAttempt struct{}

// Release implements Attempt.
func (noopAttempt) Release(context.Context) {}

// NoopAttempt returns an Attempt whose Release does nothing. Callers use it
// when the guard is not configured or its store failed.
func NoopAttempt() Attempt { return noopAttempt{} }
