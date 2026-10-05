package reversi

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

var reversiTestRedis *testutil.TestRedis

func TestMain(m *testing.M) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		log.Fatalf("reversi test: redis setup failed: %v", err)
	}
	reversiTestRedis = tr
	code := m.Run()
	reversiTestRedis.Teardown(ctx)
	os.Exit(code)
}

// newStartedGameWithRedis creates a started game wired to a real Redis so
// turn timer tests can actually exercise SET/EXISTS.
func newStartedGameWithRedis(t *testing.T) (*model.ReversiGame, *fakeRepo, *capturePublisher, *Service) {
	t.Helper()
	reversiTestRedis.FlushAll(context.Background())
	repo := newFakeRepo()
	pub := &capturePublisher{}
	svc := NewService(repo, pub, reversiTestRedis.Client)

	game := &model.ReversiGame{
		ID:                   "rg1",
		User1ID:              "alice",
		User2ID:              "bob",
		Map:                  model.StringArray{"--------", "--------", "--------", "---wb---", "---bw---", "--------", "--------", "--------"},
		BW:                   "1",
		TimeLimitForEachTurn: 1, // 1 second for fast timeout
		Logs:                 datatypes.JSON("[]"),
	}
	require.NoError(t, repo.Create(game))
	require.NoError(t, svc.UpdateReady(context.Background(), game.ID, "alice", true))
	require.NoError(t, svc.UpdateReady(context.Background(), game.ID, "bob", true))
	fresh, _ := repo.FindByID(game.ID)
	return fresh, repo, pub, svc
}

func TestService_CheckTimeout_StillRunning(t *testing.T) {
	game, _, _, svc := newStartedGameWithRedis(t)
	// Timer just set by StartGame (TTL 1s) — check immediately, still valid.
	err := svc.CheckTimeout(context.Background(), game.ID)
	require.NoError(t, err)
	got, _ := svc.Get(context.Background(), game.ID)
	assert.False(t, got.IsEnded, "timer still valid, game must not end")
}

func TestService_CheckTimeout_Expired(t *testing.T) {
	game, repo, pub, svc := newStartedGameWithRedis(t)

	// Explicitly delete the timer key to simulate expiry.
	key := turnTimerKey(game.ID, 0)
	require.NoError(t, reversiTestRedis.Client.Del(context.Background(), key).Err())

	require.NoError(t, svc.CheckTimeout(context.Background(), game.ID))

	got, _ := repo.FindByID(game.ID)
	assert.True(t, got.IsEnded)
	require.NotNil(t, got.WinnerID)
	assert.Equal(t, "bob", *got.WinnerID, "current turn is alice(black), she loses")
	require.NotNil(t, got.TimeoutUserID)
	assert.Equal(t, "alice", *got.TimeoutUserID)
	assert.Contains(t, pub.types(), "ended")
}

func TestService_SetTurnTimer_KeyExists(t *testing.T) {
	reversiTestRedis.FlushAll(context.Background())
	svc := NewService(newFakeRepo(), &capturePublisher{}, reversiTestRedis.Client)
	ctx := context.Background()
	svc.setTurnTimer(ctx, "g1", 0, 5)

	exists, err := svc.turnTimerExists(ctx, "g1", 0)
	require.NoError(t, err)
	assert.True(t, exists)

	missing, err := svc.turnTimerExists(ctx, "g1", 1)
	require.NoError(t, err)
	assert.False(t, missing)
}

func TestService_SetTurnTimer_ZeroSecondsSkipped(t *testing.T) {
	reversiTestRedis.FlushAll(context.Background())
	svc := NewService(newFakeRepo(), nil, reversiTestRedis.Client)
	svc.setTurnTimer(context.Background(), "g1", 0, 0) // no-op path
	exists, err := svc.turnTimerExists(context.Background(), "g1", 0)
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestService_PutStone_EndsGame_SmallBoard(t *testing.T) {
	// Construct a tiny 4x4 board where filling quickly ends the game so the
	// finalize branch is exercised.
	reversiTestRedis.FlushAll(context.Background())
	repo := newFakeRepo()
	pub := &capturePublisher{}
	svc := NewService(repo, pub, reversiTestRedis.Client)

	tinyMap := model.StringArray{
		"----",
		"-wb-",
		"-bw-",
		"----",
	}
	game := &model.ReversiGame{
		ID:                   "tiny1",
		User1ID:              "alice",
		User2ID:              "bob",
		Map:                  tinyMap,
		BW:                   "1",
		TimeLimitForEachTurn: 60,
		Logs:                 datatypes.JSON("[]"),
	}
	require.NoError(t, repo.Create(game))
	require.NoError(t, svc.UpdateReady(context.Background(), game.ID, "alice", true))
	require.NoError(t, svc.UpdateReady(context.Background(), game.ID, "bob", true))

	// On this board, run PutStone until one side cannot move → game ends.
	// We use a loop that alternates based on engine turn.
	for range 16 {
		g, _ := repo.FindByID(game.ID)
		if g.IsEnded {
			break
		}
		engine, err := EngineFromGame(g)
		require.NoError(t, err)
		if engine.Turn == nil {
			break
		}
		places := engine.GetPuttablePlaces(*engine.Turn)
		if len(places) == 0 {
			break
		}
		uid := playerIDForColor(g, *engine.Turn)
		require.NotEmpty(t, uid)
		require.NoError(t, svc.PutStone(context.Background(), game.ID, uid, places[0], ""))
	}

	final, _ := repo.FindByID(game.ID)
	assert.True(t, final.IsEnded, "small board should end via normal play")
	assert.NotNil(t, final.EndedAt)
	assert.NotNil(t, final.CRC32)
	assert.Contains(t, pub.types(), "ended")
	_ = time.Now
}
