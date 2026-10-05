package admin_test

import (
	"errors"
	"net/http"
	"testing"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// **system アカウントを凍結させないこと。**
//
// 凍結すると `OnUserDeleted` が既知の全リモート inbox へ `Delete(actor)` を
// 配る。system アカウントはロールを持たないので `RolePrivileges` の判定を
// 素通りしていた。削除経路と資格情報リセット経路は既に塞いである。
func TestSuspendUser_RefusesSystemAccount(t *testing.T) {
	h, users, _, _ := newTestHandler(t)
	users.Users["sys1"] = &model.User{ID: "sys1", Username: "instance.actor"}

	rec := doPost(h.SuspendUser, `{"userId":"sys1"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "ACCESS_DENIED")
	assert.False(t, users.Users["sys1"].IsSuspended, "凍結されていないこと")
}

// 普通の利用者は従来どおり凍結できること。
func TestSuspendUser_AllowsOrdinaryAccount(t *testing.T) {
	h, users, _, _ := newTestHandler(t)
	users.Users["u1"] = &model.User{ID: "u1", Username: "alice"}

	rec := doPost(h.SuspendUser, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
}

// **meta を読めないときに root の削除を通さないこと。**
//
// 本番の root は `isRoot = false` なので meta が唯一の判定材料。読めない窓で
// 通すと `user` 行が消え、`meta.rootUserId` が消えた ID を指したまま残って
// API 経由で復旧できなくなる。
func TestAccountsDelete_RefusesWhenMetaUnavailable(t *testing.T) {
	h, users, meta, _ := newTestHandler(t)
	users.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	meta.FetchErr = errors.New("db down")

	rec := doPost(h.AccountsDelete, `{"userId":"u1"}`, adminUser)
	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"判定できないときは 500 に倒すこと (削除を通さない)")
	assert.False(t, users.Users["u1"].IsDeleted, "削除されていないこと")
}

var _ = apiadmin.Handler{}
