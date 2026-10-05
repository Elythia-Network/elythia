package processors_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/elythia-network/elythia/internal/repository"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Test doubles ---

type stubHTTPClient struct {
	mu      sync.Mutex
	reqs    []*http.Request
	body    []byte
	resp    *http.Response
	respErr error
	status  int
}

func (s *stubHTTPClient) Do(req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// reqはbodyを消費してしまうので、先に保存しておく
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		s.body = b
	}
	s.reqs = append(s.reqs, req)
	if s.respErr != nil {
		return nil, s.respErr
	}
	status := s.status
	if status == 0 {
		status = http.StatusOK
	}
	if s.resp != nil {
		return s.resp, nil
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(bytes.NewReader(nil)),
	}, nil
}

type stubUserWebhookRepo struct {
	hooks        map[string]*model.Webhook
	findErr      error
	statusCalled map[string]int
}

func (r *stubUserWebhookRepo) Create(_ *model.Webhook) error { return nil }
func (r *stubUserWebhookRepo) FindByID(id string) (*model.Webhook, error) {
	if r.findErr != nil {
		return nil, r.findErr
	}
	if w, ok := r.hooks[id]; ok {
		return w, nil
	}
	// **実 repository と同じ sentinel を返す。** `errors.New("not found")` だと
	// `repository.IsNotFound` で判定できず、not-found と DB 障害を分ける実装を
	// テストできない。
	return nil, repository.ErrNotFound
}
func (r *stubUserWebhookRepo) FindByIDAndUserID(_, _ string) (*model.Webhook, error) {
	return nil, nil
}
func (r *stubUserWebhookRepo) ListByUserID(_ string) ([]*model.Webhook, error) { return nil, nil }
func (r *stubUserWebhookRepo) CountByUserID(_ string) (int64, error)           { return 0, nil }
func (r *stubUserWebhookRepo) ListActiveByUserID(_ string) ([]*model.Webhook, error) {
	return nil, nil
}
func (r *stubUserWebhookRepo) Update(_ *model.Webhook) error { return nil }
func (r *stubUserWebhookRepo) UpdateLatestStatus(id string, _ time.Time, status int) error {
	if r.statusCalled == nil {
		r.statusCalled = make(map[string]int)
	}
	r.statusCalled[id] = status
	return nil
}
func (r *stubUserWebhookRepo) Delete(_, _ string) error { return nil }

type stubSystemWebhookRepo struct {
	hooks        map[string]*model.SystemWebhook
	statusCalled map[string]int
}

func (r *stubSystemWebhookRepo) Create(_ *model.SystemWebhook) error { return nil }
func (r *stubSystemWebhookRepo) FindByID(id string) (*model.SystemWebhook, error) {
	if w, ok := r.hooks[id]; ok {
		return w, nil
	}
	return nil, repository.ErrNotFound
}
func (r *stubSystemWebhookRepo) List() ([]*model.SystemWebhook, error)       { return nil, nil }
func (r *stubSystemWebhookRepo) ListActive() ([]*model.SystemWebhook, error) { return nil, nil }
func (r *stubSystemWebhookRepo) Update(_ *model.SystemWebhook) error         { return nil }
func (r *stubSystemWebhookRepo) UpdateAdminFields(_ string, _ map[string]any) error {
	return nil
}
func (r *stubSystemWebhookRepo) UpdateLatestStatus(id string, _ time.Time, status int) error {
	if r.statusCalled == nil {
		r.statusCalled = make(map[string]int)
	}
	r.statusCalled[id] = status
	return nil
}
func (r *stubSystemWebhookRepo) Delete(_ string) error { return nil }

// --- helpers ---

func newTestWebhookProcessor(
	t *testing.T,
	client processors.WebhookHTTPClient,
	userHooks map[string]*model.Webhook,
	sysHooks map[string]*model.SystemWebhook,
) (*processors.WebhookProcessor, *stubUserWebhookRepo, *stubSystemWebhookRepo) {
	t.Helper()
	ur := &stubUserWebhookRepo{hooks: userHooks}
	sr := &stubSystemWebhookRepo{hooks: sysHooks}
	p := processors.NewWebhookProcessor(ur, sr, client, "example.com")
	return p, ur, sr
}

// --- Tests ---

func TestWebhookProcessor_User_Success(t *testing.T) {
	client := &stubHTTPClient{status: http.StatusOK}
	p, repo, _ := newTestWebhookProcessor(t, client, map[string]*model.Webhook{
		"h1": {ID: "h1", URL: "https://hook.example/u1", Secret: "s"},
	}, nil)

	task := queue.NewUserWebhookTask(queue.WebhookPayload{
		WebhookID: "h1",
		UserID:    "alice",
		EventType: "note",
		Body:      []byte(`{"type":"note"}`),
	})
	err := p.HandleUser(context.Background(), task)
	require.NoError(t, err)
	require.Len(t, client.reqs, 1)
	req := client.reqs[0]
	assert.Equal(t, "https://hook.example/u1", req.URL.String())
	assert.Equal(t, "application/json", req.Header.Get("Content-Type"))
	assert.Equal(t, "Misskey-Hooks", req.Header.Get("User-Agent"))
	assert.Equal(t, "example.com", req.Header.Get("X-Misskey-Host"))
	assert.Equal(t, "h1", req.Header.Get("X-Misskey-Hook-Id"))
	assert.Equal(t, "s", req.Header.Get("X-Misskey-Hook-Secret"))
	assert.Equal(t, http.StatusOK, repo.statusCalled["h1"])
}

// #1546: OverrideURL/Secret 指定時は保存済 webhook でなく override 先へ送り、
// 保存済 webhook の latestStatus も記録しない (test 送信は別 URL のため)。
func TestWebhookProcessor_User_Override(t *testing.T) {
	client := &stubHTTPClient{status: http.StatusOK}
	p, repo, _ := newTestWebhookProcessor(t, client, map[string]*model.Webhook{
		"h1": {ID: "h1", URL: "https://saved.example/u1", Secret: "saved"},
	}, nil)
	task := queue.NewUserWebhookTask(queue.WebhookPayload{
		WebhookID: "h1", UserID: "alice", EventType: "note", Body: []byte(`{}`),
		OverrideURL: "https://override.example/hook", OverrideSecret: "ovr",
	})
	require.NoError(t, p.HandleUser(context.Background(), task))
	require.Len(t, client.reqs, 1)
	assert.Equal(t, "https://override.example/hook", client.reqs[0].URL.String())
	assert.Equal(t, "ovr", client.reqs[0].Header.Get("X-Misskey-Hook-Secret"))
	_, recorded := repo.statusCalled["h1"]
	assert.False(t, recorded, "override test send must not record status on the saved webhook")
}

// #2106 L62: secret 空でも upstream は X-Misskey-Hook-Secret ヘッダーを (空値で) 送る。
func TestWebhookProcessor_User_EmptySecretSendsEmptyHeader(t *testing.T) {
	client := &stubHTTPClient{}
	p, _, _ := newTestWebhookProcessor(t, client, map[string]*model.Webhook{
		"h1": {ID: "h1", URL: "https://hook.example/u1"},
	}, nil)
	task := queue.NewUserWebhookTask(queue.WebhookPayload{WebhookID: "h1", EventType: "note", Body: []byte(`{}`)})
	require.NoError(t, p.HandleUser(context.Background(), task))
	require.Len(t, client.reqs, 1)
	// ヘッダー自体は存在し (omit しない)、値は空。
	_, present := client.reqs[0].Header["X-Misskey-Hook-Secret"]
	assert.True(t, present, "secret 空でも header は送出される")
	assert.Empty(t, client.reqs[0].Header.Get("X-Misskey-Hook-Secret"))
}

func TestWebhookProcessor_User_4xxSkipsRetry(t *testing.T) {
	client := &stubHTTPClient{status: http.StatusBadRequest}
	p, repo, _ := newTestWebhookProcessor(t, client, map[string]*model.Webhook{
		"h1": {ID: "h1", URL: "https://hook.example/u1"},
	}, nil)
	task := queue.NewUserWebhookTask(queue.WebhookPayload{WebhookID: "h1", EventType: "note", Body: []byte(`{}`)})
	err := p.HandleUser(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
	assert.Equal(t, http.StatusBadRequest, repo.statusCalled["h1"])
}

func TestWebhookProcessor_User_5xxRetries(t *testing.T) {
	client := &stubHTTPClient{status: http.StatusInternalServerError}
	p, repo, _ := newTestWebhookProcessor(t, client, map[string]*model.Webhook{
		"h1": {ID: "h1", URL: "https://hook.example/u1"},
	}, nil)
	task := queue.NewUserWebhookTask(queue.WebhookPayload{WebhookID: "h1", EventType: "note", Body: []byte(`{}`)})
	err := p.HandleUser(context.Background(), task)
	require.Error(t, err)
	assert.NotErrorIs(t, err, driver.ErrSkipRetry)
	assert.Equal(t, http.StatusInternalServerError, repo.statusCalled["h1"])
}

func TestWebhookProcessor_User_NetworkError(t *testing.T) {
	client := &stubHTTPClient{respErr: errors.New("connection refused")}
	p, repo, _ := newTestWebhookProcessor(t, client, map[string]*model.Webhook{
		"h1": {ID: "h1", URL: "https://hook.example/u1"},
	}, nil)
	task := queue.NewUserWebhookTask(queue.WebhookPayload{WebhookID: "h1", EventType: "note", Body: []byte(`{}`)})
	err := p.HandleUser(context.Background(), task)
	require.Error(t, err)
	assert.NotErrorIs(t, err, driver.ErrSkipRetry)
	// #2106 L60: ネットワーク障害 (非 StatusError) は upstream 同様 status=1 で記録する。
	assert.Equal(t, 1, repo.statusCalled["h1"])
}

func TestWebhookProcessor_User_NotFoundSkipsRetry(t *testing.T) {
	client := &stubHTTPClient{}
	p, _, _ := newTestWebhookProcessor(t, client, map[string]*model.Webhook{}, nil)
	task := queue.NewUserWebhookTask(queue.WebhookPayload{WebhookID: "h1", EventType: "note", Body: []byte(`{}`)})
	err := p.HandleUser(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestWebhookProcessor_User_InvalidPayloadSkipsRetry(t *testing.T) {
	p, _, _ := newTestWebhookProcessor(t, &stubHTTPClient{}, nil, nil)
	task := driver.RawTask{TypeName: queue.TaskTypeUserWebhook, Body: []byte("not json")}
	err := p.HandleUser(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestWebhookProcessor_User_MissingWebhookID(t *testing.T) {
	p, _, _ := newTestWebhookProcessor(t, &stubHTTPClient{}, nil, nil)
	task := queue.NewUserWebhookTask(queue.WebhookPayload{})
	err := p.HandleUser(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestWebhookProcessor_System_Success(t *testing.T) {
	client := &stubHTTPClient{status: http.StatusAccepted}
	p, _, sysRepo := newTestWebhookProcessor(t, client, nil, map[string]*model.SystemWebhook{
		"sh1": {ID: "sh1", URL: "https://hook.example/s1", Secret: "ss"},
	})
	task := queue.NewSystemWebhookTask(queue.WebhookPayload{WebhookID: "sh1", EventType: "userCreated", Body: []byte(`{}`)})
	err := p.HandleSystem(context.Background(), task)
	require.NoError(t, err)
	require.Len(t, client.reqs, 1)
	assert.Equal(t, "sh1", client.reqs[0].Header.Get("X-Misskey-Hook-Id"))
	assert.Equal(t, http.StatusAccepted, sysRepo.statusCalled["sh1"])
}

func TestWebhookProcessor_UserRepoMissing(t *testing.T) {
	p := processors.NewWebhookProcessor(nil, nil, &stubHTTPClient{}, "example.com")
	task := queue.NewUserWebhookTask(queue.WebhookPayload{WebhookID: "h1", EventType: "note", Body: []byte(`{}`)})
	err := p.HandleUser(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestWebhookProcessor_SystemRepoMissing(t *testing.T) {
	p := processors.NewWebhookProcessor(nil, nil, &stubHTTPClient{}, "example.com")
	task := queue.NewSystemWebhookTask(queue.WebhookPayload{WebhookID: "sh1", EventType: "userCreated", Body: []byte(`{}`)})
	err := p.HandleSystem(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestNewWebhookProcessor_DefaultClient(t *testing.T) {
	// nil client falls back to http.Client with timeout
	p := processors.NewWebhookProcessor(nil, nil, nil, "example.com")
	assert.NotNil(t, p)
}

func TestWebhookProcessor_BodyForwardedUnchanged(t *testing.T) {
	client := &stubHTTPClient{}
	p, _, _ := newTestWebhookProcessor(t, client, map[string]*model.Webhook{
		"h1": {ID: "h1", URL: "https://hook.example/u1"},
	}, nil)
	body := []byte(`{"hello":"world"}`)
	task := queue.NewUserWebhookTask(queue.WebhookPayload{WebhookID: "h1", EventType: "note", Body: body})
	require.NoError(t, p.HandleUser(context.Background(), task))
	assert.Equal(t, body, client.body)
}

// #2106 N30: webhook 先が 429 を返したら retryable (ErrSkipRetry を付けない)。
func TestWebhookProcessor_User_429Retries(t *testing.T) {
	client := &stubHTTPClient{status: http.StatusTooManyRequests}
	p, repo, _ := newTestWebhookProcessor(t, client, map[string]*model.Webhook{
		"h1": {ID: "h1", URL: "https://hook.example/u1"},
	}, nil)
	task := queue.NewUserWebhookTask(queue.WebhookPayload{WebhookID: "h1", EventType: "note", Body: []byte(`{}`)})
	err := p.HandleUser(context.Background(), task)
	require.Error(t, err)
	assert.NotErrorIs(t, err, driver.ErrSkipRetry)
	assert.Equal(t, http.StatusTooManyRequests, repo.statusCalled["h1"])
}

// 本文の HMAC を足すこと (mk-go の追加)。
//
// upstream は共有秘密を平文ヘッダに載せるだけなので、受信側は「本文が改ざん
// されていないか」を確かめられない。**受信側が自分で計算して一致を見られる**
// 形にする。
func TestWebhookProcessor_SignsBody(t *testing.T) {
	client := &stubHTTPClient{status: http.StatusOK}
	p, _, _ := newTestWebhookProcessor(t, client, map[string]*model.Webhook{
		"h1": {ID: "h1", URL: "https://hook.example/u1", Secret: "s3cr3t"},
	}, nil)

	body := []byte(`{"type":"note","id":"abc"}`)
	require.NoError(t, p.HandleUser(context.Background(), queue.NewUserWebhookTask(
		queue.WebhookPayload{WebhookID: "h1", EventType: "note", Body: body})))
	require.Len(t, client.reqs, 1)

	mac := hmac.New(sha256.New, []byte("s3cr3t"))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	assert.Equal(t, want, client.reqs[0].Header.Get("X-Hub-Signature-256"))
}

// 本文が 1 byte 変わったら署名も変わること。**固定値を返す実装では上の
// テストだけ通ってしまう。**
func TestWebhookProcessor_SignatureCoversBody(t *testing.T) {
	sigFor := func(t *testing.T, body []byte) string {
		t.Helper()
		client := &stubHTTPClient{status: http.StatusOK}
		p, _, _ := newTestWebhookProcessor(t, client, map[string]*model.Webhook{
			"h1": {ID: "h1", URL: "https://hook.example/u1", Secret: "s3cr3t"},
		}, nil)
		require.NoError(t, p.HandleUser(context.Background(), queue.NewUserWebhookTask(
			queue.WebhookPayload{WebhookID: "h1", EventType: "note", Body: body})))
		return client.reqs[0].Header.Get("X-Hub-Signature-256")
	}
	assert.NotEqual(t, sigFor(t, []byte(`{"a":1}`)), sigFor(t, []byte(`{"a":2}`)))
}

// 秘密が空なら署名しない。**空鍵の HMAC は誰でも作れる**ので、付けると
// 「検証できている」という誤解だけを生む。
func TestWebhookProcessor_NoSignatureWithoutSecret(t *testing.T) {
	client := &stubHTTPClient{status: http.StatusOK}
	p, _, _ := newTestWebhookProcessor(t, client, map[string]*model.Webhook{
		"h1": {ID: "h1", URL: "https://hook.example/u1"},
	}, nil)
	require.NoError(t, p.HandleUser(context.Background(), queue.NewUserWebhookTask(
		queue.WebhookPayload{WebhookID: "h1", EventType: "note", Body: []byte(`{}`)})))
	require.Len(t, client.reqs, 1)
	_, present := client.reqs[0].Header["X-Hub-Signature-256"]
	assert.False(t, present, "秘密が無いのに署名ヘッダを送っている")
}

// **DB 障害を恒久 failed に潰さないこと (#2792)。**
//
// 種別を見ずに `ErrSkipRetry` へ倒すと、DB が詰まった瞬間に配送待ちだった webhook が
// 1 回で恒久 failed になる (本来は 4 回 + backoff)。
func TestWebhookProcessor_DBFailureIsRetryable(t *testing.T) {
	p := processors.NewWebhookProcessor(&failingWebhookRepo{err: errors.New("db down")}, nil, &stubHTTPClient{}, "example.com")

	body, err := json.Marshal(queue.WebhookPayload{WebhookID: "w1", EventType: "test", Body: []byte(`{}`)})
	require.NoError(t, err)
	err = p.HandleUser(context.Background(), driver.RawTask{TypeName: queue.TaskTypeUserWebhook, Body: body})
	require.Error(t, err)
	require.False(t, errors.Is(err, driver.ErrSkipRetry),
		"DB 障害は retry させること (not-found だけ ErrSkipRetry)")
}

// not-found は従来どおり恒久失敗にすること (毒ジョブを回し続けない)。
func TestWebhookProcessor_NotFoundSkipsRetry(t *testing.T) {
	p := processors.NewWebhookProcessor(&failingWebhookRepo{err: repository.ErrNotFound}, nil, &stubHTTPClient{}, "example.com")

	body, err := json.Marshal(queue.WebhookPayload{WebhookID: "w1", EventType: "test", Body: []byte(`{}`)})
	require.NoError(t, err)
	err = p.HandleUser(context.Background(), driver.RawTask{TypeName: queue.TaskTypeUserWebhook, Body: body})
	require.Error(t, err)
	require.True(t, errors.Is(err, driver.ErrSkipRetry))
}

type failingWebhookRepo struct {
	repository.WebhookRepository
	err error
}

func (f *failingWebhookRepo) FindByID(string) (*model.Webhook, error) { return nil, f.err }

// countingBody counts how many bytes the processor reads from the response.
type countingBody struct {
	remaining int64
	read      int64
	closed    bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > b.remaining {
		n = b.remaining
	}
	b.remaining -= n
	b.read += n
	return int(n), nil
}

func (b *countingBody) Close() error { b.closed = true; return nil }

// 応答の本文は上限までしか読まずに閉じる (#3263)。本家は本文を読まない。
func TestWebhookProcessor_DrainsBoundedResponseBody(t *testing.T) {
	body := &countingBody{remaining: 10 << 20}
	client := &stubHTTPClient{resp: &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: body}}
	p, _, _ := newTestWebhookProcessor(t, client, nil, map[string]*model.SystemWebhook{
		"s1": {ID: "s1", URL: "https://hook.example/s1"},
	})
	task := queue.NewSystemWebhookTask(queue.WebhookPayload{WebhookID: "s1", EventType: "abuseReport", Body: []byte(`{}`)})
	require.NoError(t, p.HandleSystem(context.Background(), task))
	assert.True(t, body.closed)
	assert.LessOrEqual(t, body.read, int64(64<<10), "上限を超えて読まない")
	assert.Positive(t, body.read, "接続を使い回すため、上限までは読み捨てる")
}

// 本家の webhook 配送は HttpRequestService.send の既定 5 秒 (#3263)。
func TestDefaultWebhookTimeout_MatchesUpstream(t *testing.T) {
	assert.Equal(t, 5*time.Second, processors.DefaultWebhookTimeout)
}
