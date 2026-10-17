package cachebus

import (
	"context"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/relay"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newRelayService(t *testing.T, repo repository.RelayRepository) *relay.Service {
	t.Helper()
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	// 一覧のキャッシュに関わる経路 (状態の変更と IsRelayActor) は、配送の部品を使わない。
	return relay.NewService(repo, nil, nil, nil, gen)
}

// TestRelays_AcceptReachesOtherProcess runs two "processes" on one Redis and
// one DB and pins that a relay accepted in one of them (the inbox of the queue
// process) is used at once by the other, whose list was cached for 10 minutes
// (#3664).
func TestRelays_AcceptReachesOtherProcess(t *testing.T) {
	const relayID, inbox = "cbrelay3664", "https://relay3664.example/inbox"
	cleanup := func() { testDB.Exec(`DELETE FROM relay WHERE id = ?`, relayID) }
	cleanup()
	t.Cleanup(cleanup)
	require.NoError(t, testDB.Create(&model.Relay{ID: relayID, Inbox: inbox, Status: relay.StatusRequesting}).Error)

	busA, busB := twoBuses(t, "cbrelay3664:")
	a := newRelayService(t, repository.NewRelayRepository(testDB))
	b := newRelayService(t, repository.NewRelayRepository(testDB))
	t.Cleanup(ConnectRelays(a, busA, "A"))
	t.Cleanup(ConnectRelays(b, busB, "B"))
	waitSubscribed(t, busA, busB)

	inboxCopy := inbox
	actor := &model.User{ID: "relay-actor", SharedInbox: &inboxCopy}
	require.False(t, b.IsRelayActor(actor), "B caches the list without the relay")

	require.NoError(t, a.MarkAccepted(context.Background(), relayID))
	require.Eventually(t, func() bool { return b.IsRelayActor(actor) }, 5*time.Second, 10*time.Millisecond,
		"process B must see the relay accepted in process A")
}

func TestConnectRelays_Wire(t *testing.T) {
	repo := testutil.NewMockRelayRepository()
	svc := newRelayService(t, repo)
	bus := newLoopBus()
	stop := ConnectRelays(svc, bus, "me")
	defer stop()

	inbox := "https://r.example/inbox"
	require.NoError(t, repo.Create(&model.Relay{ID: "r1", Inbox: inbox, Status: relay.StatusRequesting}))
	actor := &model.User{ID: "x", Inbox: &inbox}
	assert.False(t, svc.IsRelayActor(actor))

	require.NoError(t, svc.MarkAccepted(context.Background(), "r1"))
	assert.Equal(t, []Message[RelayInvalidation]{{From: "me"}}, messages[RelayInvalidation](t, bus, TopicRelay))
	assert.True(t, svc.IsRelayActor(actor), "the writer drops its own list")

	// 以下は DB を直接書き換えて、キャッシュから返っていることを確かめながら見る。
	setStatus := func(status string) {
		r, err := repo.FindByID("r1")
		require.NoError(t, err)
		r.Status = status
	}
	setStatus(relay.StatusRejected)
	assert.True(t, svc.IsRelayActor(actor), "cached")
	// 自分の通知は受け流す。
	deliverMessage(t, bus, TopicRelay, Message[RelayInvalidation]{From: "me"})
	assert.True(t, svc.IsRelayActor(actor))
	// 他のプロセスの通知で捨てる。
	deliverMessage(t, bus, TopicRelay, Message[RelayInvalidation]{From: "other"})
	assert.False(t, svc.IsRelayActor(actor))
	// 読めない通知も捨てる。
	setStatus(relay.StatusAccepted)
	assert.False(t, svc.IsRelayActor(actor), "cached")
	bus.deliver(t, TopicRelay, []byte("{not json"))
	assert.True(t, svc.IsRelayActor(actor))
}

func TestConnectRelays_NilIsNoop(t *testing.T) {
	ConnectRelays(nil, newLoopBus(), "x")()
	ConnectRelays(newRelayService(t, testutil.NewMockRelayRepository()), nil, "x")()
}
