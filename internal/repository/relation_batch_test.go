package repository

import (
	"context"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRelationBatch_AnchorFilters covers the batch readers the packMany-style
// relation block uses (#3330): each returns only the candidates that hold the
// relation in the asked direction, short-circuits empty input, and the muting
// one skips expired mutes like Exists.
func TestRelationBatch_AnchorFilters(t *testing.T) {
	anchor := insertTestUser(t, "u_rb_a", "rbanchor")
	defer cleanupUser(t, anchor.ID)
	c1 := insertTestUser(t, "u_rb_c1", "rbcand1")
	defer cleanupUser(t, c1.ID)
	c2 := insertTestUser(t, "u_rb_c2", "rbcand2")
	defer cleanupUser(t, c2.ID)
	c3 := insertTestUser(t, "u_rb_c3", "rbcand3")
	defer cleanupUser(t, c3.ID)
	cands := []string{c1.ID, c2.ID, c3.ID, "ghost"}

	// following: anchor -> c1 (notify=normal, withReplies) / c2 -> anchor
	notify := "normal"
	require.NoError(t, testDB.Create(&model.Following{ID: "rb_f1", FollowerID: anchor.ID, FolloweeID: c1.ID, Notify: &notify, WithReplies: true}).Error)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "rb_f1")
	require.NoError(t, testDB.Create(&model.Following{ID: "rb_f2", FollowerID: c2.ID, FolloweeID: anchor.ID}).Error)
	defer testDB.Exec(`DELETE FROM "following" WHERE id = ?`, "rb_f2")

	fr := NewFollowingRepository(testDB).(FollowingRowsFromAnchorReader)
	rows, err := fr.ListFollowingRowsFromAnchor(anchor.ID, cands)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, c1.ID, rows[0].FolloweeID)
	require.NotNil(t, rows[0].Notify)
	assert.Equal(t, "normal", *rows[0].Notify)
	assert.True(t, rows[0].WithReplies)
	rows, err = fr.ListFollowingRowsFromAnchor("", cands)
	require.NoError(t, err)
	assert.Nil(t, rows)
	rows, err = fr.ListFollowingRowsFromAnchor(anchor.ID, nil)
	require.NoError(t, err)
	assert.Nil(t, rows)

	// blocking: anchor -> c2 / c3 -> anchor
	require.NoError(t, testDB.Create(&model.Blocking{ID: "rb_b1", BlockerID: anchor.ID, BlockeeID: c2.ID}).Error)
	defer testDB.Exec(`DELETE FROM "blocking" WHERE id = ?`, "rb_b1")
	require.NoError(t, testDB.Create(&model.Blocking{ID: "rb_b2", BlockerID: c3.ID, BlockeeID: anchor.ID}).Error)
	defer testDB.Exec(`DELETE FROM "blocking" WHERE id = ?`, "rb_b2")

	br := NewBlockingRepository(testDB).(BlockingAnchorFilter)
	ids, err := br.FilterBlockingFromAnchor(anchor.ID, cands)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{c2.ID}, ids)
	ids, err = br.FilterBlockingToAnchor(anchor.ID, cands)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{c3.ID}, ids)
	ids, err = br.FilterBlockingFromAnchor(anchor.ID, nil)
	require.NoError(t, err)
	assert.Nil(t, ids)
	ids, err = br.FilterBlockingToAnchor("", cands)
	require.NoError(t, err)
	assert.Nil(t, ids)

	// muting: anchor -> c1 (active, no expiry), anchor -> c2 (active, future),
	// anchor -> c3 (expired)。c3 -> anchor は逆向きなので数えない。
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	for _, m := range []*model.Muting{
		{ID: "rb_m1", MuterID: anchor.ID, MuteeID: c1.ID},
		{ID: "rb_m2", MuterID: anchor.ID, MuteeID: c2.ID, ExpiresAt: &future},
		{ID: "rb_m3", MuterID: anchor.ID, MuteeID: c3.ID, ExpiresAt: &past},
		{ID: "rb_m4", MuterID: c3.ID, MuteeID: anchor.ID},
	} {
		require.NoError(t, testDB.Create(m).Error)
		defer cleanupMuting(t, m.ID)
	}
	mr := NewMutingRepository(testDB).(MutingAnchorFilter)
	ids, err = mr.FilterActiveMutingFromAnchor(anchor.ID, cands)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{c1.ID, c2.ID}, ids, "期限切れの mute は Exists と同じく数えない")
	ids, err = mr.FilterActiveMutingFromAnchor(anchor.ID, nil)
	require.NoError(t, err)
	assert.Nil(t, ids)

	// renote muting: anchor -> c3
	require.NoError(t, testDB.Create(&model.RenoteMuting{ID: "rb_rm1", MuterID: anchor.ID, MuteeID: c3.ID}).Error)
	defer cleanupRenoteMuting(t, "rb_rm1")
	require.NoError(t, testDB.Create(&model.RenoteMuting{ID: "rb_rm2", MuterID: c1.ID, MuteeID: anchor.ID}).Error)
	defer cleanupRenoteMuting(t, "rb_rm2")
	rr := NewRenoteMutingRepository(testDB).(RenoteMutingAnchorFilter)
	ids, err = rr.FilterRenoteMutingFromAnchor(anchor.ID, cands)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{c3.ID}, ids)
	ids, err = rr.FilterRenoteMutingFromAnchor("", cands)
	require.NoError(t, err)
	assert.Nil(t, ids)

	// memo: anchor about c2, c1 about anchor (逆向き)
	require.NoError(t, testDB.Create(&model.UserMemo{ID: "rb_memo1", UserID: anchor.ID, TargetUserID: c2.ID, Memo: "hello"}).Error)
	defer testDB.Exec(`DELETE FROM "user_memo" WHERE id = ?`, "rb_memo1")
	require.NoError(t, testDB.Create(&model.UserMemo{ID: "rb_memo2", UserID: c1.ID, TargetUserID: anchor.ID, Memo: "other"}).Error)
	defer testDB.Exec(`DELETE FROM "user_memo" WHERE id = ?`, "rb_memo2")
	ur := NewUserMemoRepository(testDB).(UserMemoBatchReader)
	memos, err := ur.ListByUserAndTargets(anchor.ID, cands)
	require.NoError(t, err)
	require.Len(t, memos, 1)
	assert.Equal(t, c2.ID, memos[0].TargetUserID)
	assert.Equal(t, "hello", memos[0].Memo)
	memos, err = ur.ListByUserAndTargets(anchor.ID, nil)
	require.NoError(t, err)
	assert.Nil(t, memos)
}

// TestRelationBatch_QueryError covers the DB error branch of every batch
// reader (cancelled ctx → driver error → bubble up).
func TestRelationBatch_QueryError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	cands := []string{"v"}

	_, err := NewFollowingRepository(db).(FollowingRowsFromAnchorReader).ListFollowingRowsFromAnchor("u", cands)
	assert.Error(t, err)
	br := NewBlockingRepository(db).(BlockingAnchorFilter)
	_, err = br.FilterBlockingFromAnchor("u", cands)
	assert.Error(t, err)
	_, err = br.FilterBlockingToAnchor("u", cands)
	assert.Error(t, err)
	_, err = NewMutingRepository(db).(MutingAnchorFilter).FilterActiveMutingFromAnchor("u", cands)
	assert.Error(t, err)
	_, err = NewRenoteMutingRepository(db).(RenoteMutingAnchorFilter).FilterRenoteMutingFromAnchor("u", cands)
	assert.Error(t, err)
	_, err = NewUserMemoRepository(db).(UserMemoBatchReader).ListByUserAndTargets("u", cands)
	assert.Error(t, err)
}
