package muting_test

import (
	"context"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// promotingUserMaterializer creates the user row on demand, mimicking
// core/ephemeral.Materializer.
type promotingUserMaterializer struct {
	repo  *testutil.MockUserRepository
	id    string
	asked []string
	err   error
}

func (m *promotingUserMaterializer) EnsureUser(_ context.Context, userID string) (*model.User, error) {
	m.asked = append(m.asked, userID)
	if m.err != nil {
		return nil, m.err
	}
	host := "remote.example"
	u := &model.User{ID: m.id, Username: "relayonly", Host: &host}
	m.repo.Users[m.id] = u
	return u, nil
}

// リレーでしか観測していない相手を Mute できること。
// muteeId は user への外部キーなので、行が無いと登録できない。
func TestMute_MaterializesEphemeralUser(t *testing.T) {
	svc, userRepo, _ := newMuteService(t)
	userRepo.Users["me"] = &model.User{ID: "me"}
	mat := &promotingUserMaterializer{repo: userRepo, id: "eph-user"}
	svc.SetUserMaterializer(mat)

	_, err := svc.Mute("me", "eph-user", nil)
	require.NoError(t, err, "materialize 後に Mute できること")
	assert.Equal(t, []string{"eph-user"}, mat.asked)
	assert.Contains(t, userRepo.Users, "eph-user", "DB 行が作られていること")
}

// DB にいる相手では materializer を呼ばない (ホットパスに追加コストを載せない)。
func TestMute_DoesNotCallMaterializerForDBUser(t *testing.T) {
	svc, userRepo, _ := newMuteService(t)
	userRepo.Users["me"] = &model.User{ID: "me"}
	userRepo.Users["them"] = &model.User{ID: "them"}
	mat := &promotingUserMaterializer{repo: userRepo, id: "them"}
	svc.SetUserMaterializer(mat)

	_, err := svc.Mute("me", "them", nil)
	require.NoError(t, err)
	assert.Empty(t, mat.asked)
}

func TestMute_MaterializeFailureKeepsError(t *testing.T) {
	svc, userRepo, _ := newMuteService(t)
	userRepo.Users["me"] = &model.User{ID: "me"}
	svc.SetUserMaterializer(&promotingUserMaterializer{repo: userRepo, err: errors.New("gone")})

	_, err := svc.Mute("me", "ghost", nil)
	assert.Error(t, err)
}

func TestMute_NoMaterializerWired(t *testing.T) {
	svc, userRepo, _ := newMuteService(t)
	userRepo.Users["me"] = &model.User{ID: "me"}
	_, err := svc.Mute("me", "ghost", nil)
	assert.Error(t, err)
}
