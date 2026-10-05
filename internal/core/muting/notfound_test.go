package muting_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/muting"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// countingUserMaterializer records how many times it was asked to fetch a
// remote user.
type countingUserMaterializer struct{ asked int }

func (m *countingUserMaterializer) EnsureUser(context.Context, string) (*model.User, error) {
	m.asked++
	return nil, errors.New("unreachable")
}

type dbFailingUserRepo struct {
	*testutil.MockUserRepository
	err error
}

func (r *dbFailingUserRepo) FindByID(string) (*model.User, error) { return nil, r.err }

type dbFailingMutingRepo struct {
	*testutil.MockMutingRepository
	err error
}

func (r *dbFailingMutingRepo) FindByPair(string, string) (*model.Muting, error) {
	return nil, r.err
}

// **DB 障害を ErrMuteeNotFound / ErrNotMuting に丸めないこと** (#2799)。
//
// **materialize より前で分ける。** not-found でない error は materialize しても
// 直らないのに、`EnsureUser` が WebFinger + actor fetch を走らせる。
func TestMuting_DBFailureIsNotNotFound(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	idGen, _ := id.NewGenerator("aidx")

	t.Run("Mute", func(t *testing.T) {
		svc := muting.NewService(
			&dbFailingUserRepo{MockUserRepository: testutil.NewMockUserRepository(), err: dbErr},
			testutil.NewMockMutingRepository(), idGen)
		// **materialize が走らないことまで見る** (上の理由と同じ)。
		mat := &countingUserMaterializer{}
		svc.SetUserMaterializer(mat)

		_, err := svc.Mute("u1", "u2", nil)
		assert.Zero(t, mat.asked, "DB 障害で remote fetch を走らせている")
		require.Error(t, err)
		assert.False(t, errors.Is(err, muting.ErrMuteeNotFound),
			"DB 障害が not-found に丸められている")
		assert.ErrorIs(t, err, dbErr)
	})

	t.Run("Unmute", func(t *testing.T) {
		userRepo := testutil.NewMockUserRepository()
		require.NoError(t, userRepo.Create(&model.User{ID: "u2", Username: "u2", UsernameLower: "u2"}))
		svc := muting.NewService(userRepo, &dbFailingMutingRepo{
			MockMutingRepository: testutil.NewMockMutingRepository(), err: dbErr,
		}, idGen)

		err := svc.Unmute("u1", "u2")
		require.Error(t, err)
		assert.False(t, errors.Is(err, muting.ErrNotMuting),
			"DB 障害が「mute していない」に化けている")
	})
}
