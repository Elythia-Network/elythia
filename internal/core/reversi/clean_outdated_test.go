package reversi

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// outdatedGame builds a pre-start game with preloaded players, as the
// repository returns it to the clean cron (#3956).
func outdatedGame(id, user1ID, user2ID string) *model.ReversiGame {
	remoteHost := "remote.example"
	remoteURI := "https://remote.example/users/bob"
	user := func(uid string) *model.User {
		if uid == "bob" {
			return &model.User{ID: uid, Username: uid, Host: &remoteHost, URI: &remoteURI}
		}
		return &model.User{ID: uid, Username: uid}
	}
	return &model.ReversiGame{
		ID:                   id,
		User1ID:              user1ID,
		User2ID:              user2ID,
		User1:                user(user1ID),
		User2:                user(user2ID),
		Map:                  model.StringArray{"--------", "--------", "--------", "---wb---", "---bw---", "--------", "--------", "--------"},
		BW:                   "1",
		TimeLimitForEachTurn: 90,
		Logs:                 datatypes.JSON("[]"),
	}
}

// newOutdatedService creates the game in a fresh fake repo and wires
// federation with a session mapping for it.
func newOutdatedService(t *testing.T, game *model.ReversiGame, sessionID string) (*Service, *fakeRepo, *captureDeliverer, *FederationIDCache) {
	t.Helper()
	reversiTestRedis.FlushAll(context.Background())
	repo := newFakeRepo()
	svc := NewService(repo, &capturePublisher{}, nil)
	require.NoError(t, repo.Create(game))
	d := wireFederated(t, svc, game, sessionID)
	return svc, repo, d, svc.fedCache
}

func activityType(t *testing.T, body []byte) string {
	t.Helper()
	var act map[string]any
	require.NoError(t, json.Unmarshal(body, &act))
	typ, _ := act["type"].(string)
	return typ
}

func TestService_CancelOutdatedFederatedGames(t *testing.T) {
	ctx := context.Background()

	t.Run("local inviter sends Undo(Invite) and forgets the session", func(t *testing.T) {
		game := outdatedGame("g-old", "alice", "bob")
		svc, repo, d, cache := newOutdatedService(t, game, "sess-clean-undo")

		listed, removed, err := svc.CancelOutdatedFederatedGames(ctx, "z", 100)
		require.NoError(t, err)
		assert.Equal(t, 1, listed)
		assert.Equal(t, 1, removed)

		require.Len(t, d.calls, 1)
		assert.Equal(t, "alice", d.calls[0].signerUserID)
		assert.Equal(t, "bob", d.calls[0].recipientID)
		assert.Equal(t, "Undo", activityType(t, d.calls[0].body))

		_, err = repo.FindByID(game.ID)
		require.Error(t, err)
		_, ok := cache.GetSessionByGame(ctx, game.ID)
		assert.False(t, ok, "the game -> session mapping is removed")
		_, err = cache.Get(ctx, "sess-clean-undo")
		assert.Error(t, err, "the session -> game mapping is removed")
	})

	t.Run("local invitee sends Leave", func(t *testing.T) {
		game := outdatedGame("g-old", "bob", "alice")
		svc, _, d, _ := newOutdatedService(t, game, "sess-clean-leave")

		_, removed, err := svc.CancelOutdatedFederatedGames(ctx, "z", 100)
		require.NoError(t, err)
		assert.Equal(t, 1, removed)
		require.Len(t, d.calls, 1)
		assert.Equal(t, "alice", d.calls[0].signerUserID)
		assert.Equal(t, "Leave", activityType(t, d.calls[0].body))
	})

	t.Run("game started after listing is kept and not notified", func(t *testing.T) {
		game := outdatedGame("g-old", "alice", "bob")
		svc, repo, d, cache := newOutdatedService(t, game, "sess-clean-started")
		repo.beforeDelete = func(id string) {
			repo.mu.Lock()
			repo.games[id].IsStarted = true
			repo.mu.Unlock()
		}

		listed, removed, err := svc.CancelOutdatedFederatedGames(ctx, "z", 100)
		require.NoError(t, err)
		assert.Equal(t, 1, listed)
		assert.Equal(t, 0, removed)
		assert.Empty(t, d.calls)
		_, err = repo.FindByID(game.ID)
		require.NoError(t, err, "a started game must survive the clean")
		_, ok := cache.GetSessionByGame(ctx, game.ID)
		assert.True(t, ok, "a started game keeps its session mapping")
	})

	t.Run("games newer than the threshold or without a remote player are untouched", func(t *testing.T) {
		game := outdatedGame("m-new", "alice", "bob")
		svc, repo, d, _ := newOutdatedService(t, game, "sess-clean-new")
		require.NoError(t, repo.Create(outdatedGame("a-local", "alice", "carol")))

		listed, removed, err := svc.CancelOutdatedFederatedGames(ctx, "k", 100)
		require.NoError(t, err)
		assert.Equal(t, 0, listed)
		assert.Equal(t, 0, removed)
		assert.Empty(t, d.calls)
		_, err = repo.FindByID("m-new")
		require.NoError(t, err)
		_, err = repo.FindByID("a-local")
		require.NoError(t, err)
	})

	t.Run("limit bounds one call", func(t *testing.T) {
		game := outdatedGame("a1", "alice", "bob")
		svc, repo, _, _ := newOutdatedService(t, game, "sess-clean-limit")
		require.NoError(t, repo.Create(outdatedGame("a2", "alice", "bob")))

		listed, removed, err := svc.CancelOutdatedFederatedGames(ctx, "z", 1)
		require.NoError(t, err)
		assert.Equal(t, 1, listed)
		assert.Equal(t, 1, removed)
		_, err = repo.FindByID("a1")
		require.Error(t, err, "the oldest game goes first")
		_, err = repo.FindByID("a2")
		require.NoError(t, err)
	})

	t.Run("list error is returned", func(t *testing.T) {
		game := outdatedGame("g-old", "alice", "bob")
		svc, repo, d, _ := newOutdatedService(t, game, "sess-clean-listerr")
		repo.listErr = errors.New("db down")

		_, _, err := svc.CancelOutdatedFederatedGames(ctx, "z", 100)
		require.ErrorIs(t, err, repo.listErr)
		assert.Empty(t, d.calls)
	})

	t.Run("delete error stops before notifying", func(t *testing.T) {
		game := outdatedGame("g-old", "alice", "bob")
		svc, repo, d, _ := newOutdatedService(t, game, "sess-clean-delerr")
		repo.deleteErr = errors.New("db down")

		listed, removed, err := svc.CancelOutdatedFederatedGames(ctx, "z", 100)
		require.ErrorIs(t, err, repo.deleteErr)
		assert.Equal(t, 1, listed)
		assert.Equal(t, 0, removed)
		assert.Empty(t, d.calls)
	})
}

func TestLocalPlayerID(t *testing.T) {
	assert.Equal(t, "alice", localPlayerID(outdatedGame("g", "alice", "bob")))
	assert.Equal(t, "alice", localPlayerID(outdatedGame("g", "bob", "alice")))
	assert.Empty(t, localPlayerID(outdatedGame("g", "bob", "bob")))
	assert.Empty(t, localPlayerID(&model.ReversiGame{User1ID: "x", User2ID: "y"}), "players not preloaded")
}

// The session mapping must outlive the longest wait before the daily clean
// (24h plus the 10 minute threshold), or the clean cannot notify (#3956).
func TestFederationIDCache_OutlivesTheDailyClean(t *testing.T) {
	ctx := context.Background()
	reversiTestRedis.FlushAll(ctx)
	cache := NewFederationIDCache(reversiTestRedis.Client)
	cache.Set(ctx, "sess-ttl", "g-ttl")
	cache.MarkJoinSent(ctx, "sess-ttl")

	for _, key := range []string{sessionToGameKey("sess-ttl"), gameToSessionKey("g-ttl"), joinSentKey("sess-ttl")} {
		ttl, err := reversiTestRedis.Client.TTL(ctx, key).Result()
		require.NoError(t, err)
		assert.Greater(t, ttl, 24*time.Hour+10*time.Minute, key)
	}
}
