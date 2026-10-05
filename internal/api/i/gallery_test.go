package i

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubGalleryRepo implements i.GalleryRepository.
type stubGalleryRepo struct {
	posts []*model.GalleryPost
	likes []*model.GalleryLike
	err   error
}

func (s *stubGalleryRepo) ListByUser(_, _, _ string, _, _ int) ([]*model.GalleryPost, error) {
	return s.posts, s.err
}
func (s *stubGalleryRepo) ListLikesByUser(_, _, _ string, _, _ int) ([]*model.GalleryLike, error) {
	return s.likes, s.err
}
func (s *stubGalleryRepo) FindPostsByIDs(_ []string) ([]*model.GalleryPost, error) {
	return s.posts, s.err
}
func (s *stubGalleryRepo) ExistsLike(_, postID string) (bool, error) {
	for _, l := range s.likes {
		if l.PostID == postID {
			return true, nil
		}
	}
	return false, nil
}

func TestGalleryLikes(t *testing.T) {
	h, _ := newExtraHandler(t)
	assert.Equal(t, http.StatusOK, postExtra(h.GalleryLikes, `{}`, stubUser).Code)
}

func TestGalleryPostsI(t *testing.T) {
	h, _ := newExtraHandler(t)
	assert.Equal(t, http.StatusOK, postExtra(h.GalleryPosts, `{}`, stubUser).Code)
}

// --- P4-6 (#166): i/gallery/* ---

func TestGalleryPosts_WithRepo(t *testing.T) {
	h, _ := newExtraHandler(t)
	idGen, _ := id.NewGenerator("aidx")
	postID := idGen.Generate(time.Now())
	h.SetGalleryRepo(&stubGalleryRepo{posts: []*model.GalleryPost{
		{ID: postID, Title: "t", UserID: stubUser.ID},
	}})
	rec := postExtra(h.GalleryPosts, `{}`, stubUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.Equal(t, postID, got[0]["id"])
	// 認証 viewer なので isLiked は present (like していないので false)。
	assert.Equal(t, false, got[0]["isLiked"])
	shapetest.Assert(t, "GalleryPost", got[0]) // L3 (#1322)
}

// #1555 i/gallery/posts は fileIds から DriveFile を解決し、自分が like 済の
// post には isLiked=true を返す。
func TestGalleryPosts_ResolvesFilesAndIsLiked(t *testing.T) {
	h, _ := newExtraHandler(t)
	idGen, _ := id.NewGenerator("aidx")
	postID := idGen.Generate(time.Now())
	h.SetGalleryRepo(&stubGalleryRepo{
		posts: []*model.GalleryPost{{ID: postID, Title: "t", UserID: stubUser.ID, FileIDs: []string{"f1"}}},
		// stub.ExistsLike は likes に postID があれば true を返す。
		likes: []*model.GalleryLike{{ID: "l1", PostID: postID, UserID: stubUser.ID}},
	})
	driveRepo := testutil.NewMockDriveFileRepository()
	owner := stubUser.ID
	driveRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &owner, Name: "a.png", Type: "image/png"}
	h.SetDriveFileRepo(driveRepo)

	rec := postExtra(h.GalleryPosts, `{}`, stubUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	files, ok := got[0]["files"].([]any)
	require.True(t, ok)
	require.Len(t, files, 1)
	assert.Equal(t, "f1", files[0].(map[string]any)["id"])
	assert.Equal(t, true, got[0]["isLiked"])
	shapetest.Assert(t, "GalleryPost", got[0])
}

func TestGalleryLikes_WithRepo(t *testing.T) {
	h, _ := newExtraHandler(t)
	// upstream の `{id, post: GalleryPost}` shape を返す経路 (#493)。
	// stubGalleryRepo.FindPostsByIDs は posts スライスをそのまま返すので
	// PostID が一致するエントリを posts にも入れておく。
	h.SetGalleryRepo(&stubGalleryRepo{
		likes: []*model.GalleryLike{
			{ID: "l1", PostID: "p1", UserID: stubUser.ID},
		},
		// 実 repo の FindPostsByIDs は Preload("User") するため、stub でも post に
		// User を持たせて golden GalleryPost の user 必須を満たす。
		posts: []*model.GalleryPost{
			{ID: "p1", Title: "t", UserID: stubUser.ID, User: stubUser},
		},
	})
	rec := postExtra(h.GalleryLikes, `{}`, stubUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	post, ok := got[0]["post"].(map[string]any)
	require.True(t, ok, "expected post object embedded")
	assert.Equal(t, "p1", post["id"])
	shapetest.Assert(t, "GalleryPost", post) // L3 (#1324)
}
