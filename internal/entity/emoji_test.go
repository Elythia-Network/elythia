package entity

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

func TestPackEmojiDetailed_URLFromPublicURL(t *testing.T) {
	e := &model.Emoji{
		ID:          "e1",
		Name:        "smile",
		PublicURL:   "https://example.com/smile.webp",
		OriginalURL: "https://example.com/smile-orig.png",
	}
	d := PackEmojiDetailed(e)
	assert.Equal(t, "https://example.com/smile.webp", d.URL)
}

func TestPackEmojiDetailed_URLFallbackToOriginalURL(t *testing.T) {
	e := &model.Emoji{
		ID:          "e2",
		Name:        "wave",
		PublicURL:   "",
		OriginalURL: "https://example.com/wave-orig.png",
	}
	d := PackEmojiDetailed(e)
	assert.Equal(t, "https://example.com/wave-orig.png", d.URL)
}

func TestPackEmojiDetailed_AllFields(t *testing.T) {
	cat := "faces"
	lic := "CC0"
	e := &model.Emoji{
		ID:                                      "e3",
		Name:                                    "grin",
		Category:                                &cat,
		PublicURL:                               "https://example.com/grin.webp",
		OriginalURL:                             "https://example.com/grin.png",
		License:                                 &lic,
		IsSensitive:                             true,
		LocalOnly:                               true,
		Aliases:                                 model.StringArray{"happy", "joy"},
		RoleIDsThatCanBeUsedThisEmojiAsReaction: model.StringArray{"role1"},
	}
	d := PackEmojiDetailed(e)
	assert.Equal(t, "e3", d.ID)
	assert.Equal(t, "grin", d.Name)
	assert.Equal(t, &cat, d.Category)
	assert.Nil(t, d.Host)
	assert.Equal(t, "https://example.com/grin.webp", d.URL)
	assert.Equal(t, &lic, d.License)
	assert.True(t, d.IsSensitive)
	assert.True(t, d.LocalOnly)
	assert.Equal(t, []string{"happy", "joy"}, d.Aliases)
	assert.Equal(t, []string{"role1"}, d.RoleIDsThatCanBeUsedThisEmojiAsReaction)
}

func TestPackEmojiDetailed_NilSlicesReturnEmpty(t *testing.T) {
	e := &model.Emoji{
		ID:          "e4",
		Name:        "test",
		OriginalURL: "https://example.com/test.png",
	}
	d := PackEmojiDetailed(e)
	assert.NotNil(t, d.Aliases, "aliases should be empty slice, not nil")
	assert.NotNil(t, d.RoleIDsThatCanBeUsedThisEmojiAsReaction, "roleIds should be empty slice, not nil")
	assert.Len(t, d.Aliases, 0)
	assert.Len(t, d.RoleIDsThatCanBeUsedThisEmojiAsReaction, 0)
}

func TestPackEmojiDetailedList(t *testing.T) {
	emojis := []*model.Emoji{
		{ID: "e1", Name: "a", PublicURL: "https://example.com/a.png"},
		{ID: "e2", Name: "b", OriginalURL: "https://example.com/b.png"},
	}
	list := PackEmojiDetailedList(emojis)
	assert.Len(t, list, 2)
	assert.Equal(t, "https://example.com/a.png", list[0].URL)
	assert.Equal(t, "https://example.com/b.png", list[1].URL)
}

func TestPackEmojiDetailedList_Empty(t *testing.T) {
	list := PackEmojiDetailedList([]*model.Emoji{})
	assert.NotNil(t, list)
	assert.Len(t, list, 0)
}

func TestPackEmojiDetailed_DoesNotMutateInput(t *testing.T) {
	e := &model.Emoji{
		ID:                                      "e5",
		Name:                                    "safe",
		Aliases:                                 model.StringArray{"orig"},
		RoleIDsThatCanBeUsedThisEmojiAsReaction: model.StringArray{"r1"},
		OriginalURL:                             "https://example.com/safe.png",
	}
	d := PackEmojiDetailed(e)
	d.Aliases[0] = "mutated"
	d.RoleIDsThatCanBeUsedThisEmojiAsReaction[0] = "mutated"
	assert.Equal(t, "orig", e.Aliases[0], "original aliases should not be mutated")
	assert.Equal(t, "r1", e.RoleIDsThatCanBeUsedThisEmojiAsReaction[0], "original roleIds should not be mutated")
}
