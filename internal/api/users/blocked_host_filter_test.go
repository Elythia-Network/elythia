package users

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// blocked-host filter (セキュリティ監査 2026-09-21)
//
// `blockedHosts` に入れても既存のノートは消えない (upstream は
// generateBlockedHostQueryForNote を全一覧経路に通す)。mk-go は
// `notes/*` / `clips/notes` / `antennas/notes` にしか通しておらず、
// `users/notes` / `users/reactions` / `users/featured-notes` /
// `roles/notes` / `channels/timeline` / `notes/user-list-timeline` の
// 6 経路で出続けていた。

// blockedMeta returns a meta repo that blocks `bad.example`.
func blockedMeta() *testutil.MockMetaRepository {
	m := testutil.NewMockMetaRepository()
	m.Meta = &model.Meta{ID: "x", BlockedHosts: []string{"bad.example"}}
	return m
}

func remoteNoteFrom(id, host string) *model.Note {
	return &model.Note{
		ID: id, UserID: "remote-" + id, Visibility: model.NoteVisibilityPublic,
		UserHost: &host,
	}
}

func TestNotes_DropsBlockedHostNotes(t *testing.T) {
	h, userRepo := newTestHandler(t)
	userRepo.Users["user1"] = &model.User{ID: "user1", Username: "u1", UsernameLower: "u1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	h.SetUserRepo(userRepo)
	h.SetMetaRepo(blockedMeta())

	noteRepo := h.noteRepo.(*testutil.MockNoteRepository)
	noteRepo.Notes["n_bad"] = remoteNoteFrom("n_bad", "bad.example")
	noteRepo.Notes["n_bad"].UserID = "user1"
	noteRepo.Notes["n_ok"] = remoteNoteFrom("n_ok", "good.example")
	noteRepo.Notes["n_ok"].UserID = "user1"

	rec := postStub(h.Notes, `{"userId":"user1"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	ids := idSetOf(t, rec.Body.Bytes())
	assert.False(t, ids["n_bad"], "ブロック済みインスタンスのノートが users/notes に出ている")
	assert.True(t, ids["n_ok"], "ブロックしていない相手のノートは残ること")
}

// **meta が読めないときは 500 に倒す。** nil を返して素通しにすると、DB 障害の
// あいだブロック済みインスタンスのノートが出る (#1544 と同じ fail-closed)。
func TestNotes_BlockedHostFetchErrorIsFatal(t *testing.T) {
	h, userRepo := newTestHandler(t)
	userRepo.Users["user1"] = &model.User{ID: "user1", Username: "u1", UsernameLower: "u1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	h.SetUserRepo(userRepo)
	meta := testutil.NewMockMetaRepository()
	meta.FetchErr = assert.AnError
	h.SetMetaRepo(meta)

	noteRepo := h.noteRepo.(*testutil.MockNoteRepository)
	noteRepo.Notes["n_ok"] = remoteNoteFrom("n_ok", "good.example")
	noteRepo.Notes["n_ok"].UserID = "user1"

	rec := postStub(h.Notes, `{"userId":"user1"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestFeaturedNotes_DropsBlockedHostNotes(t *testing.T) {
	h, userRepo := newTestHandler(t)
	userRepo.Users["user1"] = &model.User{ID: "user1", Username: "u1", UsernameLower: "u1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	h.SetUserRepo(userRepo)
	h.SetMetaRepo(blockedMeta())

	noteRepo := h.noteRepo.(*testutil.MockNoteRepository)
	bad := remoteNoteFrom("f_bad", "bad.example")
	bad.UserID = "user1"
	bad.RenoteCount = 10
	ok := remoteNoteFrom("f_ok", "good.example")
	ok.UserID = "user1"
	ok.RenoteCount = 5
	noteRepo.Notes["f_bad"] = bad
	noteRepo.Notes["f_ok"] = ok

	rec := postStub(h.FeaturedNotes, `{"userId":"user1"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	ids := idSetOf(t, rec.Body.Bytes())
	assert.False(t, ids["f_bad"], "ブロック済みインスタンスのノートが users/featured-notes に出ている")
	assert.True(t, ids["f_ok"], "ブロックしていない相手のノートは残ること")
}

func TestReactions_DropsBlockedHostNotes(t *testing.T) {
	h, userRepo := newTestHandler(t)
	userRepo.Users["user1"] = &model.User{ID: "user1", Username: "u1", UsernameLower: "u1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	h.SetUserRepo(userRepo)
	h.SetMetaRepo(blockedMeta())

	reactionRepo := testutil.NewMockNoteReactionRepository()
	h.SetNoteReactionRepo(reactionRepo)
	bad := remoteNoteFrom("r_bad", "bad.example")
	ok := remoteNoteFrom("r_ok", "good.example")
	reactionRepo.Reactions["x1"] = &model.NoteReaction{
		ID: "x1", UserID: "user1", NoteID: bad.ID, Reaction: "👍", Note: bad,
		User: userRepo.Users["user1"],
	}
	reactionRepo.Reactions["x2"] = &model.NoteReaction{
		ID: "x2", UserID: "user1", NoteID: ok.ID, Reaction: "👍", Note: ok,
		User: userRepo.Users["user1"],
	}

	rec := postStub(h.Reactions, `{"userId":"user1"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	seen := map[string]bool{}
	for _, r := range out {
		if n, ok := r["note"].(map[string]any); ok {
			seen[n["id"].(string)] = true
		}
	}
	assert.False(t, seen["r_bad"], "ブロック済みインスタンスのノートが users/reactions に出ている")
	assert.True(t, seen["r_ok"], "ブロックしていない相手のノートは残ること")
}

// idSetOf collects the ids of a packed note array response.
func idSetOf(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	var out []map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	ids := map[string]bool{}
	for _, n := range out {
		if s, ok := n["id"].(string); ok {
			ids[s] = true
		}
	}
	return ids
}

// **述語が配線を見ていること。** 起動時の critical wiring 検査はこれを読むので、
// 常に true を返すようになると未配線を検出できなくなる。
func TestUsersHandler_HasMetaRepo(t *testing.T) {
	h, _ := newTestHandler(t)
	assert.False(t, h.HasMetaRepo(), "未配線では false")
	h.SetMetaRepo(testutil.NewMockMetaRepository())
	assert.True(t, h.HasMetaRepo(), "配線後は true")
}
