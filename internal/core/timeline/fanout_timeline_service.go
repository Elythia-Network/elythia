// Package timeline provides Redis-backed timeline services.
//
// FanoutTimelineService maintains per-user/per-channel timeline lists in
// Redis. The model mirrors Misskey本家のFanoutTimelineService:
//   - 各タイムラインは "list:<name>" キーで管理されるRedisリスト
//   - LPUSHで新しいIDを先頭に追加
//   - LRANGEで取得し、since/untilでクライアント側フィルタ
package timeline

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/redis/go-redis/v9"
)

// Name represents a timeline key name.
// 例: "homeTimeline:USER_ID", "localTimeline", "userTimeline:USER_ID"
type Name string

// Well-known timeline name constructors. ローカル/グローバル/ホーム/ユーザーの
// 4種類はPhase 2 Step Eで実装。Misskey本家のホームタイムラインsuffixをそのまま流用。
const (
	LocalTimeline Name = "localTimeline"
	// LocalTimelineWithReplies は「自分以外への返信」だけを積む LTL 系列。
	// upstream は LTL を返信の有無で 3 本に分けており、素の localTimeline には
	// 返信を入れない。withReplies=true の取得時にこちらを合流させる。
	LocalTimelineWithReplies Name = "localTimelineWithReplies"
	GlobalTimeline           Name = "globalTimeline"
	timelineKeyFmt                = "list:%s"
	defaultMaxLen                 = 200
	// trimProbability defines how often we trim a timeline list to maxLen.
	// 全プッシュごとにLTRIMすると重いので、確率的に間引く。
	trimProbability = 0.1
	// recentInsertGracePeriod is how recent a note can be to skip the lindex check.
	recentInsertGracePeriod = 3 * time.Minute
)

// UserTimelineWithRepliesName returns the user timeline key that holds the
// user's replies to others.
//
// upstream は userTimeline に「非返信 + 自己スレッド」だけを積み、他人宛ての
// 返信は userTimelineWithReplies に分ける。withReplies=true の取得時に合流。
func UserTimelineWithRepliesName(userID string) Name {
	return Name("userTimelineWithReplies:" + userID)
}

// LocalTimelineWithReplyToName returns the LTL key holding replies addressed to
// the given user.
//
// upstream は「自分宛ての返信」だけを別キーに積み、取得時に localTimeline と
// 合流させる。これにより「他人の他人への返信は出ない」「自分宛ての返信は出る」
// を両立させている。
func LocalTimelineWithReplyToName(userID string) Name {
	return Name("localTimelineWithReplyTo:" + userID)
}

// HomeTimelineName returns the home timeline list key for a user.
func HomeTimelineName(userID string) Name {
	return Name("homeTimeline:" + userID)
}

// UserTimelineName returns the per-user timeline list key.
func UserTimelineName(userID string) Name {
	return Name("userTimeline:" + userID)
}

// UserListTimelineName returns the user-list timeline list key.
func UserListTimelineName(listID string) Name {
	return Name("userListTimeline:" + listID)
}

// FanoutTimelineService manages Redis-backed timeline lists.
type FanoutTimelineService struct {
	client    *redis.Client
	idGen     id.Generator
	keyPrefix string // TS drop-in互換用 `<host>:` prefix。空なら従来通り。
	// nowFn / randFn allow tests to inject deterministic values.
	nowFn  func() time.Time
	randFn func() float64
}

// NewFanoutTimelineService creates a new FanoutTimelineService bound to the
// timelines Redis database. idGen is used to extract a note's creation time
// from its ID for the late-insert ordering check.
//
// keyPrefix (通常は `cfg.Redis.KeyPrefix()` = `<host>:`) は TS 本家と同じキー
// 名前空間 (`<host>:list:homeTimeline:<userId>` 等) を使うために全キーの前に
// 付与される。空文字列を渡すとprefix無しになり、従来の mk-only 挙動に戻る。
func NewFanoutTimelineService(client *redis.Client, idGen id.Generator, keyPrefix string) *FanoutTimelineService {
	return &FanoutTimelineService{
		client:    client,
		idGen:     idGen,
		keyPrefix: keyPrefix,
		nowFn:     time.Now,
		randFn:    rand.Float64,
	}
}

// key returns the Redis key for a timeline name, with TS-compatible prefix.
func (s *FanoutTimelineService) key(name Name) string {
	return s.keyPrefix + fmt.Sprintf(timelineKeyFmt, name)
}

// Push appends id to the named timeline. maxLen caps the list size; values
// older than recentInsertGracePeriod fall back to a tail-comparison append.
//
// 戻り値はRedisエラー (nil if no-op or success)。
func (s *FanoutTimelineService) Push(ctx context.Context, name Name, noteID string, maxLen int) error {
	if maxLen <= 0 {
		maxLen = defaultMaxLen
	}
	noteTime, err := s.idGen.ParseTime(noteID)
	if err != nil {
		// 不正なIDはタイムラインに入れない (Misskey本家のidServiceでも例外を出す)
		return fmt.Errorf("invalid note id %q: %w", noteID, err)
	}

	key := s.key(name)
	// 直近 (3分以内) のノートはそのまま先頭に追加して、確率でtrim
	if s.nowFn().Sub(noteTime) <= recentInsertGracePeriod {
		if err := s.client.LPush(ctx, key, noteID).Err(); err != nil {
			return err
		}
		if s.randFn() < trimProbability {
			// LTrimの失敗はベストエフォートで握り潰す。
			// LPushは既に成功しており、サイズ制限はあくまで確率的なソフトリミット。
			_ = s.client.LTrim(ctx, key, 0, int64(maxLen-1)).Err()
		}
		return nil
	}

	// 古いノート: 既存末尾IDより新しい場合のみ追加
	tail, err := s.client.LIndex(ctx, key, -1).Result()
	if err == redis.Nil {
		// 空のタイムラインなら追加してOK
		return s.client.LPush(ctx, key, noteID).Err()
	}
	if err != nil {
		return err
	}
	tailTime, err := s.idGen.ParseTime(tail)
	if err != nil {
		// 末尾の値が壊れているなら諦めて追加する
		return s.client.LPush(ctx, key, noteID).Err()
	}
	if noteTime.After(tailTime) {
		return s.client.LPush(ctx, key, noteID).Err()
	}
	// より古いノートはスキップ
	return nil
}

// Get returns IDs from the timeline filtered by the given since/until window.
// 結果の向きは cursor に従う (sinceId 単独なら ASC、それ以外は id 降順)。limit<=0 のときは全件返す。
func (s *FanoutTimelineService) Get(ctx context.Context, name Name, untilID, sinceID string, limit int) ([]string, error) {
	ids, err := s.client.LRange(ctx, s.key(name), 0, -1).Result()
	if err != nil {
		return nil, err
	}
	return filterAndSort(ids, untilID, sinceID, limit), nil
}

// GetMerged retrieves IDs from multiple timelines and merges them into one
// de-duplicated list, ordered by the cursor direction.
//
// upstream FanoutTimelineEndpointService は redisTimelines に複数キーを渡し、
// 取得結果をマージして 1 本の timeline として返す。LTL の返信振り分け
// (localTimeline / WithReplies / WithReplyTo) がこれに当たる。
func (s *FanoutTimelineService) GetMerged(ctx context.Context, names []Name, untilID, sinceID string, limit int) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if len(names) == 1 {
		return s.Get(ctx, names[0], untilID, sinceID, limit)
	}
	lists, err := s.GetMulti(ctx, names, untilID, sinceID, limit)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	merged := make([]string, 0, limit)
	for _, ids := range lists {
		for _, id := range ids {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			merged = append(merged, id)
		}
	}
	// 向きは cursor に従う (#2720)。Get / GetMulti が per-key で ASC を返しても、
	// ここで降順に並べ直すと昇順ページングで最新 N 件を切り出してしまう。
	// LocalTimeline はログイン中だとキーが 2 本になるので、この分岐を落とすと
	// **ログインの有無で向きが変わる**。
	if isAscending(sinceID, untilID) {
		sort.Strings(merged)
	} else {
		sort.Sort(sort.Reverse(sort.StringSlice(merged)))
	}
	// limit<=0 は Get / GetMulti と同じく全件 (timeline service が窓全体を
	// 1 回で読むときに使う、#3448)。
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

// GetMulti retrieves IDs from multiple timelines in a single pipeline call.
// 戻り値の順序はnamesと同じ。
func (s *FanoutTimelineService) GetMulti(ctx context.Context, names []Name, untilID, sinceID string, limit int) ([][]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	pipe := s.client.Pipeline()
	cmds := make([]*redis.StringSliceCmd, 0, len(names))
	for _, n := range names {
		cmds = append(cmds, pipe.LRange(ctx, s.key(n), 0, -1))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	out := make([][]string, 0, len(cmds))
	for _, cmd := range cmds {
		// pipe.Exec が成功した時点で個々の cmd.Result() は基本的にエラーを返さない。
		// 万一エラーを返した場合は空スライスとして扱う。
		ids, _ := cmd.Result()
		out = append(out, filterAndSort(ids, untilID, sinceID, limit))
	}
	return out, nil
}

// Purge removes the named timeline list.
func (s *FanoutTimelineService) Purge(ctx context.Context, name Name) error {
	return s.client.Del(ctx, s.key(name)).Err()
}

// Remove drops every occurrence of noteID from the named timeline list (#379)。
// Misskey TS の `RedisTimelineService.removeFromTimeline` 相当。Delete activity
// 受信時 / ローカル削除時に各 fanout 先 (home/local/global/user/userList) から
// 該当 ID を消すために使う。
//
// LREM count=0 は全 occurrence を削除する。空 list / 不在 ID でもエラーには
// ならず削除件数 0 が返るだけなので呼び出し側で気にする必要はない。
func (s *FanoutTimelineService) Remove(ctx context.Context, name Name, noteID string) error {
	return s.client.LRem(ctx, s.key(name), 0, noteID).Err()
}

// RemoveMany drops every occurrence of each noteID from the named timelines in
// one round trip per call (#2715)。
//
// **読み取り経路から呼ぶので pipeline にする。** 解決できない ID は 1 つの list に
// 数百単位で溜まりうる (実測: home timeline の ID の約半分)。1 件ずつ Remove すると
// その数だけ往復するので、timeline の応答時間に直接乗る。
//
// **error は返すが、握り潰すかは呼び出し側が決める。** Remove と非対称にしない
// (#2718 review LOW-6)。timeline の自己修復は失敗しても読み取りを壊さないので
// 呼び出し側が捨てる。
func (s *FanoutTimelineService) RemoveMany(ctx context.Context, names []Name, noteIDs []string) error {
	if s == nil || s.client == nil || len(names) == 0 || len(noteIDs) == 0 {
		return nil
	}
	pipe := s.client.Pipeline()
	for _, name := range names {
		key := s.key(name)
		for _, id := range noteIDs {
			pipe.LRem(ctx, key, 0, id)
		}
	}
	_, err := pipe.Exec(ctx)
	return err
}

// filterAndSort applies the since/until filter to a slice of IDs and returns
// them sorted by the cursor direction (ascending for a sinceId-only cursor), capped to limit if positive.
func filterAndSort(ids []string, untilID, sinceID string, limit int) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if untilID != "" && id >= untilID {
			continue
		}
		if sinceID != "" && id <= sinceID {
			continue
		}
		out = append(out, id)
	}
	// **sinceId 単独指定は昇順ページング** (#2720)。upstream
	// FanoutTimelineEndpointService の `ascending = ps.sinceId && !ps.untilId`
	// と同じ規則で、ASC に並べて**最古 N 件**を返す。
	//
	// 以前は常に DESC で先頭 limit 件を返していたため、cursor の直後ではなく
	// **最新 N 件**が返っていた。
	//
	// **ただし endpoint 経路からこの昇順分岐には到達しない。** timeline service の
	// shouldFallbackToDB が sinceId 付きを全て DB へ倒すため (upstream も同じ)。
	// それでも upstream の get と同じ形にしてあるのは、判定を緩めたときに
	// ここが正しくないと一斉に壊れるため。ユニットテストで固定してある。
	if isAscending(sinceID, untilID) {
		sort.Strings(out)
	} else {
		sort.Sort(sort.Reverse(sort.StringSlice(out)))
	}
	// **upstream は窓を切らない。** `FanoutTimelineService.getMulti` が
	// `lrange 0 -1` で list 全体を返し、`FanoutTimelineEndpointService` 側の
	// while ループが「limit 件が埋まるまで窓の奥へ読み進めながら hydrate する」
	// 形になっている。timeline service もこれに合わせ、limit<=0 で窓全体を
	// 1 回だけ読み、メモリ上で切り出す (readFanout、#3448)。窓の奥を読み進めるのは
	// DB fallback を切っているときだけ (docs/divergence/operations.md §5.6)。
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// isAscending reports whether the cursor pair means ascending pagination.
//
// upstream `makePaginationQuery` / FanoutTimelineEndpointService と同じ規則で、
// **sinceId があって untilId が無いときだけ**昇順。
func isAscending(sinceID, untilID string) bool {
	return sinceID != "" && untilID == ""
}
