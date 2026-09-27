package processors_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/core/deliveryhealth"
	"github.com/shiroha-a/mk/internal/core/federation"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
	"github.com/shiroha-a/mk/internal/queue/processors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubFedProcessor は processors.FederationProcessor を満たす最小スタブ。
// federation.NewProcessor の重い依存 chain (resolver / userRepo / noteRepo)
// を組まずに InboxProcessor の dispatch 経路を unit-test するため切る。
type stubFedProcessor struct {
	calls    [][]byte
	returnFn func(body []byte) error
}

func (s *stubFedProcessor) Process(body []byte) error {
	s.calls = append(s.calls, body)
	if s.returnFn != nil {
		return s.returnFn(body)
	}
	return nil
}

func mustEncode(t *testing.T, p queue.InboxPayload) []byte {
	t.Helper()
	b, err := json.Marshal(p)
	require.NoError(t, err)
	return b
}

// #534: InboxProcessor は payload decode 失敗を ErrSkipRetry で確定 fail に
// する (壊れた payload を retry しても無限ループするため)。
func TestInboxProcessor_DecodeFailureIsSkipRetry(t *testing.T) {
	p := processors.NewInboxProcessor(&stubFedProcessor{})

	err := p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     []byte(`{not json`),
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, driver.ErrSkipRetry),
		"malformed payload should bubble up driver.ErrSkipRetry to suppress retries")
}

// 正常 path: federation.Processor.Process が nil を返せば Handle も nil。
func TestInboxProcessor_HappyPathDelegatesToFederation(t *testing.T) {
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)

	body := []byte(`{"type":"Follow","actor":"https://e/u/a"}`)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, queue.InboxPayload{Body: body, Host: "e"}),
	}))
	require.Len(t, stub.calls, 1)
	assert.JSONEq(t, string(body), string(stub.calls[0]))
}

// ErrUnsupportedActivity は HTTP 同期処理時と同じく成功扱い。worker が
// 永続 retry に持ち込まないようにするため。
func TestInboxProcessor_UnsupportedActivityIsSwallowed(t *testing.T) {
	stub := &stubFedProcessor{returnFn: func(_ []byte) error {
		return federation.ErrUnsupportedActivity
	}}
	p := processors.NewInboxProcessor(stub)

	err := p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, queue.InboxPayload{Body: []byte(`{}`)}),
	})
	assert.NoError(t, err, "unsupported type should not fail the worker")
}

// stubVerifier returns the supplied actor + pubkey deterministically so
// worker-side signature verification can be exercised without the full
// federation.Resolver dep chain.
type stubVerifier struct {
	actor  *model.User
	pubKey string
}

func (s *stubVerifier) ResolveActor(_ string) (*model.User, error) {
	if s.actor == nil {
		return nil, errors.New("not found")
	}
	return s.actor, nil
}

func (s *stubVerifier) PublicKeyForActor(_ string) (string, error) {
	if s.pubKey == "" {
		return "", errors.New("no key")
	}
	return s.pubKey, nil
}

// PublicKeyForKeyID は dual lookup を mock では行わず、常に primary key を
// 返す。Ed25519 / keyId fragment 別 dispatch のテストは federation.Resolver
// 側の integration test で cover する。
func (s *stubVerifier) PublicKeyForKeyID(actorID, _ string) (string, error) {
	return s.PublicKeyForActor(actorID)
}

type stubBlocker struct{ blocked, allowed func(string) bool }

func (s *stubBlocker) IsBlocked(h string) bool {
	if s.blocked == nil {
		return false
	}
	return s.blocked(h)
}

func (s *stubBlocker) IsAllowed(h string) bool {
	if s.allowed == nil {
		return true
	}
	return s.allowed(h)
}

type stubInstanceTracker struct{ hosts []string }

func (s *stubInstanceTracker) MarkRequestReceived(host string) error {
	s.hosts = append(s.hosts, host)
	return nil
}

type stubInboxChartHook struct{ hosts []string }

func (s *stubInboxChartHook) OnInboxReceived(host string) {
	s.hosts = append(s.hosts, host)
}

// signed builds a payload whose Headers/Method/Path can be re-verified
// against the supplied private key. Mirrors what the new handler emits.
func signedInboxPayload(t *testing.T, key *activitypub.PrivateKey, body []byte) queue.InboxPayload {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "https://example.com/inbox", nil)
	req.Host = "example.com"
	digest := activitypub.SHA256Digest(body)
	require.NoError(t, activitypub.SignRequest(req, key, digest, []string{"(request-target)", "date", "host", "digest"}))
	headers := map[string]string{}
	for _, h := range []string{"Signature", "Date", "Host", "Digest"} {
		if v := req.Header.Get(h); v != "" {
			headers[h] = v
		}
	}
	if headers["Host"] == "" && req.Host != "" {
		headers["Host"] = req.Host
	}
	return queue.InboxPayload{
		Body:    body,
		Method:  req.Method,
		Path:    req.URL.Path,
		Headers: headers,
	}
}

// #565: payload に Headers が含まれている場合、worker は signature を
// 再 verify し、成功時に instance tracker / chart hook を呼ぶ。
func TestInboxProcessor_VerifiesSignatureAndCommitsHooks(t *testing.T) {
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
	tracker := &stubInstanceTracker{}
	chart := &stubInboxChartHook{}

	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)
	p.SetInstanceTracker(tracker)
	p.SetChartHook(chart)

	body := []byte(`{"id":"https://remote.example/follows/1","type":"Follow","actor":"https://remote.example/users/alice"}`)
	payload := signedInboxPayload(t, key, body)

	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	}))
	require.Len(t, stub.calls, 1, "Process should run after verify")
	assert.Equal(t, []string{host}, tracker.hosts)
	assert.Equal(t, []string{host}, chart.hosts)
}

// #1779: activity.id の host が actor の host と一致しなければ drop される
// (foreign-host id spoofing 対策、upstream signerHost!==activity.id host gate)。
func TestInboxProcessor_RejectsForeignActivityIDHost(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	host := "remote.example"
	aliceURI := "https://remote.example/users/alice"
	verifier := &stubVerifier{actor: &model.User{ID: "alice", Host: &host, URI: &aliceURI}, pubKey: pub}

	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)

	// activity.id が foreign host (evil.example) → drop。
	body := []byte(`{"id":"https://evil.example/announces/1","type":"Announce","actor":"https://remote.example/users/alice"}`)
	payload := signedInboxPayload(t, key, body)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	}))
	assert.Empty(t, stub.calls, "foreign-host activity.id must be dropped before Process")
}

// #1779: activity.id が欠落 (string でない) なら drop される。
func TestInboxProcessor_RejectsMissingActivityID(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	host := "remote.example"
	aliceURI := "https://remote.example/users/alice"
	verifier := &stubVerifier{actor: &model.User{ID: "alice", Host: &host, URI: &aliceURI}, pubKey: pub}

	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice"}`)
	payload := signedInboxPayload(t, key, body)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	}))
	assert.Empty(t, stub.calls, "activity without a string id must be dropped")
}

// host が blockedHosts に入っていれば verify 後に Process まで到達せず
// silently drop される (sender に retry 要求しない)。
func TestInboxProcessor_BlockedHostDropped(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	host := "remote.example"
	verifier := &stubVerifier{
		actor:  &model.User{ID: "alice", Host: &host},
		pubKey: pub,
	}
	blocker := &stubBlocker{blocked: func(h string) bool { return h == host }}

	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)
	p.SetHostBlockChecker(blocker)

	body := []byte(`{"type":"Follow"}`)
	payload := signedInboxPayload(t, key, body)

	err = p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	})
	assert.NoError(t, err, "blocked host should not bubble up an error")
	assert.Empty(t, stub.calls, "Process must NOT run for blocked host")
}

// signature 検証失敗 (例: pubkey 不一致) は drop で扱う。retry しても
// 成功しない静的 error なので driver retry を抑止する。
func TestInboxProcessor_VerifyFailureDropped(t *testing.T) {
	priv, _, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	// 別 keypair の pub を持たせる → verify が失敗する
	_, otherPub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)

	host := "remote.example"
	verifier := &stubVerifier{
		actor:  &model.User{ID: "alice", Host: &host},
		pubKey: otherPub,
	}
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)

	payload := signedInboxPayload(t, key, []byte(`{"type":"Follow"}`))
	err = p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	})
	assert.NoError(t, err, "verify failures are dropped silently, not retried")
	assert.Empty(t, stub.calls)
}

// hostBlocker が "allowed=false" を返した場合も blocked 経路と同じく
// silently drop されること (IsBlocked=false でも IsAllowed=false なら
// federation policy 上 reject)。
func TestInboxProcessor_DisallowedHostDropped(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)
	host := "remote.example"
	verifier := &stubVerifier{actor: &model.User{ID: "alice", Host: &host}, pubKey: pub}
	blocker := &stubBlocker{
		blocked: func(string) bool { return false },
		allowed: func(string) bool { return false },
	}
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)
	p.SetHostBlockChecker(blocker)

	payload := signedInboxPayload(t, key, []byte(`{}`))
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	}))
	assert.Empty(t, stub.calls)
}

// host が nil な actor (= local) は blocked / tracker / chart 全て
// no-op で素通りし Process まで到達する。
func TestInboxProcessor_LocalActorSkipsHostDependentHooks(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)
	verifier := &stubVerifier{actor: &model.User{ID: "alice", Host: nil}, pubKey: pub}
	tracker := &stubInstanceTracker{}
	chart := &stubInboxChartHook{}
	blocker := &stubBlocker{}

	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)
	p.SetHostBlockChecker(blocker)
	p.SetInstanceTracker(tracker)
	p.SetChartHook(chart)

	payload := signedInboxPayload(t, key, []byte(`{}`))
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	}))
	require.Len(t, stub.calls, 1)
	assert.Empty(t, tracker.hosts, "local actor must not trigger instance tracker")
	assert.Empty(t, chart.hosts, "local actor must not trigger chart hook")
}

// signature header が壊れていれば parse 段階で失敗 → drop。
func TestInboxProcessor_InvalidSignatureHeaderDropped(t *testing.T) {
	verifier := &stubVerifier{actor: &model.User{ID: "alice"}, pubKey: "x"}
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)

	payload := queue.InboxPayload{
		Body:    []byte(`{}`),
		Method:  "POST",
		Path:    "/inbox",
		Headers: map[string]string{"Signature": "not-a-valid-sig", "Host": "x", "Date": "today"},
	}
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	}))
	assert.Empty(t, stub.calls)
}

// resolver が actor を見つけられない (= ResolveActor error) と verify は
// drop される (404 sender に伝えても解決しないため retry しない)。
func TestInboxProcessor_ResolveActorErrorDropped(t *testing.T) {
	priv, _, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	verifier := &stubVerifier{actor: nil, pubKey: ""}
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)

	payload := signedInboxPayload(t, key, []byte(`{}`))
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	}))
	assert.Empty(t, stub.calls)
}

// Headers が空の payload (= legacy 形式 / 同期 handler 経路) では verify
// は skip され、Process が直接走る。後方互換のため。
func TestInboxProcessor_LegacyPayloadSkipsVerify(t *testing.T) {
	verifier := &stubVerifier{} // never called
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)

	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body: mustEncode(t, queue.InboxPayload{
			Body: []byte(`{"type":"Follow"}`),
			Host: "legacy.example",
		}),
	}))
	require.Len(t, stub.calls, 1, "legacy payload still reaches Process")
}

// --- LD-Signature gate (#1164 Phase D) ---

// stubLDVerifier captures LD-Signature verifier calls and lets tests force
// the outcome.
type stubLDVerifier struct {
	err error
	// VerifyAndCompact outcome (forwarded-activity and legacy paths).
	creator      string
	present      bool
	creatorCount int
	// compactedBody overrides the Body VerifyAndCompact returns.
	compactedBody []byte
	// CheckForbiddenDirectivesIfPresent outcome (signer==actor path, #2106 N26).
	forbiddenErr   error
	forbiddenCount int
}

func (s *stubLDVerifier) CheckForbiddenDirectivesIfPresent(_ []byte) error {
	s.forbiddenCount++
	return s.forbiddenErr
}

// VerifyAndCompact は compact の代わりに入力をそのまま Body として返す
// (compactedBody を指定したときはそれを返す)。
func (s *stubLDVerifier) VerifyAndCompact(body []byte) (federation.VerifiedLDActivity, bool, error) {
	s.creatorCount++
	if s.err != nil {
		return federation.VerifiedLDActivity{}, s.present, s.err
	}
	out := body
	if s.compactedBody != nil {
		out = s.compactedBody
	}
	return federation.VerifiedLDActivity{Creator: s.creator, Body: out}, s.present, nil
}

// multiActorVerifier resolves different actors keyed by the (fragment-less)
// actor URI passed to ResolveActor, so actor-authorization tests can model a
// signer and an LD-Signature creator that map to distinct users.
type multiActorVerifier struct {
	pubKey string
	byURI  map[string]*model.User
	// errByURI injects a per-URI lookup failure (#3121)。「解決できない」と
	// 「引けなかった」を分けて試すために使う。
	errByURI map[string]error
}

func (m *multiActorVerifier) ResolveActor(actorURI string) (*model.User, error) {
	if err, ok := m.errByURI[actorURI]; ok {
		return nil, err
	}
	if u, ok := m.byURI[actorURI]; ok {
		return u, nil
	}
	return nil, errors.New("not found")
}

func (m *multiActorVerifier) PublicKeyForActor(_ string) (string, error) {
	return m.pubKey, nil
}

func (m *multiActorVerifier) PublicKeyForKeyID(_, _ string) (string, error) {
	return m.pubKey, nil
}

func uptr(s string) *string { return &s }

// 署名者 (HTTP signature) と activity body の actor が異なり、LD-Signature も
// 無い場合は actor spoofing として drop される。
func TestInboxProcessor_ActorSpoofDropped(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	// 署名者 = evil/bob だが body actor は victim/alice を詐称する。
	key, err := activitypub.NewPrivateKey("https://evil.example/users/bob#main-key", priv)
	require.NoError(t, err)

	bobHost := "evil.example"
	verifier := &multiActorVerifier{
		pubKey: pub,
		byURI: map[string]*model.User{
			"https://evil.example/users/bob": {ID: "bob", Host: &bobHost, URI: uptr("https://evil.example/users/bob")},
		},
	}
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)
	p.SetLDSignatureVerifier(&stubLDVerifier{present: false})

	body := []byte(`{"type":"Delete","actor":"https://victim.example/users/alice","object":"https://victim.example/notes/1"}`)
	payload := signedInboxPayload(t, key, body)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	}))
	assert.Empty(t, stub.calls, "spoofed actor (signer != actor, no LD-sig) must be dropped")
}

// 署名者 != actor でも、LD-Signature が body actor を正しく認証していれば
// 転送活動として受理する (Mastodon-style forwarding)。
func TestInboxProcessor_ForwardedWithValidLDSig(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	// 署名者 = relay。
	key, err := activitypub.NewPrivateKey("https://relay.example/actor#main-key", priv)
	require.NoError(t, err)

	relayHost := "relay.example"
	originHost := "origin.example"
	verifier := &multiActorVerifier{
		pubKey: pub,
		byURI: map[string]*model.User{
			"https://relay.example/actor":        {ID: "relay", Host: &relayHost, URI: uptr("https://relay.example/actor")},
			"https://origin.example/users/alice": {ID: "alice", Host: &originHost, URI: uptr("https://origin.example/users/alice")},
		},
	}
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)
	// LD-Signature が origin/alice の鍵で署名済 (creator が alice を指す)。
	p.SetLDSignatureVerifier(&stubLDVerifier{present: true, creator: "https://origin.example/users/alice#main-key"})

	body := []byte(`{"id":"https://origin.example/creates/1","type":"Create","actor":"https://origin.example/users/alice","signature":{"type":"RsaSignature2017","creator":"https://origin.example/users/alice#main-key"}}`)
	payload := signedInboxPayload(t, key, body)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	}))
	require.Len(t, stub.calls, 1, "forwarded activity authenticated by LD-Signature must be processed")
}

// activity.actor が embedded object ({"id": ...}) 形式でも署名者 URI と
// 照合できる (upstream #17340 互換)。
func TestInboxProcessor_ActorObjectFormMatches(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	host := "remote.example"
	verifier := &multiActorVerifier{
		pubKey: pub,
		byURI: map[string]*model.User{
			"https://remote.example/users/alice": {ID: "alice", Host: &host, URI: uptr("https://remote.example/users/alice")},
		},
	}
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)

	// `id` を持たせる。upstream も `typeof activity.id !== 'string'` で
	// drop するので、無いと actor 形式に関係なく落ちてゲートを通らない。
	body := []byte(`{"id":"https://remote.example/activities/1","type":"Follow","actor":{"id":"https://remote.example/users/alice","type":"Person"}}`)
	payload := signedInboxPayload(t, key, body)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	}))
	require.Len(t, stub.calls, 1, "object-form actor matching the signer must be processed")
}

// 署名者 != actor かつ LD verifier 未配線なら drop。
func TestInboxProcessor_ActorMismatchNoLDVerifierDropped(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://evil.example/users/bob#main-key", priv)
	require.NoError(t, err)

	bobHost := "evil.example"
	verifier := &multiActorVerifier{
		pubKey: pub,
		byURI: map[string]*model.User{
			"https://evil.example/users/bob": {ID: "bob", Host: &bobHost, URI: uptr("https://evil.example/users/bob")},
		},
	}
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier) // ldVerifier は配線しない

	body := []byte(`{"type":"Delete","actor":"https://victim.example/users/alice"}`)
	payload := signedInboxPayload(t, key, body)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	}))
	assert.Empty(t, stub.calls, "mismatch with no LD verifier must be dropped")
}

// 署名者 != actor、LD-Signature は present だが creator を resolve できない
// 場合は drop。
func TestInboxProcessor_ForwardedLDCreatorUnresolvableDropped(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://relay.example/actor#main-key", priv)
	require.NoError(t, err)

	relayHost := "relay.example"
	verifier := &multiActorVerifier{
		pubKey: pub,
		byURI: map[string]*model.User{
			"https://relay.example/actor": {ID: "relay", Host: &relayHost, URI: uptr("https://relay.example/actor")},
			// LD creator (unknown.example) は byURI に無い → ResolveActor が error。
		},
	}
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)
	p.SetLDSignatureVerifier(&stubLDVerifier{present: true})

	body := []byte(`{"type":"Create","actor":"https://origin.example/users/alice","signature":{"type":"RsaSignature2017","creator":"https://unknown.example/users/x#main-key"}}`)
	payload := signedInboxPayload(t, key, body)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	}))
	assert.Empty(t, stub.calls, "unresolvable LD creator must be dropped")
}

// 署名者 != actor で LD-Signature はあるが、その creator が body actor とは
// 別人を指す場合は drop する (自分の鍵で署名して他人を詐称する攻撃)。
func TestInboxProcessor_ForwardedLDSigWrongActorDropped(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://relay.example/actor#main-key", priv)
	require.NoError(t, err)

	relayHost := "relay.example"
	evilHost := "evil.example"
	verifier := &multiActorVerifier{
		pubKey: pub,
		byURI: map[string]*model.User{
			"https://relay.example/actor":    {ID: "relay", Host: &relayHost, URI: uptr("https://relay.example/actor")},
			"https://evil.example/users/mal": {ID: "mal", Host: &evilHost, URI: uptr("https://evil.example/users/mal")},
		},
	}
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)
	// LD creator = mal だが body actor は alice を詐称。
	p.SetLDSignatureVerifier(&stubLDVerifier{present: true})

	body := []byte(`{"type":"Create","actor":"https://origin.example/users/alice","signature":{"type":"RsaSignature2017","creator":"https://evil.example/users/mal#main-key"}}`)
	payload := signedInboxPayload(t, key, body)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	}))
	assert.Empty(t, stub.calls, "LD-Signature creator != activity.actor must be dropped")
}

// AUTH-1 regression guard: gate は Process と同じ正規化 (unwrap + JSON-LD
// Normalize) を経た actor で判定するため、`as:actor` / `actor:{"@id":...}` /
// singleton-array wrap のいずれの形状でも署名者と異なる actor を詐称した
// 活動は drop される (raw body を直 parse していた頃はこれらで gate を
// すり抜けられた)。
func TestInboxProcessor_SpoofShapesDropped(t *testing.T) {
	bobHost := "evil.example"
	newP := func(t *testing.T) (*processors.InboxProcessor, *stubFedProcessor, *activitypub.PrivateKey) {
		t.Helper()
		priv, pub, err := activitypub.GenerateRSAKeypair()
		require.NoError(t, err)
		key, err := activitypub.NewPrivateKey("https://evil.example/users/bob#main-key", priv)
		require.NoError(t, err)
		verifier := &multiActorVerifier{
			pubKey: pub,
			byURI: map[string]*model.User{
				"https://evil.example/users/bob": {ID: "bob", Host: &bobHost, URI: uptr("https://evil.example/users/bob")},
			},
		}
		stub := &stubFedProcessor{}
		p := processors.NewInboxProcessor(stub)
		p.SetSignatureVerifier(verifier)
		p.SetLDSignatureVerifier(&stubLDVerifier{present: false})
		return p, stub, key
	}

	cases := map[string]string{
		"as:actor prefix": `{"type":"Delete","as:actor":"https://victim.example/users/alice","object":"https://victim.example/notes/1"}`,
		"@id object form": `{"type":"Delete","actor":{"@id":"https://victim.example/users/alice"},"object":"https://victim.example/notes/1"}`,
		"singleton array": `[{"type":"Delete","actor":"https://victim.example/users/alice","object":"https://victim.example/notes/1"}]`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p, stub, key := newP(t)
			payload := signedInboxPayload(t, key, []byte(body))
			require.NoError(t, p.Handle(context.Background(), driver.RawTask{
				TypeName: queue.TaskTypeInbox,
				Body:     mustEncode(t, payload),
			}))
			assert.Empty(t, stub.calls, "spoofed actor via %s must be dropped", name)
		})
	}
}

// SetLDSignatureVerifier 未配線 (= ldVerifier nil) なら gate は skip され
// federation.Processor.Process が呼ばれる。
func TestInboxProcessor_LDVerify_NoVerifierBypassesGate(t *testing.T) {
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, queue.InboxPayload{Body: []byte(`{"type":"Note"}`)}),
	}))
	require.Len(t, stub.calls, 1)
}

// LD-Sig verify pass (= VerifyAndCompact nil error) なら Process が呼ばれる。
func TestInboxProcessor_LDVerify_PassDelegates(t *testing.T) {
	stub := &stubFedProcessor{}
	verifier := &stubLDVerifier{err: nil}
	p := processors.NewInboxProcessor(stub)
	p.SetLDSignatureVerifier(verifier)

	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, queue.InboxPayload{Body: []byte(`{"type":"Note"}`)}),
	}))
	assert.Equal(t, 1, verifier.creatorCount, "VerifyAndCompact is called exactly once")
	require.Len(t, stub.calls, 1, "verify pass → Process is called")
}

// LD-Sig verify fail (= VerifyAndCompact returns error) なら Process は呼ばれず
// activity が drop される (= queue ack するが Process bypass)。
func TestInboxProcessor_LDVerify_FailDropsActivity(t *testing.T) {
	stub := &stubFedProcessor{}
	verifier := &stubLDVerifier{err: errors.New("bad signature")}
	p := processors.NewInboxProcessor(stub)
	p.SetLDSignatureVerifier(verifier)

	err := p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, queue.InboxPayload{Body: []byte(`{"type":"Note"}`)}),
	})
	require.NoError(t, err, "verify fail でも error は返さず ack (= 同 activity を retry に乗せない)")
	assert.Equal(t, 1, verifier.creatorCount)
	require.Len(t, stub.calls, 0, "verify fail → Process is NOT called (activity dropped)")
}

// Headers の無い legacy 経路でも、LD-Signature を検証したなら**署名が覆う内容**
// (compact 済みの文書) を渡す。生 body には署名外のキーを誰でも足せる。
func TestInboxProcessor_LDVerify_LegacyPathDispatchesCompactedBody(t *testing.T) {
	raw := []byte(`{"type":"Note","_misskey_content":"FORGED","signature":{"type":"RsaSignature2017"}}`)
	compacted := []byte(`{"type":"Note","content":"signed"}`)
	tests := []struct {
		name    string
		present bool
		want    []byte
	}{
		{name: "signature present dispatches compacted body", present: true, want: compacted},
		{name: "no signature dispatches raw body", present: false, want: raw},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubFedProcessor{}
			p := processors.NewInboxProcessor(stub)
			p.SetLDSignatureVerifier(&stubLDVerifier{present: tc.present, compactedBody: compacted})
			require.NoError(t, p.Handle(context.Background(), driver.RawTask{
				TypeName: queue.TaskTypeInbox,
				Body:     mustEncode(t, queue.InboxPayload{Body: raw}),
			}))
			require.Len(t, stub.calls, 1)
			assert.JSONEq(t, string(tc.want), string(stub.calls[0]))
		})
	}
}

// 任意 error は driver の retry policy (inboxJobMaxAttempts) に任せるため
// そのまま返す (ErrSkipRetry を付けない)。
func TestInboxProcessor_GenericErrorPropagatesForRetry(t *testing.T) {
	boom := errors.New("transient db error")
	stub := &stubFedProcessor{returnFn: func(_ []byte) error { return boom }}
	p := processors.NewInboxProcessor(stub)

	err := p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, queue.InboxPayload{Body: []byte(`{}`)}),
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, boom), "underlying error must be wrapped, not swallowed")
	assert.False(t, errors.Is(err, driver.ErrSkipRetry),
		"transient errors should NOT carry ErrSkipRetry — driver retry handles them")
}

// #2106 N26: HTTP 署名済 + signer==actor の通常経路では、LD-Signature verify 失敗
// (creator 鍵未解決 / legacy LD-sig 等) でも正規 activity を drop しない (upstream は
// LD-Signature を一切検証しない)。forbidden-directive hardening は維持する。
func TestInboxProcessor_SignerIsActor_LDSigVerifyFailureNotDropped(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	host := "remote.example"
	verifier := &multiActorVerifier{
		pubKey: pub,
		byURI: map[string]*model.User{
			"https://remote.example/users/alice": {ID: "alice", Host: &host, URI: uptr("https://remote.example/users/alice")},
		},
	}
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)
	// VerifyAndCompact はエラー (鍵未解決) だが forbidden-directive check は通る。
	ldv := &stubLDVerifier{present: true, err: errors.New("ld-sig: public key not found"), forbiddenErr: nil}
	p.SetLDSignatureVerifier(ldv)

	body := []byte(`{"id":"https://remote.example/creates/1","type":"Create","actor":"https://remote.example/users/alice","signature":{"type":"RsaSignature2017","creator":"https://remote.example/users/alice#main-key"}}`)
	payload := signedInboxPayload(t, key, body)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox, Body: mustEncode(t, payload),
	}))
	require.Len(t, stub.calls, 1, "signer==actor の HTTP 署名済 activity は LD-sig verify 失敗でも処理される")
	assert.Equal(t, 0, ldv.creatorCount, "signer==actor 経路では VerifyAndCompact を呼ばない")
	assert.Equal(t, 1, ldv.forbiddenCount, "forbidden-directive hardening は実行する")
}

// #2106 N26: signer==actor でも forbidden directive が検出されれば drop する (hardening 維持)。
func TestInboxProcessor_SignerIsActor_ForbiddenDirectiveDropped(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	host := "remote.example"
	verifier := &multiActorVerifier{
		pubKey: pub,
		byURI: map[string]*model.User{
			"https://remote.example/users/alice": {ID: "alice", Host: &host, URI: uptr("https://remote.example/users/alice")},
		},
	}
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)
	ldv := &stubLDVerifier{present: true, forbiddenErr: errors.New("ld-sig: forbidden directive")}
	p.SetLDSignatureVerifier(ldv)

	body := []byte(`{"id":"https://remote.example/creates/1","type":"Create","actor":"https://remote.example/users/alice","signature":{"type":"RsaSignature2017","creator":"https://remote.example/users/alice#main-key"}}`)
	payload := signedInboxPayload(t, key, body)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox, Body: mustEncode(t, payload),
	}))
	require.Len(t, stub.calls, 0, "forbidden directive は signer==actor でも drop する")
	assert.Equal(t, 1, ldv.forbiddenCount)
}

// memReplayGuard is an in-memory InboxReplayGuard for tests.
type memReplayGuard struct {
	seen map[string]bool
	err  error
}

func newMemReplayGuard() *memReplayGuard { return &memReplayGuard{seen: map[string]bool{}} }

func (g *memReplayGuard) Seen(_ context.Context, id string) (bool, error) {
	if g.err != nil {
		return false, g.err
	}
	return g.seen[id], nil
}

func (g *memReplayGuard) Remember(_ context.Context, id string) error {
	if g.err != nil {
		return g.err
	}
	g.seen[id] = true
	return nil
}

// signedReplayFixture builds a verifier + signed payload for one remote actor.
// 既存の signedInboxFixture (telemetry test 側) は actor に URI を持たせないので、
// authorizeActor を通る形の別ヘルパにしてある。
func signedReplayFixture(t *testing.T, body []byte) (*stubVerifier, queue.InboxPayload) {
	t.Helper()
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)
	host := "remote.example"
	aliceURI := "https://remote.example/users/alice"
	v := &stubVerifier{actor: &model.User{ID: "alice", Host: &host, URI: &aliceURI}, pubKey: pub}
	return v, signedInboxPayload(t, key, body)
}

// 同じ activity を 2 度投げたら 2 度目は処理されないこと。
//
// **署名の Date に窓を入れても、窓の内側はまだ投げ直せる。** ハンドラは冪等
// なので単発の再送は無害だが、Undo(Follow) のような「状態を戻す」活動を後から
// 差し込まれると連合の状態が巻き戻る。
func TestInboxProcessor_DropsReplayedActivity(t *testing.T) {
	body := []byte(`{"id":"https://remote.example/follows/1","type":"Follow","actor":"https://remote.example/users/alice"}`)
	verifier, payload := signedReplayFixture(t, body)

	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)
	p.SetInboxReplayGuard(newMemReplayGuard())

	task := driver.RawTask{TypeName: queue.TaskTypeInbox, Body: mustEncode(t, payload)}
	require.NoError(t, p.Handle(context.Background(), task))
	require.Len(t, stub.calls, 1)

	// 同じ payload をそのまま投げ直す (= 捕まえた署名付きリクエストの再投函)。
	require.NoError(t, p.Handle(context.Background(), task))
	assert.Len(t, stub.calls, 1, "再投函が処理されている")
}

// **処理が失敗したら覚えない。** 先に覚えると、キューの再試行を自分で
// 再投函として捨ててしまい、activity が永久に失われる。
func TestInboxProcessor_RetryAfterFailureIsNotDropped(t *testing.T) {
	body := []byte(`{"id":"https://remote.example/follows/2","type":"Follow","actor":"https://remote.example/users/alice"}`)
	verifier, payload := signedReplayFixture(t, body)

	failing := true
	stub := &stubFedProcessor{returnFn: func([]byte) error {
		if failing {
			return errors.New("transient")
		}
		return nil
	}}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)
	p.SetInboxReplayGuard(newMemReplayGuard())

	task := driver.RawTask{TypeName: queue.TaskTypeInbox, Body: mustEncode(t, payload)}
	require.Error(t, p.Handle(context.Background(), task))
	require.Len(t, stub.calls, 1)

	// 再試行は通ること。
	failing = false
	require.NoError(t, p.Handle(context.Background(), task))
	assert.Len(t, stub.calls, 2, "再試行が再投函として捨てられている")
}

// guard が壊れていても受信は止めない (fail-open)。**Redis の不調で連合の受信が
// 止まるほうが、再投函を通すより害が大きい。**
func TestInboxProcessor_ReplayGuardFailureIsFailOpen(t *testing.T) {
	body := []byte(`{"id":"https://remote.example/follows/3","type":"Follow","actor":"https://remote.example/users/alice"}`)
	verifier, payload := signedReplayFixture(t, body)

	guard := newMemReplayGuard()
	guard.err = errors.New("redis down")

	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)
	p.SetInboxReplayGuard(guard)

	task := driver.RawTask{TypeName: queue.TaskTypeInbox, Body: mustEncode(t, payload)}
	require.NoError(t, p.Handle(context.Background(), task))
	assert.Len(t, stub.calls, 1, "guard 障害で受信が止まっている")
}

// guard 未配線でも従来どおり動くこと。
func TestInboxProcessor_WorksWithoutReplayGuard(t *testing.T) {
	body := []byte(`{"id":"https://remote.example/follows/4","type":"Follow","actor":"https://remote.example/users/alice"}`)
	verifier, payload := signedReplayFixture(t, body)

	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)

	task := driver.RawTask{TypeName: queue.TaskTypeInbox, Body: mustEncode(t, payload)}
	require.NoError(t, p.Handle(context.Background(), task))
	require.NoError(t, p.Handle(context.Background(), task))
	assert.Len(t, stub.calls, 2, "guard 未配線では従来どおり両方処理する")
}

// **型エラーで actor を空にしてゲートを素通しさせない。** `ExtractActorIRI` が
// 型エラーで "" を返すと `authorizeActor` は「actor 欠落 = Process 側が弾く」と
// 解釈して素通しするが、`process()` は型エラーを握るので actor を読めてしまう。
// 結果、署名者 != actor の LD-Signature 検証・`activity.id` host ゲート・
// replay guard が全部飛ぶ (#2662)。
func TestInboxProcessor_TypeErrorDoesNotBypassActorGate(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://evil.example/users/m#main-key", priv)
	require.NoError(t, err)

	host := "evil.example"
	verifier := &multiActorVerifier{
		pubKey: pub,
		byURI: map[string]*model.User{
			"https://evil.example/users/m": {ID: "m", Host: &host, URI: uptr("https://evil.example/users/m")},
		},
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		// actor が embedded object。署名者は evil、actor は victim。
		{"object form actor", `{"id":"https://evil.example/a/1","type":"Follow","actor":{"id":"https://victim.example/users/bob"}}`},
		// 先行 field の型エラーで actor の decode 自体は成功するケース。
		{"preceding type error", `{"id":"https://evil.example/a/1","type":"Follow","published":1704067200000,"actor":"https://victim.example/users/bob"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubFedProcessor{}
			p := processors.NewInboxProcessor(stub)
			p.SetSignatureVerifier(verifier)
			payload := signedInboxPayload(t, key, []byte(tc.body))
			// drop は Handle が nil を返して Process に渡らない形で観測する
			// (retry させないため)。
			require.NoError(t, p.Handle(context.Background(), driver.RawTask{
				TypeName: queue.TaskTypeInbox,
				Body:     mustEncode(t, payload),
			}))
			assert.Empty(t, stub.calls, "署名者と異なる actor を Process へ渡さない")
		})
	}
}

// unavailableKeyVerifier は鍵の lookup だけを落とす verifier。actor は解決でき
// るので、**署名が合わないのではなく確かめられなかった**状態を再現する (#3121)。
type unavailableKeyVerifier struct {
	actor    *model.User
	pubKey   string
	keyErr   error
	actorErr error
}

func (u *unavailableKeyVerifier) ResolveActor(_ string) (*model.User, error) {
	if u.actorErr != nil {
		return nil, u.actorErr
	}
	return u.actor, nil
}

func (u *unavailableKeyVerifier) PublicKeyForActor(_ string) (string, error) {
	if u.keyErr != nil {
		return "", u.keyErr
	}
	return u.pubKey, nil
}

func (u *unavailableKeyVerifier) PublicKeyForKeyID(actorID, _ string) (string, error) {
	return u.PublicKeyForActor(actorID)
}

// #3121: 署名を「確かめられなかった」ときは ack せず job に retry させる。
// ack すると DB 障害のあいだに届いた activity がそのまま失われる (inbox は
// enqueue した時点で 202 を返すので相手の再送は当てにできない)。
func TestInboxProcessor_VerificationUnavailableIsRetried(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	host := "remote.example"
	aliceURI := "https://remote.example/users/alice"
	actor := &model.User{ID: "alice", Host: &host, URI: &aliceURI}
	body := []byte(`{"id":"https://remote.example/follows/1","type":"Follow","actor":"https://remote.example/users/alice"}`)
	payload := signedInboxPayload(t, key, body)

	t.Run("確かめられなかったときは error を返す", func(t *testing.T) {
		boom := fmt.Errorf("%w: public key for actor %q: connection refused",
			federation.ErrLookupUnavailable, "alice")
		stub := &stubFedProcessor{}
		p := processors.NewInboxProcessor(stub)
		p.SetSignatureVerifier(&unavailableKeyVerifier{actor: actor, keyErr: boom})
		tel := &recordingTelemetry{}
		p.SetDeliveryTelemetry(tel)

		err := p.Handle(context.Background(), driver.RawTask{
			TypeName: queue.TaskTypeInbox,
			Body:     mustEncode(t, payload),
		})
		require.ErrorIs(t, err, federation.ErrLookupUnavailable, "ack に潰している")
		assert.Empty(t, stub.calls, "検証を通していないのに Process へ流している")
		// **分類も見る。** signatureFailed のままだと Errored Instances 上で
		// 「相手の署名が壊れている」に見え、こちらの DB 障害が隠れる。
		require.Len(t, tel.outcomes, 1)
		assert.Equal(t, deliveryhealth.ClassProcessingError, tel.outcomes[0].Class)
	})

	// 鍵ではなく actor の lookup が引けなかった場合も同じ。**こちらのほうが
	// 先に走る**ので、塞がないと DB 障害のあいだ届いた activity はここで落ちる。
	t.Run("actor の lookup が引けないときも error を返す", func(t *testing.T) {
		boom := fmt.Errorf("%w: actor %q: connection refused",
			federation.ErrLookupUnavailable, aliceURI)
		stub := &stubFedProcessor{}
		p := processors.NewInboxProcessor(stub)
		p.SetSignatureVerifier(&unavailableKeyVerifier{actorErr: boom, pubKey: pub})
		tel := &recordingTelemetry{}
		p.SetDeliveryTelemetry(tel)

		err := p.Handle(context.Background(), driver.RawTask{
			TypeName: queue.TaskTypeInbox,
			Body:     mustEncode(t, payload),
		})
		require.ErrorIs(t, err, federation.ErrLookupUnavailable, "ack に潰している")
		assert.Empty(t, stub.calls)
		require.Len(t, tel.outcomes, 1)
		assert.Equal(t, deliveryhealth.ClassProcessingError, tel.outcomes[0].Class)
	})

	// **対になる側も見る。** 両方 retry に倒す実装でも上だけなら緑になる。
	// 署名が合わない body は retry しても結果が変わらないので ack のまま。
	t.Run("署名検証の失敗は従来どおり ack", func(t *testing.T) {
		stub := &stubFedProcessor{}
		p := processors.NewInboxProcessor(stub)
		p.SetSignatureVerifier(&unavailableKeyVerifier{actor: actor, keyErr: errors.New("no key")})
		tel := &recordingTelemetry{}
		p.SetDeliveryTelemetry(tel)

		require.NoError(t, p.Handle(context.Background(), driver.RawTask{
			TypeName: queue.TaskTypeInbox,
			Body:     mustEncode(t, payload),
		}))
		assert.Empty(t, stub.calls)
		require.Len(t, tel.outcomes, 1)
		assert.Equal(t, deliveryhealth.ClassSignatureFailed, tel.outcomes[0].Class)
	})

	// 正常系が通ることも見る (上 2 つは鍵を返さない経路なので、verify そのものを
	// 常に落とす変異でも両方緑になる)。
	t.Run("検証が通れば Process へ流す", func(t *testing.T) {
		stub := &stubFedProcessor{}
		p := processors.NewInboxProcessor(stub)
		p.SetSignatureVerifier(&unavailableKeyVerifier{actor: actor, pubKey: pub})

		require.NoError(t, p.Handle(context.Background(), driver.RawTask{
			TypeName: queue.TaskTypeInbox,
			Body:     mustEncode(t, payload),
		}))
		require.Len(t, stub.calls, 1)
	})
}

// #3121: 転送 activity の認可も DB を読む (LD-Signature の creator を
// `ResolveActor` で解決し、その鍵を DB から引く)。理由を見ずに drop すると、
// DB 障害のあいだリレー経由の配送がまるごと消える。
func TestInboxProcessor_AuthorizeActorLookupUnavailableIsRetried(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://relay.example/actor#main-key", priv)
	require.NoError(t, err)

	relayHost := "relay.example"
	body := []byte(`{"id":"https://origin.example/creates/1","type":"Create","actor":"https://origin.example/users/alice","signature":{"type":"RsaSignature2017","creator":"https://origin.example/users/alice#main-key"}}`)
	payload := signedInboxPayload(t, key, body)

	boom := fmt.Errorf("%w: actor %q: connection refused",
		federation.ErrLookupUnavailable, "https://origin.example/users/alice")
	verifier := &multiActorVerifier{
		pubKey: pub,
		byURI: map[string]*model.User{
			"https://relay.example/actor": {ID: "relay", Host: &relayHost, URI: uptr("https://relay.example/actor")},
		},
		// creator (origin/alice) の解決だけ落とす。
		errByURI: map[string]error{"https://origin.example/users/alice": boom},
	}
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)
	p.SetLDSignatureVerifier(&stubLDVerifier{present: true, creator: "https://origin.example/users/alice#main-key"})
	tel := &recordingTelemetry{}
	p.SetDeliveryTelemetry(tel)

	err = p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	})
	require.ErrorIs(t, err, federation.ErrLookupUnavailable, "ack に潰している")
	assert.Empty(t, stub.calls)
	require.Len(t, tel.outcomes, 1)
	assert.Equal(t, deliveryhealth.ClassProcessingError, tel.outcomes[0].Class)
}

// #3121: Headers 無しの legacy / direct-enqueue 経路も同じ。LD-Signature の
// creator の鍵は DB から引くので、理由を見ずに drop すると消える。
func TestInboxProcessor_LegacyLDVerifyLookupUnavailableIsRetried(t *testing.T) {
	boom := fmt.Errorf("%w: ld-sig public key: connection refused", federation.ErrLookupUnavailable)
	body := []byte(`{"id":"https://remote.example/creates/1","type":"Create","actor":"https://remote.example/users/alice"}`)

	t.Run("確かめられなかったときは error を返す", func(t *testing.T) {
		stub := &stubFedProcessor{}
		p := processors.NewInboxProcessor(stub)
		p.SetLDSignatureVerifier(&stubLDVerifier{err: boom})

		err := p.Handle(context.Background(), driver.RawTask{
			TypeName: queue.TaskTypeInbox,
			Body:     mustEncode(t, queue.InboxPayload{Body: body}),
		})
		require.ErrorIs(t, err, federation.ErrLookupUnavailable, "ack に潰している")
		assert.Empty(t, stub.calls)
	})

	// **対になる側。** 検証そのものの失敗は従来どおり drop する。
	t.Run("検証失敗は従来どおり drop", func(t *testing.T) {
		stub := &stubFedProcessor{}
		p := processors.NewInboxProcessor(stub)
		p.SetLDSignatureVerifier(&stubLDVerifier{err: errors.New("bad signature")})

		require.NoError(t, p.Handle(context.Background(), driver.RawTask{
			TypeName: queue.TaskTypeInbox,
			Body:     mustEncode(t, queue.InboxPayload{Body: body}),
		}))
		assert.Empty(t, stub.calls)
	})
}

// **転送された actor 側の連合可否も見ること。**
//
// ブロック判定は HTTP 署名者にしか掛かっていなかったので、第三者 (リレー等) が
// 転送すると `blockedHosts` に入れたホストの actor でも LD-Signature が有効なら
// 処理されていた。`Announce` / `Like` / `Follow` / `Block` / `Flag` / `Move` /
// chat がそのまま通り、defederation が素通りする。
func TestInboxProcessor_ForwardedActorFromBlockedHostDropped(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	// 署名者 = relay (ブロックしていない)。
	key, err := activitypub.NewPrivateKey("https://relay.example/actor#main-key", priv)
	require.NoError(t, err)

	relayHost := "relay.example"
	blockedHost := "blocked.example"
	verifier := &multiActorVerifier{
		pubKey: pub,
		byURI: map[string]*model.User{
			"https://relay.example/actor":         {ID: "relay", Host: &relayHost, URI: uptr("https://relay.example/actor")},
			"https://blocked.example/users/alice": {ID: "alice", Host: &blockedHost, URI: uptr("https://blocked.example/users/alice")},
		},
	}
	stub := &stubFedProcessor{}
	p := processors.NewInboxProcessor(stub)
	p.SetSignatureVerifier(verifier)
	p.SetLDSignatureVerifier(&stubLDVerifier{present: true, creator: "https://blocked.example/users/alice#main-key"})
	// **ブロックしているのは転送元 (actor) のホストだけ。** 署名者は通る。
	p.SetHostBlockChecker(&stubBlocker{blocked: func(h string) bool { return h == blockedHost }})

	body := []byte(`{"id":"https://blocked.example/creates/1","type":"Create","actor":"https://blocked.example/users/alice","signature":{"type":"RsaSignature2017","creator":"https://blocked.example/users/alice#main-key"}}`)
	payload := signedInboxPayload(t, key, body)
	require.NoError(t, p.Handle(context.Background(), driver.RawTask{
		TypeName: queue.TaskTypeInbox,
		Body:     mustEncode(t, payload),
	}), "drop は sender へ retry を要求しない")
	require.Empty(t, stub.calls,
		"ブロックしたホストの actor は、転送経由でも処理しないこと")
}
