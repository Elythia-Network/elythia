package processors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
//
// **鍵は activity.id ではなく、署名が覆った Date ヘッダーと本文のダイジェスト**
// (digestReplayKey。ダイジェストは受付が本文との一致を確かめた Digest ヘッダーの
// 値で、使えなければ activityReplayKey で本文から作る)。id を鍵にすると、同じ id で
// 中身の違う活動 (本家 Misskey の Undo(Follow) は `<Follow の id>/undo` で決定的、
// 違うのは `published` だけ) を 2 つ目から捨てる。本文だけを鍵にしても、Elythia 自身の Follow は決定的な id で
// `published` も無いので、解除してから 15 分以内の再フォローが同じバイト列になり
// 捨てられる。Date は受付が署名の対象として必須にしているので改変できず、捕まえた
// リクエストの再投函では同じ値、正当な送り直しでは新しい値になる。これが「同じ
// リクエスト」の過不足ない定義になる (2026-10-31 の監査とそのレビュー)。
//
// **処理の前に「処理中」の印を原子的に置く** (Claim)。見てから処理して覚える順だと、
// 捕まえたリクエストを最初の処理が終わる前に並行に投げれば、どれも「まだ見ていない」
// と判断して通る (2026-11-22 の監査)。印にはジョブの ID を持たせ、ワーカーが処理の途中で
// 落ちて同じジョブを取り直したときは、残った自分の印を通れるようにする (失敗して戻ったときは
// 印を外すので、再試行はそのまま通る)。
type InboxReplayGuard interface {
	// Claim marks the replay key as being processed by owner unless it is
	// marked already, and reports whether owner holds the mark: it placed the
	// mark now, or placed it before (a retry of the same job). A processed key
	// or another owner's mark is not held.
	Claim(ctx context.Context, key, owner string) (bool, error)
	// Remember records the replay key as processed.
	Remember(ctx context.Context, key string) error
	// Release drops owner's processing mark on the key. A processed record and
	// another owner's mark are left as they are.
	Release(ctx context.Context, key, owner string) error
}

// activityReplayKey derives the replay guard's key from the signed Date header
// and the signed request body. It is the key for requests without a usable
// Digest header (digestReplayKey).
func activityReplayKey(date string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(date))
	h.Write([]byte{'\n'})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// digestReplayKeyDomain separates digestReplayKey's input from
// activityReplayKey's. HTTP のヘッダーの値に NUL は入らないので、Date で始まる
// activityReplayKey の入力とは重ならない。
const digestReplayKeyDomain = "\x00digest\n"

// digestReplayKey derives the replay guard's key from the signed Date header
// and the signed Digest header.
//
// **本文の代わりに Digest を使う (#3988)。** 受付の VerifyInboxAdmission が、
// Digest が署名の対象であることと、その値が本文の SHA-256 と一致することを
// 確かめてから積んでいる。署名が覆う値なので書き換えられず、本文が違えば値も
// 違う。activityReplayKey と同じく「同じリクエストなら同じ鍵、本文か Date が
// 違えば違う鍵」になり、本文全体の SHA-256 をもう一度計算しなくて済む
// (63KB の本文で 1 件あたり約 125µs)。値は受付が受けた文字列をそのまま使う
// (書式の揺れを正規化しない。署名が覆うのはこの文字列なので、同じリクエストは
// 同じ文字列になる)。
func digestReplayKey(date, digest string) string {
	h := sha256.New()
	h.Write([]byte(digestReplayKeyDomain))
	h.Write([]byte(date))
	h.Write([]byte{'\n'})
	h.Write([]byte(digest))
	return hex.EncodeToString(h.Sum(nil))
}

// requestReplayKey returns the replay guard key of a request whose HTTP signature
// was verified.
//
// Digest が使えない (無い・署名が覆っていない・SHA-256 ではない) ときは、
// 今までどおり本文から鍵を作る。受付はそうしたリクエストを積まないが、受付を
// 通らずに積まれた payload (テストなど) でも、本文が違えば鍵が違う性質を保つ。
//
// **#3988 より前の形の鍵 (Date と本文の SHA-256) は見ない (#4275)。** 2.2.0 と 2.3.0 は、
// 起動から inboxReplayTTL の間、前の形の鍵も計算して見て、覚えていた。しかし前の形の鍵だけで
// 覚えたリリースは無く (1.3.0〜2.1.0 は activity の id が鍵。前の形だけの版はリリースの間の
// develop にだけあった)、リリースからの更新を守ったことが無いのに、起動のたびに 15 分、大きな
// 本文の受信を重くしていた (63KB の本文で約 +29%)。2.1.0 以前から上げた直後の再投函は
// 2.2.0 からの既知の差 (docs/deployment.md)。
func requestReplayKey(headers map[string]string, body []byte, digestSigned bool) string {
	date := headers["Date"]
	digest := headers["Digest"]
	if !digestSigned || digest == "" {
		return activityReplayKey(date, body)
	}
	return digestReplayKey(date, digest)
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

// inboxReplayProcessed is the value of a processed key. 処理中の印は
// inboxReplayClaimPrefix + owner で、ジョブの ID ("1" など) と取り違えない。
const (
	inboxReplayProcessed   = "1"
	inboxReplayClaimPrefix = "claim:"
)

// Claim marks the activity as being processed by owner.
//
// 印の期限は処理済みの記録と同じ。処理の途中でワーカーが落ちても、同じジョブの
// 再試行は自分の印を通れるので、期限が切れるのを待たなくてよい。
func (g *RedisInboxReplayGuard) Claim(ctx context.Context, activityID, owner string) (bool, error) {
	if g == nil || g.client == nil || activityID == "" {
		return true, nil
	}
	key, mark := inboxReplayKey(activityID), inboxReplayClaimPrefix+owner
	placed, err := g.client.SetNX(ctx, key, mark, g.ttl).Result()
	if err != nil || placed {
		return placed, err
	}
	held, err := g.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		// 見た直後に期限が切れた。もう一度だけ置いてみる。
		return g.client.SetNX(ctx, key, mark, g.ttl).Result()
	}
	if err != nil {
		return false, err
	}
	return held == mark, nil
}

// Remember records the activity as processed.
func (g *RedisInboxReplayGuard) Remember(ctx context.Context, activityID string) error {
	if g == nil || g.client == nil || activityID == "" {
		return nil
	}
	return g.client.Set(ctx, inboxReplayKey(activityID), inboxReplayProcessed, g.ttl).Err()
}

// releaseClaim deletes KEYS[1] only when it still holds the mark ARGV[1].
var releaseClaim = redis.NewScript(`if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end return 0`)

// Release drops owner's processing mark on the activity.
func (g *RedisInboxReplayGuard) Release(ctx context.Context, activityID, owner string) error {
	if g == nil || g.client == nil || activityID == "" {
		return nil
	}
	return releaseClaim.Run(ctx, g.client, []string{inboxReplayKey(activityID)}, inboxReplayClaimPrefix+owner).Err()
}
