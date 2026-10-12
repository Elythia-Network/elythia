package entity

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 取り込みで確かめる前に保存された、形の崩れたリモートの絵文字の URL は、
// ノートの emojis / reactionEmojis / 利用者の emojis に載せない。括弧などは
// 符号化した値を返す。host 別に引く経路と、まとめて引く経路の両方で確かめる。
func TestEmojiResolver_RemoteEmojiURLForm(t *testing.T) {
	host := "remote.example"
	rows := []*model.Emoji{
		{ID: "e1", Name: "bad", Host: &host, PublicURL: "https:///remote.example/bad.png"},
		{ID: "e2", Name: "rel", Host: &host, PublicURL: "", OriginalURL: "/rel.png"},
		{ID: "e3", Name: "paren", Host: &host, PublicURL: "https://remote.example/p (1).png"},
	}
	note := &model.Note{ID: "n1", UserHost: &host, Emojis: model.StringArray{"bad", "rel", "paren"},
		Reactions: []byte(`{":bad@remote.example:":1,":paren@remote.example:":1}`),
		User:      &model.User{Host: &host, Emojis: model.StringArray{"bad", "paren"}}}
	encoded := "https://remote.example/p%20%281%29.png"

	for name, lookup := range map[string]EmojiLookup{
		"per host": &stubEmojiLookup{data: map[string][]*model.Emoji{host: rows}},
		"keyed":    &keyedEmojiLookup{rows: rows},
	} {
		t.Run(name, func(t *testing.T) {
			r := NewEmojiResolver(lookup, []*model.Note{note})
			e := &NoteEntity{Emojis: &map[string]string{}}
			r.PopulateNoteEmojis(note, e)
			assert.Equal(t, map[string]string{"paren": encoded}, *e.Emojis)
			r.PopulateNoteReactionEmojis(note, e)
			assert.Equal(t, map[string]string{"paren@remote.example": encoded}, e.ReactionEmojis)
			lite := &UserLite{}
			r.PopulateUserEmojis(note.User, lite)
			assert.Equal(t, map[string]string{"paren": encoded}, lite.Emojis)
		})
	}
}

// ローカルの絵文字の URL は形の判定に掛けない。
func TestEmojiResolver_LocalEmojiURLIsNotChecked(t *testing.T) {
	lookup := &stubEmojiLookup{data: map[string][]*model.Emoji{"": {{ID: "e1", Name: "l", PublicURL: "/files/l.png"}}}}
	note := &model.Note{ID: "n1", Emojis: model.StringArray{"l"}}
	r := NewEmojiResolver(lookup, []*model.Note{note})
	assert.Equal(t, "/files/l.png", r.cache["l@"])
}

// 管理画面の一覧でも、形の崩れたリモートの絵文字の URL は空にする。
func TestPackEmojiDetailed_RemoteURLForm(t *testing.T) {
	host := "remote.example"
	assert.Equal(t, "", PackEmojiDetailed(&model.Emoji{Host: &host, PublicURL: "/a.png"}).URL)
	assert.Equal(t, "https://remote.example/a%20b.png",
		PackEmojiDetailed(&model.Emoji{Host: &host, OriginalURL: "https://remote.example/a b.png"}).URL)
	require.Equal(t, "/files/a.png", PackEmojiDetailed(&model.Emoji{PublicURL: "/files/a.png"}).URL, "ローカルは変えない")
}
