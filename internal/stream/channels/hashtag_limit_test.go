package channels

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/stream"

	"github.com/stretchr/testify/require"
)

// **購読するトピック数に上限があること。**
//
// Elythia は distinct なタグごとにトピックを購読するので、1 通の `connect` で
// トピックの枠を任意個増やせる。
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
		"33 タグは拒否すること (トピックがタグ数ぶん増える)")
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

// 保存できる長さ (128 文字) を超えるタグは拒否し、1 つも購読しないこと。
//
// 長いタグは他のタグより後ろに置く。途中で拒否する実装だと、前のタグを購読した
// まま返してしまう。
func TestHashtag_RejectsOverlongTag(t *testing.T) {
	long := strings.Repeat("あ", 129)
	body, err := json.Marshal(map[string]any{"q": [][]string{{"ok1", "ok2"}, {long}}})
	require.NoError(t, err)

	ctx := newCtx(nil)
	ch := NewHashtag(ctx)
	require.ErrorIs(t, ch.Init(json.RawMessage(body)), stream.ErrInvalidParams)
	require.Empty(t, ctx.subs, "拒否したら 1 つも購読しないこと")
	ch.Dispose()

	// 128 文字ちょうどは通る。
	body, err = json.Marshal(map[string]any{"q": [][]string{{strings.Repeat("あ", 128)}}})
	require.NoError(t, err)
	ctx = newCtx(nil)
	ch = NewHashtag(ctx)
	require.NoError(t, ch.Init(json.RawMessage(body)))
	require.Len(t, ctx.subs, 1)
	ch.Dispose()
}

// 長さは正規化の前の文字数で判定すること (本家 hashtag.ts と同じ)。結合文字の
// 並びは NFKC で短くなるので、正規化の後だけで数えると 128 文字を超える入力を
// 通してしまう。
func TestHashtag_JudgesLengthBeforeNormalization(t *testing.T) {
	// "e" + U+0301 は NFKC で "é" 1 文字になる。65 組 = 正規化前 130 文字、後 65 文字
	long := strings.Repeat("e\u0301", 65)
	body, err := json.Marshal(map[string]any{"q": [][]string{{long}}})
	require.NoError(t, err)

	ctx := newCtx(nil)
	ch := NewHashtag(ctx)
	require.ErrorIs(t, ch.Init(json.RawMessage(body)), stream.ErrInvalidParams)
	require.Empty(t, ctx.subs)
	ch.Dispose()

	// 正規化前 128 文字ちょうどは通る。
	body, err = json.Marshal(map[string]any{"q": [][]string{{strings.Repeat("e\u0301", 64)}}})
	require.NoError(t, err)
	ctx = newCtx(nil)
	ch = NewHashtag(ctx)
	require.NoError(t, ch.Init(json.RawMessage(body)))
	require.Len(t, ctx.subs, 1)
	ch.Dispose()

	// 正規化で 128 文字を超えても、正規化前が 128 文字以下なら通す (本家と同じ)。
	// U+337F (㍿) は NFKC で 4 文字になる。100 文字 = 正規化後 400 文字
	body, err = json.Marshal(map[string]any{"q": [][]string{{strings.Repeat("\u337f", 100)}}})
	require.NoError(t, err)
	ctx = newCtx(nil)
	ch = NewHashtag(ctx)
	require.NoError(t, ch.Init(json.RawMessage(body)))
	require.Len(t, ctx.subs, 1)
	ch.Dispose()
}

// initHashtag runs Init with q and returns the subscribed topics and the error.
func initHashtag(t *testing.T, q any) ([]string, error) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"q": q})
	require.NoError(t, err)
	ctx := newCtx(nil)
	ch := NewHashtag(ctx)
	initErr := ch.Init(json.RawMessage(body))
	subs := append([]string(nil), ctx.subs...)
	ch.Dispose()
	return subs, initErr
}

// グループの数は 32 まで (本家と同じ)。同じタグを並べたグループは異なるタグの数
// の上限に掛からないので、グループの数で別に止める。
func TestHashtag_RejectsTooManyGroups(t *testing.T) {
	// 上限はリテラルで書く (定数を参照すると、緩める変異と一緒に期待値が動く)。
	groups := make([][]string, 33)
	for i := range groups {
		groups[i] = []string{"a", "b"}
	}
	subs, err := initHashtag(t, groups)
	require.ErrorIs(t, err, stream.ErrInvalidParams, "33 groups must be rejected")
	require.Empty(t, subs, "nothing may be subscribed after a rejection")

	subs, err = initHashtag(t, groups[:32])
	require.NoError(t, err, "exactly 32 groups must be accepted")
	require.Len(t, subs, 2)
}

// 1 つのグループのタグの数は 32 まで (本家と同じ)。同じタグの繰り返しでも数える。
func TestHashtag_RejectsTooManyTagsPerGroup(t *testing.T) {
	group := make([]string, 33)
	for i := range group {
		group[i] = "a"
	}
	subs, err := initHashtag(t, [][]string{{"ok"}, group})
	require.ErrorIs(t, err, stream.ErrInvalidParams, "a group of 33 tags must be rejected")
	require.Empty(t, subs, "nothing may be subscribed after a rejection")

	subs, err = initHashtag(t, [][]string{{"ok"}, group[:32]})
	require.NoError(t, err, "exactly 32 tags in a group must be accepted")
	require.Len(t, subs, 2)
}

// 空のタグは拒否する (本家と同じ)。捨てて受け付けると、["a", ""] が ["a"] として
// 広く一致してしまう。
func TestHashtag_RejectsEmptyTag(t *testing.T) {
	subs, err := initHashtag(t, [][]string{{"a", ""}})
	require.ErrorIs(t, err, stream.ErrInvalidParams)
	require.Empty(t, subs)

	// 文字列でないタグも拒否する (本家の typeof y === 'string')。encoding/json は型の
	// 合わない要素をゼロ値 (空文字) のまま残すので、空のタグの判定で拒否される。
	subs, err = initHashtag(t, []any{[]any{"a", 1}})
	require.ErrorIs(t, err, stream.ErrInvalidParams)
	require.Empty(t, subs)
}
