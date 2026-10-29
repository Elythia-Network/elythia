package federation_test

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeliverActivity_EnqueueFailureStillRecordsFirstReference(t *testing.T) {
	svc, enq, userRepo, _, keypairRepo := newDeliverService(t)
	installLocalSigner(t, userRepo, keypairRepo)
	userRepo.Users["alice"].FederationStatus = model.FederationStatusOn
	observer := &localFederationObserverStub{}
	svc.SetLocalFederationObserver(observer)
	enq.err = errors.New("queue unavailable")

	err := svc.DeliverActivity("alice", []byte(`{"type":"Follow"}`), []string{"https://relay.example/inbox"})
	require.ErrorContains(t, err, "enqueue deliver to https://relay.example/inbox: queue unavailable")
	assert.Equal(t, []string{"alice:delivery:relay.example"}, observer.recorded,
		"the first attempted external delivery remains a permanent external reference even if queueing fails")
	assert.Empty(t, enq.calls, "the failing queue does not accept the delivery job")
}
