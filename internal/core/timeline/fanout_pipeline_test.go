package timeline

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFanoutTimelineService_PushMany_RecentPushesEveryList(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	older := idGen.Generate(time.Now().Add(-time.Minute))
	require.NoError(t, svc.Push(ctx, HomeTimelineName("a"), older, 100))

	noteID := idGen.Generate(time.Now())
	names := []Name{HomeTimelineName("a"), HomeTimelineName("b"), HomeTimelineName("c")}
	assert.Nil(t, svc.PushMany(ctx, names, noteID, 100))

	raw, err := testRedis.Client.LRange(ctx, "list:homeTimeline:a", 0, -1).Result()
	require.NoError(t, err)
	assert.Equal(t, []string{noteID, older}, raw, "the new id goes on the head, as LPUSH")
	for _, u := range []string{"b", "c"} {
		raw, err := testRedis.Client.LRange(ctx, "list:homeTimeline:"+u, 0, -1).Result()
		require.NoError(t, err)
		assert.Equal(t, []string{noteID}, raw, "list of %s", u)
	}
}

func TestFanoutTimelineService_PushMany_UsesKeyPrefix(t *testing.T) {
	testRedis.FlushAll(context.Background())
	svc := NewFanoutTimelineService(testRedis.Client, idGen, "host.example:")
	svc.randFn = func() float64 { return 1.0 }
	ctx := context.Background()
	noteID := idGen.Generate(time.Now())
	assert.Nil(t, svc.PushMany(ctx, []Name{HomeTimelineName("a")}, noteID, 100))
	raw, err := testRedis.Client.LRange(ctx, "host.example:list:homeTimeline:a", 0, -1).Result()
	require.NoError(t, err)
	assert.Equal(t, []string{noteID}, raw)
}

// LTRIM は Push と同じく、宛先ごとに確率で入る。
func TestFanoutTimelineService_PushMany_TrimsByProbability(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		id := idGen.Generate(time.Now().Add(time.Duration(i-10) * time.Second))
		require.NoError(t, svc.Push(ctx, HomeTimelineName("a"), id, 100))
		require.NoError(t, svc.Push(ctx, HomeTimelineName("b"), id, 100))
	}
	// a には trim を入れ、b には入れない。
	draws := []float64{0.0, 1.0}
	svc.randFn = func() float64 {
		v := draws[0]
		draws = draws[1:]
		return v
	}
	noteID := idGen.Generate(time.Now())
	assert.Nil(t, svc.PushMany(ctx, []Name{HomeTimelineName("a"), HomeTimelineName("b")}, noteID, 3))
	assert.Empty(t, draws, "one draw per timeline")

	lenA, err := testRedis.Client.LLen(ctx, "list:homeTimeline:a").Result()
	require.NoError(t, err)
	lenB, err := testRedis.Client.LLen(ctx, "list:homeTimeline:b").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(3), lenA, "trimmed to maxLen")
	assert.Equal(t, int64(6), lenB, "not trimmed")
}

// pipeline の中の 1 コマンドの失敗は、その宛先だけの失敗になる。
func TestFanoutTimelineService_PushMany_FailureIsPerTimeline(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	require.NoError(t, testRedis.Client.Set(ctx, "list:homeTimeline:broken", "x", 0).Err())

	noteID := idGen.Generate(time.Now())
	errs := svc.PushMany(ctx, []Name{HomeTimelineName("a"), HomeTimelineName("broken"), HomeTimelineName("c")}, noteID, 100)
	require.Len(t, errs, 3)
	assert.NoError(t, errs[0])
	assert.Error(t, errs[1])
	assert.NoError(t, errs[2])
	for _, u := range []string{"a", "c"} {
		raw, err := testRedis.Client.LRange(ctx, "list:homeTimeline:"+u, 0, -1).Result()
		require.NoError(t, err)
		assert.Equal(t, []string{noteID}, raw, "list of %s", u)
	}
}

// 猶予より古いノートは、Push と同じく末尾と比べてから入れる。
func TestFanoutTimelineService_PushMany_OldNoteComparesWithTail(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	newer := idGen.Generate(time.Now().Add(-5 * time.Minute))
	require.NoError(t, testRedis.Client.LPush(ctx, "list:homeTimeline:a", newer).Err())

	old := idGen.Generate(time.Now().Add(-10 * time.Minute))
	assert.Nil(t, svc.PushMany(ctx, []Name{HomeTimelineName("a"), HomeTimelineName("empty")}, old, 100))

	raw, err := testRedis.Client.LRange(ctx, "list:homeTimeline:a", 0, -1).Result()
	require.NoError(t, err)
	assert.Equal(t, []string{newer}, raw, "older than the tail: skipped")
	raw, err = testRedis.Client.LRange(ctx, "list:homeTimeline:empty", 0, -1).Result()
	require.NoError(t, err)
	assert.Equal(t, []string{old}, raw, "empty list: pushed")
}

func TestFanoutTimelineService_PushMany_InvalidIDAndEmpty(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	assert.Nil(t, svc.PushMany(ctx, nil, idGen.Generate(time.Now()), 100))

	errs := svc.PushMany(ctx, []Name{HomeTimelineName("a"), HomeTimelineName("b")}, "not-an-id", 100)
	require.Len(t, errs, 2)
	assert.Error(t, errs[0])
	assert.Error(t, errs[1])
	n, err := testRedis.Client.Exists(ctx, "list:homeTimeline:a", "list:homeTimeline:b").Result()
	require.NoError(t, err)
	assert.Zero(t, n)
}

// batchStreamingPublisher records the batches and, for each one, whether the
// note was already on every recipient's home list when it was published.
type batchStreamingPublisher struct {
	fanout       *FanoutTimelineService
	batches      [][]string
	single       []string
	pushedBefore bool
}

func (p *batchStreamingPublisher) PublishNote(topic string, _ *model.Note, _ *model.User) {
	p.single = append(p.single, topic)
}

func (p *batchStreamingPublisher) PublishNoteMany(topics []string, n *model.Note, _ *model.User) {
	p.batches = append(p.batches, append([]string(nil), topics...))
	for _, topic := range topics {
		ids, err := p.fanout.Get(context.Background(), Name(topic), "", "", 0)
		if err != nil || len(ids) == 0 || ids[0] != n.ID {
			p.pushedBefore = false
			return
		}
	}
}

// addFollowers registers count local followers of "author" named
// follower0000.., and returns their ids in the order ListFollowers pages them
// (id desc).
func addFollowers(following *testutil.MockFollowingRepository, count int) []string {
	ids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		rowID := fmt.Sprintf("f%04d", i)
		fid := "follower" + rowID
		following.Followings[rowID] = &model.Following{ID: rowID, FollowerID: fid, FolloweeID: "author"}
		ids = append(ids, fid)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids
}

// フォロワーが複数ページにわたっても、届けるべきフォロワーの home には同じ
// ID が 1 回ずつ入り、届けないフォロワー (リモート、休眠) には入らない。
// stream へは宛先ごとに 1 回、ページごとにまとめて送り、送る時点で list には
// もう入っている。
func TestFanoutHook_FollowersAcrossPagesArePipelined(t *testing.T) {
	h, fanout, following := newTestHook(t)
	ctx := context.Background()
	followers := addFollowers(following, 450)
	host := "remote.example"
	following.Followings["r0001"] = &model.Following{ID: "r0001", FollowerID: "remote1", FolloweeID: "author", FollowerHost: &host}
	following.Followings["h0001"] = &model.Following{ID: "h0001", FollowerID: "sleeper", FolloweeID: "author", IsFollowerHibernated: true}
	pub := &batchStreamingPublisher{fanout: fanout, pushedBefore: true}
	h.SetStreamingPublisher(pub)
	logs := captureSlog(t)

	noteID := idGen.Generate(time.Now())
	h.OnNoteCreated(&model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}, &model.User{ID: "author"})
	// 失敗が無ければ何も出さない。
	assert.Empty(t, recordsForNote(t, logs, "timeline push failed", noteID))
	assert.Empty(t, recordsForNote(t, logs, "timeline push failed for several timelines", noteID))

	for _, fid := range followers {
		raw, err := testRedis.Client.LRange(ctx, "list:homeTimeline:"+fid, 0, -1).Result()
		require.NoError(t, err)
		require.Equal(t, []string{noteID}, raw, "home of %s", fid)
	}
	for _, fid := range []string{"remote1", "sleeper"} {
		n, err := testRedis.Client.Exists(ctx, "list:homeTimeline:"+fid).Result()
		require.NoError(t, err)
		assert.Zero(t, n, "home of %s", fid)
	}

	var got []string
	for _, b := range pub.batches {
		assert.LessOrEqual(t, len(b), 200, "one batch per follower page")
		got = append(got, b...)
	}
	want := make([]string, 0, len(followers))
	for _, fid := range followers {
		want = append(want, "homeTimeline:"+fid)
	}
	sort.Strings(got)
	sort.Strings(want)
	assert.Equal(t, want, got, "each local follower once")
	assert.GreaterOrEqual(t, len(pub.batches), 3)
	assert.True(t, pub.pushedBefore, "the list push reaches Redis before the publish")
	assert.NotContains(t, pub.single, "homeTimeline:"+followers[0], "follower topics go through the batch")
}

// まとめて送れない publisher には、宛先ごとに PublishNote で送る。
func TestFanoutHook_FollowersFallBackToPublishNote(t *testing.T) {
	h, _, following := newTestHook(t)
	followers := addFollowers(following, 3)
	pub := &stubStreamingPublisher{}
	h.SetStreamingPublisher(pub)

	h.OnNoteCreated(&model.Note{ID: idGen.Generate(time.Now()), UserID: "author", Visibility: model.NoteVisibilityPublic}, &model.User{ID: "author"})
	for _, fid := range followers {
		assert.Contains(t, pub.topics, "homeTimeline:"+fid)
	}
}

// FTT を切っているときは list に積まないが、stream へは送る (pushWithLimit と同じ)。
func TestFanoutHook_FollowersToggleOffStillPublishes(t *testing.T) {
	h, fanout, following := newTestHook(t)
	h.SetFanoutToggle(&fakeFanoutToggle{enabled: false})
	followers := addFollowers(following, 2)
	pub := &batchStreamingPublisher{fanout: fanout, pushedBefore: true}
	h.SetStreamingPublisher(pub)

	h.OnNoteCreated(&model.Note{ID: idGen.Generate(time.Now()), UserID: "author", Visibility: model.NoteVisibilityPublic}, &model.User{ID: "author"})
	n, err := testRedis.Client.Exists(context.Background(), "list:homeTimeline:"+followers[0], "list:homeTimeline:"+followers[1]).Result()
	require.NoError(t, err)
	assert.Zero(t, n)
	require.Len(t, pub.batches, 1)
	assert.ElementsMatch(t, []string{"homeTimeline:" + followers[0], "homeTimeline:" + followers[1]}, pub.batches[0])
}

// 1 人の home への push が失敗しても、同じページの他のフォロワーには届く。
func TestFanoutHook_FollowerPushFailureIsIsolated(t *testing.T) {
	h, _, following := newTestHook(t)
	ctx := context.Background()
	followers := addFollowers(following, 3)
	require.NoError(t, testRedis.Client.Set(ctx, "list:homeTimeline:"+followers[1], "x", 0).Err())
	logs := captureSlog(t)

	noteID := idGen.Generate(time.Now())
	h.OnNoteCreated(&model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}, &model.User{ID: "author"})
	for _, fid := range []string{followers[0], followers[2]} {
		raw, err := testRedis.Client.LRange(ctx, "list:homeTimeline:"+fid, 0, -1).Result()
		require.NoError(t, err)
		assert.Equal(t, []string{noteID}, raw, "home of %s", fid)
	}

	// 失敗が 1 件なら、単発の push と同じ文言・属性 (name / err) の 1 行。
	recs := recordsForNote(t, logs, "timeline push failed", noteID)
	require.Len(t, recs, 1)
	assert.Equal(t, "homeTimeline:"+followers[1], recs[0]["name"])
	assert.Contains(t, recs[0]["err"], "WRONGTYPE")
	assert.NotContains(t, recs[0], "names")
	assert.Empty(t, recordsForNote(t, logs, "timeline push failed for several timelines", noteID))
}

// recordsForNote returns the records with msg logged for noteID.
func recordsForNote(t *testing.T, logs *lockedBuffer, msg, noteID string) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for _, r := range logs.records(t, msg) {
		if r["id"] == noteID {
			recs = append(recs, r)
		}
	}
	return recs
}

// 複数の宛先が失敗したら、別の文言の 1 行に、失敗した宛先を全部と、エラーの
// 種類ごとの件数を出す。
func TestFanoutHook_FollowerPushFailuresAreSummarized(t *testing.T) {
	h, _, following := newTestHook(t)
	ctx := context.Background()
	followers := addFollowers(following, 8)
	for _, fid := range followers {
		require.NoError(t, testRedis.Client.Set(ctx, "list:homeTimeline:"+fid, "x", 0).Err())
	}
	logs := captureSlog(t)

	noteID := idGen.Generate(time.Now())
	h.OnNoteCreated(&model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}, &model.User{ID: "author"})
	recs := recordsForNote(t, logs, "timeline push failed for several timelines", noteID)
	require.Len(t, recs, 1)
	want := make([]any, 0, len(followers))
	for _, fid := range followers {
		want = append(want, "homeTimeline:"+fid)
	}
	assert.Equal(t, want, recs[0]["names"])
	assert.EqualValues(t, 8, recs[0]["failed"])
	assert.EqualValues(t, 8, recs[0]["total"])
	require.Len(t, recs[0]["errors"], 1)
	assert.Regexp(t, `^8: WRONGTYPE`, recs[0]["errors"].([]any)[0])
	assert.Empty(t, recordsForNote(t, logs, "timeline push failed", noteID), "not also in the single form")
}

// エラーの種類ごとの件数は最初に出た順に並べ、最初のエラーを返す。
func TestSummarizeFailures(t *testing.T) {
	e1, e2 := errors.New("WRONGTYPE"), errors.New("i/o timeout")
	failed, kinds, first := summarizeFailures(
		[]string{"a", "b", "c", "d", "e"},
		[]error{nil, e2, e1, e1, nil},
	)
	assert.Equal(t, []string{"b", "c", "d"}, failed)
	assert.Equal(t, []string{"1: i/o timeout", "2: WRONGTYPE"}, kinds)
	assert.Equal(t, e2, first)

	failed, kinds, first = summarizeFailures([]string{"a"}, nil)
	assert.Empty(t, failed)
	assert.Empty(t, kinds)
	assert.NoError(t, first)
}

// チャンネルのフォロワーが複数ページにわたっても、全員の home に 1 回ずつ入る。
func TestFanoutHook_ChannelFollowersAcrossPages(t *testing.T) {
	h, _, _ := newTestHook(t)
	ctx := context.Background()
	chMock := testutil.NewMockChannelFollowingRepository()
	const count = channelFollowerPageSize*2 + 7
	fids := make([]string, 0, count)
	for i := 0; i < count; i++ {
		rowID := fmt.Sprintf("cf%05d", i)
		fid := "chfollower" + rowID
		chMock.Followings[rowID] = &model.ChannelFollowing{ID: rowID, FolloweeID: "ch1", FollowerID: fid}
		fids = append(fids, fid)
	}
	h.SetChannelFollowerRepo(chMock)

	noteID := idGen.Generate(time.Now())
	ch := "ch1"
	h.OnNoteCreated(&model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic, ChannelID: &ch}, &model.User{ID: "author"})

	pipe := testRedis.Client.Pipeline()
	cmds := make([]*redis.StringSliceCmd, len(fids))
	for i, fid := range fids {
		cmds[i] = pipe.LRange(ctx, "list:homeTimeline:"+fid, 0, -1)
	}
	_, err := pipe.Exec(ctx)
	require.NoError(t, err)
	for i, cmd := range cmds {
		require.Equal(t, []string{noteID}, cmd.Val(), "home of %s", fids[i])
	}
}

// 猶予より古いノートの経路でも、失敗はその宛先のエラーとして返す。
func TestFanoutTimelineService_PushMany_OldNoteFailureIsPerTimeline(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	require.NoError(t, testRedis.Client.Set(ctx, "list:homeTimeline:broken", "x", 0).Err())

	old := idGen.Generate(time.Now().Add(-10 * time.Minute))
	errs := svc.PushMany(ctx, []Name{HomeTimelineName("broken"), HomeTimelineName("empty")}, old, 100)
	require.Len(t, errs, 2)
	assert.ErrorContains(t, errs[0], "WRONGTYPE")
	assert.NoError(t, errs[1])
	raw, err := testRedis.Client.LRange(ctx, "list:homeTimeline:empty", 0, -1).Result()
	require.NoError(t, err)
	assert.Equal(t, []string{old}, raw)
}

// 一部だけが失敗したときは、失敗した宛先だけを名指しし、total は宛先の全数。
func TestFanoutHook_FollowerPushSomeFail(t *testing.T) {
	h, _, following := newTestHook(t)
	ctx := context.Background()
	followers := addFollowers(following, 4)
	for _, fid := range []string{followers[1], followers[3]} {
		require.NoError(t, testRedis.Client.Set(ctx, "list:homeTimeline:"+fid, "x", 0).Err())
	}
	logs := captureSlog(t)

	noteID := idGen.Generate(time.Now())
	h.OnNoteCreated(&model.Note{ID: noteID, UserID: "author", Visibility: model.NoteVisibilityPublic}, &model.User{ID: "author"})
	recs := recordsForNote(t, logs, "timeline push failed for several timelines", noteID)
	require.Len(t, recs, 1)
	assert.Equal(t, []any{"homeTimeline:" + followers[1], "homeTimeline:" + followers[3]}, recs[0]["names"])
	assert.EqualValues(t, 2, recs[0]["failed"])
	assert.EqualValues(t, 4, recs[0]["total"])
}
