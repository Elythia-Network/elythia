package notes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corenote "github.com/shiroha-a/mk/internal/core/note"
	corepoll "github.com/shiroha-a/mk/internal/core/poll"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockStub satisfies both reaction.BlockingChecker and poll.BlockingChecker
// (同一シグネチャ IsBlocked(blocker, blockee))。#1538 の vote/reaction blocking
// 経路テスト用。
type blockStub struct{ blocked bool }

func (b blockStub) IsBlocked(_, _ string) (bool, error) { return b.blocked, nil }

func strPtr(s string) *string { return &s }

// --- C3/C4: notes/create 入力検証 (validateCreateInput) ---

func TestValidateCreateInput(t *testing.T) {
	tests := []struct {
		name    string
		req     *CreateRequest
		fileIDs []string
		wantErr bool
	}{
		{"valid text", &CreateRequest{Text: strPtr("hello")}, nil, false},
		{"text at max", &CreateRequest{Text: strPtr(strings.Repeat("あ", 3000))}, nil, false},
		{"text over max", &CreateRequest{Text: strPtr(strings.Repeat("あ", 3001))}, nil, true},
		{"cw empty", &CreateRequest{Text: strPtr("x"), CW: strPtr("")}, nil, true},
		{"cw over 100", &CreateRequest{Text: strPtr("x"), CW: strPtr(strings.Repeat("c", 101))}, nil, true},
		{"cw valid", &CreateRequest{Text: strPtr("x"), CW: strPtr("spoiler")}, nil, false},
		{"too many files", &CreateRequest{}, make([]string, 17), true},
		{"16 files ok", &CreateRequest{}, make([]string, 16), false},
		{"poll too few choices", &CreateRequest{Text: strPtr("x"), Poll: &PollRequest{Choices: []string{"a"}}}, nil, true},
		{"poll too many choices", &CreateRequest{Text: strPtr("x"), Poll: &PollRequest{Choices: make([]string, 11)}}, nil, true},
		{"poll empty choice", &CreateRequest{Text: strPtr("x"), Poll: &PollRequest{Choices: []string{"a", ""}}}, nil, true},
		{"poll choice over 50", &CreateRequest{Text: strPtr("x"), Poll: &PollRequest{Choices: []string{"a", strings.Repeat("b", 51)}}}, nil, true},
		{"poll valid", &CreateRequest{Text: strPtr("x"), Poll: &PollRequest{Choices: []string{"a", "b"}}}, nil, false},
		{"whitespace-only text-only", &CreateRequest{Text: strPtr("   \n\t")}, nil, true},
		{"whitespace text with file ok", &CreateRequest{Text: strPtr("   ")}, []string{"f1"}, false},
		{"whitespace text with renote ok", &CreateRequest{Text: strPtr("   "), RenoteID: strPtr("r1")}, nil, false},
		// #1930: visibility / reactionAcceptance enum 検証。
		{"visibility empty defaults ok", &CreateRequest{Text: strPtr("x"), Visibility: ""}, nil, false},
		{"visibility public", &CreateRequest{Text: strPtr("x"), Visibility: "public"}, nil, false},
		{"visibility home", &CreateRequest{Text: strPtr("x"), Visibility: "home"}, nil, false},
		{"visibility followers", &CreateRequest{Text: strPtr("x"), Visibility: "followers"}, nil, false},
		{"visibility specified", &CreateRequest{Text: strPtr("x"), Visibility: "specified"}, nil, false},
		{"visibility invalid", &CreateRequest{Text: strPtr("x"), Visibility: "foo"}, nil, true},
		{"reactionAcceptance nil ok", &CreateRequest{Text: strPtr("x"), ReactionAcceptance: nil}, nil, false},
		{"reactionAcceptance likeOnly", &CreateRequest{Text: strPtr("x"), ReactionAcceptance: strPtr("likeOnly")}, nil, false},
		{"reactionAcceptance likeOnlyForRemote", &CreateRequest{Text: strPtr("x"), ReactionAcceptance: strPtr("likeOnlyForRemote")}, nil, false},
		{"reactionAcceptance nonSensitiveOnly", &CreateRequest{Text: strPtr("x"), ReactionAcceptance: strPtr("nonSensitiveOnly")}, nil, false},
		{"reactionAcceptance nonSensitiveOnlyForLocalLikeOnlyForRemote", &CreateRequest{Text: strPtr("x"), ReactionAcceptance: strPtr("nonSensitiveOnlyForLocalLikeOnlyForRemote")}, nil, false},
		{"reactionAcceptance invalid", &CreateRequest{Text: strPtr("x"), ReactionAcceptance: strPtr("bogus")}, nil, true},
		{"reactionAcceptance empty rejected", &CreateRequest{Text: strPtr("x"), ReactionAcceptance: strPtr("")}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCreateInput(tt.req, tt.fileIDs)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// A3 (reactions blocked code/id) と D3 (polls NO_POLL code/id) は既存の
// TestReactionsCreate_Blocked / TestPollsVote_NoPoll を強化して検証している。

// --- D4: polls/vote blocking ---

func TestPollsVote_Blocked(t *testing.T) {
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	voteRepo := testutil.NewMockPollVoteRepository()
	idGen, _ := id.NewGenerator("aidx")
	querySvc := corenote.NewQueryService(noteRepo, nil)
	pollSvc := corepoll.NewService(noteRepo, pollRepo, voteRepo, nil, idGen)
	pollSvc.SetBlockingChecker(blockStub{blocked: true})
	h := NewHandler(noteRepo, nil, nil, querySvc, nil, nil, pollSvc, nil, idGen)
	seedPollNote(noteRepo, pollRepo, model.NoteVisibilityPublic, nil)

	c, rec := newJSONRequest(t, "/api/notes/polls/vote", `{"noteId":"n1","choice":1}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.PollsVote(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "YOU_HAVE_BEEN_BLOCKED")
	assert.Contains(t, rec.Body.String(), "85a5377e-b1e9-4617-b0b9-5bea73331e49")
}

// --- D2: notes/show anonymous content restrictions ---

func newShowHandler(t *testing.T) (*Handler, *testutil.MockNoteRepository) {
	t.Helper()
	noteRepo := testutil.NewMockNoteRepository()
	idGen, _ := id.NewGenerator("aidx")
	querySvc := corenote.NewQueryService(noteRepo, nil)
	h := NewHandler(noteRepo, nil, nil, querySvc, nil, nil, nil, nil, idGen)
	return h, noteRepo
}

func TestShow_RequireSigninAnonymous(t *testing.T) {
	h, noteRepo := newShowHandler(t)
	seedReactionNote(noteRepo, "n1", "public")
	noteRepo.Notes["n1"].User.RequireSigninToViewContents = true
	c, rec := newJSONRequest(t, "/api/notes/show", `{"noteId":"n1"}`)
	// anonymous (no setAuthUser)
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "CONTENT_RESTRICTED_BY_USER")
	assert.Contains(t, rec.Body.String(), "fbcc002d-37d9-4944-a6b0-d9e29f2d33ab")
}

func TestShow_UgcVisibilityNoneAnonymous(t *testing.T) {
	h, noteRepo := newShowHandler(t)
	h.SetUGCVisibility("none")
	seedReactionNote(noteRepo, "n1", "public")
	c, rec := newJSONRequest(t, "/api/notes/show", `{"noteId":"n1"}`)
	require.NoError(t, h.Show(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "CONTENT_RESTRICTED_BY_SERVER")
	assert.Contains(t, rec.Body.String(), "145f88d2-b03d-4087-8143-a78928883c4b")
}

func TestShow_AuthedBypassesRestrictions(t *testing.T) {
	h, noteRepo := newShowHandler(t)
	h.SetUGCVisibility("none")
	seedReactionNote(noteRepo, "n1", "public")
	noteRepo.Notes["n1"].User.RequireSigninToViewContents = true
	c, rec := newJSONRequest(t, "/api/notes/show", `{"noteId":"n1"}`)
	setAuthUser(c, &model.User{ID: "viewer"})
	require.NoError(t, h.Show(c))
	// ログイン済みなら制限は一切適用されず note 本体が返る。
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestShow_PinnedPublicationExceptionTransitions(t *testing.T) {
	h, noteRepo := newShowHandler(t)
	piningRepo := testutil.NewMockUserNotePiningRepository()
	h.SetPiningRepo(piningRepo)
	seedReactionNote(noteRepo, "n1", "public")
	n := noteRepo.Notes["n1"]
	text := "published by pin"
	n.Text = &text
	hidden := 0
	n.User.MakeNotesHiddenBefore = &hidden
	pin := &model.UserNotePining{ID: "p1", UserID: n.UserID, NoteID: n.ID}
	require.NoError(t, piningRepo.Create(pin))

	show := func(viewer *model.User) *httptest.ResponseRecorder {
		c, rec := newJSONRequest(t, "/api/notes/show", `{"noteId":"n1"}`)
		if viewer != nil {
			setAuthUser(c, viewer)
		}
		require.NoError(t, h.Show(c))
		return rec
	}
	responseText := func(rec *httptest.ResponseRecorder) any {
		var response map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
		return response["text"]
	}

	assert.Equal(t, text, responseText(show(nil)), "current public pin bypasses time lockdown for anonymous viewers")

	require.NoError(t, piningRepo.Delete(pin))
	assert.Nil(t, responseText(show(nil)), "unpin immediately restores the ordinary lockdown")

	require.NoError(t, piningRepo.Create(pin))
	n.User.RequireSigninToViewContents = true
	assert.Equal(t, http.StatusBadRequest, show(nil).Code, "pin never bypasses anonymous sign-in requirement")
	assert.Equal(t, text, responseText(show(&model.User{ID: "viewer"})), "authenticated viewer may receive a public pin")

	n.Visibility = model.NoteVisibilityFollowers
	assert.Nil(t, responseText(show(&model.User{ID: "viewer"})), "login and pin together must not bypass followers ACL")
}

func TestShow_AuthenticatedRequireSigninStillUsesVisibilityACL(t *testing.T) {
	h, noteRepo := newShowHandler(t)
	piningRepo := testutil.NewMockUserNotePiningRepository()
	h.SetPiningRepo(piningRepo)
	seedReactionNote(noteRepo, "n1", "followers")
	n := noteRepo.Notes["n1"]
	text := "allowed by ordinary ACL"
	n.Text = &text
	n.User.RequireSigninToViewContents = true
	require.NoError(t, piningRepo.Create(&model.UserNotePining{ID: "p1", UserID: n.UserID, NoteID: n.ID}))
	viewer := &model.User{ID: "viewer"}

	showText := func() any {
		c, rec := newJSONRequest(t, "/api/notes/show", `{"noteId":"n1"}`)
		setAuthUser(c, viewer)
		require.NoError(t, h.Show(c))
		require.Equal(t, http.StatusOK, rec.Code)
		var response map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
		return response["text"]
	}

	n.Mentions = []string{viewer.ID}
	assert.Equal(t, text, showText(), "authenticated reply/mention recipient receives followers note despite requireSignin")
	n.Mentions = nil
	assert.Nil(t, showText(), "authentication and pin do not grant followers access by themselves")

	n.Visibility = model.NoteVisibilitySpecified
	n.VisibleUserIDs = []string{viewer.ID}
	assert.Equal(t, text, showText(), "authenticated direct recipient receives specified note despite requireSignin")
	n.VisibleUserIDs = nil
	assert.Nil(t, showText(), "authentication and pin do not grant specified access by themselves")
}
