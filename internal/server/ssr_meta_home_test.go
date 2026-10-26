package server

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

// upstream は /notes/:id を visibility In(['public', 'home']) で引き、home の
// ノートにも OGP を出す。followers / specified には出さない (#3848)。
func TestSSRNotePage_HomeNoteServesOGP(t *testing.T) {
	h, userRepo, noteRepo := newSSRTestHandler(t)
	alice := ssrTestUser("u1", "alice")
	userRepo.Users["u1"] = alice
	for _, tc := range []struct {
		vis  model.NoteVisibility
		want bool
	}{
		{model.NoteVisibilityPublic, true},
		{model.NoteVisibilityHome, true},
		{model.NoteVisibilityFollowers, false},
		{model.NoteVisibilitySpecified, false},
	} {
		t.Run(string(tc.vis), func(t *testing.T) {
			text := "body of " + string(tc.vis)
			noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", User: alice, Text: &text, Visibility: tc.vis}
			body := ssrGet(t, h.NotePage, "/notes/n1", map[string]string{"id": "n1"}).Body.String()
			if tc.want {
				assert.Contains(t, body, `<meta property="og:type" content="article">`)
				assert.Contains(t, body, `<meta property="og:description" content="`+text+`">`)
				assert.Contains(t, body, "misskey:note-id")
				return
			}
			assert.NotContains(t, body, "misskey:note-id")
			assert.NotContains(t, body, text)
		})
	}
}

// home の返信先・引用先も、未ログインで読めるので要約に本文を出す。followers の
// 返信先は隠す (#3848)。
func TestSSRNoteSummary_HomeReplyIsNotHidden(t *testing.T) {
	h, userRepo, noteRepo := newSSRTestHandler(t)
	alice := ssrTestUser("u1", "alice")
	userRepo.Users["u1"] = alice
	parent := "home parent"
	noteRepo.Notes["p1"] = &model.Note{ID: "p1", UserID: "u1", User: alice, Text: &parent, Visibility: model.NoteVisibilityHome}
	secret := "followers parent"
	noteRepo.Notes["p2"] = &model.Note{ID: "p2", UserID: "u1", User: alice, Text: &secret, Visibility: model.NoteVisibilityFollowers}

	reply := "child"
	p1, p2 := "p1", "p2"
	noteRepo.Notes["c1"] = &model.Note{ID: "c1", UserID: "u1", User: alice, Text: &reply, ReplyID: &p1, Visibility: model.NoteVisibilityPublic}
	noteRepo.Notes["c2"] = &model.Note{ID: "c2", UserID: "u1", User: alice, Text: &reply, ReplyID: &p2, Visibility: model.NoteVisibilityPublic}

	assert.Equal(t, "child\n\nRE: home parent", h.noteSummary(noteRepo.Notes["c1"]))
	assert.Equal(t, "child\n\nRE: (\u26D4)", h.noteSummary(noteRepo.Notes["c2"]))

	quote := "quote"
	noteRepo.Notes["q1"] = &model.Note{ID: "q1", UserID: "u1", User: alice, Text: &quote, RenoteID: &p1, Visibility: model.NoteVisibilityPublic}
	assert.Equal(t, "quote\n\nRN: home parent", h.noteSummary(noteRepo.Notes["q1"]))
}

// home のノートも、作者の設定で未ログインに見せないものには meta を出さない。
// home は public と同じく、期限後にフォロワー限定へ下がる設定
// (makeNotesFollowersOnlyBefore) でも隠れる。public だけでなく home の OGP も
// この判定だけで守られるので、home で確かめる (#3848)。
func TestSSRNotePage_HomeNoteRespectsAuthorSettings(t *testing.T) {
	zero := 0
	for name, set := range map[string]func(u *model.User){
		"followers-only-before": func(u *model.User) { u.MakeNotesFollowersOnlyBefore = &zero },
		"hidden-before":         func(u *model.User) { u.MakeNotesHiddenBefore = &zero },
		"require-signin":        func(u *model.User) { u.RequireSigninToViewContents = true },
	} {
		t.Run(name, func(t *testing.T) {
			h, userRepo, noteRepo := newSSRTestHandler(t)
			alice := ssrTestUser("u1", "alice")
			set(alice)
			userRepo.Users["u1"] = alice
			text := "home body"
			noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", User: alice, Text: &text, Visibility: model.NoteVisibilityHome}
			body := ssrGet(t, h.NotePage, "/notes/n1", map[string]string{"id": "n1"}).Body.String()
			assert.NotContains(t, body, "misskey:note-id")
			assert.NotContains(t, body, text)
		})
	}
}
