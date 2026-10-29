package relay_test

import (
	"context"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/core/relay"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type relayDeliveryQueue struct {
	calls []queue.DeliverPayload
	err   error
}

func (q *relayDeliveryQueue) EnqueueDeliver(payload queue.DeliverPayload, _ ...driver.EnqueueOption) error {
	if q.err != nil {
		return q.err
	}
	q.calls = append(q.calls, payload)
	return nil
}
func (*relayDeliveryQueue) EnqueueExport(queue.ExportPayload) error                    { return nil }
func (*relayDeliveryQueue) EnqueueImport(queue.ImportPayload) error                    { return nil }
func (*relayDeliveryQueue) EnqueueWebPush(context.Context, queue.WebPushPayload) error { return nil }
func (*relayDeliveryQueue) EnqueueUserWebhook(context.Context, queue.WebhookPayload) error {
	return nil
}
func (*relayDeliveryQueue) EnqueueSystemWebhook(context.Context, queue.WebhookPayload) error {
	return nil
}
func (*relayDeliveryQueue) EnqueueInbox(context.Context, queue.InboxPayload) error { return nil }
func (*relayDeliveryQueue) EnqueuePostScheduledNote(queue.PostScheduledNotePayload, ...driver.EnqueueOption) error {
	return nil
}
func (*relayDeliveryQueue) ClearScheduledNote(string) error { return nil }
func (*relayDeliveryQueue) Close() error                    { return nil }

type relayFederationObserver struct {
	recorded []string
}

func (o *relayFederationObserver) IsExternallyAvailable(user *model.User) bool {
	return user != nil && user.FederationStatus == model.FederationStatusOn
}
func (o *relayFederationObserver) ObserveExternalReference(userID, reason, host string) (bool, error) {
	o.recorded = append(o.recorded, userID+":"+reason+":"+host)
	return true, nil
}

func newRelayDeliveryPath(t *testing.T, status model.FederationStatus, queueErr error) (*relay.Service, *relayDeliveryQueue, *relayFederationObserver) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice", FederationStatus: status}
	followingRepo := testutil.NewMockFollowingRepository()
	keypairRepo := testutil.NewMockUserKeypairRepository()
	keypairRepo.Keypairs["alice"] = &model.UserKeypair{UserID: "alice", PublicKey: "PUB", PrivateKey: "PEM"}
	urls := activitypub.NewURLBuilder("https://example.com")
	deliveryQueue := &relayDeliveryQueue{err: queueErr}
	delivery := corefederation.NewDeliverService(deliveryQueue, userRepo, followingRepo, keypairRepo, urls, nil)
	observer := &relayFederationObserver{}
	delivery.SetLocalFederationObserver(observer)

	relayRepo := testutil.NewMockRelayRepository()
	require.NoError(t, relayRepo.Create(&model.Relay{ID: "relay1", Inbox: "https://relay.example/inbox", Status: relay.StatusAccepted}))
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	relayService := relay.NewService(relayRepo, nil, activitypub.NewRenderer(urls), delivery, idGen)
	return relayService, deliveryQueue, observer
}

func TestDeliverToAccepted_UsesLocalFederationPolicyAndReferenceOrdering(t *testing.T) {
	t.Run("off signer is rejected before observation or queueing", func(t *testing.T) {
		service, deliveryQueue, observer := newRelayDeliveryPath(t, model.FederationStatusOff, nil)
		err := service.DeliverToAccepted(context.Background(), "alice", map[string]any{"type": "Create"})
		require.ErrorIs(t, err, corefederation.ErrFederationDisabled)
		assert.Empty(t, observer.recorded)
		assert.Empty(t, deliveryQueue.calls)
	})

	t.Run("successful attempt observes the signer", func(t *testing.T) {
		service, deliveryQueue, observer := newRelayDeliveryPath(t, model.FederationStatusOn, nil)
		require.NoError(t, service.DeliverToAccepted(context.Background(), "alice", map[string]any{"type": "Create"}))
		assert.Equal(t, []string{"alice:delivery:relay.example"}, observer.recorded)
		require.Len(t, deliveryQueue.calls, 1)
		assert.Equal(t, "https://relay.example/inbox", deliveryQueue.calls[0].Inbox)
	})

	t.Run("queue failure does not undo first-reference observation", func(t *testing.T) {
		service, deliveryQueue, observer := newRelayDeliveryPath(t, model.FederationStatusOn, errors.New("queue unavailable"))
		err := service.DeliverToAccepted(context.Background(), "alice", map[string]any{"type": "Create"})
		require.ErrorContains(t, err, "enqueue deliver to https://relay.example/inbox: queue unavailable")
		assert.Equal(t, []string{"alice:delivery:relay.example"}, observer.recorded)
		assert.Empty(t, deliveryQueue.calls)
	})
}
