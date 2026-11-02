package reversi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
)

// 対局の操作の REST の入口 (#3942)。websocket の reversiGame チャンネルと同じ操作を、
// 同じ条件で断る。

var u2 = &model.User{ID: "u2", Username: "bob"}

// cloningRepo returns copies of the stored games, like the real database.
// mockReversiRepo は保存している行そのものを返すので、サービスが開始の印を書いた時点で
// 保存側も始まったことになり、MarkStarted が取り合いに負けたと判定して手番のタイマーを
// 置かない。
type cloningRepo struct{ *mockReversiRepo }

func (r cloningRepo) FindByID(id string) (*model.ReversiGame, error) {
	g, err := r.mockReversiRepo.FindByID(id)
	if err != nil {
		return nil, err
	}
	clone := *g
	return &clone, nil
}

func (r cloningRepo) UpdateReadyState(gameID string, user1, ready bool) (*model.ReversiGame, error) {
	g, err := r.mockReversiRepo.UpdateReadyState(gameID, user1, ready)
	if g == nil || err != nil {
		return g, err
	}
	clone := *g
	return &clone, nil
}

func newActionsHandler(t *testing.T) (*Handler, *mockReversiRepo) {
	t.Helper()
	apiReversiRedis.FlushAll(context.Background())
	h, repo := newTestHandler()
	h.SetService(corereversi.NewService(cloningRepo{repo}, nil, apiReversiRedis.Client))
	return h, repo
}

// u1 (黒) と u2 の、始まる前の対局を置く。
func seedPendingGame(t *testing.T, repo *mockReversiRepo, id string) *model.ReversiGame {
	t.Helper()
	g := &model.ReversiGame{
		ID: id, User1ID: "u1", User2ID: "u2",
		Map:                  model.StringArray{"--------", "--------", "--------", "---wb---", "---bw---", "--------", "--------", "--------"},
		BW:                   "1",
		TimeLimitForEachTurn: 90,
		Logs:                 datatypes.JSON("[]"),
	}
	require.NoError(t, repo.Create(g))
	return g
}

func startGame(t *testing.T, h *Handler, repo *mockReversiRepo, id string) {
	t.Helper()
	seedPendingGame(t, repo, id)
	require.Equal(t, http.StatusNoContent, post(h.Ready, `{"gameId":"`+id+`","ready":true}`, u1).Code)
	require.Equal(t, http.StatusNoContent, post(h.Ready, `{"gameId":"`+id+`","ready":true}`, u2).Code)
	require.True(t, repo.games[id].IsStarted)
}

func errorOf(t *testing.T, rec *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
			ID   string `json:"id"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body.Error.Code, body.Error.ID
}

func TestReady_StartsWhenBothAreReady(t *testing.T) {
	h, repo := newActionsHandler(t)
	seedPendingGame(t, repo, "g1")

	require.Equal(t, http.StatusNoContent, post(h.Ready, `{"gameId":"g1","ready":true}`, u1).Code)
	assert.True(t, repo.games["g1"].User1Ready)
	assert.False(t, repo.games["g1"].IsStarted)

	// 取り消せる
	require.Equal(t, http.StatusNoContent, post(h.Ready, `{"gameId":"g1","ready":false}`, u1).Code)
	assert.False(t, repo.games["g1"].User1Ready)

	require.Equal(t, http.StatusNoContent, post(h.Ready, `{"gameId":"g1","ready":true}`, u1).Code)
	require.Equal(t, http.StatusNoContent, post(h.Ready, `{"gameId":"g1","ready":true}`, u2).Code)
	assert.True(t, repo.games["g1"].IsStarted)
}

func TestReady_Errors(t *testing.T) {
	h, repo := newActionsHandler(t)
	seedPendingGame(t, repo, "pending")
	startGame(t, h, repo, "started")
	ended := seedPendingGame(t, repo, "ended")
	ended.IsEnded = true

	cases := []struct {
		name, body string
		user       *model.User
		code, id   string
	}{
		{"対局が無い", `{"gameId":"nope","ready":true}`, u1, "NO_SUCH_GAME", "08beb6e2-6cde-48c6-87db-a5ce07923cde"},
		{"対局者でない", `{"gameId":"pending","ready":true}`, &model.User{ID: "u3"}, "ACCESS_DENIED", "90727173-553f-42bd-8c91-c1bd8683527d"},
		{"始まっている", `{"gameId":"started","ready":true}`, u1, "ALREADY_STARTED", "c3edf082-e015-4235-9446-470f6dc95ee7"},
		{"終わっている", `{"gameId":"ended","ready":true}`, u1, "ALREADY_ENDED", "3f128b3c-340c-4b49-b117-91981b51dbaa"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := post(h.Ready, c.body, c.user)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			code, id := errorOf(t, rec)
			assert.Equal(t, c.code, code)
			assert.Equal(t, c.id, id)
		})
	}
}

func TestPutStone_PutsOnlyOnTheCallersTurn(t *testing.T) {
	h, repo := newActionsHandler(t)
	startGame(t, h, repo, "g1")

	// u1 が黒 (BW="1") で先手。本家の ajv と同じく 19.0 も 19 として受ける
	require.Equal(t, http.StatusNoContent, post(h.PutStone, `{"gameId":"g1","pos":19.0,"id":"op1"}`, u1).Code)
	var logs [][]int
	require.NoError(t, json.Unmarshal(repo.games["g1"].Logs, &logs))
	require.Len(t, logs, 1)
	assert.Equal(t, 19, logs[0][3])

	rec := post(h.PutStone, `{"gameId":"g1","pos":18}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	code, id := errorOf(t, rec)
	assert.Equal(t, "NOT_YOUR_TURN", code)
	assert.Equal(t, "1ff5c3c3-d4f5-4fc0-99a3-5f2d2a8b69aa", id)
}

func TestPutStone_Errors(t *testing.T) {
	h, repo := newActionsHandler(t)
	seedPendingGame(t, repo, "pending")
	startGame(t, h, repo, "started")
	startGame(t, h, repo, "ended")
	repo.games["ended"].IsEnded = true

	cases := []struct {
		name, body string
		user       *model.User
		code, id   string
	}{
		{"対局が無い", `{"gameId":"nope","pos":19}`, u1, "NO_SUCH_GAME", "602749a2-db0c-4978-bffa-b535be6b5c8e"},
		{"対局者でない", `{"gameId":"started","pos":19}`, &model.User{ID: "u3"}, "ACCESS_DENIED", "db6179ef-2feb-47b1-afad-d75423d718d2"},
		{"始まっていない", `{"gameId":"pending","pos":19}`, u1, "NOT_STARTED", "b2b320f2-cc77-4089-8fbe-5e40951001af"},
		{"終わっている", `{"gameId":"ended","pos":19}`, u1, "ALREADY_ENDED", "8bdf089c-34bb-4e22-91e9-13e82c62b5cd"},
		{"置けない場所", `{"gameId":"started","pos":0}`, u1, "INVALID_MOVE", "eb0041ee-40e2-4913-8d75-6e7ac73db7a3"},
		{"盤の外", `{"gameId":"started","pos":999}`, u1, "INVALID_MOVE", "eb0041ee-40e2-4913-8d75-6e7ac73db7a3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := post(h.PutStone, c.body, c.user)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			code, id := errorOf(t, rec)
			assert.Equal(t, c.code, code)
			assert.Equal(t, c.id, id)
		})
	}
}

func TestClaimTimeIsUp_EndsOnlyAfterTheTimer(t *testing.T) {
	h, repo := newActionsHandler(t)
	startGame(t, h, repo, "g1")

	// 時間が残っている間は何もしない。対局者でなくても呼べる (チャンネルと同じ)
	require.Equal(t, http.StatusNoContent, post(h.ClaimTimeIsUp, `{"gameId":"g1"}`, &model.User{ID: "u3"}).Code)
	assert.False(t, repo.games["g1"].IsEnded)

	require.NoError(t, apiReversiRedis.Client.Del(context.Background(), fmt.Sprintf("reversi:game:turnTimer:%s:%d", "g1", 0)).Err())
	require.Equal(t, http.StatusNoContent, post(h.ClaimTimeIsUp, `{"gameId":"g1"}`, u2).Code)
	assert.True(t, repo.games["g1"].IsEnded)
	require.NotNil(t, repo.games["g1"].WinnerID)
	assert.Equal(t, "u2", *repo.games["g1"].WinnerID, "手番だった u1 の時間切れ")

	rec := post(h.ClaimTimeIsUp, `{"gameId":"nope"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	code, id := errorOf(t, rec)
	assert.Equal(t, "NO_SUCH_GAME", code)
	assert.Equal(t, "971a87d5-5a0c-4517-9a90-cbd0f63309a2", id)
}

func TestActions_RejectMissingParams(t *testing.T) {
	h, _ := newActionsHandler(t)
	cases := map[string]struct {
		fn   func(echo.Context) error
		body string
	}{
		"ready の gameId が無い":            {h.Ready, `{"ready":true}`},
		"ready の ready が無い":             {h.Ready, `{"gameId":"g1"}`},
		"put-stone の pos が無い":           {h.PutStone, `{"gameId":"g1"}`},
		"put-stone の pos が小数":           {h.PutStone, `{"gameId":"g1","pos":1.5}`},
		"claim-time-is-up の gameId が無い": {h.ClaimTimeIsUp, `{}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			rec := post(c.fn, c.body, u1)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			code, _ := errorOf(t, rec)
			assert.Equal(t, "INVALID_PARAM", code)
		})
	}
}

// failingFindRepo makes FindByID fail like a database outage.
type failingFindRepo struct{ *mockReversiRepo }

func (failingFindRepo) FindByID(string) (*model.ReversiGame, error) {
	return nil, errors.New("db down")
}

// DB の障害は対局が無いことに丸めず、内部の文面も返さない。サービスが無いときも 500。
func TestActions_InternalErrors(t *testing.T) {
	h, _ := newTestHandler()
	for name, fn := range map[string]func(echo.Context) error{"ready": h.Ready, "put-stone": h.PutStone, "claim-time-is-up": h.ClaimTimeIsUp} {
		rec := post(fn, `{"gameId":"g1","ready":true,"pos":19}`, u1)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, "%s: サービスが無い", name)
	}

	h2, _ := newTestHandler()
	h2.SetService(corereversi.NewService(failingFindRepo{newMock()}, nil, apiReversiRedis.Client))
	for name, fn := range map[string]func(echo.Context) error{"ready": h2.Ready, "put-stone": h2.PutStone, "claim-time-is-up": h2.ClaimTimeIsUp} {
		rec := post(fn, `{"gameId":"g1","ready":true,"pos":19}`, u1)
		assert.Equal(t, http.StatusInternalServerError, rec.Code, name)
		assert.NotContains(t, rec.Body.String(), "db down", name)
	}
}

// postCanceled calls handler with a request whose context is already canceled
// (the client hung up right after sending it).
func postCanceled(handler func(echo.Context) error, body string, user *model.User) *httptest.ResponseRecorder {
	e := echo.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)).WithContext(ctx)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), user)
	_ = handler(c)
	return rec
}

func turnTimerExists(t *testing.T, gameID string, turn int) bool {
	t.Helper()
	n, err := apiReversiRedis.Client.Exists(context.Background(), fmt.Sprintf("reversi:game:turnTimer:%s:%d", gameID, turn)).Result()
	require.NoError(t, err)
	return n > 0
}

// 送った直後に接続を切っても、手番のタイマーを置く。置かないと、相手は時間切れを
// すぐに申告できる (接続を切るだけで相手を負けにできる)。
func TestActions_KeepTheTurnTimerWhenTheClientHangsUp(t *testing.T) {
	h, repo := newActionsHandler(t)
	seedPendingGame(t, repo, "g1")
	require.Equal(t, http.StatusNoContent, post(h.Ready, `{"gameId":"g1","ready":true}`, u1).Code)
	require.Equal(t, http.StatusNoContent, postCanceled(h.Ready, `{"gameId":"g1","ready":true}`, u2).Code)
	require.True(t, repo.games["g1"].IsStarted)
	assert.True(t, turnTimerExists(t, "g1", 0), "対局を始めた ready で最初の手番のタイマーを置く")

	require.Equal(t, http.StatusNoContent, postCanceled(h.PutStone, `{"gameId":"g1","pos":19}`, u1).Code)
	assert.True(t, turnTimerExists(t, "g1", 1), "手を打った後の手番のタイマーを置く")

	require.Equal(t, http.StatusNoContent, post(h.ClaimTimeIsUp, `{"gameId":"g1"}`, u1).Code)
	assert.False(t, repo.games["g1"].IsEnded, "時間は残っているので時間切れにならない")
}

type capturedStreamEvent struct {
	kind string
	body any
}

type capturingGamePublisher struct {
	mu     sync.Mutex
	events []capturedStreamEvent
}

func (p *capturingGamePublisher) PublishGameEvent(_ string, eventType string, body any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, capturedStreamEvent{eventType, body})
}

// id はチャンネルの putStone と同じく、stream の log イベントにそのまま載る。
func TestPutStone_EchoesTheOperationIDInTheLog(t *testing.T) {
	apiReversiRedis.FlushAll(context.Background())
	h, repo := newTestHandler()
	pub := &capturingGamePublisher{}
	h.SetService(corereversi.NewService(cloningRepo{repo}, pub, apiReversiRedis.Client))
	startGame(t, h, repo, "g1")

	require.Equal(t, http.StatusNoContent, post(h.PutStone, `{"gameId":"g1","pos":19,"id":"op-42"}`, u1).Code)
	var log map[string]any
	for _, e := range pub.events {
		if e.kind == "log" {
			log, _ = e.body.(map[string]any)
		}
	}
	require.NotNil(t, log)
	assert.Equal(t, "op-42", log["id"])
}
