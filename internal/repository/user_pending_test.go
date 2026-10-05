package repository

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserPendingRepository_CRUD(t *testing.T) {
	repo := NewUserPendingRepository(testDB)

	p := &model.UserPending{
		ID:       "up_1",
		Code:     "test_code_abc",
		Username: "pendinguser",
		Email:    "pending@example.com",
		Password: "$2a$10$fakehash",
	}
	require.NoError(t, repo.Create(p))
	defer testDB.Exec(`DELETE FROM "user_pending" WHERE id = ?`, p.ID)

	// FindByCode
	found, err := repo.FindByCode("test_code_abc")
	require.NoError(t, err)
	assert.Equal(t, "up_1", found.ID)
	assert.Equal(t, "pendinguser", found.Username)
	assert.Equal(t, "pending@example.com", found.Email)

	// FindByCode: not found
	_, err = repo.FindByCode("nonexistent")
	assert.Error(t, err)

	// Delete
	require.NoError(t, repo.Delete("up_1"))
	_, err = repo.FindByCode("test_code_abc")
	assert.Error(t, err)
}

func TestUserPendingRepository_Errors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewUserPendingRepository(testDB.WithContext(ctx))

	_, err := repo.FindByCode("x")
	assert.Error(t, err)
}

// **期限切れの行を実際に消せること (#3037)。**
//
// `PromotePending` は期限切れを拒否するだけで行を消さないので、`user_pending`
// は放置された登録の**メールアドレスとパスワードハッシュ**を無期限に貯め
// 続けていた。日次の `clean` から呼ぶ。
func TestUserPendingRepository_DeleteOlderThan(t *testing.T) {
	repo := NewUserPendingRepository(testDB)
	defer testDB.Exec(`DELETE FROM "user_pending" WHERE id LIKE 'up_del%'`)

	rows := []*model.UserPending{
		{ID: "up_del_a", Code: "c_del_a", Username: "a", Email: "a@example.com", Password: "h"},
		{ID: "up_del_b", Code: "c_del_b", Username: "b", Email: "b@example.com", Password: "h"},
		// **閾値ちょうどの行。** `<` と `<=` の取り違えを見るために要る。
		{ID: "up_del_c", Code: "c_del_c", Username: "c", Email: "c@example.com", Password: "h"},
		{ID: "up_del_z", Code: "c_del_z", Username: "z", Email: "z@example.com", Password: "h"},
	}
	for _, r := range rows {
		require.NoError(t, repo.Create(r))
	}

	// 閾値より前の 2 件だけ消える。
	n, err := repo.DeleteOlderThan("up_del_c")
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)

	_, err = repo.FindByCode("c_del_a")
	assert.Error(t, err, "閾値より古い行が残っている")
	got, err := repo.FindByCode("c_del_z")
	require.NoError(t, err, "閾値より新しい行を消している")
	assert.Equal(t, "up_del_z", got.ID)
	_, err = repo.FindByCode("c_del_c")
	assert.NoError(t, err, "閾値ちょうどの行を消している")

	// **列に入らない値はクエリごと落とすので、投げる前に弾く (#3025)。**
	n, err = repo.DeleteOlderThan("up\x00del")
	require.NoError(t, err)
	assert.Zero(t, n)
	_, err = repo.FindByCode("c_del_z")
	assert.NoError(t, err, "guard が行を消している")
}
