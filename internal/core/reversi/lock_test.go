package reversi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// 手番のタイマーと手のロック (#3942)。

// hookRepo runs a hook right after a game row is written.
type hookRepo struct {
	*fakeRepo
	beforeUpdate     func()
	afterUpdate      func()
	afterMarkStarted func()
}

func (r *hookRepo) Update(g *model.ReversiGame) error {
	if r.beforeUpdate != nil {
		hook := r.beforeUpdate
		r.beforeUpdate = nil
		hook()
	}
	err := r.fakeRepo.Update(g)
	if r.afterUpdate != nil {
		hook := r.afterUpdate
		r.afterUpdate = nil
		hook()
	}
	return err
}

func (r *hookRepo) MarkStarted(g *model.ReversiGame) (bool, error) {
	ok, err := r.fakeRepo.MarkStarted(g)
	if r.afterMarkStarted != nil {
		hook := r.afterMarkStarted
		r.afterMarkStarted = nil
		hook()
	}
	return ok, err
}

// 手を書いた直後に時間切れを申告されても、相手を負けにしない。タイマーを書いた後に
// 置くと、その間はキーが無く、打った本人が申告の時機を選べる。
func TestService_ClaimRightAfterAMoveDoesNotTimeOutTheOpponent(t *testing.T) {
	game, repo, pub, _ := newStartedGameWithRedis(t)
	hr := &hookRepo{fakeRepo: repo}
	svc := NewService(hr, pub, reversiTestRedis.Client)
	svc.lockWait = 50 * time.Millisecond
	hr.afterUpdate = func() {
		require.NoError(t, svc.CheckTimeout(context.Background(), game.ID))
	}

	require.NoError(t, svc.PutStone(context.Background(), game.ID, "alice", 19, ""))
	got, _ := repo.FindByID(game.ID)
	assert.False(t, got.IsEnded)
	assert.Nil(t, got.TimeoutUserID)
}

// 始まった印を書いた直後に申告されても、先手を負けにしない。
func TestService_ClaimRightAfterTheStartDoesNotTimeOutTheFirstPlayer(t *testing.T) {
	reversiTestRedis.FlushAll(context.Background())
	game, repo, pub, _ := newPendingGame(t)
	hr := &hookRepo{fakeRepo: repo}
	svc := NewService(hr, pub, reversiTestRedis.Client)
	hr.afterMarkStarted = func() {
		require.NoError(t, svc.CheckTimeout(context.Background(), game.ID))
	}
	require.NoError(t, svc.UpdateReady(context.Background(), game.ID, "alice", true))
	require.NoError(t, svc.UpdateReady(context.Background(), game.ID, "bob", true))
	got, _ := repo.FindByID(game.ID)
	assert.True(t, got.IsStarted)
	assert.False(t, got.IsEnded)
}

// 手の後にロックを残さない。
func TestService_PutStone_ReleasesTheLock(t *testing.T) {
	game, _, _, svc := newStartedGameWithRedis(t)
	require.NoError(t, svc.PutStone(context.Background(), game.ID, "alice", 19, ""))
	n, err := reversiTestRedis.Client.Exists(context.Background(), gameLockKey(game.ID)).Result()
	require.NoError(t, err)
	assert.Zero(t, n)
}

// ロックが空かなければ (持っている処理が止まったなど)、待ちの上限の後にロック無しで
// 進む。他人のロックは消さない。
func TestService_PutStone_MovesWithoutTheLockAfterWaiting(t *testing.T) {
	game, _, _, svc := newStartedGameWithRedis(t)
	svc.lockWait = 100 * time.Millisecond
	ctx := context.Background()
	require.NoError(t, reversiTestRedis.Client.Set(ctx, gameLockKey(game.ID), "someone-else", time.Minute).Err())

	start := time.Now()
	require.NoError(t, svc.PutStone(ctx, game.ID, "alice", 19, ""))
	assert.GreaterOrEqual(t, time.Since(start), 100*time.Millisecond, "上限まで待つ")
	v, err := reversiTestRedis.Client.Get(ctx, gameLockKey(game.ID)).Result()
	require.NoError(t, err)
	assert.Equal(t, "someone-else", v, "他人のロックを消さない")
}

// Redis が使えなくても手は打てる (手が打てなくなる方が害が大きい)。
func TestService_PutStone_MovesWhenRedisIsDown(t *testing.T) {
	game, repo, pub, _ := newStartedGameWithRedis(t)
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { _ = down.Close() })
	svc := NewService(repo, pub, down)
	require.NoError(t, svc.PutStone(context.Background(), game.ID, "alice", 19, ""))
}

// 対局者でない人の手は、ロックを待たずに断る。待たせると、他人の対局のロックを
// 占有して対局者の手を遅らせられる。
func TestService_PutStone_RejectsANonPlayerWithoutTakingTheLock(t *testing.T) {
	game, _, _, svc := newStartedGameWithRedis(t)
	svc.lockWait = 2 * time.Second
	ctx := context.Background()
	require.NoError(t, reversiTestRedis.Client.Set(ctx, gameLockKey(game.ID), "someone-else", time.Minute).Err())

	start := time.Now()
	assert.ErrorIs(t, svc.PutStone(ctx, game.ID, "carol", 19, ""), ErrNotPlayer)
	assert.Less(t, time.Since(start), 500*time.Millisecond)
}

// 自分のロックが期限切れで消え、別の処理が取り直した後に解放しても、相手のロックは
// 消さない (トークンを照合して消す)。
func TestService_LockReleaseKeepsALockSomeoneElseTookOver(t *testing.T) {
	reversiTestRedis.FlushAll(context.Background())
	svc := NewService(newFakeRepo(), nil, reversiTestRedis.Client)
	ctx := context.Background()
	release := svc.lockGame(ctx, "g1")
	require.NoError(t, reversiTestRedis.Client.Set(ctx, gameLockKey("g1"), "someone-else", time.Minute).Err())

	release()
	v, err := reversiTestRedis.Client.Get(ctx, gameLockKey("g1")).Result()
	require.NoError(t, err)
	assert.Equal(t, "someone-else", v)
}

// interleaveWithMove runs op in the background while alice's move is about to
// write its row, and waits for both. op が手のロックを待たなければ、手の書き戻しと
// 交差する。
func interleaveWithMove(t *testing.T, prep func(), op func(svc *Service) error) (*fakeRepo, *capturePublisher) {
	t.Helper()
	game, repo, pub, _ := newStartedGameWithRedis(t)
	prep()
	hr := &hookRepo{fakeRepo: repo}
	svc := NewService(hr, pub, reversiTestRedis.Client)
	svc.lockWait = 2 * time.Second
	done := make(chan error, 1)
	hr.beforeUpdate = func() {
		go func() { done <- op(svc) }()
		time.Sleep(200 * time.Millisecond)
	}
	require.NoError(t, svc.PutStone(context.Background(), game.ID, "alice", 19, ""))
	require.NoError(t, <-done)
	return repo, pub
}

// 手を書いている最中の投了は、手の後に終局させる。交差すると手の書き戻しが投了を
// 消し、ended を配ったのに DB では対局が続く。
func TestService_SurrenderDuringAMoveIsKept(t *testing.T) {
	repo, pub := interleaveWithMove(t, func() {}, func(svc *Service) error {
		return svc.Surrender(context.Background(), "rg1", "bob")
	})
	got, _ := repo.FindByID("rg1")
	assert.True(t, got.IsEnded, "投了が消えた")
	require.NotNil(t, got.SurrenderedUserID)
	assert.Equal(t, "bob", *got.SurrenderedUserID)
	var logs [][]int
	require.NoError(t, json.Unmarshal(got.Logs, &logs))
	assert.Len(t, logs, 1, "手も残る")
	assert.Equal(t, []string{"log", "ended"}, pub.types()[len(pub.types())-2:])
}

// 手を書いている最中の時間切れの申告は、手が終わってから読み直す。手番は進み、
// 新しい手番のタイマーがあるので何もしない。ロックを待たずに読み直すと、前の手番の
// キーが無いのを見て ended を配る (DB は手の書き戻しで上書きされるので、配られた
// イベントで見る)。
func TestService_ClaimDuringAMoveWaitsForIt(t *testing.T) {
	expire := func() {
		require.NoError(t, reversiTestRedis.Client.Del(context.Background(), turnTimerKey("rg1", 0)).Err())
	}
	repo, pub := interleaveWithMove(t, expire, func(svc *Service) error {
		return svc.CheckTimeout(context.Background(), "rg1")
	})
	assert.NotContains(t, pub.types(), "ended")
	got, _ := repo.FindByID("rg1")
	assert.False(t, got.IsEnded)
}

// 時間が残っている申告はロックを待たない。誰でも呼べるので、待たせると他人の対局の
// ロックを占有して手を遅らせられる。
func TestService_ClaimWithTimeLeftDoesNotWaitForTheLock(t *testing.T) {
	game, _, _, svc := newStartedGameWithRedis(t)
	svc.lockWait = 2 * time.Second
	ctx := context.Background()
	require.NoError(t, reversiTestRedis.Client.Set(ctx, gameLockKey(game.ID), "someone-else", time.Minute).Err())

	start := time.Now()
	require.NoError(t, svc.CheckTimeout(ctx, game.ID))
	assert.Less(t, time.Since(start), 500*time.Millisecond)
}
