package user_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// dbFailingUserRepo makes every user lookup look like a database failure.
type dbFailingUserRepo struct {
	*testutil.MockUserRepository
	err error
}

func (r *dbFailingUserRepo) FindByID(string) (*model.User, error) { return nil, r.err }

func (r *dbFailingUserRepo) FindByUsernameLower(string, *string) (*model.User, error) {
	return nil, r.err
}

func (r *dbFailingUserRepo) FindProfileByUserID(string) (*model.UserProfile, error) {
	return nil, r.err
}

// **DB 障害を ErrUserNotFound に丸めないこと** (#2799)。
//
// handler は sentinel を 4xx にするので、service で潰すと接続断が
// 「そんなユーザーは居ない」として返る。
func TestUserService_DBFailureIsNotUserNotFound(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(
		&dbFailingUserRepo{MockUserRepository: testutil.NewMockUserRepository(), err: dbErr},
		testutil.NewMockNoteRepository(), testutil.NewMockUserNotePiningRepository(), idGen)

	for _, tt := range []struct {
		name string
		run  func() error
	}{
		{"ShowByID", func() error { _, err := svc.ShowByID("u1"); return err }},
		{"ShowByUsernameDB", func() error { _, err := svc.ShowByUsernameDB("u1", nil); return err }},
		{"GetProfileErr", func() error { _, err := svc.GetProfileErr("u1"); return err }},
		// **#2996 で追加。** ここは DB miss を「リモートへ問い合わせる合図」に
		// 使う経路なので、丸めると 4xx になるだけでなく**外向きリクエストに化ける**
		// (そちらは `TestShowByUsername_DBErrorIsNotTreatedAsMiss` が固定する)。
		{"ShowByUsername", func() error { _, err := svc.ShowByUsername("u1", nil); return err }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run()
			require.Error(t, err)
			assert.False(t, errors.Is(err, coreuser.ErrUserNotFound),
				"DB 障害が not-found に丸められている")
			assert.ErrorIs(t, err, dbErr, "元の error がそのまま返るべき")
		})
	}
}
