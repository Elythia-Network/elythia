package blocking_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/blocking"
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

// failingUserRepo makes every user lookup look like a database failure.
type failingUserRepo struct {
	*testutil.MockUserRepository
	err error
}

func (r *failingUserRepo) FindByID(string) (*model.User, error) { return nil, r.err }

// **DB 障害を ErrBlockeeNotFound に丸めないこと** (#2799)。
//
// **materialize より前で分ける。** not-found でない error は materialize しても
// 直らないのに、`EnsureUser` が WebFinger + actor fetch を走らせる。DB 断中は
// 1 リクエストごとに outbound HTTP が 1 発出る。
func TestBlock_DBFailureIsNotBlockeeNotFound(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	idGen, _ := id.NewGenerator("aidx")
	svc := blocking.NewService(
		&failingUserRepo{MockUserRepository: testutil.NewMockUserRepository(), err: dbErr},
		testutil.NewMockBlockingRepository(), idGen)
	// **materialize が走らないことまで見る** (呼び出し回数を見ないと、guard を
	// materialize の後ろに戻す変異が生き残る)。
	mat := &countingUserMaterializer{}
	svc.SetUserMaterializer(mat)

	_, err := svc.Block("u1", "u2")
	assert.Zero(t, mat.asked, "DB 障害で remote fetch を走らせている")
	require.Error(t, err)
	assert.False(t, errors.Is(err, blocking.ErrBlockeeNotFound),
		"DB 障害が not-found に丸められている")
	assert.ErrorIs(t, err, dbErr, "元の error がそのまま返るべき")
}

// Unblock の pair lookup も同じ (#2799)。ここを潰すと「block していない」に化ける。
func TestUnblock_DBFailureIsNotNotBlocking(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	idGen, _ := id.NewGenerator("aidx")
	userRepo := testutil.NewMockUserRepository()
	require.NoError(t, userRepo.Create(&model.User{ID: "u2", Username: "u2", UsernameLower: "u2"}))
	svc := blocking.NewService(userRepo, &dbFailingBlockingRepo{
		MockBlockingRepository: testutil.NewMockBlockingRepository(), err: dbErr,
	}, idGen)

	err := svc.Unblock("u1", "u2")
	require.Error(t, err)
	assert.False(t, errors.Is(err, blocking.ErrNotBlocking),
		"DB 障害が「block していない」に化けている")
}

// dbFailingBlockingRepo makes the pair lookup look like a database failure.
type dbFailingBlockingRepo struct {
	*testutil.MockBlockingRepository
	err error
}

func (r *dbFailingBlockingRepo) FindByPair(string, string) (*model.Blocking, error) {
	return nil, r.err
}
