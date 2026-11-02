package reversi

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// stream に出すイベントを、対局の行と一緒に観測者へも知らせる (#3942)。
// プラグインが管理するアカウントへの通知が、これを受けて対局者へ積む。

type observed struct {
	gameID, event, user1, user2 string
	ended                       bool
}

type captureObserver struct {
	mu     sync.Mutex
	events []observed
}

func (o *captureObserver) ReversiGameEvent(_ context.Context, game *model.ReversiGame, event string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events = append(o.events, observed{game.ID, event, game.User1ID, game.User2ID, game.IsEnded})
}

func (o *captureObserver) kinds() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]string, len(o.events))
	for i, e := range o.events {
		out[i] = e.event
	}
	return out
}

// 観測者が受けるイベントは、stream に出すものと同じ順で 1 対 1。
func assertObservedMatchesPublished(t *testing.T, obs *captureObserver, pub *capturePublisher, game *model.ReversiGame) {
	t.Helper()
	require.NotEmpty(t, obs.events)
	assert.Equal(t, pub.types(), obs.kinds())
	for _, e := range obs.events {
		assert.Equal(t, game.ID, e.gameID)
		// 対局者が分かる行を渡す (通知は対局者へ積むので、ID だけでは足りない)
		assert.Equal(t, "alice", e.user1)
		assert.Equal(t, "bob", e.user2)
	}
}

func TestService_Observer_ReadySettingsStartAndMoves(t *testing.T) {
	game, _, pub, svc := newPendingGame(t)
	obs := &captureObserver{}
	svc.SetGameObserver(obs)
	ctx := context.Background()

	require.NoError(t, svc.UpdateSettings(ctx, game.ID, "alice", "isLlotheo", json.RawMessage("true")))
	require.NoError(t, svc.UpdateSettings(ctx, game.ID, "alice", "isLlotheo", json.RawMessage("false")))
	require.NoError(t, svc.UpdateReady(ctx, game.ID, "alice", true))
	require.NoError(t, svc.UpdateReady(ctx, game.ID, "bob", true))
	require.NoError(t, svc.PutStone(ctx, game.ID, "alice", 19, ""))

	assertObservedMatchesPublished(t, obs, pub, game)
	for _, want := range []string{"updateSettings", "changeReadyStates", "started", "log"} {
		assert.Contains(t, obs.kinds(), want)
	}
}

func TestService_Observer_Surrender(t *testing.T) {
	game, _, pub, svc := startedGame(t)
	obs := &captureObserver{}
	svc.SetGameObserver(obs)
	pub.events = nil

	require.NoError(t, svc.Surrender(context.Background(), game.ID, "alice"))
	assertObservedMatchesPublished(t, obs, pub, game)
	assert.Equal(t, []string{"ended"}, obs.kinds())
	// 終わった後の行を渡す
	assert.True(t, obs.events[0].ended)
}

// 取り消しは行を消してから知らせるので、消す前に読んだ行を渡す。
func TestService_Observer_Cancel(t *testing.T) {
	game, _, pub, svc := newPendingGame(t)
	obs := &captureObserver{}
	svc.SetGameObserver(obs)

	require.NoError(t, svc.CancelGame(context.Background(), game.ID, "bob"))
	assertObservedMatchesPublished(t, obs, pub, game)
	assert.Equal(t, []string{"canceled"}, obs.kinds())
}

func TestService_Observer_Timeout(t *testing.T) {
	game, _, pub, svc := newStartedGameWithRedis(t)
	obs := &captureObserver{}
	svc.SetGameObserver(obs)
	pub.events = nil
	require.NoError(t, reversiTestRedis.Client.Del(context.Background(), turnTimerKey(game.ID, 0)).Err())

	require.NoError(t, svc.CheckTimeout(context.Background(), game.ID))
	assert.Equal(t, []string{"ended"}, obs.kinds())
	assert.Equal(t, pub.types(), obs.kinds())
	assert.Equal(t, game.ID, obs.events[0].gameID)
}

// 失敗した操作では知らせない (stream にも出さない)。
func TestService_Observer_NotToldOnFailure(t *testing.T) {
	game, _, _, svc := newPendingGame(t)
	obs := &captureObserver{}
	svc.SetGameObserver(obs)
	assert.ErrorIs(t, svc.UpdateReady(context.Background(), game.ID, "carol", true), ErrNotPlayer)
	assert.ErrorIs(t, svc.PutStone(context.Background(), game.ID, "alice", 19, ""), ErrNotStarted)
	assert.Empty(t, obs.events)
}

// barrierRepo holds each Update until two games have been read (or a short
// timeout passes), so that two concurrent moves both read the game before
// either writes, unless something serializes them.
type barrierRepo struct {
	*fakeRepo
	mu    sync.Mutex
	reads int
	read  chan struct{}
}

func newBarrierRepo(r *fakeRepo) *barrierRepo {
	return &barrierRepo{fakeRepo: r, read: make(chan struct{}, 8)}
}

func (r *barrierRepo) FindByID(id string) (*model.ReversiGame, error) {
	g, err := r.fakeRepo.FindByID(id)
	r.mu.Lock()
	r.reads++
	r.mu.Unlock()
	r.read <- struct{}{}
	return g, err
}

func (r *barrierRepo) Update(g *model.ReversiGame) error {
	deadline := time.After(300 * time.Millisecond)
	for {
		r.mu.Lock()
		n := r.reads
		r.mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case <-r.read:
		case <-deadline:
			return r.fakeRepo.Update(g)
		}
	}
	return r.fakeRepo.Update(g)
}

// 同じ手番に 2 手が同時に来ても、通るのは 1 手だけ。もう 1 手は最新の盤面で手番を
// 確かめ直して断る (#3942)。読んでから行ごと書き戻すので、直列にしないと両方が通り、
// 後から書いた方だけが残る。
func TestService_PutStone_SerializesConcurrentMoves(t *testing.T) {
	game, repo, pub, _ := newStartedGameWithRedis(t)
	svc := NewService(newBarrierRepo(repo), pub, reversiTestRedis.Client)
	pub.events = nil

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, pos := range []int{19, 26} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = svc.PutStone(context.Background(), game.ID, "alice", pos, "")
		}()
	}
	wg.Wait()

	var ok, notYourTurn int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case assert.ErrorIs(t, err, ErrNotYourTurn):
			notYourTurn++
		}
	}
	assert.Equal(t, 1, ok)
	assert.Equal(t, 1, notYourTurn)
	got, _ := repo.FindByID(game.ID)
	var logs [][]int
	require.NoError(t, json.Unmarshal(got.Logs, &logs))
	assert.Len(t, logs, 1)
	n := 0
	for _, k := range pub.types() {
		if k == "log" {
			n++
		}
	}
	assert.Equal(t, 1, n, "log は 1 回だけ配る")
}
