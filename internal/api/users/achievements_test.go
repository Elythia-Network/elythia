package users

import (
	"encoding/json"
	"net/http"
	"testing"

	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestAchievements_Success(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	repo.Profiles["u1"] = &model.UserProfile{
		UserID:       "u1",
		Achievements: datatypes.JSON(`[{"name":"notes1","unlockedAt":1000}]`),
	}
	rec := postStub(h.Achievements, `{"userId":"u1"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "notes1")

	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	shapetest.Assert(t, "Achievement", resp[0]) // L3 (#1294)
}

func TestAchievements_NoProfile(t *testing.T) {
	h, repo := newTestHandler(t)
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	rec := postStub(h.Achievements, `{"userId":"u1"}`, nil)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

func TestAchievements_NotFound(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := postStub(h.Achievements, `{"userId":"ghost"}`, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), `"code":"NO_SUCH_USER"`)
}

// 対象の利用者を引けないのが DB の障害なら、NO_SUCH_USER の404でなく500にする (#2792、#4122)。
func TestAchievements_UserLookupErrorIs500(t *testing.T) {
	repo := &failingFindUserRepo{&countingUserRepo{MockUserRepository: testutil.NewMockUserRepository()}}
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(repo, testutil.NewMockNoteRepository(), testutil.NewMockUserNotePiningRepository(), idGen, nil)
	h := NewHandler(svc, nil, testutil.NewMockNoteRepository(), idGen, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	rec := postStub(h.Achievements, `{"userId":"u1"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "NO_SUCH_USER")
}

func TestAchievements_InvalidParam(t *testing.T) {
	h, _ := newTestHandler(t)
	rec := postStub(h.Achievements, `{}`, nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}
