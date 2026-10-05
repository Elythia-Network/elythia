package blocking

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/core/userpack"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// manyExtras records the batch fill and marks every user with a pinned note.
type manyExtras struct {
	calls    int
	viewerID string
	users    []string
}

func (m *manyExtras) FillDetailedExtrasMany(_ context.Context, viewer *model.User, targets []userpack.DetailTarget) {
	m.calls++
	m.viewerID = viewer.ID
	for _, t := range targets {
		m.users = append(m.users, t.User.ID)
		t.Detailed.PinnedNoteIDs = []string{"pin-" + t.User.ID}
	}
}

// 本家 BlockingEntityService は相手を packMany (UserDetailedNotMe) で組むので、blocking/list の
// 利用者にもピン留めと移行先が乗る。まとめて 1 回で埋める (#3330)。
func TestList_FillsDetailExtrasInOneBatch(t *testing.T) {
	h, repo := newHandler(t)
	extras := &manyExtras{}
	h.SetDetailExtras(extras)
	addUser(repo, "alice")
	addUser(repo, "bob")
	addUser(repo, "carol")
	for _, target := range []string{"bob", "carol"} {
		c, _ := newReq(t, `{"userId":"`+target+`"}`)
		setUser(c, "alice")
		require.NoError(t, h.Create(c))
	}

	c, rec := newReq(t, `{}`)
	setUser(c, "alice")
	require.NoError(t, h.List(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 2)
	for _, row := range resp {
		u := row["blockee"].(map[string]any)
		assert.Equal(t, []any{"pin-" + u["id"].(string)}, u["pinnedNoteIds"])
	}
	assert.Equal(t, 1, extras.calls, "N+1 にしない")
	assert.ElementsMatch(t, []string{"bob", "carol"}, extras.users)
	assert.Equal(t, "alice", extras.viewerID)
}

func TestHandler_HasDetailExtras(t *testing.T) {
	h, _ := newHandler(t)
	assert.False(t, h.HasDetailExtras())
	h.SetDetailExtras(&manyExtras{})
	assert.True(t, h.HasDetailExtras())
}
