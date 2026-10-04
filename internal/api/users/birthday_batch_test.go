package users_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/shiroha-a/mk/internal/api/users"
	corefollowing "github.com/shiroha-a/mk/internal/core/following"
	coreuser "github.com/shiroha-a/mk/internal/core/user"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingUserRepo counts the user and profile lookups. FindManyByIDs returns
// the rows in reverse order so a handler that follows the repository order
// instead of its own row order is caught.
type countingUserRepo struct {
	*testutil.MockUserRepository
	findByID       int
	findManyByID   int
	profileLookups int
}

func (c *countingUserRepo) FindByID(id string) (*model.User, error) {
	c.findByID++
	return c.MockUserRepository.FindByID(id)
}

func (c *countingUserRepo) FindManyByIDs(ids []string) ([]*model.User, error) {
	c.findManyByID++
	got, err := c.MockUserRepository.FindManyByIDs(ids)
	out := make([]*model.User, 0, len(got))
	for i := len(got) - 1; i >= 0; i-- {
		out = append(out, got[i])
	}
	return out, err
}

func (c *countingUserRepo) FindProfileByUserID(userID string) (*model.UserProfile, error) {
	c.profileLookups++
	return c.MockUserRepository.FindProfileByUserID(userID)
}

func (c *countingUserRepo) FindProfilesByUserIDs(userIDs []string) ([]*model.UserProfile, error) {
	c.profileLookups++
	return c.MockUserRepository.FindProfilesByUserIDs(userIDs)
}

// 誕生日の一覧の相手は利用者だけを 1 回でまとめて引く (行ごとの ShowByID は N+1、
// 応答は UserLite なので profile は要らない、#3330)。並びは誕生日の順 (rows の順)
// のままで、引けない相手の行は飛ばす。
func TestGetFollowingUsersByBirthday_FetchesUsersInOneBatch(t *testing.T) {
	mock := testutil.NewMockUserRepository()
	repo := &countingUserRepo{MockUserRepository: mock}
	fRepo := testutil.NewMockFollowingRepository()
	idGen, _ := id.NewGenerator("aidx")
	h := users.NewHandler(
		coreuser.NewService(repo, testutil.NewMockNoteRepository(), testutil.NewMockUserNotePiningRepository(), idGen),
		corefollowing.NewService(repo, fRepo, testutil.NewMockFollowRequestRepository(), idGen),
		testutil.NewMockNoteRepository(), idGen)
	h.SetFollowingRepo(fRepo)
	mock.Users["me"] = &model.User{ID: "me"}
	for fe, bd := range map[string]string{"fe1": "1990-05-12", "fe2": "1991-05-10", "fe3": "1992-05-11"} {
		mock.Users[fe] = &model.User{ID: fe, Username: fe}
		fRepo.Followings["f-"+fe] = &model.Following{ID: "f-" + fe, FollowerID: "me", FolloweeID: fe}
		fRepo.Birthdays[fe] = bd
	}
	// 行はあるが利用者が消えている相手。
	fRepo.Followings["f-gone"] = &model.Following{ID: "f-gone", FollowerID: "me", FolloweeID: "gone"}
	fRepo.Birthdays["gone"] = "1990-05-13"

	rec := postP189(h.GetFollowingUsersByBirthday,
		`{"birthday":{"begin":{"month":5,"day":1},"end":{"month":5,"day":31}}}`, &model.User{ID: "me"})
	require.Equal(t, http.StatusOK, rec.Code)
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	ids := make([]any, 0, len(out))
	for _, row := range out {
		assert.Equal(t, row["id"], row["user"].(map[string]any)["id"])
		ids = append(ids, row["id"])
	}
	assert.Equal(t, []any{"fe2", "fe3", "fe1"}, ids, "誕生日の順のまま、消えた相手は飛ばす")
	assert.Equal(t, 0, repo.findByID, "利用者ごとに引かない")
	assert.Equal(t, 1, repo.findManyByID, "1 回でまとめて引く")
	assert.Equal(t, 0, repo.profileLookups, "UserLite なので profile は引かない")
}
