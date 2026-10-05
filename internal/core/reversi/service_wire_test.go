package reversi

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// --- in-memory repository ---

type fakeRepo struct {
	mu        sync.Mutex
	games     map[string]*model.ReversiGame
	findErr   error
	updateErr error
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{games: make(map[string]*model.ReversiGame)}
}

func (r *fakeRepo) Create(g *model.ReversiGame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	clone := *g
	r.games[g.ID] = &clone
	return nil
}

func (r *fakeRepo) FindByID(id string) (*model.ReversiGame, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.findErr != nil {
		return nil, r.findErr
	}
	if g, ok := r.games[id]; ok {
		clone := *g
		return &clone, nil
	}
	// **repository の sentinel を返す。** 汎用 error だと #2799 の
	// 「DB 障害は raw error のまま」に引っかかる。
	return nil, repository.ErrNotFound
}

func (r *fakeRepo) Update(g *model.ReversiGame) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updateErr != nil {
		return r.updateErr
	}
	clone := *g
	r.games[g.ID] = &clone
	return nil
}

func (r *fakeRepo) UpdateReadyState(gameID string, user1 bool, ready bool) (*model.ReversiGame, error) {
	r.mu.Lock()
	if r.updateErr != nil {
		r.mu.Unlock()
		return nil, r.updateErr
	}
	g, ok := r.games[gameID]
	if !ok || g.IsStarted || g.IsEnded {
		r.mu.Unlock()
		return nil, nil
	}
	// 実DB実装と同じく自分のカラムだけを書き換える (#1626)
	if user1 {
		g.User1Ready = ready
	} else {
		g.User2Ready = ready
	}
	r.mu.Unlock()
	// 実DB実装ではUPDATEとre-readが別ステートメントなので、両goroutineが
	// both-readyを観測してStartGameへ二重到達し得る。fakeも同じ二段構成に
	// して、MarkStartedのclaim排他をserviceテストで踏めるようにする。
	return r.FindByID(gameID)
}

func (r *fakeRepo) MarkStarted(g *model.ReversiGame) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updateErr != nil {
		return false, r.updateErr
	}
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

func (r *fakeRepo) FindPendingInvitation(inviteeID, inviterID string) (*model.ReversiGame, error) {
	games, err := r.ListByUser(inviteeID, 0)
	if err != nil {
		return nil, err
	}
	var found *model.ReversiGame
	for _, g := range games {
		if g.IsStarted || g.IsEnded {
			continue
		}
		if g.User1ID != inviterID || g.User2ID != inviteeID {
			continue
		}
		if found == nil || g.ID > found.ID {
			found = g
		}
	}
	return found, nil
}

func (r *fakeRepo) ListByUser(userID string, limit int) ([]*model.ReversiGame, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*model.ReversiGame, 0)
	for _, g := range r.games {
		if g.User1ID == userID || g.User2ID == userID {
			clone := *g
			out = append(out, &clone)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *fakeRepo) ListByUserCursor(_, _, _ string, _ int) ([]*model.ReversiGame, error) {
	return nil, nil
}

func (r *fakeRepo) ListStartedCursor(_, _ string, _ int) ([]*model.ReversiGame, error) {
	return nil, nil
}

func (r *fakeRepo) ListActive() ([]*model.ReversiGame, error) {
	return nil, nil
}

func (r *fakeRepo) Delete(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.games, id)
	return nil
}

func (r *fakeRepo) DeleteOutdatedGames(thresholdID string) (int64, error) {
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

// --- capture publisher ---

type capturePublisher struct {
	mu     sync.Mutex
	events []capturedEvent
}

type capturedEvent struct {
	gameID string
	kind   string
	body   any
}

func (p *capturePublisher) PublishGameEvent(gameID, eventType string, body any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, capturedEvent{gameID, eventType, body})
}

func (p *capturePublisher) types() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.events))
	for i, e := range p.events {
		out[i] = e.kind
	}
	return out
}

// --- helpers ---

func newPendingGame(t *testing.T) (*model.ReversiGame, *fakeRepo, *capturePublisher, *Service) {
	t.Helper()
	repo := newFakeRepo()
	pub := &capturePublisher{}
	svc := NewService(repo, pub, nil)
	game := &model.ReversiGame{
		ID:                   "g1",
		User1ID:              "alice",
		User2ID:              "bob",
		Map:                  model.StringArray{"--------", "--------", "--------", "---wb---", "---bw---", "--------", "--------", "--------"},
		BW:                   "1",
		TimeLimitForEachTurn: 90,
		Logs:                 datatypes.JSON("[]"),
	}
	require.NoError(t, repo.Create(game))
	return game, repo, pub, svc
}

// --- Ready / Start ---

func TestService_UpdateReady_Both_TriggersStart(t *testing.T) {
	game, repo, pub, svc := newPendingGame(t)
	ctx := context.Background()

	require.NoError(t, svc.UpdateReady(ctx, game.ID, "alice", true))
	require.NoError(t, svc.UpdateReady(ctx, game.ID, "bob", true))

	got, err := repo.FindByID(game.ID)
	require.NoError(t, err)
	assert.True(t, got.IsStarted)
	assert.NotNil(t, got.StartedAt)
	assert.NotNil(t, got.Black)

	// ready → ready → changeReadyStates, changeReadyStates, started
	kinds := pub.types()
	assert.Contains(t, kinds, "changeReadyStates")
	assert.Contains(t, kinds, "started")
}

// TS本家 reversiGame.changeReadyStates の body shape は
// `{ user1: boolean; user2: boolean }`。Go 側の emit がそのまま一致して
// いることを確認する (Phase 7-4 疎通検証)。
func TestService_UpdateReady_ChangeReadyStatesBodyShape(t *testing.T) {
	game, _, pub, svc := newPendingGame(t)
	ctx := context.Background()

	require.NoError(t, svc.UpdateReady(ctx, game.ID, "alice", true))

	// 最初の event が changeReadyStates で body が期待形式であることを確認。
	require.GreaterOrEqual(t, len(pub.events), 1)
	first := pub.events[0]
	assert.Equal(t, "g1", first.gameID)
	assert.Equal(t, "changeReadyStates", first.kind)
	body, ok := first.body.(map[string]any)
	require.True(t, ok, "body should be map")
	assert.Equal(t, true, body["user1"])
	assert.Equal(t, false, body["user2"])
	// TS 型にない余計なキーが混入していないこと。
	assert.Len(t, body, 2)
}

func TestService_UpdateReady_NotPlayer(t *testing.T) {
	game, _, _, svc := newPendingGame(t)
	err := svc.UpdateReady(context.Background(), game.ID, "carol", true)
	assert.ErrorIs(t, err, ErrNotPlayer)
}

func TestService_UpdateReady_GameNotFound(t *testing.T) {
	_, _, _, svc := newPendingGame(t)
	err := svc.UpdateReady(context.Background(), "ghost", "alice", true)
	assert.ErrorIs(t, err, ErrGameNotFound)
}

func TestService_UpdateReady_AlreadyStarted(t *testing.T) {
	game, repo, _, svc := newPendingGame(t)
	game.IsStarted = true
	_ = repo.Update(game)
	err := svc.UpdateReady(context.Background(), game.ID, "alice", true)
	assert.ErrorIs(t, err, ErrAlreadyStarted)
}

func TestService_UpdateReady_AlreadyEnded(t *testing.T) {
	game, repo, _, svc := newPendingGame(t)
	game.IsEnded = true
	_ = repo.Update(game)
	err := svc.UpdateReady(context.Background(), game.ID, "alice", true)
	assert.ErrorIs(t, err, ErrAlreadyEnded)
}

// --- Settings ---

func TestService_UpdateSettings_ChangesMapResetsReady(t *testing.T) {
	game, repo, pub, svc := newPendingGame(t)
	game.User1Ready = true
	_ = repo.Update(game)

	raw, _ := json.Marshal([]string{"bw", "wb"})
	err := svc.UpdateSettings(context.Background(), game.ID, "alice", "map", raw)
	require.NoError(t, err)

	got, _ := repo.FindByID(game.ID)
	assert.Equal(t, []string{"bw", "wb"}, []string(got.Map))
	assert.False(t, got.User1Ready)
	assert.Contains(t, pub.types(), "updateSettings")
	assert.Contains(t, pub.types(), "changeReadyStates")
}

func TestService_UpdateSettings_InvalidKey(t *testing.T) {
	game, _, _, svc := newPendingGame(t)
	err := svc.UpdateSettings(context.Background(), game.ID, "alice", "bogus", json.RawMessage(`null`))
	assert.ErrorIs(t, err, ErrInvalidSetting)
}

func TestService_UpdateSettings_NotPlayer(t *testing.T) {
	game, _, _, svc := newPendingGame(t)
	err := svc.UpdateSettings(context.Background(), game.ID, "carol", "isLlotheo", json.RawMessage(`true`))
	assert.ErrorIs(t, err, ErrNotPlayer)
}

func TestService_UpdateSettings_AlreadyStarted(t *testing.T) {
	game, repo, _, svc := newPendingGame(t)
	game.IsStarted = true
	_ = repo.Update(game)
	err := svc.UpdateSettings(context.Background(), game.ID, "alice", "isLlotheo", json.RawMessage(`true`))
	assert.ErrorIs(t, err, ErrAlreadyStarted)
}

func TestService_UpdateSettings_TypedKeys(t *testing.T) {
	game, repo, _, svc := newPendingGame(t)
	cases := []struct {
		key string
		val string
	}{
		{"bw", `"2"`},
		{"isLlotheo", `true`},
		{"canPutEverywhere", `true`},
		{"loopedBoard", `true`},
		{"timeLimitForEachTurn", `60`},
		{"noIrregularRules", `true`},
	}
	for _, tc := range cases {
		err := svc.UpdateSettings(context.Background(), game.ID, "alice", tc.key, json.RawMessage(tc.val))
		require.NoError(t, err, tc.key)
	}
	got, _ := repo.FindByID(game.ID)
	assert.Equal(t, "2", got.BW)
	assert.True(t, got.IsLlotheo)
	assert.True(t, got.CanPutEverywhere)
	assert.True(t, got.LoopedBoard)
	assert.Equal(t, 60, got.TimeLimitForEachTurn)
	assert.True(t, got.NoIrregularRules)
}

func TestService_UpdateSettings_BadJSON(t *testing.T) {
	game, _, _, svc := newPendingGame(t)
	err := svc.UpdateSettings(context.Background(), game.ID, "alice", "isLlotheo", json.RawMessage(`"not-bool"`))
	assert.ErrorIs(t, err, ErrInvalidSetting)
}

func TestService_UpdateSettings_NegativeTimeLimit(t *testing.T) {
	game, _, _, svc := newPendingGame(t)
	err := svc.UpdateSettings(context.Background(), game.ID, "alice", "timeLimitForEachTurn", json.RawMessage(`-1`))
	assert.ErrorIs(t, err, ErrInvalidSetting)
}

// --- Cancel ---

func TestService_CancelGame(t *testing.T) {
	game, repo, pub, svc := newPendingGame(t)
	require.NoError(t, svc.CancelGame(context.Background(), game.ID, "alice"))
	_, err := repo.FindByID(game.ID)
	assert.Error(t, err)
	assert.Contains(t, pub.types(), "canceled")
}

func TestService_CancelGame_NotPlayer(t *testing.T) {
	game, _, _, svc := newPendingGame(t)
	err := svc.CancelGame(context.Background(), game.ID, "carol")
	assert.ErrorIs(t, err, ErrNotPlayer)
}

func TestService_CancelGame_AlreadyStarted(t *testing.T) {
	game, repo, _, svc := newPendingGame(t)
	game.IsStarted = true
	_ = repo.Update(game)
	err := svc.CancelGame(context.Background(), game.ID, "alice")
	assert.ErrorIs(t, err, ErrAlreadyStarted)
}

// --- PutStone ---

func startedGame(t *testing.T) (*model.ReversiGame, *fakeRepo, *capturePublisher, *Service) {
	t.Helper()
	game, repo, pub, svc := newPendingGame(t)
	require.NoError(t, svc.UpdateReady(context.Background(), game.ID, "alice", true))
	require.NoError(t, svc.UpdateReady(context.Background(), game.ID, "bob", true))
	fresh, _ := repo.FindByID(game.ID)
	return fresh, repo, pub, svc
}

func TestService_PutStone_Valid(t *testing.T) {
	game, repo, pub, svc := startedGame(t)

	// alice is black (BW="1")
	// 8x8 default board: valid black moves include pos 19 (3,2), 26 (2,3), 37 (5,4), 44 (4,5)
	require.NoError(t, svc.PutStone(context.Background(), game.ID, "alice", 19, ""))

	got, _ := repo.FindByID(game.ID)
	var logs [][]int
	_ = json.Unmarshal(got.Logs, &logs)
	require.Len(t, logs, 1)
	// log shape: [timeDelta, player, operation, pos] (misskey-reversi format)
	require.Len(t, logs[0], 4)
	assert.Equal(t, 0, logs[0][2], "operation: 0 (put)")
	assert.Equal(t, 19, logs[0][3], "pos")

	// log event published
	types := pub.types()
	assert.Contains(t, types, "log")
}

// #1549: putStone の log event は client op id を含める。
func TestService_PutStone_LogIncludesOpID(t *testing.T) {
	game, _, pub, svc := startedGame(t)
	require.NoError(t, svc.PutStone(context.Background(), game.ID, "alice", 19, "op123"))
	var logBody map[string]any
	for _, e := range pub.events {
		if e.kind == "log" {
			logBody, _ = e.body.(map[string]any)
		}
	}
	require.NotNil(t, logBody, "log event published")
	assert.Equal(t, "op123", logBody["id"])
}

// op id が空のときは log.id を null (nil) で返す (upstream `id ?? null`)。
func TestService_PutStone_LogOpIDNilWhenEmpty(t *testing.T) {
	game, _, pub, svc := startedGame(t)
	require.NoError(t, svc.PutStone(context.Background(), game.ID, "alice", 19, ""))
	for _, e := range pub.events {
		if e.kind == "log" {
			body, _ := e.body.(map[string]any)
			assert.Nil(t, body["id"], "空 opID は null")
		}
	}
}

// TestService_CRC32MaintainedAcrossMoves guards that StartGame stores the
// initial board crc32 and PutStone refreshes it on every move (#1553).
// /reversi/verify は保存済み crc32 と比較する (upstream checkCrc 互換) ため、
// ここの更新が無いと対局中の verify が常に desynced になる。
func TestService_CRC32MaintainedAcrossMoves(t *testing.T) {
	game, repo, _, svc := startedGame(t)

	started, _ := repo.FindByID(game.ID)
	require.NotNil(t, started.CRC32, "開始時に初期盤面の crc32 を保存する")
	engine, err := EngineFromGame(started)
	require.NoError(t, err)
	assert.Equal(t, strconv.FormatUint(uint64(engine.CalcCRC32()), 10), *started.CRC32)

	require.NoError(t, svc.PutStone(context.Background(), game.ID, "alice", 19, ""))

	moved, _ := repo.FindByID(game.ID)
	require.NotNil(t, moved.CRC32)
	assert.NotEqual(t, *started.CRC32, *moved.CRC32, "一手ごとに crc32 を更新する")
	engine2, err := EngineFromGame(moved)
	require.NoError(t, err)
	assert.Equal(t, strconv.FormatUint(uint64(engine2.CalcCRC32()), 10), *moved.CRC32)
}

func TestService_PutStone_NotYourTurn(t *testing.T) {
	game, _, _, svc := startedGame(t)
	// bob is white; black moves first
	err := svc.PutStone(context.Background(), game.ID, "bob", 19, "")
	assert.ErrorIs(t, err, ErrNotYourTurn)
}

func TestService_PutStone_InvalidMove(t *testing.T) {
	game, _, _, svc := startedGame(t)
	err := svc.PutStone(context.Background(), game.ID, "alice", 0, "") // corner, not valid opening
	assert.ErrorIs(t, err, ErrInvalidMove)
}

func TestService_PutStone_NotStarted(t *testing.T) {
	game, _, _, svc := newPendingGame(t)
	err := svc.PutStone(context.Background(), game.ID, "alice", 19, "")
	assert.ErrorIs(t, err, ErrNotStarted)
}

func TestService_PutStone_AlreadyEnded(t *testing.T) {
	game, repo, _, svc := startedGame(t)
	game.IsEnded = true
	_ = repo.Update(game)
	err := svc.PutStone(context.Background(), game.ID, "alice", 19, "")
	assert.ErrorIs(t, err, ErrAlreadyEnded)
}

func TestService_PutStone_NotPlayer(t *testing.T) {
	game, _, _, svc := startedGame(t)
	err := svc.PutStone(context.Background(), game.ID, "carol", 19, "")
	assert.ErrorIs(t, err, ErrNotPlayer)
}

// --- Surrender ---

func TestService_Surrender_SetsOpponentAsWinner(t *testing.T) {
	game, repo, pub, svc := startedGame(t)
	require.NoError(t, svc.Surrender(context.Background(), game.ID, "alice"))

	got, _ := repo.FindByID(game.ID)
	assert.True(t, got.IsEnded)
	require.NotNil(t, got.WinnerID)
	assert.Equal(t, "bob", *got.WinnerID)
	require.NotNil(t, got.SurrenderedUserID)
	assert.Equal(t, "alice", *got.SurrenderedUserID)
	assert.Contains(t, pub.types(), "ended")
}

func TestService_Surrender_NotPlayer(t *testing.T) {
	game, _, _, svc := startedGame(t)
	err := svc.Surrender(context.Background(), game.ID, "carol")
	assert.ErrorIs(t, err, ErrNotPlayer)
}

func TestService_Surrender_AlreadyEnded(t *testing.T) {
	game, repo, _, svc := startedGame(t)
	game.IsEnded = true
	_ = repo.Update(game)
	err := svc.Surrender(context.Background(), game.ID, "alice")
	assert.ErrorIs(t, err, ErrAlreadyEnded)
}

func TestService_Surrender_NotStarted(t *testing.T) {
	// 対局開始前 (マッチメイキング中) に Surrender を呼ぶと ErrNotStarted。
	// 旧実装では IsStarted チェックが無かったため、未開始の対局でも
	// 一方的に敗北扱いになってしまう不具合があった。
	game, _, _, svc := newPendingGame(t)
	err := svc.Surrender(context.Background(), game.ID, "alice")
	assert.ErrorIs(t, err, ErrNotStarted)
}

// --- CheckTimeout (nil Redis skips the effective check) ---

func TestService_CheckTimeout_NilRedisIsNoop(t *testing.T) {
	// svc.redis is nil; turnTimerExists returns true → CheckTimeout returns nil
	game, _, _, svc := startedGame(t)
	err := svc.CheckTimeout(context.Background(), game.ID)
	assert.NoError(t, err)
}

func TestService_CheckTimeout_PendingGameNoop(t *testing.T) {
	game, _, _, svc := newPendingGame(t)
	err := svc.CheckTimeout(context.Background(), game.ID)
	assert.NoError(t, err)
}

func TestService_CheckTimeout_GameNotFound(t *testing.T) {
	_, _, _, svc := newPendingGame(t)
	err := svc.CheckTimeout(context.Background(), "ghost")
	assert.ErrorIs(t, err, ErrGameNotFound)
}

// --- helpers ---

func TestPickBlack_Explicit(t *testing.T) {
	assert.Equal(t, 1, pickBlack("1", ""))
	assert.Equal(t, 2, pickBlack("2", ""))
	// random mode returns 1 or 2
	for range 5 {
		v := pickBlack("random", "")
		assert.True(t, v == 1 || v == 2)
	}
}

// packGame が User1 / User2 に UserLite 互換 map を埋めることを確認
// (#417 Devin review: entity 依存を外したあとの自前 userLiteMap)。
func TestPackGame_EmbedsUserLiteMaps(t *testing.T) {
	remoteHost := "remote.example"
	name := "Alice"
	g := &model.ReversiGame{
		ID: "g1", User1ID: "alice", User2ID: "bob",
		User1: &model.User{ID: "alice", Username: "alice", Name: &name, Host: &remoteHost},
		User2: &model.User{ID: "bob", Username: "bob"},
	}
	out := packGame(g)
	u1, ok := out["user1"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "alice", u1["id"])
	assert.Equal(t, "alice", u1["username"])
	assert.Equal(t, &name, u1["name"])
	assert.Equal(t, &remoteHost, u1["host"])
	// avatar 未設定なら identicon URL が入る
	if url, ok := u1["avatarUrl"].(*string); ok && url != nil {
		assert.Contains(t, *url, "/identicon/alice")
	}
	u2, _ := out["user2"].(map[string]any)
	assert.Equal(t, "bob", u2["id"])
}

// TestPackGame_WinnerField は winnerId から winner UserLite が派生することを
// guard する。frontend (game.board.vue) の `v-if="game.winner"` 判定が
// winner 欠落で常に draw 表示になる #649 の regression 検知用。
func TestPackGame_WinnerField(t *testing.T) {
	user1 := &model.User{ID: "alice", Username: "alice"}
	user2 := &model.User{ID: "bob", Username: "bob"}
	winnerID := "alice"

	t.Run("user1 wins", func(t *testing.T) {
		g := &model.ReversiGame{
			ID: "g1", User1ID: "alice", User2ID: "bob",
			User1: user1, User2: user2,
			WinnerID: &winnerID,
		}
		out := packGame(g)
		w, ok := out["winner"].(map[string]any)
		require.True(t, ok, "winner must be present")
		assert.Equal(t, "alice", w["id"])
	})

	t.Run("user2 wins", func(t *testing.T) {
		bobID := "bob"
		g := &model.ReversiGame{
			ID: "g1", User1ID: "alice", User2ID: "bob",
			User1: user1, User2: user2,
			WinnerID: &bobID,
		}
		out := packGame(g)
		w, ok := out["winner"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "bob", w["id"])
	})

	t.Run("draw (winnerId nil)", func(t *testing.T) {
		g := &model.ReversiGame{
			ID: "g1", User1ID: "alice", User2ID: "bob",
			User1: user1, User2: user2,
		}
		out := packGame(g)
		_, has := out["winner"]
		assert.False(t, has, "winner must be omitted when WinnerID is nil")
	})

	t.Run("user objects not preloaded", func(t *testing.T) {
		// User1/User2 が preload されていない場合でも winnerId は出る
		// (UserLite は出せないので winner だけ省略する fallback 挙動)
		g := &model.ReversiGame{
			ID: "g1", User1ID: "alice", User2ID: "bob",
			WinnerID: &winnerID,
		}
		out := packGame(g)
		_, has := out["winner"]
		assert.False(t, has)
		assert.Equal(t, &winnerID, out["winnerId"])
	})
}

// sessionID が与えられれば random でも両サイドで一致する決定論的な値を返す。
func TestPickBlack_FederatedDeterministic(t *testing.T) {
	// 同じ session で複数回呼んでも同じ値
	v1 := pickBlack("random", "abc-123")
	v2 := pickBlack("random", "abc-123")
	assert.Equal(t, v1, v2)
	// 先頭 codePoint の偶奇で決まる (CherryPick 仕様)
	// "b" = 0x62 = 98 (偶) → 1
	// "a" = 0x61 = 97 (奇) → 2
	assert.Equal(t, 1, pickBlack("random", "b-xxx"))
	assert.Equal(t, 2, pickBlack("random", "a-xxx"))
}

func TestPlayerColor(t *testing.T) {
	b1 := 1
	g := &model.ReversiGame{User1ID: "a", User2ID: "b", Black: &b1}
	c, err := playerColor(g, "a")
	require.NoError(t, err)
	assert.Equal(t, Black, c)
	c, err = playerColor(g, "b")
	require.NoError(t, err)
	assert.Equal(t, White, c)
	b2 := 2
	g.Black = &b2
	c, _ = playerColor(g, "a")
	assert.Equal(t, White, c)
	c, _ = playerColor(g, "b")
	assert.Equal(t, Black, c)
}

func TestPlayerColor_BlackNil(t *testing.T) {
	g := &model.ReversiGame{User1ID: "a", User2ID: "b"}
	_, err := playerColor(g, "a")
	assert.Error(t, err)
}

func TestPlayerColor_NotPlayer(t *testing.T) {
	b1 := 1
	g := &model.ReversiGame{User1ID: "a", User2ID: "b", Black: &b1}
	_, err := playerColor(g, "c")
	assert.ErrorIs(t, err, ErrNotPlayer)
}

func TestPlayerIDForColor(t *testing.T) {
	b1 := 1
	g := &model.ReversiGame{User1ID: "a", User2ID: "b", Black: &b1}
	assert.Equal(t, "a", playerIDForColor(g, Black))
	assert.Equal(t, "b", playerIDForColor(g, White))
	b2 := 2
	g.Black = &b2
	assert.Equal(t, "b", playerIDForColor(g, Black))
	assert.Equal(t, "a", playerIDForColor(g, White))
	g.Black = nil
	assert.Equal(t, "", playerIDForColor(g, Black))
}

func TestIsPlayerOpponent(t *testing.T) {
	g := &model.ReversiGame{User1ID: "a", User2ID: "b"}
	assert.True(t, isPlayer(g, "a"))
	assert.True(t, isPlayer(g, "b"))
	assert.False(t, isPlayer(g, "c"))
	assert.Equal(t, "b", opponent(g, "a"))
	assert.Equal(t, "a", opponent(g, "b"))
}

func TestEngineFromGame_ReplaysLogs(t *testing.T) {
	game := &model.ReversiGame{
		Map:  model.StringArray{"--------", "--------", "--------", "---wb---", "---bw---", "--------", "--------", "--------"},
		Logs: datatypes.JSON(`[[19,1]]`),
	}
	engine, err := EngineFromGame(game)
	require.NoError(t, err)
	// alice put at pos 19; engine should now be at white's turn
	require.NotNil(t, engine.Turn)
	assert.Equal(t, White, *engine.Turn)
}

func TestEngineFromGame_BadLogs(t *testing.T) {
	game := &model.ReversiGame{
		Map:  model.StringArray{"--------"},
		Logs: datatypes.JSON(`not-json`),
	}
	_, err := EngineFromGame(game)
	assert.Error(t, err)
}

func TestService_Nil_NoPanic(t *testing.T) {
	s := NewService(newFakeRepo(), nil, nil)
	s.publish("g1", "t", nil) // no publisher → no-op
	s.setTurnTimer(context.Background(), "g1", 0, 90)

	_, err := s.turnTimerExists(context.Background(), "g1", 0)
	assert.NoError(t, err)

	// nil service is nil-safe where relevant
	assert.NotPanics(t, func() {
		s.SetFederationCache(NewFederationIDCache(nil))
	})
}

func TestApplySetting_AllCases(t *testing.T) {
	g := &model.ReversiGame{}
	// each branch already exercised via UpdateSettings; here we call directly
	// for the unknown-key path
	err := applySetting(g, "unknown", json.RawMessage(`null`))
	assert.ErrorIs(t, err, ErrInvalidSetting)
}

func TestService_PutStone_RestoresFromLogs(t *testing.T) {
	// Two moves: ensure the second move sees the board state from the first
	game, _, _, svc := startedGame(t)
	ctx := context.Background()
	require.NoError(t, svc.PutStone(ctx, game.ID, "alice", 19, "")) // black
	// after black at 19, white's valid moves include 18 (2,2) among others
	// alice is black here so bob is white
	require.NoError(t, svc.PutStone(ctx, game.ID, "bob", 18, ""))
}

// capture UpdateError path
func TestService_UpdateReady_UpdateError(t *testing.T) {
	game, repo, _, svc := newPendingGame(t)
	repo.updateErr = errors.New("db boom")
	err := svc.UpdateReady(context.Background(), game.ID, "alice", true)
	assert.Error(t, err)
}

func TestService_CancelGame_GameNotFound(t *testing.T) {
	_, _, _, svc := newPendingGame(t)
	err := svc.CancelGame(context.Background(), "ghost", "alice")
	assert.ErrorIs(t, err, ErrGameNotFound)
}

func TestService_Surrender_GameNotFound(t *testing.T) {
	_, _, _, svc := newPendingGame(t)
	err := svc.Surrender(context.Background(), "ghost", "alice")
	assert.ErrorIs(t, err, ErrGameNotFound)
}

func TestService_PutStone_GameNotFound(t *testing.T) {
	_, _, _, svc := newPendingGame(t)
	err := svc.PutStone(context.Background(), "ghost", "alice", 19, "")
	assert.ErrorIs(t, err, ErrGameNotFound)
}

// TestStartGame_AlreadyEnded ensures StartGame rejects terminal games.
func TestService_StartGame_AlreadyEnded(t *testing.T) {
	game, _, _, svc := newPendingGame(t)
	game.IsEnded = true
	err := svc.StartGame(context.Background(), game)
	assert.ErrorIs(t, err, ErrAlreadyEnded)
}

// ensure the SetFederationCache setter stores the passed cache
func TestSetFederationCache(t *testing.T) {
	s := NewService(newFakeRepo(), nil, nil)
	c := NewFederationIDCache(nil)
	s.SetFederationCache(c)
	assert.Same(t, c, s.fedCache)
}

// make sure time.Time zero-value is not stored for winnerID (no panic)
func TestService_Finalize_NoWinnerDraw(t *testing.T) {
	// craft a game whose engine.Winner() returns nil (draw)
	game, _, _, svc := newPendingGame(t)
	game.IsStarted = true
	b := 1
	game.Black = &b
	engine := NewGame([]string(game.Map), Options{})
	engine.Turn = nil
	require.NoError(t, svc.finalizeGame(context.Background(), game, engine))
	// IsEnded set, no winner
	assert.True(t, game.IsEnded)
	assert.NotNil(t, game.EndedAt)
	// Winner ID unset for draw
	assert.Nil(t, game.WinnerID)
	// Sanity on generated CRC32
	require.NotNil(t, game.CRC32)
	_ = time.Now
}
