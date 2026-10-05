package processors_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/deliveryhealth"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBreaker records what the processor told it and answers Check with a
// canned decision.
type fakeBreaker struct {
	mu        sync.Mutex
	decision  deliveryhealth.BreakerDecision
	wait      time.Duration
	hasState  bool
	failures  []string
	probes    []string
	released  []string
	token     string
	successes []string
	jobs      []string
	throttles map[string]time.Duration
	// openAfterFailure は RecordFailure の後にブレーカーが開いているか。
	openAfterFailure bool
}

func (f *fakeBreaker) Check(_ context.Context, _, job string) deliveryhealth.CheckResult {
	f.mu.Lock()
	f.jobs = append(f.jobs, job)
	f.mu.Unlock()
	r := deliveryhealth.CheckResult{Decision: f.decision, Delay: f.wait, HasState: f.hasState}
	if f.decision == deliveryhealth.BreakerProbe {
		r.ProbeToken = f.token
	}
	return r
}

func (f *fakeBreaker) RecordFailure(_ context.Context, host, probeToken string) (bool, time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, host)
	f.probes = append(f.probes, probeToken)
	return f.openAfterFailure, 7 * time.Minute
}

func (f *fakeBreaker) ReleaseProbe(_ context.Context, _ string, probeToken string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, probeToken)
}

func (f *fakeBreaker) HoldFor(untilProbe time.Duration) time.Duration { return untilProbe / 7 * 3 }

func (f *fakeBreaker) RecordSuccess(_ context.Context, host string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.successes = append(f.successes, host)
}

func (f *fakeBreaker) Throttle(_ context.Context, host string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.throttles == nil {
		f.throttles = map[string]time.Duration{}
	}
	f.throttles[host] = d
}

// 開いている (または 429 で間隔を空けている) 間は送らず、試行回数を消費しない
// 遅延で返す。
func TestDeliverProcessor_Breaker_HoldsWithoutSending(t *testing.T) {
	signer := &stubSigner{resp: okResponse(http.StatusOK)}
	p := processors.NewDeliverProcessor(signer)
	b := &fakeBreaker{decision: deliveryhealth.BreakerDelay, wait: 42 * time.Second, hasState: true}
	p.SetDeliveryBreaker(b)

	err := p.Handle(context.Background(), makeTask(t, makePayload(t)))
	var delayed *driver.DelayError
	require.ErrorAs(t, err, &delayed)
	assert.Equal(t, 42*time.Second, delayed.Delay)
	assert.NotErrorIs(t, err, driver.ErrSkipRetry, "a held job must not be dropped")
	assert.Empty(t, signer.gotURL, "nothing is sent while held")
	assert.Empty(t, b.failures)
	assert.Empty(t, b.successes)
}

// 起きるたびに同じキーを渡す (429 の予約を覚えておくため)。違うジョブには
// 違うキーを渡す (同じキーだと予約を取り合う)。
func TestDeliverProcessor_Breaker_PassesAStableJobKey(t *testing.T) {
	p := processors.NewDeliverProcessor(&stubSigner{resp: okResponse(http.StatusOK)})
	b := &fakeBreaker{decision: deliveryhealth.BreakerDelay, wait: time.Second}
	p.SetDeliveryBreaker(b)

	a := makePayload(t)
	other := a
	other.Inbox = "https://remote.example/users/bob/inbox"
	_ = p.Handle(context.Background(), makeTask(t, a))
	_ = p.Handle(context.Background(), makeTask(t, a))
	_ = p.Handle(context.Background(), makeTask(t, other))

	require.Len(t, b.jobs, 3)
	assert.NotEmpty(t, b.jobs[0])
	assert.Equal(t, b.jobs[0], b.jobs[1], "the same job keeps its key across wake-ups")
	assert.NotEqual(t, b.jobs[0], b.jobs[2], "different jobs get different keys")
}

// 接続失敗と 5xx だけが相手の不調として数えられる。
func TestDeliverProcessor_Breaker_CountsTransportAndServerErrors(t *testing.T) {
	for name, signer := range map[string]*stubSigner{
		"transport": {err: errors.New("connection refused")},
		"5xx":       {resp: okResponse(http.StatusServiceUnavailable)},
	} {
		t.Run(name, func(t *testing.T) {
			p := processors.NewDeliverProcessor(signer)
			b := &fakeBreaker{}
			p.SetDeliveryBreaker(b)
			_ = p.Handle(context.Background(), makeTask(t, makePayload(t)))
			assert.Equal(t, []string{"remote.example"}, b.failures)
			assert.Empty(t, b.successes)
		})
	}
}

// 開いている間 (または今回の失敗で開いた) の失敗は、試行回数を消費しない遅延で
// 返す。普通のエラーで返すと、溜まったジョブが試行のたびに回数を失い、7 日の
// 自動停止より前に捨てられる。
func TestDeliverProcessor_Breaker_FailureWhileOpenIsHeld(t *testing.T) {
	for name, signer := range map[string]*stubSigner{
		"transport": {err: errors.New("connection refused")},
		"5xx":       {resp: okResponse(http.StatusBadGateway)},
	} {
		t.Run(name, func(t *testing.T) {
			p := processors.NewDeliverProcessor(signer)
			b := &fakeBreaker{openAfterFailure: true}
			p.SetDeliveryBreaker(b)
			err := p.Handle(context.Background(), makeTask(t, makePayload(t)))
			var delayed *driver.DelayError
			require.ErrorAs(t, err, &delayed)
			assert.Equal(t, 3*time.Minute, delayed.Delay, "the wait comes from HoldFor(untilProbe)")
		})
	}
}

// 閉じている間の失敗は今までどおり retry (試行回数を消費する)。
func TestDeliverProcessor_Breaker_FailureWhileClosedRetries(t *testing.T) {
	p := processors.NewDeliverProcessor(&stubSigner{err: errors.New("connection refused")})
	p.SetDeliveryBreaker(&fakeBreaker{})
	err := p.Handle(context.Background(), makeTask(t, makePayload(t)))
	require.Error(t, err)
	var delayed *driver.DelayError
	assert.False(t, errors.As(err, &delayed))
}

// 試行かどうかをブレーカーに伝える (間隔を倍にするのは試行の失敗だけ)。
func TestDeliverProcessor_Breaker_ReportsWhetherItWasTheProbe(t *testing.T) {
	for _, tc := range []struct {
		decision deliveryhealth.BreakerDecision
		token    string
	}{{deliveryhealth.BreakerProbe, "t-1"}, {deliveryhealth.BreakerAllow, ""}} {
		p := processors.NewDeliverProcessor(&stubSigner{resp: okResponse(http.StatusServiceUnavailable)})
		b := &fakeBreaker{decision: tc.decision, token: "t-1"}
		p.SetDeliveryBreaker(b)
		_ = p.Handle(context.Background(), makeTask(t, makePayload(t)))
		assert.Equal(t, []string{tc.token}, b.probes)
	}
}

// HTTP の送信まで至らなかった失敗 (署名鍵の読み込みなど、こちら側の事情) は
// 相手の不調として数えない。数えると、DB の瞬断で全ての配送先が止まる。
func TestDeliverProcessor_Breaker_LocalFailuresAreNotCounted(t *testing.T) {
	payload := makePayload(t)
	payload.KeyPEM = ""
	payload.SignerUserID = "u1"
	p := processors.NewDeliverProcessor(&stubSigner{resp: okResponse(http.StatusOK)})
	p.SetSigningKeySource(failingKeySource{})
	b := &fakeBreaker{openAfterFailure: true}
	p.SetDeliveryBreaker(b)
	err := p.Handle(context.Background(), makeTask(t, payload))
	require.Error(t, err)
	assert.Empty(t, b.failures)
	var delayed *driver.DelayError
	assert.False(t, errors.As(err, &delayed))
}

// 恒久的な鍵のエラー (鍵が無い) も相手の不調ではない。
func TestDeliverProcessor_Breaker_PermanentKeyErrorIsNotCounted(t *testing.T) {
	payload := makePayload(t)
	payload.KeyPEM = ""
	payload.SignerUserID = "u1"
	p := processors.NewDeliverProcessor(&stubSigner{resp: okResponse(http.StatusOK)})
	p.SetSigningKeySource(missingKeySource{})
	b := &fakeBreaker{openAfterFailure: true}
	p.SetDeliveryBreaker(b)
	err := p.Handle(context.Background(), makeTask(t, payload))
	require.ErrorIs(t, err, driver.ErrSkipRetry)
	assert.Empty(t, b.failures)
}

// 試行がこちら側の失敗で終わったら、枠だけ返す (5 分間次の試行ができなくなる)。
func TestDeliverProcessor_Breaker_LocalFailureReleasesTheProbe(t *testing.T) {
	payload := makePayload(t)
	payload.KeyPEM = ""
	payload.SignerUserID = "u1"
	p := processors.NewDeliverProcessor(&stubSigner{resp: okResponse(http.StatusOK)})
	p.SetSigningKeySource(failingKeySource{})
	b := &fakeBreaker{decision: deliveryhealth.BreakerProbe, token: "t-9"}
	p.SetDeliveryBreaker(b)
	_ = p.Handle(context.Background(), makeTask(t, payload))
	assert.Empty(t, b.failures)
	assert.Equal(t, []string{"t-9"}, b.released)
}

// Ed25519 で 4xx → RSA で送り直すときに RSA の鍵が読めない (こちら側の失敗) なら、
// 数えない。
func TestDeliverProcessor_Breaker_RSARetryLocalFailureIsNotCounted(t *testing.T) {
	payload := makePayload(t)
	payload.KeyPEM = ""
	payload.SignerUserID = "u1"
	payload.Ed25519KeyID = "https://example.com/users/u1#ed25519-key"
	payload.Ed25519PrivPEM = generateTestEd25519Key(t)
	p := processors.NewDeliverProcessor(&sequenceSigner{steps: []signerStep{{resp: okResponse(http.StatusUnauthorized)}}})
	p.SetSigningKeySource(failingKeySource{})
	b := &fakeBreaker{openAfterFailure: true}
	p.SetDeliveryBreaker(b)
	err := p.Handle(context.Background(), makeTask(t, payload))
	require.Error(t, err)
	assert.Empty(t, b.failures)
	var delayed *driver.DelayError
	assert.False(t, errors.As(err, &delayed))
}

// Ed25519 で 4xx → RSA で送り直す経路でも、送り直しの接続失敗はブレーカーに数え、
// 開いていれば試行回数を消費しない遅延で返す。
func TestDeliverProcessor_Breaker_RSARetryPathCountsAndHolds(t *testing.T) {
	payload := makePayload(t)
	payload.Ed25519KeyID = "https://example.com/users/u1#ed25519-key"
	payload.Ed25519PrivPEM = generateTestEd25519Key(t)
	signer := &sequenceSigner{steps: []signerStep{
		{resp: okResponse(http.StatusUnauthorized)},
		{err: errors.New("connection reset")},
	}}
	p := processors.NewDeliverProcessor(signer)
	b := &fakeBreaker{openAfterFailure: true}
	p.SetDeliveryBreaker(b)
	err := p.Handle(context.Background(), makeTask(t, payload))
	var delayed *driver.DelayError
	require.ErrorAs(t, err, &delayed)
	assert.Equal(t, []string{"remote.example"}, b.failures)
}

// 応答が返ってきた (4xx / 410 / 429 / 2xx) なら相手は健在なので閉じる側に数える。
// 4xx は送ったものが悪いのであって、止めても直らない。
func TestDeliverProcessor_Breaker_AnyResponseCloses(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusForbidden, http.StatusGone, http.StatusNotFound, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			p := processors.NewDeliverProcessor(&stubSigner{resp: okResponse(status)})
			b := &fakeBreaker{hasState: true}
			p.SetDeliveryBreaker(b)
			_ = p.Handle(context.Background(), makeTask(t, makePayload(t)))
			assert.Empty(t, b.failures)
			assert.Equal(t, []string{"remote.example"}, b.successes)
		})
	}
}

// 状態の無いホストの成功で Redis を叩かない (健全なホストへの配送が大半なので)。
func TestDeliverProcessor_Breaker_HealthyHostSkipsRecordSuccess(t *testing.T) {
	p := processors.NewDeliverProcessor(&stubSigner{resp: okResponse(http.StatusOK)})
	b := &fakeBreaker{}
	p.SetDeliveryBreaker(b)
	require.NoError(t, p.Handle(context.Background(), makeTask(t, makePayload(t))))
	assert.Empty(t, b.successes)
}

// 半開の試行は状態が無く見えても (Check の後で閉じられた等)、結果を必ず返す。
// 返さないと、試行が成功しても閉じない。
func TestDeliverProcessor_Breaker_ProbeAlwaysReports(t *testing.T) {
	p := processors.NewDeliverProcessor(&stubSigner{resp: okResponse(http.StatusOK)})
	b := &fakeBreaker{decision: deliveryhealth.BreakerProbe}
	p.SetDeliveryBreaker(b)
	require.NoError(t, p.Handle(context.Background(), makeTask(t, makePayload(t))))
	assert.Equal(t, []string{"remote.example"}, b.successes)
}

// 429 は止めずに間隔を空ける。Retry-After を読む。このジョブ自体は今までどおり
// retry で戻る (試行回数を消費する)。
func TestDeliverProcessor_Breaker_TooManyRequestsThrottles(t *testing.T) {
	resp := okResponse(http.StatusTooManyRequests)
	resp.Header = http.Header{"Retry-After": []string{"120"}}
	p := processors.NewDeliverProcessor(&stubSigner{resp: resp})
	b := &fakeBreaker{}
	p.SetDeliveryBreaker(b)

	err := p.Handle(context.Background(), makeTask(t, makePayload(t)))
	require.Error(t, err)
	var delayed *driver.DelayError
	assert.False(t, errors.As(err, &delayed), "the 429 job itself retries normally")
	assert.Equal(t, map[string]time.Duration{"remote.example": 120 * time.Second}, b.throttles)
	assert.Empty(t, b.failures, "a 429 does not open the breaker")
}

func TestDeliverProcessor_Breaker_TooManyRequestsWithoutRetryAfter(t *testing.T) {
	p := processors.NewDeliverProcessor(&stubSigner{resp: okResponse(http.StatusTooManyRequests)})
	b := &fakeBreaker{}
	p.SetDeliveryBreaker(b)
	_ = p.Handle(context.Background(), makeTask(t, makePayload(t)))
	assert.Equal(t, map[string]time.Duration{"remote.example": 0}, b.throttles, "0 lets the breaker apply its default")
}

// 止められた / ブロックされたホストへは、ブレーカーより先に配送自体を止める。
func TestDeliverProcessor_Breaker_GateRunsFirst(t *testing.T) {
	p := processors.NewDeliverProcessor(&stubSigner{resp: okResponse(http.StatusOK)})
	p.SetDeliveryGate(skipAllGate{})
	b := &fakeBreaker{decision: deliveryhealth.BreakerDelay, wait: time.Minute}
	p.SetDeliveryBreaker(b)
	require.NoError(t, p.Handle(context.Background(), makeTask(t, makePayload(t))),
		"a suspended host's held jobs are dropped, not held forever")
}

// missingKeySource reports that the signer has no key (a permanent error).
type missingKeySource struct{}

func (missingKeySource) SigningKeyPEM(string, string) (string, error) {
	return "", processors.ErrSigningKeyMissing
}

type signerStep struct {
	resp *http.Response
	err  error
}

// sequenceSigner answers each PostSigned with the next step.
type sequenceSigner struct {
	mu    sync.Mutex
	steps []signerStep
}

func (s *sequenceSigner) PostSigned(string, []byte, *activitypub.PrivateKey) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.steps[0]
	if len(s.steps) > 1 {
		s.steps = s.steps[1:]
	}
	return st.resp, st.err
}

// failingKeySource simulates a DB outage while loading the signing key.
type failingKeySource struct{}

func (failingKeySource) SigningKeyPEM(string, string) (string, error) {
	return "", errors.New("dial tcp: connection refused")
}

type skipAllGate struct{}

func (skipAllGate) ShouldSkipDelivery(string) bool { return true }

// 本物のブレーカーでの通し: 接続失敗が閾値まで続くと、次のジョブは送らずに遅延で
// 返り、相手が戻って試行が通ると閉じる。
func TestDeliverProcessor_Breaker_EndToEnd(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	breaker := deliveryhealth.NewBreaker(rdb)

	down := &stubSigner{err: errors.New("connection refused")}
	p := processors.NewDeliverProcessor(down)
	p.SetDeliveryBreaker(breaker)
	ctx := context.Background()
	for i := range deliveryhealth.BreakerThreshold {
		err := p.Handle(ctx, makeTask(t, makePayload(t)))
		var delayed *driver.DelayError
		if i < deliveryhealth.BreakerThreshold-1 {
			require.False(t, errors.As(err, &delayed), "below the threshold a failure retries normally")
		} else {
			// 閾値に達した失敗で開く。そのジョブも試行回数を消費せずに待たせる。
			require.True(t, errors.As(err, &delayed), "the failure that opens the breaker is held")
		}
	}

	down.gotURL = ""
	err := p.Handle(ctx, makeTask(t, makePayload(t)))
	var delayed *driver.DelayError
	require.ErrorAs(t, err, &delayed)
	assert.GreaterOrEqual(t, delayed.Delay, deliveryhealth.BreakerInitialInterval-time.Second)
	assert.Empty(t, down.gotURL, "an open breaker sends nothing")

	// 開いている間は試行回数を消費しない遅延で返す。
	// 手で閉じれば (または試行が通れば) また送る。
	require.NoError(t, breaker.Close(ctx, "remote.example"))
	up := &stubSigner{resp: okResponse(http.StatusOK)}
	p2 := processors.NewDeliverProcessor(up)
	p2.SetDeliveryBreaker(breaker)
	require.NoError(t, p2.Handle(ctx, makeTask(t, makePayload(t))))
	assert.NotEmpty(t, up.gotURL)
}
