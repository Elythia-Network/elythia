package federation

import (
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fillMissingExtraKeys(m *missingExtraKeys, now time.Time) {
	for i := 0; m.total < missingExtraKeyMax; i++ {
		m.remember("u"+strconv.Itoa(i/missingExtraKeysPerActor), "k"+strconv.Itoa(i%missingExtraKeysPerActor), m.generation(), now)
	}
}

// 満杯のときの全件の掃除は期限に 1 回まで。毎回掃くと、署名なしの要求 1 件ごとに
// mutex を握ったまま全件を走査することになる (#3610 H-1)。
func TestMissingExtraKeys_FullSweepAtMostOncePerTTL(t *testing.T) {
	var m missingExtraKeys
	t0 := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	fillMissingExtraKeys(&m, t0)
	require.Equal(t, missingExtraKeyMax, m.total)

	// 30 秒後: 掃く (まだ何も期限切れでない) が、覚えられない。
	m.remember("new", "k", m.generation(), t0.Add(30*time.Second))
	assert.False(t, m.known("new", "k", t0.Add(30*time.Second)))
	// 61 秒後: 最初の記録は期限切れだが、前の掃除から 31 秒なので掃かない。
	m.remember("new", "k", m.generation(), t0.Add(61*time.Second))
	assert.False(t, m.known("new", "k", t0.Add(61*time.Second)), "a full table must not be swept again within the TTL")
	assert.Equal(t, missingExtraKeyMax, m.total)
	// 91 秒後: 掃いて覚える。
	m.remember("new", "k", m.generation(), t0.Add(91*time.Second))
	assert.True(t, m.known("new", "k", t0.Add(91*time.Second)))
	assert.Equal(t, 1, m.total)
}

// 覚える keyId は署名ヘッダから切り離した写し (ヘッダ全体を保持しない)。
func TestMissingExtraKeys_KeyIDIsCopied(t *testing.T) {
	var m missingExtraKeys
	header := `keyId="https://remote.example/users/alice#main-key",signature="` + strings.Repeat("A", 4096) + `"`
	keyID := header[7:50]
	m.remember("alice", keyID, m.generation(), time.Now())
	for stored := range m.byUser["alice"] {
		// assert.NotEqual はポインタの先を DeepEqual で比べるので、アドレスを直接比べる。
		assert.False(t, unsafe.StringData(keyID) == unsafe.StringData(stored), "the stored keyId must not alias the header")
	}
	require.Len(t, m.byUser["alice"], 1)
}

// 世代が変わったら覚えない。forget は件数を正しく戻す。
func TestMissingExtraKeys_GenerationAndForget(t *testing.T) {
	var m missingExtraKeys
	now := time.Now()
	gen := m.generation()
	m.remember("alice", "a", gen, now)
	m.remember("alice", "b", gen, now)
	m.forget("alice")
	assert.Zero(t, m.total)
	m.remember("alice", "c", gen, now)
	assert.False(t, m.known("alice", "c", now), "a remember with a stale generation must be ignored")
	m.remember("alice", "c", m.generation(), now)
	assert.True(t, m.known("alice", "c", now))
	assert.Equal(t, 1, m.total)
}

// 期限切れで actor の記録が空になったら、その actor の内側の map も消す。
// 残すと空の map が actor の数だけ溜まり、件数の上限 (total) に数えられない。
func TestMissingExtraKeys_EmptyActorIsDropped(t *testing.T) {
	var m missingExtraKeys
	t0 := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	m.remember("alice", "a", m.generation(), t0)
	m.remember("bob", "b", m.generation(), t0)
	require.Len(t, m.byUser, 2)

	assert.False(t, m.known("alice", "a", t0.Add(61*time.Second)))
	assert.Len(t, m.byUser, 1, "an actor whose last record expired must be dropped")
	assert.Equal(t, 1, m.total)
}

// actor ごとの上限に達していても、期限が切れた分は掃いて新しい keyId を覚える。
// 掃かないと、偽の fragment を 4 件覚えさせられた actor の `#main-key` が、
// actor を読み直すまで覚えられなくなる。
func TestMissingExtraKeys_PerActorBoundSweepsExpired(t *testing.T) {
	var m missingExtraKeys
	t0 := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	for i := 0; i < missingExtraKeysPerActor; i++ {
		m.remember("alice", "fake"+strconv.Itoa(i), m.generation(), t0)
	}
	m.remember("alice", "main-key", m.generation(), t0.Add(30*time.Second))
	require.False(t, m.known("alice", "main-key", t0.Add(30*time.Second)), "precondition: the actor is full")

	t1 := t0.Add(61 * time.Second)
	m.remember("alice", "main-key", m.generation(), t1)
	assert.True(t, m.known("alice", "main-key", t1), "expired records must make room for a new keyId")
	assert.Equal(t, 1, m.total)
	assert.Len(t, m.byUser["alice"], 1)
}

// dropAll forgets every record and advances the generation (#3732), so a
// lookup that started before it does not remember its result after.
func TestMissingExtraKeys_DropAll(t *testing.T) {
	var m missingExtraKeys
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	m.remember("u1", "k", m.generation(), now)
	m.remember("u2", "k", m.generation(), now)
	gen := m.generation()

	m.dropAll()
	assert.False(t, m.known("u1", "k", now))
	assert.False(t, m.known("u2", "k", now))
	assert.Equal(t, 0, m.total)
	m.remember("u1", "k", gen, now)
	assert.False(t, m.known("u1", "k", now), "a lookup that started before dropAll was remembered")
}

// DropLocal keeps the backoff after failed key fetches (#3732): it is not a
// cache of a value, and dropping it would only let the fetches hammer a host
// that is down.
func TestResolver_DropLocalKeepsFetchBackoff(t *testing.T) {
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	r := &Resolver{
		keys:             map[string]publicKeyEntry{"u1": {pem: "P", fetchedAt: now}},
		keyFetchFailures: map[string]time.Time{"u1": now},
	}
	r.DropLocal()
	assert.Empty(t, r.keys)
	assert.Equal(t, map[string]time.Time{"u1": now}, r.keyFetchFailures, "DropLocal dropped the fetch backoff")
}
