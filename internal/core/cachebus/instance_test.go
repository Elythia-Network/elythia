package cachebus

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/instance"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/processors"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newInstanceService builds the instance service one process would have, on
// the shared test DB.
func newInstanceService(t *testing.T) *instance.Service {
	t.Helper()
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return instance.NewService(repository.NewInstanceRepository(testDB), repository.NewMetaRepository(testDB), gen)
}

// seedInstance inserts an unsuspended instance row and lets federation run
// for every host, so only the suspension decides.
func seedInstance(t *testing.T, host string) {
	t.Helper()
	cleanup := func() { testDB.Exec(`DELETE FROM instance WHERE host = ?`, host) }
	cleanup()
	t.Cleanup(cleanup)
	// 新しい DB には meta の行がまだ無い。Fetch で作ってから書き換える (作らずに
	// UPDATE すると 0 行で終わり、既定の行で判定される)。
	_, err := repository.NewMetaRepository(testDB).Fetch()
	require.NoError(t, err)
	require.NoError(t, testDB.Exec(`UPDATE meta SET federation = 'all'`).Error)
	require.NoError(t, testDB.Create(&model.Instance{ID: "cb3664_" + host, Host: host, SuspensionState: model.SuspensionStateNone}).Error)
}

// TestInstanceSuspension_ReachesOtherProcess runs two "processes" (two
// instance services and two pub/sub services on one Redis and one DB) and pins
// that suspending a host in one of them is seen at once by the other, whose
// decision was already cached for the 5-minute TTL (#3664).
func TestInstanceSuspension_ReachesOtherProcess(t *testing.T) {
	const host = "susp3664.example"
	seedInstance(t, host)
	busA, busB := twoBuses(t, "cbinst3664:")
	a, b := newInstanceService(t), newInstanceService(t)
	t.Cleanup(ConnectInstanceSuspension(a, busA, "A"))
	t.Cleanup(ConnectInstanceSuspension(b, busB, "B"))
	waitSubscribed(t, busA, busB)

	require.False(t, b.ShouldSkipDelivery(host), "B caches \"deliver\" for the TTL")

	require.NoError(t, a.Suspend(host, model.SuspensionStateManuallySuspended))
	require.Eventually(t, func() bool { return b.ShouldSkipDelivery(host) }, 5*time.Second, 10*time.Millisecond,
		"process B must stop delivering to a host suspended in process A")

	// 停止の解除も伝わる。
	require.NoError(t, a.Suspend(host, model.SuspensionStateNone))
	require.Eventually(t, func() bool { return !b.ShouldSkipDelivery(host) }, 5*time.Second, 10*time.Millisecond)
}

// countingSigner counts the deliveries that reached the HTTP layer.
type countingSigner struct{ posts atomic.Int32 }

func (s *countingSigner) PostSigned(string, []byte, *activitypub.PrivateKey) (*http.Response, error) {
	s.posts.Add(1)
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(""))}, nil
}

func testKeyPEM(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)}))
}

// TestInstanceSuspension_StopsDeliveryInOtherProcess drives the deliver job of
// the queue process (its dispatch-time gate is the instance service) and pins
// that a suspension made in the web process stops the very next delivery,
// instead of after the 5-minute cache (#3664).
func TestInstanceSuspension_StopsDeliveryInOtherProcess(t *testing.T) {
	const host = "deliver3664.example"
	seedInstance(t, host)
	busWeb, busQueue := twoBuses(t, "cbdeliver3664:")
	web, queueSide := newInstanceService(t), newInstanceService(t)
	t.Cleanup(ConnectInstanceSuspension(web, busWeb, "web"))
	t.Cleanup(ConnectInstanceSuspension(queueSide, busQueue, "queue"))
	waitSubscribed(t, busWeb, busQueue)

	signer := &countingSigner{}
	p := processors.NewDeliverProcessor(signer, queueSide, nil, nil, nil, nil)
	payload := queue.DeliverPayload{
		Inbox:  "https://" + host + "/inbox",
		Body:   []byte(`{"type":"Create"}`),
		KeyID:  "https://local.example/users/u1#main-key",
		KeyPEM: testKeyPEM(t),
	}
	deliver := func() {
		t.Helper()
		require.NoError(t, p.Handle(context.Background(), queue.NewDeliverTask(payload)))
	}

	deliver()
	require.Equal(t, int32(1), signer.posts.Load(), "delivers before the suspension (and caches that decision)")

	require.NoError(t, web.Suspend(host, model.SuspensionStateManuallySuspended))
	require.Eventually(t, func() bool {
		before := signer.posts.Load()
		deliver()
		return signer.posts.Load() == before
	}, 5*time.Second, 10*time.Millisecond, "the queue process must stop delivering once the web process suspends the host")
}

// TestConnectInstanceSuspension_Wire pins the wire format, that the process
// ignores its own messages, applies others' and drops everything on a message
// it cannot read.
func TestConnectInstanceSuspension_Wire(t *testing.T) {
	svc, repo := newMockInstanceService(t)
	for _, h := range []string{"a.example", "b.example"} {
		repo.Instances[h] = &model.Instance{ID: h, Host: h, SuspensionState: model.SuspensionStateNone}
	}
	bus := newLoopBus()
	stop := ConnectInstanceSuspension(svc, bus, "me")
	defer stop()

	require.NoError(t, svc.Suspend("a.example", model.SuspensionStateManuallySuspended))
	assert.Equal(t, []Message[InstanceSuspensionInvalidation]{{From: "me", Inv: InstanceSuspensionInvalidation{Host: "a.example"}}},
		messages[InstanceSuspensionInvalidation](t, bus, TopicInstanceSuspension))

	cache := func() {
		svc.ShouldSkipDelivery("a.example")
		svc.ShouldSkipDelivery("b.example")
		require.Equal(t, 2, svc.SuspendCacheLen())
	}
	cache()
	// 自分の通知は受け流す。
	deliverMessage(t, bus, TopicInstanceSuspension, Message[InstanceSuspensionInvalidation]{From: "me", Inv: InstanceSuspensionInvalidation{Host: "a.example"}})
	assert.Equal(t, 2, svc.SuspendCacheLen())
	// 他のプロセスの通知はその host だけ捨てる。
	deliverMessage(t, bus, TopicInstanceSuspension, Message[InstanceSuspensionInvalidation]{From: "other", Inv: InstanceSuspensionInvalidation{Host: "a.example"}})
	assert.Equal(t, 1, svc.SuspendCacheLen())
	// 読めない通知は全体を捨てる。
	cache()
	bus.deliver(t, TopicInstanceSuspension, []byte("{not json"))
	assert.Equal(t, 0, svc.SuspendCacheLen())

	// publish に失敗しても、自プロセスの判定は捨てる (今までどおり)。
	cache()
	bus.setPublishErr(assert.AnError)
	require.NoError(t, svc.Suspend("b.example", model.SuspensionStateManuallySuspended))
	assert.True(t, svc.ShouldSkipDelivery("b.example"))
}

func TestConnectInstanceSuspension_NilIsNoop(t *testing.T) {
	ConnectInstanceSuspension(nil, newLoopBus(), "x")()
	svc, _ := newMockInstanceService(t)
	ConnectInstanceSuspension(svc, nil, "x")()
}

func newMockInstanceService(t *testing.T) (*instance.Service, *testutil.MockInstanceRepository) {
	t.Helper()
	repo := testutil.NewMockInstanceRepository()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{}
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return instance.NewService(repo, metaRepo, gen), repo
}
