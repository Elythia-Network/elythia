package ap

import (
	"context"
	"testing"

	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/model"
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

// 本家 ap/show は pack(user, me, {schema: 'UserDetailedNotMe'}) なので、
// ピン留めと移行先を閲覧者から見た形で埋める (#3330)。
func TestPackUserForAPI_FillsDetailExtras(t *testing.T) {
	h, _, _, _ := newHandler(t)
	extras := &recordingExtras{}
	h.SetDetailExtras(extras)
	assert.True(t, h.HasDetailExtras())

	result := h.packUserForAPI(&model.User{ID: "viewer"}, &model.User{ID: "u1", Username: "alice"}, nil)
	d, ok := result.(entity.UserDetailed)
	require.True(t, ok)
	assert.Equal(t, []string{"pinned"}, d.PinnedNoteIDs)
	require.NotNil(t, extras.viewer)
	assert.Equal(t, "viewer", extras.viewer.ID)
}

func TestHandler_HasDetailExtrasUnwired(t *testing.T) {
	h, _, _, _ := newHandler(t)
	assert.False(t, h.HasDetailExtras())
}
