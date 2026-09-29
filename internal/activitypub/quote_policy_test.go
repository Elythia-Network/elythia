package activitypub

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/model"
)

func renderNoteJSON(t *testing.T, n *model.Note) map[string]any {
	t.Helper()
	r := newRenderer()
	out := r.RenderNote(n, newIDGen(t))
	AddContext(out)
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	return m
}

// 公開範囲より広く引用させない (FEP-044f、#3234)。
func TestRenderNote_QuotePolicy(t *testing.T) {
	idGen := newIDGen(t)
	text := "hi"
	for vis, want := range map[model.NoteVisibility]string{
		model.NoteVisibilityPublic:    Public,
		model.NoteVisibilityHome:      Public,
		model.NoteVisibilityFollowers: "https://example.com/users/alice/followers",
		// 誰も引用できないときは作者だけ (空配列は「項目が無い」と同じになる)。
		model.NoteVisibilitySpecified: "https://example.com/users/alice",
	} {
		t.Run(string(vis), func(t *testing.T) {
			m := renderNoteJSON(t, &model.Note{ID: idGen.Generate(time.Now()), UserID: "alice", Text: &text, Visibility: vis})
			policy, ok := m["interactionPolicy"].(map[string]any)
			require.True(t, ok, "interactionPolicy is rendered")
			canQuote := policy["canQuote"].(map[string]any)
			assert.Equal(t, []any{want}, canQuote["automaticApproval"])
			assert.NotContains(t, canQuote, "manualApproval")
		})
	}
}

// 受け取る側は Mastodon と同じ IRI で語を解釈する。context に無いと、JSON-LD を
// 解釈する相手には項目が無いのと同じになる。
func TestContext_FEP044fTerms(t *testing.T) {
	want := map[string]string{
		"quote":              "https://w3id.org/fep/044f#quote",
		"quoteAuthorization": "https://w3id.org/fep/044f#quoteAuthorization",
		"interactionPolicy":  "gts:interactionPolicy",
		"canQuote":           "gts:canQuote",
		"automaticApproval":  "gts:automaticApproval",
		"manualApproval":     "gts:manualApproval",
		"interactingObject":  "gts:interactingObject",
		"interactionTarget":  "gts:interactionTarget",
	}
	for term, iri := range want {
		def, ok := MisskeyContext[term].(map[string]string)
		require.True(t, ok, term)
		assert.Equal(t, iri, def["@id"], term)
		assert.Equal(t, "@id", def["@type"], term)
	}
	assert.Equal(t, "https://gotosocial.org/ns#", MisskeyContext["gts"])
	assert.Equal(t, "https://w3id.org/fep/044f#QuoteRequest", MisskeyContext["QuoteRequest"])
	assert.Equal(t, "https://w3id.org/fep/044f#QuoteAuthorization", MisskeyContext["QuoteAuthorization"])
}

func TestRenderQuoteAuthorization(t *testing.T) {
	r := newRenderer()
	out := r.RenderQuoteAuthorization(&model.Note{ID: "n1", UserID: "alice"},
		&model.NoteQuoteAuthorization{ID: "qa1", NoteID: "n1", QuotingURI: "https://remote.example/statuses/1"})
	raw, err := json.Marshal(out)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, "https://example.com/notes/n1/quote-authorizations/qa1", m["id"])
	assert.Equal(t, "QuoteAuthorization", m["type"])
	assert.Equal(t, "https://example.com/users/alice", m["attributedTo"])
	// URI だけを入れ、投稿を埋め込まない (FEP の MUST NOT)。
	assert.Equal(t, "https://remote.example/statuses/1", m["interactingObject"])
	assert.Equal(t, "https://example.com/notes/n1", m["interactionTarget"])
	assert.NotNil(t, m["@context"])
}

// Follow への Accept は result を出さない (従来どおり)。
func TestRenderAccept_NoResultByDefault(t *testing.T) {
	raw, err := json.Marshal(newRenderer().RenderAccept("alice", map[string]any{"type": "Follow"}))
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"result"`)
}

// 受け取った Note の interactionPolicy が想定と違う形でも、Note 全体の読み取りは
// 失敗しない (投稿を取り込めなくならない)。
func TestNote_UnmarshalToleratesOddInteractionPolicy(t *testing.T) {
	for _, policy := range []string{`"x"`, `[1,2]`, `{"canQuote":"public"}`, `null`} {
		var n Note
		require.NoError(t, json.Unmarshal([]byte(`{"id":"https://r.example/n","type":"Note","content":"c","interactionPolicy":`+policy+`}`), &n), policy)
		assert.Equal(t, "c", n.Content, policy)
	}
}
