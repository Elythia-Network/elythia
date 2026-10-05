package channels

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/elythia-network/elythia/internal/stream"

	"github.com/stretchr/testify/require"
)

// **購読するトピック数に上限があること。**
//
// mk-go は distinct なタグごとに Redis の SUBSCRIBE を張るので、1 通の
// `connect` で Redis 接続を任意個増やせる。本番 valkey の `maxclients` に
// 達すると、同じ valkey を使う job queue / timelines / reactions を含めて
// インスタンス全体が新規接続を拒否する。
func TestCountDistinctTags(t *testing.T) {
	t.Parallel()

	require.Equal(t, 0, countDistinctTags(nil))
	require.Equal(t, 1, countDistinctTags([][]string{{"a"}}))
	require.Equal(t, 1, countDistinctTags([][]string{{"a"}, {"a"}}), "重複は 1 つと数える")
	require.Equal(t, 2, countDistinctTags([][]string{{"a", "b"}}))
	require.Equal(t, 0, countDistinctTags([][]string{{""}}), "正規化して空になるものは購読しない")

	// 上限はリテラルで書く (定数を参照すると、緩める変異と一緒に期待値が動く)。
	many := make([][]string, 0, 33)
	for i := 0; i < 33; i++ {
		many = append(many, []string{string(rune('a'+i%26)) + string(rune('0'+i/26))})
	}
	require.Greater(t, countDistinctTags(many), 32, "33 個は上限 32 を超えること")
}

// Init がタグ数の上限を通していること。
//
// **述語だけのテストでは足りない。** 呼び出しを外す変異を検出できない。
func TestHashtag_RejectsTooManyTags(t *testing.T) {
	tags := make([]string, 0, 33)
	for i := 0; i < 33; i++ {
		tags = append(tags, fmt.Sprintf("tag%02d", i))
	}
	body, err := json.Marshal(map[string]any{"q": [][]string{tags}})
	require.NoError(t, err)

	ctx := newCtx(nil)
	ch := NewHashtag(ctx)
	require.ErrorIs(t, ch.Init(json.RawMessage(body)), stream.ErrInvalidParams,
		"33 タグは拒否すること (Redis 購読がタグ数ぶん増える)")
	require.Empty(t, ctx.subs, "拒否したら 1 つも購読しないこと")
	ch.Dispose()
}

// 上限ちょうどは通ること (通る集合を狭めていない)。
func TestHashtag_AcceptsUpToLimit(t *testing.T) {
	tags := make([]string, 0, 32)
	for i := 0; i < 32; i++ {
		tags = append(tags, fmt.Sprintf("tag%02d", i))
	}
	body, err := json.Marshal(map[string]any{"q": [][]string{tags}})
	require.NoError(t, err)

	ctx := newCtx(nil)
	ch := NewHashtag(ctx)
	require.NoError(t, ch.Init(json.RawMessage(body)))
	require.Len(t, ctx.subs, 32)
	ch.Dispose()
}
