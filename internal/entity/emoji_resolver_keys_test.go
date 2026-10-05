package entity

import (
	"errors"
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keyedEmojiLookup implements both EmojiLookup and EmojiKeyLookup. The
// per-host method must not be used when the keyed one is available.
type keyedEmojiLookup struct {
	stubEmojiLookup
	rows      []*model.Emoji
	err       error
	keyCalls  [][]model.EmojiKey
	hostCalls int
}

func (k *keyedEmojiLookup) FindManyByNamesAndHost(names []string, host *string) ([]*model.Emoji, error) {
	k.hostCalls++
	return k.stubEmojiLookup.FindManyByNamesAndHost(names, host)
}

func (k *keyedEmojiLookup) FindManyByKeys(keys []model.EmojiKey) ([]*model.Emoji, error) {
	k.keyCalls = append(k.keyCalls, keys)
	return k.rows, k.err
}

// #3383: 複数 host の絵文字を 1 回で引き、結果は host 別の経路と同じになる。
func TestNewEmojiResolver_UsesKeyLookupAcrossHosts(t *testing.T) {
	hostA, hostB := "alpha.example", "beta.example"
	lookup := &keyedEmojiLookup{rows: []*model.Emoji{
		{ID: "e1", Name: "smile", Host: &hostA, PublicURL: "https://alpha.example/smile.webp", OriginalURL: "https://alpha.example/smile.png"},
		{ID: "e2", Name: "smile", Host: &hostB, OriginalURL: "https://beta.example/smile.png"},
		{ID: "e3", Name: "wave", Host: &hostB, PublicURL: "https://beta.example/wave.webp", OriginalURL: "https://beta.example/wave.png"},
	}}
	notes := []*model.Note{
		{ID: "n1", UserHost: &hostA, Emojis: model.StringArray{"smile"}},
		{ID: "n2", UserHost: &hostB, Emojis: model.StringArray{"smile"},
			Reactions: []byte(`{":wave@beta.example:":1}`),
			User:      &model.User{Host: &hostB, Emojis: model.StringArray{"wave"}}},
	}

	r := NewEmojiResolver(lookup, notes)

	require.Len(t, lookup.keyCalls, 1, "every host is resolved in one call")
	assert.Zero(t, lookup.hostCalls, "the per-host lookup is not used")
	assert.ElementsMatch(t, []model.EmojiKey{
		{Name: "smile", Host: hostA},
		{Name: "smile", Host: hostB},
		{Name: "wave", Host: hostB},
	}, lookup.keyCalls[0])

	a := &NoteEntity{Emojis: &map[string]string{}}
	r.PopulateNoteEmojis(notes[0], a)
	assert.Equal(t, map[string]string{"smile": "https://alpha.example/smile.webp"}, *a.Emojis)

	b := &NoteEntity{Emojis: &map[string]string{}}
	r.PopulateNoteEmojis(notes[1], b)
	r.PopulateNoteReactionEmojis(notes[1], b)
	assert.Equal(t, map[string]string{"smile": "https://beta.example/smile.png"}, *b.Emojis, "OriginalURL is the fallback")
	assert.Equal(t, map[string]string{"wave@beta.example": "https://beta.example/wave.webp"}, b.ReactionEmojis)

	lite := &UserLite{}
	r.PopulateUserEmojis(notes[1].User, lite)
	assert.Equal(t, map[string]string{"wave": "https://beta.example/wave.webp"}, lite.Emojis)
}

// エラーでも返った行 (キャッシュに載っていた分) は使う。
func TestNewEmojiResolver_KeyLookupPartialResultOnError(t *testing.T) {
	host := "alpha.example"
	lookup := &keyedEmojiLookup{
		rows: []*model.Emoji{{ID: "e1", Name: "smile", Host: &host, OriginalURL: "https://alpha.example/smile.png"}},
		err:  errors.New("db down"),
	}
	note := &model.Note{ID: "n1", UserHost: &host, Emojis: model.StringArray{"smile", "wave"}}

	r := NewEmojiResolver(lookup, []*model.Note{note})
	e := &NoteEntity{Emojis: &map[string]string{}}
	r.PopulateNoteEmojis(note, e)
	assert.Equal(t, map[string]string{"smile": "https://alpha.example/smile.png"}, *e.Emojis)
}

func TestNewEmojiResolver_KeyLookupNotCalledWithoutEmojis(t *testing.T) {
	lookup := &keyedEmojiLookup{}
	NewEmojiResolver(lookup, []*model.Note{{ID: "n1"}})
	assert.Empty(t, lookup.keyCalls)
}
