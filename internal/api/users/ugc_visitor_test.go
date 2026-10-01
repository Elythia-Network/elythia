package users

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

// ugcVisibilityForVisitor (upstream 2026.10.0) の匿名 visitor への gate。
//
// 各 endpoint で「匿名 + local で remote が消える」「ログイン中は残る」
// 「匿名 + all は残る」「匿名 + none は空」を見る。gate を消すと local / none の
// 行が、条件を反転すると all / ログイン中の行が落ちる。

const ugcRemoteHost = "remote.example"

// ugcFixture builds a handler with a local user `loc` and a remote user `rem`,
// each with one public note, plus `loc`'s pure renote of `rem`'s note.
func ugcFixture(t *testing.T, policy string) (*Handler, *testutil.MockUserRepository, *testutil.MockNoteRepository) {
	t.Helper()
	h, userRepo := newTestHandler(t)
	h.SetUGCVisibilityLookup(func() string { return policy })
	host := ugcRemoteHost
	userRepo.Users["loc"] = &model.User{ID: "loc", Username: "loc", UsernameLower: "loc", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	userRepo.Users["rem"] = &model.User{ID: "rem", Username: "rem", UsernameLower: "rem", Host: &host, AvatarDecorations: datatypes.JSON([]byte("[]"))}
	userRepo.Profiles["loc"] = &model.UserProfile{UserID: "loc", PublicReactions: true, Fields: datatypes.JSON([]byte("[]"))}
	userRepo.Profiles["rem"] = &model.UserProfile{UserID: "rem", Fields: datatypes.JSON([]byte("[]"))}
	h.SetUserRepo(userRepo)

	noteRepo := h.noteRepo.(*testutil.MockNoteRepository)
	remNoteID := "n_rem"
	remUserID := "rem"
	noteRepo.Notes["n_loc"] = &model.Note{ID: "n_loc", UserID: "loc", Visibility: model.NoteVisibilityPublic}
	noteRepo.Notes["n_rem"] = &model.Note{ID: "n_rem", UserID: "rem", UserHost: &host, Visibility: model.NoteVisibilityPublic}
	noteRepo.Notes["n_rn"] = &model.Note{ID: "n_rn", UserID: "loc", RenoteID: &remNoteID, RenoteUserID: &remUserID, RenoteUserHost: &host, Visibility: model.NoteVisibilityPublic}
	// loc → rem への返信 (get-frequently-replied-users 用)。
	noteRepo.Notes["n_reply"] = &model.Note{ID: "n_reply", UserID: "loc", ReplyID: &remNoteID, ReplyUserID: &remUserID, ReplyUserHost: &host, Visibility: model.NoteVisibilityPublic, Text: ugcStrPtr("re")}

	reactions := testutil.NewMockNoteReactionRepository()
	reactions.Reactions["r1"] = &model.NoteReaction{ID: "r1", UserID: "loc", NoteID: "n_rem", Reaction: "x", Note: noteRepo.Notes["n_rem"], User: userRepo.Users["loc"]}
	h.SetNoteReactionRepo(reactions)
	return h, userRepo, noteRepo
}

func ugcStrPtr(s string) *string { return &s }

// arrayIDs decodes a JSON array response and returns the sorted `id` of each
// element (or of its `user` for frequently-replied-users rows).
func arrayIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var out []map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	ids := make([]string, 0, len(out))
	for _, e := range out {
		if s, ok := e["id"].(string); ok {
			ids = append(ids, s)
			continue
		}
		if u, ok := e["user"].(map[string]any); ok {
			ids = append(ids, u["id"].(string))
		}
	}
	sort.Strings(ids)
	return ids
}

var ugcViewer = &model.User{ID: "viewer"}

func TestNotes_UGCVisibilityForVisitor(t *testing.T) {
	cases := []struct {
		name   string
		policy string
		viewer *model.User
		userID string
		want   []string
	}{
		{name: "anon local hides remote user's notes", policy: "local", userID: "rem", want: []string{}},
		{name: "signed-in local keeps remote user's notes", policy: "local", viewer: ugcViewer, userID: "rem", want: []string{"n_rem"}},
		{name: "anon all keeps remote user's notes", policy: "all", userID: "rem", want: []string{"n_rem"}},
		{name: "anon unknown policy behaves like all", policy: "", userID: "rem", want: []string{"n_rem"}},
		{name: "anon none hides local user's notes", policy: "none", userID: "loc", want: []string{}},
		{name: "signed-in none keeps local user's notes", policy: "none", viewer: ugcViewer, userID: "loc", want: []string{"n_loc", "n_rn"}},
		// filter は行自身の userHost に掛かるので、remote のノートの renote は残る。
		{name: "anon local keeps local user's renote of remote note", policy: "local", userID: "loc", want: []string{"n_loc", "n_rn"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := ugcFixture(t, tc.policy)
			rec := postStub(h.Notes, `{"userId":"`+tc.userID+`"}`, tc.viewer)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, tc.want, arrayIDs(t, rec.Body.Bytes()))
		})
	}
}

func TestShowBulk_UGCVisibilityForVisitor(t *testing.T) {
	cases := []struct {
		name   string
		policy string
		viewer *model.User
		want   []string
	}{
		{name: "anon local omits remote user", policy: "local", want: []string{"loc"}},
		{name: "signed-in local keeps remote user", policy: "local", viewer: ugcViewer, want: []string{"loc", "rem"}},
		{name: "anon all keeps remote user", policy: "all", want: []string{"loc", "rem"}},
		// upstream show.ts の bulk は `none` を見ていない (local の host 条件だけ)。
		{name: "anon none is not applied to bulk", policy: "none", want: []string{"loc", "rem"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := ugcFixture(t, tc.policy)
			rec := postStub(h.Show, `{"userIds":["loc","rem"]}`, tc.viewer)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, tc.want, arrayIDs(t, rec.Body.Bytes()))
		})
	}
}

// TestUsersEndpoints_UGCVisibilityNoneForVisitor lists the anonymous-reachable
// users/* endpoints whose upstream query goes through generateVisibilityQuery
// (anonymous + `none` → `1=0`). Add new ones here.
func TestUsersEndpoints_UGCVisibilityNoneForVisitor(t *testing.T) {
	endpoints := []struct {
		name    string
		handler func(h *Handler) func(c echo.Context) error
		body    string
		nonNone []string
	}{
		{name: "users/notes", handler: func(h *Handler) func(c echo.Context) error { return h.Notes }, body: `{"userId":"loc"}`, nonNone: []string{"n_loc", "n_rn"}},
		{name: "users/reactions", handler: func(h *Handler) func(c echo.Context) error { return h.Reactions }, body: `{"userId":"loc"}`, nonNone: []string{"r1"}},
		{name: "users/get-frequently-replied-users", handler: func(h *Handler) func(c echo.Context) error { return h.GetFrequentlyRepliedUsers }, body: `{"userId":"loc"}`, nonNone: []string{"rem"}},
	}
	for _, ep := range endpoints {
		for _, tc := range []struct {
			policy string
			viewer *model.User
			empty  bool
		}{
			{policy: "none", empty: true},
			{policy: "none", viewer: ugcViewer},
			{policy: "all"},
			{policy: "local"},
		} {
			name := ep.name + "/" + tc.policy
			if tc.viewer != nil {
				name += "/signed-in"
			} else {
				name += "/anon"
			}
			t.Run(name, func(t *testing.T) {
				h, _, _ := ugcFixture(t, tc.policy)
				rec := postStub(ep.handler(h), ep.body, tc.viewer)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				want := ep.nonNone
				if tc.empty {
					want = []string{}
				}
				assert.Equal(t, want, arrayIDs(t, rec.Body.Bytes()))
			})
		}
	}
}

// 上流の検査 (NO_SUCH_USER / IS_REMOTE_USER) は `none` の空配列より先に返る。
func TestUsersEndpoints_UGCVisibilityNoneKeepsUpstreamErrorOrder(t *testing.T) {
	h, _, _ := ugcFixture(t, "none")
	assert.Equal(t, http.StatusBadRequest, postStub(h.Reactions, `{"userId":"rem"}`, nil).Code, "IS_REMOTE_USER")
	assert.Equal(t, http.StatusBadRequest, postStub(h.Reactions, `{"userId":"ghost"}`, nil).Code, "NO_SUCH_USER")
	assert.Equal(t, http.StatusNotFound, postStub(h.GetFrequentlyRepliedUsers, `{"userId":"ghost"}`, nil).Code, "NO_SUCH_USER")
}
