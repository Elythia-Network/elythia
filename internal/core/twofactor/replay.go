package twofactor

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
)

// bypassForTest reports whether TOTP validation should be skipped entirely.
//
// upstream UserAuthService.validateOtp は
// `NODE_ENV === 'test' && MISSKEY_TEST_CHECK_DUPLICATED_TOTP !== '1'` のとき
// 検証ごと素通しにする。本家 e2e は 2FA 登録に使ったコードを直後の signin でも
// 使い回すので、素通しにしないと replay 保護に引っかかって成立しない。
// replay 保護そのものを検証するテストだけが同 env に '1' を立てる。
//
// 環境変数は毎回読む。e2e ハーネスがテストの途中で値を差し替えるため。
//
// testModeは呼び出し元(api/signinとapi/iのhandler)が設定から受け取って渡す
// (#3780)。以前はパッケージの変数で、Routerが起動のたびに書き換えていた。
func bypassForTest(testMode bool) bool {
	return testMode && os.Getenv("MISSKEY_TEST_CHECK_DUPLICATED_TOTP") != "1"
}

// ReplayGuard records previously accepted TOTP codes so the same code
// cannot be replayed within its acceptance window.
//
// RFC 6238 §5.2 requires that "the verifier MUST NOT accept the second
// attempt of the OTP after the successful validation has been issued
// for the first OTP". Without it, an attacker who observes a 6-digit code
// (phishing, shoulder surfing, MITM proxy) can replay it within the code's
// acceptance window — long enough to open multiple sessions or to chain into
// other 2FA-gated endpoints (i/2fa/done, key registration, etc.).
//
// **upstream も 2026.6.0 で同等の機構を持つ** (`UserAuthService.validateOtp` が
// Redis の `SET NX EX` で使用済みトークンを記録する)。mk-go が先行実装したもので、
// 現在は差分なし (docs/divergence.md)。バックアップコードの単回予約
// (BackupCodeGuardKey + ReserveOnce) のほうは upstream に無い。
//
// 実装は drop-in 互換の schema の上に載る: no new tables, no new error codes, just a Redis
// keyspace that mirrors the validity window of a TOTP code.
type ReplayGuard interface {
	// MarkUsed records (userID, code) as consumed. It returns true if
	// the entry was newly inserted (caller may treat the code as valid),
	// or false if the same code was already accepted within the window
	// (caller must reject the request as a replay).
	MarkUsed(ctx context.Context, userID, code string) (bool, error)
}

// ReplayReleaser is an optional extension of ReplayGuard for guards that can
// undo a MarkUsed.
//
// **必要になる理由。** `MarkUsed` は SETNX で「検査」と「記録」が不可分なので、
// 2FA を検証した時点で記録してしまう。ところが呼び出し側は、その後に走る
// password 検証で落ちることがある (#2852)。記録を残したままにすると、利用者が
// 同じ (まだ有効な) コードで打ち直したときに replay として弾かれ、原因から
// 遠い `INVALID_TOKEN` になる。
//
// **検査を非破壊にする形は採らない。** EXISTS と SETNX に割ると、その隙間に
// 同じコードで 2 本通せてしまい replay 保護が緩む。記録は今までどおり検証時に
// 行い、失敗が確定した時点で取り消す。
//
// 実装しない guard は取り消しが no-op になる (Release を呼ぶ側が type assertion
// で判定する)。fail-open 側なので、記録が残るだけで安全側に倒れる。
type ReplayReleaser interface {
	Release(ctx context.Context, userID, code string) error
}

// defaultReplayTTL covers the full acceptance window of a TOTP code under
// our validation settings (Period=30s, Skew=totpSkew). A code generated for
// step T stays structurally valid across [T-skew, T+skew] = (2*skew+1) steps,
// so the guard entry must survive at least that long after the first
// acceptance — otherwise the code could be replayed during the residual
// validity once the entry expires. We pad by one extra step for clock jitter.
// totpSkew から導出して lockstep を保つ。#2069 で totpSkew が 5→1 になった
// (upstream #17580 validationWindow:1 追従) ため Skew=1 → 4 steps = 120s
// (validity 90s + 30s jitter pad)。upstream ttl=timeStep*(window*2+1)=90s より 1 step
// 保守的。
const defaultReplayTTL = time.Duration(2*totpSkew+2) * 30 * time.Second

// RedisReplayGuard implements ReplayGuard on top of go-redis. Keys are
// per-user and per-code; collisions across users are impossible because
// the userID is part of the key.
type RedisReplayGuard struct {
	Client redis.UniversalClient
	// KeyPrefix is prepended to every Redis key. When empty, a sensible
	// default ("mk:2fa:totp:used") is used so callers can opt into the
	// guard without configuring anything else.
	KeyPrefix string
	// TTL is the lifetime of each replay record. When zero, defaults to
	// defaultReplayTTL (= TOTP acceptance window for the configured Skew,
	// with a one-step safety margin).
	TTL time.Duration
}

// NewRedisReplayGuard creates a ReplayGuard backed by the given Redis
// client. Passing nil returns nil — callers should treat a nil guard
// as "protection disabled" (used in unit tests that do not need a Redis
// fixture; production paths should always wire a non-nil guard).
func NewRedisReplayGuard(client redis.UniversalClient) *RedisReplayGuard {
	if client == nil {
		return nil
	}
	return &RedisReplayGuard{Client: client}
}

// MarkUsed atomically inserts the (userID, code) entry. Returns true if
// inserted (= code is fresh), false if it was already present (= replay).
func (g *RedisReplayGuard) MarkUsed(ctx context.Context, userID, code string) (bool, error) {
	if g == nil || g.Client == nil {
		// fail-open: keep historical Misskey behaviour when no guard is
		// wired (= unit tests, dev without Redis)
		return true, nil
	}
	prefix := g.KeyPrefix
	if prefix == "" {
		prefix = "mk:2fa:totp:used"
	}
	ttl := g.TTL
	if ttl <= 0 {
		// 未設定時は TOTP acceptance window をカバーする default を使う。
		ttl = defaultReplayTTL
	}
	key := fmt.Sprintf("%s:%s:%s", prefix, userID, code)
	return g.Client.SetNX(ctx, key, "1", ttl).Result()
}

// Release removes the (userID, code) entry recorded by MarkUsed.
//
// 取り消せなかった場合は error を返すが、呼び出し側は握り潰してよい —
// 記録が残るだけで、TOTP コードが 1 つ余分に使えなくなるのは安全側。
func (g *RedisReplayGuard) Release(ctx context.Context, userID, code string) error {
	if g == nil || g.Client == nil {
		return nil
	}
	prefix := g.KeyPrefix
	if prefix == "" {
		prefix = "mk:2fa:totp:used"
	}
	return g.Client.Del(ctx, fmt.Sprintf("%s:%s:%s", prefix, userID, code)).Err()
}

// releaseReplay undoes a MarkUsed when the guard supports it.
//
// guard が nil、または ReplayReleaser を実装していない場合は何もしない。
func releaseReplay(ctx context.Context, guard ReplayGuard, userID, code string) {
	releaser, ok := guard.(ReplayReleaser)
	if !ok || releaser == nil {
		return
	}
	if err := releaser.Release(ctx, userID, code); err != nil {
		// 取り消せなくても安全側 (コードが 1 つ使えなくなるだけ)。
		slog.Warn("twofactor: failed to release replay guard entry", "userID", userID, "err", err)
	}
}

// ReserveOnce records a single-use credential so a concurrent request cannot
// accept the same one. Returns false when it was already reserved.
//
// **バックアップコードの単回性を守るために要る** (#2852)。DB の配列を読んでから
// 書き戻すまでの間隔に複数のリクエストが入ると、全部が同じスナップショットを
// 読んで全部が通る。SETNX なら原子的に 1 本だけに絞れる。
//
// guard が nil のときは true (fail-open)。Redis 障害で 2FA を閉塞させると
// operator が自分の環境から締め出されるため、MarkUsed と同じ判断に揃える。
func ReserveOnce(ctx context.Context, guard ReplayGuard, userID, key string) bool {
	if guard == nil {
		return true
	}
	ok, err := guard.MarkUsed(ctx, userID, key)
	if err != nil {
		slog.Warn("twofactor: reservation guard degraded, falling back to no protection", "userID", userID, "err", err)
		return true
	}
	return ok
}

// ValidateWithReplay validates a TOTP code AND records it as consumed
// via the supplied guard. Returns true only when the code is structurally
// valid for the secret AND has not been accepted within the replay
// window. A nil guard disables replay protection (returns Validate-only
// semantics) — production callers should always pass a non-nil guard.
//
// testMode is config.testMode. When it is true and the environment variable
// MISSKEY_TEST_CHECK_DUPLICATED_TOTP is not "1", any code is accepted without
// validation or replay protection, as upstream does in its test runs.
//
// The caller is expected to translate a false return into the same
// "invalid token" response shape it already returns for ordinary TOTP
// failures, so the frontend / client behaviour is unchanged.
func ValidateWithReplay(ctx context.Context, guard ReplayGuard, userID, code, secret string, testMode bool) bool {
	if bypassForTest(testMode) {
		return true
	}
	if !Validate(code, secret) {
		return false
	}
	if guard == nil {
		return true
	}
	ok, err := guard.MarkUsed(ctx, userID, code)
	if err != nil {
		// Redis 障害時に 2FA を完全閉塞させてしまうと operator が自分の
		// 環境から締め出される事故が起きるため、guard 障害時は historical
		// 挙動 (= replay 保護無し) に fail-open する。「2FA だけ degraded」
		// に気付けるよう warn ログを残す (他経路 queue / cache の alarm が
		// 出ていなくても本ログだけで判別できる)。
		slog.Warn("twofactor: replay guard degraded, falling back to no replay protection", "userID", userID, "err", err)
		return true
	}
	return ok
}

// BackupCodeGuardKey namespaces a backup code inside the TOTP replay keyspace.
//
// 同じ guard を共有するので prefix で分ける。**今のコード生成では衝突しない**
// (バックアップコードは hex 16 文字、TOTP は 6 桁) が、片方の桁数を変えたときに
// 「一方を使うともう一方が使えなくなる」形で壊れるのを防ぐ。
//
// **signin と i/* が同じ関数を使うこと** (#2862)。prefix が食い違うと、両者が
// 別々の keyspace を予約することになり、跨いだ同時実行を 1 本に絞れない。
func BackupCodeGuardKey(code string) string { return "bc:" + code }

// ReleaseReservation undoes a MarkUsed, detached from the request context.
//
// **リクエストの ctx をそのまま使うと取り消せない** (#2852)。password 検証が
// `OutcomeUnavailable` になる理由の 1 つが ctx キャンセルで、そのときは
// `Del` も `context canceled` で落ちて記録が残る。安全側ではあるが、
// 「503 でも焼けない」という狙いが半分しか効かない。
func ReleaseReservation(ctx context.Context, guard ReplayGuard, userID, key string) {
	out, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	releaseReplay(out, guard, userID, key)
}

// releaseTimeout bounds the best-effort rollback so a stuck Redis cannot hold
// the request goroutine.
const releaseTimeout = 3 * time.Second
