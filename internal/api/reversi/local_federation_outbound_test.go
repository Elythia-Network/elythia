package reversi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
)

func TestMatch_RemoteInvitationRequiresFederationBeforeCreate(t *testing.T) {
	host := "remote.example"
	uri := "https://remote.example/users/bob"
	for _, status := range []model.FederationStatus{model.FederationStatusOff, model.FederationStatusPending} {
		t.Run(string(status), func(t *testing.T) {
			h, repo := newTestHandler()
			users := testutil.NewMockUserRepository()
			users.Users["remote"] = &model.User{ID: "remote", Username: "bob", Host: &host, URI: &uri}
			deliverer := &mockDeliverer{}
			h.SetFederation("https://local.example", deliverer, corereversi.NewFederationIDCache(nil), users)
			actor := *u1
			actor.FederationStatus = status

			rec := post(h.Match, `{"userId":"remote"}`, &actor)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "FEDERATION_DISABLED")
			assert.Empty(t, repo.games, "refused remote invite must be rejected before game creation")
			assert.Zero(t, deliverer.calls)
			assert.Nil(t, actor.FirstExternalReferenceAt)
		})
	}
}

func TestMatch_LocalOpponentRemainsAvailableWhenFederationIsOff(t *testing.T) {
	h, repo := newTestHandler()
	users := testutil.NewMockUserRepository()
	users.Users["u2"] = &model.User{ID: "u2", Username: "bob", FederationStatus: model.FederationStatusOn}
	h.SetFederation("https://local.example", nil, nil, users)
	actor := *u1
	actor.FederationStatus = model.FederationStatusOff

	rec := post(h.Match, `{"userId":"u2"}`, &actor)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, repo.games, 1)
}

func TestMatchAndShowGame_RemoteInviteAcceptRequiresFederation(t *testing.T) {
	host := "remote.example"
	uri := "https://remote.example/users/inviter"
	for _, status := range []model.FederationStatus{model.FederationStatusOff, model.FederationStatusPending} {
		t.Run(string(status), func(t *testing.T) {
			for _, route := range []string{"match", "random-match", "show-game"} {
				t.Run(route, func(t *testing.T) {
					h, repo := newTestHandler()
					users := testutil.NewMockUserRepository()
					users.Users["inviter"] = &model.User{ID: "inviter", Username: "alice", Host: &host, URI: &uri}
					deliverer := &mockDeliverer{}
					h.SetFederation("https://local.example", deliverer, corereversi.NewFederationIDCache(nil), users)
					repo.games["game"] = &model.ReversiGame{ID: "game", User1ID: "inviter", User2ID: u1.ID}
					actor := *u1
					actor.FederationStatus = status

					var rec *httptest.ResponseRecorder
					if route == "match" {
						rec = post(h.Match, `{"userId":"inviter"}`, &actor)
					} else if route == "random-match" {
						rec = post(h.Match, `{}`, &actor)
					} else {
						rec = post(h.ShowGame, `{"gameId":"game"}`, &actor)
					}
					assert.Equal(t, http.StatusBadRequest, rec.Code)
					assert.Contains(t, rec.Body.String(), "FEDERATION_DISABLED")
					assert.Zero(t, deliverer.calls, "refused accept must not deliver Join")
					assert.Contains(t, repo.games, "game", "the inbound invitation remains available")
					assert.Nil(t, actor.FirstExternalReferenceAt)
				})
			}
		})
	}
}
