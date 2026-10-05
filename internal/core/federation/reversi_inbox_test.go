package federation_test

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"sync"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	coreblocking "github.com/elythia-network/elythia/internal/core/blocking"
	"github.com/elythia-network/elythia/internal/core/federation"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// --- Redis for FederationIDCache ---

var fedTestRedis *testutil.TestRedis

func TestMain(m *testing.M) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		log.Fatalf("federation reversi test: redis setup failed: %v", err)
	}
	fedTestRedis = tr
	code := m.Run()
	fedTestRedis.Teardown(ctx)
	os.Exit(code)
}

// --- in-memory reversi repository ---

type fedFakeReversiRepo struct {
	mu    sync.Mutex
	games map[string]*model.ReversiGame
}

func newFedFakeReversiRepo() *fedFakeReversiRepo {
	return &fedFakeReversiRepo{games: make(map[string]*model.ReversiGame)}
}

func (r *fedFakeReversiRepo) Create(g *model.ReversiGame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	clone := *g
	r.games[g.ID] = &clone
	return nil
}

func (r *fedFakeReversiRepo) FindByID(id string) (*model.ReversiGame, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if g, ok := r.games[id]; ok {
		clone := *g
		return &clone, nil
	}
	return nil, assertError
}

func (r *fedFakeReversiRepo) Update(g *model.ReversiGame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	clone := *g
	r.games[g.ID] = &clone
	return nil
}

func (r *fedFakeReversiRepo) UpdateReadyState(gameID string, user1 bool, ready bool) (*model.ReversiGame, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
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

func (r *fedFakeReversiRepo) MarkStarted(g *model.ReversiGame) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
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

func (r *fedFakeReversiRepo) ListByUser(userID string, _ int) ([]*model.ReversiGame, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*model.ReversiGame, 0)
	for _, g := range r.games {
		if g.User1ID == userID || g.User2ID == userID {
			clone := *g
			out = append(out, &clone)
		}
	}
	return out, nil
}
func (r *fedFakeReversiRepo) FindPendingInvitation(inviteeID, inviterID string) (*model.ReversiGame, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var found *model.ReversiGame
	for _, g := range r.games {
		if g.IsStarted || g.IsEnded {
			continue
		}
		if g.User1ID != inviterID || g.User2ID != inviteeID {
			continue
		}
		if found == nil || g.ID > found.ID {
			clone := *g
			found = &clone
		}
	}
	return found, nil
}

func (r *fedFakeReversiRepo) ListByUserCursor(_, _, _ string, _ int) ([]*model.ReversiGame, error) {
	return nil, nil
}
func (r *fedFakeReversiRepo) ListStartedCursor(_, _ string, _ int) ([]*model.ReversiGame, error) {
	return nil, nil
}
func (r *fedFakeReversiRepo) ListActive() ([]*model.ReversiGame, error) { return nil, nil }
func (r *fedFakeReversiRepo) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.games, id)
	return nil
}

func (r *fedFakeReversiRepo) DeleteOutdatedGames(thresholdID string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for id, g := range r.games {
		if id < thresholdID && !g.IsStarted {
			delete(r.games, id)
			n++
		}
	}
	return n, nil
}

// game-by-session via Redis cache + in-memory map traversal helper.
func (r *fedFakeReversiRepo) findGameBySession(t *testing.T, cache *corereversi.FederationIDCache, sessionID string) *model.ReversiGame {
	t.Helper()
	gid, err := cache.Get(context.Background(), sessionID)
	if err != nil || gid == "" {
		return nil
	}
	g, err := r.FindByID(gid)
	if err != nil {
		return nil
	}
	return g
}

var assertError = assertErrSentinel("not found")

type assertErrSentinel string

func (e assertErrSentinel) Error() string { return string(e) }

// --- bundle helper ---

type reversiFedBundle struct {
	processor  *federation.Processor
	userRepo   *testutil.MockUserRepository
	gameRepo   *fedFakeReversiRepo
	reversiSvc *corereversi.Service
	fedCache   *corereversi.FederationIDCache
	idGen      id.Generator
}

func newReversiProcessor(t *testing.T) *reversiFedBundle {
	t.Helper()
	fedTestRedis.FlushAll(context.Background())

	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	processor := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)

	fedCache := corereversi.NewFederationIDCache(fedTestRedis.Client)
	gameRepo := newFedFakeReversiRepo()
	reversiSvc := corereversi.NewService(gameRepo, nil, fedTestRedis.Client)
	// Service 側にも fedCache を配線 (プロダクション router.go と同じ)。
	// Surrender/CancelGame/PutStone 終了時の cleanupFedCache が有効になる。
	reversiSvc.SetFederationCache(fedCache)
	processor.SetReversi(reversiSvc, gameRepo, idGen, fedCache)

	return &reversiFedBundle{
		processor:  processor,
		userRepo:   repo,
		gameRepo:   gameRepo,
		reversiSvc: reversiSvc,
		fedCache:   fedCache,
		idGen:      idGen,
	}
}

func registerLocalBob(t *testing.T, repo *testutil.MockUserRepository) {
	t.Helper()
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{
		ID: "bob", Username: "bob", UsernameLower: "bob", URI: &bobURI,
	}
}

func registerRemoteAlice(t *testing.T, repo *testutil.MockUserRepository) {
	t.Helper()
	aliceURI := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["alice"] = &model.User{
		ID: "alice", Username: "alice", UsernameLower: "alice",
		Host: &host, URI: &aliceURI,
	}
}

// seedFederatedGame creates an in-memory game row and registers its federation
// session id in Redis via FederationIDCache. The resulting game has user1=bob
// (local), user2=alice (remote), mirroring the "local player invited a remote
// opponent" flow.
func seedFederatedGame(t *testing.T, b *reversiFedBundle, sessionID string, started bool) *model.ReversiGame {
	t.Helper()
	bw := 1
	game := &model.ReversiGame{
		ID:                   "fedg-" + sessionID,
		User1ID:              "bob",
		User2ID:              "alice",
		Map:                  model.StringArray{"--------", "--------", "--------", "---wb---", "---bw---", "--------", "--------", "--------"},
		BW:                   "1",
		TimeLimitForEachTurn: 90,
		Logs:                 datatypes.JSON("[]"),
		IsStarted:            started,
	}
	if started {
		game.Black = &bw
	}
	require.NoError(t, b.gameRepo.Create(game))
	b.fedCache.Set(context.Background(), sessionID, game.ID)
	return game
}

// --- Invite ---

func TestReversiInbox_Invite_CreatesGame(t *testing.T) {
	b := newReversiProcessor(t)
	registerLocalBob(t, b.userRepo)

	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"to": "https://example.com/users/bob",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {"game_session_id": "sess-001"}
		}
	}`)
	require.NoError(t, b.processor.Process(body))

	g := b.gameRepo.findGameBySession(t, b.fedCache, "sess-001")
	require.NotNil(t, g)
	assert.NotEmpty(t, g.User1ID)
	assert.Equal(t, "bob", g.User2ID)
}

// リモートから受信した Invite で local user の reversi stream に
// `invited` イベントがリアルタイム push される (#417 P2)。
func TestReversiInbox_Invite_PublishesInvitedToStream(t *testing.T) {
	b := newReversiProcessor(t)
	registerLocalBob(t, b.userRepo)
	stub := &stubInvitedStreamPub{}
	b.processor.SetReversiStreamPublisher(stub)

	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"to": "https://example.com/users/bob",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {"game_session_id": "sess-p2"}
		}
	}`)
	require.NoError(t, b.processor.Process(body))

	require.Len(t, stub.calls, 1)
	assert.Equal(t, "bob", stub.calls[0].targetUserID)
	require.NotNil(t, stub.calls[0].inviter)
	// inviter は resolveActor 結果 (remote alice)
	assert.NotEmpty(t, stub.calls[0].inviter.ID)
}

type stubInvitedCall struct {
	targetUserID string
	inviter      *model.User
}

type stubInvitedStreamPub struct {
	calls []stubInvitedCall
}

func (s *stubInvitedStreamPub) PublishInvited(targetUserID string, inviter *model.User) {
	s.calls = append(s.calls, stubInvitedCall{targetUserID: targetUserID, inviter: inviter})
}

// CherryPick は ack が返らないと fresh session_id で Invite を 5 秒ごと再送
// する。同じ (inviter, invitee) の pending game は 1 つに集約し、最新の
// session_id に mapping を差し替える必要がある (#417 P1 UDS で発覚した増殖)。
func TestReversiInbox_Invite_RetryWithNewSessionReusesPendingGame(t *testing.T) {
	b := newReversiProcessor(t)
	registerLocalBob(t, b.userRepo)
	send := func(sessionID string) {
		body := []byte(`{
			"type": "Invite",
			"actor": "https://remote.example/users/alice",
			"to": "https://example.com/users/bob",
			"object": {
				"type": "Game",
				"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
				"game_state": {"game_session_id": "` + sessionID + `"}
			}
		}`)
		require.NoError(t, b.processor.Process(body))
	}

	send("sess-retry-1")
	send("sess-retry-2")
	send("sess-retry-3")

	// ゲームは 1 件のまま (dedup)
	assert.Len(t, b.gameRepo.games, 1)

	// 最新の session にだけ mapping が残っている
	latestGameID, err := b.fedCache.Get(context.Background(), "sess-retry-3")
	require.NoError(t, err)
	assert.NotEmpty(t, latestGameID)

	// 古い session は削除されている
	_, err = b.fedCache.Get(context.Background(), "sess-retry-1")
	assert.Error(t, err, "old session mapping must be cleaned up")
}

func TestReversiInbox_Invite_IdempotentOnResend(t *testing.T) {
	b := newReversiProcessor(t)
	registerLocalBob(t, b.userRepo)
	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"to": "https://example.com/users/bob",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {"game_session_id": "sess-dup"}
		}
	}`)
	require.NoError(t, b.processor.Process(body))
	require.NoError(t, b.processor.Process(body))
	assert.Len(t, b.gameRepo.games, 1)
}

func TestReversiInbox_Invite_MissingRecipient(t *testing.T) {
	b := newReversiProcessor(t)
	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {"game_session_id": "s"}
		}
	}`)
	assert.Error(t, b.processor.Process(body))
}

// プロダクション DB の local user は user.uri が NULL なので、inbound Invite
// の `to` が "{localBaseURL}/users/{id}" 形式のときは resolveTargetUser 経由で
// ID から引く必要がある (FindByURI 直だと NULL 列とは match しないため)。
// #417 P1 deploy で "recipient ... not found" エラーが出た regression を防ぐ。
func TestReversiInbox_Invite_RecipientLocalWithoutURI(t *testing.T) {
	b := newReversiProcessor(t)
	b.processor.SetLocalBaseURL("https://example.com")
	// local user は URI 未設定 (プロダクション挙動)
	b.userRepo.Users["localbob"] = &model.User{
		ID: "localbob", Username: "localbob", UsernameLower: "localbob",
	}
	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"to": "https://example.com/users/localbob",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {"game_session_id": "sess-local-noURI"}
		}
	}`)
	require.NoError(t, b.processor.Process(body))
	g := b.gameRepo.findGameBySession(t, b.fedCache, "sess-local-noURI")
	require.NotNil(t, g)
	assert.Equal(t, "localbob", g.User2ID)
}

func TestReversiInbox_Invite_RecipientNotLocal(t *testing.T) {
	b := newReversiProcessor(t)
	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"to": "https://example.com/users/ghost",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {"game_session_id": "s"}
		}
	}`)
	assert.Error(t, b.processor.Process(body))
}

func TestReversiInbox_Invite_NotReversiGame(t *testing.T) {
	b := newReversiProcessor(t)
	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"to": "https://example.com/users/bob",
		"object": {"type": "Other"}
	}`)
	assert.Error(t, b.processor.Process(body))
}

// --- Join ---

func TestReversiInbox_Join_KnownSession(t *testing.T) {
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	seedFederatedGame(t, b, "sess-join-1", false)

	body := []byte(`{
		"type": "Join",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {"game_session_id": "sess-join-1"}
		}
	}`)
	require.NoError(t, b.processor.Process(body))
}

func TestReversiInbox_Join_UnknownSession(t *testing.T) {
	b := newReversiProcessor(t)
	body := []byte(`{
		"type": "Join",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {"game_session_id": "ghost"}
		}
	}`)
	assert.Error(t, b.processor.Process(body))
}

// --- Leave ---

func TestReversiInbox_Leave_PreStartCancels(t *testing.T) {
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	seedFederatedGame(t, b, "sess-leave-pre", false)

	body := []byte(`{
		"type": "Leave",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {"game_session_id": "sess-leave-pre"}
		}
	}`)
	require.NoError(t, b.processor.Process(body))

	assert.Nil(t, b.gameRepo.findGameBySession(t, b.fedCache, "sess-leave-pre"))
}

func TestReversiInbox_Leave_StartedSurrenders(t *testing.T) {
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	seedFederatedGame(t, b, "sess-leave-started", true)

	body := []byte(`{
		"type": "Leave",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {"game_session_id": "sess-leave-started"}
		}
	}`)
	require.NoError(t, b.processor.Process(body))

	// ゲーム行は IsEnded で残る。fedCache 側の session mapping は Surrender
	// 成功時に削除される (#417 Devin review: orphan mapping 掃除)。
	g, err := b.gameRepo.FindByID("fedg-sess-leave-started")
	require.NoError(t, err)
	assert.True(t, g.IsEnded)
	require.NotNil(t, g.WinnerID)
	assert.Equal(t, "bob", *g.WinnerID)
	// session mapping は片付けられている
	_, cacheErr := b.fedCache.Get(context.Background(), "sess-leave-started")
	assert.Error(t, cacheErr, "session mapping must be cleaned up after Leave")
}

// --- Undo(Invite) — pre-start invitation retracted by remote inviter (#417 P4) ---

func TestReversiInbox_UndoInvite_CancelsPreStartGame(t *testing.T) {
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	// alice が招待した pre-start ゲーム (User1=bob はダミーで User1ID=alice
	// にしないと actor.ID を CancelGame に渡す時 not-player になってしまう)。
	// seedFederatedGame は User1=bob 固定なので手動構築する。
	ctx := context.Background()
	game := &model.ReversiGame{
		ID:                   "fedg-undo",
		User1ID:              "alice",
		User2ID:              "bob",
		Map:                  model.StringArray{"--------", "--------", "--------", "---wb---", "---bw---", "--------", "--------", "--------"},
		BW:                   "1",
		TimeLimitForEachTurn: 90,
		Logs:                 datatypes.JSON("[]"),
	}
	require.NoError(t, b.gameRepo.Create(game))
	b.fedCache.Set(ctx, "sess-undo", game.ID)

	body := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Invite",
			"actor": "https://remote.example/users/alice",
			"object": {
				"type": "Game",
				"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
				"game_state": {"game_session_id": "sess-undo"}
			}
		}
	}`)
	require.NoError(t, b.processor.Process(body))

	assert.Nil(t, b.gameRepo.findGameBySession(t, b.fedCache, "sess-undo"),
		"Undo(Invite) must delete the pre-start game and clean fedCache")
}

func TestReversiInbox_UndoInvite_StartedGameIgnored(t *testing.T) {
	// Undo(Invite) は pre-start 専用。started 後に届いた場合はエラーにせず
	// 黙って ack するのが仕様 (#417 P4)。
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	seedFederatedGame(t, b, "sess-undo-started", true)

	body := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Invite",
			"object": {
				"type": "Game",
				"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
				"game_state": {"game_session_id": "sess-undo-started"}
			}
		}
	}`)
	require.NoError(t, b.processor.Process(body))

	// started ゲームは無効な Undo で消滅しない
	g, err := b.gameRepo.FindByID("fedg-sess-undo-started")
	require.NoError(t, err)
	assert.True(t, g.IsStarted)
	assert.False(t, g.IsEnded)
}

func TestReversiInbox_UndoInvite_NonReversiReturnsUnsupported(t *testing.T) {
	// 非 reversi な Undo(Invite) (Group Invite 等) は ErrUnsupportedActivity
	// を返し、inbox handler で 202 ack されるべき (#417 P4 Devin review)。
	// 400 にしてしまうと peer の配送 retry を招く。
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	body := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Invite",
			"object": {
				"type": "Group",
				"id": "https://remote.example/groups/42"
			}
		}
	}`)
	err := b.processor.Process(body)
	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
}

func TestReversiInbox_Invite_NonReversiReturnsUnsupported(t *testing.T) {
	// 非 reversi な top-level Invite (Group Invite 等) も同様に
	// ErrUnsupportedActivity を返す (pre-existing regression を同時に修正)。
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	registerLocalBob(t, b.userRepo)
	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"to": "https://example.com/users/bob",
		"object": {
			"type": "Group",
			"id": "https://remote.example/groups/42"
		}
	}`)
	err := b.processor.Process(body)
	assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
}

func TestReversiInbox_UndoInvite_UnknownSessionIgnored(t *testing.T) {
	// Session TTL 切れ後の Undo(Invite) は ack 扱いにする (#417 P4)。
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	body := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Invite",
			"object": {
				"type": "Game",
				"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
				"game_state": {"game_session_id": "ghost-undo"}
			}
		}
	}`)
	assert.NoError(t, b.processor.Process(body))
}

func TestReversiInbox_Leave_UnknownSession(t *testing.T) {
	b := newReversiProcessor(t)
	body := []byte(`{
		"type": "Leave",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {"game_session_id": "ghost"}
		}
	}`)
	assert.Error(t, b.processor.Process(body))
}

// --- EmojiReaction / Like over reversi game session URI (#417 P5) ---
//
// 純正 Misskey フロントは reversi `reacted` を表示する UI を持たないので
// mk-go では state 変化させず 202 ack だけ返す。Note Like より早く分岐
// しないと resolver.ResolveNote が 404 で失敗するため URI パターンで弾く。

func TestReversiInbox_Reaction_AcksGameURI(t *testing.T) {
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	body := []byte(`{
		"type": "EmojiReaction",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/games/1c086295-25e3-4b82-b31e-3e3959906312/sess-anything",
		"content": ":fire:",
		"_misskey_reaction": ":fire:"
	}`)
	require.NoError(t, b.processor.Process(body))
}

func TestReversiInbox_Reaction_LikeTypeAlsoAcked(t *testing.T) {
	// type: "Like" でも reversi URI なら同じく ack。
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	body := []byte(`{
		"type": "Like",
		"actor": "https://remote.example/users/alice",
		"object": "https://remote.example/games/1c086295-25e3-4b82-b31e-3e3959906312/sess-like",
		"content": "❤️"
	}`)
	require.NoError(t, b.processor.Process(body))
}

func TestReversiInbox_UndoReaction_AcksGameURI(t *testing.T) {
	// Undo(EmojiReaction) も reversi URI 対象なら ack 扱いにする
	// (#417 P5 Devin review: handleLike と handleUndoLike の対称化)。
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	body := []byte(`{
		"type": "Undo",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "EmojiReaction",
			"actor": "https://remote.example/users/alice",
			"object": "https://remote.example/games/1c086295-25e3-4b82-b31e-3e3959906312/sess-undo-react",
			"content": ":fire:"
		}
	}`)
	require.NoError(t, b.processor.Process(body))
}

// --- Update (reversi variant) ---

func TestReversiInbox_Update_ReadyStates(t *testing.T) {
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	seedFederatedGame(t, b, "sess-ready", false)

	body := []byte(`{
		"type": "Update",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {
				"game_session_id": "sess-ready",
				"type": "ready_states",
				"ready": true
			}
		}
	}`)
	require.NoError(t, b.processor.Process(body))

	g := b.gameRepo.findGameBySession(t, b.fedCache, "sess-ready")
	require.NotNil(t, g)
	// alice is user2; her ready flag should be true
	assert.True(t, g.User2Ready)
}

func TestReversiInbox_Update_Settings(t *testing.T) {
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	seedFederatedGame(t, b, "sess-settings", false)

	body := []byte(`{
		"type": "Update",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {
				"game_session_id": "sess-settings",
				"type": "settings",
				"key": "isLlotheo",
				"value": true
			}
		}
	}`)
	require.NoError(t, b.processor.Process(body))

	g := b.gameRepo.findGameBySession(t, b.fedCache, "sess-settings")
	require.NotNil(t, g)
	assert.True(t, g.IsLlotheo)
}

func TestReversiInbox_Update_PutStone(t *testing.T) {
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	seedFederatedGame(t, b, "sess-put", false)

	g := b.gameRepo.findGameBySession(t, b.fedCache, "sess-put")
	require.NotNil(t, g)
	require.NoError(t, b.reversiSvc.StartGame(context.Background(), g))
	_ = b.gameRepo.Update(g)

	// bob (local, black) moves first, then alice (remote, white) via Update
	require.NoError(t, b.reversiSvc.PutStone(context.Background(), g.ID, "bob", 19, ""))

	body := []byte(`{
		"type": "Update",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {
				"game_session_id": "sess-put",
				"type": "putstone",
				"pos": 18
			}
		}
	}`)
	require.NoError(t, b.processor.Process(body))

	fresh := b.gameRepo.findGameBySession(t, b.fedCache, "sess-put")
	require.NotNil(t, fresh)
	var logs [][]int
	_ = json.Unmarshal(fresh.Logs, &logs)
	assert.Len(t, logs, 2)
}

func TestReversiInbox_Update_UnknownGameStateType(t *testing.T) {
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	seedFederatedGame(t, b, "sess-u", false)

	body := []byte(`{
		"type": "Update",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {
				"game_session_id": "sess-u",
				"type": "bogus"
			}
		}
	}`)
	assert.Error(t, b.processor.Process(body))
}

func TestReversiInbox_Update_MissingReadyField(t *testing.T) {
	b := newReversiProcessor(t)
	registerRemoteAlice(t, b.userRepo)
	seedFederatedGame(t, b, "sess-r", false)

	body := []byte(`{
		"type": "Update",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {"game_session_id": "sess-r", "type": "ready_states"}
		}
	}`)
	assert.Error(t, b.processor.Process(body))
}

func TestReversiInbox_Update_NotReversiGame(t *testing.T) {
	b := newReversiProcessor(t)
	body := []byte(`{
		"type": "Update",
		"actor": "https://remote.example/users/alice",
		"object": {"type": "Note", "id": "https://remote.example/notes/x"}
	}`)
	err := b.processor.Process(body)
	assert.NoError(t, err)
}

// --- Unwired processor ---

func TestReversiInbox_Unwired_Unsupported(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)

	for _, typ := range []string{"Invite", "Join", "Leave"} {
		body := []byte(`{"type":"` + typ + `","actor":"https://remote.example/users/alice","object":{}}`)
		err := p.Process(body)
		assert.ErrorIs(t, err, federation.ErrUnsupportedActivity)
	}
}

// --- Invite の宛先 / ブロック判定 (セキュリティ監査 2026-09-21) ---

// registerRemoteCarol registers a second remote user so that a `to` pointing at
// a remote actor resolves via FindByURI.
func registerRemoteCarol(t *testing.T, repo *testutil.MockUserRepository) {
	t.Helper()
	carolURI := "https://other.example/users/carol"
	host := "other.example"
	repo.Users["carol"] = &model.User{
		ID: "carol", Username: "carol", UsernameLower: "carol",
		Host: &host, URI: &carolURI,
	}
}

// **招待先がリモートなら作らない。**
//
// `resolveTargetUser` はローカル prefix に一致しなければ `FindByURI` へ落ちる
// ので、`to` にリモートの actor URI を書くと解決されてしまう。見ないと、
// 第三者が「リモート同士の対局行」をこのサーバーに作れる。
func TestReversiInbox_Invite_RemoteRecipientRejected(t *testing.T) {
	b := newReversiProcessor(t)
	b.processor.SetLocalBaseURL("https://example.com")
	registerRemoteCarol(t, b.userRepo)

	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"to": "https://other.example/users/carol",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {"game_session_id": "sess-remote-to"}
		}
	}`)
	assert.Error(t, b.processor.Process(body))

	gid, err := b.fedCache.Get(context.Background(), "sess-remote-to")
	assert.True(t, err != nil || gid == "", "リモート宛の Invite で対局行を作らないこと")
}

// wireBlocking installs a blocking service backed by an in-memory repository.
func wireBlocking(t *testing.T, b *reversiFedBundle) *testutil.MockBlockingRepository {
	t.Helper()
	blockRepo := testutil.NewMockBlockingRepository()
	b.processor.SetBlockingService(coreblocking.NewService(
		b.userRepo, blockRepo, b.idGen,
	))
	return blockRepo
}

// **ブロックしている相手からの招待は通さない。**
//
// 通すと、ブロック済みのリモート利用者が対象の `reversi:<userID>` ストリームへ
// `invited` を push できる (= ブロックを迂回した接触経路になる)。
func TestReversiInbox_Invite_BlockedActorDropped(t *testing.T) {
	b := newReversiProcessor(t)
	b.processor.SetLocalBaseURL("https://example.com")
	registerRemoteAlice(t, b.userRepo)
	registerLocalBob(t, b.userRepo)
	blockRepo := wireBlocking(t, b)
	require.NoError(t, blockRepo.Create(&model.Blocking{
		ID: "blk-1", BlockerID: "bob", BlockeeID: "alice",
	}))
	stub := &stubInvitedStreamPub{}
	b.processor.SetReversiStreamPublisher(stub)

	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"to": "https://example.com/users/bob",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {"game_session_id": "sess-blocked"}
		}
	}`)
	// ブロック済みは「握り潰す」— 相手に配送失敗を返す必要はない。
	require.NoError(t, b.processor.Process(body))

	gid, err := b.fedCache.Get(context.Background(), "sess-blocked")
	assert.True(t, err != nil || gid == "", "ブロック済みの相手の対局行を作らないこと")
	assert.Empty(t, stub.calls, "ブロック済みの相手の invited を push しないこと")
}

// **判定できないなら通さない (#2792 と同じ向き)。**
func TestReversiInbox_Invite_BlockLookupFailureRejected(t *testing.T) {
	b := newReversiProcessor(t)
	b.processor.SetLocalBaseURL("https://example.com")
	registerRemoteAlice(t, b.userRepo)
	registerLocalBob(t, b.userRepo)
	blockRepo := wireBlocking(t, b)
	blockRepo.ExistsErr = errors.New("db down")

	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"to": "https://example.com/users/bob",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {"game_session_id": "sess-blockerr"}
		}
	}`)
	assert.Error(t, b.processor.Process(body))

	gid, err := b.fedCache.Get(context.Background(), "sess-blockerr")
	assert.True(t, err != nil || gid == "", "ブロック判定に失敗したら対局行を作らないこと")
}

// **ブロックしていない相手は通る。** 上 2 つが「常に落とす」実装でも緑に
// ならないようにする。
func TestReversiInbox_Invite_UnblockedActorPasses(t *testing.T) {
	b := newReversiProcessor(t)
	b.processor.SetLocalBaseURL("https://example.com")
	registerRemoteAlice(t, b.userRepo)
	registerLocalBob(t, b.userRepo)
	wireBlocking(t, b)

	body := []byte(`{
		"type": "Invite",
		"actor": "https://remote.example/users/alice",
		"to": "https://example.com/users/bob",
		"object": {
			"type": "Game",
			"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
			"game_state": {"game_session_id": "sess-unblocked"}
		}
	}`)
	require.NoError(t, b.processor.Process(body))
	g := b.gameRepo.findGameBySession(t, b.fedCache, "sess-unblocked")
	require.NotNil(t, g)
	assert.Equal(t, "bob", g.User2ID)
}
