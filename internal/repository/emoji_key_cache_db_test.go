package repository_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// countEmojiQueries counts SELECTs against the emoji table on db until the
// test ends.
func countEmojiQueries(t *testing.T, db *gorm.DB) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	name := "test:count_emoji_queries:" + t.Name()
	require.NoError(t, db.Callback().Query().After("gorm:query").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table == "emoji" || strings.Contains(tx.Statement.SQL.String(), `"emoji"`) {
			n.Add(1)
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Query().Remove(name) })
	return &n
}

// 2 ページ目で同じ絵文字が出ても、絵文字を引く SQL を投げない (#3383)。
// 実 PostgreSQL に対して PackNotes を 2 回呼び、emoji への SELECT を数える。
func TestCachedEmojiRepository_PackNotesSecondPageSkipsDB(t *testing.T) {
	db := repository.SharedTestDB()
	hostA, hostB := "kc-a.example", "kc-b.example"
	rows := []*model.Emoji{
		{ID: "kc_note", Name: "kc_note_emoji", Host: &hostA, OriginalURL: "https://kc-a.example/n.png", PublicURL: "https://kc-a.example/n.webp"},
		{ID: "kc_user", Name: "kc_user_emoji", Host: &hostA, OriginalURL: "https://kc-a.example/u.png"},
		{ID: "kc_react", Name: "kc_react_emoji", Host: &hostB, OriginalURL: "https://kc-b.example/r.png"},
	}
	for _, e := range rows {
		require.NoError(t, db.Create(e).Error)
		t.Cleanup(func() { db.Exec(`DELETE FROM "emoji" WHERE id = ?`, e.ID) })
	}
	cached := repository.NewCachedEmojiRepository(repository.NewEmojiRepository(db))
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	page := func(noteID string) []*model.Note {
		return []*model.Note{{
			ID:        noteID,
			UserID:    "kc_u1",
			UserHost:  &hostA,
			Emojis:    model.StringArray{"kc_note_emoji", "kc_absent"},
			Reactions: []byte(`{":kc_react_emoji@kc-b.example:":2,"👍":1}`),
			User: &model.User{
				ID:       "kc_u1",
				Username: "alice",
				Host:     &hostA,
				Emojis:   model.StringArray{"kc_user_emoji"},
			},
		}}
	}
	queries := countEmojiQueries(t, db)

	first := entity.PackNotes(context.Background(), page("kc_n1"), idGen, nil, cached, nil)
	require.Len(t, first, 1)
	firstQueries := queries.Load()
	require.Equal(t, int64(1), firstQueries, "the first page fetches every host in one query")

	second := entity.PackNotes(context.Background(), page("kc_n2"), idGen, nil, cached, nil)
	require.Len(t, second, 1)
	assert.Equal(t, firstQueries, queries.Load(), "the second page must not query the emoji table again")

	// 解決結果はキャッシュの有無で変わらない。
	for _, got := range []entity.NoteEntity{first[0], second[0]} {
		require.NotNil(t, got.Emojis)
		assert.Equal(t, map[string]string{"kc_note_emoji": "https://kc-a.example/n.webp"}, *got.Emojis)
		assert.Equal(t, map[string]string{"kc_react_emoji@kc-b.example": "https://kc-b.example/r.png"}, got.ReactionEmojis)
		assert.Equal(t, map[string]string{"kc_user_emoji": "https://kc-a.example/u.png"}, got.User.Emojis)
	}

	// URL が変わったら、次のページは新しい URL を引き直す。
	require.NoError(t, cached.UpdateFields("kc_note", map[string]any{"publicUrl": "https://kc-a.example/n2.webp"}))
	third := entity.PackNotes(context.Background(), page("kc_n3"), idGen, nil, cached, nil)
	assert.Equal(t, firstQueries+1, queries.Load())
	assert.Equal(t, map[string]string{"kc_note_emoji": "https://kc-a.example/n2.webp"}, *third[0].Emojis)

	// 無かった絵文字が作られたら、次のページで解決される。
	created := &model.Emoji{ID: "kc_absent", Name: "kc_absent", Host: &hostA, OriginalURL: "https://kc-a.example/a.png"}
	require.NoError(t, cached.Create(created))
	t.Cleanup(func() { db.Exec(`DELETE FROM "emoji" WHERE id = ?`, created.ID) })
	fourth := entity.PackNotes(context.Background(), page("kc_n4"), idGen, nil, cached, nil)
	assert.Equal(t, "https://kc-a.example/a.png", (*fourth[0].Emojis)["kc_absent"])

	// 消したら、次のページには出ない。
	require.NoError(t, cached.Delete("kc_react"))
	fifth := entity.PackNotes(context.Background(), page("kc_n5"), idGen, nil, cached, nil)
	assert.Empty(t, fifth[0].ReactionEmojis)
}
