package processors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/elythia-network/elythia/internal/activitypub"
)

// InboxReplayGuard remembers activities that were already processed, so the
// same signed request cannot be posted again inside its Date window.
//
// **upstream にこの層は無い。** upstream (と mk-go) はハンドラの冪等性で
// 二重配送を吸収する設計で、それ自体は正しいが「古い活動を後から差し込む」形は
// 吸収できない。捕まえた署名付きリクエストを投げ直すと、Undo(Follow) /
// Undo(Block) のような**状態を戻す**活動が効いてしまう。
//
// 署名の Date に窓 (activitypub.InboxDateSkew) を入れたことで再投函できる時間は
// 有限になったが、窓の内側はまだ通る。ここはその内側を塞ぐ。
type InboxReplayGuard interface {
	// Seen reports whether activityID was already processed.
	Seen(ctx context.Context, activityID string) (bool, error)
	// Remember records activityID as processed.
	Remember(ctx context.Context, activityID string) error
}

// inboxReplayTTL is how long a processed activity is remembered.
//
// **Date の窓より長くする。** 署名は `now-skew` から `now+skew` まで受理される
// ので、最も早く受け取ってから最も遅く再投函されるまで最大 2*skew 開く。
// そこに時計のずれ分の余裕を足す。
const inboxReplayTTL = 2*activitypub.InboxDateSkew + 5*time.Minute

// inboxReplayKeyPrefix namespaces the guard's Redis keys.
const inboxReplayKeyPrefix = "mk:ap:inbox:seen:"

// RedisInboxReplayGuard implements InboxReplayGuard on top of go-redis.
type RedisInboxReplayGuard struct {
	client redis.UniversalClient
	ttl    time.Duration
}

// NewRedisInboxReplayGuard returns a guard backed by client. nil client returns
// nil so callers can treat "未配線 = 保護なし" uniformly.
func NewRedisInboxReplayGuard(client redis.UniversalClient) *RedisInboxReplayGuard {
	if client == nil {
		return nil
	}
	return &RedisInboxReplayGuard{client: client, ttl: inboxReplayTTL}
}

// inboxReplayKey hashes the activity ID.
//
// activity.id はリモートが決める URL なので長さも中身も任意。**そのまま key に
// すると長さが読めず、Redis の keyspace に相手の文字列がそのまま載る。**
// 固定長のハッシュにしておけば両方とも起きない。
func inboxReplayKey(activityID string) string {
	sum := sha256.Sum256([]byte(activityID))
	return inboxReplayKeyPrefix + hex.EncodeToString(sum[:])
}

// Seen reports whether the activity was already processed.
func (g *RedisInboxReplayGuard) Seen(ctx context.Context, activityID string) (bool, error) {
	if g == nil || g.client == nil || activityID == "" {
		return false, nil
	}
	n, err := g.client.Exists(ctx, inboxReplayKey(activityID)).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// Remember records the activity as processed.
func (g *RedisInboxReplayGuard) Remember(ctx context.Context, activityID string) error {
	if g == nil || g.client == nil || activityID == "" {
		return nil
	}
	return g.client.Set(ctx, inboxReplayKey(activityID), "1", g.ttl).Err()
}
