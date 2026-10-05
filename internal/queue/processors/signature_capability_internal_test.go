package processors

import (
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// internalCapabilityRecorder captures both inbound signals.
type internalCapabilityRecorder struct {
	algs    map[string]string
	ldHosts []string
	edHosts []string
}

func newInternalCapabilityRecorder() *internalCapabilityRecorder {
	return &internalCapabilityRecorder{algs: map[string]string{}}
}

func (r *internalCapabilityRecorder) ObserveInboundAlg(host, alg string) { r.algs[host] = alg }
func (r *internalCapabilityRecorder) ObserveLDSignature(host string) {
	r.ldHosts = append(r.ldHosts, host)
}
func (r *internalCapabilityRecorder) ObserveEd25519Accepted(host string) {
	r.edHosts = append(r.edHosts, host)
}

// hasLDSignature の判定条件は LDSignatureVerifier 側の presence 判定
// (`!hasSig || sigRaw == nil`) と一致していなければならない。ここがずれると
// 「verifier は LD-Sig 有りと見なすのにラベルが出ない」(または逆) になる。
func TestHasLDSignature(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"signature オブジェクトあり", `{"type":"Create","signature":{"type":"RsaSignature2017"}}`, true},
		{"signature なし", `{"type":"Create"}`, false},
		{"signature が null", `{"type":"Create","signature":null}`, false},
		{"signature が空オブジェクト", `{"type":"Create","signature":{}}`, true},
		{"不正 JSON", `not json`, false},
		{"空 body", ``, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, hasLDSignature([]byte(tt.body)))
		})
	}
}

func TestInboxProcessor_RecordSignatureCapability(t *testing.T) {
	host := "remote.example"
	body := []byte(`{"type":"Create","signature":{"type":"RsaSignature2017"}}`)

	recorder := newInternalCapabilityRecorder()
	p := &InboxProcessor{capabilities: recorder}
	p.recordSignatureCapability(&model.User{Host: &host}, activitypub.KeyTypeEd25519, body)

	assert.Equal(t, model.SignatureAlgEd25519, recorder.algs[host])
	assert.Equal(t, []string{host}, recorder.ldHosts)
}

// LD-Signature が無い activity では ldSignature を記録しない。窓内で観測して
// いない系統を書くと repository の部分 upsert が既存記録を潰す。
func TestInboxProcessor_RecordSignatureCapabilityWithoutLDSignature(t *testing.T) {
	host := "remote.example"
	recorder := newInternalCapabilityRecorder()
	p := &InboxProcessor{capabilities: recorder}
	p.recordSignatureCapability(&model.User{Host: &host}, activitypub.KeyTypeRSA,
		[]byte(`{"type":"Create"}`))

	assert.Equal(t, model.SignatureAlgRSA, recorder.algs[host])
	assert.Empty(t, recorder.ldHosts)
}

func TestInboxProcessor_RecordSignatureCapabilitySkipsLocalOrUnwired(t *testing.T) {
	recorder := newInternalCapabilityRecorder()
	p := &InboxProcessor{capabilities: recorder}
	p.recordSignatureCapability(nil, activitypub.KeyTypeRSA, nil)
	p.recordSignatureCapability(&model.User{}, activitypub.KeyTypeRSA, nil)
	assert.Empty(t, recorder.algs)

	unwired := &InboxProcessor{}
	host := "remote.example"
	assert.NotPanics(t, func() {
		unwired.recordSignatureCapability(&model.User{Host: &host}, activitypub.KeyTypeRSA, nil)
	})
}

func TestDeliverProcessor_RecordEd25519Accepted(t *testing.T) {
	recorder := newInternalCapabilityRecorder()
	p := &DeliverProcessor{capabilities: recorder}
	p.recordEd25519Accepted("remote.example")
	assert.Equal(t, []string{"remote.example"}, recorder.edHosts)

	// host 不明 / 未配線は no-op。
	p.recordEd25519Accepted("")
	assert.Len(t, recorder.edHosts, 1)
	assert.NotPanics(t, func() { (&DeliverProcessor{}).recordEd25519Accepted("remote.example") })
}

func TestDeliverProcessor_SetSignatureCapabilityRecorder(t *testing.T) {
	recorder := newInternalCapabilityRecorder()
	p := NewDeliverProcessor(nil)
	p.SetSignatureCapabilityRecorder(recorder)
	p.recordEd25519Accepted("remote.example")
	require.Equal(t, []string{"remote.example"}, recorder.edHosts)
}

func TestInboxProcessor_SetSignatureCapabilityRecorder(t *testing.T) {
	recorder := newInternalCapabilityRecorder()
	p := NewInboxProcessor(nil)
	p.SetSignatureCapabilityRecorder(recorder)
	host := "remote.example"
	p.recordSignatureCapability(&model.User{Host: &host}, activitypub.KeyTypeRSA, nil)
	require.Equal(t, model.SignatureAlgRSA, recorder.algs[host])
}
