package server

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gatedDeliveryHook records each call when it starts and blocks it until its
// gate is released.
type gatedDeliveryHook struct {
	mu      sync.Mutex
	started []string
	gates   map[string]chan struct{}
}

func newGatedDeliveryHook(names ...string) *gatedDeliveryHook {
	h := &gatedDeliveryHook{gates: map[string]chan struct{}{}}
	for _, n := range names {
		h.gates[n] = make(chan struct{})
	}
	return h
}

func (h *gatedDeliveryHook) call(name string) {
	h.mu.Lock()
	h.started = append(h.started, name)
	gate := h.gates[name]
	h.mu.Unlock()
	if gate != nil {
		<-gate
	}
}

func (h *gatedDeliveryHook) startedCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.started...)
}

func (h *gatedDeliveryHook) OnReactionAdded(_ *model.User, n *model.Note, _ string) {
	h.call("added:" + n.ID)
}

func (h *gatedDeliveryHook) OnReactionRemoved(_ *model.User, n *model.Note, _ string) {
	h.call("removed:" + n.ID)
}

func (h *gatedDeliveryHook) OnNoteDeleted(_ *model.User, n *model.Note) { h.call("deleted:" + n.ID) }

func localUser(id string) *model.User { return &model.User{ID: id} }

func remoteUser(id string) *model.User {
	host := "remote.example"
	return &model.User{ID: id, Host: &host}
}

func returnsWithin(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the hook waited for the delivery")
	}
}

// リアクションとノートの削除の配送は、要求の外で積む (#4216)。内側のhookが
// 戻らなくても、呼び出し元はすぐに戻り、hookは後で走る。
func TestAsyncDeliveryHooksReturnBeforeTheInnerHook(t *testing.T) {
	inner := newGatedDeliveryHook("added:n1", "removed:n2", "deleted:n3")
	runner := newAsyncHookRunner(bgwork.New(), 0)
	reaction := asyncReactionFederationHook{inner: inner, runner: runner}
	del := asyncNoteDeleteFederationHook{inner: inner, runner: runner}

	returnsWithin(t, func() {
		reaction.OnReactionAdded(localUser("u"), &model.Note{ID: "n1"}, ":like@.:")
		reaction.OnReactionRemoved(localUser("u"), &model.Note{ID: "n2"}, ":like@.:")
		del.OnNoteDeleted(localUser("u"), &model.Note{ID: "n3"})
	})
	require.Eventually(t, func() bool { return len(inner.startedCalls()) == 3 }, 5*time.Second, 10*time.Millisecond)
	assert.ElementsMatch(t, []string{"added:n1", "removed:n2", "deleted:n3"}, inner.startedCalls())
	for _, g := range inner.gates {
		close(g)
	}
}

// 同じ人と同じノートのhookは、呼ばれた順に1本ずつ走る。リアクションの付け替えで
// Undo(旧)とLike(新)が逆の順で積まれると、相手のサーバーでリアクションが消える。
func TestAsyncReactionHookKeepsTheOrderForOneNote(t *testing.T) {
	inner := newGatedDeliveryHook("removed:n1")
	hook := asyncReactionFederationHook{inner: inner, runner: newAsyncHookRunner(bgwork.New(), 0)}

	returnsWithin(t, func() {
		hook.OnReactionRemoved(localUser("u"), &model.Note{ID: "n1"}, ":old@.:")
		hook.OnReactionAdded(localUser("u"), &model.Note{ID: "n1"}, ":new@.:")
	})
	require.Eventually(t, func() bool { return len(inner.startedCalls()) == 1 }, 5*time.Second, 10*time.Millisecond)
	// Undoが終わるまでLikeは始まらない。
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, []string{"removed:n1"}, inner.startedCalls())

	close(inner.gates["removed:n1"])
	require.Eventually(t, func() bool { return len(inner.startedCalls()) == 2 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"removed:n1", "added:n1"}, inner.startedCalls())
}

// 同時に走るhookの数は枠で絞る。別の鍵のhookでも、枠が空くまで始まらない。
func TestAsyncHookRunnerBoundsConcurrency(t *testing.T) {
	inner := newGatedDeliveryHook("added:n1", "added:n2")
	runner := newAsyncHookRunner(bgwork.New(), 0)
	runner.slots = make(chan struct{}, 1)
	hook := asyncReactionFederationHook{inner: inner, runner: runner}

	returnsWithin(t, func() {
		hook.OnReactionAdded(localUser("u"), &model.Note{ID: "n1"}, ":like@.:")
		hook.OnReactionAdded(localUser("u"), &model.Note{ID: "n2"}, ":like@.:")
	})
	require.Eventually(t, func() bool { return len(inner.startedCalls()) == 1 }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	require.Len(t, inner.startedCalls(), 1)

	close(inner.gates[inner.startedCalls()[0]])
	require.Eventually(t, func() bool { return len(inner.startedCalls()) == 2 }, 5*time.Second, 10*time.Millisecond)
	close(inner.gates[inner.startedCalls()[1]])
}

// 受信したリアクションとノートの削除(リモートの人)と、ローカル限定のノートは、
// 内側のhookが何もしないので、goroutineを起こさない。
func TestAsyncDeliveryHooksSkipRemoteActors(t *testing.T) {
	inner := newGatedDeliveryHook()
	g := bgwork.New()
	runner := newAsyncHookRunner(g, 0)
	reaction := asyncReactionFederationHook{inner: inner, runner: runner}
	del := asyncNoteDeleteFederationHook{inner: inner, runner: runner}

	reaction.OnReactionAdded(remoteUser("r"), &model.Note{ID: "n1"}, ":like@.:")
	reaction.OnReactionRemoved(remoteUser("r"), &model.Note{ID: "n1"}, ":like@.:")
	del.OnNoteDeleted(remoteUser("r"), &model.Note{ID: "n2"})
	reaction.OnReactionAdded(localUser("u"), &model.Note{ID: "n3", LocalOnly: true}, ":like@.:")
	reaction.OnReactionRemoved(localUser("u"), &model.Note{ID: "n3", LocalOnly: true}, ":like@.:")
	del.OnNoteDeleted(localUser("u"), &model.Note{ID: "n4", LocalOnly: true})
	reaction.OnReactionAdded(nil, &model.Note{ID: "n5"}, ":like@.:")
	reaction.OnReactionRemoved(localUser("u"), nil, ":like@.:")
	del.OnNoteDeleted(nil, &model.Note{ID: "n6"})

	assert.Zero(t, g.Running())
	g.Stop(context.Background())
	assert.Empty(t, inner.startedCalls())
	assert.Zero(t, g.Dropped())
}

// 世代が止まった後に呼ばれたhookは走らせない。後に続く同じ鍵のhookが、走らなかった
// hookを待ったまま残らない。
func TestAsyncHookRunnerDoesNotWaitForAHookItDidNotStart(t *testing.T) {
	g := bgwork.New()
	g.Stop(context.Background())
	runner := newAsyncHookRunner(g, 0)
	hook := asyncReactionFederationHook{inner: newGatedDeliveryHook(), runner: runner}

	hook.OnReactionRemoved(localUser("u"), &model.Note{ID: "n1"}, ":like@.:")
	hook.OnReactionAdded(localUser("u"), &model.Note{ID: "n1"}, ":like@.:")

	runner.mu.Lock()
	defer runner.mu.Unlock()
	assert.Empty(t, runner.tails)
}

// 組み立てたサーバーで、リアクションとノートの削除のサービスに、世代のbgworkで
// 要求の外で積むhookが付いていること (#4216)。付け忘れると応答がまた配送を待ち、
// 世代のbgworkでないと停止のときに待たれない。
func TestBuiltServerQueuesReactionAndDeleteDeliveryOutsideTheRequest(t *testing.T) {
	srv := newServerWithConfig(t, func(cfg *config.Config) { cfg.NoteHookConcurrency = 3 })

	reactionHook := reflect.ValueOf(srv.builtCore.reactionService).Elem().FieldByName("federationHook")
	require.True(t, reactionHook.IsValid())
	require.False(t, reactionHook.IsNil())
	require.Equal(t, reflect.TypeOf(asyncReactionFederationHook{}), reactionHook.Elem().Type())

	deleteService := reflect.ValueOf(srv.builtRouteParts.notes).Elem().FieldByName("deleteService")
	require.True(t, deleteService.IsValid())
	deleteHook := deleteService.Elem().FieldByName("federationHook")
	require.True(t, deleteHook.IsValid())
	require.False(t, deleteHook.IsNil())
	require.Equal(t, reflect.TypeOf(asyncNoteDeleteFederationHook{}), deleteHook.Elem().Type())

	// 削除が続いてもリアクションの配送が待たされないよう、枠は別に持つ。
	assert.NotEqual(t, reactionHook.Elem().FieldByName("runner").Pointer(), deleteHook.Elem().FieldByName("runner").Pointer())
	for name, hook := range map[string]reflect.Value{"reaction": reactionHook.Elem(), "note delete": deleteHook.Elem()} {
		runner := hook.FieldByName("runner")
		require.False(t, runner.IsNil(), name)
		assert.False(t, hook.FieldByName("inner").IsNil(), name)
		assert.Equal(t, reflect.ValueOf(srv.background).Pointer(), runner.Elem().FieldByName("background").Pointer(), name)
		// 枠の大きさは noteHookConcurrency に従う。
		assert.Equal(t, 3, runner.Elem().FieldByName("slots").Cap(), name)
	}
}

// 先頭のhookが終わって次のhookが走っている間に呼ばれたhookも、走っているhookを
// 待つ。先頭のhookの後始末が、後ろのhookの印を消してはいけない。
func TestAsyncHookRunnerKeepsTheOrderAfterTheFirstHookFinished(t *testing.T) {
	inner := newGatedDeliveryHook("removed:n1", "added:n1")
	hook := asyncReactionFederationHook{inner: inner, runner: newAsyncHookRunner(bgwork.New(), 0)}

	hook.OnReactionRemoved(localUser("u"), &model.Note{ID: "n1"}, ":a@.:")
	hook.OnReactionAdded(localUser("u"), &model.Note{ID: "n1"}, ":b@.:")
	require.Eventually(t, func() bool { return len(inner.startedCalls()) == 1 }, 5*time.Second, 10*time.Millisecond)
	close(inner.gates["removed:n1"])
	require.Eventually(t, func() bool { return len(inner.startedCalls()) == 2 }, 5*time.Second, 10*time.Millisecond)

	// 2本目が走っている間に3本目を呼ぶ。
	hook.OnReactionRemoved(localUser("u"), &model.Note{ID: "n1"}, ":b@.:")
	time.Sleep(100 * time.Millisecond)
	assert.Len(t, inner.startedCalls(), 2)

	close(inner.gates["added:n1"])
	require.Eventually(t, func() bool { return len(inner.startedCalls()) == 3 }, 5*time.Second, 10*time.Millisecond)
}

// 世代の停止で待ちが尽きたら、前のhookを待っていたhookは走らせずに捨てる。前の
// hookがまだ走っているのに、同じ鍵の後ろのhookが走ることは無い。
func TestAsyncHookRunnerDropsWaitingHooksOnStop(t *testing.T) {
	// 3本目の待ち(select)は、前のhookの終わりと停止のどちらも準備できていて、Goが
	// 乱数で選ぶので、何度か繰り返す。
	for range 20 {
		inner := newGatedDeliveryHook("removed:n1")
		g := bgwork.New()
		hook := asyncReactionFederationHook{inner: inner, runner: newAsyncHookRunner(g, 0)}

		hook.OnReactionRemoved(localUser("u"), &model.Note{ID: "n1"}, ":a@.:")
		hook.OnReactionAdded(localUser("u"), &model.Note{ID: "n1"}, ":b@.:")
		hook.OnReactionRemoved(localUser("u"), &model.Note{ID: "n1"}, ":b@.:")
		require.Eventually(t, func() bool { return len(inner.startedCalls()) == 1 }, 5*time.Second, 10*time.Millisecond)

		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		g.Stop(ctx)
		cancel()
		require.Eventually(t, func() bool { return g.Dropped() == 2 }, 5*time.Second, 10*time.Millisecond)
		assert.Equal(t, []string{"removed:n1"}, inner.startedCalls())
		close(inner.gates["removed:n1"])
	}
}

// panickingDeliveryHook panics on its first OnReactionRemoved.
type panickingDeliveryHook struct {
	*gatedDeliveryHook
	once sync.Once
}

func (h *panickingDeliveryHook) OnReactionRemoved(u *model.User, n *model.Note, r string) {
	h.once.Do(func() { panic("boom") })
	h.gatedDeliveryHook.OnReactionRemoved(u, n, r)
}

// hookがpanicしても、同じ鍵の後ろのhookは止まらず、枠も返る。枠が1つなので、
// panicで枠が漏れると後ろのhookは走れない。
func TestAsyncHookRunnerContinuesAfterAPanic(t *testing.T) {
	inner := &panickingDeliveryHook{gatedDeliveryHook: newGatedDeliveryHook()}
	runner := newAsyncHookRunner(bgwork.New(), 1)
	hook := asyncReactionFederationHook{inner: inner, runner: runner}

	hook.OnReactionRemoved(localUser("u"), &model.Note{ID: "n1"}, ":a@.:")
	hook.OnReactionAdded(localUser("u"), &model.Note{ID: "n1"}, ":b@.:")
	require.Eventually(t, func() bool { return len(inner.startedCalls()) == 1 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"added:n1"}, inner.startedCalls())
}
