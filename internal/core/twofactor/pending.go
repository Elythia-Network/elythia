package twofactor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Limits of a temp secret issued by i/2fa/register: it can be confirmed by
// i/2fa/done within PendingTTL, with at most PendingMaxAttempts wrong codes.
//
// 仮の秘密に期限と試行回数の上限を置く。登録を途中でやめた状態が長く残らない
// ようにするため。QR を読んで 6 桁を打つまでに 1 時間・10 回あれば足りる。
const (
	PendingTTL         = time.Hour
	PendingMaxAttempts = 10
)

// ErrPendingExpired reports a temp secret that can no longer be confirmed:
// it was not issued through PendingGuard.Start (or that record expired), or
// its wrong-code budget is used up. The caller discards the temp secret.
var ErrPendingExpired = errors.New("twofactor: pending secret expired")

// PendingGuard bounds how long and how many times a temp secret can be
// confirmed.
type PendingGuard interface {
	// Start records that secret was issued to userID, replacing any earlier
	// record and its count.
	Start(ctx context.Context, userID, secret string) error
	// Attempt reserves one confirmation of secret. It returns an error wrapping
	// ErrPendingExpired when the secret must not be checked any more.
	Attempt(ctx context.Context, userID, secret string) error
	// Finish drops the record once the secret was confirmed or discarded.
	Finish(ctx context.Context, userID string) error
}

// RedisPendingGuard implements PendingGuard with one Redis hash per user
// holding the digest of the issued secret and the attempts made so far.
type RedisPendingGuard struct {
	client      redis.UniversalClient
	ttl         time.Duration
	maxAttempts int64
}

// NewRedisPendingGuard returns a PendingGuard with PendingTTL and
// PendingMaxAttempts. Passing nil returns nil (no limit, unit tests only).
func NewRedisPendingGuard(client redis.UniversalClient) *RedisPendingGuard {
	if client == nil {
		return nil
	}
	return &RedisPendingGuard{client: client, ttl: PendingTTL, maxAttempts: PendingMaxAttempts}
}

func pendingKey(userID string) string { return "mk:2fa:pending:" + userID }

// 秘密そのものは Redis に置かない。照合できればよいので digest にする。
func pendingDigest(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Start implements PendingGuard.
func (g *RedisPendingGuard) Start(ctx context.Context, userID, secret string) error {
	key := pendingKey(userID)
	pipe := g.client.TxPipeline()
	pipe.Del(ctx, key)
	pipe.HSet(ctx, key, "s", pendingDigest(secret), "n", 0)
	pipe.PExpire(ctx, key, g.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("twofactor: start pending secret: %w", err)
	}
	return nil
}

// 照合と加算を 1 つの script にする。分けると、並行に投げた試行がすべて
// 加算前の回数を見て通り、上限を超えて照合できる。key が無いときに HINCRBY で
// 期限の無い key を作らないよう、digest を確かめてから数える。
var pendingAttemptScript = redis.NewScript(`
local s = redis.call('HGET', KEYS[1], 's')
if not s or s ~= ARGV[1] then
  return -1
end
return redis.call('HINCRBY', KEYS[1], 'n', 1)
`)

// Attempt implements PendingGuard.
func (g *RedisPendingGuard) Attempt(ctx context.Context, userID, secret string) error {
	n, err := pendingAttemptScript.Run(ctx, g.client, []string{pendingKey(userID)}, pendingDigest(secret)).Int64()
	if err != nil {
		return fmt.Errorf("twofactor: pending attempt: %w", err)
	}
	if n < 0 || n > g.maxAttempts {
		return ErrPendingExpired
	}
	return nil
}

// Finish implements PendingGuard.
func (g *RedisPendingGuard) Finish(ctx context.Context, userID string) error {
	if err := g.client.Del(ctx, pendingKey(userID)).Err(); err != nil {
		return fmt.Errorf("twofactor: finish pending secret: %w", err)
	}
	return nil
}
