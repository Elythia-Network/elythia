package stream

import (
	"encoding/json"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubHardMuteLookup は HardMuteRulesLookup の test stub。userID 単位で
// 任意の rules を返す。
type stubHardMuteLookup struct {
	rules map[string][]byte
}

func (s *stubHardMuteLookup) HardMutedWordsForUser(userID string) []byte {
	return s.rules[userID]
}

// pubsub message を発行すると Manager が該当 user の connection の rules を
// 更新する end-to-end の検証 (#791)。
func TestManager_SubscribeWordMuteReload_RefreshesMatchingConnection(t *testing.T) {
	bus := newStubBus()
	m := NewManager(NewRegistry(), bus)
	lookup := &stubHardMuteLookup{rules: map[string][]byte{
		"alice": []byte(`["initial"]`),
	}}
	m.SetHardMuteLookup(lookup)

	// alice の connection を 1 つ登録。SetHardMuteRules で initial rules を
	// 持たせる (production の Accept 経路を模倣)。
	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	conn.SetHardMuteRules(lookup.HardMutedWordsForUser("alice"))
	m.register(conn)
	t.Cleanup(func() { m.unregister(conn.ID()) })
	require.Equal(t, `["initial"]`, string(conn.HardMuteRules()))

	// reload subscriber を起動 → bus.Subscribe が登録される。
	m.SubscribeWordMuteReload()
	require.Contains(t, bus.subs, WordMuteReloadTopic)

	// pubsub に新しい rules が反映された状態で reload payload を deliver。
	lookup.rules["alice"] = []byte(`["updated"]`)
	payload, _ := json.Marshal(WordMuteReloadPayload{UserID: "alice"})
	bus.deliver(WordMuteReloadTopic, payload)

	assert.Equal(t, `["updated"]`, string(conn.HardMuteRules()))
}

// 違う user の reload event は影響しないこと。
func TestManager_RefreshHardMuteRules_OnlyAffectsTargetUser(t *testing.T) {
	bus := newStubBus()
	m := NewManager(NewRegistry(), bus)
	lookup := &stubHardMuteLookup{rules: map[string][]byte{
		"alice": []byte(`["alice-new"]`),
		"bob":   []byte(`["bob-original"]`),
	}}
	m.SetHardMuteLookup(lookup)

	alice := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	alice.SetHardMuteRules([]byte(`["alice-original"]`))
	bob := NewConnection("c2", &model.User{ID: "bob"}, newFakeConn())
	bob.SetHardMuteRules([]byte(`["bob-original"]`))
	m.register(alice)
	m.register(bob)
	t.Cleanup(func() {
		m.unregister(alice.ID())
		m.unregister(bob.ID())
	})

	m.RefreshHardMuteRules("alice")

	assert.Equal(t, `["alice-new"]`, string(alice.HardMuteRules()))
	assert.Equal(t, `["bob-original"]`, string(bob.HardMuteRules()),
		"refresh must not touch other users' connections")
}

// hardMute lookup が未配線なら no-op。
func TestManager_RefreshHardMuteRules_NoLookupIsNoOp(t *testing.T) {
	m := NewManager(NewRegistry(), newStubBus())
	conn := NewConnection("c1", &model.User{ID: "alice"}, newFakeConn())
	conn.SetHardMuteRules([]byte(`["keep"]`))
	m.register(conn)
	t.Cleanup(func() { m.unregister(conn.ID()) })

	m.RefreshHardMuteRules("alice")
	assert.Equal(t, `["keep"]`, string(conn.HardMuteRules()))
}

// 不正な payload (壊れた JSON / userID 欠落) は warn して skip、panic しない。
func TestManager_SubscribeWordMuteReload_HandlesMalformedPayload(t *testing.T) {
	bus := newStubBus()
	m := NewManager(NewRegistry(), bus)
	m.SetHardMuteLookup(&stubHardMuteLookup{rules: map[string][]byte{}})
	m.SubscribeWordMuteReload()

	// JSON 構文エラー
	bus.deliver(WordMuteReloadTopic, []byte(`{not json`))
	// userId 欠落
	bus.deliver(WordMuteReloadTopic, []byte(`{}`))
	// 単に panic しないことを確認するだけで十分。
}

// bus が nil のときは subscribe / unsubscribe いずれも no-op。
func TestManager_SubscribeWordMuteReload_NilBus(t *testing.T) {
	m := NewManager(NewRegistry(), nil)
	// panic しないこと。
	m.SubscribeWordMuteReload()
	m.UnsubscribeWordMuteReload()
}

// Shutdown が pubsub subscriber を解放すること (= bus.Unsubscribe が呼ばれる)。
func TestManager_Shutdown_UnsubscribesWordMuteReload(t *testing.T) {
	bus := newStubBus()
	m := NewManager(NewRegistry(), bus)
	m.SubscribeWordMuteReload()
	require.Contains(t, bus.subs, WordMuteReloadTopic)

	m.Shutdown()
	assert.NotContains(t, bus.subs, WordMuteReloadTopic,
		"Shutdown must unsubscribe wordmute reload to release the goroutine")
}
