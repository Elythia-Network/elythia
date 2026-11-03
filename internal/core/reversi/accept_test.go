package reversi

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// 招待が受けられたことの記録と、招待した人への知らせ (#3946)。

type recordingMatched struct {
	inviters []string
}

func (r *recordingMatched) ReversiMatched(inviterID string, _ *model.ReversiGame) {
	r.inviters = append(r.inviters, inviterID)
}

func pendingGame() *model.ReversiGame {
	return &model.ReversiGame{ID: "acc1", User1ID: "alice", User2ID: "bob"}
}

func TestService_AcceptInvitation_TellsTheInviterOnce(t *testing.T) {
	reversiTestRedis.FlushAll(context.Background())
	svc := NewService(newFakeRepo(), nil, reversiTestRedis.Client)
	rec := &recordingMatched{}
	svc.SetMatchedNotifier(rec)
	ctx := context.Background()

	assert.False(t, svc.InvitationAcceptedRecently(ctx, "acc1"))
	svc.MarkInviterWaiting(ctx, "acc1")
	svc.AcceptInvitation(ctx, pendingGame())
	svc.AcceptInvitation(ctx, pendingGame())
	assert.Equal(t, []string{"alice"}, rec.inviters)
	assert.True(t, svc.InvitationAcceptedRecently(ctx, "acc1"))

	// 印は、未開始の対局が掃除される (毎日 0 時、10 分を過ぎたもの) まで持つ。途中で
	// 切れると、招待された側の画面の呼び直しで付き直り、知らせ直してしまう
	ttl, err := reversiTestRedis.Client.TTL(ctx, acceptedKey("acc1")).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, 24*time.Hour+10*time.Minute)
	waitTTL, err := reversiTestRedis.Client.TTL(ctx, inviterWaitingKey("acc1")).Result()
	require.NoError(t, err)
	assert.Greater(t, waitTTL, time.Duration(0))
	assert.LessOrEqual(t, waitTTL, 20*time.Second, "本家の招待の期限と同じ")
}

// 招待した人が待っていなければ (去った後の古い招待など)、受けた印だけ付けて知らせない。
// 知らせると、別の相手を待っている画面がこの対局へ移ってしまう。
func TestService_AcceptInvitation_DoesNotTellAnInviterWhoIsNotWaiting(t *testing.T) {
	reversiTestRedis.FlushAll(context.Background())
	svc := NewService(newFakeRepo(), nil, reversiTestRedis.Client)
	rec := &recordingMatched{}
	svc.SetMatchedNotifier(rec)
	ctx := context.Background()

	svc.MarkInviterWaiting(ctx, "another-game")
	svc.AcceptInvitation(ctx, pendingGame())
	assert.Empty(t, rec.inviters)
	assert.True(t, svc.InvitationAcceptedRecently(ctx, "acc1"))
}

func TestService_AcceptInvitation_IgnoresStartedAndEndedGames(t *testing.T) {
	reversiTestRedis.FlushAll(context.Background())
	svc := NewService(newFakeRepo(), nil, reversiTestRedis.Client)
	rec := &recordingMatched{}
	svc.SetMatchedNotifier(rec)
	ctx := context.Background()

	started := pendingGame()
	started.IsStarted = true
	ended := pendingGame()
	ended.IsEnded = true
	svc.MarkInviterWaiting(ctx, "acc1")
	svc.AcceptInvitation(ctx, started)
	svc.AcceptInvitation(ctx, ended)
	svc.AcceptInvitation(ctx, nil)
	assert.Empty(t, rec.inviters)
	assert.False(t, svc.InvitationAcceptedRecently(ctx, "acc1"))
}

// Redis が無いとき・使えないときは、知らせず、受けられていない扱いにする。印が無いと、
// 招待された側の画面が呼び直すたびに知らせてしまうため。
func TestService_AcceptInvitation_WithoutRedis(t *testing.T) {
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { _ = down.Close() })
	for name, r := range map[string]redis.Cmdable{"nil": nil, "down": down} {
		t.Run(name, func(t *testing.T) {
			svc := NewService(newFakeRepo(), nil, r)
			rec := &recordingMatched{}
			svc.SetMatchedNotifier(rec)
			svc.MarkInviterWaiting(context.Background(), "acc1")
			svc.AcceptInvitation(context.Background(), pendingGame())
			svc.AcceptInvitation(context.Background(), pendingGame())
			assert.Empty(t, rec.inviters)
			assert.False(t, svc.InvitationAcceptedRecently(context.Background(), "acc1"))
		})
	}
}

// 知らせる先が無くても、印は付ける。
func TestService_AcceptInvitation_WithoutNotifier(t *testing.T) {
	reversiTestRedis.FlushAll(context.Background())
	svc := NewService(newFakeRepo(), nil, reversiTestRedis.Client)
	svc.MarkInviterWaiting(context.Background(), "acc1")
	svc.AcceptInvitation(context.Background(), pendingGame())
	assert.True(t, svc.InvitationAcceptedRecently(context.Background(), "acc1"))
}

// 招待した人の reversi/match が受けた対局を返すのは、受けてから 3 分 (本家と同じ) まで。
func TestService_InvitationAcceptedRecently_CountsFromTheAcceptance(t *testing.T) {
	reversiTestRedis.FlushAll(context.Background())
	svc := NewService(newFakeRepo(), nil, reversiTestRedis.Client)
	ctx := context.Background()
	set := func(ago time.Duration) {
		require.NoError(t, reversiTestRedis.Client.Set(ctx, acceptedKey("acc1"), time.Now().Add(-ago).UnixMilli(), time.Hour).Err())
	}
	set(2 * time.Minute)
	assert.True(t, svc.InvitationAcceptedRecently(ctx, "acc1"))
	set(3*time.Minute + time.Second)
	assert.False(t, svc.InvitationAcceptedRecently(ctx, "acc1"))

	// 読めない値は受けられていない扱い
	require.NoError(t, reversiTestRedis.Client.Set(ctx, acceptedKey("acc1"), "x", time.Hour).Err())
	assert.False(t, svc.InvitationAcceptedRecently(ctx, "acc1"))
}

// 受けられたかの判定 (#3949): 受けた印か、招待された側の準備。
func TestService_WasInvitationAccepted(t *testing.T) {
	reversiTestRedis.FlushAll(context.Background())
	ctx := context.Background()
	svc := NewService(newFakeRepo(), nil, reversiTestRedis.Client)

	assert.False(t, svc.WasInvitationAccepted(ctx, pendingGame()))
	ready := pendingGame()
	ready.User2Ready = true
	assert.True(t, svc.WasInvitationAccepted(ctx, ready))
	// 招待した人の準備は手がかりにしない
	inviterReady := pendingGame()
	inviterReady.User1Ready = true
	assert.False(t, svc.WasInvitationAccepted(ctx, inviterReady))

	svc.AcceptInvitation(ctx, pendingGame())
	assert.True(t, svc.WasInvitationAccepted(ctx, pendingGame()))

	// 受けてから 3 分を過ぎても、取り消さない判定には使う
	require.NoError(t, reversiTestRedis.Client.Set(ctx, acceptedKey("acc1"), time.Now().Add(-time.Hour).UnixMilli(), time.Hour).Err())
	assert.False(t, svc.InvitationAcceptedRecently(ctx, "acc1"))
	assert.True(t, svc.WasInvitationAccepted(ctx, pendingGame()))
}

func TestService_WasInvitationAccepted_WithoutRedis(t *testing.T) {
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { _ = down.Close() })
	for name, r := range map[string]redis.Cmdable{"nil": nil, "down": down} {
		t.Run(name, func(t *testing.T) {
			svc := NewService(newFakeRepo(), nil, r)
			assert.False(t, svc.WasInvitationAccepted(context.Background(), pendingGame()))
			ready := pendingGame()
			ready.User2Ready = true
			assert.True(t, svc.WasInvitationAccepted(context.Background(), ready))
		})
	}
}

// 招待として扱わなくなるのは、受けた印があり、受けてから 3 分を過ぎたときだけ。準備の
// 状態だけでは受けた時刻が分からないので、真にしない。
func TestService_AcceptedLongAgo(t *testing.T) {
	reversiTestRedis.FlushAll(context.Background())
	ctx := context.Background()
	svc := NewService(newFakeRepo(), nil, reversiTestRedis.Client)
	set := func(ago time.Duration) {
		require.NoError(t, reversiTestRedis.Client.Set(ctx, acceptedKey("acc1"), time.Now().Add(-ago).UnixMilli(), time.Hour).Err())
	}

	ready := pendingGame()
	ready.User2Ready = true
	assert.False(t, svc.AcceptedLongAgo(ctx, ready), "準備だけでは真にしない")
	set(time.Hour)
	assert.True(t, svc.AcceptedLongAgo(ctx, pendingGame()))
	set(time.Minute)
	assert.False(t, svc.AcceptedLongAgo(ctx, pendingGame()))
	assert.False(t, NewService(newFakeRepo(), nil, nil).AcceptedLongAgo(ctx, ready))
}
