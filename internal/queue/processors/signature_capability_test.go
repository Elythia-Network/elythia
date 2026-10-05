package processors_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// capabilityRecorderStub captures every capability signal the processors emit.
type capabilityRecorderStub struct {
	algs    map[string]string
	ldHosts []string
	edHosts []string
}

func newCapabilityRecorderStub() *capabilityRecorderStub {
	return &capabilityRecorderStub{algs: map[string]string{}}
}

func (r *capabilityRecorderStub) ObserveInboundAlg(host, alg string) { r.algs[host] = alg }
func (r *capabilityRecorderStub) ObserveLDSignature(host string) {
	r.ldHosts = append(r.ldHosts, host)
}
func (r *capabilityRecorderStub) ObserveEd25519Accepted(host string) {
	r.edHosts = append(r.edHosts, host)
}

// worker verify 経路でも署名方式が記録される。handler 側だけに入れると、
// production の async 構成では丸ごと記録されない。
func TestInboxProcessor_RecordsSignatureCapabilityAfterVerify(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	host := "remote.example"
	aliceURI := "https://remote.example/users/alice"
	verifier := &stubVerifier{
		actor:  &model.User{ID: "alice", Host: &host, URI: &aliceURI},
		pubKey: pub,
	}
	recorder := newCapabilityRecorderStub()

	p := processors.NewInboxProcessor(&stubFedProcessor{})
	p.SetSignatureVerifier(verifier)
	p.SetSignatureCapabilityRecorder(recorder)

	body := []byte(`{"id":"https://remote.example/follows/1","type":"Follow","actor":"https://remote.example/users/alice"}`)
	payload := signedInboxPayload(t, key, body)

	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	}))
	assert.Equal(t, model.SignatureAlgRSA, recorder.algs[host])
	assert.Empty(t, recorder.ldHosts, "LD-Signature の無い activity では記録しない")
}

// 署名検証に失敗した payload では何も記録しない。
func TestInboxProcessor_DoesNotRecordCapabilityOnVerifyFailure(t *testing.T) {
	priv, _, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	host := "remote.example"
	aliceURI := "https://remote.example/users/alice"
	// verifier は別鍵を返すので verify に失敗する。
	_, otherPub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	verifier := &stubVerifier{
		actor:  &model.User{ID: "alice", Host: &host, URI: &aliceURI},
		pubKey: otherPub,
	}
	recorder := newCapabilityRecorderStub()

	p := processors.NewInboxProcessor(&stubFedProcessor{})
	p.SetSignatureVerifier(verifier)
	p.SetSignatureCapabilityRecorder(recorder)

	body := []byte(`{"id":"https://remote.example/follows/1","type":"Follow","actor":"https://remote.example/users/alice"}`)
	payload := signedInboxPayload(t, key, body)

	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	}))
	assert.Empty(t, recorder.algs)
}

// block した host は記録しない。連合しない相手の行を作っても表示先が無いうえ、
// 記録位置が block gate より前に動くと「拒否した相手の観測が残る」ことになる。
// この不変条件を固定するためのテスト。
func TestInboxProcessor_DoesNotRecordCapabilityForBlockedHost(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	host := "remote.example"
	aliceURI := "https://remote.example/users/alice"
	verifier := &stubVerifier{
		actor:  &model.User{ID: "alice", Host: &host, URI: &aliceURI},
		pubKey: pub,
	}
	recorder := newCapabilityRecorderStub()

	p := processors.NewInboxProcessor(&stubFedProcessor{})
	p.SetSignatureVerifier(verifier)
	p.SetHostBlockChecker(&stubBlocker{blocked: func(h string) bool { return h == host }})
	p.SetSignatureCapabilityRecorder(recorder)

	body := []byte(`{"id":"https://remote.example/follows/1","type":"Follow","actor":"https://remote.example/users/alice"}`)
	payload := signedInboxPayload(t, key, body)

	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	}))
	assert.Empty(t, recorder.algs, "block した host の観測は残さない")
}

// LD-Signature 付き activity では inboundAlg と併せて ldSignature も記録する。
func TestInboxProcessor_RecordsLDSignature(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	host := "remote.example"
	aliceURI := "https://remote.example/users/alice"
	verifier := &stubVerifier{
		actor:  &model.User{ID: "alice", Host: &host, URI: &aliceURI},
		pubKey: pub,
	}
	recorder := newCapabilityRecorderStub()

	p := processors.NewInboxProcessor(&stubFedProcessor{})
	p.SetSignatureVerifier(verifier)
	p.SetSignatureCapabilityRecorder(recorder)

	body := []byte(`{"id":"https://remote.example/follows/1","type":"Follow",` +
		`"actor":"https://remote.example/users/alice",` +
		`"signature":{"type":"RsaSignature2017","creator":"https://remote.example/users/alice#main-key"}}`)
	payload := signedInboxPayload(t, key, body)

	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	}))
	assert.Equal(t, model.SignatureAlgRSA, recorder.algs[host])
	assert.Equal(t, []string{host}, recorder.ldHosts)
}

// Ed25519 で署名した配送が 2xx を返したら記録する (= 同期的に拒否されなかった)。
func TestDeliverProcessor_RecordsEd25519Accepted(t *testing.T) {
	signer := &recordingSigner{responses: []*http.Response{okResponse(http.StatusOK)}}
	p := processors.NewDeliverProcessor(signer)
	recorder := newCapabilityRecorderStub()
	p.SetSignatureCapabilityRecorder(recorder)

	payload := makePayload(t)
	payload.Ed25519KeyID = "https://example.com/users/u1#ed25519-key"
	payload.Ed25519PrivPEM = generateTestEd25519Key(t)

	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	assert.Equal(t, []string{hostOfPayload(t, payload)}, recorder.edHosts)
}

// RSA で送った 2xx は Ed25519 の成功として数えない。
func TestDeliverProcessor_DoesNotRecordEd25519OnRSADelivery(t *testing.T) {
	signer := &recordingSigner{responses: []*http.Response{okResponse(http.StatusOK)}}
	p := processors.NewDeliverProcessor(signer)
	recorder := newCapabilityRecorderStub()
	p.SetSignatureCapabilityRecorder(recorder)

	require.NoError(t, p.Handle(context.Background(), makeTask(t, makePayload(t))))
	assert.Empty(t, recorder.edHosts)
}

// Ed25519 鍵が壊れていて RSA に落ちた場合も記録しない。sendOnce の引数
// (useEd25519) ではなく実際に署名した方式を見ていないと、ここが誤記録になる。
func TestDeliverProcessor_DoesNotRecordEd25519WhenKeyParseFails(t *testing.T) {
	signer := &recordingSigner{responses: []*http.Response{okResponse(http.StatusOK)}}
	p := processors.NewDeliverProcessor(signer)
	recorder := newCapabilityRecorderStub()
	p.SetSignatureCapabilityRecorder(recorder)

	payload := makePayload(t)
	payload.Ed25519KeyID = "https://example.com/users/u1#ed25519-key"
	payload.Ed25519PrivPEM = "NOT A VALID PEM"

	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	assert.Empty(t, recorder.edHosts, "RSA fallback を Ed25519 成功として数えない")
}

// Ed25519 が 4xx で弾かれ RSA 再送で 2xx になった場合も記録しない。相手は
// Ed25519 を検証できていない。
func TestDeliverProcessor_DoesNotRecordEd25519AfterRSARetry(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	signer := &recordingSigner{responses: []*http.Response{
		okResponse(http.StatusBadRequest), // Ed25519 で 4xx
		okResponse(http.StatusOK),         // RSA 再送で 2xx
	}}
	p := processors.NewDeliverProcessor(signer)
	p.SetRedis(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	recorder := newCapabilityRecorderStub()
	p.SetSignatureCapabilityRecorder(recorder)

	payload := makePayload(t)
	payload.Ed25519KeyID = "https://example.com/users/u1#ed25519-key"
	payload.Ed25519PrivPEM = generateTestEd25519Key(t)

	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	require.Len(t, signer.calls, 2, "Ed25519 → RSA の 2 回送信")
	assert.Empty(t, recorder.edHosts, "RSA 再送の 2xx を Ed25519 成功にしない")
}

// hostOfPayload derives the host the processor uses for capability recording.
func hostOfPayload(t *testing.T, payload queue.DeliverPayload) string {
	t.Helper()
	// makePayload の Inbox は https://<host>/... 形式。
	const prefix = "https://"
	require.True(t, len(payload.Inbox) > len(prefix))
	rest := payload.Inbox[len(prefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			return rest[:i]
		}
	}
	return rest
}
