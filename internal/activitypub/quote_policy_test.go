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

// 引用の `quote` / `quoteAuthorization` は承認があるときだけ付ける (#3234 段階 3)。
// 承認の無いまま `quote` を付けると、Mastodon は引用を「承認待ち」として表示し、
// 本文の RE: リンクを消す (承認を返さない相手への引用は永久にそのまま)。
func TestRenderNote_QuoteAndAuthorization(t *testing.T) {
	idGen := newIDGen(t)
	text := "look"
	target := "target1"
	quoting := &model.Note{ID: idGen.Generate(time.Now()), UserID: "alice", Text: &text, RenoteID: &target, Visibility: model.NoteVisibilityPublic}
	targetURI := "https://example.com/notes/target1"

	render := func(t *testing.T, r *Renderer, n *model.Note) map[string]any {
		t.Helper()
		raw, err := json.Marshal(r.RenderNote(n, idGen))
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m))
		return m
	}

	t.Run("quote without approval stays legacy", func(t *testing.T) {
		m := render(t, newRenderer(), quoting)
		assert.NotContains(t, m, "quote")
		assert.NotContains(t, m, "quoteAuthorization")
		assert.Equal(t, targetURI, m["_misskey_quote"])
		assert.Contains(t, m["content"], "RE: ")
	})

	t.Run("quote with approval", func(t *testing.T) {
		r := newRenderer()
		var asked *model.Note
		r.SetQuoteApprovalResolver(func(n *model.Note) (string, error) {
			asked = n
			return "https://remote.example/approvals/1", nil
		})
		m := render(t, r, quoting)
		assert.Equal(t, targetURI, m["quote"])
		assert.Equal(t, "https://remote.example/approvals/1", m["quoteAuthorization"])
		assert.Same(t, quoting, asked)
	})

	t.Run("resolver returning empty adds nothing", func(t *testing.T) {
		r := newRenderer()
		r.SetQuoteApprovalResolver(func(*model.Note) (string, error) { return "", nil })
		m := render(t, r, quoting)
		assert.NotContains(t, m, "quote")
		assert.NotContains(t, m, "quoteAuthorization")
	})

	t.Run("lookup failure renders an unapproved quote", func(t *testing.T) {
		r := newRenderer()
		r.SetQuoteApprovalResolver(func(*model.Note) (string, error) { return "x", assert.AnError })
		m := render(t, r, quoting)
		assert.NotContains(t, m, "quote")
		assert.NotContains(t, m, "quoteAuthorization")
		assert.Equal(t, targetURI, m["_misskey_quote"])
	})

	t.Run("pure renote and plain note carry no quote", func(t *testing.T) {
		r := newRenderer()
		called := false
		r.SetQuoteApprovalResolver(func(*model.Note) (string, error) { called = true; return "x", nil })
		plain := &model.Note{ID: idGen.Generate(time.Now()), UserID: "alice", Text: &text, Visibility: model.NoteVisibilityPublic}
		pure := &model.Note{ID: idGen.Generate(time.Now()), UserID: "alice", RenoteID: &target, Visibility: model.NoteVisibilityPublic}
		for _, n := range []*model.Note{plain, pure} {
			m := render(t, r, n)
			assert.NotContains(t, m, "quote")
			assert.NotContains(t, m, "quoteAuthorization")
		}
		assert.False(t, called, "the resolver is only asked for quotes")
	})
}

func TestIsQuote(t *testing.T) {
	text, empty, target := "t", "", "n1"
	assert.True(t, IsQuote(&model.Note{RenoteID: &target, Text: &text}))
	assert.False(t, IsQuote(&model.Note{RenoteID: &target}))
	assert.False(t, IsQuote(&model.Note{RenoteID: &target, Text: &empty}))
	assert.False(t, IsQuote(&model.Note{Text: &text}))
}

func TestRenderQuoteRequest(t *testing.T) {
	idGen := newIDGen(t)
	text := "look"
	target := "remote1"
	n := &model.Note{ID: "q1", UserID: "alice", Text: &text, RenoteID: &target, Visibility: model.NoteVisibilityPublic}
	raw, err := json.Marshal(newRenderer().RenderQuoteRequest(n, "https://remote.example/notes/9", idGen))
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, "https://example.com/notes/q1#quote-request", m["id"])
	assert.Equal(t, "QuoteRequest", m["type"])
	assert.Equal(t, "https://example.com/users/alice", m["actor"])
	assert.Equal(t, "https://remote.example/notes/9", m["object"])
	assert.NotNil(t, m["@context"])
	inst, ok := m["instrument"].(map[string]any)
	require.True(t, ok, "the quoting note is inlined")
	assert.Equal(t, "https://example.com/notes/q1", inst["id"])
	assert.NotContains(t, inst, "@context")
}

func TestRenderNoteUpdate(t *testing.T) {
	idGen := newIDGen(t)
	text := "hi"
	n := &model.Note{ID: "u1", UserID: "alice", Text: &text, Visibility: model.NoteVisibilityPublic}
	u, err := newRenderer().RenderNoteUpdate(n, idGen)
	require.NoError(t, err)
	raw, err := json.Marshal(u)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, "Update", m["type"])
	assert.Contains(t, m["id"], "https://example.com/notes/u1#updates/")
	obj := m["object"].(map[string]any)
	assert.Equal(t, "Note", obj["type"])
	assert.Equal(t, "https://example.com/notes/u1", obj["id"])
	// 編集ではないので updated を付けない (Mastodon は updated の無い Update を
	// 付随情報の更新として扱う)。
	assert.NotContains(t, obj, "updated")
	assert.NotContains(t, obj, "@context")
	assert.Equal(t, obj["to"], m["to"])
}

// 承認済みの引用を配り直す Update は、承認を引けなければ作らない。承認の抜けた
// Update を受けた Mastodon は、承認済みの引用を未承認に戻す。
func TestRenderNoteUpdate_QuoteApproval(t *testing.T) {
	idGen := newIDGen(t)
	text, target := "look", "target1"
	quoting := &model.Note{ID: "u2", UserID: "alice", Text: &text, RenoteID: &target, Visibility: model.NoteVisibilityPublic}

	r := newRenderer()
	r.SetQuoteApprovalResolver(func(*model.Note) (string, error) { return "", assert.AnError })
	_, err := r.RenderNoteUpdate(quoting, idGen)
	assert.ErrorIs(t, err, assert.AnError)
	_, err = r.RenderQuestionUpdate(quoting, idGen)
	assert.ErrorIs(t, err, assert.AnError)

	// 引用でなければ承認を引かない (引けなくても作れる)。
	_, err = r.RenderNoteUpdate(&model.Note{ID: "u3", UserID: "alice", Text: &text, Visibility: model.NoteVisibilityPublic}, idGen)
	assert.NoError(t, err)

	calls := 0
	r.SetQuoteApprovalResolver(func(*model.Note) (string, error) { calls++; return "https://remote.example/approvals/1", nil })
	u, err := r.RenderNoteUpdate(quoting, idGen)
	require.NoError(t, err)
	obj := u.Object.(*Note)
	assert.Equal(t, APLenientID("https://remote.example/approvals/1"), obj.QuoteAuthorization)
	assert.Equal(t, APLenientID("https://example.com/notes/target1"), obj.Quote)
	assert.Equal(t, 1, calls, "the approval is looked up once")
}

func TestRenderQuoteAuthorizationDelete(t *testing.T) {
	n := &model.Note{ID: "n1", UserID: "alice"}
	a := &model.NoteQuoteAuthorization{ID: "a1", NoteID: "n1", QuoterID: "bob", QuotingURI: "https://remote.example/notes/q"}
	raw, err := json.Marshal(newRenderer().RenderQuoteAuthorizationDelete(n, a))
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	stampURI := "https://example.com/notes/n1/quote-authorizations/a1"
	assert.Equal(t, "Delete", m["type"])
	assert.Equal(t, stampURI+"#delete", m["id"])
	assert.Equal(t, "https://example.com/users/alice", m["actor"])
	assert.NotNil(t, m["@context"])
	// フォロワー限定の投稿を指す承認を公開宛てとして出さない。
	assert.NotContains(t, m, "to")
	obj := m["object"].(map[string]any)
	assert.Equal(t, stampURI, obj["id"])
	assert.Equal(t, "QuoteAuthorization", obj["type"])
	assert.Equal(t, "https://remote.example/notes/q", obj["interactingObject"])
	assert.NotContains(t, obj, "@context")
}
