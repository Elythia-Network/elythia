package processors_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingKeySource is keySourceStub that counts reads and runs during inside
// the first one.
type countingKeySource struct {
	keySourceStub
	reads  atomic.Int32
	during func()
}

func (s *countingKeySource) SigningKeyPEM(userID, kind string) (string, error) {
	n := s.reads.Add(1)
	pem, err := s.keySourceStub.SigningKeyPEM(userID, kind)
	if n == 1 && s.during != nil {
		s.during()
	}
	return pem, err
}

func signWithSignerUser(t *testing.T, p *processors.DeliverProcessor) {
	t.Helper()
	payload := makePayload(t)
	payload.KeyPEM = ""
	payload.SignerUserID = "u1"
	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
}

func newDropProcessor(t *testing.T) (*processors.DeliverProcessor, *countingKeySource) {
	t.Helper()
	responses := make([]*http.Response, 4)
	for i := range responses {
		responses[i] = okResponse(http.StatusOK)
	}
	src := &countingKeySource{keySourceStub: keySourceStub{rsa: generateTestKey(t)}}
	p := processors.NewDeliverProcessor(&recordingSigner{responses: responses}, nil, src, nil, nil, nil)
	return p, src
}

// TestDeliverProcessor_DropLocal pins that DropLocal drops the signing keys
// read for a signer user, so the next job reads the key again (#3732).
func TestDeliverProcessor_DropLocal(t *testing.T) {
	p, src := newDropProcessor(t)
	signWithSignerUser(t, p)
	signWithSignerUser(t, p)
	require.Equal(t, int32(1), src.reads.Load(), "前提: 署名者の鍵は cache から返る")

	p.DropLocal()
	signWithSignerUser(t, p)
	assert.Equal(t, int32(2), src.reads.Load(), "DropLocal did not drop the signing keys")
}

// TestDeliverProcessor_DropLocalDuringReadIsNotCached pins that a key read
// while DropLocal ran signs that one job but is not stored (#3732).
func TestDeliverProcessor_DropLocalDuringReadIsNotCached(t *testing.T) {
	p, src := newDropProcessor(t)
	src.during = p.DropLocal

	signWithSignerUser(t, p)
	signWithSignerUser(t, p)
	assert.Equal(t, int32(2), src.reads.Load(), "the key read during DropLocal was cached")
	signWithSignerUser(t, p)
	assert.Equal(t, int32(2), src.reads.Load(), "a later read is cached again")
}

// TestDeliverProcessor_DropLocalWithoutCache pins that DropLocal is safe on a
// processor without a key cache.
func TestDeliverProcessor_DropLocalWithoutCache(t *testing.T) {
	var p processors.DeliverProcessor
	assert.NotPanics(t, p.DropLocal)
}
