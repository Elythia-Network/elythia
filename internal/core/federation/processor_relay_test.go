package federation_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/relay"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// 本家 2026.10.0 updateRequestingRelayStatus: relay の応答で status が動くのは
// requesting の間だけ。inbox 処理から relay.Service まで通して、確定した relay
// に遅れて届いた Accept / Reject が status を変えないことを固定する。
func TestProcess_FollowRelay_OnlyRequestingChanges(t *testing.T) {
	for _, tc := range []struct {
		name       string
		from, kind string
		want       string
	}{
		{"requesting then Accept", relay.StatusRequesting, "Accept", relay.StatusAccepted},
		{"requesting then Reject", relay.StatusRequesting, "Reject", relay.StatusRejected},
		{"accepted then late Reject", relay.StatusAccepted, "Reject", relay.StatusAccepted},
		{"rejected then late Accept", relay.StatusRejected, "Accept", relay.StatusRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, users, _, _ := newProcessor(t, aliceActor)
			p.SetLocalBaseURL("https://example.com")

			relayRepo := testutil.NewMockRelayRepository()
			require.NoError(t, relayRepo.Create(&model.Relay{ID: "rel123", Inbox: "https://relay.example/inbox", Status: tc.from}))
			idGen, err := id.NewGenerator("aidx")
			require.NoError(t, err)
			p.SetRelayMarker(relay.NewService(relayRepo, nil, nil, nil, idGen))
			seedRelayActor(users, "https://relay.example/actor", "https://relay.example/inbox", "")

			body := relayAcceptBody(tc.kind, "https://relay.example/actor",
				"https://example.com/activities/follow-relay/rel123")
			require.NoError(t, p.Process(body))

			rel, err := relayRepo.FindByID("rel123")
			require.NoError(t, err)
			assert.Equal(t, tc.want, rel.Status)
		})
	}
}
