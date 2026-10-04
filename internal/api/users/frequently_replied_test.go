package users

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	corefollowing "github.com/shiroha-a/mk/internal/core/following"
	coreuser "github.com/shiroha-a/mk/internal/core/user"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

// countingUserRepo counts the user lookups a handler makes.
type countingUserRepo struct {
	*testutil.MockUserRepository
	findByID, findMany, findProfile int
}

func (r *countingUserRepo) FindByID(id string) (*model.User, error) {
	r.findByID++
	return r.MockUserRepository.FindByID(id)
}

func (r *countingUserRepo) FindManyByIDs(ids []string) ([]*model.User, error) {
	r.findMany++
	return r.MockUserRepository.FindManyByIDs(ids)
}

func (r *countingUserRepo) FindProfileByUserID(id string) (*model.UserProfile, error) {
	r.findProfile++
	return r.MockUserRepository.FindProfileByUserID(id)
}

func newFrequentlyRepliedHandler(t *testing.T) (*Handler, *countingUserRepo, *testutil.MockNoteRepository) {
	t.Helper()
	repo := &countingUserRepo{MockUserRepository: testutil.NewMockUserRepository()}
	noteRepo := testutil.NewMockNoteRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(repo, noteRepo, testutil.NewMockUserNotePiningRepository(), idGen)
	fSvc := corefollowing.NewService(repo, testutil.NewMockFollowingRepository(), testutil.NewMockFollowRequestRepository(), idGen)
	return NewHandler(svc, fSvc, noteRepo, idGen), repo, noteRepo
}

// seedReplies makes author reply count times to each target.
func seedReplies(noteRepo *testutil.MockNoteRepository, author string, counts map[string]int) {
	rid := "parent"
	n := 0
	for target, c := range counts {
		for i := 0; i < c; i++ {
			tgt := target
			nid := fmt.Sprintf("n%03d", n)
			n++
			noteRepo.Notes[nid] = &model.Note{ID: nid, UserID: author, ReplyID: &rid, ReplyUserID: &tgt, Visibility: model.NoteVisibilityPublic}
		}
	}
}

func addReplyUser(repo *countingUserRepo, id string) *model.User {
	u := &model.User{ID: id, Username: id, UsernameLower: id, AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users[id] = u
	repo.Profiles[id] = &model.UserProfile{UserID: id}
	return u
}

// 本家 get-frequently-replied-users は返信先を packMany(UserDetailed) で組むので、
// 返信先に閲覧者本人が居ればその行は MeDetailed になる。形は {user, weight} の
// まま。利用者と profile はまとめて引き、返信先の数に比例して問い合わせを増やさない (#3330)。
func TestGetFrequentlyRepliedUsers_BatchesAndPromotesSelf(t *testing.T) {
	h, repo, noteRepo := newFrequentlyRepliedHandler(t)
	addReplyUser(repo, "author")
	viewer := addReplyUser(repo, "viewer")
	counts := map[string]int{"viewer": 4}
	for i := 0; i < 5; i++ {
		uid := fmt.Sprintf("t%d", i)
		addReplyUser(repo, uid)
		counts[uid] = i + 1
	}
	seedReplies(noteRepo, "author", counts)
	repo.findByID, repo.findMany, repo.findProfile = 0, 0, 0

	rec := postStub(h.GetFrequentlyRepliedUsers, `{"userId":"author"}`, viewer)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 6)

	assert.Equal(t, 1, repo.findByID, "対象の存在確認の 1 回だけ (返信先ごとに引かない)")
	assert.Equal(t, 1, repo.findMany, "返信先はまとめて 1 回")
	assert.Equal(t, 1, repo.findProfile, "profile は存在確認の分だけ (返信先はまとめて引く)")

	byID := map[string]map[string]any{}
	for _, r := range rows {
		assert.Len(t, r, 2, "形は {user, weight}")
		u := r["user"].(map[string]any)
		byID[u["id"].(string)] = r
	}
	self := byID["viewer"]
	require.NotNil(t, self)
	assert.Contains(t, self["user"], "twoFactorBackupCodesStock", "閲覧者本人の行は MeDetailed")
	assert.InDelta(t, 4.0/5.0, self["weight"], 1e-9)
	other := byID["t4"]
	require.NotNil(t, other)
	assert.NotContains(t, other["user"], "twoFactorBackupCodesStock")
	assert.Equal(t, 1.0, other["weight"])
	assert.InDelta(t, 1.0/5.0, byID["t0"]["weight"], 1e-9)
}

// 返信先の一括解決の失敗は「居ない」に潰さず 500 にする。
func TestGetFrequentlyRepliedUsers_BatchLookupErrorIs500(t *testing.T) {
	h, repo, noteRepo := newFrequentlyRepliedHandler(t)
	addReplyUser(repo, "author")
	addReplyUser(repo, "t1")
	seedReplies(noteRepo, "author", map[string]int{"t1": 1})
	repo.FindManyByIDsErr = errors.New("connection refused")

	rec := postStub(h.GetFrequentlyRepliedUsers, `{"userId":"author"}`, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}
