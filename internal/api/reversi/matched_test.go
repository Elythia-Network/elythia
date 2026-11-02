package reversi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// 相手を指定した招待が受けられたら、招待した人の画面を対局へ進める (#3946)。
// stream の matched を流し、招待した人の reversi/match も対局を返す。

func newMatchedHandler(t *testing.T) (*Handler, *mockReversiRepo, *stubStreamPub) {
	t.Helper()
	apiReversiRedis.FlushAll(context.Background())
	repo := newMock()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(repo, idGen, nil, nil)
	svc := corereversi.NewService(repo, nil, apiReversiRedis.Client)
	svc.SetMatchedNotifier(h)
	h.SetService(svc)
	pub := &stubStreamPub{}
	h.SetStreamPublisher(pub)
	return h, repo, pub
}

func gameIDOf(t *testing.T, body []byte) string {
	t.Helper()
	var g struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(body, &g))
	require.NotEmpty(t, g.ID)
	return g.ID
}

// 招待された側が reversi/match で受けると、招待した人に matched が流れ、招待した人の
// reversi/match も対局を返す。受ける前は 204 のまま。
func TestMatch_AcceptingTellsTheInviter(t *testing.T) {
	h, repo, pub := newMatchedHandler(t)

	require.Equal(t, http.StatusNoContent, post(h.Match, `{"userId":"u2"}`, u1).Code)
	game := onlyGame(t, repo)
	require.Equal(t, http.StatusNoContent, post(h.Match, `{"userId":"u2"}`, u1).Code, "受ける前は待ったまま")
	assert.Empty(t, pub.matched)

	rec := post(h.Match, `{"userId":"u1"}`, u2)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, game.ID, gameIDOf(t, rec.Body.Bytes()))

	require.Len(t, pub.matched, 1)
	assert.Equal(t, "u1", pub.matched[0].targetUserID, "招待した人へ流す")
	assert.Equal(t, game.ID, pub.matched[0].game["id"])

	rec = post(h.Match, `{"userId":"u2"}`, u1)
	require.Equal(t, http.StatusOK, rec.Code, "受けられた後は対局を返す")
	assert.Equal(t, game.ID, gameIDOf(t, rec.Body.Bytes()))
	assert.Len(t, repo.games, 1, "新しい招待を作らない")
}

// 招待から 3 分を過ぎてから受けられても、招待した人の reversi/match は新しい招待を
// 作らずに、受けられた対局を返す。招待した人はこの対局を待っていない (待っている印が
// 無い) ので、matched は流さない。
func TestMatch_AcceptedInvitationOutlivesTheDedupWindow(t *testing.T) {
	h, repo, pub := newMatchedHandler(t)
	old := &model.ReversiGame{
		ID: h.idGen.Generate(time.Now().Add(-10 * time.Minute)), User1ID: "u1", User2ID: "u2",
		Map: defaultMap, BW: "random", TimeLimitForEachTurn: 90, Logs: datatypes.JSON("[]"),
	}
	require.NoError(t, repo.Create(old))
	require.False(t, h.withinMatchDedupWindow(old))

	require.Equal(t, http.StatusOK, post(h.ShowGame, `{"gameId":"`+old.ID+`"}`, u2).Code)
	assert.Empty(t, pub.matched, "待っていない招待した人には流さない")

	rec := post(h.Match, `{"userId":"u2"}`, u1)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, old.ID, gameIDOf(t, rec.Body.Bytes()))
	assert.Len(t, repo.games, 1)
}

// 招待された側が対局を開く (show-game) のも受けたことになる。何度開いても matched は
// 1 回だけ。招待した人が開いても受けたことにはならない。
func TestShowGame_InviteeOpeningAcceptsOnce(t *testing.T) {
	h, repo, pub := newMatchedHandler(t)
	require.Equal(t, http.StatusNoContent, post(h.Match, `{"userId":"u2"}`, u1).Code)
	game := onlyGame(t, repo)
	body := `{"gameId":"` + game.ID + `"}`

	require.Equal(t, http.StatusOK, post(h.ShowGame, body, u1).Code)
	assert.Empty(t, pub.matched, "招待した人が開いても受けたことにならない")
	assert.Equal(t, http.StatusNoContent, post(h.Match, `{"userId":"u2"}`, u1).Code)

	require.Equal(t, http.StatusOK, post(h.ShowGame, body, u2).Code)
	require.Equal(t, http.StatusOK, post(h.ShowGame, body, u2).Code)
	require.Len(t, pub.matched, 1)
	assert.Equal(t, "u1", pub.matched[0].targetUserID)
}

// ランダムマッチで自分宛ての招待を受けたときも、招待した人に知らせる。
func TestMatchAny_AcceptingAnInvitationTellsTheInviter(t *testing.T) {
	h, repo, pub := newMatchedHandler(t)
	require.Equal(t, http.StatusNoContent, post(h.Match, `{"userId":"u2"}`, u1).Code)
	game := onlyGame(t, repo)

	rec := post(h.Match, `{}`, u2)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, game.ID, gameIDOf(t, rec.Body.Bytes()))
	require.Len(t, pub.matched, 1)
	assert.Equal(t, "u1", pub.matched[0].targetUserID)
}

// Redis が使えないときは、受けられたかが分からないので従来どおり 204 を返し、
// matched も流さない (流すと、招待された側の画面が呼び直すたびに流れる)。
func TestMatch_WithoutRedisFallsBackToWaiting(t *testing.T) {
	repo := newMock()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(repo, idGen, nil, nil)
	svc := corereversi.NewService(repo, nil, closedRedisClient(t))
	svc.SetMatchedNotifier(h)
	h.SetService(svc)
	pub := &stubStreamPub{}
	h.SetStreamPublisher(pub)

	require.Equal(t, http.StatusNoContent, post(h.Match, `{"userId":"u2"}`, u1).Code)
	require.Equal(t, http.StatusOK, post(h.Match, `{"userId":"u1"}`, u2).Code)
	assert.Empty(t, pub.matched)
	assert.Equal(t, http.StatusNoContent, post(h.Match, `{"userId":"u2"}`, u1).Code)
}

// 招待した人が去った後の古い招待を受けても、招待した人には流さない。別の相手を
// 待っている画面が、その対局へ移ってしまうため。待っている間の呼び直しは印を延ばす。
func TestMatch_AcceptingAnAbandonedInvitationDoesNotMoveTheInviter(t *testing.T) {
	h, repo, pub := newMatchedHandler(t)
	ctx := context.Background()
	require.Equal(t, http.StatusNoContent, post(h.Match, `{"userId":"u2"}`, u1).Code)
	old := onlyGame(t, repo)
	require.NoError(t, apiReversiRedis.Client.Del(ctx, "reversi:game:inviterWaiting:"+old.ID).Err())

	// 呼び直すと、待っている印が付き直る
	require.Equal(t, http.StatusNoContent, post(h.Match, `{"userId":"u2"}`, u1).Code)
	n, err := apiReversiRedis.Client.Exists(ctx, "reversi:game:inviterWaiting:"+old.ID).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 1, n)

	// 去った (印が切れた) 後に受けられても流さない
	require.NoError(t, apiReversiRedis.Client.Del(ctx, "reversi:game:inviterWaiting:"+old.ID).Err())
	require.Equal(t, http.StatusOK, post(h.Match, `{"userId":"u1"}`, u2).Code)
	assert.Empty(t, pub.matched)
}

// 受けられたまま放置された対局は、受けてから 3 分を過ぎたら返さない。同じ相手を
// 招待し直すと、本家と同じく新しい招待を作る。返し続けると、相手に招待が届かない。
func TestMatch_StaleAcceptedGameIsNotReturned(t *testing.T) {
	h, repo, _ := newMatchedHandler(t)
	ctx := context.Background()
	old := &model.ReversiGame{
		ID: h.idGen.Generate(time.Now().Add(-2 * time.Hour)), User1ID: "u1", User2ID: "u2",
		Map: defaultMap, BW: "random", TimeLimitForEachTurn: 90, Logs: datatypes.JSON("[]"),
	}
	require.NoError(t, repo.Create(old))
	acceptedAt := time.Now().Add(-4 * time.Minute).UnixMilli()
	require.NoError(t, apiReversiRedis.Client.Set(ctx, "reversi:game:accepted:"+old.ID, acceptedAt, time.Hour).Err())

	assert.Equal(t, http.StatusNoContent, post(h.Match, `{"userId":"u2"}`, u1).Code)
	assert.Len(t, repo.games, 2, "新しい招待を作る")
}
