package processors_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/deliveryhealth"
	"github.com/elythia-network/elythia/internal/queue/processors"
)

// recordingTelemetry captures every RecordDelivery call.
type recordingTelemetry struct {
	hosts    []string
	outcomes []deliveryhealth.Outcome
}

func (r *recordingTelemetry) RecordDelivery(host string, o deliveryhealth.Outcome) {
	r.hosts = append(r.hosts, host)
	r.outcomes = append(r.outcomes, o)
}

// **分類は deliver.go の応答 switch と一致していなければならない。** telemetry
// 側で再分類すると「成功とみなす範囲」が二重管理になるので、ここで対応表を
// 固定する (#2461)。
func TestDeliverTelemetry_ClassMatchesResponseSwitch(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		wantClass deliveryhealth.OutcomeClass
	}{
		{"200 は success", http.StatusOK, deliveryhealth.ClassSuccess},
		{"202 は success", http.StatusAccepted, deliveryhealth.ClassSuccess},
		{"410 は gone", http.StatusGone, deliveryhealth.ClassGone},
		{"404 は gone", http.StatusNotFound, deliveryhealth.ClassGone},
		{"429 は rateLimited", http.StatusTooManyRequests, deliveryhealth.ClassRateLimited},
		{"401 は clientError", http.StatusUnauthorized, deliveryhealth.ClassClientError},
		{"400 は clientError", http.StatusBadRequest, deliveryhealth.ClassClientError},
		{"500 は serverError", http.StatusInternalServerError, deliveryhealth.ClassServerError},
		{"503 は serverError", http.StatusServiceUnavailable, deliveryhealth.ClassServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			signer := &stubSigner{resp: okResponse(tc.status)}
			p := processors.NewDeliverProcessor(signer)
			tel := &recordingTelemetry{}
			p.SetDeliveryTelemetry(tel)

			_ = p.Handle(context.Background(), makeTask(t, makePayload(t)))

			require.Len(t, tel.outcomes, 1)
			assert.Equal(t, tc.wantClass, tel.outcomes[0].Class)
			assert.Equal(t, tc.status, tel.outcomes[0].Status)
			assert.Equal(t, "remote.example", tel.hosts[0])
		})
	}
}

// HTTP 応答に至らなかった失敗は transport。status は 0 になる。
func TestDeliverTelemetry_TransportError(t *testing.T) {
	signer := &stubSigner{err: errors.New("dial tcp: i/o timeout")}
	p := processors.NewDeliverProcessor(signer)
	tel := &recordingTelemetry{}
	p.SetDeliveryTelemetry(tel)

	err := p.Handle(context.Background(), makeTask(t, makePayload(t)))
	require.Error(t, err)

	require.Len(t, tel.outcomes, 1)
	assert.Equal(t, deliveryhealth.ClassTransport, tel.outcomes[0].Class)
	assert.Zero(t, tel.outcomes[0].Status, "応答が無いので status は 0")
	assert.Contains(t, tel.outcomes[0].Err, "i/o timeout")
}

// レイテンシは必ず記録される (0 でも「計測した」ことが分かる必要はないが、
// 負値やゼロ値の未設定を避ける)。
func TestDeliverTelemetry_RecordsLatency(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusAccepted)}
	p := processors.NewDeliverProcessor(signer)
	tel := &recordingTelemetry{}
	p.SetDeliveryTelemetry(tel)

	require.NoError(t, p.Handle(context.Background(), makeTask(t, makePayload(t))))
	require.Len(t, tel.outcomes, 1)
	assert.GreaterOrEqual(t, tel.outcomes[0].Latency.Nanoseconds(), int64(0))
}

// telemetry 未配線でも配送は動く。観測のために配送を落とさない。
func TestDeliverTelemetry_UnwiredDoesNotBreakDelivery(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusAccepted)}
	p := processors.NewDeliverProcessor(signer)

	require.NoError(t, p.Handle(context.Background(), makeTask(t, makePayload(t))))
}
