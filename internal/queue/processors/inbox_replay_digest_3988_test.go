package processors_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
)

const replayDigestTestBody = `{"id":"https://remote.example/follows/3988","type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`

// endReplayKeyTransition moves the processor's clock past the window in which
// it also uses the replay key of before #3988.
func endReplayKeyTransition(p *processors.InboxProcessor) {
	after := p.LegacyReplayKeysUntilForTest().Add(time.Second)
	p.SetNowForTest(func() time.Time { return after })
}

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

// The window starts when the processor is built and lasts inboxReplayTTL: a
// key remembered before the switch expires within that time.
func TestInboxProcessor_LegacyReplayKeyWindowIsReplayTTL(t *testing.T) {
	before := time.Now()
	p := processors.NewInboxProcessor(&stubFedProcessor{}, nil, nil, nil, nil, newMemReplayGuard())
	after := time.Now()
	until := p.LegacyReplayKeysUntilForTest()
	assert.False(t, until.Before(before.Add(processors.InboxReplayTTLForTest)))
	assert.False(t, until.After(after.Add(processors.InboxReplayTTLForTest)))
}

// #3988: 再投函の鍵は、署名が覆った Date と Digest から作る。切り替えの窓の後は
// その鍵だけを覚え、同じリクエストの再投函を捨てる。
func TestInboxProcessor_ReplayKeyIsSignedDateAndDigest(t *testing.T) {
	verifier, key := replayDigestFixture(t)
	payload := signedInboxPayloadWithDate(t, key, []byte(replayDigestTestBody), "", true)
	stub := &stubFedProcessor{}
	guard := newMemReplayGuard()
	p := processors.NewInboxProcessor(stub, verifier, nil, nil, nil, guard)
	endReplayKeyTransition(p)

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

// #3988: 鍵の形を変えた直後 (inboxReplayTTL の間) は、前の形の鍵 (本文の
// ダイジェスト) で覚えたリクエストの再投函も捨て、両方の形で覚える。窓の後は
// 前の形の鍵を見ない (その頃には Redis から消えている)。
func TestInboxProcessor_ReplayKeyTransitionHonoursPreviousKeys(t *testing.T) {
	t.Run("replay of a request remembered before the switch is dropped", func(t *testing.T) {
		verifier, payload := signedReplayFixture(t, []byte(replayDigestTestBody))
		stub := &stubFedProcessor{}
		guard := newMemReplayGuard()
		guard.seen[replayKeyOf(payload.Headers["Date"], payload.Body)] = true
		p := processors.NewInboxProcessor(stub, verifier, nil, nil, nil, guard)
		require.NoError(t, p.Handle(context.Background(), replayDigestTask(t, payload)))
		assert.Empty(t, stub.calls, "a request processed before the switch must not be processed again")
	})
	t.Run("both keys are remembered inside the window", func(t *testing.T) {
		verifier, payload := signedReplayFixture(t, []byte(replayDigestTestBody))
		stub := &stubFedProcessor{}
		guard := newMemReplayGuard()
		p := processors.NewInboxProcessor(stub, verifier, nil, nil, nil, guard)
		require.NoError(t, p.Handle(context.Background(), replayDigestTask(t, payload)))
		require.Len(t, stub.calls, 1)
		assert.Equal(t, map[string]bool{
			digestReplayKeyOf(payload.Headers["Date"], payload.Headers["Digest"]): true,
			replayKeyOf(payload.Headers["Date"], payload.Body):                    true,
		}, guard.seen)
	})
	t.Run("the previous key is not consulted after the window", func(t *testing.T) {
		verifier, payload := signedReplayFixture(t, []byte(replayDigestTestBody))
		stub := &stubFedProcessor{}
		guard := newMemReplayGuard()
		guard.seen[replayKeyOf(payload.Headers["Date"], payload.Body)] = true
		p := processors.NewInboxProcessor(stub, verifier, nil, nil, nil, guard)
		endReplayKeyTransition(p)
		require.NoError(t, p.Handle(context.Background(), replayDigestTask(t, payload)))
		assert.Len(t, stub.calls, 1)
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
