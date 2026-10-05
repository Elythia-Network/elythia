package repository

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestEmojiRepository_FindManyByKeys(t *testing.T) {
	repo := NewEmojiRepository(testDB)
	hostA, hostB := "fbk-a.example", "fbk-b.example"
	rows := []*model.Emoji{
		{ID: "fbk_local", Name: "fbk_same", OriginalURL: "https://local.example/l.png"},
		{ID: "fbk_a", Name: "fbk_same", Host: &hostA, OriginalURL: "https://a.example/a.png"},
		{ID: "fbk_b", Name: "fbk_same", Host: &hostB, OriginalURL: "https://b.example/b.png"},
		{ID: "fbk_b2", Name: "fbk_only_b", Host: &hostB, OriginalURL: "https://b.example/b2.png"},
	}
	for _, e := range rows {
		require.NoError(t, testDB.Create(e).Error)
		t.Cleanup(func() { cleanupEmoji(t, e.ID) })
	}

	idsOf := func(es []*model.Emoji) []string {
		out := make([]string, 0, len(es))
		for _, e := range es {
			out = append(out, e.ID)
		}
		return out
	}

	t.Run("pairs across hosts and local in one call", func(t *testing.T) {
		got, err := repo.FindManyByKeys([]model.EmojiKey{
			{Name: "fbk_same", Host: ""},
			{Name: "fbk_same", Host: hostA},
			{Name: "fbk_only_b", Host: hostB},
			// 名前と host の組で引く。名前だけ・host だけが一致するものは返さない。
			{Name: "fbk_only_b", Host: hostA},
		})
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"fbk_local", "fbk_a", "fbk_b2"}, idsOf(got))
	})

	t.Run("remote only", func(t *testing.T) {
		got, err := repo.FindManyByKeys([]model.EmojiKey{{Name: "fbk_same", Host: hostB}})
		require.NoError(t, err)
		assert.Equal(t, []string{"fbk_b"}, idsOf(got))
	})

	t.Run("local only", func(t *testing.T) {
		got, err := repo.FindManyByKeys([]model.EmojiKey{{Name: "fbk_same"}})
		require.NoError(t, err)
		assert.Equal(t, []string{"fbk_local"}, idsOf(got))
	})

	t.Run("unstorable keys do not fail the query", func(t *testing.T) {
		got, err := repo.FindManyByKeys([]model.EmojiKey{
			{Name: "bad\x00", Host: hostA},
			{Name: "fbk_same", Host: "bad\x00host"},
			{Name: "fbk_same", Host: hostA},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"fbk_a"}, idsOf(got))

		got, err = repo.FindManyByKeys([]model.EmojiKey{{Name: "bad\x00"}})
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("more keys than one chunk", func(t *testing.T) {
		keys := make([]model.EmojiKey, 0, 1200)
		for i := 0; i < 1200; i++ {
			keys = append(keys, model.EmojiKey{Name: fmt.Sprintf("fbk_absent_%d", i), Host: hostA})
		}
		// 先頭と末尾 (別の chunk) に実在するキーを置く。
		keys[0] = model.EmojiKey{Name: "fbk_same", Host: hostA}
		keys[len(keys)-1] = model.EmojiKey{Name: "fbk_same"}
		got, err := repo.FindManyByKeys(keys)
		require.NoError(t, err)
		assert.ElementsMatch(t, []string{"fbk_a", "fbk_local"}, idsOf(got))
	})

	t.Run("a failing chunk does not stop the rest", func(t *testing.T) {
		// 最初の chunk の SELECT だけを失敗させる。
		var calls atomic.Int64
		name := "test:fail_first_emoji_chunk"
		require.NoError(t, testDB.Callback().Query().Before("gorm:query").Register(name, func(tx *gorm.DB) {
			if tx.Statement.Table == "emoji" && calls.Add(1) == 1 {
				_ = tx.AddError(errors.New("injected chunk failure"))
			}
		}))
		t.Cleanup(func() { _ = testDB.Callback().Query().Remove(name) })

		keys := make([]model.EmojiKey, 0, 1000)
		for i := 0; i < 1000; i++ {
			keys = append(keys, model.EmojiKey{Name: fmt.Sprintf("fbk_absent_%d", i), Host: hostA})
		}
		keys[0] = model.EmojiKey{Name: "fbk_same", Host: hostB}           // 落ちる chunk
		keys[len(keys)-1] = model.EmojiKey{Name: "fbk_same", Host: hostA} // 後ろの chunk
		got, err := repo.FindManyByKeys(keys)
		require.Error(t, err, "the first error is reported")
		assert.Equal(t, []string{"fbk_a"}, idsOf(got), "rows of the later chunk are still returned")
		assert.Equal(t, int64(2), calls.Load())
	})

	t.Run("empty", func(t *testing.T) {
		got, err := repo.FindManyByKeys(nil)
		require.NoError(t, err)
		assert.Empty(t, got)
	})
}
