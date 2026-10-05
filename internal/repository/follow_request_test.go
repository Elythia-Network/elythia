package repository

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func insertFollowRequest(t *testing.T, id, followerID, followeeID string) *model.FollowRequest {
	t.Helper()
	r := &model.FollowRequest{
		ID:         id,
		FollowerID: followerID,
		FolloweeID: followeeID,
	}
	require.NoError(t, testDB.Create(r).Error)
	return r
}

func TestFollowRequestRepository_Create_FindByPair(t *testing.T) {
	repo := NewFollowRequestRepository(testDB)
	follower := insertTestUser(t, "u_fr_1", "frfollower1")
	defer cleanupUser(t, follower.ID)
	followee := insertTestUser(t, "u_fr_2", "frfollowee1")
	defer cleanupUser(t, followee.ID)

	r := &model.FollowRequest{
		ID:         "fr_1",
		FollowerID: follower.ID,
		FolloweeID: followee.ID,
	}
	require.NoError(t, repo.Create(r))
	defer testDB.Exec(`DELETE FROM "follow_request" WHERE id = ?`, r.ID)

	found, err := repo.FindByPair(follower.ID, followee.ID)
	require.NoError(t, err)
	assert.Equal(t, r.ID, found.ID)
}

func TestFollowRequestRepository_FindByPair_NotFound(t *testing.T) {
	repo := NewFollowRequestRepository(testDB)
	_, err := repo.FindByPair("ghost1", "ghost2")
	assert.Error(t, err)
}

func TestFollowRequestRepository_Exists(t *testing.T) {
	repo := NewFollowRequestRepository(testDB)
	follower := insertTestUser(t, "u_fr_3", "frfollower3")
	defer cleanupUser(t, follower.ID)
	followee := insertTestUser(t, "u_fr_4", "frfollowee4")
	defer cleanupUser(t, followee.ID)

	exists, err := repo.Exists(follower.ID, followee.ID)
	require.NoError(t, err)
	assert.False(t, exists)

	insertFollowRequest(t, "fr_2", follower.ID, followee.ID)
	defer testDB.Exec(`DELETE FROM "follow_request" WHERE id = ?`, "fr_2")

	exists, err = repo.Exists(follower.ID, followee.ID)
	require.NoError(t, err)
	assert.True(t, exists)
}

// TestFollowRequestRepository_FilterPendingFromAnchor + ToAnchor cover the
// batch lookup added for users/{followers,following} pending request flag
// population (#1144 #2、MkFollowButton の `hasPendingFollowRequestFromYou`
// 表示分岐に必要)。
func TestFollowRequestRepository_FilterPendingFromAnchor(t *testing.T) {
	repo := NewFollowRequestRepository(testDB)
	anchor := insertTestUser(t, "u_pf_a", "anchorpf")
	defer cleanupUser(t, anchor.ID)
	t1 := insertTestUser(t, "u_pf_t1", "targetpf1")
	defer cleanupUser(t, t1.ID)
	t2 := insertTestUser(t, "u_pf_t2", "targetpf2")
	defer cleanupUser(t, t2.ID)

	insertFollowRequest(t, "pf_1", anchor.ID, t1.ID)
	defer testDB.Exec(`DELETE FROM "follow_request" WHERE id = ?`, "pf_1")

	out, err := repo.FilterPendingFromAnchor("", []string{t1.ID})
	require.NoError(t, err)
	assert.Nil(t, out)
	out, err = repo.FilterPendingFromAnchor(anchor.ID, nil)
	require.NoError(t, err)
	assert.Nil(t, out)

	out, err = repo.FilterPendingFromAnchor(anchor.ID, []string{t1.ID, t2.ID, "ghost"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{t1.ID}, out)
}

func TestFollowRequestRepository_FilterPendingToAnchor(t *testing.T) {
	repo := NewFollowRequestRepository(testDB)
	anchor := insertTestUser(t, "u_pt_a", "anchorpt")
	defer cleanupUser(t, anchor.ID)
	c1 := insertTestUser(t, "u_pt_c1", "candpt1")
	defer cleanupUser(t, c1.ID)
	c2 := insertTestUser(t, "u_pt_c2", "candpt2")
	defer cleanupUser(t, c2.ID)

	insertFollowRequest(t, "pt_1", c1.ID, anchor.ID)
	defer testDB.Exec(`DELETE FROM "follow_request" WHERE id = ?`, "pt_1")

	out, err := repo.FilterPendingToAnchor("", []string{c1.ID})
	require.NoError(t, err)
	assert.Nil(t, out)
	out, err = repo.FilterPendingToAnchor(anchor.ID, nil)
	require.NoError(t, err)
	assert.Nil(t, out)

	out, err = repo.FilterPendingToAnchor(anchor.ID, []string{c1.ID, c2.ID, "ghost"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{c1.ID}, out)
}

// TestFollowRequestRepository_FilterPending_QueryError covers the DB error
// branch of both Filter helpers (cancelled ctx).
func TestFollowRequestRepository_FilterPending_QueryError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewFollowRequestRepository(db)
	_, err := repo.FilterPendingFromAnchor("u", []string{"v"})
	assert.Error(t, err)
	_, err = repo.FilterPendingToAnchor("u", []string{"v"})
	assert.Error(t, err)
}

func TestFollowRequestRepository_Delete(t *testing.T) {
	repo := NewFollowRequestRepository(testDB)
	follower := insertTestUser(t, "u_fr_5", "frfollower5")
	defer cleanupUser(t, follower.ID)
	followee := insertTestUser(t, "u_fr_6", "frfollowee6")
	defer cleanupUser(t, followee.ID)

	r := insertFollowRequest(t, "fr_3", follower.ID, followee.ID)
	require.NoError(t, repo.Delete(r))

	exists, err := repo.Exists(follower.ID, followee.ID)
	require.NoError(t, err)
	assert.False(t, exists)
}

// #1759: DeleteAllByUser は user が follower / followee いずれかの request を
// 双方向で消し、無関係な request は残す。
func TestFollowRequestRepository_DeleteAllByUser(t *testing.T) {
	repo := NewFollowRequestRepository(testDB)
	u := insertTestUser(t, "u_fr_dab", "frdab")
	defer cleanupUser(t, u.ID)
	other := insertTestUser(t, "u_fr_dab2", "frdab2")
	defer cleanupUser(t, other.ID)
	p := insertTestUser(t, "u_fr_dab3", "frdab3")
	defer cleanupUser(t, p.ID)
	q := insertTestUser(t, "u_fr_dab4", "frdab4")
	defer cleanupUser(t, q.ID)

	outgoing := insertFollowRequest(t, "fr_dab_out", u.ID, other.ID) // u → other
	incoming := insertFollowRequest(t, "fr_dab_in", other.ID, u.ID)  // other → u
	unrelated := insertFollowRequest(t, "fr_dab_un", p.ID, q.ID)     // p → q
	defer testDB.Exec(`DELETE FROM "follow_request" WHERE id = ?`, unrelated.ID)

	require.NoError(t, repo.DeleteAllByUser(u.ID))

	out, _ := repo.Exists(outgoing.FollowerID, outgoing.FolloweeID)
	assert.False(t, out, "outgoing request involving user removed")
	in, _ := repo.Exists(incoming.FollowerID, incoming.FolloweeID)
	assert.False(t, in, "incoming request involving user removed")
	un, _ := repo.Exists(unrelated.FollowerID, unrelated.FolloweeID)
	assert.True(t, un, "unrelated request kept")
}

func TestFollowRequestRepository_QueryErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewFollowRequestRepository(db)

	_, err := repo.Exists("a", "b")
	assert.Error(t, err)

	_, err = repo.ListReceived("a", 10, "", "")
	assert.Error(t, err)

	_, err = repo.ListSent("a", 10, "", "")
	assert.Error(t, err)
}

func TestFollowRequestRepository_ListReceived_ListSent(t *testing.T) {
	repo := NewFollowRequestRepository(testDB)
	a := insertTestUser(t, "u_fr_7", "fruser7")
	defer cleanupUser(t, a.ID)
	b := insertTestUser(t, "u_fr_8", "fruser8")
	defer cleanupUser(t, b.ID)
	c := insertTestUser(t, "u_fr_9", "fruser9")
	defer cleanupUser(t, c.ID)

	insertFollowRequest(t, "fr_4", a.ID, b.ID)
	defer testDB.Exec(`DELETE FROM "follow_request" WHERE id = ?`, "fr_4")
	insertFollowRequest(t, "fr_5", c.ID, b.ID)
	defer testDB.Exec(`DELETE FROM "follow_request" WHERE id = ?`, "fr_5")
	insertFollowRequest(t, "fr_6", a.ID, c.ID)
	defer testDB.Exec(`DELETE FROM "follow_request" WHERE id = ?`, "fr_6")

	received, err := repo.ListReceived(b.ID, 10, "", "")
	require.NoError(t, err)
	assert.Len(t, received, 2)

	sent, err := repo.ListSent(a.ID, 10, "", "")
	require.NoError(t, err)
	assert.Len(t, sent, 2)
}
