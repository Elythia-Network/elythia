package server

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/model"
)

// 返信先・引用先が公開でも、作者が「ログイン必須」「期間で隠す」を有効にしていれば
// 要約に本文を出さない。upstream は pack の shouldHideNote で (⛔) に落とす。
// /embed/notes の notehide と同じ判定で、ページ本体のノートの作者ではなく
// 返信先・引用先の作者の設定を見る。
func TestSSRNotePage_SummaryHidesAncestorsByAuthorPrefs(t *testing.T) {
	const secret = "CAROL-SIGNIN-ONLY-SECRET"
	zero := 0
	cases := []struct {
		name  string
		setup func(carol *model.User)
	}{
		{"requireSigninToViewContents", func(carol *model.User) { carol.RequireSigninToViewContents = true }},
		{"makeNotesHiddenBefore", func(carol *model.User) { carol.MakeNotesHiddenBefore = &zero }},
		{"makeNotesFollowersOnlyBefore", func(carol *model.User) { carol.MakeNotesFollowersOnlyBefore = &zero }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, userRepo, noteRepo := newSSRTestHandler(t)
			gen := testIDGen(t)
			alice := ssrTestUser("u1", "alice")
			carol := ssrTestUser("u2", "carol")
			tc.setup(carol)
			userRepo.Users["u1"] = alice
			userRepo.Users["u2"] = carol

			text := secret
			parentID := gen.Generate(time.Now().Add(-time.Hour))
			noteRepo.Notes[parentID] = &model.Note{
				ID: parentID, UserID: "u2", User: carol, Text: &text,
				Visibility: model.NoteVisibilityPublic,
			}
			child := "reply and quote"
			childID := gen.Generate(time.Now())
			noteRepo.Notes[childID] = &model.Note{
				ID: childID, UserID: "u1", User: alice, Text: &child,
				ReplyID: &parentID, RenoteID: &parentID,
				Visibility: model.NoteVisibilityPublic,
			}

			body := ssrGet(t, h.NotePage, "/notes/"+childID, map[string]string{"id": childID}).Body.String()

			assert.Contains(t, body, "misskey:note-id", "the page note itself is still described")
			assert.NotContains(t, body, secret)
			assert.Contains(t, body, "RE: (⛔)")
			assert.Contains(t, body, "RN: (⛔)")
		})
	}
}

// 作者の行が読めない返信先・引用先とページ本体のノートは、設定を確かめられないので
// 本文を出さない側に倒す。
func TestSSRNotePage_MissingAuthorFailsClosed(t *testing.T) {
	h, userRepo, noteRepo := newSSRTestHandler(t)
	alice := ssrTestUser("u1", "alice")
	userRepo.Users["u1"] = alice
	orphanText := "orphan body"
	noteRepo.Notes["orphan"] = &model.Note{
		ID: "orphan", UserID: "gone", Text: &orphanText,
		Visibility: model.NoteVisibilityPublic,
	}
	orphanID := "orphan"
	child := "child"
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "u1", User: alice, Text: &child,
		ReplyID: &orphanID, Visibility: model.NoteVisibilityPublic,
	}

	body := ssrGet(t, h.NotePage, "/notes/n1", map[string]string{"id": "n1"}).Body.String()
	assert.Contains(t, body, "misskey:note-id")
	assert.NotContains(t, body, orphanText)
	assert.Contains(t, body, "RE: (⛔)")

	top := ssrGet(t, h.NotePage, "/notes/orphan", map[string]string{"id": "orphan"}).Body.String()
	assert.NotContains(t, top, "misskey:note-id")
	assert.NotContains(t, top, orphanText)
}
