package repository

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// #2973: 凍結の由来を記録する表。migration が実 DB で作られることと、
// upsert で置き換わることを見る。
func TestUserSuspensionOriginRepository_RoundTrip(t *testing.T) {
	user := insertTestUser(t, "u_so_a1", "soa1")
	defer cleanupUser(t, user.ID)

	r := NewUserSuspensionOriginRepository(testDB)

	got, err := r.Origin(user.ID)
	require.NoError(t, err)
	require.Empty(t, got, "記録が無ければ空 (not-found は error にしない)")

	require.NoError(t, r.Set(user.ID, model.SuspensionOriginRemote))
	got, err = r.Origin(user.ID)
	require.NoError(t, err)
	require.Equal(t, model.SuspensionOriginRemote, got)

	// **upsert で置き換わる。** モデレーターが後から操作したら local に上書き
	// されないと、リモートに解除されてしまう。
	require.NoError(t, r.Set(user.ID, model.SuspensionOriginLocal))
	got, err = r.Origin(user.ID)
	require.NoError(t, err)
	require.Equal(t, model.SuspensionOriginLocal, got)

	require.NoError(t, r.Clear(user.ID))
	got, err = r.Origin(user.ID)
	require.NoError(t, err)
	require.Empty(t, got)
}

// 空の userID は no-op (呼び出し側が nil ガードを書かなくて済むように)。
func TestUserSuspensionOriginRepository_EmptyUserID(t *testing.T) {
	r := NewUserSuspensionOriginRepository(testDB)
	got, err := r.Origin("")
	require.NoError(t, err)
	require.Empty(t, got)
	require.NoError(t, r.Set("", model.SuspensionOriginLocal))
	require.NoError(t, r.Clear(""))
}

// **user を消したら由来も消える** (FK の ON DELETE CASCADE)。孤児が残ると、
// 同じ ID が再利用されたときに古い判断が効いてしまう。
func TestUserSuspensionOriginRepository_CascadesOnUserDelete(t *testing.T) {
	user := insertTestUser(t, "u_so_a2", "soa2")
	r := NewUserSuspensionOriginRepository(testDB)
	require.NoError(t, r.Set(user.ID, model.SuspensionOriginRemote))

	cleanupUser(t, user.ID)

	var count int64
	require.NoError(t, testDB.Model(&model.UserSuspensionOrigin{}).
		Where(`"userId" = ?`, user.ID).Count(&count).Error)
	require.Zero(t, count, "user を消しても由来が残っている (CASCADE が効いていない)")
}
