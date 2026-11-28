package clips

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	coreclip "github.com/elythia-network/elythia/internal/core/clip"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// PackForEmbed packs without a viewer, so viewer-only fields stay out (#2389).
func TestPackForEmbed_OmitsViewerFields(t *testing.T) {
	h, _, _, _ := newHandler(t)
	got := h.PackForEmbed(&model.Clip{ID: "c1", UserID: "alice", Name: "alpha", IsPublic: true})
	raw, err := json.Marshal(got)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, "c1", m["id"])
	assert.NotContains(t, m, "isFavorited")
	assert.NotContains(t, m, "notesCount")
}

// clipLimitPolicy gives every user the same clipLimit.
type clipLimitPolicy float64

func (p clipLimitPolicy) GetUserPolicies(string) map[string]any {
	return map[string]any{"clipLimit": float64(p)}
}

// clipLimit に達した利用者の clips/create は TOO_MANY_CLIPS にし、作らない (#1029)。
func TestCreate_TooManyClips(t *testing.T) {
	repo := testutil.NewMockClipRepository()
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", Name: "alpha"}
	idGen, _ := id.NewGenerator("aidx")
	svc := coreclip.NewService(repo, testutil.NewMockClipNoteRepository(), testutil.NewMockNoteRepository(), idGen, clipLimitPolicy(1), nil)
	h := NewHandler(svc, idGen, nil, nil, nil, nil, nil, nil)
	c, rec := newReq(t, `{"name":"beta"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "TOO_MANY_CLIPS")
	assert.Len(t, repo.Clips, 1, "no clip may be created")
}

// limit は 1〜100。範囲の外は INVALID_PARAM にする。
func TestListAndNotes_LimitOutOfRange(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", Name: "alpha", IsPublic: true}
	for _, body := range []string{`{"limit":0}`, `{"limit":101}`} {
		c, rec := newReq(t, body)
		setUser(c, "alice")
		require.NoError(t, h.List(c))
		assert.Equal(t, http.StatusBadRequest, rec.Code, "clips/list "+body)

		c, rec = newReq(t, `{"clipId":"c1",`+body[1:])
		setUser(c, "alice")
		require.NoError(t, h.Notes(c))
		assert.Equal(t, http.StatusBadRequest, rec.Code, "clips/notes "+body)
	}
}

// 存在しない clip (他人の非公開を含む) のお気に入りは NO_SUCH_CLIP。
func TestClipFavorite_NoSuchClip(t *testing.T) {
	h, clipRepo, favRepo := newStubHandler(t)
	clipRepo.Clips["priv"] = &model.Clip{ID: "priv", UserID: "owner", Name: "p"}
	for _, id := range []string{"missing", "priv"} {
		rec := postStubWithBody(t, h.Favorite, `{"clipId":"`+id+`"}`, "u1")
		assert.Equal(t, http.StatusBadRequest, rec.Code, id)
		assert.Contains(t, rec.Body.String(), "4c2aaeae-80d8-4250-9606-26cb1fdb77a5", id)
		exists, _ := favRepo.Exists("u1", id)
		assert.False(t, exists, id)
	}
}

// favoriteRepo が無い構成では、解除は何もせず、一覧は空を返す。
func TestClipFavorites_NilRepo(t *testing.T) {
	h, _, _, _ := newHandler(t)
	rec := postStubWithBody(t, h.Unfavorite, `{"clipId":"cl1"}`, "u1")
	assert.Equal(t, http.StatusNoContent, rec.Code)
	rec = postStubWithBody(t, h.MyFavorites, `{}`, "u1")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[]`, rec.Body.String())
}

// nilMetaRepo returns no meta row and no error.
type nilMetaRepo struct{ *testutil.MockMetaRepository }

func (nilMetaRepo) Fetch() (*model.Meta, error) { return nil, nil }

// meta の行が無いときは、ブロックしたホストが無いものとして扱い、500 にしない
// (Fetch の error だけを fail-closed にする。TestNotes_MetaFetchErrorIs500)。
func TestNotes_NoMetaRowMeansNoBlockedHosts(t *testing.T) {
	h, repo, _, _ := newHandler(t)
	h.metaRepo = nilMetaRepo{testutil.NewMockMetaRepository()}
	repo.Clips["c1"] = &model.Clip{ID: "c1", UserID: "alice", IsPublic: true}
	c, rec := newReq(t, `{"clipId":"c1"}`)
	setUser(c, "alice")
	require.NoError(t, h.Notes(c))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
