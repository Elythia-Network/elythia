package processors_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dispatch 時の block / suspend 判定は inbox URL の host で行う。既定ポートを
// 剥がさないと `https://bad.example:443/inbox` が `bad.example:443` になり、
// `blockedHosts` (= 完全一致 / suffix 一致) をすり抜ける。enqueue 時のゲートを
// 通り抜けたジョブ (block 前に積まれたもの / retry backoff 中のもの) を
// 止める最後の砦なので、ここが空振りすると defederation が効かない。
func TestDeliverProcessor_DeliveryGate_NormalizesInboxHost(t *testing.T) {
	cases := []struct {
		name     string
		inbox    string
		wantHost string
	}{
		{"https default port stripped", "https://bad.example:443/inbox", "bad.example"},
		{"http default port stripped", "http://bad.example:80/inbox", "bad.example"},
		{"mixed case lowered", "https://Bad.Example/inbox", "bad.example"},
		{"no port unchanged", "https://bad.example/inbox", "bad.example"},
		// 非既定ポートは別 host のまま (upstream と同じ)。
		{"non-default port kept", "https://bad.example:8443/inbox", "bad.example:8443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			signer := &stubSigner{resp: okResponse(http.StatusOK)}
			p := processors.NewDeliverProcessor(signer)
			gate := &stubDeliveryGate{skip: map[string]bool{"bad.example": true}}
			p.SetDeliveryGate(gate)

			payload := makePayload(t)
			payload.Inbox = tc.inbox
			require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))

			require.Equal(t, []string{tc.wantHost}, gate.seen)
			if tc.wantHost == "bad.example" {
				assert.Empty(t, signer.gotURL, "blocked host へは POST されない")
			} else {
				assert.Equal(t, tc.inbox, signer.gotURL, "非既定ポートは別 host なので配送される")
			}
		})
	}
}

// host を取り出せない inbox では gate を呼ばず、従来どおり配送を試す
// (POST 自体が失敗するので、ここで fail-closed にする必要は無い)。
func TestDeliverProcessor_DeliveryGate_UnparseableInboxHost(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusOK)}
	p := processors.NewDeliverProcessor(signer)
	gate := &stubDeliveryGate{skip: map[string]bool{"bad.example": true}}
	p.SetDeliveryGate(gate)

	payload := makePayload(t)
	payload.Inbox = "://bad-url"
	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	assert.Empty(t, gate.seen, "host 不明なら gate は呼ばない")
}
