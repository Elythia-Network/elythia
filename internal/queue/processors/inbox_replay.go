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
type InboxReplayGuard interface {
	// Seen reports whether a request with this replay key was already processed.
	Seen(ctx context.Context, key string) (bool, error)
	// Remember records the replay key as processed.
	Remember(ctx context.Context, key string) error
}

// activityReplayKey derives the replay guard's key from the signed Date header
// and the signed request body. It is the key for requests without a usable
// Digest header (digestReplayKey), and the key before #3988.
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

// replayKeys returns the replay guard keys of a request whose HTTP signature
// was verified: the first is the key to remember the request by from now on,
// and every key is checked when looking for a replay.
//
// Digest が使えない (無い・署名が覆っていない・SHA-256 ではない) ときは、
// 今までどおり本文から鍵を作る。受付はそうしたリクエストを積まないが、受付を
// 通らずに積まれた payload (テストなど) でも、本文が違えば鍵が違う性質を保つ。
func (p *InboxProcessor) replayKeys(headers map[string]string, body []byte, digestSigned bool) []string {
	date := headers["Date"]
	digest := headers["Digest"]
	if !digestSigned || digest == "" {
		return []string{activityReplayKey(date, body)}
	}
	keys := []string{digestReplayKey(date, digest)}
	// **鍵の形を変えた直後は、前の形の鍵も見て、覚える。** 切り替える前に処理した
	// リクエストは前の形の鍵でしか Redis に残っていないので、新しい鍵だけを見ると、
	// 切り替えの直後 inboxReplayTTL の間はそれらを 1 回ずつ再投函できてしまう。
	// 前の形の鍵も覚えるのは、入れ替えの途中で古い版のプロセスが同時に動いて
	// いる間、そちらにも新しい版が処理したリクエストを見せるため。起動から
	// inboxReplayTTL が過ぎれば、前の形の鍵は全部期限が切れているので計算しない。
	//
	// **前提は、新旧の版が並んで動くのが inboxReplayTTL より短いこと。** 古い版の
	// worker を窓より長く併走させると、窓の後は両方の向きで 1 回ずつ再投函を通す。
	// 新しい版が処理したリクエストを、古い版は新しい形の鍵で見つけられない。古い版が
	// 前の形の鍵でだけ覚えたリクエストを、新しい版は窓の後には前の形の鍵を見ないので
	// 見つけられない。新しい版から古い版へ
	// 戻した直後も同じで、古い版は新しい形の鍵を見ないので、戻す前の inboxReplayTTL
	// の間に処理したリクエストを 1 回ずつ通す。
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	if now().Before(p.legacyReplayKeysUntil) {
		keys = append(keys, activityReplayKey(date, body))
	}
	return keys
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
