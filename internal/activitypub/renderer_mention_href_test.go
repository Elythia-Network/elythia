package activitypub

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

// TestRenderer_RenderNote_MentionHrefFromMentionedRemoteUsers checks that the
// note content links a remote mention to the user's url taken from the note's
// mentionedRemoteUsers column, as upstream ApMfmService.getNoteHtml does
// (#3329). A mention missing from the column links to this server's
// `/@<acct>` page.
func TestRenderer_RenderNote_MentionHrefFromMentionedRemoteUsers(t *testing.T) {
	r := newRenderer()
	r.SetHost("example.com")
	idGen := newIDGen(t)
	n := &model.Note{
		ID:                   idGen.Generate(time.Now()),
		UserID:               "author",
		Visibility:           model.NoteVisibilityPublic,
		Text:                 new(string("@Bob@Remote.Example @carol@other.example @dave")),
		MentionedRemoteUsers: `[{"uri":"https://remote.example/users/1","url":"https://remote.example/@bob","username":"bob","host":"remote.example"}]`,
	}
	out := r.RenderNote(n, idGen)
	assert.Equal(t,
		`<a href="https://remote.example/@bob" class="u-url mention">@Bob@Remote.Example</a> `+
			`<a href="https://example.com/@carol@other.example" class="u-url mention">@carol@other.example</a> `+
			`<a href="https://example.com/@dave" class="u-url mention">@dave</a>`,
		out.Content)
}

// TestRenderer_RenderNote_BrokenMentionedRemoteUsers checks that a column
// that is not valid JSON does not stop rendering; every mention then links to
// this server.
func TestRenderer_RenderNote_BrokenMentionedRemoteUsers(t *testing.T) {
	r := newRenderer()
	r.SetHost("example.com")
	idGen := newIDGen(t)
	n := &model.Note{
		ID:                   idGen.Generate(time.Now()),
		UserID:               "author",
		Visibility:           model.NoteVisibilityPublic,
		Text:                 new(string("@bob@remote.example")),
		MentionedRemoteUsers: `{`,
	}
	out := r.RenderNote(n, idGen)
	assert.Equal(t, `<a href="https://example.com/@bob@remote.example" class="u-url mention">@bob@remote.example</a>`, out.Content)
}
