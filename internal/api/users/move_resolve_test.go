package users

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// stubUserRepoByURI implements only FindByURI; embedding the interface
// satisfies repository.UserRepository without the unused methods.
type stubUserRepoByURI struct {
	repository.UserRepository
	byURI map[string]*model.User
	err   error
}

func (s stubUserRepoByURI) FindByURI(uri string) (*model.User, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.byURI[uri], nil
}

func TestResolveUserIDByURI(t *testing.T) {
	h := &Handler{}

	// unwired repo -> not resolvable.
	if id, ok := h.resolveUserIDByURI("u://a"); ok || id != "" {
		t.Errorf("nil userRepo: got %q %v, want \"\" false", id, ok)
	}

	h.SetUserRepo(stubUserRepoByURI{byURI: map[string]*model.User{"u://a": {ID: "id_a"}}})

	if id, ok := h.resolveUserIDByURI("u://a"); !ok || id != "id_a" {
		t.Errorf("known URI: got %q %v, want id_a true", id, ok)
	}
	if _, ok := h.resolveUserIDByURI("u://missing"); ok {
		t.Error("unknown URI must not resolve")
	}

	h.SetUserRepo(stubUserRepoByURI{err: errors.New("boom")})
	if _, ok := h.resolveUserIDByURI("u://a"); ok {
		t.Error("repo error must not resolve")
	}
}
