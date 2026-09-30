package processors_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/deliveryhealth"
	"github.com/shiroha-a/mk/internal/queue/driver"
	"github.com/shiroha-a/mk/internal/queue/processors"
)

// 期限付きの配送 (#3238、引用の承認の QuoteRequest) は、期限を過ぎていれば送らない。
func TestDeliverProcessor_Deadline_ExpiredIsDropped(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusOK)}
	p := processors.NewDeliverProcessor(signer)
	payload := makePayload(t)
	payload.NotAfter = time.Now().Add(-time.Second).UnixMilli()
	err := p.Handle(context.Background(), makeTask(t, payload))
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
	assert.Empty(t, signer.gotURL, "nothing is sent past the deadline")

	// 期限内なら送る。
	payload.NotAfter = time.Now().Add(time.Minute).UnixMilli()
	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	assert.NotEmpty(t, signer.gotURL)
}

// 期限付きの配送は、ブレーカーや流量の制限で後へ回さずに捨てる (後へ回すと試行を
// 消費しないまま、状態が変わった後に届きうる)。期限の無い配送は今までどおり後へ回す。
func TestDeliverProcessor_Deadline_HeldIsDropped(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusOK)}
	p := processors.NewDeliverProcessor(signer)
	p.SetDeliveryBreaker(&fakeBreaker{decision: deliveryhealth.BreakerDelay, wait: time.Minute, hasState: true})
	payload := makePayload(t)
	payload.NotAfter = time.Now().Add(time.Minute).UnixMilli()
	err := p.Handle(context.Background(), makeTask(t, payload))
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
	var delayed *driver.DelayError
	assert.False(t, errors.As(err, &delayed), "a job with a deadline is not held")
	assert.Empty(t, signer.gotURL)

	for name, s := range map[string]*stubSigner{
		"transport": {err: errors.New("connection refused")},
		"5xx":       {resp: okResponse(http.StatusBadGateway)},
	} {
		t.Run(name, func(t *testing.T) {
			p := processors.NewDeliverProcessor(s)
			p.SetDeliveryBreaker(&fakeBreaker{openAfterFailure: true})
			err := p.Handle(context.Background(), makeTask(t, payload))
			assert.ErrorIs(t, err, driver.ErrSkipRetry)
			var delayed *driver.DelayError
			assert.False(t, errors.As(err, &delayed))

			// 期限が無ければ今までどおり後へ回す。
			plain := payload
			plain.NotAfter = 0
			err = p.Handle(context.Background(), makeTask(t, plain))
			require.ErrorAs(t, err, &delayed)
			assert.NotErrorIs(t, err, driver.ErrSkipRetry)
		})
	}
}

// Ed25519 で 4xx が返り、RSA での送り直しが失敗してブレーカーが開いたときも、
// 期限付きの配送は後へ回さずに捨てる。
func TestDeliverProcessor_Deadline_RSARetryHeldIsDropped(t *testing.T) {
	mr := miniredis.RunT(t)
	signer := &sequenceSigner{steps: []signerStep{
		{resp: okResponse(http.StatusBadRequest)},
		{err: errors.New("connection refused")},
	}}
	p := processors.NewDeliverProcessor(signer)
	p.SetRedis(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	p.SetDeliveryBreaker(&fakeBreaker{openAfterFailure: true})
	payload := makePayload(t)
	payload.Ed25519KeyID = "https://example.com/users/u1#ed25519-key"
	payload.Ed25519PrivPEM = generateTestEd25519Key(t)
	payload.NotAfter = time.Now().Add(time.Minute).UnixMilli()

	err := p.Handle(context.Background(), makeTask(t, payload))
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
	var delayed *driver.DelayError
	assert.False(t, errors.As(err, &delayed))
}
