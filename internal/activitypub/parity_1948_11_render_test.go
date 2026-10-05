package activitypub

import (
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// millisZ matches the upstream Date.toISOString() shape (fixed 3-digit ms + Z).
var millisZ = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)

func marshalMap(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	return m
}

// #1948-11: RenderVote の object Note は最小フィールド (id/type/attributedTo/to/
// inReplyTo/name) のみ。content/published/sensitive は upstream renderVote が出さない。
func TestRenderVote_MinimalNote(t *testing.T) {
	r := newRenderer()
	voter := &model.User{ID: "voter1"}
	target := &model.Note{ID: "poll1"}
	c := r.RenderVote(voter, target, "https://remote.example/notes/poll1", "https://remote.example/users/author", "choiceA")
	m := marshalMap(t, c)
	note := m["object"].(map[string]any)
	assert.Equal(t, "Note", note["type"])
	assert.Equal(t, "choiceA", note["name"])
	assert.Equal(t, "https://remote.example/notes/poll1", note["inReplyTo"])
	_, hasContent := note["content"]
	_, hasPublished := note["published"]
	_, hasSensitive := note["sensitive"]
	assert.False(t, hasContent, "vote note に content は出さない (#1948-11)")
	assert.False(t, hasPublished, "vote note に published は出さない (#1948-11)")
	assert.False(t, hasSensitive, "vote note に sensitive は出さない (#1948-11)")
	// Create activity 側の published は .000Z。
	assert.Regexp(t, millisZ, m["published"], "Create.published は .000Z (#1948-11)")
}

// #1948-11: Delete / UndoLike / UndoAnnounce に published(.000Z) が常に乗る。
func TestRenderDeleteAndUndo_HavePublished(t *testing.T) {
	r := newRenderer()
	author := &model.User{ID: "u1"}

	d := marshalMap(t, r.RenderDelete(author, "https://example.com/notes/n1"))
	assert.Regexp(t, millisZ, d["published"], "Delete.published は .000Z (#1948-11)")

	da := marshalMap(t, r.RenderDeleteActor(author))
	assert.Regexp(t, millisZ, da["published"], "DeleteActor.published は .000Z (#1948-11)")

	ul := marshalMap(t, r.RenderUndoLike(author, &Like{Activity: Activity{Object: Object{ID: "https://example.com/likes/l1"}}}))
	assert.Regexp(t, millisZ, ul["published"], "UndoLike.published は .000Z (#1948-11)")

	ua := marshalMap(t, r.RenderUndoAnnounce(author, &Announce{Activity: Activity{Object: Object{ID: "https://example.com/notes/n1/activity"}}}))
	assert.Regexp(t, millisZ, ua["published"], "UndoAnnounce.published は .000Z (#1948-11)")

	ub := marshalMap(t, r.RenderUndoBlock("u1", "https://remote.example/users/b"))
	assert.Regexp(t, millisZ, ub["published"], "UndoBlock.published は .000Z (#1948-11)")

	uda := marshalMap(t, r.RenderUndoDeleteActor(author))
	assert.Regexp(t, millisZ, uda["published"], "UndoDeleteActor.published は .000Z (#1948-11)")

	// #1993: RenderUndoFollow (local→remote unfollow) も published(.000Z) を持つ。
	uf := marshalMap(t, r.RenderUndoFollow("u1", "https://remote.example/users/b"))
	assert.Regexp(t, millisZ, uf["published"], "UndoFollow.published は .000Z (#1993)")
	assert.Equal(t, "Undo", uf["type"])
	inner, ok := uf["object"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Follow", inner["type"])
	_, hasCtx := inner["@context"]
	assert.False(t, hasCtx, "inner Follow は @context を持たない (#1993)")
}

// #1948-11: RenderQuestionUpdate の published は .000Z だが、Activity ID は
// 同一秒内の連続投票でも衝突しないよう nano 精度を維持する。
func TestRenderQuestionUpdate_PublishedMillisIDNano(t *testing.T) {
	r := newRenderer()
	idGen := newIDGen(t)
	n := &model.Note{ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic, HasPoll: true}
	u, err := r.RenderQuestionUpdate(n, idGen)
	require.NoError(t, err)
	m := marshalMap(t, u)
	assert.Regexp(t, millisZ, m["published"], "QuestionUpdate.published は .000Z (#1948-11)")
	// ID は #updates/<nano> 形式 (ミリ秒超の精度を含むため millisZ には一致しない)。
	assert.Contains(t, m["id"], "#updates/", "QuestionUpdate.id は #updates/<nano> 形式")
}

// #1948-11: actor の manuallyApprovesFollowers/discoverable/isCat/
// _misskey_requireSigninToViewContents は false でも常に key として present。
func TestRenderPerson_BoolFlagsAlwaysPresent(t *testing.T) {
	r := newRenderer()
	// 全 bool が false になる素のユーザー。
	u := &model.User{ID: "u1", Username: "alice"}
	m := marshalMap(t, r.RenderPerson(u, nil, "PUBKEY", nil))
	for _, k := range []string{"manuallyApprovesFollowers", "discoverable", "isCat", "_misskey_requireSigninToViewContents"} {
		v, ok := m[k]
		assert.True(t, ok, k+" は false でも present (#1948-11)")
		assert.Equal(t, false, v, k+" は false")
	}

	// 値が user 状態を反映することも確認 (explorable/cat/locked = true)。
	u2 := &model.User{ID: "u2", Username: "bob", IsExplorable: true, IsCat: true, IsLocked: true}
	m2 := marshalMap(t, r.RenderPerson(u2, nil, "PUBKEY", nil))
	assert.Equal(t, true, m2["discoverable"])
	assert.Equal(t, true, m2["isCat"])
	assert.Equal(t, true, m2["manuallyApprovesFollowers"])
}

// #1948-11: local custom emoji tag は id(=baseURL/emojis/name) / updated(.000Z) /
// icon.mediaType を必ず持ち、publicUrl 空時は originalUrl にフォールバックする。
func TestAddEmojiTags_LocalIDUpdatedMediaType(t *testing.T) {
	r := newRenderer()
	upd := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	typ := "image/webp"
	r.SetEmojiResolver(&stubEmojiResolver{emojis: map[string]*model.Emoji{
		"smile": {Name: "smile", PublicURL: "https://example.com/files/smile.webp", UpdatedAt: &upd, Type: &typ},
	}})
	l := r.RenderLike(&model.User{ID: "alice"}, "https://remote.example/notes/n1", ":smile@.:", "https://example.com/likes/l1")
	require.Len(t, l.Tag, 1)
	et := l.Tag[0].(EmojiTag)
	assert.Equal(t, "https://example.com/emojis/smile", et.ID, "local emoji tag は local id を持つ (#1948-11)")
	assert.Equal(t, "2026-01-02T03:04:05.000Z", et.Updated, "updated は .000Z (#1948-11)")
	assert.Equal(t, "image/webp", et.Icon.MediaType, "icon.mediaType (#1948-11)")

	// publicUrl 空 → originalUrl fallback、Type nil → image/png default、UpdatedAt nil → now。
	r.SetEmojiResolver(&stubEmojiResolver{emojis: map[string]*model.Emoji{
		"foo": {Name: "foo", PublicURL: "", OriginalURL: "https://example.com/files/foo-orig.png"},
	}})
	l2 := r.RenderLike(&model.User{ID: "alice"}, "https://remote.example/notes/n1", ":foo@.:", "https://example.com/likes/l2")
	require.Len(t, l2.Tag, 1)
	et2 := l2.Tag[0].(EmojiTag)
	assert.Equal(t, "https://example.com/files/foo-orig.png", et2.Icon.URL.String(), "publicUrl 空は originalUrl fallback (#1948-11)")
	assert.Equal(t, "image/png", et2.Icon.MediaType, "Type nil は image/png default (#1948-11)")
	assert.Regexp(t, millisZ, et2.Updated, "UpdatedAt nil は now(.000Z) (#1948-11)")
}

// stubNoteResolver1948 returns a fixed quote URI.
type stubNoteResolver1948 struct{ uri string }

func (s stubNoteResolver1948) FindByID(string) (*model.Note, error) {
	return &model.Note{URI: &s.uri}, nil
}

// #1948-11: simple text の quote renote でも _misskey_content / source を必ず出す
// (upstream extraHtml!=null で noMisskeyContent=false 強制)。
func TestRenderNote_SimpleQuoteEmitsSource(t *testing.T) {
	r := newRenderer()
	r.SetNoteResolver(stubNoteResolver1948{uri: "https://remote.example/notes/orig"})
	idGen := newIDGen(t)
	text := "hi" // mfm.IsSimple が true になる素の本文
	renoteID := "renote-target"
	n := &model.Note{ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic, Text: &text, RenoteID: &renoteID}
	out := r.RenderNote(n, idGen)
	assert.Equal(t, "hi", out.MisskeyContent.String(), "simple quote renote でも _misskey_content を出す (#1948-11)")
	require.NotNil(t, out.Source, "simple quote renote でも source を出す (#1948-11)")
	assert.Equal(t, "text/x.misskeymarkdown", out.Source.MediaType)
	assert.Equal(t, "https://remote.example/notes/orig", out.QuoteURL.String())

	// 対照: quote なしの simple text は従来どおり source/_misskey_content を省略。
	n2 := &model.Note{ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic, Text: &text}
	out2 := r.RenderNote(n2, idGen)
	assert.Empty(t, out2.MisskeyContent, "quote なし simple text は _misskey_content を出さない")
	assert.Nil(t, out2.Source, "quote なし simple text は source を出さない")
}

// #1948-11: RenderNote の published が .000Z (parseNoteTime 経由)。
func TestRenderNote_PublishedMillis(t *testing.T) {
	r := newRenderer()
	idGen := newIDGen(t)
	text := "hello"
	n := &model.Note{ID: idGen.Generate(time.Now()), UserID: "u1", Visibility: model.NoteVisibilityPublic, Text: &text}
	out := r.RenderNote(n, idGen)
	assert.Regexp(t, millisZ, out.Published, "Note.published は .000Z (#1948-11)")
}

// **`suspended` は読むだけで出さない** (#2951)。
//
// Mastodon 拡張を読む側だけ足したので、こちらから出すと upstream と wire
// shape が乖離する (upstream の renderPerson は出さない)。
//
// このフィールドは `manuallyApprovesFollowers` / `discoverable` の間に置かれて
// おり、**周りは「false でも必ず出す」という正反対の方針** (#1948-11)。
// 「周りに揃える」リファクタで `omitempty` が外れると黙って出るようになるので、
// ここで固定する。
func TestRenderPerson_SuspendedIsNeverEmitted(t *testing.T) {
	r := newRenderer()

	m := marshalMap(t, r.RenderPerson(&model.User{ID: "u1", Username: "alice"}, nil, "PUBKEY", nil))
	_, ok := m["suspended"]
	assert.False(t, ok, "RenderPerson が suspended を出している (upstream は出さない)")

	// 凍結されたローカルユーザーでも出さない。
	m2 := marshalMap(t, r.RenderPerson(
		&model.User{ID: "u2", Username: "bob", IsSuspended: true}, nil, "PUBKEY", nil))
	_, ok2 := m2["suspended"]
	assert.False(t, ok2, "凍結ユーザーの actor に suspended が出ている")
}
