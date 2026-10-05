package signup_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	apisignup "github.com/elythia-network/elythia/internal/api/signup"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

var emailTestDB *gorm.DB

func emailDB(t *testing.T) *gorm.DB {
	t.Helper()
	if emailTestDB == nil {
		emailTestDB = testutil.MustOpenTestDB()
		testutil.ApplyMigrations(emailTestDB)
	}
	return emailTestDB
}

func TestEmailAvailable(t *testing.T) {
	type result struct {
		Available bool    `json:"available"`
		Reason    *string `json:"reason"`
	}
	call := func(t *testing.T, h *apisignup.Handler, body string) result {
		t.Helper()
		rec := doPost(h.EmailAvailable, body)
		require.Equal(t, http.StatusOK, rec.Code)
		var got result
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		return got
	}

	t.Run("a fresh address is available with a null reason", func(t *testing.T) {
		h, _, _ := newTestHandler(t)
		got := call(t, h, `{"emailAddress":"new@example.test"}`)

		assert.True(t, got.Available)
		assert.Nil(t, got.Reason)
	})

	t.Run("a malformed address reports format, not a 400", func(t *testing.T) {
		// upstream paramDef は minLength を持たないので、空文字も 400 では
		// なく available:false / reason:"format" になる。
		h, _, _ := newTestHandler(t)

		for _, addr := range []string{"", "nope", "a@"} {
			got := call(t, h, `{"emailAddress":"`+addr+`"}`)
			assert.False(t, got.Available, addr)
			require.NotNil(t, got.Reason, addr)
			assert.Equal(t, "format", *got.Reason, addr)
		}
	})

	t.Run("a banned domain reports banned", func(t *testing.T) {
		h, _, meta := newTestHandler(t)
		meta.Meta = &model.Meta{ID: "x", BannedEmailDomains: model.StringArray{"spam.test"}}

		got := call(t, h, `{"emailAddress":"a@spam.test"}`)
		assert.False(t, got.Available)
		require.NotNil(t, got.Reason)
		assert.Equal(t, "banned", *got.Reason)
	})

	t.Run("a verified duplicate reports used", func(t *testing.T) {
		db := emailDB(t)
		require.NoError(t, db.Exec(`DELETE FROM "user_profile"`).Error)
		require.NoError(t, db.Exec(`DELETE FROM "user"`).Error)

		require.NoError(t, db.Create(&model.User{
			ID: "eu1", Username: "eu1", UsernameLower: "eu1",
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		}).Error)
		taken := "taken@example.test"
		require.NoError(t, db.Create(&model.UserProfile{
			UserID: "eu1", Email: &taken, EmailVerified: true,
		}).Error)

		h, _, _ := newTestHandler(t)
		h.SetEmailAvailabilityDB(db)

		got := call(t, h, `{"emailAddress":"taken@example.test"}`)
		assert.False(t, got.Available)
		require.NotNil(t, got.Reason)
		assert.Equal(t, "used", *got.Reason)
	})

	t.Run("an unverified duplicate does not block registration", func(t *testing.T) {
		// **未認証の行まで見ない。** 見ると、他人のアドレスを登録途中で放置
		// するだけで本人の登録を妨害できる。
		db := emailDB(t)
		require.NoError(t, db.Exec(`DELETE FROM "user_profile"`).Error)
		require.NoError(t, db.Exec(`DELETE FROM "user"`).Error)

		require.NoError(t, db.Create(&model.User{
			ID: "eu2", Username: "eu2", UsernameLower: "eu2",
			AvatarDecorations: datatypes.JSON([]byte("[]")),
		}).Error)
		pending := "pending@example.test"
		require.NoError(t, db.Create(&model.UserProfile{
			UserID: "eu2", Email: &pending, EmailVerified: false,
		}).Error)

		h, _, _ := newTestHandler(t)
		h.SetEmailAvailabilityDB(db)

		got := call(t, h, `{"emailAddress":"pending@example.test"}`)
		assert.True(t, got.Available)
		assert.Nil(t, got.Reason)
	})

	t.Run("a malformed body is a 400", func(t *testing.T) {
		h, _, _ := newTestHandler(t)
		rec := doPost(h.EmailAvailable, `{`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("unwired db skips only the duplicate check", func(t *testing.T) {
		// format と banned は効いたままにする。ここで一律 false にすると
		// DB を渡さないテスト構成でサインアップ画面が何も通らなくなる。
		h, _, meta := newTestHandler(t)
		meta.Meta = &model.Meta{ID: "x", BannedEmailDomains: model.StringArray{"spam.test"}}

		assert.True(t, call(t, h, `{"emailAddress":"ok@example.test"}`).Available)
		assert.False(t, call(t, h, `{"emailAddress":"a@spam.test"}`).Available)
	})
}
