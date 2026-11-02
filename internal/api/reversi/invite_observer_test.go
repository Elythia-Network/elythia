package reversi

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// ローカルの利用者への招待を、対局の ID と一緒に観測者へ知らせる (#3942)。
// プラグインが管理するアカウントへの通知が、これを受けて積む。

type invitedCall struct{ invitee, inviter, game string }

type stubInviteObserver struct{ calls []invitedCall }

func (s *stubInviteObserver) ReversiInvited(_ context.Context, inviteeID, inviterID, gameID string) {
	s.calls = append(s.calls, invitedCall{inviteeID, inviterID, gameID})
}

func newInviteHandler(t *testing.T) (*Handler, *mockReversiRepo, *stubInviteObserver) {
	t.Helper()
	h, repo := newTestHandler()
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u1"] = u1
	userRepo.Users["u2"] = &model.User{ID: "u2", Username: "bob"}
	host := "remote.example"
	uri := "https://remote.example/users/alice"
	userRepo.Users["remoteAlice"] = &model.User{ID: "remoteAlice", Username: "alice", Host: &host, URI: &uri}
	h.SetFederation("https://example.com", &mockDeliverer{}, corereversi.NewFederationIDCache(apiReversiRedis.Client), userRepo)
	obs := &stubInviteObserver{}
	h.SetInviteObserver(obs)
	return h, repo, obs
}

func onlyGame(t *testing.T, repo *mockReversiRepo) *model.ReversiGame {
	t.Helper()
	require.Len(t, repo.games, 1)
	for _, g := range repo.games {
		return g
	}
	return nil
}

// ストリームの配信 (SetStreamPublisher) が無くても知らせる。
func TestMatch_LocalInviteTellsObserverWithGame(t *testing.T) {
	h, repo, obs := newInviteHandler(t)
	rec := post(h.Match, `{"userId":"u2"}`, u1)
	require.Equal(t, http.StatusNoContent, rec.Code)

	game := onlyGame(t, repo)
	assert.Equal(t, []invitedCall{{"u2", "u1", game.ID}}, obs.calls)
}

func TestMatch_RemoteInviteDoesNotTellObserver(t *testing.T) {
	apiReversiRedis.FlushAll(context.Background())
	h, _, obs := newInviteHandler(t)
	rec := post(h.Match, `{"userId":"remoteAlice"}`, u1)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, obs.calls, "リモートの相手はこのサーバーのプラグインが管理しない")
}

// 招待を受ける側の match と、3 分以内の重ねての招待は、新しい招待ではない。
func TestMatch_AcceptAndDuplicateDoNotTellObserver(t *testing.T) {
	h, repo, obs := newInviteHandler(t)
	now := time.Now()
	require.NoError(t, repo.Create(&model.ReversiGame{
		ID: h.idGen.Generate(now), User1ID: "u2", User2ID: "u1", Map: defaultMap, BW: "random", TimeLimitForEachTurn: 90, Logs: datatypes.JSON("[]"),
	}))

	// u1 が u2 からの招待を受ける
	require.Equal(t, http.StatusOK, post(h.Match, `{"userId":"u2"}`, u1).Code)
	assert.Empty(t, obs.calls)

	// u2 がもう一度 u1 を招待する (自分発の招待がまだ受けられていない間は 204 を返すだけ)
	repo.games = map[string]*model.ReversiGame{}
	require.Equal(t, http.StatusNoContent, post(h.Match, `{"userId":"u1"}`, &model.User{ID: "u2", Username: "bob"}).Code)
	require.Len(t, obs.calls, 1)
	require.Equal(t, http.StatusNoContent, post(h.Match, `{"userId":"u1"}`, &model.User{ID: "u2", Username: "bob"}).Code)
	assert.Len(t, obs.calls, 1, "重ねての招待では知らせない")
}

// ランダムマッチで組まれたら、待っていた側に組まれた対局を知らせる。
func TestMatchAny_PairTellsObserverAboutWaitingSide(t *testing.T) {
	h, repo, _ := newMatchAnyHandler(t)
	obs := &stubInviteObserver{}
	h.SetInviteObserver(obs)

	require.Equal(t, http.StatusNoContent, post(h.Match, `{}`, u1).Code)
	assert.Empty(t, obs.calls, "待っている間は知らせない")
	require.Equal(t, http.StatusOK, post(h.Match, `{}`, matchAnyU2).Code)

	game := onlyGame(t, repo)
	assert.Equal(t, []invitedCall{{u1.ID, matchAnyU2.ID, game.ID}}, obs.calls)
}
