package resetpassword

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// プラグインが管理するアカウント (#3468) は、パスワードの再設定を受け付けない。
// 確認済みのメールを持たせ、拒否が「メールが無いから」で偶然に成り立って
// いないことを示す。普通の利用者は今までどおり。
func TestRequestReset_PluginManagedRefused(t *testing.T) {
	managed := "bot-plugin"
	for _, tc := range []struct {
		name    string
		managed *string
		want    int
	}{
		{name: "plugin managed", managed: &managed, want: 0},
		{name: "ordinary user", managed: nil, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, userRepo, resetRepo := newTestHandler()
			email := "bot@example.com"
			userRepo.users["u1"] = &model.User{ID: "u1", Username: "bot", UsernameLower: "bot", ManagedByPlugin: tc.managed}
			userRepo.profiles["u1"] = &model.UserProfile{UserID: "u1", Email: &email, EmailVerified: true}
			rec := post(h.RequestReset, `{"username":"bot","email":"bot@example.com"}`)
			assert.Equal(t, http.StatusNoContent, rec.Code, "応答は居ない利用者と同じ")
			assert.Len(t, resetRepo.requests, tc.want)
		})
	}
}

func TestReset_PluginManagedRefused(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	managed := "bot-plugin"
	for _, tc := range []struct {
		name    string
		managed *string
		want    int
	}{
		{name: "plugin managed", managed: &managed, want: http.StatusBadRequest},
		{name: "ordinary user", managed: nil, want: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, userRepo, resetRepo := newTestHandler()
			userRepo.users["u1"] = &model.User{ID: "u1", ManagedByPlugin: tc.managed}
			userRepo.profiles["u1"] = &model.UserProfile{UserID: "u1"}
			resetID := idGen.Generate(time.Now())
			resetRepo.requests[resetID] = &model.PasswordResetRequest{ID: resetID, Token: "tok", UserID: "u1"}

			rec := post(h.Reset, `{"token":"tok","password":"newpassword"}`)
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
			assert.Empty(t, resetRepo.requests, "どちらでも要求は使い切る")
			if tc.managed != nil {
				assert.Nil(t, userRepo.profiles["u1"].Password, "パスワードを書かない")
			} else {
				require.NotNil(t, userRepo.profiles["u1"].Password)
			}
		})
	}
}

// 持ち主を引けない DB 障害は 500。「管理するアカウントではない」に倒して
// パスワードを書かない。
func TestReset_OwnerLookupFailure(t *testing.T) {
	idGen, _ := id.NewGenerator("aidx")
	h, _, resetRepo := newTestHandler()
	failing := &failingFindByIDRepo{mockUserRepo: newMockUserRepo()}
	h.userRepo = failing
	failing.profiles["u1"] = &model.UserProfile{UserID: "u1"}
	resetID := idGen.Generate(time.Now())
	resetRepo.requests[resetID] = &model.PasswordResetRequest{ID: resetID, Token: "tok", UserID: "u1"}

	rec := post(h.Reset, `{"token":"tok","password":"newpassword"}`)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Nil(t, failing.profiles["u1"].Password)
}

type failingFindByIDRepo struct{ *mockUserRepo }

func (r *failingFindByIDRepo) FindByID(string) (*model.User, error) {
	return nil, errors.New("db down")
}
