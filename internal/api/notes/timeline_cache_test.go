package notes

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func boolPtr(b bool) *bool { return &b }

func TestTimelineJSONCache_GetSetTTL(t *testing.T) {
	c := newTimelineJSONCache(3 * time.Second)
	base := time.Unix(1000, 0)

	// miss before set
	_, ok := c.get("k", base)
	assert.False(t, ok)

	c.set(c.generation(), "k", []byte(`[{"id":"a"}]`), base)

	// hit within TTL
	body, ok := c.get("k", base.Add(2*time.Second))
	assert.True(t, ok)
	assert.Equal(t, `[{"id":"a"}]`, string(body))

	// miss after TTL (期限切れは lazy 削除される)
	_, ok = c.get("k", base.Add(4*time.Second))
	assert.False(t, ok)
	// 削除済みなので TTL 内 now で再 get しても miss
	_, ok = c.get("k", base.Add(2*time.Second))
	assert.False(t, ok)
}

// timelineCacheKey は出力に影響する全 param で別 key になること (= 別 viewer /
// 別条件の応答を取り違えない)。同一 param なら同一 key。
func TestTimelineCacheKey_Distinguishes(t *testing.T) {
	baseReq := TimelineRequest{Limit: intPtr(10)}
	base := timelineCacheKey("home", "u1", baseReq)

	// 同一条件 → 同一 key
	assert.Equal(t, base, timelineCacheKey("home", "u1", TimelineRequest{Limit: intPtr(10)}))

	// 差分はすべて別 key
	assert.NotEqual(t, base, timelineCacheKey("local", "u1", baseReq), "kind")
	assert.NotEqual(t, base, timelineCacheKey("home", "u2", baseReq), "viewer")
	assert.NotEqual(t, base, timelineCacheKey("home", "u1", TimelineRequest{Limit: intPtr(20)}), "limit")
	assert.NotEqual(t, base, timelineCacheKey("home", "u1", TimelineRequest{Limit: intPtr(10), WithFiles: true}), "withFiles")
	assert.NotEqual(t, base, timelineCacheKey("home", "u1", TimelineRequest{Limit: intPtr(10), AllowPartial: true}), "allowPartial")

	// pointer bool は nil / true / false で 3 通り別 key
	kNil := timelineCacheKey("home", "u1", TimelineRequest{Limit: intPtr(10), WithRenotes: nil})
	kTrue := timelineCacheKey("home", "u1", TimelineRequest{Limit: intPtr(10), WithRenotes: boolPtr(true)})
	kFalse := timelineCacheKey("home", "u1", TimelineRequest{Limit: intPtr(10), WithRenotes: boolPtr(false)})
	assert.NotEqual(t, kNil, kTrue)
	assert.NotEqual(t, kNil, kFalse)
	assert.NotEqual(t, kTrue, kFalse)
}

// 上限到達時は期限切れを掃いて bounded に保ち、それでも上限なら cache しない。
func TestTimelineJSONCache_Bounded(t *testing.T) {
	c := newTimelineJSONCache(3 * time.Second)
	c.maxEntries = 2
	base := time.Unix(1000, 0)

	c.set(c.generation(), "a", []byte("1"), base)
	c.set(c.generation(), "b", []byte("2"), base)
	// 上限到達。全 entry が未期限なので "c" は cache されない。
	c.set(c.generation(), "c", []byte("3"), base)
	_, okC := c.get("c", base)
	assert.False(t, okC, "上限到達かつ全 entry 未期限なら新規は cache しない")

	// a が期限切れになれば sweep されて空きができ、新規を入れられる。
	c.set(c.generation(), "d", []byte("4"), base.Add(4*time.Second))
	body, okD := c.get("d", base.Add(4*time.Second))
	assert.True(t, okD, "期限切れ sweep 後は空きに入る")
	assert.Equal(t, "4", string(body))
}

func intPtr(v int) *int { return &v }
