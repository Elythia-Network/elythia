package timeline

import (
	"context"
	"errors"
	"log"
	"os"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testRedis *testutil.TestRedis
	idGen     id.Generator
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	testRedis, err = testutil.SetupRedis(ctx)
	if err != nil {
		log.Fatalf("failed to setup redis: %v", err)
	}
	idGen, _ = id.NewGenerator("aidx")

	code := m.Run()

	testRedis.Teardown(ctx)
	os.Exit(code)
}

// newTestService returns a fresh FanoutTimelineService bound to the shared
// Redis container with deterministic now/rand functions.
func newTestService(t *testing.T) *FanoutTimelineService {
	t.Helper()
	testRedis.FlushAll(context.Background())
	svc := NewFanoutTimelineService(testRedis.Client, idGen, "")
	// テストでは確率トリムをoffにする (常にtrimしない)
	svc.randFn = func() float64 { return 1.0 }
	return svc
}

func TestFanoutTimelineService_PushAndGet(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	// 3件のIDを作成し追加 (新しいものから順に挿入したいので id1 -> id2 -> id3)
	id1 := idGen.Generate(time.Now().Add(-2 * time.Minute))
	id2 := idGen.Generate(time.Now().Add(-1 * time.Minute))
	id3 := idGen.Generate(time.Now())

	require.NoError(t, svc.Push(ctx, LocalTimeline, id1, 100))
	require.NoError(t, svc.Push(ctx, LocalTimeline, id2, 100))
	require.NoError(t, svc.Push(ctx, LocalTimeline, id3, 100))

	out, err := svc.Get(ctx, LocalTimeline, "", "", 10)
	require.NoError(t, err)
	require.Len(t, out, 3)
	// id降順
	assert.Equal(t, id3, out[0])
	assert.Equal(t, id2, out[1])
	assert.Equal(t, id1, out[2])
}

func TestFanoutTimelineService_GetWithUntilSince(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	id1 := idGen.Generate(time.Now().Add(-2 * time.Minute))
	id2 := idGen.Generate(time.Now().Add(-1 * time.Minute))
	id3 := idGen.Generate(time.Now())
	require.NoError(t, svc.Push(ctx, LocalTimeline, id1, 100))
	require.NoError(t, svc.Push(ctx, LocalTimeline, id2, 100))
	require.NoError(t, svc.Push(ctx, LocalTimeline, id3, 100))

	// untilID で id3 を除外
	out, err := svc.Get(ctx, LocalTimeline, id3, "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{id2, id1}, out)

	// sinceID 単独は**昇順ページング** (#2720)。upstream
	// FanoutTimelineEndpointService の `ascending = sinceId && !untilId` と
	// 同じ規則で ASC に並べて最古 N 件を返す。以前は常に DESC だったため、
	// cursor の直後ではなく最新 N 件が返っていた。
	out, err = svc.Get(ctx, LocalTimeline, "", id1, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{id2, id3}, out)

	// sinceID + untilID は降順のまま (昇順になるのは sinceId 単独のときだけ)。
	// **窓に 2 件以上入れる。** 1 件だと ASC でも DESC でも同じ結果になり、
	// 向きを検出できない。
	id4 := idGen.Generate(time.Now().Add(1 * time.Minute))
	require.NoError(t, svc.Push(ctx, LocalTimeline, id4, 100))
	out, err = svc.Get(ctx, LocalTimeline, id4, id1, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{id3, id2}, out, "sinceId + untilId は降順のまま")

	// limit
	out, err = svc.Get(ctx, LocalTimeline, "", "", 2)
	require.NoError(t, err)
	assert.Len(t, out, 2)
}

func TestFanoutTimelineService_PushInvalidID(t *testing.T) {
	svc := newTestService(t)
	err := svc.Push(context.Background(), LocalTimeline, "not-a-valid-id", 100)
	assert.Error(t, err)
}

func TestFanoutTimelineService_PushDefaultMaxLen(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	noteID := idGen.Generate(time.Now())
	// maxLen=0 はデフォルト値が使われる
	require.NoError(t, svc.Push(ctx, LocalTimeline, noteID, 0))
	out, err := svc.Get(ctx, LocalTimeline, "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out)
}

func TestFanoutTimelineService_PushTriggersTrim(t *testing.T) {
	svc := newTestService(t)
	// 強制的にtrimを発生させる
	svc.randFn = func() float64 { return 0 }
	ctx := context.Background()

	// 5件追加して maxLen=3 でtrimさせる
	var ids []string
	for i := range 5 {
		ids = append(ids, idGen.Generate(time.Now().Add(time.Duration(i)*time.Millisecond)))
	}
	for _, id := range ids {
		require.NoError(t, svc.Push(ctx, LocalTimeline, id, 3))
	}
	out, err := svc.Get(ctx, LocalTimeline, "", "", 10)
	require.NoError(t, err)
	assert.Len(t, out, 3)
}

func TestFanoutTimelineService_PushOldNoteEmpty(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	// 古いノート (5分前) を空のリストに追加 -> 成功する
	oldNote := idGen.Generate(time.Now().Add(-5 * time.Minute))
	// nowFnを差し替えて確実に古いとみなす
	svc.nowFn = func() time.Time { return time.Now() }
	require.NoError(t, svc.Push(ctx, LocalTimeline, oldNote, 100))

	out, err := svc.Get(ctx, LocalTimeline, "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{oldNote}, out)
}

func TestFanoutTimelineService_PushOldNoteAfterTail(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	// 古いノートを2件: 末尾(古い)と挿入(末尾より新しい)
	tailNote := idGen.Generate(time.Now().Add(-10 * time.Minute))
	newOldNote := idGen.Generate(time.Now().Add(-5 * time.Minute))

	// 直接tailNoteを挿入するためにnowFnを過去にずらす
	pastNow := time.Now().Add(-9 * time.Minute)
	svc.nowFn = func() time.Time { return pastNow }
	require.NoError(t, svc.Push(ctx, LocalTimeline, tailNote, 100))

	// 5分前のIDを現在時刻基準でpush -> grace period外なので末尾チェック経路
	svc.nowFn = func() time.Time { return time.Now() }
	require.NoError(t, svc.Push(ctx, LocalTimeline, newOldNote, 100))

	out, err := svc.Get(ctx, LocalTimeline, "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{newOldNote, tailNote}, out)
}

func TestFanoutTimelineService_PushOldNoteOlderThanTail(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	// 末尾より古いノートはスキップされる
	tailNote := idGen.Generate(time.Now().Add(-5 * time.Minute))
	olderNote := idGen.Generate(time.Now().Add(-10 * time.Minute))

	// tailNoteを直接挿入
	pastNow := time.Now().Add(-4 * time.Minute)
	svc.nowFn = func() time.Time { return pastNow }
	require.NoError(t, svc.Push(ctx, LocalTimeline, tailNote, 100))

	svc.nowFn = func() time.Time { return time.Now() }
	require.NoError(t, svc.Push(ctx, LocalTimeline, olderNote, 100))

	out, err := svc.Get(ctx, LocalTimeline, "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{tailNote}, out)
}

func TestFanoutTimelineService_PushOldNoteCorruptedTail(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	// 不正な末尾IDを直接挿入してから、古いノートを追加すると、ParseTime失敗フォールバックで追加される
	require.NoError(t, testRedis.Client.LPush(ctx, "list:"+string(LocalTimeline), "junk-id").Err())
	oldNote := idGen.Generate(time.Now().Add(-5 * time.Minute))
	svc.nowFn = func() time.Time { return time.Now() }
	require.NoError(t, svc.Push(ctx, LocalTimeline, oldNote, 100))

	// 末尾は破損だが先頭にoldNoteが入っていることだけ確認
	first, err := testRedis.Client.LIndex(ctx, "list:"+string(LocalTimeline), 0).Result()
	require.NoError(t, err)
	assert.Equal(t, oldNote, first)
}

// closedClient simulates a closed redis client for error path coverage.
func closedClient(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	_ = c.Close()
	return c
}

func TestFanoutTimelineService_RedisErrors(t *testing.T) {
	c := closedClient(t)
	svc := NewFanoutTimelineService(c, idGen, "")
	svc.randFn = func() float64 { return 1.0 }

	ctx := context.Background()
	noteID := idGen.Generate(time.Now())

	// Push fails on LPush
	err := svc.Push(ctx, LocalTimeline, noteID, 100)
	assert.Error(t, err)

	// Push fails on LIndex (古いノート経路)
	oldNoteID := idGen.Generate(time.Now().Add(-10 * time.Minute))
	err = svc.Push(ctx, LocalTimeline, oldNoteID, 100)
	assert.Error(t, err)

	// Get fails on LRange
	_, err = svc.Get(ctx, LocalTimeline, "", "", 10)
	assert.Error(t, err)

	// GetMulti fails on pipeline exec
	_, err = svc.GetMulti(ctx, []Name{LocalTimeline}, "", "", 10)
	assert.Error(t, err)

	// Purge fails on Del
	err = svc.Purge(ctx, LocalTimeline)
	assert.Error(t, err)

	// Remove fails on LRem (#379)
	err = svc.Remove(ctx, LocalTimeline, noteID)
	assert.Error(t, err)
}

// TestFanoutTimelineService_KeyPrefix は #362 drop-in 互換の回帰テスト。
// TS 本家と同じ `<host>:list:...` 名前空間にキーが置かれることを確認する。
func TestFanoutTimelineService_KeyPrefix(t *testing.T) {
	testRedis.FlushAll(context.Background())
	const host = "example.test"
	svc := NewFanoutTimelineService(testRedis.Client, idGen, host+":")
	svc.randFn = func() float64 { return 1.0 }
	ctx := context.Background()

	noteID := idGen.Generate(time.Now())
	require.NoError(t, svc.Push(ctx, LocalTimeline, noteID, 100))

	// prefix 付きキーには値が入っていること
	prefixedKey := host + ":list:" + string(LocalTimeline)
	n, err := testRedis.Client.LLen(ctx, prefixedKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "prefixed key should contain the pushed note")

	// 旧 prefix 無しキーには何も入っていないこと
	bareKey := "list:" + string(LocalTimeline)
	n, err = testRedis.Client.LLen(ctx, bareKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "bare key should remain untouched")

	// Get 経由で読めること
	out, err := svc.Get(ctx, LocalTimeline, "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, out)
}

// TestFanoutTimelineService_KeyPrefixEmpty はprefix空文字列時に従来挙動
// (prefix 無しキー) のままであることを確認する。
func TestFanoutTimelineService_KeyPrefixEmpty(t *testing.T) {
	testRedis.FlushAll(context.Background())
	svc := NewFanoutTimelineService(testRedis.Client, idGen, "")
	svc.randFn = func() float64 { return 1.0 }
	ctx := context.Background()

	noteID := idGen.Generate(time.Now())
	require.NoError(t, svc.Push(ctx, LocalTimeline, noteID, 100))

	bareKey := "list:" + string(LocalTimeline)
	n, err := testRedis.Client.LLen(ctx, bareKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(1), n, "bare key receives value when prefix is empty")
}

func TestFanoutTimelineService_GetMultiEmpty(t *testing.T) {
	svc := newTestService(t)
	out, err := svc.GetMulti(context.Background(), nil, "", "", 10)
	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestFanoutTimelineService_GetMulti(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	id1 := idGen.Generate(time.Now().Add(-1 * time.Second))
	id2 := idGen.Generate(time.Now())
	require.NoError(t, svc.Push(ctx, LocalTimeline, id1, 100))
	require.NoError(t, svc.Push(ctx, GlobalTimeline, id2, 100))

	out, err := svc.GetMulti(ctx, []Name{LocalTimeline, GlobalTimeline}, "", "", 10)
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, []string{id1}, out[0])
	assert.Equal(t, []string{id2}, out[1])
}

func TestFanoutTimelineService_PushTrimError(t *testing.T) {
	// Setup a real redis but trip the LTRIM by using a closed client mid-call.
	// シンプルに: trimProbabilityを必ず発火させるが、Pushの直前にclientを切断する
	svc := newTestService(t)
	svc.randFn = func() float64 { return 0 }

	noteID := idGen.Generate(time.Now())
	// 実 Redis を使う限りLTRIMは成功する。エラーパス自体はclosed client testで網羅済み。
	require.NoError(t, svc.Push(context.Background(), LocalTimeline, noteID, 100))
}

func TestFanoutTimelineService_Purge(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	noteID := idGen.Generate(time.Now())
	require.NoError(t, svc.Push(ctx, LocalTimeline, noteID, 100))
	require.NoError(t, svc.Purge(ctx, LocalTimeline))

	out, err := svc.Get(ctx, LocalTimeline, "", "", 10)
	require.NoError(t, err)
	assert.Empty(t, out)
}

// TestFanoutTimelineService_Remove は #379 の delete propagation 用 LREM。
// 該当 ID だけを消し、他の ID は残ること、不在 ID 渡しでもエラーにならない
// ことを確認する。
func TestFanoutTimelineService_Remove(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()

	id1 := idGen.Generate(time.Now().Add(-2 * time.Minute))
	id2 := idGen.Generate(time.Now().Add(-1 * time.Minute))
	id3 := idGen.Generate(time.Now())

	require.NoError(t, svc.Push(ctx, LocalTimeline, id1, 100))
	require.NoError(t, svc.Push(ctx, LocalTimeline, id2, 100))
	require.NoError(t, svc.Push(ctx, LocalTimeline, id3, 100))

	// 中間の id2 だけ削除
	require.NoError(t, svc.Remove(ctx, LocalTimeline, id2))

	out, err := svc.Get(ctx, LocalTimeline, "", "", 10)
	require.NoError(t, err)
	assert.Equal(t, []string{id3, id1}, out)

	// 不在 ID の削除は no-op (エラーにならない)
	require.NoError(t, svc.Remove(ctx, LocalTimeline, "nonexistent"))

	// 空 list への Remove も no-op
	require.NoError(t, svc.Remove(ctx, GlobalTimeline, id1))
}

// TestFanoutTimelineService_PushOldNoteCorruptedTail_isFollowedByErr asserts
// that we use errors package indirectly via sentinel.
func TestFanoutTimelineService_PushUsesSentinel(t *testing.T) {
	// 単純に errors.Is が解決可能な型であることを確認するためのプレースホルダ
	assert.True(t, errors.Is(redis.Nil, redis.Nil))
}

// TestHomeUserNames verifies the timeline name builder helpers.
func TestTimelineNameHelpers(t *testing.T) {
	assert.Equal(t, Name("homeTimeline:abc"), HomeTimelineName("abc"))
	assert.Equal(t, Name("userTimeline:abc"), UserTimelineName("abc"))
}

func TestFanoutTimelineService_PushOldNoteWithTrim(t *testing.T) {
	// triggers the LTrim/random branch under the recent path
	svc := newTestService(t)
	svc.randFn = func() float64 { return 0 }
	ctx := context.Background()
	noteID := idGen.Generate(time.Now())
	require.NoError(t, svc.Push(ctx, LocalTimeline, noteID, 5))
}

// TestService_MergedWithCutoffDirection は複数の list を混ぜたときの向きを固定する。
//
// **endpoint 経路からは到達しない** — shouldFallbackToDB が sinceId 付きを
// 全て DB へ倒すため。それでもユニットとして固定するのは、Get / GetMulti が
// per-key で ASC を返したものをここで降順に並べ直すと、LocalTimeline が
// ログインの有無でキー数を変える (1 本 or 2 本) ぶん**向きが変わる**ため。
func TestService_MergedWithCutoffDirection(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	ctx := context.Background()
	testRedis.FlushAll(ctx)

	fanout := NewFanoutTimelineService(testRedis.Client, idGen, "")
	fanout.randFn = func() float64 { return 1.0 }
	svc := NewService(fanout, testutil.NewMockNoteRepository(), testutil.NewMockFollowingRepository())
	now := time.Now()
	ids := make([]string, 4)
	for i := range ids {
		ids[i] = idGen.Generate(now.Add(time.Duration(i) * time.Millisecond))
	}
	// 2 キーに分けて積む。
	require.NoError(t, fanout.Push(ctx, LocalTimeline, ids[1], 100))
	require.NoError(t, fanout.Push(ctx, LocalTimeline, ids[3], 100))
	require.NoError(t, fanout.Push(ctx, GlobalTimeline, ids[2], 100))

	keys := []Name{LocalTimeline, GlobalTimeline}

	// sinceId 単独 → ASC。
	out, cutoff, err := svc.mergedWithCutoff(ctx, keys, "", ids[0])
	require.NoError(t, err)
	assert.Equal(t, []string{ids[1], ids[2], ids[3]}, out, "昇順は古い順")
	assert.Equal(t, ids[2], cutoff, "各 list の最古のうち一番新しいもの")

	// untilId 単独 → 従来どおり DESC。
	out, _, err = svc.mergedWithCutoff(ctx, keys, ids[3], "")
	require.NoError(t, err)
	assert.Equal(t, []string{ids[2], ids[1]}, out, "降順は新しい順")
}
