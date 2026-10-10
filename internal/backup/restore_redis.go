package backup

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/redis/go-redis/v9"
)

// RedisCleanupTarget is one Redis client and the key prefix the server
// puts in front of its prefixed keys (config RedisOptions.KeyPrefix()).
type RedisCleanupTarget struct {
	Client *redis.Client
	Prefix string
}

// RedisCleanupTargets are the clients whose keys a restore clears.
type RedisCleanupTargets struct {
	// Default is the `redis:` client.
	Default RedisCleanupTarget
	// Timelines is the `redisForTimelines:` client (the same Redis as
	// Default unless configured separately).
	Timelines RedisCleanupTarget
	// JobQueue is the `redisForJobQueue:` client. mkq does not use the
	// prefix (its keys start with "bull:"), so Prefix is ignored.
	JobQueue RedisCleanupTarget
}

// timelinePatterns are cleared on the timelines client, after its prefix.
//
// どれも DB の行 (note / user / drive_file) の ID を持ち、バックアップより後に
// 作られたものを指しうる。
//   - list:*: FTT のタイムライン (home / local / global / user / list)。残すと、
//     戻した DB に無い投稿の ID がタイムラインの先頭に残る
//   - eph*: リレーだけの投稿 (#2722)。ephFile: は drive_file を掃除から守る印で、
//     印の付いた行が戻した DB に無いか、別のものになっている
var timelinePatterns = []string{
	"list:*",
	"ephNote:*", "ephNoteURI:*", "ephUser:*", "ephUserURI:*", "ephFile:*",
}

// defaultPrefixedPatterns are cleared on the default client, after its
// prefix.
//
//   - cleanRemoteNotes:cursor: リモートの投稿の掃除が次に読む note の ID。戻した
//     DB より先を指していると、その間の投稿を掃除しない
var defaultPrefixedPatterns = []string{
	"cleanRemoteNotes:cursor",
}

// defaultPatterns are cleared on the default client without a prefix (the
// server writes these keys without one).
//
//   - antennaTimeline:* / featured*Ranking:*: note の ID の ZSET
//   - reaction-buffer:*: 未反映のリアクション数の差分。反映すると、戻した note の
//     数に後の差分が足される
//   - userSwSubscriptions:*: sw_subscription の行のキャッシュ
//   - reversi:fed:* / reversi:game:* / reversi:matchAny / bubbleVersus:*: 対局の
//     ID と利用者の ID を持つ途中の状態
//   - apFederationRule:*: 連合のルールの ID ごとの集計
//   - oauth:*: 認可の途中の状態。発行した token の ID (DB の行) を持つ
var defaultPatterns = []string{
	"antennaTimeline:*",
	"featuredGlobalNotesRanking:*", "featuredInChannelNotesRanking:*",
	"featuredPerUserNotesRanking:*", "featuredGalleryPostsRanking:*",
	"reaction-buffer:*",
	"userSwSubscriptions:*",
	"reversi:fed:*", "reversi:game:*", "reversi:matchAny",
	"bubbleVersus:*",
	"apFederationRule:*",
	"oauth:*",
}

// 消さないもの (docs/deployment.md の「バックアップから戻す」にも書く):
//   - <prefix>notificationTimeline:* / latestReadNotification:*: 通知は Redis にしか
//     無い (本家と同じ)。消すとバックアップより前の通知も失われる。読むときに、
//     note や送り主が DB に無い通知は落とす (internal/api/notifications) ので、
//     戻した後に存在しない投稿は出ない
//   - mk:ap:inbox:seen:* / passwordguard:* / mk:2fa:totp:used:* /
//     mk:signupform:nonce:* / limit:*: 再送・総当たり・再利用を止めるための記録。
//     消すと、その窓が開き直る
//   - apDelivery* / apInboxHealth:* / ed25519:* / reversi:federation:version:* /
//     <prefix>url-preview:*: 相手のサーバーや外部の URL についての記録で、DB から
//     作ったものではない
//   - <prefix>elythia:maintenance*: メンテナンスの状態 (#3463)。画面からの
//     リストアは、メンテナンス中にこの後始末を呼ぶ
//   - bull:* のうち deliver 以外の queue (jobQueue): バックアップより後に届いた
//     連合の activity (inbox) は、戻した後も当てる価値がある。消えた行を指す
//     job (relationship / export など) は失敗して捨てられるが、戻した DB にも行が
//     ある操作 (待っていたフォロー解除など) は、戻した DB に当たる
//
// -mode empty (版上げ・引っ越し) では、CLI は既定でこの後始末を呼ばない
// (internal/cli/backup の cleanRedisFor)。
//   - bull:deliver:meta / bull:deliver:repeat: 管理画面の一時停止 (meta の paused、
//     #2072) と定期 job の予定。消すと一時停止が黙って外れる
//   - pubsub: channel だけで、key を持たない

// deliverQueuePattern is cleared on the job queue client: the jobs waiting
// to deliver activities to other servers.
//
// **配送の job は捨てる。** 残っている配送は、ほとんどがバックアップより後の
// 投稿・リアクション・フォローなどの activity で、戻した DB にはその行が無い。
// 配ると、連合の相手にだけ存在する投稿 (こちらで取り消しの Delete を出せない)
// が増える。バックアップより前の activity の再試行も一緒に消えるが、失うのは
// 一部の相手への届き遅れで、幽霊を増やすより害が小さい。
const deliverQueuePattern = "bull:deliver:*"

// deliverQueueKept are the keys under deliverQueuePattern that are kept.
var deliverQueueKept = map[string]bool{"bull:deliver:meta": true, "bull:deliver:repeat": true}

// protectedKeyPrefixes are never deleted, whatever the patterns match.
// パターンを足したときに、消してはいけない記録を巻き込まないための守り。
var protectedKeyPrefixes = []string{"passwordguard:", "mk:2fa:totp:used:", "mk:ap:inbox:seen:", "mk:signupform:nonce:"}

// protectedKey reports whether key must survive a restore cleanup. prefix
// is the default client's key prefix.
func protectedKey(key, prefix string) bool {
	if strings.HasPrefix(key, prefix+"elythia:maintenance") || strings.HasPrefix(key, "elythia:maintenance") {
		return true
	}
	for _, p := range protectedKeyPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// RedisCleanupResult counts the deleted keys per pattern.
type RedisCleanupResult struct {
	Deleted map[string]int64
}

// Total is the number of deleted keys.
func (r RedisCleanupResult) Total() int64 {
	var n int64
	for _, v := range r.Deleted {
		n += v
	}
	return n
}

// CleanRedisAfterRestore deletes the Redis keys that point at database rows
// newer than the restored backup. Run it with the server stopped.
//
// **FLUSHDB は使わない。** 既定の構成では 5 つの用途が同じ Redis の同じ DB を
// 共有しているので、job queue と、再送・総当たりを止める記録まで消える。
func CleanRedisAfterRestore(ctx context.Context, t RedisCleanupTargets) (RedisCleanupResult, error) {
	res := RedisCleanupResult{Deleted: map[string]int64{}}
	type job struct {
		target  RedisCleanupTarget
		pattern string
		keep    map[string]bool
	}
	var jobs []job
	for _, p := range timelinePatterns {
		jobs = append(jobs, job{target: t.Timelines, pattern: escapeGlob(t.Timelines.Prefix) + p})
	}
	for _, p := range defaultPrefixedPatterns {
		jobs = append(jobs, job{target: t.Default, pattern: escapeGlob(t.Default.Prefix) + p})
	}
	for _, p := range defaultPatterns {
		jobs = append(jobs, job{target: t.Default, pattern: p})
	}
	jobs = append(jobs, job{target: t.JobQueue, pattern: deliverQueuePattern, keep: deliverQueueKept})
	for _, j := range jobs {
		if j.target.Client == nil {
			continue
		}
		keep := func(key string) bool { return j.keep[key] || protectedKey(key, t.Default.Prefix) }
		n, err := deleteMatching(ctx, j.target.Client, j.pattern, keep)
		res.Deleted[j.pattern] += n
		if err != nil {
			return res, fmt.Errorf("backup: delete redis keys %q: %w", j.pattern, err)
		}
	}
	return res, nil
}

// deleteMatching deletes every key matching pattern, except those keep
// accepts, with SCAN + UNLINK. SCAN は KEYS と違って Redis を止めない。
func deleteMatching(ctx context.Context, c *redis.Client, pattern string, keep func(string) bool) (int64, error) {
	var deleted int64
	var cursor uint64
	for {
		keys, next, err := c.Scan(ctx, cursor, pattern, 1000).Result()
		if err != nil {
			return deleted, err
		}
		keys = slices.DeleteFunc(keys, keep)
		if len(keys) > 0 {
			n, err := c.Unlink(ctx, keys...).Result()
			deleted += n
			if err != nil {
				return deleted, err
			}
		}
		if next == 0 {
			return deleted, nil
		}
		cursor = next
	}
}

// escapeGlob escapes the glob characters of a Redis MATCH pattern.
func escapeGlob(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch c {
		case '\\', '*', '?', '[', ']':
			b.WriteRune('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}
