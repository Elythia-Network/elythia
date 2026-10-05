package stream

import (
	"encoding/json"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// relFollowingLookup / relMuteBlockLookup は snapshot lookup の test stub。
// 呼び出し回数も数えて、scope による呼び分けを検証できるようにする。
type relFollowingLookup struct {
	snaps map[string]map[string]bool
	calls int
}

func (s *relFollowingLookup) FollowingSnapshotForUser(userID string) map[string]bool {
	s.calls++
	return s.snaps[userID]
}

type relMuteBlockLookup struct {
	snaps map[string]*MuteBlockSnapshot
	calls int
}

func (s *relMuteBlockLookup) MuteBlockSnapshotForUser(userID string) *MuteBlockSnapshot {
	s.calls++
	return s.snaps[userID]
}

func muteSnapshot(mutedIDs ...string) *MuteBlockSnapshot {
	snap := &MuteBlockSnapshot{
		Muting:         map[string]struct{}{},
		BlockingMe:     map[string]struct{}{},
		RenoteMuting:   map[string]struct{}{},
		MutedInstances: map[string]struct{}{},
		MutingChannels: map[string]struct{}{},
	}
	for _, id := range mutedIDs {
		snap.Muting[id] = struct{}{}
	}
	return snap
}

func newRelationManager(t *testing.T) (*Manager, *stubBus, *relFollowingLookup, *relMuteBlockLookup) {
	t.Helper()
	bus := newStubBus()
	m := NewManager(NewRegistry(), bus)
	fl := &relFollowingLookup{snaps: map[string]map[string]bool{}}
	ml := &relMuteBlockLookup{snaps: map[string]*MuteBlockSnapshot{}}
	m.SetFollowingSnapshotLookup(fl)
	m.SetMuteBlockSnapshotLookup(ml)
	return m, bus, fl, ml
}

func registerConn(t *testing.T, m *Manager, id, userID string) *Connection {
	t.Helper()
	c := NewConnection(id, &model.User{ID: userID}, newFakeConn())
	m.register(c)
	t.Cleanup(func() { m.unregister(c.ID()) })
	return c
}

// #2400 の本体。pubsub message で既存 connection の snapshot が取り直される。
// これが無いと mute / block した直後も対象 user の event が届き続ける。
func TestManager_SubscribeRelationReload_RefreshesSnapshot(t *testing.T) {
	m, bus, fl, ml := newRelationManager(t)
	ml.snaps["alice"] = muteSnapshot()
	fl.snaps["alice"] = map[string]bool{"bob": false}

	conn := registerConn(t, m, "c1", "alice")
	conn.SetMuteBlockSnapshot(ml.snaps["alice"])
	conn.SetFollowingSnapshot(fl.snaps["alice"])
	require.Empty(t, conn.MuteBlockSnapshot().Muting)

	m.SubscribeRelationReload()
	require.Contains(t, bus.subs, RelationReloadTopic)

	// alice が carol を mute した状態にして reload を配送。
	ml.snaps["alice"] = muteSnapshot("carol")
	payload, _ := json.Marshal(RelationReloadPayload{UserID: "alice", Scope: RelationScopeMuteBlock})
	bus.deliver(RelationReloadTopic, payload)

	assert.Contains(t, conn.MuteBlockSnapshot().Muting, "carol")
}

// 対象 user の connection だけを更新する。
func TestManager_RefreshRelations_OnlyAffectsTargetUser(t *testing.T) {
	m, _, _, ml := newRelationManager(t)
	ml.snaps["alice"] = muteSnapshot("carol")
	ml.snaps["bob"] = muteSnapshot()

	alice := registerConn(t, m, "c1", "alice")
	bob := registerConn(t, m, "c2", "bob")
	bob.SetMuteBlockSnapshot(muteSnapshot())

	m.RefreshRelations("alice", RelationScopeMuteBlock)

	require.NotNil(t, alice.MuteBlockSnapshot())
	assert.Contains(t, alice.MuteBlockSnapshot().Muting, "carol")
	assert.Empty(t, bob.MuteBlockSnapshot().Muting, "他 user の snapshot は触らない")
}

// 同じ user が複数タブで繋いでいる場合、全 connection が更新される。
// 片方だけ更新すると「タブによって流れたり流れなかったり」になる。
func TestManager_RefreshRelations_UpdatesAllConnectionsOfUser(t *testing.T) {
	m, _, _, ml := newRelationManager(t)
	ml.snaps["alice"] = muteSnapshot("carol")

	c1 := registerConn(t, m, "c1", "alice")
	c2 := registerConn(t, m, "c2", "alice")

	m.RefreshRelations("alice", RelationScopeMuteBlock)

	assert.Contains(t, c1.MuteBlockSnapshot().Muting, "carol")
	assert.Contains(t, c2.MuteBlockSnapshot().Muting, "carol")
}

// scope で lookup を呼び分ける。無駄な DB 往復を避けるため。
func TestManager_RefreshRelations_ScopeSelectsLookup(t *testing.T) {
	tests := []struct {
		name          string
		scope         RelationScope
		wantFollowing int
		wantMuteBlock int
	}{
		{"following のみ", RelationScopeFollowing, 1, 0},
		{"muteblock のみ", RelationScopeMuteBlock, 0, 1},
		{"all は両方", RelationScopeAll, 1, 1},
		// 未知 scope は取りこぼさない方 (= 両方) へ倒す。
		{"未知の scope は両方", RelationScope("future"), 1, 1},
		{"空 scope は両方", RelationScope(""), 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, fl, ml := newRelationManager(t)
			registerConn(t, m, "c1", "alice")

			m.RefreshRelations("alice", tt.scope)

			assert.Equal(t, tt.wantFollowing, fl.calls, "following lookup 呼び出し回数")
			assert.Equal(t, tt.wantMuteBlock, ml.calls, "muteblock lookup 呼び出し回数")
		})
	}
}

// 同じ event が重複して届いても壊れない (冪等)。pubsub は at-least-once。
func TestManager_RefreshRelations_Idempotent(t *testing.T) {
	m, _, _, ml := newRelationManager(t)
	ml.snaps["alice"] = muteSnapshot("carol")
	conn := registerConn(t, m, "c1", "alice")

	for i := 0; i < 3; i++ {
		m.RefreshRelations("alice", RelationScopeMuteBlock)
	}

	assert.Contains(t, conn.MuteBlockSnapshot().Muting, "carol")
	assert.Len(t, conn.MuteBlockSnapshot().Muting, 1)
}

// 接続が無い user の event では lookup を呼ばない。全 user 分の reload が
// 飛んでくる構成 (= 複数 worker) で無駄な DB 往復を撃たないため。
func TestManager_RefreshRelations_NoConnectionSkipsLookup(t *testing.T) {
	m, _, fl, ml := newRelationManager(t)

	m.RefreshRelations("nobody", RelationScopeAll)

	assert.Zero(t, fl.calls)
	assert.Zero(t, ml.calls)
}

// lookup 失敗 (nil) は fail-open のまま。#2400 で維持を確定した挙動。
func TestManager_RefreshRelations_NilSnapshotIsFailOpen(t *testing.T) {
	m, _, _, ml := newRelationManager(t)
	conn := registerConn(t, m, "c1", "alice")
	conn.SetMuteBlockSnapshot(muteSnapshot("carol"))
	// lookup は alice の entry を持たないので nil を返す。
	m.RefreshRelations("alice", RelationScopeMuteBlock)

	assert.Nil(t, conn.MuteBlockSnapshot(), "nil = filter しない (fail-open)")
	assert.Equal(t, 1, ml.calls)
}

func TestManager_RefreshRelations_EmptyUserIDIsNoOp(t *testing.T) {
	m, _, fl, ml := newRelationManager(t)
	registerConn(t, m, "c1", "alice")

	m.RefreshRelations("", RelationScopeAll)

	assert.Zero(t, fl.calls)
	assert.Zero(t, ml.calls)
}

// bus 未配線でも panic しない (既存構成が壊れない)。
func TestManager_SubscribeRelationReload_NilBus(t *testing.T) {
	m := NewManager(NewRegistry(), nil)
	assert.NotPanics(t, m.SubscribeRelationReload)
	assert.NotPanics(t, m.UnsubscribeRelationReload)
}

func TestManager_SubscribeRelationReload_Unsubscribe(t *testing.T) {
	m, bus, _, _ := newRelationManager(t)
	m.SubscribeRelationReload()
	require.Contains(t, bus.subs, RelationReloadTopic)

	m.UnsubscribeRelationReload()
	assert.NotContains(t, bus.subs, RelationReloadTopic)
}

// 壊れた payload / userId 欠落は無視する。
func TestManager_SubscribeRelationReload_BadPayload(t *testing.T) {
	m, bus, fl, ml := newRelationManager(t)
	registerConn(t, m, "c1", "alice")
	m.SubscribeRelationReload()

	assert.NotPanics(t, func() {
		bus.deliver(RelationReloadTopic, []byte("not json"))
		bus.deliver(RelationReloadTopic, []byte(`{"scope":"all"}`))
	})
	assert.Zero(t, fl.calls)
	assert.Zero(t, ml.calls)
}
