package cachebus

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// startUserLink connects c and runs its sender until the test ends.
func startUserLink(t *testing.T, c *repository.CachedUserRepository, bus Bus, sender string) {
	t.Helper()
	link := ConnectUsers(c, bus, sender)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		link.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		link.Stop()
	})
}

// TestUsers_WriteReachesOtherProcess runs two "processes" (two user caches and
// two pub/sub services on one Redis and one DB) and pins that a user updated
// through one of them (as the inbox of the queue process does) is read fresh
// at once by the other, whose cache held the row for 5 minutes (#3664).
func TestUsers_WriteReachesOtherProcess(t *testing.T) {
	const prefix = "cbuser3664_"
	cleanup := func() { testDB.Exec(`DELETE FROM "user" WHERE id LIKE ?`, prefix+"%") }
	cleanup()
	t.Cleanup(cleanup)
	id := prefix + "u1"
	require.NoError(t, testDB.Create(&model.User{ID: id, Username: id, UsernameLower: id, AvatarDecorations: datatypes.JSON([]byte("[]"))}).Error)
	require.NoError(t, testDB.Create(&model.UserProfile{UserID: id}).Error)

	busA, busB := twoBuses(t, "cbuser3664:")
	a := repository.NewCachedUserRepository(repository.NewUserRepository(testDB))
	b := repository.NewCachedUserRepository(repository.NewUserRepository(testDB))
	startUserLink(t, a, busA, "A")
	startUserLink(t, b, busB, "B")
	waitSubscribed(t, busA, busB)

	u, err := b.FindByID(id)
	require.NoError(t, err)
	require.False(t, u.IsSuspended)
	// B がキャッシュから返していることを確かめる (repository を通らない書き込みは見えない)。
	require.NoError(t, testDB.Model(&model.User{}).Where("id = ?", id).Update("notesCount", 5).Error)
	u, err = b.FindByID(id)
	require.NoError(t, err)
	require.Equal(t, 0, u.NotesCount)

	require.NoError(t, a.UpdateUser(id, map[string]any{"isSuspended": true}))
	require.Eventually(t, func() bool {
		u, err := b.FindByID(id)
		return err == nil && u.IsSuspended
	}, 5*time.Second, 10*time.Millisecond, "process B must see the suspension written in process A")

	// profile (パスワードや 2FA を持つ) も同じ。
	p, err := b.FindProfileByUserID(id)
	require.NoError(t, err)
	require.Nil(t, p.Description)
	require.NoError(t, a.UpdateProfile(id, map[string]any{"description": "new"}))
	require.Eventually(t, func() bool {
		p, err := b.FindProfileByUserID(id)
		return err == nil && p.Description != nil && *p.Description == "new"
	}, 5*time.Second, 10*time.Millisecond)

	// B が「その URI の利用者は居ない」を持っている間に A がリモートの利用者を作る。
	uri := "https://remote3664.example/users/r1"
	_, err = b.FindByURI(uri)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	rid := prefix + "r1"
	host := "remote3664.example"
	require.NoError(t, a.Create(&model.User{ID: rid, Username: "r1", UsernameLower: "r1", Host: &host, URI: &uri, AvatarDecorations: datatypes.JSON([]byte("[]"))}))
	require.Eventually(t, func() bool {
		u, err := b.FindByURI(uri)
		return err == nil && u.ID == rid
	}, 5*time.Second, 10*time.Millisecond, "process B must find the user created in process A")
}

// collectingPublisher records what the queue publishes.
type collectingPublisher struct {
	mu   sync.Mutex
	got  []repository.UserCacheInvalidation
	gate chan struct{}
}

func (p *collectingPublisher) publish(inv repository.UserCacheInvalidation) {
	if p.gate != nil {
		<-p.gate
	}
	p.mu.Lock()
	p.got = append(p.got, inv)
	p.mu.Unlock()
}

func (p *collectingPublisher) published() []repository.UserCacheInvalidation {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]repository.UserCacheInvalidation(nil), p.got...)
}

// TestUserQueue_CoalescesAndBatches pins how writes become messages: what
// piles up while a publish is in flight goes out as one message, the same id
// is sent once, and one message carries at most the batch size.
func TestUserQueue_CoalescesAndBatches(t *testing.T) {
	pub := &collectingPublisher{}
	q := newUserQueue(100, 3, pub.publish)

	q.Enqueue(repository.UserCacheInvalidation{IDs: []string{"a"}})
	q.Enqueue(repository.UserCacheInvalidation{IDs: []string{"b", "a"}})
	q.Enqueue(repository.UserCacheInvalidation{URIs: []string{"u1"}})
	q.Enqueue(repository.UserCacheInvalidation{IDs: []string{"c", "d", ""}, URIs: []string{"u1", "", "u2"}})
	q.publishPending(context.Background(), time.Time{})

	assert.Equal(t, []repository.UserCacheInvalidation{
		{IDs: []string{"a", "b", "c"}},
		{IDs: []string{"d"}, URIs: []string{"u1", "u2"}},
	}, pub.published())

	// 送り終えた id は、また書かれたら送る。
	q.Enqueue(repository.UserCacheInvalidation{IDs: []string{"a"}})
	q.publishPending(context.Background(), time.Time{})
	assert.Equal(t, repository.UserCacheInvalidation{IDs: []string{"a"}}, pub.published()[2])
}

// TestUserQueue_OverflowCollapsesToAll pins that the queue never drops an
// invalidation silently: past the limit, everything pending becomes one All,
// which also absorbs what comes after it until it is sent.
func TestUserQueue_OverflowCollapsesToAll(t *testing.T) {
	pub := &collectingPublisher{}
	q := newUserQueue(3, 10, pub.publish)
	for i := range 4 {
		q.Enqueue(repository.UserCacheInvalidation{IDs: []string{fmt.Sprint(i)}})
	}
	q.Enqueue(repository.UserCacheInvalidation{IDs: []string{"later"}})
	q.publishPending(context.Background(), time.Time{})
	assert.Equal(t, []repository.UserCacheInvalidation{{All: true}}, pub.published())

	q.Enqueue(repository.UserCacheInvalidation{IDs: []string{"x"}})
	q.Enqueue(repository.UserCacheInvalidation{All: true})
	q.publishPending(context.Background(), time.Time{})
	assert.Equal(t, []repository.UserCacheInvalidation{{All: true}, {All: true}}, pub.published())
}

// TestUserQueue_RunDrainsOnStop pins that Run publishes what is still pending
// when it is stopped, within the drain timeout, and then returns.
func TestUserQueue_RunDrainsOnStop(t *testing.T) {
	pub := &collectingPublisher{gate: make(chan struct{})}
	q := newUserQueue(100, 1, pub.publish)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		q.Run(ctx)
	}()
	q.Enqueue(repository.UserCacheInvalidation{IDs: []string{"a"}})
	// 1 件目の送信中に 2 件積み、止める。
	require.Eventually(t, func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return len(q.ids) == 0
	}, time.Second, time.Millisecond)
	q.Enqueue(repository.UserCacheInvalidation{IDs: []string{"b"}})
	q.Enqueue(repository.UserCacheInvalidation{IDs: []string{"c"}})
	cancel()
	close(pub.gate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	assert.Equal(t, []repository.UserCacheInvalidation{{IDs: []string{"a"}}, {IDs: []string{"b"}}, {IDs: []string{"c"}}}, pub.published())
}

// TestUserQueue_DrainStopsAtDeadline pins that the drain on stop gives up at
// its deadline instead of holding the shutdown.
func TestUserQueue_DrainStopsAtDeadline(t *testing.T) {
	pub := &collectingPublisher{}
	q := newUserQueue(100, 1, pub.publish)
	q.Enqueue(repository.UserCacheInvalidation{IDs: []string{"a"}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	q.publishPending(ctx, time.Now().Add(-time.Second))
	q.publishPending(ctx, time.Time{})
	assert.Empty(t, pub.published())
	q.drainTimeout = time.Second
	q.Run(ctx)
	assert.Equal(t, []repository.UserCacheInvalidation{{IDs: []string{"a"}}}, pub.published())
}

// TestConnectUsers_Wire pins the wire format, that the process ignores its
// own messages, applies others' and drops everything on a message it cannot
// read.
func TestConnectUsers_Wire(t *testing.T) {
	inner := repository.NewUserRepository(testDB)
	const prefix = "cbuserwire3664_"
	cleanup := func() { testDB.Exec(`DELETE FROM "user" WHERE id LIKE ?`, prefix+"%") }
	cleanup()
	t.Cleanup(cleanup)
	id := prefix + "u1"
	require.NoError(t, testDB.Create(&model.User{ID: id, Username: id, UsernameLower: id, AvatarDecorations: datatypes.JSON([]byte("[]"))}).Error)

	c := repository.NewCachedUserRepository(inner)
	bus := newLoopBus()
	link := ConnectUsers(c, bus, "me")
	defer link.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		link.Run(ctx)
	}()
	defer func() { cancel(); <-done }()

	require.NoError(t, c.IncrementNotesCount(id, 1))
	require.Eventually(t, func() bool { return len(messages[repository.UserCacheInvalidation](t, bus, TopicUser)) == 1 }, 5*time.Second, time.Millisecond)
	assert.Equal(t, Message[repository.UserCacheInvalidation]{From: "me", Inv: repository.UserCacheInvalidation{IDs: []string{id}}},
		messages[repository.UserCacheInvalidation](t, bus, TopicUser)[0])

	notes := func() int {
		u, err := c.FindByID(id)
		require.NoError(t, err)
		return u.NotesCount
	}
	require.Equal(t, 1, notes())
	bump := func() {
		require.NoError(t, testDB.Model(&model.User{}).Where("id = ?", id).Update("notesCount", gorm.Expr(`"notesCount" + 1`)).Error)
	}
	bump()
	require.Equal(t, 1, notes(), "cached")
	// 自分の通知は受け流す。
	deliverMessage(t, bus, TopicUser, Message[repository.UserCacheInvalidation]{From: "me", Inv: repository.UserCacheInvalidation{IDs: []string{id}}})
	assert.Equal(t, 1, notes())
	// 他のプロセスの通知で捨てる。
	deliverMessage(t, bus, TopicUser, Message[repository.UserCacheInvalidation]{From: "other", Inv: repository.UserCacheInvalidation{IDs: []string{id}}})
	assert.Equal(t, 2, notes())
	// 読めない通知は全体を捨てる。
	bump()
	require.Equal(t, 2, notes(), "cached")
	bus.deliver(t, TopicUser, []byte("{not json"))
	assert.Equal(t, 3, notes())
}

func TestConnectUsers_NilIsNoop(t *testing.T) {
	link := ConnectUsers(nil, newLoopBus(), "x")
	link.Run(context.Background())
	link.Stop()
	link = ConnectUsers(repository.NewCachedUserRepository(repository.NewUserRepository(testDB)), nil, "x")
	link.Run(context.Background())
	link.Stop()
}
