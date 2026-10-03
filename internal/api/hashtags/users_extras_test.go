package hashtags_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/core/userpack"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// manyExtras records the batch fill and marks every user with a pinned note.
type manyExtras struct {
	calls  int
	viewer *model.User
}

func (m *manyExtras) FillDetailedExtrasMany(_ context.Context, viewer *model.User, targets []userpack.DetailTarget) {
	m.calls++
	m.viewer = viewer
	for _, t := range targets {
		t.Detailed.PinnedNoteIDs = []string{"pin-" + t.User.ID}
	}
}

// 本家 hashtags/users は packMany(users, me, {schema: 'UserDetailed'}) なので、
// ピン留めと移行先もまとめて埋める (#3330)。閲覧者 (匿名なら nil) をそのまま渡す。
func TestUsers_FillsDetailExtrasInOneBatch(t *testing.T) {
	now := time.Now()
	seedTagUser(t, "u_htu_px1", "htupx1", []string{"pinxtag"}, nil, false, 2, now)
	seedTagUser(t, "u_htu_px2", "htupx2", []string{"pinxtag"}, nil, false, 1, now)
	h := newHandler()
	extras := &manyExtras{}
	h.SetDetailExtras(extras)

	rec := doPostAs(h.Users, `{"tag":"pinxtag","sort":"+follower"}`, "viewer1")
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 2)
	for _, r := range rows {
		assert.Equal(t, []any{"pin-" + r["id"].(string)}, r["pinnedNoteIds"])
	}
	assert.Equal(t, 1, extras.calls, "N+1 にしない")
	require.NotNil(t, extras.viewer)
	assert.Equal(t, "viewer1", extras.viewer.ID)

	rec = doPost(h.Users, `{"tag":"pinxtag","sort":"+follower"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Nil(t, extras.viewer, "匿名は nil のまま渡す (本家 packMany は匿名にピン留めを出さない)")
}

func TestHandler_HasDetailExtras(t *testing.T) {
	h := newHandler()
	assert.False(t, h.HasDetailExtras())
	h.SetDetailExtras(&manyExtras{})
	assert.True(t, h.HasDetailExtras())
}
