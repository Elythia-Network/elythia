package relationcache

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/event"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var (
	testRedis *testutil.TestRedis
	testDB    *gorm.DB
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	testRedis, err = testutil.SetupRedis(ctx)
	if err != nil {
		log.Fatalf("failed to setup redis: %v", err)
	}
	testDB, err = testutil.OpenTestDB()
	if err != nil {
		testRedis.Teardown(ctx)
		log.Fatalf("failed to open test DB: %v", err)
	}
	testutil.ApplyMigrations(testDB)
	code := m.Run()
	testRedis.Teardown(ctx)
	os.Exit(code)
}

func seedUsers(t *testing.T, prefix string, names ...string) []string {
	t.Helper()
	cleanup := func() { testDB.Exec(`DELETE FROM "user" WHERE id LIKE ?`, prefix+"%") }
	cleanup()
	t.Cleanup(cleanup)
	ids := make([]string, 0, len(names))
	for _, n := range names {
		id := prefix + n
		require.NoError(t, testDB.Create(&model.User{ID: id, Username: id, UsernameLower: id, AvatarDecorations: datatypes.JSON([]byte("[]"))}).Error)
		ids = append(ids, id)
	}
	return ids
}

// TestConnect_WriteInOneProcessReachesAnother runs two "processes" (two caches
// and two pub/sub services on one Redis) and pins that a mute written through
// one process's repository is seen by the other process's next read (#3607).
func TestConnect_WriteInOneProcessReachesAnother(t *testing.T) {
	ids := seedUsers(t, "rcx3607_", "v", "x", "y")
	viewer, x, y := ids[0], ids[1], ids[2]

	busA := event.NewPubSubService(testRedis.Client, "rcx3607:")
	busB := event.NewPubSubService(testRedis.Client, "rcx3607:")
	t.Cleanup(func() { _ = busA.Close(); _ = busB.Close() })
	cacheA := repository.NewRelationCacheWithTTL(testDB, time.Hour)
	cacheB := repository.NewRelationCacheWithTTL(testDB, time.Hour)
	stopA := Connect(cacheA, busA, "A")
	stopB := Connect(cacheB, busB, "B")
	t.Cleanup(func() { stopA(); stopB() })
	// PSUBSCRIBE は非同期に張られる。張り終わる前の publish は届かない。
	require.Eventually(t, func() bool {
		n, err := testRedis.Client.PubSubNumPat(context.Background()).Result()
		return err == nil && n >= 1
	}, 5*time.Second, 10*time.Millisecond)

	got, err := cacheB.MutedUserIDs(viewer)
	require.NoError(t, err)
	assert.Nil(t, got)
	// B がキャッシュから返していることを確かめる (repository を通らない行は見えない)。
	require.NoError(t, testDB.Create(&model.Muting{ID: "rcx3607_m0", MuterID: viewer, MuteeID: y}).Error)
	got, err = cacheB.MutedUserIDs(viewer)
	require.NoError(t, err)
	assert.Nil(t, got)

	repoA := repository.NewMutingRepositoryWithRelationCache(testDB, cacheA)
	require.NoError(t, repoA.Create(&model.Muting{ID: "rcx3607_m1", MuterID: viewer, MuteeID: x}))

	require.Eventually(t, func() bool {
		got, err := cacheB.MutedUserIDs(viewer)
		return err == nil && len(got) == 2
	}, 5*time.Second, 10*time.Millisecond, "process B must drop its cached list")
}

// loopBus delivers every publish to the subscribed handler synchronously.
type loopBus struct {
	mu         sync.Mutex
	handler    func([]byte)
	published  []Message
	publishErr error
}

func (b *loopBus) Publish(_ context.Context, channel string, payload any) error {
	if channel != Topic {
		return errors.New("unexpected channel " + channel)
	}
	if b.publishErr != nil {
		return b.publishErr
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return err
	}
	b.mu.Lock()
	b.published = append(b.published, msg)
	h := b.handler
	b.mu.Unlock()
	if h != nil {
		h(raw)
	}
	return nil
}

func (b *loopBus) Subscribe(_ context.Context, channel string, handler func([]byte)) func() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if channel == Topic {
		b.handler = handler
	}
	return func() {
		b.mu.Lock()
		b.handler = nil
		b.mu.Unlock()
	}
}

func (b *loopBus) deliver(t *testing.T, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	b.mu.Lock()
	h := b.handler
	b.mu.Unlock()
	require.NotNil(t, h)
	h(raw)
}

func TestConnect_FiltersOwnMessagesAndAppliesOthers(t *testing.T) {
	ids := seedUsers(t, "rcl3607_", "v", "x", "y", "z")
	viewer, x, y, z := ids[0], ids[1], ids[2], ids[3]
	rc := repository.NewRelationCacheWithTTL(testDB, time.Hour)
	bus := &loopBus{}
	stop := Connect(rc, bus, "me")
	defer stop()

	repo := repository.NewBlockingRepositoryWithRelationCache(testDB, rc)
	require.NoError(t, repo.Create(&model.Blocking{ID: "rcl3607_b1", BlockerID: x, BlockeeID: viewer}))
	require.Len(t, bus.published, 1)
	assert.Equal(t, Message{From: "me", Inv: repository.RelationInvalidation{Kind: repository.RelationBlockedBy, UserIDs: []string{viewer}}}, bus.published[0])

	got, err := rc.BlockerIDs(viewer)
	require.NoError(t, err)
	assert.Equal(t, []string{x}, got)

	// 自分の通知は受け流す。
	require.NoError(t, testDB.Create(&model.Blocking{ID: "rcl3607_b2", BlockerID: y, BlockeeID: viewer}).Error)
	bus.deliver(t, Message{From: "me", Inv: repository.RelationInvalidation{Kind: repository.RelationBlockedBy, UserIDs: []string{viewer}}})
	got, err = rc.BlockerIDs(viewer)
	require.NoError(t, err)
	assert.Equal(t, []string{x}, got)

	// 他のプロセスの通知は捨てる。
	bus.deliver(t, Message{From: "other", Inv: repository.RelationInvalidation{Kind: repository.RelationBlockedBy, UserIDs: []string{viewer}}})
	got, err = rc.BlockerIDs(viewer)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{x, y}, got)

	// 読めない通知は全体を捨てる。
	require.NoError(t, testDB.Create(&model.Blocking{ID: "rcl3607_b3", BlockerID: z, BlockeeID: viewer}).Error)
	bus.mu.Lock()
	h := bus.handler
	bus.mu.Unlock()
	h([]byte("{not json"))
	got, err = rc.BlockerIDs(viewer)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{x, y, z}, got)

	// publish の失敗は書き込みを失敗させない (他のプロセスでは TTL が上限)。
	bus.publishErr = errors.New("redis down")
	require.NoError(t, repo.Delete(&model.Blocking{ID: "rcl3607_b1"}))
	got, err = rc.BlockerIDs(viewer)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{y, z}, got)
}

func TestConnect_NilIsNoop(t *testing.T) {
	Connect(nil, &loopBus{}, "x")()
	Connect(repository.NewRelationCache(testDB), nil, "x")()
}
