package processors_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
)

const replayDigestTestBody = `{"id":"https://remote.example/follows/3988","type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`

// replayDigestFixture returns a verifier for the remote actor and the key it
// signs with.
func replayDigestFixture(t *testing.T) (*stubVerifier, *activitypub.PrivateKey) {
	t.Helper()
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)
	host := "remote.example"
	aliceURI := "https://remote.example/users/alice"
	return &stubVerifier{actor: &model.User{ID: "alice", Host: &host, URI: &aliceURI}, pubKey: pub}, key
}

func replayDigestTask(t *testing.T, payload queue.InboxPayload) driver.RawTask {
	t.Helper()
	return driver.RawTask{TypeName: queue.TaskTypeInbox, Body: mustEncode(t, payload)}
}

// #3988: 再投函の鍵は、署名が覆った Date と Digest から作る。起動の直後から
// その鍵だけを覚え、同じリクエストの再投函を捨てる (#4275 で前の形の鍵をやめた)。
func TestInboxProcessor_ReplayKeyIsSignedDateAndDigest(t *testing.T) {
	verifier, key := replayDigestFixture(t)
	payload := signedInboxPayloadWithDate(t, key, []byte(replayDigestTestBody), "", true)
	stub := &stubFedProcessor{}
	guard := newMemReplayGuard()
	p := processors.NewInboxProcessor(stub, verifier, nil, nil, nil, guard)

	require.NoError(t, p.Handle(context.Background(), replayDigestTask(t, payload)))
	require.Len(t, stub.calls, 1)
	assert.Equal(t, map[string]bool{digestReplayKeyOf(payload.Headers["Date"], payload.Headers["Digest"]): true}, guard.seen)

	require.NoError(t, p.Handle(context.Background(), replayDigestTask(t, payload)))
	assert.Len(t, stub.calls, 1, "the replay must be dropped")

	// 同じ Date でも本文 (= Digest) が違えば別のリクエスト。
	other := signedInboxPayloadWithDate(t, key, []byte(`{"id":"https://remote.example/follows/3988b","type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`), payload.Headers["Date"], true)
	require.NotEqual(t, payload.Headers["Digest"], other.Headers["Digest"])
	require.NoError(t, p.Handle(context.Background(), replayDigestTask(t, other)))
	assert.Len(t, stub.calls, 2, "a different body under the same Date is a different request")
}

// #4275: 起動の直後も、#3988 より前の形の鍵 (Date と本文の SHA-256) は計算せず、
// 見ず、覚えない。2.2.0 と 2.3.0 は起動から inboxReplayTTL の間だけ前の形の鍵も
// 使っていたが、前の形の鍵だけで覚えたリリースは無く、大きな本文の受信を重くするだけだった。
func TestInboxProcessor_PreviousReplayKeyIsNotUsedRightAfterStart(t *testing.T) {
	t.Run("a request remembered by the previous key is not a replay", func(t *testing.T) {
		verifier, payload := signedReplayFixture(t, []byte(replayDigestTestBody))
		previous := replayKeyOf(payload.Headers["Date"], payload.Body)
		stub := &stubFedProcessor{}
		guard := newMemReplayGuard()
		guard.seen[previous] = true
		p := processors.NewInboxProcessor(stub, verifier, nil, nil, nil, guard)

		require.NoError(t, p.Handle(context.Background(), replayDigestTask(t, payload)))
		assert.Len(t, stub.calls, 1, "the previous key must not be consulted")
		assert.Empty(t, guard.claims)
	})
	t.Run("only the current key is remembered", func(t *testing.T) {
		verifier, payload := signedReplayFixture(t, []byte(replayDigestTestBody))
		stub := &stubFedProcessor{}
		guard := newMemReplayGuard()
		p := processors.NewInboxProcessor(stub, verifier, nil, nil, nil, guard)

		require.NoError(t, p.Handle(context.Background(), replayDigestTask(t, payload)))
		require.Len(t, stub.calls, 1)
		assert.Equal(t, map[string]bool{
			digestReplayKeyOf(payload.Headers["Date"], payload.Headers["Digest"]): true,
		}, guard.seen, "the previous key must not be remembered")
		assert.Empty(t, guard.claims)
	})
}

// #3988: Digest を署名が覆っていない・Digest が無い payload (受付を通らずに積まれた
// もの) では、今までどおり本文から鍵を作る。Digest の値を本文の代わりにすると、
// 署名に縛られない値で別の本文を同じリクエストに見せられる。
func TestInboxProcessor_ReplayKeyFallsBackToBodyWithoutSignedDigest(t *testing.T) {
	for _, tc := range []struct {
		name       string
		signDigest bool
		dropDigest bool
	}{
		{name: "digest not signed"},
		{name: "digest missing", dropDigest: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verifier, key := replayDigestFixture(t)
			payload := signedInboxPayloadWithDate(t, key, []byte(replayDigestTestBody), "", tc.signDigest)
			if tc.dropDigest {
				delete(payload.Headers, "Digest")
			} else {
				require.NotEmpty(t, payload.Headers["Digest"])
			}
			stub := &stubFedProcessor{}
			guard := newMemReplayGuard()
			p := processors.NewInboxProcessor(stub, verifier, nil, nil, nil, guard)
			require.NoError(t, p.Handle(context.Background(), replayDigestTask(t, payload)))
			require.Len(t, stub.calls, 1)
			assert.Equal(t, map[string]bool{replayKeyOf(payload.Headers["Date"], payload.Body): true}, guard.seen)

			require.NoError(t, p.Handle(context.Background(), replayDigestTask(t, payload)))
			assert.Len(t, stub.calls, 1, "the replay must be dropped")
		})
	}
}

// signedInboxPayloadWithDate signs body with key and the
// given Date ("" for now); signDigest decides whether the signature covers the
// Digest header (it is sent either way).
func signedInboxPayloadWithDate(t *testing.T, key *activitypub.PrivateKey, body []byte, date string, signDigest bool) queue.InboxPayload {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://example.com/inbox", nil)
	req.Host = "example.com"
	if date != "" {
		req.Header.Set("Date", date)
	}
	signed := []string{"(request-target)", "date", "host"}
	if signDigest {
		signed = append(signed, "digest")
	}
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(body), signed))
	headers := map[string]string{}
	for _, h := range []string{"Signature", "Date", "Host", "Digest"} {
		if v := req.Header.Get(h); v != "" {
			headers[h] = v
		}
	}
	if headers["Host"] == "" {
		headers["Host"] = req.Host
	}
	return queue.InboxPayload{Body: body, Method: req.Method, Path: req.URL.RequestURI(), Headers: headers}
}

// #3988: 署名者 == actor の経路の禁止された directive の検査には、生の本文
// (正規化の前) を渡す。
func TestInboxProcessor_ForbiddenDirectiveCheckGetsRawBody(t *testing.T) {
	body := []byte(`{"@context":["https://www.w3.org/ns/activitystreams"],"id":"https://remote.example/follows/3988c","type":"Follow","https://www.w3.org/ns/activitystreams#actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	verifier, payload := signedReplayFixture(t, body)
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub, verifier, nil, nil, nil, newMemReplayGuard())
	ld := &stubLDVerifier{}
	p.SetLDSignatureVerifier(ld)
	require.NoError(t, p.Handle(context.Background(), replayDigestTask(t, payload)))
	require.Equal(t, 1, ld.forbiddenCount)
	assert.Equal(t, body, ld.forbiddenSources[0])
}
