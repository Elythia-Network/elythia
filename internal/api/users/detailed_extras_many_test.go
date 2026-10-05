package users

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/core/userpack"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// countingPiningRepo counts the per-user and the batched pin lookups.
type countingPiningRepo struct {
	*testutil.MockUserNotePiningRepository
	perUser int
	batched int
}

func (c *countingPiningRepo) ListByUser(userID string) ([]*model.UserNotePining, error) {
	c.perUser++
	return c.MockUserNotePiningRepository.ListByUser(userID)
}

func (c *countingPiningRepo) ListByUsers(userIDs []string) ([]*model.UserNotePining, error) {
	c.batched++
	return c.MockUserNotePiningRepository.ListByUsers(userIDs)
}

// countingPageRepo counts the per-page and the batched page lookups.
type countingPageRepo struct {
	stubPageRepoForPin
	pages   map[string]*model.Page
	single  int
	batched int
}

func (c *countingPageRepo) FindByID(id string) (*model.Page, error) {
	c.single++
	return c.pages[id], nil
}

func (c *countingPageRepo) FindManyByIDs(ids []string) ([]*model.Page, error) {
	c.batched++
	out := make([]*model.Page, 0, len(ids))
	for _, id := range ids {
		if p, ok := c.pages[id]; ok {
			out = append(out, p)
		}
	}
	return out, nil
}

func pinUser(repo *testutil.MockUserRepository, id string) *model.User {
	u := &model.User{ID: id, Username: id, UsernameLower: id, IsExplorable: true,
		AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users[id] = u
	repo.Profiles[id] = &model.UserProfile{UserID: id, Fields: datatypes.JSON([]byte("[]")),
		FollowersVisibility: model.FollowingVisibilityPublic, FollowingVisibility: model.FollowingVisibilityPublic}
	return u
}

func addPinnedNote(t *testing.T, h *Handler, pins *testutil.MockUserNotePiningRepository, owner *model.User, pinID, noteID string) {
	t.Helper()
	require.NoError(t, pins.Create(&model.UserNotePining{ID: pinID, UserID: owner.ID, NoteID: noteID}))
	text := "pinned " + noteID
	h.noteRepo.(*testutil.MockNoteRepository).Notes[noteID] = &model.Note{ID: noteID, UserID: owner.ID, User: owner,
		Text: &text, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
}

func noteIDs(d *entity.UserDetailed) []string {
	out := make([]string, 0, len(d.PinnedNotes))
	for _, n := range d.PinnedNotes {
		out = append(out, n.(entity.NoteEntity).ID)
	}
	return out
}

// 一覧ではピン留めとピン留めのページをまとめて引き、利用者の数だけ問い合わせを
// 増やさない。並びは本家と同じくピン留めの id の降順。
func TestFillDetailedExtrasMany_BatchesAndOrdersPins(t *testing.T) {
	h, userRepo := newTestHandler(t)
	a := pinUser(userRepo, "alice")
	b := pinUser(userRepo, "bob")
	c := pinUser(userRepo, "carol")
	pins := &countingPiningRepo{MockUserNotePiningRepository: testutil.NewMockUserNotePiningRepository()}
	h.SetPiningRepo(pins)
	addPinnedNote(t, h, pins.MockUserNotePiningRepository, a, "pin1", "a-old")
	addPinnedNote(t, h, pins.MockUserNotePiningRepository, a, "pin3", "a-new")
	addPinnedNote(t, h, pins.MockUserNotePiningRepository, b, "pin2", "b-only")
	pageA, pageB := "page-a", "page-b"
	pages := &countingPageRepo{pages: map[string]*model.Page{
		pageA: {ID: pageA, UserID: a.ID, Title: "a"},
		pageB: {ID: pageB, UserID: b.ID, Title: "b"},
	}}
	h.SetPageRepo(pages)
	profA := &model.UserProfile{UserID: a.ID, PinnedPageID: &pageA}
	profB := &model.UserProfile{UserID: b.ID, PinnedPageID: &pageB}

	ds := []entity.UserDetailed{
		entity.PackUserDetailed(a, profA, h.idGen),
		entity.PackUserDetailed(b, profB, h.idGen),
		entity.PackUserDetailed(c, nil, h.idGen),
	}
	targets := []userpack.DetailTarget{
		{User: a, Profile: profA, Detailed: &ds[0]},
		{User: b, Profile: profB, Detailed: &ds[1]},
		{User: c, Detailed: &ds[2]},
	}
	h.FillDetailedExtrasMany(context.Background(), &model.User{ID: "viewer"}, targets)

	assert.Equal(t, 1, pins.batched, "ピン留めは 1 回で引く")
	assert.Zero(t, pins.perUser, "利用者ごとに引かない")
	assert.Equal(t, 1, pages.batched, "ページは 1 回で引く")
	assert.Zero(t, pages.single)
	assert.Equal(t, []string{"a-new", "a-old"}, ds[0].PinnedNoteIDs)
	assert.Equal(t, []string{"a-new", "a-old"}, noteIDs(&ds[0]))
	assert.Equal(t, []string{"b-only"}, ds[1].PinnedNoteIDs)
	assert.Equal(t, []string{"b-only"}, noteIDs(&ds[1]))
	assert.Empty(t, ds[2].PinnedNoteIDs)
	assert.Empty(t, ds[2].PinnedNotes)
	require.NotNil(t, ds[0].PinnedPage)
	require.NotNil(t, ds[1].PinnedPage)
	assert.Nil(t, ds[2].PinnedPage)
}

// 本家 packMany は閲覧者がいないとピン留めを引かない (pinnedNoteIds /
// pinnedNotes は空)。ピン留めのページと移行先は埋める。単体の pack
// (FillDetailedExtras) は匿名にもピン留めを返す。
func TestFillDetailedExtrasMany_AnonymousGetsNoPins(t *testing.T) {
	h, userRepo := newTestHandler(t)
	h.SetUserRepo(userRepo)
	h.SetServerURL("https://local.example")
	a := pinUser(userRepo, "alice")
	pinUser(userRepo, "dest")
	moved := "https://local.example/users/dest"
	a.MovedToURI = &moved
	pins := testutil.NewMockUserNotePiningRepository()
	h.SetPiningRepo(pins)
	addPinnedNote(t, h, pins, a, "pin1", "a-note")
	page := "page-a"
	h.SetPageRepo(&stubPageRepoForPin{page: &model.Page{ID: page, UserID: a.ID, Title: "a"}})
	prof := &model.UserProfile{UserID: a.ID, PinnedPageID: &page}

	many := entity.PackUserDetailed(a, prof, h.idGen)
	h.FillDetailedExtrasMany(context.Background(), nil, []userpack.DetailTarget{{User: a, Profile: prof, Detailed: &many}})
	assert.Empty(t, many.PinnedNoteIDs)
	assert.Empty(t, many.PinnedNotes)
	assert.NotNil(t, many.PinnedPage)
	require.NotNil(t, many.MovedTo)
	assert.Equal(t, "dest", *many.MovedTo)

	single := entity.PackUserDetailed(a, prof, h.idGen)
	h.FillDetailedExtras(context.Background(), nil, a, prof, &single)
	assert.Equal(t, []string{"a-note"}, single.PinnedNoteIDs)
	assert.Equal(t, []string{"a-note"}, noteIDs(&single))
}

// 一覧系の応答 (本家が packMany で UserDetailed / UserDetailedNotMe を組む
// もの) にピン留めが乗る (#3330)。
func TestListEndpoints_CarryPinnedNotes(t *testing.T) {
	viewer := &model.User{ID: "viewer", Username: "viewer"}
	now := time.Now()
	cases := []struct {
		name string
		call func(h *Handler) echo.HandlerFunc
		body string
		// pick extracts the packed alice from the response.
		pick func(t *testing.T, body []byte) map[string]any
	}{
		{"users", func(h *Handler) echo.HandlerFunc { return h.List }, `{}`, pickFromArray},
		{"pinned-users", func(h *Handler) echo.HandlerFunc { return h.PinnedUsers }, `{}`, pickFromArray},
		{"users/search", func(h *Handler) echo.HandlerFunc { return h.Search }, `{"query":"alice"}`, pickFromArray},
		{"users/search-by-username-and-host", func(h *Handler) echo.HandlerFunc { return h.SearchByUsernameAndHost }, `{"username":"alice"}`, pickFromArray},
		{"users/show (userIds)", func(h *Handler) echo.HandlerFunc { return h.Show }, `{"userIds":["alice"]}`, pickFromArray},
		{"users/recommendation", func(h *Handler) echo.HandlerFunc { return h.UserRecommendation }, `{}`, pickFromArray},
		{"users/followers", func(h *Handler) echo.HandlerFunc { return h.Followers }, `{"userId":"bob"}`, pickField("follower")},
		{"users/following", func(h *Handler) echo.HandlerFunc { return h.Following }, `{"userId":"bob"}`, pickField("followee")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, userRepo := newTestHandler(t)
			h.SetUserRepo(userRepo)
			a := pinUser(userRepo, "alice")
			name := "alice"
			a.Name = &name
			a.UpdatedAt = &now
			b := pinUser(userRepo, "bob")
			pins := testutil.NewMockUserNotePiningRepository()
			h.SetPiningRepo(pins)
			addPinnedNote(t, h, pins, a, "pin1", "a-note")
			meta := testutil.NewMockMetaRepository()
			meta.Meta = &model.Meta{PinnedUsers: model.StringArray{"@alice"}}
			h.SetMetaRepo(meta)
			// followers は bob をフォローしている alice、following は bob がフォロー
			// している alice を返す。following service と同じ repo に入れる。
			fRepo := h.followingRepo.(*testutil.MockFollowingRepository)
			require.NoError(t, fRepo.Create(&model.Following{ID: "f1", FollowerID: a.ID, FolloweeID: b.ID}))
			require.NoError(t, fRepo.Create(&model.Following{ID: "f2", FollowerID: b.ID, FolloweeID: a.ID}))

			rec := postStub(tc.call(h), tc.body, viewer)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			u := tc.pick(t, rec.Body.Bytes())
			require.NotNil(t, u, rec.Body.String())
			assert.Equal(t, []any{"a-note"}, u["pinnedNoteIds"])
			notes, _ := u["pinnedNotes"].([]any)
			require.Len(t, notes, 1)
		})
	}
}

func pickFromArray(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(body, &rows))
	for _, r := range rows {
		if r["id"] == "alice" {
			return r
		}
		if u, ok := r["user"].(map[string]any); ok && u["id"] == "alice" {
			return u
		}
	}
	return nil
}

func pickField(field string) func(t *testing.T, body []byte) map[string]any {
	return func(t *testing.T, body []byte) map[string]any {
		t.Helper()
		var rows []map[string]any
		require.NoError(t, json.Unmarshal(body, &rows))
		for _, r := range rows {
			if u, ok := r[field].(map[string]any); ok && u["id"] == "alice" {
				return u
			}
		}
		return nil
	}
}
