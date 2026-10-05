package users

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// failingUserListRepo makes every list lookup look like a database failure.
type failingUserListRepo struct {
	*testutil.MockUserListRepository
	err error
}

func (r *failingUserListRepo) FindByID(string) (*model.UserList, error) { return nil, r.err }

// **DB 障害を「そんなリストは無い」にしない** (#2792)。
//
// 公開リストの複製で障害を 400 にすると「リストが消えた」と読めてしまう。
func TestListsCreateFromPublic_DBFailureIsNot4xx(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetUserListRepo(&failingUserListRepo{
		MockUserListRepository: testutil.NewMockUserListRepository(),
		err:                    errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"),
	})

	rec := postStub(h.ListsCreateFromPublic, `{"listId":"l1","name":"n"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"DB 障害が 4xx に化けている (#2792)")
}

// list を引く endpoint をまとめて固定する。ここが無いと guard を巻き戻しても
// CI が緑のまま通る (#2792)。
func TestUserLists_DBFailureIsNot4xx(t *testing.T) {
	me := &model.User{ID: "u1"}
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")

	newFailing := func(t *testing.T) *Handler {
		t.Helper()
		h, _ := newTestHandler(t)
		h.SetUserListRepo(&failingUserListRepo{
			MockUserListRepository: testutil.NewMockUserListRepository(),
			err:                    dbErr,
		})
		// favorite / unfavorite は repo が未配線だと lookup 前に 204 で抜ける。
		h.SetUserListFavoriteRepo(testutil.NewMockUserListFavoriteRepository())
		return h
	}

	for _, tt := range []struct {
		name string
		run  func(*Handler) int
	}{
		{"users/lists/favorite", func(h *Handler) int {
			return postStub(h.ListsFavorite, `{"listId":"l1"}`, me).Code
		}},
		{"users/lists/unfavorite", func(h *Handler) int {
			return postStub(h.ListsUnfavorite, `{"listId":"l1"}`, me).Code
		}},
		{"users/lists/update", func(h *Handler) int {
			return postStub(h.ListsUpdate, `{"listId":"l1","name":"n"}`, me).Code
		}},
		{"users/lists/update-membership", func(h *Handler) int {
			return postStub(h.ListsUpdateMembership, `{"listId":"l1","userId":"u2"}`, me).Code
		}},
		{"users/lists/get-memberships", func(h *Handler) int {
			return postStub(h.ListsGetMemberships, `{"listId":"l1"}`, me).Code
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, http.StatusInternalServerError, tt.run(newFailing(t)),
				"DB 障害が 4xx に化けている (#2792)")
		})
	}
}

// dbFailingProfileRepo makes every profile lookup look like a database failure.
type dbFailingProfileRepo struct {
	*testutil.MockUserRepository
	err error
}

func (r *dbFailingProfileRepo) FindProfileByUserID(string) (*model.UserProfile, error) {
	return nil, r.err
}

// **DB 障害を「リアクションは公開」に倒さない** (#2799)。
//
// `GetProfile` が err を捨てて nil を返していたので、接続断のあいだ
// `publicReactions = false` の利用者のリアクションが読めていた。not-found を
// 4xx に潰す形と根は同じだが、こちらは**倒れる先が公開側**なので害が重い。
func TestReactions_ProfileDBFailureIsNot200(t *testing.T) {
	h, repo := newTestHandler(t)
	target := &model.User{ID: "target", Username: "t", UsernameLower: "t"}
	if err := repo.Create(target); err != nil {
		t.Fatalf("create: %v", err)
	}
	h.SetUserRepo(repo)
	h.userService = coreuser.NewService(
		&dbFailingProfileRepo{
			MockUserRepository: repo,
			err:                errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"),
		},
		testutil.NewMockNoteRepository(), testutil.NewMockUserNotePiningRepository(), h.idGen)

	rec := postStub(h.Reactions, `{"userId":"target"}`, &model.User{ID: "viewer"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"DB 障害でリアクションが公開扱いになっている (#2799)")
}

// **DB 障害でフォロワー一覧を公開側に倒さない** (#2799)。
//
// `followersVisibility: private` の利用者に対し、`user_profile` の読みが
// 落ちているあいだ誰でも一覧を読めていた。同 function の
// `followingRepo.Exists` は既に fail-closed なので向きが揃っていなかった。
func TestRelationVisibility_ProfileDBFailureIsNot200(t *testing.T) {
	for _, ep := range []struct {
		name string
		run  func(*Handler) int
	}{
		{"users/followers", func(h *Handler) int {
			return postStub(h.Followers, `{"userId":"target"}`, &model.User{ID: "viewer"}).Code
		}},
		{"users/following", func(h *Handler) int {
			return postStub(h.Following, `{"userId":"target"}`, &model.User{ID: "viewer"}).Code
		}},
	} {
		t.Run(ep.name, func(t *testing.T) {
			h, repo := newTestHandler(t)
			target := &model.User{ID: "target", Username: "t", UsernameLower: "t"}
			if err := repo.Create(target); err != nil {
				t.Fatalf("create: %v", err)
			}
			h.SetUserRepo(repo)
			h.userService = coreuser.NewService(
				&dbFailingProfileRepo{
					MockUserRepository: repo,
					err:                errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"),
				},
				testutil.NewMockNoteRepository(), testutil.NewMockUserNotePiningRepository(), h.idGen)

			assert.Equal(t, http.StatusInternalServerError, ep.run(h),
				"DB 障害でフォロワー一覧が公開扱いになっている (#2799)")
		})
	}
}

// dbFailingAcctLookupRepo makes acct (username/host) lookups look like a
// database failure.
type dbFailingAcctLookupRepo struct {
	*testutil.MockUserRepository
	err error
}

func (r *dbFailingAcctLookupRepo) FindByUsernameLower(string, *string) (*model.User, error) {
	return nil, r.err
}

// **acct 引きの DB 障害を 404 に丸めない** (#2792 / #2996)。
//
// #2996 で `ShowByUsername` が DB 障害をそのまま返すようになった (それ以前は
// DB miss と同じ扱いで WebFinger へ落ちていた)。handler が `err != nil` を一律
// NO_SUCH_USER にしていると、接続断が「そんなユーザーは居ない」として返り、
// クライアントからは区別できず監視でも 5xx が立たない。
func TestAcctLookupDBFailureIsNot404(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	const body = `{"username":"alice","host":"remote.example"}`
	for _, ep := range []struct {
		name string
		run  func(*Handler) int
	}{
		{"users/show", func(h *Handler) int {
			return postStub(h.Show, body, &model.User{ID: "viewer"}).Code
		}},
		{"users/followers", func(h *Handler) int {
			return postStub(h.Followers, body, &model.User{ID: "viewer"}).Code
		}},
		{"users/following", func(h *Handler) int {
			return postStub(h.Following, body, &model.User{ID: "viewer"}).Code
		}},
	} {
		t.Run(ep.name, func(t *testing.T) {
			h, repo := newTestHandler(t)
			h.SetUserRepo(repo)
			h.userService = coreuser.NewService(
				&dbFailingAcctLookupRepo{MockUserRepository: repo, err: dbErr},
				testutil.NewMockNoteRepository(), testutil.NewMockUserNotePiningRepository(), h.idGen)

			assert.Equal(t, http.StatusInternalServerError, ep.run(h),
				"DB 障害が NO_SUCH_USER に丸められている (#2792)")
		})
	}
}
