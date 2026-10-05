package federation

import (
	"errors"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
)

type fakeResolver struct {
	called  int
	lastURI string
	err     error
}

func (f *fakeResolver) ForceResolveActor(uri string) (*model.User, error) {
	f.called++
	f.lastURI = uri
	return nil, f.err
}

func TestUpdateRemoteUser_MissingUserID(t *testing.T) {
	h, _ := newHandler(t)
	assert.Equal(t, http.StatusBadRequest, postStub(h.UpdateRemoteUser).Code)
}

func TestUpdateRemoteUser_LocalUserIsNoop(t *testing.T) {
	h, _ := newHandler(t)
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u-local"] = &model.User{ID: "u-local", Username: "me", Host: nil, URI: nil}
	h.SetUserRepo(userRepo)
	h.SetResolver(&fakeResolver{})
	assert.Equal(t, http.StatusNoContent, postBody(h.UpdateRemoteUser, `{"userId":"u-local"}`).Code)
}

func TestUpdateRemoteUser_ResolvesRemoteActor(t *testing.T) {
	h, _ := newHandler(t)
	userRepo := testutil.NewMockUserRepository()
	uri := "https://remote.example/users/alice"
	userRepo.Users["u-r"] = &model.User{ID: "u-r", Username: "alice", URI: &uri}
	h.SetUserRepo(userRepo)
	fr := &fakeResolver{}
	h.SetResolver(fr)

	rec := postBody(h.UpdateRemoteUser, `{"userId":"u-r"}`)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, 1, fr.called)
	assert.Equal(t, uri, fr.lastURI)
}

func TestUpdateRemoteUser_UnknownUser(t *testing.T) {
	h, _ := newHandler(t)
	h.SetUserRepo(testutil.NewMockUserRepository())
	rec := postBody(h.UpdateRemoteUser, `{"userId":"ghost"}`)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestUpdateRemoteUser_ResolverError(t *testing.T) {
	h, _ := newHandler(t)
	userRepo := testutil.NewMockUserRepository()
	uri := "https://remote.example/users/alice"
	userRepo.Users["u-r"] = &model.User{ID: "u-r", URI: &uri}
	h.SetUserRepo(userRepo)
	h.SetResolver(&fakeResolver{err: errors.New("network")})

	rec := postBody(h.UpdateRemoteUser, `{"userId":"u-r"}`)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestUpdateRemoteUser_NoUserRepoUnwired(t *testing.T) {
	h, _ := newHandler(t)
	// userRepo 未注入 → 204 (no-op)
	rec := postBody(h.UpdateRemoteUser, `{"userId":"u1"}`)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestUpdateRemoteUser_NoResolverUnwired(t *testing.T) {
	h, _ := newHandler(t)
	userRepo := testutil.NewMockUserRepository()
	uri := "https://remote.example/users/alice"
	userRepo.Users["u-r"] = &model.User{ID: "u-r", URI: &uri}
	h.SetUserRepo(userRepo)
	rec := postBody(h.UpdateRemoteUser, `{"userId":"u-r"}`)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}
