package reversi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"errors"

	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// matchAnyU2 is the second local user used by the random-match tests.
// handler_test.go の u1 と対になる。
var matchAnyU2 = &model.User{ID: "u2", Username: "bob"}

// errCreateFailed is a sentinel used to force repo.Create failures.
var errCreateFailed = errors.New("create failed")

// newMatchAnyHandler wires a handler with a real-Redis service so the waiting
// queue actually works.
func newMatchAnyHandler(t *testing.T) (*Handler, *mockReversiRepo, *stubStreamPub) {
	t.Helper()
	apiReversiRedis.FlushAll(context.Background())
	repo := newMock()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(repo, idGen)
	h.SetService(corereversi.NewService(repo, nil, apiReversiRedis.Client))
	pub := &stubStreamPub{}
	h.SetStreamPublisher(pub)
	return h, repo, pub
}

// 最初の 1 人は待機列に入るだけ (204)。仮置き game 行は作らない。
func TestMatchAny_FirstCallerWaits(t *testing.T) {
	h, repo, _ := newMatchAnyHandler(t)

	rec := post(h.Match, `{}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
	assert.Empty(t, repo.games, "待機中は game 行を作らない")
}

// 2 人目が来たらペアが成立し、対局が 1 つだけ作られる。
func TestMatchAny_SecondCallerPairs(t *testing.T) {
	h, repo, pub := newMatchAnyHandler(t)

	require.Equal(t, http.StatusNoContent, post(h.Match, `{}`, u1).Code)

	rec := post(h.Match, `{}`, matchAnyU2)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"id"`)
	require.Len(t, repo.games, 1, "対局は 1 つだけ")

	for _, g := range repo.games {
		assert.False(t, g.IsStarted)
		// 待機していた側が User1、確保した側が User2。
		assert.Equal(t, u1.ID, g.User1ID)
		assert.Equal(t, matchAnyU2.ID, g.User2ID)
		assert.NotEqual(t, g.User1ID, g.User2ID, "自分自身との対局を作らない")
	}

	// 待機していた側は heartbeat を待たずに対局へ入れるよう通知する。
	require.Len(t, pub.calls, 1)
	assert.Equal(t, u1.ID, pub.calls[0].targetUserID)
}

// heartbeat で何度呼んでも、相手が居なければ対局は生まれない。
func TestMatchAny_HeartbeatDoesNotSelfPair(t *testing.T) {
	h, repo, _ := newMatchAnyHandler(t)

	for range 5 {
		rec := post(h.Match, `{}`, u1)
		require.Equal(t, http.StatusNoContent, rec.Code)
	}
	assert.Empty(t, repo.games, "自分自身とマッチしてはいけない")
}

// 直近の未開始対局があれば、待機列に入らずそれを返す (upstream の再利用)。
func TestMatchAny_ReusesExistingPendingGame(t *testing.T) {
	h, repo, _ := newMatchAnyHandler(t)
	idGen, _ := id.NewGenerator("aidx")
	existing := &model.ReversiGame{
		ID: idGen.Generate(time.Now()), User1ID: u1.ID, User2ID: matchAnyU2.ID,
	}
	repo.games[existing.ID] = existing

	rec := post(h.Match, `{}`, u1)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), existing.ID)
	assert.Len(t, repo.games, 1, "新しい対局を作らない")
}

// 自分宛ての招待があれば、待機列より先にそれを受ける。名指しで誘っている相手が
// 居るのに無関係な相手とマッチしてはいけない。
func TestMatchAny_PrefersPendingInvitation(t *testing.T) {
	h, repo, _ := newMatchAnyHandler(t)
	ctx := context.Background()

	// 別の待機者を先に列へ入れておく。
	require.NoError(t, h.svc.CancelMatchAny(ctx, "waiting"))
	_, err := h.svc.EnqueueMatchAny(ctx, "waiting", false)
	require.NoError(t, err)

	idGen, _ := id.NewGenerator("aidx")
	invite := &model.ReversiGame{
		ID: idGen.Generate(time.Now()), User1ID: matchAnyU2.ID, User2ID: u1.ID,
	}
	repo.games[invite.ID] = invite

	rec := post(h.Match, `{}`, u1)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), invite.ID,
		"自分宛ての招待を優先する")
	assert.Len(t, repo.games, 1, "待機者と新しい対局を作らない")
}

// cancel-match (userId 無し) で待機列から外れる。外れないと、取り消したつもりの
// 利用者が他人にマッチされ続ける。
func TestCancelMatch_DequeuesFromMatchAny(t *testing.T) {
	h, _, _ := newMatchAnyHandler(t)
	ctx := context.Background()

	require.Equal(t, http.StatusNoContent, post(h.Match, `{}`, u1).Code)

	rec := post(h.CancelMatch, `{}`, u1)
	require.Equal(t, http.StatusNoContent, rec.Code)

	// u2 が来ても u1 とはマッチしない。
	res, err := h.svc.EnqueueMatchAny(ctx, matchAnyU2.ID, false)
	require.NoError(t, err)
	assert.False(t, res.Matched(), "取り消した利用者は確保されない")
}

// service 未配線でも 204 で応答する (旧挙動の互換)。
func TestMatchAny_WithoutServiceReturns204(t *testing.T) {
	h, repo := newTestHandler()
	rec := post(h.Match, `{}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, repo.games)
}

// 対局の作成に失敗したら相手を待機列に戻す。戻さないと、確保だけして対局が無い
// 「消えた待機者」になり、その利用者は heartbeat を回しても永久にマッチしない。
func TestMatchAny_CreateFailureRequeuesOpponent(t *testing.T) {
	h, repo, _ := newMatchAnyHandler(t)
	ctx := context.Background()

	require.Equal(t, http.StatusNoContent, post(h.Match, `{}`, u1).Code)

	repo.createErr = errCreateFailed
	rec := post(h.Match, `{}`, matchAnyU2)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, repo.games)

	// u1 は待機列に戻っているので、次の呼び出しで確保できる。
	repo.createErr = nil
	res, err := h.svc.EnqueueMatchAny(ctx, "u3", false)
	require.NoError(t, err)
	require.True(t, res.Matched(), "確保だけして対局が無い状態を残さない")
	assert.Equal(t, u1.ID, res.OpponentID)
}

// 開始済み / 終了済みの対局は再利用しない。終わった対局を返すと、フロントが
// 終局画面に戻されてマッチが進まない。
func TestMatchAny_IgnoresStartedAndEndedGames(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*model.ReversiGame)
	}{
		{"開始済み", func(g *model.ReversiGame) { g.IsStarted = true }},
		{"終了済み", func(g *model.ReversiGame) { g.IsEnded = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, repo, _ := newMatchAnyHandler(t)
			idGen, _ := id.NewGenerator("aidx")
			g := &model.ReversiGame{
				ID: idGen.Generate(time.Now()), User1ID: u1.ID, User2ID: matchAnyU2.ID,
			}
			tc.mut(g)
			repo.games[g.ID] = g

			rec := post(h.Match, `{}`, u1)
			assert.Equal(t, http.StatusNoContent, rec.Code, "待機列に入るだけ")
		})
	}
}

// multiple=true は既存対局の再利用を飛ばす (upstream ReversiService.ts:146)。
func TestMatchAny_MultipleSkipsReuse(t *testing.T) {
	h, repo, _ := newMatchAnyHandler(t)
	idGen, _ := id.NewGenerator("aidx")
	existing := &model.ReversiGame{
		ID: idGen.Generate(time.Now()), User1ID: u1.ID, User2ID: matchAnyU2.ID,
	}
	repo.games[existing.ID] = existing

	rec := post(h.Match, `{"multiple":true}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"multiple=true では既存対局を返さず待機列へ入る")
}

// 自分が User1 の招待 (= 自分が誘った側) は「自分宛ての招待」ではない。
// これを拾うと、相手が受けていないのに対局が始まったことになる。
func TestMatchAny_OwnOutgoingInviteIsNotIncoming(t *testing.T) {
	h, repo, _ := newMatchAnyHandler(t)
	idGen, _ := id.NewGenerator("aidx")
	// 3 分より前に作られた自分発の招待 (再利用ウィンドウ外)。
	outgoing := &model.ReversiGame{
		ID: idGen.Generate(time.Now().Add(-10 * time.Minute)),
		// mk-go では招待も game 行。User1=自分 = 自分が誘った側。
		User1ID: u1.ID, User2ID: matchAnyU2.ID,
	}
	repo.games[outgoing.ID] = outgoing

	rec := post(h.Match, `{}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"自分発の招待を「自分宛て」として受けてはいけない")
}

// 既存対局の探索に失敗しても待機列へ進む。ここで 500 にすると、DB が一時的に
// 不調なだけでランダムマッチが完全に止まる。
func TestMatchAny_ListFailureFallsThroughToQueue(t *testing.T) {
	h, repo, _ := newMatchAnyHandler(t)
	repo.listErr = errCreateFailed

	rec := post(h.Match, `{}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// 待機列が使えない (redis 障害) 場合は 500。待機列に入れていないのに 204 を
// 返すと、フロントは待機中だと思って永久に heartbeat を回す。
func TestMatchAny_QueueFailureReturns500(t *testing.T) {
	repo := newMock()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(repo, idGen)
	// 閉じた client を渡して redis 操作を必ず失敗させる。
	h.SetService(corereversi.NewService(repo, nil, closedRedisClient(t)))

	rec := post(h.Match, `{}`, u1)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// cancel-match も待機列が使えないだけで失敗させない (pending game の片付けは
// 続行する)。
func TestCancelMatch_QueueFailureStillCleansGames(t *testing.T) {
	repo := newMock()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(repo, idGen)
	h.SetService(corereversi.NewService(repo, nil, closedRedisClient(t)))

	rec := post(h.CancelMatch, `{}`, u1)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// closedRedisClient returns a client whose every command fails.
func closedRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	c := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	require.NoError(t, c.Close())
	return c
}
