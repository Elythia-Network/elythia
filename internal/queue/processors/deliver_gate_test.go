package processors_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubDeliveryGate skips delivery for hosts present in skip.
type stubDeliveryGate struct {
	skip map[string]bool
	seen []string
}

func (g *stubDeliveryGate) ShouldSkipDelivery(host string) bool {
	g.seen = append(g.seen, host)
	return g.skip[host]
}

// suspend / block されたインスタンス宛のジョブは dispatch 時にスキップされ、
// signer は呼ばれず success 扱いで完了する (#1404)。
func TestDeliverProcessor_DeliveryGate_SkipsBlockedHost(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusOK)}
	p := processors.NewDeliverProcessor(signer)
	gate := &stubDeliveryGate{skip: map[string]bool{"remote.example": true}}
	p.SetDeliveryGate(gate)

	err := p.Handle(context.Background(), makeTask(t, makePayload(t)))
	require.NoError(t, err)
	assert.Equal(t, []string{"remote.example"}, gate.seen)
	assert.Empty(t, signer.gotURL, "blocked/suspended host へは POST されない")
}

// gate が false を返す host へは従来どおり配送される。
func TestDeliverProcessor_DeliveryGate_AllowsActiveHost(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusOK)}
	p := processors.NewDeliverProcessor(signer)
	gate := &stubDeliveryGate{skip: map[string]bool{}}
	p.SetDeliveryGate(gate)

	payload := makePayload(t)
	err := p.Handle(context.Background(), makeTask(t, payload))
	require.NoError(t, err)
	assert.Equal(t, payload.Inbox, signer.gotURL)
}
