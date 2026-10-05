package role_test

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

// stubUserRolesProvider is a fake role.Service for adapter tests.
type stubUserRolesProvider struct {
	roles map[string][]*model.Role
	err   error
}

func (s *stubUserRolesProvider) GetUserRoles(userID string) ([]*model.Role, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.roles[userID], nil
}

func TestUserRolesLookup_HappyPath(t *testing.T) {
	lookup := role.NewUserRolesLookup(&stubUserRolesProvider{
		roles: map[string][]*model.Role{
			"u1": {{ID: "r1", Name: "Mod"}},
		},
	})
	got := lookup.LookupUserRoles("u1")
	assert.Len(t, got, 1)
	assert.Equal(t, "r1", got[0].ID)
}

// provider が error を返した場合、adapter は nil slice を返す (= entity
// 層で空配列として描画される、安全な fail-open)。
func TestUserRolesLookup_ProviderError(t *testing.T) {
	lookup := role.NewUserRolesLookup(&stubUserRolesProvider{
		err: errors.New("DB down"),
	})
	got := lookup.LookupUserRoles("u1")
	assert.Nil(t, got)
}

// nil provider を渡されても panic せず nil を返す (= entity が空配列描画)。
func TestUserRolesLookup_NilProvider(t *testing.T) {
	lookup := role.NewUserRolesLookup(nil)
	got := lookup.LookupUserRoles("u1")
	assert.Nil(t, got)
}

// nil receiver でも panic せず nil を返す (defensive)。
func TestUserRolesLookup_NilReceiver(t *testing.T) {
	var lookup *role.UserRolesLookupAdapter
	got := lookup.LookupUserRoles("u1")
	assert.Nil(t, got)
}
