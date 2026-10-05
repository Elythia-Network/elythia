package notes

import (
	"encoding/json"
	"net/http"
	"testing"

	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blocked-host filter (セキュリティ監査 2026-09-21)
//
// list のメンバーは利用者が自由に編集できるので、`blockedHosts` に入れた後も
// リストに残っている相手のノートが `notes/user-list-timeline` から出続けて
// いた。`ListByUserList` の SQL push-down には blocked-host が入っていない。

// newUserListTimelineHandler builds a handler whose ListByUserList returns rows
// verbatim (the bare mock panics on the muting subquery, see
// TestUserListTimeline_Success).
func newUserListTimelineHandler(t *testing.T, rows []*model.Note) *Handler {
	t.Helper()
	noteRepo := &userListNotesRepo{MockNoteRepository: testutil.NewMockNoteRepository(), rows: rows}
	idGen, _ := id.NewGenerator("aidx")
	querySvc := corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository())
	h := NewHandler(noteRepo,
		corenote.NewCreateService(noteRepo, testutil.NewMockPollRepository(), idGen, nil),
		corenote.NewDeleteService(noteRepo), querySvc, nil, nil, nil, nil, idGen)
	listRepo := testutil.NewMockUserListRepository()
	listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "u1", Name: "my list"}
	h.SetUserListRepo(listRepo)
	return h
}

func userListBlockedHostRows() []*model.Note {
	bad, good := "bad.example", "good.example"
	return []*model.Note{
		{ID: "n_bad", UserID: "m_bad", Visibility: "public", UserHost: &bad, User: &model.User{ID: "m_bad", Host: &bad}},
		{ID: "n_ok", UserID: "m_ok", Visibility: "public", UserHost: &good, User: &model.User{ID: "m_ok", Host: &good}},
	}
}

func TestUserListTimeline_DropsBlockedHostNotes(t *testing.T) {
	h := newUserListTimelineHandler(t, userListBlockedHostRows())
	meta := testutil.NewMockMetaRepository()
	meta.Meta = &model.Meta{ID: "x", BlockedHosts: []string{"bad.example"}}
	h.SetMetaRepo(meta)

	rec := postExtra(h.UserListTimeline, `{"listId":"l1"}`, &model.User{ID: "u1"})
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ids := map[string]bool{}
	for _, n := range resp {
		ids[n["id"].(string)] = true
	}
	assert.False(t, ids["n_bad"], "ブロック済みインスタンスのノートが user-list-timeline に出ている")
	assert.True(t, ids["n_ok"], "ブロックしていない相手のノートは残ること")
}

// **meta が読めないときは 500 に倒す** (#1544 と同じ fail-closed)。
func TestUserListTimeline_BlockedHostFetchErrorIsFatal(t *testing.T) {
	h := newUserListTimelineHandler(t, userListBlockedHostRows())
	meta := testutil.NewMockMetaRepository()
	meta.FetchErr = assert.AnError
	h.SetMetaRepo(meta)

	rec := postExtra(h.UserListTimeline, `{"listId":"l1"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}
