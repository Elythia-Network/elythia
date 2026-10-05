package reversi

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/require"
)

// TestService_UpdateReady_ConcurrentBothReady_StartsExactlyOnce is the
// regression test for #1626: when both players' ready updates run
// concurrently (local WebSocket handler vs federation inbox goroutine),
// neither update may be lost and the game must start with exactly one
// `started` event.
//
// 修正前のUpdateReadyはGet→full-row Saveのread-modify-writeだったため、
// 並行実行でstale stateが相手のready=trueを上書きし(lost update)、
// 両者readyが揃わずstartedが発火しないことがあった。-raceと反復実行で
// その退行を検出する。
func TestService_UpdateReady_ConcurrentBothReady_StartsExactlyOnce(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		game, repo, pub, svc := newPendingGame(t)

		var wg sync.WaitGroup
		wg.Add(2)
		errs := make([]error, 2)
		go func() {
			defer wg.Done()
			errs[0] = svc.UpdateReady(ctx, game.ID, "alice", true)
		}()
		go func() {
			defer wg.Done()
			errs[1] = svc.UpdateReady(ctx, game.ID, "bob", true)
		}()
		wg.Wait()
		require.NoError(t, errs[0], "iteration %d", i)
		require.NoError(t, errs[1], "iteration %d", i)

		got, err := repo.FindByID(game.ID)
		require.NoError(t, err)
		require.True(t, got.User1Ready, "iteration %d: user1Ready must not be lost", i)
		require.True(t, got.User2Ready, "iteration %d: user2Ready must not be lost", i)
		require.True(t, got.IsStarted, "iteration %d: both-ready must start the game", i)

		started := 0
		for _, kind := range pub.types() {
			if kind == "started" {
				started++
			}
		}
		require.Equal(t, 1, started, "iteration %d: started must be published exactly once", i)
	}
}

// readyGuardFailRepo simulates losing the UpdateReadyState guard race:
// pre-checkは通ったがUPDATE時点でstarted/ended/削除へ遷移していたケースを
// 決定論的に再現する。
type readyGuardFailRepo struct {
	*fakeRepo
	mutate func(r *fakeRepo)
}

func (r *readyGuardFailRepo) UpdateReadyState(_ string, _ bool, _ bool) (*model.ReversiGame, error) {
	if r.mutate != nil {
		r.mutate(r.fakeRepo)
	}
	return nil, nil
}

// TestService_UpdateReady_GuardFailMapsToStateError verifies the (nil, nil)
// guard result is mapped to the precise state error via re-fetch (#1626).
func TestService_UpdateReady_GuardFailMapsToStateError(t *testing.T) {
	ctx := context.Background()

	t.Run("game started concurrently", func(t *testing.T) {
		game, repo, _, _ := newPendingGame(t)
		wrapped := &readyGuardFailRepo{fakeRepo: repo, mutate: func(r *fakeRepo) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.games[game.ID].IsStarted = true
		}}
		svc := NewService(wrapped, &capturePublisher{}, nil)
		err := svc.UpdateReady(ctx, game.ID, "alice", true)
		require.ErrorIs(t, err, ErrAlreadyStarted)
	})

	t.Run("game ended concurrently", func(t *testing.T) {
		game, repo, _, _ := newPendingGame(t)
		wrapped := &readyGuardFailRepo{fakeRepo: repo, mutate: func(r *fakeRepo) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.games[game.ID].IsEnded = true
		}}
		svc := NewService(wrapped, &capturePublisher{}, nil)
		err := svc.UpdateReady(ctx, game.ID, "alice", true)
		require.ErrorIs(t, err, ErrAlreadyEnded)
	})

	t.Run("game deleted concurrently", func(t *testing.T) {
		game, repo, _, _ := newPendingGame(t)
		wrapped := &readyGuardFailRepo{fakeRepo: repo, mutate: func(r *fakeRepo) {
			r.mu.Lock()
			defer r.mu.Unlock()
			delete(r.games, game.ID)
		}}
		svc := NewService(wrapped, &capturePublisher{}, nil)
		err := svc.UpdateReady(ctx, game.ID, "alice", true)
		require.ErrorIs(t, err, ErrGameNotFound)
	})
}

func TestService_UpdateReady_RepoErrorPropagates(t *testing.T) {
	game, repo, _, svc := newPendingGame(t)
	repo.updateErr = errors.New("boom")
	require.Error(t, svc.UpdateReady(context.Background(), game.ID, "alice", true))
}

func TestService_StartGame_MarkStartedErrorPropagates(t *testing.T) {
	game, repo, _, svc := newPendingGame(t)
	repo.updateErr = errors.New("boom")
	require.Error(t, svc.StartGame(context.Background(), game))
}

// TestService_StartGame_ClaimLostIsSilentNoop verifies that a StartGame call
// losing the started claim neither errors nor double-publishes (#1626).
func TestService_StartGame_ClaimLostIsSilentNoop(t *testing.T) {
	ctx := context.Background()
	game, _, pub, svc := newPendingGame(t)

	require.NoError(t, svc.StartGame(ctx, game))

	// 並行goroutineが持つstale copy (IsStarted=false) からの二重到達を模す
	stale := *game
	stale.IsStarted = false
	require.NoError(t, svc.StartGame(ctx, &stale))

	started := 0
	for _, kind := range pub.types() {
		if kind == "started" {
			started++
		}
	}
	require.Equal(t, 1, started, "claim loser must not publish a second started event")
}
