package reversi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// --- FederationIDCache (real Redis) ---

func TestFederationIDCache_SetGetDelete_Roundtrip(t *testing.T) {
	ctx := context.Background()
	reversiTestRedis.FlushAll(ctx)
	c := NewFederationIDCache(reversiTestRedis.Client)

	c.Set(ctx, "sess-abc", "game-xyz")

	// Forward lookup: session → game
	gameID, err := c.Get(ctx, "sess-abc")
	require.NoError(t, err)
	assert.Equal(t, "game-xyz", gameID)

	// Reverse lookup: game → session
	session, ok := c.GetSessionByGame(ctx, "game-xyz")
	assert.True(t, ok)
	assert.Equal(t, "sess-abc", session)

	// Delete wipes both directions
	c.Delete(ctx, "sess-abc", "game-xyz")

	_, err = c.Get(ctx, "sess-abc")
	assert.ErrorIs(t, err, redis.Nil)

	_, ok = c.GetSessionByGame(ctx, "game-xyz")
	assert.False(t, ok)
}

func TestFederationIDCache_GetSessionByGame_Missing(t *testing.T) {
	ctx := context.Background()
	reversiTestRedis.FlushAll(ctx)
	c := NewFederationIDCache(reversiTestRedis.Client)

	session, ok := c.GetSessionByGame(ctx, "nonexistent")
	assert.False(t, ok)
	assert.Empty(t, session)
}

func TestFederationIDCache_Delete_NilAndEmptyArgs(t *testing.T) {
	ctx := context.Background()
	reversiTestRedis.FlushAll(ctx)

	// Nil redis should be a no-op and not panic.
	nilCache := NewFederationIDCache(nil)
	nilCache.Delete(ctx, "s", "g") // must not panic

	// Real redis: empty args should skip the corresponding DEL.
	c := NewFederationIDCache(reversiTestRedis.Client)
	c.Set(ctx, "sess-x", "game-y")
	c.Delete(ctx, "", "game-y") // only game side
	_, err := c.Get(ctx, "sess-x")
	require.NoError(t, err) // sess side still present

	c.Delete(ctx, "sess-x", "") // only session side
	session, ok := c.GetSessionByGame(ctx, "game-y")
	assert.False(t, ok)
	assert.Empty(t, session)
}

func TestFederationIDCache_Get_NilRedisReturnsRedisNil(t *testing.T) {
	c := NewFederationIDCache(nil)
	_, err := c.Get(context.Background(), "sess")
	assert.ErrorIs(t, err, redis.Nil)
}

func TestFederationIDCache_GetSessionByGame_NilRedis(t *testing.T) {
	c := NewFederationIDCache(nil)
	_, ok := c.GetSessionByGame(context.Background(), "game")
	assert.False(t, ok)
}

// sessionToGameKey / gameToSessionKey are unexported helpers — exercise them
// directly so they are counted in coverage.
func TestFederationIDCache_KeyHelpers(t *testing.T) {
	assert.Equal(t, "reversi:fed:session:abc", sessionToGameKey("abc"))
	assert.Equal(t, "reversi:fed:game:xyz", gameToSessionKey("xyz"))
}

// --- service edge paths that existing tests miss ---

// CancelGame の repo.Delete がエラーを返す経路をカバーする。
// fakeRepo に deleteErr を付けた派生 stub を使う。
type failingDeleteRepo struct {
	*fakeRepo
	deleteErr error
}

func (r *failingDeleteRepo) Delete(_ string) error { return r.deleteErr }

func TestService_CancelGame_DeleteError(t *testing.T) {
	repo := newFakeRepo()
	wrapped := &failingDeleteRepo{fakeRepo: repo, deleteErr: errors.New("delete boom")}
	svc := NewService(wrapped, &capturePublisher{}, nil)

	g := &model.ReversiGame{
		ID: "g1", User1ID: "alice", User2ID: "bob",
		Map:  model.StringArray{"-"},
		Logs: datatypes.JSON("[]"),
	}
	require.NoError(t, repo.Create(g))
	err := svc.CancelGame(context.Background(), g.ID, "alice")
	assert.ErrorIs(t, err, wrapped.deleteErr)
}

// StartGame の repo.Update がエラーを返す経路をカバーする。
func TestService_StartGame_UpdateError(t *testing.T) {
	game, repo, _, svc := newPendingGame(t)
	repo.updateErr = errors.New("update boom")
	err := svc.StartGame(context.Background(), game)
	assert.Error(t, err)
}

// UpdateSettings の repo.Update エラー経路。
func TestService_UpdateSettings_UpdateError(t *testing.T) {
	game, repo, _, svc := newPendingGame(t)
	repo.updateErr = errors.New("update boom")
	raw, _ := json.Marshal([]string{"bw", "wb"})
	err := svc.UpdateSettings(context.Background(), game.ID, "alice", "map", raw)
	assert.Error(t, err)
}

// Surrender の repo.Update エラー経路。
func TestService_Surrender_UpdateError(t *testing.T) {
	game, repo, _, svc := startedGame(t)
	repo.updateErr = errors.New("update boom")
	err := svc.Surrender(context.Background(), game.ID, "alice")
	assert.Error(t, err)
}

// PutStone の repo.Update エラー経路 (非終局手)。
func TestService_PutStone_UpdateError(t *testing.T) {
	game, repo, _, svc := startedGame(t)
	repo.updateErr = errors.New("update boom")
	err := svc.PutStone(context.Background(), game.ID, "alice", 19, "")
	assert.Error(t, err)
}

// PutStone の engine 復元に失敗する経路 (logs が壊れている)。
func TestService_PutStone_BadLogs(t *testing.T) {
	game, repo, _, svc := startedGame(t)
	// 壊れた logs を書き込む
	game.Logs = datatypes.JSON("not-json")
	_ = repo.Update(game)
	err := svc.PutStone(context.Background(), game.ID, "alice", 19, "")
	assert.Error(t, err)
}

// CheckTimeout で EngineFromGame が失敗する経路 (logs が壊れている)。
func TestService_CheckTimeout_BadLogs(t *testing.T) {
	reversiTestRedis.FlushAll(context.Background())
	game, repo, _, _ := startedGame(t)
	// redis を差し替えて実タイマーキーを使う
	svc := NewService(repo, &capturePublisher{}, reversiTestRedis.Client)

	// タイマーキーを削除してタイムアウト判定に進ませる
	require.NoError(t, reversiTestRedis.Client.Del(context.Background(), turnTimerKey(game.ID, 0)).Err())

	// logs を壊す (unmarshal は握りつぶすが、EngineFromGame 呼び出しで失敗する)
	game.Logs = datatypes.JSON("not-json")
	_ = repo.Update(game)

	err := svc.CheckTimeout(context.Background(), game.ID)
	assert.Error(t, err)
}

// CheckTimeout で engine.Turn == nil (ゲーム的には既に終局) の経路。
// finalize 済みの盤面を logs として流し込み、repo に書き戻して呼ぶ。
func TestService_CheckTimeout_EngineTurnNil(t *testing.T) {
	reversiTestRedis.FlushAll(context.Background())
	repo := newFakeRepo()
	pub := &capturePublisher{}
	svc := NewService(repo, pub, reversiTestRedis.Client)

	// 初期マップを使い、Turn=nil に誘導するのは難しいので直接 WhiteCount=4
	// のような終局盤を注入する。engine.Turn==nil になる盤面: 全マスが埋まっている。
	fullMap := model.StringArray{"bbbb", "bbbb", "bbbb", "wwww"}
	black := 1
	game := &model.ReversiGame{
		ID:                   "full1",
		User1ID:              "alice",
		User2ID:              "bob",
		Map:                  fullMap,
		BW:                   "1",
		Black:                &black,
		IsStarted:            true,
		TimeLimitForEachTurn: 1,
		Logs:                 datatypes.JSON("[]"),
	}
	require.NoError(t, repo.Create(game))
	// タイマーキーを無くす
	require.NoError(t, reversiTestRedis.Client.Del(context.Background(), turnTimerKey(game.ID, 0)).Err())

	err := svc.CheckTimeout(context.Background(), game.ID)
	require.NoError(t, err)

	got, _ := repo.FindByID(game.ID)
	// engine.Turn==nil なのでゲーム自体は ended にならない (timeout 経路を抜ける)
	assert.False(t, got.IsEnded)
}

// turnTimerExists: Redis の Exists が失敗するケース。
// 実 redis client を閉じてから呼ぶことで err 経路を確実に踏む。
func TestService_TurnTimerExists_RedisClosed(t *testing.T) {
	// 専用 redis クライアントを作って閉じる (親接続を壊さないため)
	c := redis.NewClient(&redis.Options{Addr: reversiTestRedis.Client.Options().Addr})
	require.NoError(t, c.Close())

	svc := NewService(newFakeRepo(), nil, c)
	_, err := svc.turnTimerExists(context.Background(), "g1", 0)
	assert.Error(t, err)
}

// CheckTimeout は turnTimerExists のエラーをそのまま伝搬する。
func TestService_CheckTimeout_TurnTimerError(t *testing.T) {
	c := redis.NewClient(&redis.Options{Addr: reversiTestRedis.Client.Options().Addr})
	require.NoError(t, c.Close())

	game, repo, _, _ := newPendingGame(t)
	// Start 済みにする
	b := 1
	game.IsStarted = true
	game.Black = &b
	_ = repo.Update(game)

	svc := NewService(repo, &capturePublisher{}, c)
	err := svc.CheckTimeout(context.Background(), game.ID)
	assert.Error(t, err)
}

// applySetting: bad-JSON 経路を全 key について個別に exercise する。
func TestApplySetting_BadJSONPerKey(t *testing.T) {
	cases := []struct {
		key string
		raw string
	}{
		{"map", `"not-array"`},
		{"bw", `{`}, // malformed
		{"isLlotheo", `"nope"`},
		{"canPutEverywhere", `"nope"`},
		{"loopedBoard", `"nope"`},
		{"timeLimitForEachTurn", `"nope"`},
		{"noIrregularRules", `"nope"`},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			err := applySetting(&model.ReversiGame{}, tc.key, json.RawMessage(tc.raw))
			assert.ErrorIs(t, err, ErrInvalidSetting)
		})
	}
}

// pickBlack: "random" ケースの両分岐 (1 と 2) を何度もサンプルして両方踏ませる。
// rand.Intn は seed が固定されていなくても、十分な試行数で 1/2 両方が観測される。
func TestPickBlack_RandomSamples(t *testing.T) {
	seenOne, seenTwo := false, false
	for range 1000 {
		v := pickBlack("random", "")
		if v == 1 {
			seenOne = true
		} else if v == 2 {
			seenTwo = true
		}
		if seenOne && seenTwo {
			return
		}
	}
	t.Fatalf("rand.Intn skewed: one=%v two=%v", seenOne, seenTwo)
}

// NewGame: non-existent stone symbols should be ignored (coverage for the
// default branch in the cell parser).
func TestNewGame_IgnoresUnknownCells(t *testing.T) {
	g := NewGame([]string{"bxw"}, Options{})
	assert.Equal(t, 1, g.BlackCount())
	assert.Equal(t, 1, g.WhiteCount())
}

// sanity: zero-value time used here just to make the "time" import stay.
var _ = time.Now
