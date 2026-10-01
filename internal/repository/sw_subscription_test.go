package repository

import (
	"sort"
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSwSubscriptionRepository_Full(t *testing.T) {
	repo := NewSwSubscriptionRepository(testDB)
	user := insertTestUser(t, "u_sw_1", "swuser")
	defer cleanupUser(t, user.ID)

	// Create
	sub := &model.SwSubscription{
		ID:              "sw_1",
		UserID:          user.ID,
		Endpoint:        "https://push.example/test",
		Auth:            "authdata",
		PublicKey:       "pubkeydata",
		SendReadMessage: false,
	}
	require.NoError(t, repo.Create(sub))
	defer testDB.Exec(`DELETE FROM "sw_subscription" WHERE id = ?`, sub.ID)

	// FindByUserAndEndpoint
	found, err := repo.FindByUserAndEndpoint(user.ID, "https://push.example/test")
	require.NoError(t, err)
	assert.Equal(t, "sw_1", found.ID)
	assert.Equal(t, false, found.SendReadMessage)

	// FindByUserAndEndpoint - not found
	_, err = repo.FindByUserAndEndpoint(user.ID, "https://ghost")
	assert.Error(t, err)

	// FindByUserEndpointAuthKey - 4-tuple full match (#1775)
	found4, err := repo.FindByUserEndpointAuthKey(user.ID, "https://push.example/test", "authdata", "pubkeydata")
	require.NoError(t, err)
	assert.Equal(t, "sw_1", found4.ID)

	// FindByUserEndpointAuthKey - key rotation (auth/publickey mismatch) must NOT match
	_, err = repo.FindByUserEndpointAuthKey(user.ID, "https://push.example/test", "rotated-auth", "rotated-pk")
	assert.Error(t, err)

	// Update
	found.SendReadMessage = true
	require.NoError(t, repo.Update(found))
	updated, err := repo.FindByUserAndEndpoint(user.ID, "https://push.example/test")
	require.NoError(t, err)
	assert.Equal(t, true, updated.SendReadMessage)

	// DeleteByUserAndEndpoint
	require.NoError(t, repo.DeleteByUserAndEndpoint(user.ID, "https://push.example/test"))
	_, err = repo.FindByUserAndEndpoint(user.ID, "https://push.example/test")
	assert.Error(t, err)

}

// FindByEndpointAuthKey matches on the full (endpoint, auth, publickey)
// triple, optionally narrowed to one user, and DeleteByIDs removes exactly the
// given rows (upstream sw/unregister findBy + delete).
func TestSwSubscriptionRepository_FindByEndpointAuthKeyAndDeleteByIDs(t *testing.T) {
	repo := NewSwSubscriptionRepository(testDB)
	u1 := insertTestUser(t, "u_sw_ek1", "swek1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "u_sw_ek2", "swek2")
	defer cleanupUser(t, u2.ID)

	const endpoint = "https://push.example/shared"
	rows := []*model.SwSubscription{
		{ID: "sw_ek_1", UserID: u1.ID, Endpoint: endpoint, Auth: "a1", PublicKey: "pk1"},
		{ID: "sw_ek_2", UserID: u2.ID, Endpoint: endpoint, Auth: "a1", PublicKey: "pk1"},
		{ID: "sw_ek_3", UserID: u1.ID, Endpoint: endpoint, Auth: "a-other", PublicKey: "pk1"},
	}
	for _, r := range rows {
		require.NoError(t, repo.Create(r))
	}
	defer testDB.Exec(`DELETE FROM "sw_subscription" WHERE id IN ?`, []string{"sw_ek_1", "sw_ek_2", "sw_ek_3"})

	ids := func(subs []*model.SwSubscription) []string {
		out := make([]string, len(subs))
		for i, s := range subs {
			out[i] = s.ID
		}
		sort.Strings(out)
		return out
	}

	all, err := repo.FindByEndpointAuthKey(nil, endpoint, "a1", "pk1")
	require.NoError(t, err)
	assert.Equal(t, []string{"sw_ek_1", "sw_ek_2"}, ids(all))

	own, err := repo.FindByEndpointAuthKey(&u1.ID, endpoint, "a1", "pk1")
	require.NoError(t, err)
	assert.Equal(t, []string{"sw_ek_1"}, ids(own))

	for _, tc := range []struct{ auth, pk string }{{"wrong", "pk1"}, {"a1", "wrong"}} {
		none, err := repo.FindByEndpointAuthKey(nil, endpoint, tc.auth, tc.pk)
		require.NoError(t, err)
		assert.Empty(t, none, "auth=%s publickey=%s", tc.auth, tc.pk)
	}
	none, err := repo.FindByEndpointAuthKey(nil, "https://push.example/ghost", "a1", "pk1")
	require.NoError(t, err)
	assert.Empty(t, none)

	// 列に入らない値は SQL に載せず、空で返す。
	bad := "a\x00b"
	for name, find := range map[string]func() ([]*model.SwSubscription, error){
		"userId": func() ([]*model.SwSubscription, error) {
			return repo.FindByEndpointAuthKey(&bad, endpoint, "a1", "pk1")
		},
		"endpoint":  func() ([]*model.SwSubscription, error) { return repo.FindByEndpointAuthKey(nil, bad, "a1", "pk1") },
		"auth":      func() ([]*model.SwSubscription, error) { return repo.FindByEndpointAuthKey(nil, endpoint, bad, "pk1") },
		"publickey": func() ([]*model.SwSubscription, error) { return repo.FindByEndpointAuthKey(nil, endpoint, "a1", bad) },
	} {
		got, err := find()
		require.NoError(t, err, name)
		assert.Empty(t, got, name)
	}

	require.NoError(t, repo.DeleteByIDs([]string{"sw_ek_1", "sw_ek_2", bad}))
	require.NoError(t, repo.DeleteByIDs(nil))
	var left []string
	require.NoError(t, testDB.Model(&model.SwSubscription{}).Where(`"endpoint" = ?`, endpoint).Order("id").Pluck("id", &left).Error)
	assert.Equal(t, []string{"sw_ek_3"}, left)
}
