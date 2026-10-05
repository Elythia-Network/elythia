package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingExtras records the viewer and marks the packed user with a pin.
type recordingExtras struct {
	called bool
	viewer *model.User
}

func (r *recordingExtras) FillDetailedExtras(_ context.Context, viewer, _ *model.User, _ *model.UserProfile, d *entity.UserDetailed) {
	r.called = true
	r.viewer = viewer
	d.PinnedNoteIDs = []string{"pinned"}
}

// 本家 find-by-email は pack(user, null, {schema: 'UserDetailedNotMe'}) なので、
// ピン留めと移行先を匿名の閲覧者として埋める (#3330)。
func TestAccountsFindByEmail_FillsDetailExtras(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	assert.False(t, h.HasDetailExtras())
	extras := &recordingExtras{}
	h.SetDetailExtras(extras)
	assert.True(t, h.HasDetailExtras())
	email := "alice@example.com"
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Profiles["alice"] = &model.UserProfile{UserID: "alice", Email: &email}

	rec := doPost(h.AccountsFindByEmail, `{"email":"alice@example.com"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, []any{"pinned"}, resp["pinnedNoteIds"])
	assert.True(t, extras.called)
	assert.Nil(t, extras.viewer, "本家は me=null で pack する")
}
