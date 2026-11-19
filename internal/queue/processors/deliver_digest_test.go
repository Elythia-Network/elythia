package processors_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/processors"
)

// TestDeliverProcessor_UsesEnqueuedDigest pins #4221: the Digest computed when
// the job was enqueued is sent as is, and a job without one (enqueued before
// #4221) gets it computed from the body.
func TestDeliverProcessor_UsesEnqueuedDigest(t *testing.T) {
	signer := &stubSigner{resp: okResponse(200)}
	p := processors.NewDeliverProcessor(signer, nil, nil, nil, nil, nil)

	payload := makePayload(t)
	payload.Digest = "SHA-256=enqueued"
	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	assert.Equal(t, "SHA-256=enqueued", signer.gotDigest)

	signer.resp = okResponse(200)
	payload.Digest = ""
	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	assert.Equal(t, activitypub.SHA256Digest(payload.Body), signer.gotDigest)
}

// TestDeliverProcessor_JobKeyWithoutRehashingBody pins that a job with a
// Digest is keyed by it and the other fields, so the key does not hash the
// body again, while it still tells deliveries apart.
func TestDeliverProcessor_JobKeyWithoutRehashingBody(t *testing.T) {
	b := &fakeBreaker{}
	p := processors.NewDeliverProcessor(&recordingSigner{}, nil, nil, nil, nil, nil)
	p.SetDeliveryBreaker(b)
	handle := func(mutate func(*queue.DeliverPayload)) {
		t.Helper()
		payload := makePayload(t)
		payload.KeyPEM = ""
		payload.SignerUserID = "u1"
		payload.Digest = "SHA-256=d"
		mutate(&payload)
		_ = p.Handle(context.Background(), makeTask(t, payload))
	}

	handle(func(*queue.DeliverPayload) {})
	// 本文だけが違う (Digest が同じ) ものは同じ鍵になる = 本文を読んでいない。
	handle(func(p *queue.DeliverPayload) { p.Body = []byte(`{"type":"Delete"}`) })
	handle(func(p *queue.DeliverPayload) { p.Digest = "SHA-256=other" })
	handle(func(p *queue.DeliverPayload) { p.Inbox = "https://remote.example/inbox" })
	handle(func(p *queue.DeliverPayload) { p.SignerUserID = "u2" })
	handle(func(p *queue.DeliverPayload) { p.NotAfter = time.Now().Add(time.Hour).UnixMilli() })
	handle(func(p *queue.DeliverPayload) { p.IsSharedInbox = true })
	handle(func(p *queue.DeliverPayload) { p.KeyID = "k2" })
	handle(func(p *queue.DeliverPayload) { p.Ed25519KeyID = "e2" })

	require.Len(t, b.jobs, 9)
	assert.Equal(t, b.jobs[0], b.jobs[1], "the body is not part of the key")
	seen := map[string]int{}
	for i, j := range b.jobs[1:] {
		if prev, ok := seen[j]; ok {
			t.Errorf("job %d has the key of job %d", i+2, prev+1)
		}
		seen[j] = i + 1
	}

	// 項目の境目をずらしただけの組を取り違えない。
	handle(func(p *queue.DeliverPayload) { p.KeyID += "x" })
	handle(func(p *queue.DeliverPayload) { p.Ed25519KeyID = "x" })
	assert.NotEqual(t, b.jobs[9], b.jobs[10])

	// Digest の無い job は、今までどおり payload 全体から作る (本文で変わる)。
	handle(func(p *queue.DeliverPayload) { p.Digest = "" })
	handle(func(p *queue.DeliverPayload) { p.Digest = ""; p.Body = []byte(`{"type":"Delete"}`) })
	assert.NotEqual(t, b.jobs[11], b.jobs[12])
}

// TestDeliverProcessor_JobKeyCoversEveryField pins that every DeliverPayload
// field either changes the job key or is listed here as left out on purpose,
// so a field added later cannot make two different deliveries share a 429
// reservation unnoticed (#4221).
func TestDeliverProcessor_JobKeyCoversEveryField(t *testing.T) {
	// 鍵に入れない項目と、その理由。
	excluded := map[string]string{
		"Body":           "Digest が本文を表す",
		"KeyPEM":         "新しい job には詰めない (鍵は SignerUserID から引く)",
		"Ed25519PrivPEM": "新しい job には詰めない (鍵は SignerUserID から引く)",
	}
	base := makePayload(t)
	base.KeyPEM = ""
	base.SignerUserID = "u1"
	base.Digest = "SHA-256=d"
	base.NotAfter = time.Now().Add(time.Hour).UnixMilli()

	b := &fakeBreaker{}
	p := processors.NewDeliverProcessor(&recordingSigner{}, nil, nil, nil, nil, nil)
	p.SetDeliveryBreaker(b)
	keyOf := func(payload queue.DeliverPayload) string {
		t.Helper()
		before := len(b.jobs)
		_ = p.Handle(context.Background(), makeTask(t, payload))
		require.Len(t, b.jobs, before+1, "the delivery reached the breaker")
		return b.jobs[before]
	}
	baseKey := keyOf(base)

	typ := reflect.TypeOf(base)
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if _, ok := excluded[name]; ok {
			continue
		}
		changed := base
		f := reflect.ValueOf(&changed).Elem().Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString(f.String() + "x")
		case reflect.Int64:
			f.SetInt(f.Int() + 1)
		case reflect.Bool:
			f.SetBool(!f.Bool())
		default:
			t.Fatalf("decide whether %s (%s) belongs in deliveryJobKey", name, f.Kind())
		}
		assert.NotEqual(t, baseKey, keyOf(changed), "%s is not part of the job key", name)
	}
}
