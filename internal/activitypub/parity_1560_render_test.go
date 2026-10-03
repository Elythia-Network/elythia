package activitypub

import (
	"strings"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #1560 [LOW] RenderNote: 空文字 CW は summary=ZWSP + sensitive=true。
func TestRenderNote_EmptyCWZWSP(t *testing.T) {
	r := newRenderer()
	idGen := newIDGen(t)
	empty := ""
	n := &model.Note{ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic, CW: &empty}
	out := r.RenderNote(n, idGen)
	assert.Equal(t, "\u200b", out.Summary.String(), "empty CW must render ZWSP summary")
	assert.True(t, out.Sensitive.Bool(), "any non-nil CW (incl empty) must set sensitive=true")

	// CW=nil は summary 無し / sensitive=false
	n2 := &model.Note{ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic}
	out2 := r.RenderNote(n2, idGen)
	assert.Empty(t, out2.Summary)
	assert.False(t, out2.Sensitive.Bool())
}

// #1560 [LOW] RenderNote: quote note は content 末尾に quote-inline span を付ける。
func TestRenderNote_QuoteInlineHTML(t *testing.T) {
	r := newRenderer()
	r.SetNoteResolver(stubNoteResolver1560{uri: "https://remote.example/notes/orig"})
	idGen := newIDGen(t)
	text := "my comment"
	renoteID := "rt1"
	n := &model.Note{ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic, Text: &text, RenoteID: &renoteID}
	out := r.RenderNote(n, idGen)
	assert.Contains(t, out.Content, `<span class="quote-inline">RE: <a href="https://remote.example/notes/orig">https://remote.example/notes/orig</a></span>`)
	assert.Equal(t, "https://remote.example/notes/orig", out.QuoteURL.String())
}

// TestRenderNote_QuoteInlineEscapesLikeUpstream checks that the quote-inline
// link escapes the quoted URI with upstream's escapeHtml notation (#3329).
//
// 本家 ApRendererService は escapeHtml で `'` を `&#039;`、`"` を `&quot;` にする。
func TestRenderNote_QuoteInlineEscapesLikeUpstream(t *testing.T) {
	r := newRenderer()
	r.SetNoteResolver(stubNoteResolver1560{uri: `https://remote.example/notes/a'b"c&d`})
	idGen := newIDGen(t)
	text := "my comment"
	renoteID := "rt1"
	n := &model.Note{ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic, Text: &text, RenoteID: &renoteID}
	out := r.RenderNote(n, idGen)
	const esc = `https://remote.example/notes/a&#039;b&quot;c&amp;d`
	assert.Contains(t, out.Content, `<span class="quote-inline">RE: <a href="`+esc+`">`+esc+`</a></span>`)
}

// #1560 [LOW] RenderPerson: top-level sharedInbox + user.tags Hashtag。
func TestRenderPerson_SharedInboxAndHashtags(t *testing.T) {
	r := newRenderer()
	r.SetHost("example.com")
	u := &model.User{ID: "u1", Username: "alice", Tags: model.StringArray{"go", "misskey"}}
	p := r.RenderPerson(u, nil, "PEM", nil)
	assert.Equal(t, "https://example.com/inbox", p.SharedInbox.String(), "top-level sharedInbox must be set")
	assert.Equal(t, "https://example.com/inbox", p.Endpoints.SharedInbox.String())
	// user.tags が Hashtag として tag に入る
	var hashtags int
	for _, tg := range p.Tag {
		if h, ok := tg.(Hashtag); ok {
			hashtags++
			assert.True(t, strings.HasPrefix(h.Name, "#"))
		}
	}
	assert.Equal(t, 2, hashtags, "user.tags must be rendered as Hashtag entries")
}

type stubNoteResolver1560 struct{ uri string }

func (s stubNoteResolver1560) FindByID(string) (*model.Note, error) {
	remote := "remote.example"
	u := s.uri
	return &model.Note{ID: "rt1", URI: &u, UserHost: &remote}, nil
}

// #1560 [MEDIUM] RenderBlock / RenderUndoBlock (outbound Block delivery)。
func TestRenderBlock_AndUndo(t *testing.T) {
	r := newRenderer()
	blockee := "https://remote.example/users/bob"
	bl := r.RenderBlock("alice", blockee)
	assert.Equal(t, "Block", bl.Type.String())
	assert.Equal(t, "https://example.com/users/alice", bl.Actor)
	assert.Equal(t, blockee, bl.Object)
	assert.NotEmpty(t, bl.ID)
	assert.NotNil(t, bl.Context, "standalone Block must carry @context (#1560 review)")

	undo := r.RenderUndoBlock("alice", blockee)
	assert.Equal(t, "Undo", undo.Type.String())
	assert.Equal(t, "https://example.com/users/alice", undo.Actor)
	inner, ok := undo.Object.(*Block)
	require.True(t, ok)
	assert.Equal(t, blockee, inner.Object)
	assert.Nil(t, inner.Context, "inner Block must not carry @context")
	// Block と Undo(Block) の inner id は同じ (block 行 id 非依存で決定的)
	assert.Equal(t, bl.ID, inner.ID)
}

// #1560 [MEDIUM] RenderUpdate(Person) (outbound profile update delivery)。
func TestRenderUpdate_Person(t *testing.T) {
	r := newRenderer()
	u := &model.User{ID: "alice", Username: "alice"}
	person := r.RenderPerson(u, nil, "PEM", nil)
	upd := r.RenderUpdate(person)
	assert.Equal(t, "Update", upd.Type.String())
	assert.Equal(t, "https://example.com/users/alice", upd.Actor)
	assert.Contains(t, upd.To, Public)
	inner, ok := upd.Object.(*Person)
	require.True(t, ok)
	assert.Nil(t, inner.Context, "inner Person must not carry @context (集約)")
	assert.Equal(t, "https://example.com/users/alice", inner.ID)
}
