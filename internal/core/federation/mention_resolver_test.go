package federation_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
)

func TestUserMentionResolver_Local(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["uA"] = &model.User{ID: "uA", Username: "alice", UsernameLower: "alice"}
	urls := activitypub.NewURLBuilder("https://example.com")
	r := federation.NewUserMentionResolver(userRepo, urls)

	name, uri, err := r.ResolveMention("uA")
	assert.NoError(t, err)
	assert.Equal(t, "@alice", name)
	assert.Equal(t, "https://example.com/users/uA", uri)
}

func TestUserMentionResolver_Remote(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	host := "remote.example"
	remoteURI := "https://remote.example/users/bob"
	userRepo.Users["uB"] = &model.User{
		ID:            "uB",
		Username:      "bob",
		UsernameLower: "bob",
		Host:          &host,
		URI:           &remoteURI,
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	r := federation.NewUserMentionResolver(userRepo, urls)

	name, uri, err := r.ResolveMention("uB")
	assert.NoError(t, err)
	assert.Equal(t, "@bob@remote.example", name)
	assert.Equal(t, remoteURI, uri)
}

func TestUserMentionResolver_NotFound(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	r := federation.NewUserMentionResolver(userRepo, urls)

	_, _, err := r.ResolveMention("ghost")
	assert.ErrorIs(t, err, activitypub.ErrMentionUserNotFound)
}

func TestUserMentionResolver_RemoteNoURI(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	host := "remote.example"
	userRepo.Users["uB"] = &model.User{
		ID:       "uB",
		Username: "bob",
		Host:     &host,
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	r := federation.NewUserMentionResolver(userRepo, urls)

	_, _, err := r.ResolveMention("uB")
	assert.ErrorIs(t, err, activitypub.ErrMentionUserNotFound)
}

// DB 障害 (NotFound 以外の error) は ErrMentionUserNotFound に変換せず
// 生 error を伝播する (#572 item 2)。
func TestUserMentionResolver_DBError(t *testing.T) {
	userRepo := &failingUserRepoMR{
		MockUserRepository: testutil.NewMockUserRepository(),
		err:                assert.AnError,
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	r := federation.NewUserMentionResolver(userRepo, urls)

	_, _, err := r.ResolveMention("anyid")
	assert.ErrorIs(t, err, assert.AnError)
	assert.NotErrorIs(t, err, activitypub.ErrMentionUserNotFound)
}

// failingUserRepoMR は FindByID で常に指定 error を返す test double。
// MockUserRepository を embed することで他メソッドはそのまま使える。
type failingUserRepoMR struct {
	*testutil.MockUserRepository
	err error
}

func (r *failingUserRepoMR) FindByID(_ string) (*model.User, error) {
	return nil, r.err
}
