package notification

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

type observed struct {
	recipient *model.User
	n         *Notification
}

type recordingObserver struct {
	mu   sync.Mutex
	seen []observed
}

func (o *recordingObserver) NotificationCreated(_ context.Context, recipient *model.User, n *Notification) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, observed{recipient: recipient, n: n})
}

// 作った通知が、宛先の利用者と一緒に observer へ渡る (#3469)。
func TestHook_CreatedObserver_SeesPersistedNotification(t *testing.T) {
	h, svc, repo := newTestHook(t)
	addLocalUser(repo, "alice", "alice")
	obs := &recordingObserver{}
	h.SetCreatedObserver(obs)

	h.OnReactionCreated("alice", "bob", "note1", ":x@.:")

	require.Len(t, obs.seen, 1)
	assert.Equal(t, "alice", obs.seen[0].recipient.ID)
	got := obs.seen[0].n
	assert.Equal(t, TypeReaction, got.Type)
	assert.Equal(t, "bob", got.NotifierID)
	assert.Equal(t, "note1", got.NoteID)
	assert.Equal(t, ":x@.:", got.Reaction)
	list, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, list[0].ID, got.ID, "observer が受け取るのは永続化した通知そのもの")
}

// 抑制で作られなかった通知は observer へ渡さない (#3469)。ミュート・受信設定・
// ロールの opt-out・リモート宛て・自分自身の操作。
func TestHook_CreatedObserver_NotCalledWhenSuppressed(t *testing.T) {
	t.Run("mute", func(t *testing.T) {
		h, _, repo := newTestHook(t)
		addLocalUser(repo, "alice", "alice")
		obs := &recordingObserver{}
		h.SetCreatedObserver(obs)
		h.SetMuteChecker(&stubMuteChecker{muted: true})
		h.OnFollowed("bob", "alice")
		assert.Empty(t, obs.seen)
	})
	t.Run("receiveConfig never", func(t *testing.T) {
		h, _, repo := newTestHook(t)
		addLocalUser(repo, "alice", "alice")
		repo.Profiles["alice"] = &model.UserProfile{UserID: "alice", NotificationRecieveConfig: []byte(`{"follow":{"type":"never"}}`)}
		obs := &recordingObserver{}
		h.SetCreatedObserver(obs)
		h.OnFollowed("bob", "alice")
		assert.Empty(t, obs.seen)
	})
	t.Run("role opt-out", func(t *testing.T) {
		h, svc, repo := newTestHook(t)
		addLocalUser(repo, "alice", "alice")
		svc.SetPolicyResolver(&stubPolicyResolver{optOut: map[string][]string{"alice": {string(TypeFollow)}}})
		obs := &recordingObserver{}
		h.SetCreatedObserver(obs)
		h.OnFollowed("bob", "alice")
		assert.Empty(t, obs.seen)
	})
	t.Run("remote recipient", func(t *testing.T) {
		h, _, repo := newTestHook(t)
		addRemoteUser(repo, "alice", "alice", "remote.example")
		obs := &recordingObserver{}
		h.SetCreatedObserver(obs)
		h.OnFollowed("bob", "alice")
		assert.Empty(t, obs.seen)
	})
	t.Run("self", func(t *testing.T) {
		h, _, repo := newTestHook(t)
		addLocalUser(repo, "alice", "alice")
		obs := &recordingObserver{}
		h.SetCreatedObserver(obs)
		h.OnReactionCreated("alice", "alice", "note1", ":x@.:")
		assert.Empty(t, obs.seen)
	})
}

// 宛先を確かめられない構成 (userRepo 未配線) では、通知は作っても observer へは
// 渡さない。誰の通知か分からないまま積むと、プラグインの取り違えになりうる。
func TestHook_CreatedObserver_NeedsUserRepo(t *testing.T) {
	_, svc, _ := newTestHook(t)
	h := NewHook(svc, nil)
	obs := &recordingObserver{}
	h.SetCreatedObserver(obs)
	h.OnFollowed("bob", "alice")
	list, err := svc.List(context.Background(), "alice", "", "", 10, nil, nil)
	require.NoError(t, err)
	assert.Len(t, list, 1, "通知そのものは作る")
	assert.Empty(t, obs.seen)
}
