package processors_test

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	webpushlib "github.com/SherClockHolmes/webpush-go"
	"github.com/shiroha-a/mk/internal/core/webpush"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
	"github.com/shiroha-a/mk/internal/queue/processors"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Test doubles ---

type stubWebPushSender struct {
	mu     sync.Mutex
	calls  []webpushSendCall
	status int
	err    error
	// byEndpoint optionally returns a specific status per endpoint.
	byEndpoint map[string]int
}

type webpushSendCall struct {
	endpoint string
	body     []byte
	options  *webpushlib.Options
}

func (s *stubWebPushSender) Send(_ context.Context, sub *webpushlib.Subscription, message []byte, opts *webpushlib.Options) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, webpushSendCall{
		endpoint: sub.Endpoint,
		body:     append([]byte(nil), message...),
		options:  opts,
	})
	if s.err != nil {
		return nil, s.err
	}
	status := s.status
	if s.byEndpoint != nil {
		if v, ok := s.byEndpoint[sub.Endpoint]; ok {
			status = v
		}
	}
	if status == 0 {
		status = http.StatusCreated
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(bytes.NewReader(nil)),
	}, nil
}

type fakeSwRepoForProcessor struct {
	subs         map[string][]*model.SwSubscription
	deleteCalled []string
}

func (r *fakeSwRepoForProcessor) FindByUserAndEndpoint(_, _ string) (*model.SwSubscription, error) {
	return nil, errors.New("not used")
}
func (r *fakeSwRepoForProcessor) FindByUserEndpointAuthKey(_, _, _, _ string) (*model.SwSubscription, error) {
	return nil, errors.New("not used")
}
func (r *fakeSwRepoForProcessor) FindByUserID(userID string) ([]*model.SwSubscription, error) {
	return r.subs[userID], nil
}
func (r *fakeSwRepoForProcessor) Create(_ *model.SwSubscription) error { return nil }
func (r *fakeSwRepoForProcessor) Update(_ *model.SwSubscription) error { return nil }
func (r *fakeSwRepoForProcessor) DeleteByEndpoint(_ string) error      { return nil }
func (r *fakeSwRepoForProcessor) DeleteByUserAndEndpoint(userID, endpoint string) error {
	r.deleteCalled = append(r.deleteCalled, userID+":"+endpoint)
	// also remove from in-memory map so cache invalidation + refetch works.
	list := r.subs[userID]
	out := list[:0]
	for _, s := range list {
		if s.Endpoint != endpoint {
			out = append(out, s)
		}
	}
	r.subs[userID] = out
	return nil
}

// --- Helpers ---

func newProcessor(
	t *testing.T,
	sender processors.Sender,
	meta *model.Meta,
	subs map[string][]*model.SwSubscription,
) (*processors.WebPushProcessor, *fakeSwRepoForProcessor) {
	t.Helper()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = meta
	repo := &fakeSwRepoForProcessor{subs: subs}
	cache := webpush.NewSubscriptionCache(repo, nil)
	p := processors.NewWebPushProcessor(cache, repo, metaRepo, sender, "https://example.com")
	return p, repo
}

func metaWithVAPID() *model.Meta {
	pub := "BKaVb6hgdOsmOkLG1nHWyX9kYRZqQHbjRpwbhTrYNbGQ" // not actually validated by stub
	priv := "priv"
	return &model.Meta{EnableServiceWorker: true, SwPublicKey: &pub, SwPrivateKey: &priv}
}

func taskFromPayload(t *testing.T, p queue.WebPushPayload) driver.Task {
	t.Helper()
	return queue.NewWebPushTask(p)
}

// --- Tests ---

func TestWebPushProcessor_Success(t *testing.T) {
	sender := &stubWebPushSender{status: http.StatusCreated}
	subs := map[string][]*model.SwSubscription{
		"u1": {
			{ID: "s1", UserID: "u1", Endpoint: "https://push.example/1", Auth: "a", PublicKey: "p"},
		},
	}
	p, _ := newProcessor(t, sender, metaWithVAPID(), subs)

	body, _ := json.Marshal(map[string]any{"type": "mention"})
	payload := queue.WebPushPayload{
		UserID: "u1",
		Type:   webpush.TypeNotification,
		Body:   body,
	}
	err := p.Handle(context.Background(), taskFromPayload(t, payload))
	require.NoError(t, err)
	assert.Len(t, sender.calls, 1)

	// Check the wire format: {type, body, userId, dateTime}
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(sender.calls[0].body, &envelope))
	assert.Equal(t, "notification", envelope["type"])
	assert.Equal(t, "u1", envelope["userId"])
	assert.NotNil(t, envelope["dateTime"])
	assert.NotNil(t, envelope["body"])
}

func TestWebPushProcessor_GoneDeletesSubscription(t *testing.T) {
	sender := &stubWebPushSender{status: http.StatusGone}
	subs := map[string][]*model.SwSubscription{
		"u1": {
			{ID: "s1", UserID: "u1", Endpoint: "https://push.example/1", Auth: "a", PublicKey: "p"},
		},
	}
	p, repo := newProcessor(t, sender, metaWithVAPID(), subs)

	payload := queue.WebPushPayload{UserID: "u1", Type: webpush.TypeReadAllNotifications}
	// sendReadMessage=false by default — readAllNotifications はスキップされるので
	// ここでは notification タイプで 410 を検証する
	payload.Type = webpush.TypeNotification
	payload.Body, _ = json.Marshal(map[string]any{"type": "follow"})

	err := p.Handle(context.Background(), taskFromPayload(t, payload))
	require.NoError(t, err)
	assert.Equal(t, []string{"u1:https://push.example/1"}, repo.deleteCalled)
}

func TestWebPushProcessor_MultipleSubscribers_PartialGone(t *testing.T) {
	sender := &stubWebPushSender{
		byEndpoint: map[string]int{
			"https://push.example/1": http.StatusCreated,
			"https://push.example/2": http.StatusGone,
		},
	}
	subs := map[string][]*model.SwSubscription{
		"u1": {
			{ID: "s1", UserID: "u1", Endpoint: "https://push.example/1", Auth: "a", PublicKey: "p"},
			{ID: "s2", UserID: "u1", Endpoint: "https://push.example/2", Auth: "a", PublicKey: "p"},
		},
	}
	p, repo := newProcessor(t, sender, metaWithVAPID(), subs)

	payload := queue.WebPushPayload{
		UserID: "u1",
		Type:   webpush.TypeNotification,
		Body:   json.RawMessage(`{"type":"mention"}`),
	}
	err := p.Handle(context.Background(), taskFromPayload(t, payload))
	require.NoError(t, err)
	assert.Len(t, sender.calls, 2)
	assert.Equal(t, []string{"u1:https://push.example/2"}, repo.deleteCalled)
}

func TestWebPushProcessor_NoSubscribersIsNoop(t *testing.T) {
	sender := &stubWebPushSender{}
	p, _ := newProcessor(t, sender, metaWithVAPID(), map[string][]*model.SwSubscription{})

	payload := queue.WebPushPayload{UserID: "u1", Type: webpush.TypeNotification}
	err := p.Handle(context.Background(), taskFromPayload(t, payload))
	require.NoError(t, err)
	assert.Empty(t, sender.calls)
}

func TestWebPushProcessor_ServiceWorkerDisabled(t *testing.T) {
	sender := &stubWebPushSender{}
	meta := &model.Meta{EnableServiceWorker: false}
	subs := map[string][]*model.SwSubscription{
		"u1": {{UserID: "u1", Endpoint: "https://push.example/1"}},
	}
	p, _ := newProcessor(t, sender, meta, subs)

	payload := queue.WebPushPayload{UserID: "u1", Type: webpush.TypeNotification}
	err := p.Handle(context.Background(), taskFromPayload(t, payload))
	require.NoError(t, err)
	assert.Empty(t, sender.calls)
}

func TestWebPushProcessor_MissingVAPIDKeys(t *testing.T) {
	sender := &stubWebPushSender{}
	meta := &model.Meta{EnableServiceWorker: true}
	subs := map[string][]*model.SwSubscription{
		"u1": {{UserID: "u1", Endpoint: "https://push.example/1"}},
	}
	p, _ := newProcessor(t, sender, meta, subs)

	payload := queue.WebPushPayload{UserID: "u1", Type: webpush.TypeNotification}
	err := p.Handle(context.Background(), taskFromPayload(t, payload))
	require.NoError(t, err)
	assert.Empty(t, sender.calls)
}

func TestWebPushProcessor_MetaFetchError(t *testing.T) {
	sender := &stubWebPushSender{}
	metaRepo := testutil.NewMockMetaRepository() // Meta is nil -> error
	repo := &fakeSwRepoForProcessor{subs: map[string][]*model.SwSubscription{}}
	cache := webpush.NewSubscriptionCache(repo, nil)
	p := processors.NewWebPushProcessor(cache, repo, metaRepo, sender, "https://example.com")

	payload := queue.WebPushPayload{UserID: "u1", Type: webpush.TypeNotification}
	err := p.Handle(context.Background(), taskFromPayload(t, payload))
	assert.Error(t, err)
}

func TestWebPushProcessor_ReadAllNotifications_RespectsSendReadMessage(t *testing.T) {
	sender := &stubWebPushSender{}
	subs := map[string][]*model.SwSubscription{
		"u1": {
			{UserID: "u1", Endpoint: "https://push.example/1", SendReadMessage: false},
			{UserID: "u1", Endpoint: "https://push.example/2", SendReadMessage: true},
		},
	}
	p, _ := newProcessor(t, sender, metaWithVAPID(), subs)

	payload := queue.WebPushPayload{UserID: "u1", Type: webpush.TypeReadAllNotifications}
	err := p.Handle(context.Background(), taskFromPayload(t, payload))
	require.NoError(t, err)
	require.Len(t, sender.calls, 1)
	assert.Equal(t, "https://push.example/2", sender.calls[0].endpoint)
}

func TestWebPushProcessor_DeliverErrorLogged(t *testing.T) {
	sender := &stubWebPushSender{err: errors.New("connection refused")}
	subs := map[string][]*model.SwSubscription{
		"u1": {{UserID: "u1", Endpoint: "https://push.example/1"}},
	}
	p, repo := newProcessor(t, sender, metaWithVAPID(), subs)
	payload := queue.WebPushPayload{UserID: "u1", Type: webpush.TypeNotification}
	err := p.Handle(context.Background(), taskFromPayload(t, payload))
	require.NoError(t, err) // errors are swallowed; only 410 triggers deletion
	assert.Empty(t, repo.deleteCalled)
}

func TestWebPushProcessor_ServerError(t *testing.T) {
	sender := &stubWebPushSender{status: http.StatusInternalServerError}
	subs := map[string][]*model.SwSubscription{
		"u1": {{UserID: "u1", Endpoint: "https://push.example/1"}},
	}
	p, repo := newProcessor(t, sender, metaWithVAPID(), subs)
	payload := queue.WebPushPayload{UserID: "u1", Type: webpush.TypeNotification}
	err := p.Handle(context.Background(), taskFromPayload(t, payload))
	require.NoError(t, err)
	assert.Empty(t, repo.deleteCalled)
}

func TestWebPushProcessor_InvalidPayloadSkipsRetry(t *testing.T) {
	p, _ := newProcessor(t, &stubWebPushSender{}, metaWithVAPID(), nil)
	task := driver.RawTask{TypeName: queue.TaskTypeWebPush, Body: []byte("not json")}
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestWebPushProcessor_MissingUserID(t *testing.T) {
	p, _ := newProcessor(t, &stubWebPushSender{}, metaWithVAPID(), nil)
	body, _ := json.Marshal(queue.WebPushPayload{Type: webpush.TypeNotification})
	task := driver.RawTask{TypeName: queue.TaskTypeWebPush, Body: body}
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestNewWebPushProcessor_DefaultSender(t *testing.T) {
	// Passing nil sender should fall back to LibraryWebPushSender.
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = metaWithVAPID()
	repo := &fakeSwRepoForProcessor{subs: map[string][]*model.SwSubscription{}}
	cache := webpush.NewSubscriptionCache(repo, nil)
	p := processors.NewWebPushProcessor(cache, repo, metaRepo, nil, "https://example.com")
	assert.NotNil(t, p)
}

func TestLibraryWebPushSender_Send(t *testing.T) {
	// LibraryWebPushSender is a pass-through to webpush-go. We just verify it
	// does not panic when called with a dummy subscription; the actual HTTP
	// call is expected to fail.
	s := processors.LibraryWebPushSender{}
	_, err := s.Send(context.Background(), &webpushlib.Subscription{
		Endpoint: "https://invalid.localhost.example",
		Keys:     webpushlib.Keys{Auth: "a", P256dh: "p"},
	}, []byte(`{}`), &webpushlib.Options{})
	// 鍵が不正なので必ずエラーになる
	assert.Error(t, err)
}

// captureClient is a no-op webpushlib.HTTPClient used by mutation tests.
// It implements the interface but is never actually called: the mutation
// behavior under test trips before any HTTP I/O is attempted.
type captureClient struct{}

func (c *captureClient) Do(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

// LibraryWebPushSender に sender-level HTTPClient をセットしておくと、
// Send 呼び出しでも caller の Options が mutate されない (sender 側で copy
// してから library に渡す、#638 review)。
//
// 実際の HTTP 呼び出しまで到達させるには有効な subscription 鍵が必要だが、
// このテストでは「opts が破壊されないこと」だけ確認すれば十分なので、
// 鍵不正で encryption 段階で error になる subscription を使う。
func TestLibraryWebPushSender_DoesNotMutateOpts(t *testing.T) {
	s := processors.LibraryWebPushSender{HTTPClient: &captureClient{}}
	opts := &webpushlib.Options{TTL: 30}

	_, _ = s.Send(context.Background(), &webpushlib.Subscription{
		Endpoint: "https://push.example/x",
		Keys:     webpushlib.Keys{Auth: "a", P256dh: "p"},
	}, []byte(`{}`), opts)

	// caller の opts は mutate されないこと (Options.HTTPClient は nil のまま)
	assert.Nil(t, opts.HTTPClient)
}

// 既に opts.HTTPClient がセットされている場合は sender 側で何もしない
// (caller の指定を尊重)。
func TestLibraryWebPushSender_RespectsExistingHTTPClient(t *testing.T) {
	preset := &captureClient{}
	overridden := &captureClient{}
	s := processors.LibraryWebPushSender{HTTPClient: overridden}
	opts := &webpushlib.Options{TTL: 30, HTTPClient: preset}

	_, _ = s.Send(context.Background(), &webpushlib.Subscription{
		Endpoint: "https://push.example/y",
		Keys:     webpushlib.Keys{Auth: "a", P256dh: "p"},
	}, []byte(`{}`), opts)

	// 鍵不正で実 HTTP までは行かないが、caller の HTTPClient ポインタは
	// 引き続き preset を指したまま (sender 側で書き換えない)。
	assert.Same(t, preset, opts.HTTPClient)
}

// 本家 PushNotificationService と同じく、保存済みの購読でも配送先として使えない
// endpoint には送らない。410 のときのような削除もしない (本家も飛ばすだけ)。
func TestWebPushProcessor_SkipsInvalidStoredEndpoint(t *testing.T) {
	sender := &stubWebPushSender{}
	subs := map[string][]*model.SwSubscription{
		"u1": {
			{UserID: "u1", Endpoint: "http://push.example/plain-http"},
			{UserID: "u1", Endpoint: "https://user:pass@push.example/userinfo"},
			{UserID: "u1", Endpoint: "https:push.example/opaque"},
			{UserID: "u1", Endpoint: "https://push.example/ok"},
		},
	}
	p, repo := newProcessor(t, sender, metaWithVAPID(), subs)

	payload := queue.WebPushPayload{UserID: "u1", Type: webpush.TypeNotification}
	require.NoError(t, p.Handle(context.Background(), taskFromPayload(t, payload)))
	require.Len(t, sender.calls, 1)
	assert.Equal(t, "https://push.example/ok", sender.calls[0].endpoint)
	assert.Empty(t, repo.deleteCalled)
}

// validSubscriptionKeys returns client keys webpush-go can encrypt to, so a
// Send call actually reaches the HTTP client.
func validSubscriptionKeys(t *testing.T) webpushlib.Keys {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	require.NoError(t, err)
	auth := make([]byte, 16)
	_, err = rand.Read(auth)
	require.NoError(t, err)
	return webpushlib.Keys{
		P256dh: base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
		Auth:   base64.RawURLEncoding.EncodeToString(auth),
	}
}

// vapidOptions returns Options carrying a freshly generated VAPID key pair.
func vapidOptions(t *testing.T) *webpushlib.Options {
	t.Helper()
	vapidPriv, vapidPub, err := webpushlib.GenerateVAPIDKeys()
	require.NoError(t, err)
	return &webpushlib.Options{
		Subscriber:      "https://example.com",
		VAPIDPublicKey:  vapidPub,
		VAPIDPrivateKey: vapidPriv,
		TTL:             30,
	}
}

// HTTPClient を注入しないときも、宛先を検査しない http.DefaultClient には
// 落ちない。loopback の endpoint へは接続しない。
func TestLibraryWebPushSender_DefaultClientRefusesLoopback(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	s := processors.LibraryWebPushSender{}
	resp, err := s.Send(context.Background(), &webpushlib.Subscription{
		Endpoint: srv.URL + "/push",
		Keys:     validSubscriptionKeys(t),
	}, []byte(`{}`), vapidOptions(t))
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.Zero(t, hits.Load(), "the fallback client must not connect to a loopback endpoint")
}

// countingClient records how many requests it was asked to perform.
type countingClient struct{ n atomic.Int32 }

func (c *countingClient) Do(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return &http.Response{StatusCode: http.StatusCreated, Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

// 注入した HTTPClient がある場合は、既定の client ではなくそれを使う。
func TestLibraryWebPushSender_UsesInjectedClient(t *testing.T) {
	client := &countingClient{}
	s := processors.LibraryWebPushSender{HTTPClient: client}
	resp, err := s.Send(context.Background(), &webpushlib.Subscription{
		Endpoint: "https://push.example/z",
		Keys:     validSubscriptionKeys(t),
	}, []byte(`{}`), vapidOptions(t))
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, int32(1), client.n.Load())
}
