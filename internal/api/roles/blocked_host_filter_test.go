package roles_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blocked-host filter (セキュリティ監査 2026-09-21)
//
// `blockedHosts` に入れても `roles/notes` からは既存のノートが出続けていた。
// upstream は generateBlockedHostQueryForNote を全一覧経路に通す。

func rolesBlockedHostNotes() []*model.Note {
	bad, good := "bad.example", "good.example"
	return []*model.Note{
		{ID: "n_bad", UserID: "u_bad", Visibility: "public", UserHost: &bad},
		{ID: "n_ok", UserID: "u_ok", Visibility: "public", UserHost: &good},
	}
}

func TestRolesNotes_DropsBlockedHostNotes(t *testing.T) {
	meta := testutil.NewMockMetaRepository()
	meta.Meta = &model.Meta{ID: "x", BlockedHosts: []string{"bad.example"}}
	h, roleRepo := newTestHandlerWithMeta(t, meta)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Public", IsPublic: true, IsExplorable: true}
	h.SetNotesQuery(&mockRoleNotesQuery{Notes: rolesBlockedHostNotes()})

	rec := doPost(h.Notes, `{"roleId":"r1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var arr []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &arr))
	ids := map[string]bool{}
	for _, n := range arr {
		ids[n["id"].(string)] = true
	}
	assert.False(t, ids["n_bad"], "ブロック済みインスタンスのノートが roles/notes に出ている")
	assert.True(t, ids["n_ok"], "ブロックしていない相手のノートは残ること")
}

// **meta が読めないときは 500 に倒す** (#1544 と同じ fail-closed)。
func TestRolesNotes_BlockedHostFetchErrorIsFatal(t *testing.T) {
	meta := testutil.NewMockMetaRepository()
	meta.FetchErr = assert.AnError
	h, roleRepo := newTestHandlerWithMeta(t, meta)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Public", IsPublic: true, IsExplorable: true}
	h.SetNotesQuery(&mockRoleNotesQuery{Notes: rolesBlockedHostNotes()})

	rec := doPost(h.Notes, `{"roleId":"r1"}`)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}
