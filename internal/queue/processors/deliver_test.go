package processors_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubSigner records the call and returns a canned response or error.
type stubSigner struct {
	resp     *http.Response
	err      error
	gotURL   string
	gotBody  []byte
	gotKeyID string
}

func (s *stubSigner) PostSigned(url string, body []byte, key *activitypub.PrivateKey) (*http.Response, error) {
	s.gotURL = url
	s.gotBody = body
	if key != nil {
		s.gotKeyID = key.KeyID
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.resp, nil
}

// generateTestKey returns a freshly minted PEM-encoded RSA private key for
// the processor to parse via activitypub.NewPrivateKey.
func generateTestKey(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)
	der := x509.MarshalPKCS1PrivateKey(priv)
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}
	return string(pem.EncodeToMemory(block))
}

func makeTask(t *testing.T, payload queue.DeliverPayload) driver.Task {
	t.Helper()
	return queue.NewDeliverTask(payload)
}

func makePayload(t *testing.T) queue.DeliverPayload {
	t.Helper()
	return queue.DeliverPayload{
		Inbox:  "https://remote.example/users/alice/inbox",
		Body:   []byte(`{"type":"Create"}`),
		KeyID:  "https://example.com/users/u1#main-key",
		KeyPEM: generateTestKey(t),
	}
}

func okResponse(status int) *http.Response {
	return &http.Response{
		Status:     http.StatusText(status),
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader("")),
	}
}

func TestDeliverProcessor_Success(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusOK)}
	p := processors.NewDeliverProcessor(signer)

	payload := makePayload(t)
	err := p.Handle(context.Background(), makeTask(t, payload))
	require.NoError(t, err)
	assert.Equal(t, payload.Inbox, signer.gotURL)
	assert.Equal(t, payload.Body, signer.gotBody)
	assert.Equal(t, payload.KeyID, signer.gotKeyID)
}

func TestDeliverProcessor_Accepted(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusAccepted)}
	p := processors.NewDeliverProcessor(signer)
	require.NoError(t, p.Handle(context.Background(), makeTask(t, makePayload(t))))
}

func TestDeliverProcessor_Gone_SkipsRetry(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusGone)}
	p := processors.NewDeliverProcessor(signer)
	err := p.Handle(context.Background(), makeTask(t, makePayload(t)))
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestDeliverProcessor_NotFound_SkipsRetry(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusNotFound)}
	p := processors.NewDeliverProcessor(signer)
	err := p.Handle(context.Background(), makeTask(t, makePayload(t)))
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestDeliverProcessor_Forbidden_SkipsRetry(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusForbidden)}
	p := processors.NewDeliverProcessor(signer)
	err := p.Handle(context.Background(), makeTask(t, makePayload(t)))
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestDeliverProcessor_ServerError_RetriesByReturningError(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusInternalServerError)}
	p := processors.NewDeliverProcessor(signer)
	err := p.Handle(context.Background(), makeTask(t, makePayload(t)))
	require.Error(t, err)
	assert.NotErrorIs(t, err, driver.ErrSkipRetry)
}

func TestDeliverProcessor_NetworkError_Retries(t *testing.T) {
	netErr := errors.New("connection refused")
	signer := &stubSigner{err: netErr}
	p := processors.NewDeliverProcessor(signer)
	err := p.Handle(context.Background(), makeTask(t, makePayload(t)))
	require.Error(t, err)
	assert.ErrorIs(t, err, netErr)
	assert.NotErrorIs(t, err, driver.ErrSkipRetry)
}

func TestDeliverProcessor_BadPayload_SkipsRetry(t *testing.T) {
	p := processors.NewDeliverProcessor(&stubSigner{})
	task := driver.RawTask{TypeName: queue.TaskTypeDeliver, Body: []byte(`{not json`)}
	err := p.Handle(context.Background(), task)
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

func TestDeliverProcessor_BadKey_SkipsRetry(t *testing.T) {
	p := processors.NewDeliverProcessor(&stubSigner{})
	payload := queue.DeliverPayload{
		Inbox:  "https://remote.example/inbox",
		Body:   []byte(`{}`),
		KeyID:  "k",
		KeyPEM: "not a pem",
	}
	err := p.Handle(context.Background(), makeTask(t, payload))
	require.Error(t, err)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
}

// --- Ed25519 sign / capability gate / degrade safeguard (#1067 / #1071) ---

// recordingSigner records every PostSigned call so multi-call test (Ed25519
// 4xx → RSA retry) can assert which keyId was used in each attempt.
type recordingSigner struct {
	responses []*http.Response // pop in order
	errs      []error
	calls     []string // recorded KeyID for each call
}

func (r *recordingSigner) PostSigned(_ string, _ []byte, key *activitypub.PrivateKey) (*http.Response, error) {
	if key != nil {
		r.calls = append(r.calls, key.KeyID)
	}
	if len(r.responses) == 0 && len(r.errs) == 0 {
		return okResponse(http.StatusOK), nil
	}
	if len(r.errs) > 0 && r.errs[0] != nil {
		err := r.errs[0]
		r.errs = r.errs[1:]
		if len(r.responses) > 0 {
			r.responses = r.responses[1:]
		}
		return nil, err
	}
	resp := r.responses[0]
	r.responses = r.responses[1:]
	if len(r.errs) > 0 {
		r.errs = r.errs[1:]
	}
	return resp, nil
}

// generateTestEd25519Key returns a freshly minted PEM-encoded Ed25519 (PKCS8)
// private key for the processor to parse via activitypub.NewEd25519PrivateKey.
func generateTestEd25519Key(t *testing.T) string {
	t.Helper()
	priv, _, err := activitypub.GenerateEd25519Keypair()
	require.NoError(t, err)
	return priv
}

// payload に Ed25519 鍵情報がある場合、processor は Ed25519 で sign する。
func TestDeliverProcessor_Ed25519_PreferredWhenPayloadHasKey(t *testing.T) {
	signer := &recordingSigner{responses: []*http.Response{okResponse(http.StatusOK)}}
	p := processors.NewDeliverProcessor(signer)

	payload := makePayload(t)
	payload.Ed25519KeyID = "https://example.com/users/u1#ed25519-key"
	payload.Ed25519PrivPEM = generateTestEd25519Key(t)

	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	require.Len(t, signer.calls, 1)
	assert.Equal(t, payload.Ed25519KeyID, signer.calls[0], "Ed25519 key was used for signing")
}

// payload に Ed25519 鍵情報がない場合は従来通り RSA で sign される。
func TestDeliverProcessor_Ed25519_AbsentFallbackToRSA(t *testing.T) {
	signer := &recordingSigner{responses: []*http.Response{okResponse(http.StatusOK)}}
	p := processors.NewDeliverProcessor(signer)

	payload := makePayload(t) // Ed25519KeyID / Ed25519PrivPEM は空
	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	require.Len(t, signer.calls, 1)
	assert.Equal(t, payload.KeyID, signer.calls[0], "RSA key was used")
}

// Ed25519 PEM が壊れていれば RSA に fallback する (fail-soft)。
func TestDeliverProcessor_Ed25519_MalformedKeyFallsBackToRSA(t *testing.T) {
	signer := &recordingSigner{responses: []*http.Response{okResponse(http.StatusOK)}}
	p := processors.NewDeliverProcessor(signer)

	payload := makePayload(t)
	payload.Ed25519KeyID = "https://example.com/users/u1#ed25519-key"
	payload.Ed25519PrivPEM = "NOT A VALID PEM"

	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	require.Len(t, signer.calls, 1)
	assert.Equal(t, payload.KeyID, signer.calls[0])
}

// Ed25519 sign で 4xx が返ったら fail counter を INCR し、3 件未満なら
// degrade flag は立てずに RSA で再送する (= 1 件 false positive を防ぐ)。
func TestDeliverProcessor_Ed25519_4xxIncrementsCounterButNoDegrade(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	signer := &recordingSigner{
		responses: []*http.Response{
			okResponse(http.StatusBadRequest),
			okResponse(http.StatusOK),
		},
	}
	p := processors.NewDeliverProcessor(signer)
	p.SetRedis(rdb)

	payload := makePayload(t)
	payload.Ed25519KeyID = "https://example.com/users/u1#ed25519-key"
	payload.Ed25519PrivPEM = generateTestEd25519Key(t)

	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	require.Len(t, signer.calls, 2, "Ed25519 4xx → RSA retry の 2 段送信")
	assert.Equal(t, payload.Ed25519KeyID, signer.calls[0])
	assert.Equal(t, payload.KeyID, signer.calls[1])

	// fail counter は 1、degrade flag は立たない (= threshold=3 で trip するため)
	cnt, err := rdb.Get(context.Background(), "ed25519:fail:remote.example").Result()
	require.NoError(t, err)
	assert.Equal(t, "1", cnt)
	degraded, err := rdb.Exists(context.Background(), "ed25519:degrade:remote.example").Result()
	require.NoError(t, err)
	assert.EqualValues(t, 0, degraded, "1 件失敗では degrade は立たない")
}

// 3 件連続 Ed25519 4xx で fail counter が threshold (3) に達したら degrade
// flag が立つ。以後 5min は同 host への deliver が即 RSA に縮退する。
func TestDeliverProcessor_Ed25519_3xFailureTripsDegrade(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	// 3 件分の Ed25519 4xx + 3 件分の RSA retry 200 を用意
	signer := &recordingSigner{
		responses: []*http.Response{
			okResponse(http.StatusBadRequest), okResponse(http.StatusOK),
			okResponse(http.StatusBadRequest), okResponse(http.StatusOK),
			okResponse(http.StatusBadRequest), okResponse(http.StatusOK),
		},
	}
	p := processors.NewDeliverProcessor(signer)
	p.SetRedis(rdb)

	payload := makePayload(t)
	payload.Ed25519KeyID = "https://example.com/users/u1#ed25519-key"
	payload.Ed25519PrivPEM = generateTestEd25519Key(t)

	for i := 0; i < 3; i++ {
		require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	}

	// counter が threshold に達して degrade flag が立つ
	degraded, err := rdb.Exists(context.Background(), "ed25519:degrade:remote.example").Result()
	require.NoError(t, err)
	assert.EqualValues(t, 1, degraded, "3 件失敗で degrade 確定")
}

// degrade flag が立っている host への deliver は最初から RSA で送られる。
func TestDeliverProcessor_Ed25519_DegradeFlagSkipsEd25519(t *testing.T) {
	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	defer rdb.Close()

	// 事前に degrade flag を立てる
	require.NoError(t, rdb.Set(context.Background(), "ed25519:degrade:remote.example", "1", 0).Err())

	signer := &recordingSigner{responses: []*http.Response{okResponse(http.StatusOK)}}
	p := processors.NewDeliverProcessor(signer)
	p.SetRedis(rdb)

	payload := makePayload(t)
	payload.Ed25519KeyID = "https://example.com/users/u1#ed25519-key"
	payload.Ed25519PrivPEM = generateTestEd25519Key(t)

	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	require.Len(t, signer.calls, 1)
	assert.Equal(t, payload.KeyID, signer.calls[0], "degrade flag により最初から RSA で送信")
}

// stubResponseHook captures host events for assertions.
type stubResponseHook struct {
	successes []string
	errors    []string
	goneHosts []string
}

func (s *stubResponseHook) RecordResponseSuccess(host string) error {
	s.successes = append(s.successes, host)
	return nil
}

func (s *stubResponseHook) RecordResponseError(host string) error {
	s.errors = append(s.errors, host)
	return nil
}

func (s *stubResponseHook) MarkGoneSuspended(host string) error {
	s.goneHosts = append(s.goneHosts, host)
	return nil
}

func TestDeliverProcessor_ResponseHook_Success(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusOK)}
	p := processors.NewDeliverProcessor(signer)
	hook := &stubResponseHook{}
	p.SetResponseHook(hook)
	require.NoError(t, p.Handle(context.Background(), makeTask(t, makePayload(t))))
	assert.Equal(t, []string{"remote.example"}, hook.successes)
	assert.Empty(t, hook.errors)
}

func TestDeliverProcessor_ResponseHook_Gone_RecordsSuccess(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusGone)}
	p := processors.NewDeliverProcessor(signer)
	hook := &stubResponseHook{}
	p.SetResponseHook(hook)
	err := p.Handle(context.Background(), makeTask(t, makePayload(t)))
	require.Error(t, err)
	assert.Equal(t, []string{"remote.example"}, hook.successes)
}

// #1811: shared inbox が 410 Gone を返したらインスタンス全体を goneSuspended にする。
func TestDeliverProcessor_SharedInbox_Gone_SuspendsInstance(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusGone)}
	p := processors.NewDeliverProcessor(signer)
	hook := &stubResponseHook{}
	p.SetResponseHook(hook)
	payload := makePayload(t)
	payload.IsSharedInbox = true
	err := p.Handle(context.Background(), makeTask(t, payload))
	require.Error(t, err)
	assert.Equal(t, []string{"remote.example"}, hook.goneHosts, "shared inbox 410 -> goneSuspended")
}

// 個別 inbox の 410 はインスタンスを suspend しない (個別 actor 不在の可能性)。
func TestDeliverProcessor_PersonalInbox_Gone_NoSuspend(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusGone)}
	p := processors.NewDeliverProcessor(signer)
	hook := &stubResponseHook{}
	p.SetResponseHook(hook)
	payload := makePayload(t) // IsSharedInbox=false
	err := p.Handle(context.Background(), makeTask(t, payload))
	require.Error(t, err)
	assert.Empty(t, hook.goneHosts, "personal inbox 410 must not suspend the instance")
}

// shared inbox でも 404 は suspend しない (410 Gone のみが「消滅」シグナル)。
func TestDeliverProcessor_SharedInbox_NotFound_NoSuspend(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusNotFound)}
	p := processors.NewDeliverProcessor(signer)
	hook := &stubResponseHook{}
	p.SetResponseHook(hook)
	payload := makePayload(t)
	payload.IsSharedInbox = true
	err := p.Handle(context.Background(), makeTask(t, payload))
	require.Error(t, err)
	assert.Empty(t, hook.goneHosts, "404 must not suspend (only 410 Gone)")
}

func TestDeliverProcessor_ResponseHook_ClientError_RecordsSuccess(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusForbidden)}
	p := processors.NewDeliverProcessor(signer)
	hook := &stubResponseHook{}
	p.SetResponseHook(hook)
	err := p.Handle(context.Background(), makeTask(t, makePayload(t)))
	require.Error(t, err)
	assert.Equal(t, []string{"remote.example"}, hook.successes)
}

func TestDeliverProcessor_ResponseHook_ServerError_RecordsError(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusInternalServerError)}
	p := processors.NewDeliverProcessor(signer)
	hook := &stubResponseHook{}
	p.SetResponseHook(hook)
	err := p.Handle(context.Background(), makeTask(t, makePayload(t)))
	require.Error(t, err)
	assert.Equal(t, []string{"remote.example"}, hook.errors)
}

func TestDeliverProcessor_ResponseHook_NetworkError_RecordsError(t *testing.T) {
	signer := &stubSigner{err: errors.New("net down")}
	p := processors.NewDeliverProcessor(signer)
	hook := &stubResponseHook{}
	p.SetResponseHook(hook)
	err := p.Handle(context.Background(), makeTask(t, makePayload(t)))
	require.Error(t, err)
	assert.Equal(t, []string{"remote.example"}, hook.errors)
}

func TestDeliverProcessor_ResponseHook_UnparseableInboxSuccess(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusOK)}
	p := processors.NewDeliverProcessor(signer)
	hook := &stubResponseHook{}
	p.SetResponseHook(hook)
	payload := queue.DeliverPayload{
		Inbox:  "://bad-url",
		Body:   []byte(`{}`),
		KeyID:  "https://example.com/users/u1#main-key",
		KeyPEM: generateTestKey(t),
	}
	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	assert.Empty(t, hook.successes)
}

func TestDeliverProcessor_ResponseHook_UnparseableInboxError(t *testing.T) {
	signer := &stubSigner{err: errors.New("net down")}
	p := processors.NewDeliverProcessor(signer)
	hook := &stubResponseHook{}
	p.SetResponseHook(hook)
	payload := queue.DeliverPayload{
		Inbox:  "://bad-url",
		Body:   []byte(`{}`),
		KeyID:  "https://example.com/users/u1#main-key",
		KeyPEM: generateTestKey(t),
	}
	err := p.Handle(context.Background(), makeTask(t, payload))
	require.Error(t, err)
	assert.Empty(t, hook.errors)
}

// stubChartHook captures OnDelivered events from DeliverProcessor.
type stubChartHook struct {
	delivered []struct {
		host      string
		succeeded bool
	}
}

func (s *stubChartHook) OnDelivered(host string, succeeded bool) {
	s.delivered = append(s.delivered, struct {
		host      string
		succeeded bool
	}{host, succeeded})
}

func TestDeliverProcessor_ChartHook_Success(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusOK)}
	p := processors.NewDeliverProcessor(signer)
	hook := &stubChartHook{}
	p.SetChartHook(hook)
	require.NoError(t, p.Handle(context.Background(), makeTask(t, makePayload(t))))
	require.Len(t, hook.delivered, 1)
	assert.Equal(t, "remote.example", hook.delivered[0].host)
	assert.True(t, hook.delivered[0].succeeded)
}

func TestDeliverProcessor_ChartHook_NetworkError(t *testing.T) {
	signer := &stubSigner{err: errors.New("net down")}
	p := processors.NewDeliverProcessor(signer)
	hook := &stubChartHook{}
	p.SetChartHook(hook)
	err := p.Handle(context.Background(), makeTask(t, makePayload(t)))
	require.Error(t, err)
	require.Len(t, hook.delivered, 1)
	assert.False(t, hook.delivered[0].succeeded)
}

func TestDeliverProcessor_ChartHook_UnparseableInbox(t *testing.T) {
	// 不正な inbox URL でも chart hook は host="" で発火し、route 別に
	// no-op で扱われる (charthook 側のテストでカバー)。
	signer := &stubSigner{resp: okResponse(http.StatusOK)}
	p := processors.NewDeliverProcessor(signer)
	hook := &stubChartHook{}
	p.SetChartHook(hook)
	payload := queue.DeliverPayload{
		Inbox:  "://bad-url",
		Body:   []byte(`{}`),
		KeyID:  "https://example.com/users/u1#main-key",
		KeyPEM: generateTestKey(t),
	}
	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	require.Len(t, hook.delivered, 1)
	assert.Equal(t, "", hook.delivered[0].host)
}

// keyCapturingSigner records the *PrivateKey passed to each PostSigned so a
// test can assert pointer identity (= parsed-key cache hit).
type keyCapturingSigner struct {
	resp *http.Response
	keys []*activitypub.PrivateKey
}

func (s *keyCapturingSigner) PostSigned(_ string, _ []byte, key *activitypub.PrivateKey) (*http.Response, error) {
	s.keys = append(s.keys, key)
	return s.resp, nil
}

// #1425: 同一署名者へ fan-out する deliver job は、PEM のパース結果が job 横断で
// メモ化され同じ *PrivateKey を再利用すること。PEM が変われば (鍵 rotation 相当)
// 別 entry で再パースされること。
func TestDeliverProcessor_RSAKeyCacheReuse(t *testing.T) {
	signer := &keyCapturingSigner{resp: okResponse(http.StatusOK)}
	p := processors.NewDeliverProcessor(signer)

	payload := makePayload(t) // KeyID + KeyPEM 固定
	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	require.Len(t, signer.keys, 2)
	assert.Same(t, signer.keys[0], signer.keys[1], "同一 PEM は cache hit で同じ *PrivateKey を再利用する")

	// 同じ KeyID でも PEM が変われば (= rotation) 別 entry で再パースし、別 instance。
	rotated := makePayload(t) // generateTestKey は毎回新規 RSA 鍵
	require.NoError(t, p.Handle(context.Background(), makeTask(t, rotated)))
	require.Len(t, signer.keys, 3)
	assert.NotSame(t, signer.keys[1], signer.keys[2], "別 PEM は別 entry でパースし直す")
}

// Ed25519 経路も同様に parsed-key がメモ化されること (#1425)。
func TestDeliverProcessor_Ed25519KeyCacheReuse(t *testing.T) {
	signer := &keyCapturingSigner{resp: okResponse(http.StatusOK)}
	p := processors.NewDeliverProcessor(signer)

	payload := makePayload(t)
	payload.Ed25519KeyID = "https://example.com/users/u1#ed25519-key"
	payload.Ed25519PrivPEM = generateTestEd25519Key(t)

	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	require.Len(t, signer.keys, 2)
	assert.Same(t, signer.keys[0], signer.keys[1], "同一 Ed25519 PEM は cache hit で再利用する")
}

// #2106 N30: 429 (rate limited) は 4xx だが upstream 同様 retryable (ErrSkipRetry を付けない)。
func TestDeliverProcessor_RateLimited_Retries(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusTooManyRequests)}
	p := processors.NewDeliverProcessor(signer)
	err := p.Handle(context.Background(), makeTask(t, makePayload(t)))
	require.Error(t, err)
	assert.NotErrorIs(t, err, driver.ErrSkipRetry)
}

// keySourceStub resolves keys the way the production repo-backed source does.
type keySourceStub struct {
	rsa string
	ed  string
}

func (s *keySourceStub) SigningKeyPEM(_ string, kind string) (string, error) {
	if kind == "ed" {
		if s.ed == "" {
			return "", processors.ErrSigningKeyMissing
		}
		return s.ed, nil
	}
	if s.rsa == "" {
		return "", processors.ErrSigningKeyMissing
	}
	return s.rsa, nil
}

// **新しい形の payload (鍵を載せず SignerUserID だけ) で end-to-end に署名できること。**
//
// 署名鍵は `admin/queue/jobs` から読めてしまうため payload に載せなくなった。
// worker が DB から引いて署名するところまで通らないと**配送が全滅する**が、
// それは相手サーバー側でしか分からない。Ed25519 / RSA の両方を固定する。
func TestDeliverProcessor_SignsWithoutKeyInPayload_Ed25519(t *testing.T) {
	signer := &recordingSigner{responses: []*http.Response{okResponse(http.StatusOK)}}
	p := processors.NewDeliverProcessor(signer)
	p.SetSigningKeySource(&keySourceStub{
		rsa: generateTestKey(t),
		ed:  generateTestEd25519Key(t),
	})

	payload := makePayload(t)
	payload.KeyPEM = ""
	payload.Ed25519PrivPEM = ""
	payload.Ed25519KeyID = "https://example.com/users/u1#ed25519-key"
	payload.SignerUserID = "u1"

	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	require.Len(t, signer.calls, 1)
	assert.Equalf(t, payload.Ed25519KeyID, signer.calls[0],
		"鍵を載せない payload で Ed25519 署名が発行されていない (判定を PrivPEM に戻すとこうなる)")
}

func TestDeliverProcessor_SignsWithoutKeyInPayload_RSA(t *testing.T) {
	signer := &recordingSigner{responses: []*http.Response{okResponse(http.StatusOK)}}
	p := processors.NewDeliverProcessor(signer)
	p.SetSigningKeySource(&keySourceStub{rsa: generateTestKey(t)})

	payload := makePayload(t)
	payload.KeyPEM = ""
	payload.SignerUserID = "u1"

	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	require.Len(t, signer.calls, 1)
	assert.Equal(t, payload.KeyID, signer.calls[0])
}

// Ed25519 の鍵が引けないときは RSA へ落ちる (配送を止めない)。
func TestDeliverProcessor_FallsBackToRSAWhenEd25519KeyMissing(t *testing.T) {
	signer := &recordingSigner{responses: []*http.Response{okResponse(http.StatusOK)}}
	p := processors.NewDeliverProcessor(signer)
	p.SetSigningKeySource(&keySourceStub{rsa: generateTestKey(t)}) // ed は無し

	payload := makePayload(t)
	payload.KeyPEM = ""
	payload.Ed25519PrivPEM = ""
	payload.Ed25519KeyID = "https://example.com/users/u1#ed25519-key"
	payload.SignerUserID = "u1"

	require.NoError(t, p.Handle(context.Background(), makeTask(t, payload)))
	require.Len(t, signer.calls, 1)
	assert.Equal(t, payload.KeyID, signer.calls[0], "Ed25519 が引けないなら RSA で署名すること")
}
