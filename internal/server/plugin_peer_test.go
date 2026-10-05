package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/plugin"
)

// --- test doubles ---

type fakePeerResolver struct {
	host string
	pem  string
	err  error
}

func (r *fakePeerResolver) ResolveActor(string) (*model.User, error) {
	if r.err != nil {
		return nil, r.err
	}
	h := r.host
	return &model.User{ID: "u1", Host: &h}, nil
}

func (r *fakePeerResolver) PublicKeyForKeyID(string, string) (string, error) {
	return r.pem, r.err
}

type fakePeerBlocker struct {
	blocked map[string]bool
	// allowOnly, when non-empty, mimics federation=specified.
	allowOnly map[string]bool
	// suspended mimics instance.suspensionState != none. **受信では効かない** —
	// 本物の `ShouldSkipDelivery` だけがこれを見る。
	suspended map[string]bool
}

func (b *fakePeerBlocker) IsBlocked(host string) bool { return b.blocked[host] }
func (b *fakePeerBlocker) IsAllowed(host string) bool {
	if len(b.allowOnly) == 0 {
		return true
	}
	return b.allowOnly[host]
}

// ShouldSkipDelivery mirrors instance.Service: blockedHosts / federation モード
// に加えて suspensionState も見る。
func (b *fakePeerBlocker) ShouldSkipDelivery(host string) bool {
	return b.blocked[host] || !b.IsAllowed(host) || b.suspended[host]
}

type fakePeerLister struct {
	forgotten map[string]int
	byHost    map[string][]string
	err       error
}

func (l *fakePeerLister) Plugins(_ context.Context, host string) ([]string, error) {
	if l.err != nil {
		return nil, l.err
	}
	return l.byHost[host], nil
}

// forgotten records Forget calls so tests can assert the cache is dropped.
func (l *fakePeerLister) Forget(host string) {
	if l.forgotten == nil {
		l.forgotten = map[string]int{}
	}
	l.forgotten[host]++
}

func testIDGenerator(t *testing.T) id.Generator {
	t.Helper()
	g, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return g
}

func testPeer(t *testing.T, deps *pluginPeerDeps) *pluginPeer {
	t.Helper()
	if deps.idGen == nil {
		deps.idGen = testIDGenerator(t)
	}
	if deps.selfHost == "" {
		deps.selfHost = "self.example"
	}
	if deps.blocker == nil {
		// **本番は fail-closed** (配線が落ちたらブロックリストが無効になるより
		// 全部止める方を選ぶ)。テストの既定は「何もブロックしない」にして、
		// ブロック判定そのものを試すテストだけが明示的に差し替える。
		deps.blocker = allowAllPeerBlocker{}
	}
	return &pluginPeer{name: "demo", peered: true, deps: deps, logger: testLogger(), maxBody: peerDefaultMaxBody}
}

// --- Send のガード ---

// 宣言していないプラグインは経路そのものを使えない。
func TestPluginPeer_RequiresDeclaration(t *testing.T) {
	p := &pluginPeer{name: "demo", peered: false, deps: &pluginPeerDeps{}, logger: testLogger(), maxBody: peerDefaultMaxBody}

	_, err := p.Send(context.Background(), "other.example", map[string]any{})
	assert.ErrorIs(t, err, errNotPeered)

	_, err = p.Has(context.Background(), "other.example")
	assert.ErrorIs(t, err, errNotPeered)
}

// **送る前に落とす。** ブロックしている相手には接続そのものをしない。
func TestPluginPeer_SendRefusesBlockedAndSelf(t *testing.T) {
	p := testPeer(t, &pluginPeerDeps{
		blocker: &fakePeerBlocker{blocked: map[string]bool{"bad.example": true}},
		remote:  &fakePeerLister{byHost: map[string][]string{"bad.example": {"demo"}, "ok.example": {"demo"}}},
	})

	_, err := p.Send(context.Background(), "bad.example", map[string]any{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ブロック")

	_, err = p.Send(context.Background(), "self.example", map[string]any{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "自分自身")

	_, err = p.Send(context.Background(), "", map[string]any{})
	assert.Error(t, err)
}

// federation=specified で許可していない相手にも送らない。
func TestPluginPeer_SendRespectsAllowlist(t *testing.T) {
	p := testPeer(t, &pluginPeerDeps{
		blocker: &fakePeerBlocker{allowOnly: map[string]bool{"ok.example": true}},
		remote:  &fakePeerLister{byHost: map[string][]string{"other.example": {"demo"}}},
	})

	_, err := p.Send(context.Background(), "other.example", map[string]any{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ブロック")
}

// 相手が同じプラグインを持たなければ送らない (無関係なインスタンスに
// こちらの都合でリクエストを飛ばさない)。
func TestPluginPeer_SendRequiresSamePlugin(t *testing.T) {
	p := testPeer(t, &pluginPeerDeps{
		remote: &fakePeerLister{byHost: map[string][]string{"other.example": {"another"}}},
	})

	_, err := p.Send(context.Background(), "other.example", map[string]any{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "持っていません")
}

func TestPluginPeer_Has(t *testing.T) {
	p := testPeer(t, &pluginPeerDeps{
		blocker: &fakePeerBlocker{blocked: map[string]bool{"bad.example": true}},
		remote: &fakePeerLister{byHost: map[string][]string{
			"ok.example":  {"demo", "other"},
			"bad.example": {"demo"},
		}},
	})
	ctx := context.Background()

	got, err := p.Has(ctx, "ok.example")
	require.NoError(t, err)
	assert.True(t, got)

	got, err = p.Has(ctx, "none.example")
	require.NoError(t, err)
	assert.False(t, got, "宣言していない相手")

	got, err = p.Has(ctx, "bad.example")
	require.NoError(t, err)
	assert.False(t, got, "ブロックしている相手は持っていても false")

	got, err = p.Has(ctx, "self.example")
	require.NoError(t, err)
	assert.False(t, got, "自分自身")
}

// ホストの表記ゆれを吸収する。URL で渡されても動くこと。
func TestNormalizePeerHost(t *testing.T) {
	for in, want := range map[string]string{
		"Example.COM":          "example.com",
		"https://example.com":  "example.com",
		"https://example.com/": "example.com",
		"  example.com  ":      "example.com",
		"example.com:3000":     "example.com:3000",
		"xn--r8jz45g.example":  "xn--r8jz45g.example", // IDN は punycode で来る
	} {
		assert.Equal(t, want, normalizePeerHost(in), in)
	}
}

// ホスト名の形をしていない値は空にする。
//
// **`@` が要点。** 素通しすると peerURL / nodeinfo の URL に連結されて userinfo
// として解釈され、宣言した宛先とは別のホストへ署名付きのリクエストが飛ぶ。
func TestNormalizePeerHost_RejectsNonHost(t *testing.T) {
	for _, in := range []string{
		"",
		"example.test@10.0.0.1",
		"example.test@evil.example",
		"example.test/../../evil",
		"example.test?x=1",
		"example.test#frag",
		"example.test/path",
		"example.test:80/x",
		"exa mple.test",
		"example.test:notaport",
		"-example.test",
		"example.test.",
		"[::1]",
		"日本語.example", // punycode 化されていない値は受けない
	} {
		assert.Equal(t, "", normalizePeerHost(in), in)
	}
}

// --- 受信 ---

func peerRequest(t *testing.T, body []byte, sign func(*http.Request)) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	// 絶対 URL で作る。署名は Host を対象に含むので、実際の送信と同じ形に
	// しないと検証できない。
	req := httptest.NewRequest(http.MethodPost, "https://self.example/api/plugin/demo/_peer",
		strings.NewReader(string(body)))
	req.Host = "self.example"
	if sign != nil {
		sign(req)
	}
	rec := httptest.NewRecorder()
	return echo.New().NewContext(req, rec), rec
}

// 署名が無ければ受け付けない。**名乗りは信じない。**
func TestPluginPeer_ServeRejectsUnsigned(t *testing.T) {
	p := testPeer(t, &pluginPeerDeps{keyCache: activitypub.NewPublicKeyCache(4)})
	p.Handle(func(context.Context, string, json.RawMessage) (any, error) {
		t.Fatal("署名を検証せずにハンドラを呼んだ")
		return nil, nil
	})

	body, _ := json.Marshal(peerEnvelope{ID: "x", Payload: json.RawMessage(`{}`)})
	c, rec := peerRequest(t, body, nil)
	require.NoError(t, p.echoHandler()(c))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// 上限を超える本文は読み切らずに落とす。
func TestPluginPeer_ServeRejectsOversizedBody(t *testing.T) {
	p := testPeer(t, &pluginPeerDeps{keyCache: activitypub.NewPublicKeyCache(4)})
	p.Handle(func(context.Context, string, json.RawMessage) (any, error) { return nil, nil })

	c, rec := peerRequest(t, []byte(strings.Repeat("a", int(peerDefaultMaxBody)+10)), nil)
	require.NoError(t, p.echoHandler()(c))
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

// 署名が通っても、ハンドラのエラー文面は相手に返さない
// (プラグインの内部事情が他インスタンスに漏れる)。
func TestPluginPeer_ServeHidesHandlerError(t *testing.T) {
	key, pem := testPeerKeypair(t)
	p := testPeer(t, &pluginPeerDeps{
		keyCache: activitypub.NewPublicKeyCache(4),
		resolver: &fakePeerResolver{host: "sender.example", pem: pem},
	})
	p.Handle(func(context.Context, string, json.RawMessage) (any, error) {
		return nil, assertAnError{}
	})

	body, _ := json.Marshal(peerEnvelope{ID: "x", Payload: json.RawMessage(`{"a":1}`)})
	c, rec := peerRequest(t, body, func(r *http.Request) { signPeerRequest(t, r, key, body) })
	require.NoError(t, p.echoHandler()(c))

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "内部の詳細")
}

// 署名が通れば、送信元は **鍵の持ち主** として渡る。
func TestPluginPeer_ServePassesVerifiedSender(t *testing.T) {
	key, pem := testPeerKeypair(t)
	p := testPeer(t, &pluginPeerDeps{
		keyCache: activitypub.NewPublicKeyCache(4),
		resolver: &fakePeerResolver{host: "sender.example", pem: pem},
	})

	var gotFrom string
	var gotPayload json.RawMessage
	p.Handle(func(_ context.Context, from string, payload json.RawMessage) (any, error) {
		gotFrom, gotPayload = from, payload
		return map[string]any{"ok": true}, nil
	})

	body, _ := json.Marshal(peerEnvelope{ID: "x", Payload: json.RawMessage(`{"a":1}`)})
	c, rec := peerRequest(t, body, func(r *http.Request) { signPeerRequest(t, r, key, body) })
	require.NoError(t, p.echoHandler()(c))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "sender.example", gotFrom)
	assert.JSONEq(t, `{"a":1}`, string(gotPayload))
	assert.JSONEq(t, `{"ok":true}`, rec.Body.String())
}

// ブロックしている相手からは受け取らない。
func TestPluginPeer_ServeRejectsBlockedSender(t *testing.T) {
	key, pem := testPeerKeypair(t)
	p := testPeer(t, &pluginPeerDeps{
		keyCache: activitypub.NewPublicKeyCache(4),
		resolver: &fakePeerResolver{host: "sender.example", pem: pem},
		blocker:  &fakePeerBlocker{blocked: map[string]bool{"sender.example": true}},
	})
	p.Handle(func(context.Context, string, json.RawMessage) (any, error) {
		t.Fatal("ブロックした相手のハンドラを呼んだ")
		return nil, nil
	})

	body, _ := json.Marshal(peerEnvelope{ID: "x", Payload: json.RawMessage(`{}`)})
	c, rec := peerRequest(t, body, func(r *http.Request) { signPeerRequest(t, r, key, body) })
	require.NoError(t, p.echoHandler()(c))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// ハンドラ未登録は「こちらが受けられない」と伝える (相手のせいではない)。
func TestPluginPeer_ServeWithoutHandler(t *testing.T) {
	key, pem := testPeerKeypair(t)
	p := testPeer(t, &pluginPeerDeps{
		keyCache: activitypub.NewPublicKeyCache(4),
		resolver: &fakePeerResolver{host: "sender.example", pem: pem},
	})

	body, _ := json.Marshal(peerEnvelope{ID: "x", Payload: json.RawMessage(`{}`)})
	c, rec := peerRequest(t, body, func(r *http.Request) { signPeerRequest(t, r, key, body) })
	require.NoError(t, p.echoHandler()(c))
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

// --- 予約パス ---

// プラグインに受け口を奪わせない。
func TestReservedPluginPath(t *testing.T) {
	assert.True(t, reservedPluginPath("/_peer"))
	assert.True(t, reservedPluginPath("/_anything"))
	assert.False(t, reservedPluginPath("/peer"))
	assert.False(t, reservedPluginPath("/me"))
}

func TestPluginRouter_RefusesReservedPath(t *testing.T) {
	r := &pluginRouter{group: echo.New().Group("/api/plugin/demo")}

	r.POST("/_peer", func(plugin.Request) (any, error) { return nil, nil })
	require.Error(t, r.err, "予約パスの登録は失敗させる")
	assert.Contains(t, r.err.Error(), "予約")

	r2 := &pluginRouter{group: echo.New().Group("/api/plugin/demo")}
	r2.GET("/_x", func(plugin.Request) (any, error) { return nil, nil })
	assert.Error(t, r2.err)
}

type assertAnError struct{}

func (assertAnError) Error() string { return "内部の詳細" }

// testLogger keeps test output quiet while still exercising the log paths.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testPeerKeypair returns a signing key and the matching public PEM.
func testPeerKeypair(t *testing.T) (*activitypub.PrivateKey, string) {
	t.Helper()
	privPEM, pubPEM, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://sender.example/users/u1#main-key", privPEM)
	require.NoError(t, err)
	return key, pubPEM
}

// signPeerRequest signs req the same way the sender does.
func signPeerRequest(t *testing.T, req *http.Request, key *activitypub.PrivateKey, body []byte) {
	t.Helper()
	digest := activitypub.SHA256Digest(body)
	require.NoError(t, activitypub.SignRequest(req, key, digest,
		[]string{"(request-target)", "date", "host", "digest"}))
}

// 送信先の URL を実際に確かめる。
//
// **`/api` を落とすと SPA catchall に落ちて 405 が返る。** 「相手が受け取らない」
// という形で出るので、送信側のログだけ見ても原因が分かりにくい。実際にこれで
// 相互に入れても何も出ない不具合を踏んだ。
func TestPluginPeer_DeliverURLAndRoundTrip(t *testing.T) {
	var gotPath, gotSig, gotDigest string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotSig = r.Header.Get("Signature")
		gotDigest = r.Header.Get("Digest")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"score":7}`))
	}))
	defer srv.Close()

	key, _ := testPeerKeypair(t)
	p := testPeer(t, &pluginPeerDeps{
		client: srv.Client(),
		signer: &fakePeerSigner{key: key},
		// httptest は http なので、既定の https 決め打ちでは繋がらない。
		// **パスは実装のものをそのまま使う** — ここを差し替えると URL の
		// 検証にならない。
		urlFor: func(_, plugin string) string {
			return srv.URL + peerAPIPrefix + plugin + peerPath
		},
	})

	replies := make(chan json.RawMessage, 1)
	p.OnReply(func(_ context.Context, _, _ string, reply json.RawMessage) error {
		replies <- reply
		return nil
	})

	// deliverOnce は Send のガードを通さず直接叩く (宛先の検査は別テスト)。
	envelope, err := json.Marshal(peerEnvelope{ID: "id1", Payload: json.RawMessage(`{"user":"alice"}`)})
	require.NoError(t, err)
	require.NoError(t, p.deliverOnce(peerJob{Host: "other.example", SendID: "id1", Envelope: envelope}))

	assert.Equal(t, "/api/plugin/demo/_peer", gotPath, "/api を含む正しいパスへ送る")
	assert.NotEmpty(t, gotSig, "署名を付ける")
	assert.NotEmpty(t, gotDigest, "Digest を付ける")
	assert.JSONEq(t, `{"id":"id1","payload":{"user":"alice"}}`, string(gotBody))

	select {
	case reply := <-replies:
		assert.JSONEq(t, `{"score":7}`, string(reply))
	default:
		t.Fatal("OnReply が呼ばれていない")
	}
}

type fakePeerSigner struct{ key *activitypub.PrivateKey }

func (s *fakePeerSigner) Signer() (*activitypub.PrivateKey, error) { return s.key, nil }

// **停止したインスタンスへ送らない (#3037)。** AP の deliver は
// `ShouldSkipDelivery` を通すので `instance.suspensionState` が効くが、peer は
// 受信側と同じ `IsBlocked || !IsAllowed` を使っていたため、運営者が相手を停止
// しても peer だけ署名付きで飛び続けていた。
func TestPluginPeer_SendSkipsSuspendedInstance(t *testing.T) {
	blocker := &fakePeerBlocker{suspended: map[string]bool{"dead.example": true}}
	p := testPeer(t, &pluginPeerDeps{
		blocker: blocker,
		remote:  &fakePeerLister{byHost: map[string][]string{"dead.example": {"demo"}}},
	})

	_, err := p.Send(context.Background(), "dead.example", map[string]any{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ブロック")

	got, err := p.Has(context.Background(), "dead.example")
	require.NoError(t, err)
	assert.False(t, got, "停止した相手には nodeinfo も引きに行かない")
}

// dispatch 時にも見る。積んでから飛ぶまでの間に停止されたジョブを止める。
func TestPluginPeer_DeliverOnceSkipsSuspendedInstance(t *testing.T) {
	p := testPeer(t, &pluginPeerDeps{
		blocker: &fakePeerBlocker{suspended: map[string]bool{"dead.example": true}},
		// **client は渡さない。** ガードが効いていなければ nil client で
		// panic するので、素通りが「たまたま成功」に化けない。
		urlFor: func(host, plugin string) string { return "https://" + host + "/x" },
	})

	err := p.deliverOnce(peerJob{Host: "dead.example", SendID: "id1", Envelope: []byte(`{}`)})
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry, "恒久的な失敗なので再試行させない")
}

// **受信は停止状態を見ない。** AP の inbox も見ないので揃える。相手を停止して
// いても、向こうから届いたものは (ブロックしていない限り) 受ける。
func TestPluginPeer_InboundIgnoresSuspension(t *testing.T) {
	p := testPeer(t, &pluginPeerDeps{
		blocker: &fakePeerBlocker{suspended: map[string]bool{"dead.example": true}},
	})

	assert.False(t, p.blocked("dead.example"), "受信側で停止状態を見てしまっている")
	assert.True(t, p.skipDelivery("dead.example"), "送信側で停止状態を見ていない")
}

// **既定ポートは剥がす。** `HostMatchesAny` は `"." + host` の suffix 一致なので、
// `blocked.example:443` は `blocked.example` というブロック指定に当たらない。
// 剥がさないと、同じ authority の別綴りを 1 つ足すだけでブロックをすり抜けられる。
func TestNormalizePeerHost_StripsDefaultPort(t *testing.T) {
	for in, want := range map[string]string{
		"blocked.example:443":         "blocked.example",
		"https://blocked.example:443": "blocked.example",
		"BLOCKED.example:443":         "blocked.example",
		// 非既定ポートは別ホストのまま残す (upstream も同じ)。
		"blocked.example:8443": "blocked.example:8443",
		"blocked.example:80":   "blocked.example:80",
		"blocked.example":      "blocked.example",
	} {
		assert.Equal(t, want, normalizePeerHost(in), in)
	}
}

// 正規化が効いていることを、ブロック判定まで通して見る。
//
// **`normalizePeerHost` の単体テストだけでは足りない** — 呼び忘れた経路が
// あれば同じ穴が残る。
func TestPluginPeer_SendRefusesBlockedHostWithDefaultPort(t *testing.T) {
	p := testPeer(t, &pluginPeerDeps{
		blocker: &fakePeerBlocker{blocked: map[string]bool{"bad.example": true}},
		remote:  &fakePeerLister{byHost: map[string][]string{"bad.example": {"demo"}}},
	})

	_, err := p.Send(context.Background(), "bad.example:443", map[string]any{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ブロック")

	got, err := p.Has(context.Background(), "bad.example:443")
	require.NoError(t, err)
	assert.False(t, got)
}

// **`url` に既定ポートを明記した構成でも「自分自身」を弾く (#3037)。**
//
// `config.Host` は `parsedURL.Host` の生の authority なので `:443` が残る。
// `normalizePeerHost` は既定ポートを剥がすようになったので、片側だけ正規化
// すると判定が外れ、**自分の `/_peer` へ署名付きで POST する**。
func TestPluginPeer_SelfHostWithDefaultPort(t *testing.T) {
	p := testPeer(t, &pluginPeerDeps{
		selfHost: "self.example:443",
		remote:   &fakePeerLister{byHost: map[string][]string{"self.example": {"demo"}}},
	})

	for _, target := range []string{"self.example", "self.example:443", "https://self.example:443"} {
		_, err := p.Send(context.Background(), target, map[string]any{})
		require.Error(t, err, "target=%s", target)
		assert.Contains(t, err.Error(), "自分自身", "target=%s", target)
	}

	// **他所は通ったまま。** これが無いと「常に自分自身と判定する」実装でも
	// 上のテストが通る。
	got, err := p.Has(context.Background(), "other.example")
	require.NoError(t, err)
	assert.False(t, got, "宣言していない相手")
}

// allowAllPeerBlocker is the test default: nothing is blocked.
type allowAllPeerBlocker struct{}

func (allowAllPeerBlocker) IsBlocked(string) bool          { return false }
func (allowAllPeerBlocker) IsAllowed(string) bool          { return true }
func (allowAllPeerBlocker) ShouldSkipDelivery(string) bool { return false }

// **blocker が未配線なら通さないこと (fail-closed)。**
//
// ここは「相手がブロック対象でないこと」を確かめる判定なので、判定できないこと
// を理由に通すとブロックリストが黙って無効になる。起動時のゲート
// (`peer.blocker`) と二重にしてある。
func TestPluginPeer_BlockerMissingIsFailClosed(t *testing.T) {
	p := &pluginPeer{name: "demo", peered: true, logger: testLogger(),
		deps: &pluginPeerDeps{selfHost: "self.example", idGen: testIDGenerator(t)}}

	require.True(t, p.blocked("remote.example"),
		"受信側: 判定できないときは通さないこと")
	require.True(t, p.skipDelivery("remote.example"),
		"送信側: 判定できないときは送らないこと")
}

// 配線の有無を起動時ゲートが見られること。
func TestPluginPeerDeps_HasBlocker(t *testing.T) {
	require.False(t, (&pluginPeerDeps{}).HasBlocker())
	require.False(t, (*pluginPeerDeps)(nil).HasBlocker())
	require.True(t, (&pluginPeerDeps{blocker: allowAllPeerBlocker{}}).HasBlocker())
}
