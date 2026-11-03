package reversi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// 招待した人の cancel-match は、受けられた対局を取り消さない (#3949)。待機画面は
// 離れるときに必ず cancel-match を呼ぶので、受けられた後に画面を離れると対局が消えていた。

func newCancelHandler(t *testing.T) (*Handler, *mockReversiRepo, *capturingPublisher) {
	t.Helper()
	apiReversiRedis.FlushAll(context.Background())
	repo := newMock()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(repo, idGen, nil, nil)
	pub := &capturingPublisher{}
	svc := corereversi.NewService(cloningRepo{repo}, pub, apiReversiRedis.Client)
	svc.SetMatchedNotifier(h)
	h.SetService(svc)
	h.SetStreamPublisher(&stubStreamPub{})
	return h, repo, pub
}

func cancelEvents(pub *capturingPublisher) int {
	n := 0
	for _, e := range pub.events {
		if e == "canceled" {
			n++
		}
	}
	return n
}

// 受けられた後に招待した人が待機画面を離れても対局は残り、「自分の対局」から開いて
// 始められる。
func TestCancelMatch_KeepsAnAcceptedInvitation(t *testing.T) {
	h, repo, pub := newCancelHandler(t)
	require.Equal(t, http.StatusNoContent, post(h.Match, `{"userId":"u2"}`, u1).Code)
	game := onlyGame(t, repo)
	require.Equal(t, http.StatusOK, post(h.Match, `{"userId":"u1"}`, u2).Code)

	require.Equal(t, http.StatusNoContent, post(h.CancelMatch, `{"userId":"u2"}`, u1).Code)
	require.Contains(t, repo.games, game.ID, "受けられた対局は残る")
	assert.Zero(t, cancelEvents(pub))

	body := `{"gameId":"` + game.ID + `","ready":true}`
	require.Equal(t, http.StatusOK, post(h.ShowGame, `{"gameId":"`+game.ID+`"}`, u1).Code)
	require.Equal(t, http.StatusNoContent, post(h.Ready, body, u1).Code)
	require.Equal(t, http.StatusNoContent, post(h.Ready, body, u2).Code)
	assert.True(t, repo.games[game.ID].IsStarted)
}

// 受けられていない招待は、これまでどおり取り消す。
func TestCancelMatch_CancelsAnInvitationNotYetAccepted(t *testing.T) {
	h, repo, pub := newCancelHandler(t)
	require.Equal(t, http.StatusNoContent, post(h.Match, `{"userId":"u2"}`, u1).Code)
	require.Len(t, repo.games, 1)

	require.Equal(t, http.StatusNoContent, post(h.CancelMatch, `{"userId":"u2"}`, u1).Code)
	assert.Empty(t, repo.games)
	assert.Equal(t, 1, cancelEvents(pub))
}

// Redis が使えなくても、招待された側が準備を済ませていれば受けられたとみなして残す。
// どちらの手がかりも無ければ取り消す。
func TestCancelMatch_WithoutRedisFallsBackToTheReadyState(t *testing.T) {
	for _, ready := range []bool{true, false} {
		repo := newMock()
		idGen, _ := id.NewGenerator("aidx")
		h := NewHandler(repo, idGen, nil, nil)
		h.SetService(corereversi.NewService(cloningRepo{repo}, nil, closedRedisClient(t)))
		g := sampleGame()
		g.User2Ready = ready
		repo.games[g.ID] = g

		require.Equal(t, http.StatusNoContent, post(h.CancelMatch, `{"userId":"u2"}`, u1).Code)
		_, kept := repo.games[g.ID]
		assert.Equal(t, ready, kept, "user2Ready=%v", ready)
	}
}

// 連合: リモートの相手が受けた (Join が届いた) 対局は、招待した人が待機画面を離れても
// 残し、Undo(Invite) を送らない。送ると相手のサーバーの対局まで消える。
func TestCancelMatch_KeepsAnAcceptedFederatedInvitation(t *testing.T) {
	ctx := context.Background()
	apiReversiRedis.FlushAll(ctx)
	h, repo := newTestHandler()
	d := &mockDeliverer{}
	userRepo := testutil.NewMockUserRepository()
	host := "remote.example"
	uri := "https://remote.example/users/u2"
	userRepo.Users["u2"] = &model.User{ID: "u2", Username: "bob", Host: &host, URI: &uri}
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	g := sampleGame()
	repo.games[g.ID] = g
	fedCache := corereversi.NewFederationIDCache(apiReversiRedis.Client)
	fedCache.Set(ctx, "sess-accepted", g.ID)
	h.SetFederation("https://example.com", d, fedCache, userRepo)
	svc := corereversi.NewService(repo, nil, apiReversiRedis.Client)
	svc.SetFederationCache(fedCache)
	svc.SetFederationDeliverer(d)
	svc.SetUserRepo(userRepo)
	svc.SetBaseURL("https://example.com")
	h.SetService(svc)

	// 連合の Join を受けたときと同じく、受けた印を付ける (core/federation の handleReversiJoin)
	svc.AcceptInvitation(ctx, g)

	require.Equal(t, http.StatusNoContent, post(h.CancelMatch, `{"userId":"u2"}`, u1).Code)
	assert.Zero(t, d.calls, "Undo(Invite) を送らない")
	assert.Contains(t, repo.games, g.ID)
	_, ok := fedCache.GetSessionByGame(ctx, g.ID)
	assert.True(t, ok, "相手との対応も残す")
}

// staleAcceptedGame puts a game alice invited bob to an hour ago and bob
// accepted (the accepted mark is an hour old), which alice left without starting.
func staleAcceptedGame(t *testing.T, h *Handler, repo *mockReversiRepo) *model.ReversiGame {
	t.Helper()
	g := sampleGame()
	g.ID = h.idGen.Generate(time.Now().Add(-time.Hour))
	repo.games[g.ID] = g
	acceptedAt := time.Now().Add(-time.Hour).UnixMilli()
	require.NoError(t, apiReversiRedis.Client.Set(context.Background(), "reversi:game:accepted:"+g.ID, acceptedAt, 25*time.Hour).Err())
	return g
}

// 受けてから 3 分を過ぎた印でも、招待した人の cancel-match では残す。
func TestCancelMatch_KeepsAGameAcceptedLongAgo(t *testing.T) {
	h, repo, _ := newCancelHandler(t)
	g := staleAcceptedGame(t, h, repo)
	require.Equal(t, http.StatusNoContent, post(h.CancelMatch, `{"userId":"u2"}`, u1).Code)
	assert.Contains(t, repo.games, g.ID)
}

// 残した古い対局を、招待された側の操作が招待として拾わない。本家は受けた時点で招待を
// 消すので、相手のいない対局へ飛ばされない。
func TestStaleAcceptedGameIsNoLongerAnInvitation(t *testing.T) {
	t.Run("reverse invitation creates a new one", func(t *testing.T) {
		h, repo, _ := newCancelHandler(t)
		old := staleAcceptedGame(t, h, repo)
		require.Equal(t, http.StatusNoContent, post(h.Match, `{"userId":"u1"}`, u2).Code)
		require.Len(t, repo.games, 2)
		for id, g := range repo.games {
			if id != old.ID {
				assert.Equal(t, "u2", g.User1ID, "bob から alice への新しい招待")
			}
		}
	})
	t.Run("random match does not pick it", func(t *testing.T) {
		h, repo, _ := newCancelHandler(t)
		staleAcceptedGame(t, h, repo)
		assert.Equal(t, http.StatusNoContent, post(h.Match, `{}`, u2).Code)
	})
	t.Run("not listed as an invitation", func(t *testing.T) {
		h, repo, _ := newCancelHandler(t)
		staleAcceptedGame(t, h, repo)
		rec := post(h.Invitations, `{}`, u2)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `[]`, rec.Body.String())
	})
}

// 受けてから 3 分の間は、本家と同じく招待された側の reversi/match も対局を返す。
func TestRecentlyAcceptedGameIsStillReturnedToTheInvitee(t *testing.T) {
	h, repo, _ := newCancelHandler(t)
	require.Equal(t, http.StatusNoContent, post(h.Match, `{"userId":"u2"}`, u1).Code)
	game := onlyGame(t, repo)
	require.Equal(t, http.StatusOK, post(h.ShowGame, `{"gameId":"`+game.ID+`"}`, u2).Code)

	rec := post(h.Match, `{"userId":"u1"}`, u2)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, game.ID, gameIDOf(t, rec.Body.Bytes()))
	assert.Len(t, repo.games, 1)
}

// staleListRepo returns games as they were before the invitee got ready, as
// if the invitee accepted right after the list was read.
type staleListRepo struct{ *mockReversiRepo }

func (r staleListRepo) ListByUser(userID string, limit int) ([]*model.ReversiGame, error) {
	games, err := r.mockReversiRepo.ListByUser(userID, limit)
	out := make([]*model.ReversiGame, len(games))
	for i, g := range games {
		clone := *g
		clone.User2Ready = false
		out[i] = &clone
	}
	return out, err
}

// 一覧を取った後に受けられた対局は、取り消す直前に読み直して残す。
func TestCancelMatch_RechecksRightBeforeCancelling(t *testing.T) {
	apiReversiRedis.FlushAll(context.Background())
	repo := newMock()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(staleListRepo{repo}, idGen, nil, nil)
	h.SetService(corereversi.NewService(cloningRepo{repo}, nil, apiReversiRedis.Client))
	g := sampleGame()
	g.User2Ready = true
	repo.games[g.ID] = g

	require.Equal(t, http.StatusNoContent, post(h.CancelMatch, `{"userId":"u2"}`, u1).Code)
	assert.Contains(t, repo.games, g.ID)
}

// 受けた直後 (3 分以内) でも、受けた対局は招待一覧に出さない。本家は受けた時点で
// 招待を消す。
func TestInvitations_HidesAJustAcceptedGame(t *testing.T) {
	h, repo, _ := newCancelHandler(t)
	require.Equal(t, http.StatusNoContent, post(h.Match, `{"userId":"u2"}`, u1).Code)
	game := onlyGame(t, repo)
	// 実際の repo は招待した人を読み込む (Preload)。一覧はそこから組み立てる
	game.User1 = &model.User{ID: "u1", Username: "alice"}
	rec := post(h.Invitations, `{}`, u2)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[{"id":"u1"}]`, pickIDs(t, rec.Body.Bytes()), "受ける前は出る")

	require.Equal(t, http.StatusOK, post(h.ShowGame, `{"gameId":"`+game.ID+`"}`, u2).Code)
	rec = post(h.Invitations, `{}`, u2)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[]`, rec.Body.String())
}

// failOnceFindRepo fails the first FindByID only, like a momentary DB error.
type failOnceFindRepo struct {
	*mockReversiRepo
	failed *bool
}

func (r failOnceFindRepo) FindByID(id string) (*model.ReversiGame, error) {
	if !*r.failed {
		*r.failed = true
		return nil, errors.New("db down")
	}
	return r.mockReversiRepo.FindByID(id)
}

// 取り消す直前に読み直せないとき (DB の障害) は、取り消さない側に倒す。直後に
// 読めるようになっても、その呼び出しでは取り消さない。
func TestCancelMatch_KeepsTheGameWhenTheReloadFails(t *testing.T) {
	apiReversiRedis.FlushAll(context.Background())
	repo := newMock()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(repo, idGen, nil, nil)
	failed := false
	h.SetService(corereversi.NewService(failOnceFindRepo{repo, &failed}, nil, apiReversiRedis.Client))
	g := sampleGame()
	repo.games[g.ID] = g

	require.Equal(t, http.StatusNoContent, post(h.CancelMatch, `{"userId":"u2"}`, u1).Code)
	assert.Contains(t, repo.games, g.ID)
}

// pickIDs reduces a UserLite[] response to its ids.
func pickIDs(t *testing.T, body []byte) string {
	t.Helper()
	var users []struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(body, &users))
	out, err := json.Marshal(users)
	require.NoError(t, err)
	return string(out)
}

// 同じ相手から、受けられていない古い招待と、受けてから 3 分を過ぎた招待が並んでいても、
// 招待一覧に出た招待を受けられる。最も新しい 1 件だけを見ると、逆向きの招待になる。
func TestMatch_AcceptsAnOlderInvitationBehindAStaleAcceptedOne(t *testing.T) {
	h, repo, _ := newCancelHandler(t)
	unaccepted := sampleGame()
	unaccepted.ID = h.idGen.Generate(time.Now().Add(-2 * time.Hour))
	repo.games[unaccepted.ID] = unaccepted
	stale := staleAcceptedGame(t, h, repo)
	require.Greater(t, stale.ID, unaccepted.ID)

	rec := post(h.Invitations, `{}`, u2)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[{"id":"u1"}]`, pickIDs(t, rec.Body.Bytes()))

	rec = post(h.Match, `{"userId":"u1"}`, u2)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, unaccepted.ID, gameIDOf(t, rec.Body.Bytes()))
	assert.Len(t, repo.games, 2, "逆向きの招待を作らない")
}
