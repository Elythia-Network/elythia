package i

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

// failingSKRepo makes every security key lookup look like a database failure.
type failingSKRepo struct {
	*inMemorySecurityKeyRepo
	err error
}

func (r *failingSKRepo) FindByID(string) (*model.UserSecurityKey, error) { return nil, r.err }

// **DB 障害を「そんなキーは無い」にしない** (#2792)。
//
// セキュリティキーの更新で障害を 400 にすると、利用者は「キーが消えた」と
// 判断して登録し直す。監視でも 5xx が立たない。
func TestUpdateKey_DBFailureIsNot4xx(t *testing.T) {
	h, userRepo := newExtraHandler(t)
	u := &model.User{ID: "u1", Username: "u1", UsernameLower: "u1",
		AvatarDecorations: datatypes.JSON([]byte("[]"))}
	require.NoError(t, userRepo.Create(u))
	h.SetWebAuthn(nil, &failingSKRepo{
		inMemorySecurityKeyRepo: newInMemSKRepo(),
		err:                     errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"),
	})

	rec := postRegistryWithScope(h.TwoFAUpdateKey, `{"credentialId":"c1","name":"x"}`, u, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// dbFailingProfileRepo makes every profile lookup look like a database failure.
type dbFailingProfileRepo struct {
	*testutil.MockUserRepository
	err error
}

func (r *dbFailingProfileRepo) FindProfileByUserID(string) (*model.UserProfile, error) {
	return nil, r.err
}

// **DB 障害を「パスワード未設定」にしない** (#2799)。
//
// `GetProfile` が err を捨てていたので、接続断中の 2FA 操作が
// 「ACCESS_DENIED / No password set.」を返していた。not-found より誤解を招く。
func TestPasswordGate_DBFailureIsNot4xx(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")

	newFailing := func(t *testing.T) (*Handler, *model.User) {
		t.Helper()
		userRepo := &dbFailingProfileRepo{
			MockUserRepository: testutil.NewMockUserRepository(), err: dbErr,
		}
		noteRepo := testutil.NewMockNoteRepository()
		piningRepo := testutil.NewMockUserNotePiningRepository()
		idGen, _ := id.NewGenerator("aidx")
		h := NewHandler(coreuser.NewService(userRepo, noteRepo, piningRepo, idGen), idGen)
		u := &model.User{ID: "u1", Username: "u1", UsernameLower: "u1",
			AvatarDecorations: datatypes.JSON([]byte("[]"))}
		require.NoError(t, userRepo.Create(u))
		return h, u
	}

	// **`i/2fa/{remove-key,update-key}` は入っていない。** `requireWebAuthn` は
	// `webauthnSvc` が nil だと profile lookup の手前で 503 を返すので、この
	// 経路の guard は単体では通せない (実サービスの構築が要る)。
	for _, tt := range []struct {
		name string
		run  func(*Handler, *model.User) int
	}{
		{"i/2fa/register", func(h *Handler, u *model.User) int {
			return postRegistryWithScope(h.TwoFARegister, `{"password":"p"}`, u, nil).Code
		}},
		{"i/2fa/done", func(h *Handler, u *model.User) int {
			return postRegistryWithScope(h.TwoFADone, `{"token":"123456"}`, u, nil).Code
		}},
		{"i/2fa/unregister", func(h *Handler, u *model.User) int {
			return postRegistryWithScope(h.TwoFAUnregister, `{"password":"p"}`, u, nil).Code
		}},
		{"i/change-password", func(h *Handler, u *model.User) int {
			return postRegistryWithScope(h.ChangePassword, `{"currentPassword":"a","newPassword":"bbbbbbbb"}`, u, nil).Code
		}},
		{"i/delete-account", func(h *Handler, u *model.User) int {
			return postRegistryWithScope(h.DeleteAccount, `{"password":"p"}`, u, nil).Code
		}},
		{"i/regenerate-token", func(h *Handler, u *model.User) int {
			return postRegistryWithScope(h.RegenerateToken, `{"password":"p"}`, u, nil).Code
		}},
		{"i/move", func(h *Handler, u *model.User) int {
			return postRegistryWithScope(h.Move, `{"moveToAccount":"@x@remote.example","password":"p"}`, u, nil).Code
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, u := newFailing(t)
			assert.Equal(t, http.StatusInternalServerError, tt.run(h, u),
				"DB 障害が 4xx に化けている (#2799)")
		})
	}
}

// dbFailingDecoRepo makes every decoration lookup look like a database failure.
type dbFailingDecoRepo struct {
	repository.AvatarDecorationRepository
	err error
}

func (r *dbFailingDecoRepo) FindByID(string) (*model.AvatarDecoration, error) {
	return nil, r.err
}

// **DB 障害を「そのデコレーションは存在しない」にしない** (#2792 / #2799)。
func TestUpdate_AvatarDecoration_DBFailureIsNot4xx(t *testing.T) {
	h, repo := newExtraHandler(t)
	u := &model.User{ID: "u1", Username: "u1", UsernameLower: "u1",
		AvatarDecorations: datatypes.JSON([]byte("[]"))}
	require.NoError(t, repo.Create(u))
	repo.Profiles["u1"] = &model.UserProfile{UserID: "u1"}
	h.SetAvatarDecorationRepo(&dbFailingDecoRepo{
		AvatarDecorationRepository: testutil.NewMockAvatarDecorationRepository(),
		err:                        errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"),
	})

	rec := postRegistryWithScope(h.Update, `{"avatarDecorations":[{"id":"d1"}]}`, u, nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"DB 障害が 4xx に化けている (#2792)")
}
