package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingExtras records the viewer and marks the packed user with a pin.
type recordingExtras struct {
	calls  int
	viewer *model.User
}

func (r *recordingExtras) FillDetailedExtras(_ context.Context, viewer, _ *model.User, _ *model.UserProfile, d *entity.UserDetailed) {
	r.calls++
	r.viewer = viewer
	d.PinnedNoteIDs = []string{"pinned"}
}

// 本家 auth/session/userkey は pack(session.userId, null, {schema:
// 'UserDetailedNotMe'}) なので、ピン留めと移行先を匿名の閲覧者として埋める (#3330)。
func TestSessionUserkey_FillsDetailExtras(t *testing.T) {
	h, repo := newTestHandler()
	assert.False(t, h.HasDetailExtras())
	extras := &recordingExtras{}
	h.SetDetailExtras(extras)
	assert.True(t, h.HasDetailExtras())
	repo.apps["s1"] = &model.App{ID: "a1", Secret: "s1"}
	userID := "u1"
	user := &model.User{ID: "u1", Username: "alice"}
	repo.sessions["tok1"] = &model.AuthSession{ID: "sess1", Token: "tok1", AppID: "a1", UserID: &userID, User: user}
	appID := "a1"
	repo.accessTokens["a1:u1"] = &model.AccessToken{ID: "at1", Token: "mytoken", AppID: &appID, UserID: "u1"}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u1"] = user
	h.SetUserRepo(userRepo)

	rec := post(h.SessionUserkey, `{"appSecret":"s1","token":"tok1"}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	userObj := resp["user"].(map[string]any)
	assert.Equal(t, []any{"pinned"}, userObj["pinnedNoteIds"])
	assert.Equal(t, 1, extras.calls)
	assert.Nil(t, extras.viewer)
}

// 本家 miauth の check も pack(token.userId, null, {schema: 'UserDetailedNotMe'})。
func TestMiAuthCheck_FillsDetailExtras(t *testing.T) {
	h, repo := newTestHandler()
	extras := &recordingExtras{}
	h.SetDetailExtras(extras)
	sess := "sess-abc"
	repo.accessTokens["k"] = &model.AccessToken{ID: "at1", Token: "secret-token", UserID: "u1", Session: &sess}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	rec := postMiAuthCheck(h, sess, userRepo)
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	user := out["user"].(map[string]any)
	assert.Equal(t, []any{"pinned"}, user["pinnedNoteIds"])
	assert.Equal(t, 1, extras.calls)
	assert.Nil(t, extras.viewer)
}
