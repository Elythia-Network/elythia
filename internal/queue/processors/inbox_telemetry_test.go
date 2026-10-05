package processors_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/deliveryhealth"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
)

// signedInboxFixture builds a signed payload plus a verifier that accepts it.
func signedInboxFixture(t *testing.T, body []byte) (queue.InboxPayload, *stubVerifier, string) {
	t.Helper()
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	host := "remote.example"
	v := &stubVerifier{actor: &model.User{ID: "alice", Host: &host}, pubKey: pub}
	return signedInboxPayload(t, key, body), v, host
}

func runInbox(t *testing.T, p *processors.InboxProcessor, payload queue.InboxPayload) error {
	t.Helper()
	return p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	})
}

// **inbox.go の 5 つの drop 分岐がすべて分類されること。** 分類が漏れると
// 「届かない」の原因がまた見えなくなる (#2471)。
func TestInboxTelemetry_AcceptedAndUnsupported(t *testing.T) {
	t.Run("accepted", func(t *testing.T) {
		payload, v, host := signedInboxFixture(t, []byte(`{"type":"Follow"}`))
		p := processors.NewInboxProcessor(&stubFedProcessor{})
		p.SetSignatureVerifier(v)
		tel := &recordingTelemetry{}
		p.SetDeliveryTelemetry(tel)

		require.NoError(t, runInbox(t, p, payload))
		require.Len(t, tel.outcomes, 1)
		assert.Equal(t, deliveryhealth.ClassAccepted, tel.outcomes[0].Class)
		assert.Equal(t, host, tel.hosts[0])
	})

	t.Run("unsupported", func(t *testing.T) {
		payload, v, _ := signedInboxFixture(t, []byte(`{"type":"Follow"}`))
		p := processors.NewInboxProcessor(&stubFedProcessor{returnFn: func([]byte) error { return federation.ErrUnsupportedActivity }})
		p.SetSignatureVerifier(v)
		tel := &recordingTelemetry{}
		p.SetDeliveryTelemetry(tel)

		require.NoError(t, runInbox(t, p, payload))
		require.Len(t, tel.outcomes, 1)
		// 異常ではない。相手は正しく送っており、こちらが対応していないだけ。
		assert.Equal(t, deliveryhealth.ClassUnsupported, tel.outcomes[0].Class)
		assert.True(t, tel.outcomes[0].Class.SucceededInbound(), "受理側に数える")
	})
}

func TestInboxTelemetry_SignatureFailed(t *testing.T) {
	priv, _, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)
	// 別 keypair の pub を持たせて verify を失敗させる。
	_, otherPub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)

	host := "remote.example"
	p := processors.NewInboxProcessor(&stubFedProcessor{})
	p.SetSignatureVerifier(&stubVerifier{actor: &model.User{ID: "alice", Host: &host}, pubKey: otherPub})
	tel := &recordingTelemetry{}
	p.SetDeliveryTelemetry(tel)

	require.NoError(t, runInbox(t, p, signedInboxPayload(t, key, []byte(`{"type":"Follow"}`))))
	require.Len(t, tel.outcomes, 1)
	assert.Equal(t, deliveryhealth.ClassSignatureFailed, tel.outcomes[0].Class)
	assert.NotEmpty(t, tel.outcomes[0].Err, "原因が分からないと直せない")
}

// **ブロック済みホストは元々ログすら出ない。** ここが唯一の観測点になる。
func TestInboxTelemetry_BlockedHostIsRecorded(t *testing.T) {
	payload, v, host := signedInboxFixture(t, []byte(`{"type":"Follow"}`))
	p := processors.NewInboxProcessor(&stubFedProcessor{})
	p.SetSignatureVerifier(v)
	p.SetHostBlockChecker(&stubBlocker{blocked: func(h string) bool { return h == host }})
	tel := &recordingTelemetry{}
	p.SetDeliveryTelemetry(tel)

	require.NoError(t, runInbox(t, p, payload))
	require.Len(t, tel.outcomes, 1)
	assert.Equal(t, deliveryhealth.ClassBlocked, tel.outcomes[0].Class)
	assert.Equal(t, host, tel.hosts[0], "ブロックしたホスト名で記録する")
	// 意図した拒否だが、成功率に混ぜると「ブロックしたのに健全」に見える。
	assert.False(t, tel.outcomes[0].Class.SucceededInbound())
}

func TestInboxTelemetry_ProcessingError(t *testing.T) {
	payload, v, _ := signedInboxFixture(t, []byte(`{"type":"Follow"}`))
	p := processors.NewInboxProcessor(&stubFedProcessor{returnFn: func([]byte) error { return errors.New("db down") }})
	p.SetSignatureVerifier(v)
	tel := &recordingTelemetry{}
	p.SetDeliveryTelemetry(tel)

	err := runInbox(t, p, payload)
	require.Error(t, err, "retry させるためにエラーを返す")
	require.Len(t, tel.outcomes, 1)
	assert.Equal(t, deliveryhealth.ClassProcessingError, tel.outcomes[0].Class)
}

// telemetry 未配線でも inbox 処理は動く。観測のために受信を落とさない。
func TestInboxTelemetry_UnwiredDoesNotBreakProcessing(t *testing.T) {
	payload, v, _ := signedInboxFixture(t, []byte(`{"type":"Follow"}`))
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(v)

	require.NoError(t, runInbox(t, p, payload))
	assert.Len(t, stub.calls, 1)
}

// 送信側と受信側で成功判定が違うこと。1 つに混ぜると方向によって意味の変わる
// 関数になり、呼び出し側が取り違える。
func TestOutcomeClass_InboundSuccessDiffersFromOutbound(t *testing.T) {
	assert.True(t, deliveryhealth.ClassAccepted.SucceededInbound())
	assert.True(t, deliveryhealth.ClassUnsupported.SucceededInbound())
	for _, c := range []deliveryhealth.OutcomeClass{
		deliveryhealth.ClassSignatureFailed, deliveryhealth.ClassBlocked,
		deliveryhealth.ClassActorUnauthorized, deliveryhealth.ClassLDSignatureFailed,
		deliveryhealth.ClassProcessingError,
	} {
		assert.Falsef(t, c.SucceededInbound(), "%s は受理として数えない", c)
	}
	// 送信側の判定は accepted を成功と見なさない (別の分類体系)。
	assert.False(t, deliveryhealth.ClassAccepted.Succeeded())
}

// **本番の inbox handler は payload.Host を設定しない。** リクエストの Host
// header は「こちらのホスト名」なので送信元を表さないため。署名の keyId から
// 導けないと、集計が全部「空ホスト」に落ちて何も見えなくなる (#2471)。
func TestInboxTelemetry_DerivesHostFromSignatureWhenPayloadHostEmpty(t *testing.T) {
	payload, v, host := signedInboxFixture(t, []byte(`{"type":"Follow"}`))
	require.Empty(t, payload.Host, "本番と同じく payload.Host は空")

	p := processors.NewInboxProcessor(&stubFedProcessor{})
	p.SetSignatureVerifier(v)
	tel := &recordingTelemetry{}
	p.SetDeliveryTelemetry(tel)

	require.NoError(t, runInbox(t, p, payload))
	require.Len(t, tel.hosts, 1)
	assert.Equal(t, host, tel.hosts[0])
}

// 署名が無い / 壊れているときは空を返す (集計しない)。誤った host に足し込む
// より、記録しない方がまし。
func TestInboxTelemetry_NoHostWithoutSignature(t *testing.T) {
	p := processors.NewInboxProcessor(&stubFedProcessor{})
	tel := &recordingTelemetry{}
	p.SetDeliveryTelemetry(tel)

	require.NoError(t, runInbox(t, p, queue.InboxPayload{Body: []byte(`{"type":"Follow"}`)}))
	assert.Empty(t, tel.outcomes, "host が特定できないものは数えない")
}
