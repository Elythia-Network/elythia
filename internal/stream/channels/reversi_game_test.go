package channels

import (
	"encoding/json"
	"testing"

	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// --- in-memory repo reused for channel tests ---

type channelFakeRepo struct {
	games map[string]*model.ReversiGame
}

func (r *channelFakeRepo) Create(g *model.ReversiGame) error {
	r.games[g.ID] = g
	return nil
}
func (r *channelFakeRepo) FindByID(id string) (*model.ReversiGame, error) {
	if g, ok := r.games[id]; ok {
		clone := *g
		return &clone, nil
	}
	return nil, assertError
}
func (r *channelFakeRepo) Update(g *model.ReversiGame) error {
	r.games[g.ID] = g
	return nil
}
func (r *channelFakeRepo) UpdateReadyState(gameID string, user1 bool, ready bool) (*model.ReversiGame, error) {
	g, ok := r.games[gameID]
	if !ok || g.IsStarted || g.IsEnded {
		return nil, nil
	}
	if user1 {
		g.User1Ready = ready
	} else {
		g.User2Ready = ready
	}
	clone := *g
	return &clone, nil
}
func (r *channelFakeRepo) MarkStarted(g *model.ReversiGame) (bool, error) {
	stored, ok := r.games[g.ID]
	if !ok || stored.IsStarted {
		return false, nil
	}
	stored.Black = g.Black
	stored.IsStarted = true
	stored.StartedAt = g.StartedAt
	stored.CRC32 = g.CRC32
	return true, nil
}
func (r *channelFakeRepo) FindPendingInvitation(_, _ string) (*model.ReversiGame, error) {
	return nil, nil
}

func (r *channelFakeRepo) ListByUser(_ string, _ int) ([]*model.ReversiGame, error) {
	return nil, nil
}
func (r *channelFakeRepo) ListByUserCursor(_, _, _ string, _ int) ([]*model.ReversiGame, error) {
	return nil, nil
}
func (r *channelFakeRepo) ListStartedCursor(_, _ string, _ int) ([]*model.ReversiGame, error) {
	return nil, nil
}
func (r *channelFakeRepo) ListActive() ([]*model.ReversiGame, error) { return nil, nil }
func (r *channelFakeRepo) Delete(id string) error {
	delete(r.games, id)
	return nil
}

func (r *channelFakeRepo) DeleteOutdatedGames(thresholdID string) (int64, error) {
	var n int64
	for id, g := range r.games {
		if id < thresholdID && !g.IsStarted {
			delete(r.games, id)
			n++
		}
	}
	return n, nil
}

var assertError = assertErr("not found")

type assertErr string

func (e assertErr) Error() string { return string(e) }

// --- channel conformance ---

var _ stream.Channel = (*ReversiGameChannel)(nil)

func newGameForChannel(t *testing.T) *model.ReversiGame {
	t.Helper()
	return &model.ReversiGame{
		ID:                   "g1",
		User1ID:              "alice",
		User2ID:              "bob",
		Map:                  model.StringArray{"--------", "--------", "--------", "---wb---", "---bw---", "--------", "--------", "--------"},
		BW:                   "1",
		TimeLimitForEachTurn: 90,
		Logs:                 datatypes.JSON("[]"),
	}
}

func newChannel(t *testing.T, user any) (*ReversiGameChannel, *stubContext, *channelFakeRepo, *model.ReversiGame) {
	t.Helper()
	repo := &channelFakeRepo{games: map[string]*model.ReversiGame{}}
	game := newGameForChannel(t)
	require.NoError(t, repo.Create(game))
	svc := corereversi.NewService(repo, nil, nil)
	factory := NewReversiGameFactory(svc)
	ctx := newCtx(user)
	ch := factory.New(ctx).(*ReversiGameChannel)
	return ch, ctx, repo, game
}

func TestReversiChannel_Init_SubscribesToTopic(t *testing.T) {
	ch, ctx, _, _ := newChannel(t, &model.User{ID: "alice"})
	require.NoError(t, ch.Init(json.RawMessage(`{"gameId":"g1"}`)))
	assert.Equal(t, []string{"reversiGame:g1"}, ctx.subs)
}

func TestReversiChannel_Init_MissingGameID(t *testing.T) {
	ch, ctx, _, _ := newChannel(t, nil)
	require.NoError(t, ch.Init(json.RawMessage(`{}`)))
	assert.Empty(t, ctx.subs)
}

func TestReversiChannel_Init_BadJSON(t *testing.T) {
	ch, ctx, _, _ := newChannel(t, nil)
	require.NoError(t, ch.Init(json.RawMessage(`not json`)))
	assert.Empty(t, ctx.subs)
}

func TestReversiChannel_OnRedisEvent_Forwards(t *testing.T) {
	ch, ctx, _, _ := newChannel(t, nil)
	require.NoError(t, ch.Init(json.RawMessage(`{"gameId":"g1"}`)))
	ch.OnRedisEvent([]byte(`{"type":"log","body":{"pos":19}}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "log", ctx.sentType[0])
}

func TestReversiChannel_OnRedisEvent_InvalidJSON(t *testing.T) {
	ch, ctx, _, _ := newChannel(t, nil)
	require.NoError(t, ch.Init(json.RawMessage(`{"gameId":"g1"}`)))
	ch.OnRedisEvent([]byte(`not-json`))
	assert.Empty(t, ctx.sentType)
}

func TestReversiChannel_OnRedisEvent_NoType(t *testing.T) {
	ch, ctx, _, _ := newChannel(t, nil)
	require.NoError(t, ch.Init(json.RawMessage(`{"gameId":"g1"}`)))
	ch.OnRedisEvent([]byte(`{"body":{}}`))
	assert.Empty(t, ctx.sentType)
}

func TestReversiChannel_OnClientMessage_ReadyValid(t *testing.T) {
	ch, _, repo, game := newChannel(t, &model.User{ID: "alice"})
	require.NoError(t, ch.Init(json.RawMessage(`{"gameId":"g1"}`)))
	ch.OnClientMessage("ready", json.RawMessage(`true`))
	got, _ := repo.FindByID(game.ID)
	assert.True(t, got.User1Ready)
}

func TestReversiChannel_OnClientMessage_Unauthenticated(t *testing.T) {
	ch, _, repo, _ := newChannel(t, nil)
	require.NoError(t, ch.Init(json.RawMessage(`{"gameId":"g1"}`)))
	ch.OnClientMessage("ready", json.RawMessage(`true`))
	got, _ := repo.FindByID("g1")
	assert.False(t, got.User1Ready, "unauthenticated clients must not mutate state")
}

func TestReversiChannel_OnClientMessage_NilService(t *testing.T) {
	// Direct construction with nil service → OnClientMessage is a no-op.
	factory := NewReversiGameFactory(nil)
	ctx := newCtx(&model.User{ID: "alice"})
	ch := factory.New(ctx).(*ReversiGameChannel)
	ch.gameID = "g1"
	ch.OnClientMessage("ready", json.RawMessage(`true`)) // no panic, no Send
	assert.Empty(t, ctx.sentType)
}

func TestReversiChannel_OnClientMessage_UpdateSettings(t *testing.T) {
	ch, _, repo, _ := newChannel(t, &model.User{ID: "alice"})
	require.NoError(t, ch.Init(json.RawMessage(`{"gameId":"g1"}`)))
	ch.OnClientMessage("updateSettings", json.RawMessage(`{"key":"isLlotheo","value":true}`))
	got, _ := repo.FindByID("g1")
	assert.True(t, got.IsLlotheo)
}

func TestReversiChannel_OnClientMessage_UpdateSettings_InvalidBody(t *testing.T) {
	ch, ctx, _, _ := newChannel(t, &model.User{ID: "alice"})
	require.NoError(t, ch.Init(json.RawMessage(`{"gameId":"g1"}`)))
	ch.OnClientMessage("updateSettings", json.RawMessage(`not-json`))
	assert.Empty(t, ctx.sentType) // bad body → silent drop
}

func TestReversiChannel_OnClientMessage_PutStone_PreStartError(t *testing.T) {
	ch, ctx, _, _ := newChannel(t, &model.User{ID: "alice"})
	require.NoError(t, ch.Init(json.RawMessage(`{"gameId":"g1"}`)))
	ch.OnClientMessage("putStone", json.RawMessage(`{"pos":19,"id":"x"}`))
	// Game not started → service returns error → channel emits "error" event.
	require.NotEmpty(t, ctx.sentType)
	assert.Equal(t, "error", ctx.sentType[0])
}

func TestReversiChannel_OnClientMessage_PutStone_BadJSON(t *testing.T) {
	ch, ctx, _, _ := newChannel(t, &model.User{ID: "alice"})
	require.NoError(t, ch.Init(json.RawMessage(`{"gameId":"g1"}`)))
	ch.OnClientMessage("putStone", json.RawMessage(`not-json`))
	assert.Empty(t, ctx.sentType)
}

func TestReversiChannel_OnClientMessage_Surrender(t *testing.T) {
	ch, ctx, repo, game := newChannel(t, &model.User{ID: "alice"})
	// force started state
	game.IsStarted = true
	b := 1
	game.Black = &b
	_ = repo.Update(game)

	require.NoError(t, ch.Init(json.RawMessage(`{"gameId":"g1"}`)))
	ch.OnClientMessage("surrender", json.RawMessage(`null`))
	got, _ := repo.FindByID("g1")
	assert.True(t, got.IsEnded)
	assert.Empty(t, ctx.sentType) // success → no error event
}

func TestReversiChannel_OnClientMessage_Cancel(t *testing.T) {
	ch, _, repo, _ := newChannel(t, &model.User{ID: "alice"})
	require.NoError(t, ch.Init(json.RawMessage(`{"gameId":"g1"}`)))
	ch.OnClientMessage("cancel", json.RawMessage(`null`))
	_, err := repo.FindByID("g1")
	assert.Error(t, err)
}

func TestReversiChannel_OnClientMessage_ClaimTimeIsUp(t *testing.T) {
	ch, ctx, _, _ := newChannel(t, &model.User{ID: "alice"})
	require.NoError(t, ch.Init(json.RawMessage(`{"gameId":"g1"}`)))
	// nil redis → svc.CheckTimeout returns nil without ending the game
	ch.OnClientMessage("claimTimeIsUp", json.RawMessage(`null`))
	assert.Empty(t, ctx.sentType)
}

func TestReversiChannel_OnClientMessage_UnknownType(t *testing.T) {
	ch, ctx, _, _ := newChannel(t, &model.User{ID: "alice"})
	require.NoError(t, ch.Init(json.RawMessage(`{"gameId":"g1"}`)))
	ch.OnClientMessage("bogus", json.RawMessage(`null`))
	assert.Empty(t, ctx.sentType)
}

func TestReversiChannel_OnClientMessage_NoGameID(t *testing.T) {
	ch, ctx, _, _ := newChannel(t, &model.User{ID: "alice"})
	// Init skipped → gameID empty → OnClientMessage early-return
	ch.OnClientMessage("ready", json.RawMessage(`true`))
	assert.Empty(t, ctx.sentType)
}

func TestReversiChannel_Dispose_Unsubscribes(t *testing.T) {
	ch, ctx, _, _ := newChannel(t, nil)
	require.NoError(t, ch.Init(json.RawMessage(`{"gameId":"g1"}`)))
	ch.Dispose()
	assert.Equal(t, []string{"reversiGame:g1"}, ctx.unsubs)
}

func TestReversiChannel_Dispose_WithoutInit(t *testing.T) {
	ch, ctx, _, _ := newChannel(t, nil)
	ch.Dispose() // no panic, no unsubscribe
	assert.Empty(t, ctx.unsubs)
}

func TestReversiChannel_OnClientMessage_ReadyBadJSON(t *testing.T) {
	ch, ctx, _, _ := newChannel(t, &model.User{ID: "alice"})
	require.NoError(t, ch.Init(json.RawMessage(`{"gameId":"g1"}`)))
	ch.OnClientMessage("ready", json.RawMessage(`not-a-bool`))
	assert.Empty(t, ctx.sentType)
}

func TestReversiChannel_OnClientMessage_SettingsEmptyKey(t *testing.T) {
	ch, ctx, _, _ := newChannel(t, &model.User{ID: "alice"})
	require.NoError(t, ch.Init(json.RawMessage(`{"gameId":"g1"}`)))
	ch.OnClientMessage("updateSettings", json.RawMessage(`{"key":"","value":true}`))
	assert.Empty(t, ctx.sentType)
}
