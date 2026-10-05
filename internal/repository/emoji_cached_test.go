package repository_test

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingEmojiRepo wraps a slice-based fake EmojiRepository so we can both
// observe call counts and stub mutation paths. Mock-style fake — keeps the
// test focused on the wrapper's caching semantics.
type countingEmojiRepo struct {
	listLocalCalls atomic.Int64
	emojis         []*model.Emoji
	listErr        error
	createErr      error
	updateErr      error
	deleteErr      error
}

func (c *countingEmojiRepo) ListLocal() ([]*model.Emoji, error) {
	c.listLocalCalls.Add(1)
	if c.listErr != nil {
		return nil, c.listErr
	}
	out := make([]*model.Emoji, len(c.emojis))
	copy(out, c.emojis)
	return out, nil
}

func (c *countingEmojiRepo) Create(e *model.Emoji) error {
	if c.createErr != nil {
		return c.createErr
	}
	c.emojis = append(c.emojis, e)
	return nil
}

func (c *countingEmojiRepo) UpdateFields(_ string, _ map[string]any) error { return c.updateErr }
func (c *countingEmojiRepo) UpdateFieldsMany(_ []string, _ map[string]any) error {
	return c.updateErr
}
func (c *countingEmojiRepo) Delete(_ string) error       { return c.deleteErr }
func (c *countingEmojiRepo) DeleteMany(_ []string) error { return c.deleteErr }

func (c *countingEmojiRepo) FindByNameAndHost(_ string, _ *string) (*model.Emoji, error) {
	return nil, nil
}
func (c *countingEmojiRepo) FindByID(_ string) (*model.Emoji, error)          { return nil, nil }
func (c *countingEmojiRepo) FindManyByIDs(_ []string) ([]*model.Emoji, error) { return nil, nil }
func (c *countingEmojiRepo) FindManyByNamesAndHost(_ []string, _ *string) ([]*model.Emoji, error) {
	return nil, nil
}
func (c *countingEmojiRepo) FindManyByKeys(_ []model.EmojiKey) ([]*model.Emoji, error) {
	return nil, nil
}
func (c *countingEmojiRepo) ListWithFilter(_, _ string, _ bool, _, _ string, _, _ int) ([]*model.Emoji, error) {
	return nil, nil
}
func (c *countingEmojiRepo) ListRemoteWithFilter(_, _, _, _ string, _, _ int) ([]*model.Emoji, error) {
	return nil, nil
}
func (c *countingEmojiRepo) ListV2(_ model.EmojiV2Filter) ([]*model.Emoji, error) { return nil, nil }
func (c *countingEmojiRepo) CountV2(_ model.EmojiV2Filter) (int64, error)         { return 0, nil }

func TestCachedEmojiRepository_ListLocalHitsInnerOnce(t *testing.T) {
	inner := &countingEmojiRepo{
		emojis: []*model.Emoji{{ID: "e1", Name: "smile"}, {ID: "e2", Name: "wave"}},
	}
	cached := repository.NewCachedEmojiRepository(inner)

	for i := 0; i < 10; i++ {
		got, err := cached.ListLocal()
		require.NoError(t, err)
		require.Len(t, got, 2)
	}
	assert.Equal(t, int64(1), inner.listLocalCalls.Load(),
		"inner ListLocal must be called exactly once for 10 cached calls")
}

func TestCachedEmojiRepository_ListLocalRefreshesAfterTTL(t *testing.T) {
	inner := &countingEmojiRepo{emojis: []*model.Emoji{{ID: "e1"}}}
	cached := repository.NewCachedEmojiRepositoryWithTTL(inner, 1*time.Millisecond)

	_, err := cached.ListLocal()
	require.NoError(t, err)
	time.Sleep(2 * time.Millisecond)
	_, err = cached.ListLocal()
	require.NoError(t, err)
	assert.Equal(t, int64(2), inner.listLocalCalls.Load(),
		"second call after TTL must hit inner again")
}

func TestCachedEmojiRepository_CreateInvalidatesCache(t *testing.T) {
	inner := &countingEmojiRepo{emojis: []*model.Emoji{{ID: "e1"}}}
	cached := repository.NewCachedEmojiRepository(inner)

	got, _ := cached.ListLocal()
	assert.Len(t, got, 1)

	require.NoError(t, cached.Create(&model.Emoji{ID: "e2"}))

	got, _ = cached.ListLocal()
	assert.Len(t, got, 2, "create should have invalidated the cache and refetched")
	assert.Equal(t, int64(2), inner.listLocalCalls.Load())
}

func TestCachedEmojiRepository_DeleteInvalidatesCache(t *testing.T) {
	inner := &countingEmojiRepo{emojis: []*model.Emoji{{ID: "e1"}}}
	cached := repository.NewCachedEmojiRepository(inner)

	_, _ = cached.ListLocal() // warm
	require.NoError(t, cached.Delete("e1"))
	_, _ = cached.ListLocal()
	assert.Equal(t, int64(2), inner.listLocalCalls.Load(),
		"delete must invalidate cache so next ListLocal hits DB")
}

func TestCachedEmojiRepository_UpdateInvalidatesCache(t *testing.T) {
	inner := &countingEmojiRepo{emojis: []*model.Emoji{{ID: "e1"}}}
	cached := repository.NewCachedEmojiRepository(inner)

	_, _ = cached.ListLocal()
	require.NoError(t, cached.UpdateFields("e1", map[string]any{"category": "x"}))
	_, _ = cached.ListLocal()
	require.NoError(t, cached.UpdateFieldsMany([]string{"e1"}, map[string]any{"category": "y"}))
	_, _ = cached.ListLocal()
	require.NoError(t, cached.DeleteMany([]string{"e1"}))
	_, _ = cached.ListLocal()
	// 1 (warm) + 1 (after Update) + 1 (after UpdateMany) + 1 (after DeleteMany) = 4
	assert.Equal(t, int64(4), inner.listLocalCalls.Load(),
		"every mutating method should invalidate cache")
}

// 失敗した mutation はキャッシュを invalidate しない (DB 状態が変わらないため)。
func TestCachedEmojiRepository_FailedMutationDoesNotInvalidate(t *testing.T) {
	inner := &countingEmojiRepo{
		emojis:    []*model.Emoji{{ID: "e1"}},
		createErr: errors.New("db down"),
	}
	cached := repository.NewCachedEmojiRepository(inner)

	_, _ = cached.ListLocal() // warm
	err := cached.Create(&model.Emoji{ID: "e2"})
	assert.Error(t, err)
	_, _ = cached.ListLocal()
	assert.Equal(t, int64(1), inner.listLocalCalls.Load(),
		"failed Create must not invalidate cache")
}

func TestCachedEmojiRepository_ListLocalErrorIsNotCached(t *testing.T) {
	inner := &countingEmojiRepo{listErr: errors.New("db down")}
	cached := repository.NewCachedEmojiRepository(inner)

	_, err := cached.ListLocal()
	require.Error(t, err)
	_, err = cached.ListLocal()
	require.Error(t, err)
	assert.Equal(t, int64(2), inner.listLocalCalls.Load(),
		"errors must not be cached so transient failures recover on next call")
}

// 空 emoji table のとき inner.ListLocal() は (nil, nil) を返すが、それでも
// cache が効くべき。`c.local != nil` で判定すると毎回 DB を叩くバグに
// なるので `c.at.IsZero()` で判定する (Devin #541 BUG-1)。
func TestCachedEmojiRepository_EmptyResultIsCached(t *testing.T) {
	inner := &countingEmojiRepo{emojis: nil}
	cached := repository.NewCachedEmojiRepository(inner)

	for i := 0; i < 5; i++ {
		got, err := cached.ListLocal()
		require.NoError(t, err)
		require.Empty(t, got)
	}
	assert.Equal(t, int64(1), inner.listLocalCalls.Load(),
		"empty emoji table must still be cached; only the first call should hit inner")
}

func TestCachedEmojiRepository_InvalidatePublic(t *testing.T) {
	inner := &countingEmojiRepo{emojis: []*model.Emoji{{ID: "e1"}}}
	cached := repository.NewCachedEmojiRepositoryWithTTL(inner, time.Hour)

	_, _ = cached.ListLocal()
	cached.Invalidate()
	_, _ = cached.ListLocal()
	assert.Equal(t, int64(2), inner.listLocalCalls.Load(),
		"explicit Invalidate() should drop the cache")
}

// #739: 単純 delegate read-only methods の coverage 補完。inner mock を
// counting せずに通すだけで wrapper が panic / wrap せず素通りすること。
func TestCachedEmojiRepository_ReadOnlyDelegations(t *testing.T) {
	inner := &countingEmojiRepo{emojis: []*model.Emoji{{ID: "e1", Name: "smile"}}}
	cached := repository.NewCachedEmojiRepository(inner)

	_, _ = cached.FindByNameAndHost("smile", nil)
	_, _ = cached.FindByID("e1")
	_, _ = cached.FindManyByIDs([]string{"e1"})
	_, _ = cached.FindManyByNamesAndHost([]string{"smile"}, nil)
	_, _ = cached.ListWithFilter("", "", true, "", "", 10, 0)
	_, _ = cached.ListRemoteWithFilter("", "remote.example", "", "", 10, 0)
	_, _ = cached.ListV2(model.EmojiV2Filter{})
	_, _ = cached.CountV2(model.EmojiV2Filter{})
}
