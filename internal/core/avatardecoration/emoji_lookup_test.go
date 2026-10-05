package avatardecoration

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// fakeEmojiSource is a controllable EmojiSource.
type fakeEmojiSource struct {
	rows  []*model.Emoji
	err   error
	calls int
}

func (f *fakeEmojiSource) ListLocal() ([]*model.Emoji, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func strptr(s string) *string { return &s }

func TestEmojiResolver_ResolvesLocalNonSensitive(t *testing.T) {
	src := &fakeEmojiSource{rows: []*model.Emoji{
		{ID: "e1", Name: "party", PublicURL: "https://e/party.webp"},
	}}
	r := NewEmojiResolver(src)

	url, name, ok := r.LookupEmojiDecoration("e1")
	require.True(t, ok)
	assert.Equal(t, "https://e/party.webp", url)
	assert.Equal(t, "party", name)
}

// **センシティブな絵文字は載せない。** ここが表示側の唯一の判定なので、
// 外すと「後からセンシティブにしても既に装着済みのアイコンには残る」に戻る。
func TestEmojiResolver_ExcludesSensitive(t *testing.T) {
	src := &fakeEmojiSource{rows: []*model.Emoji{
		{ID: "e1", Name: "nsfw", PublicURL: "https://e/nsfw.webp", IsSensitive: true},
		{ID: "e2", Name: "ok", PublicURL: "https://e/ok.webp"},
	}}
	r := NewEmojiResolver(src)

	_, _, ok := r.LookupEmojiDecoration("e1")
	assert.False(t, ok, "センシティブな絵文字が解決できてしまう")
	_, _, ok = r.LookupEmojiDecoration("e2")
	assert.True(t, ok)
}

// ListLocal は host IS NULL で引くが、述語は resolver 側にも置いてある。
func TestEmojiResolver_ExcludesRemote(t *testing.T) {
	src := &fakeEmojiSource{rows: []*model.Emoji{
		{ID: "e1", Name: "remote", PublicURL: "https://e/r.webp", Host: strptr("other.example")},
	}}
	r := NewEmojiResolver(src)

	_, _, ok := r.LookupEmojiDecoration("e1")
	assert.False(t, ok)
}

func TestEmojiResolver_FallsBackToOriginalURL(t *testing.T) {
	src := &fakeEmojiSource{rows: []*model.Emoji{
		{ID: "e1", Name: "party", OriginalURL: "https://e/orig.png"},
	}}
	r := NewEmojiResolver(src)

	url, _, ok := r.LookupEmojiDecoration("e1")
	require.True(t, ok)
	assert.Equal(t, "https://e/orig.png", url)
}

func TestEmojiResolver_NotFound(t *testing.T) {
	r := NewEmojiResolver(&fakeEmojiSource{})
	_, _, ok := r.LookupEmojiDecoration("missing")
	assert.False(t, ok)
}

func TestEmojiResolver_NilSafe(t *testing.T) {
	var r *EmojiResolver
	_, _, ok := r.LookupEmojiDecoration("x")
	assert.False(t, ok)

	r = NewEmojiResolver(nil)
	_, _, ok = r.LookupEmojiDecoration("x")
	assert.False(t, ok)

	_, _, ok = NewEmojiResolver(&fakeEmojiSource{}).LookupEmojiDecoration("")
	assert.False(t, ok)
}

// TTL 内は DB を引き直さない (timeline hot path で PackUserLite x N が走る)。
func TestEmojiResolver_CachesWithinTTL(t *testing.T) {
	src := &fakeEmojiSource{rows: []*model.Emoji{{ID: "e1", Name: "a", PublicURL: "u"}}}
	r := NewEmojiResolver(src)

	for i := 0; i < 5; i++ {
		_, _, _ = r.LookupEmojiDecoration("e1")
	}
	assert.Equal(t, 1, src.calls)
}

// Invalidate すると次で引き直す (#2258 と同じ理由: 作った直後に装着できるように)。
func TestEmojiResolver_InvalidateForcesRefresh(t *testing.T) {
	src := &fakeEmojiSource{}
	r := NewEmojiResolver(src)
	_, _, ok := r.LookupEmojiDecoration("e1")
	require.False(t, ok)
	require.Equal(t, 1, src.calls)

	src.rows = []*model.Emoji{{ID: "e1", Name: "new", PublicURL: "u"}}
	r.Invalidate()

	_, name, ok := r.LookupEmojiDecoration("e1")
	assert.True(t, ok)
	assert.Equal(t, "new", name)
	assert.Equal(t, 2, src.calls)

	var nilResolver *EmojiResolver
	nilResolver.Invalidate() // nil-safe
}

// 失敗時は前回の map を残し、backoff のあいだ再試行しない (retry storm 回避)。
func TestEmojiResolver_KeepsPreviousMapOnFailure(t *testing.T) {
	src := &fakeEmojiSource{rows: []*model.Emoji{{ID: "e1", Name: "a", PublicURL: "u"}}}
	r := NewEmojiResolver(src)
	_, _, ok := r.LookupEmojiDecoration("e1")
	require.True(t, ok)

	src.err = errors.New("db down")
	// TTL を強制的に切らして refresh を起こす。
	r.mu.Lock()
	r.loaded = time.Now().Add(-2 * cacheTTL)
	r.mu.Unlock()

	_, name, ok := r.LookupEmojiDecoration("e1")
	assert.True(t, ok, "障害中に全プロフィールからデコレーションが消えてはいけない")
	assert.Equal(t, "a", name)

	before := src.calls
	_, _, _ = r.LookupEmojiDecoration("e1")
	assert.Equal(t, before, src.calls, "backoff 中は再試行しない")
}

// **初回の失敗では何も出さない (fail-closed)。** 「センシティブでないと確認できた
// もの」しか載せない設計なので、確認できない状態では出さない。
func TestEmojiResolver_FailClosedOnFirstFailure(t *testing.T) {
	r := NewEmojiResolver(&fakeEmojiSource{err: errors.New("db down")})
	_, _, ok := r.LookupEmojiDecoration("e1")
	assert.False(t, ok)
}

func TestEmojiUsableAsDecoration(t *testing.T) {
	assert.False(t, EmojiUsableAsDecoration(nil))
	assert.True(t, EmojiUsableAsDecoration(&model.Emoji{ID: "e1"}))
	assert.False(t, EmojiUsableAsDecoration(&model.Emoji{ID: "e1", IsSensitive: true}))
	assert.False(t, EmojiUsableAsDecoration(&model.Emoji{ID: "e1", Host: strptr("x.example")}))
	// **空文字列の host もリモート扱い (fail-closed)。** ローカルは NULL で
	// 表すのが upstream から続く約束なので、非 nil を通す形にすると lookup の
	// 条件が緩んだときにリモート絵文字が装着できてしまう。
	assert.False(t, EmojiUsableAsDecoration(&model.Emoji{ID: "e1", Host: strptr("")}))
}

func TestEmojiDecorationURL(t *testing.T) {
	assert.Equal(t, "", EmojiDecorationURL(nil))
	assert.Equal(t, "pub", EmojiDecorationURL(&model.Emoji{PublicURL: "pub", OriginalURL: "orig"}))
	assert.Equal(t, "orig", EmojiDecorationURL(&model.Emoji{OriginalURL: "orig"}))
}

// nil 行が混ざっても落ちない (repo の実装差で起きうる)。
func TestEmojiResolver_SkipsNilRows(t *testing.T) {
	src := &fakeEmojiSource{rows: []*model.Emoji{nil, {ID: "e1", Name: "a", PublicURL: "u"}}}
	r := NewEmojiResolver(src)
	_, _, ok := r.LookupEmojiDecoration("e1")
	assert.True(t, ok)
}
