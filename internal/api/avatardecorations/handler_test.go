package avatardecorations

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
)

var testDB *gorm.DB

func init() {
	testDB = testutil.MustOpenTestDB()
	testutil.ApplyMigrations(testDB)
}

// call invokes the handler as an anonymous caller.
func call(t *testing.T, h *Handler) *httptest.ResponseRecorder {
	t.Helper()
	return callAs(t, h, nil)
}

// callAs invokes the handler as the given user (nil means anonymous).
func callAs(t *testing.T, h *Handler, user *model.User) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	if user != nil {
		c.Set(string(middleware.UserContextKey), user)
	}
	require.NoError(t, h.Get(c))
	return rec
}

var signedIn = &model.User{ID: "viewer"}

func allRoles() (map[string]bool, error) {
	return map[string]bool{"pubRole": true, "privRole": true}, nil
}

func publicRoles() (map[string]bool, error) {
	return map[string]bool{"pubRole": true}, nil
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	return got
}

func seed(t *testing.T, roleIDs ...string) {
	t.Helper()
	require.NoError(t, testDB.Exec(`DELETE FROM "avatar_decoration"`).Error)
	cat := "hats"
	require.NoError(t, testDB.Create(&model.AvatarDecoration{
		ID: "dec1", Name: "hat", URL: "https://example.test/hat.png",
		Description: "a hat", RoleIDs: model.StringArray(roleIDs), Category: &cat,
	}).Error)
}

func TestGet(t *testing.T) {
	t.Run("returns the catalog with every field", func(t *testing.T) {
		seed(t)
		got := decode(t, call(t, NewHandler(testDB, nil, nil)))

		require.Len(t, got, 1)
		for _, k := range []string{
			"id", "name", "description", "url",
			"roleIdsThatCanBeUsedThisDecoration", "category",
		} {
			assert.Contains(t, got[0], k)
		}
		assert.Equal(t, "hats", got[0]["category"])
	})

	t.Run("drops role ids that no longer exist", func(t *testing.T) {
		// 削除済みロールの ID が残ると、管理画面が存在しないロールを表示する
		// (#1543)。
		seed(t, "roleA", "gone")
		h := NewHandler(testDB, func() (map[string]bool, error) {
			return map[string]bool{"roleA": true}, nil
		}, nil)

		got := decode(t, callAs(t, h, signedIn))
		assert.Equal(t, []any{"roleA"}, got[0]["roleIdsThatCanBeUsedThisDecoration"])
	})

	t.Run("role lookup failure keeps the ids verbatim", func(t *testing.T) {
		// **空集合として扱わない。** 全 roleId が落ちて、ロール限定の
		// デコレーションが誰にも使えなくなる。古い ID が残るほうが害が小さい。
		seed(t, "roleA", "gone")
		h := NewHandler(testDB, func() (map[string]bool, error) {
			return nil, errors.New("boom")
		}, nil)

		got := decode(t, callAs(t, h, signedIn))
		assert.Equal(t, []any{"roleA", "gone"}, got[0]["roleIdsThatCanBeUsedThisDecoration"])
	})

	t.Run("unwired role provider keeps the ids verbatim", func(t *testing.T) {
		seed(t, "roleA", "gone")

		got := decode(t, callAs(t, NewHandler(testDB, nil, nil), signedIn))
		assert.Equal(t, []any{"roleA", "gone"}, got[0]["roleIdsThatCanBeUsedThisDecoration"])
	})

	t.Run("anonymous caller sees only public role ids", func(t *testing.T) {
		// 本家 2026.10.0 (#17987): 未ログインには isPublic なロールの ID だけを返す。
		seed(t, "pubRole", "privRole", "gone")
		h := NewHandler(testDB, allRoles, publicRoles)

		got := decode(t, call(t, h))
		assert.Equal(t, []any{"pubRole"}, got[0]["roleIdsThatCanBeUsedThisDecoration"])
	})

	t.Run("signed-in caller sees every existing role id", func(t *testing.T) {
		seed(t, "pubRole", "privRole", "gone")
		h := NewHandler(testDB, allRoles, publicRoles)

		got := decode(t, callAs(t, h, signedIn))
		assert.Equal(t, []any{"pubRole", "privRole"}, got[0]["roleIdsThatCanBeUsedThisDecoration"])
	})

	t.Run("anonymous caller gets no role ids when public lookup fails", func(t *testing.T) {
		// **verbatim に倒さない。** 非公開ロールの ID が匿名に漏れる。
		seed(t, "pubRole", "privRole")
		h := NewHandler(testDB, allRoles, func() (map[string]bool, error) {
			return nil, errors.New("boom")
		})

		got := decode(t, call(t, h))
		assert.Equal(t, []any{}, got[0]["roleIdsThatCanBeUsedThisDecoration"])
	})

	t.Run("anonymous caller gets no role ids when public provider is unwired", func(t *testing.T) {
		seed(t, "pubRole", "privRole")

		got := decode(t, call(t, NewHandler(testDB, allRoles, nil)))
		assert.Equal(t, []any{}, got[0]["roleIdsThatCanBeUsedThisDecoration"])
	})

	t.Run("no rows returns an empty array, not null", func(t *testing.T) {
		require.NoError(t, testDB.Exec(`DELETE FROM "avatar_decoration"`).Error)

		rec := call(t, NewHandler(testDB, nil, nil))
		assert.JSONEq(t, `[]`, rec.Body.String())
	})

	t.Run("unwired db returns an empty array", func(t *testing.T) {
		rec := call(t, NewHandler(nil, nil, nil))
		assert.JSONEq(t, `[]`, rec.Body.String())
	})
}
