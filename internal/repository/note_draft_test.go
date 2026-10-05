package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNoteDraftRepository_Full(t *testing.T) {
	repo := NewNoteDraftRepository(testDB)
	user := insertTestUser(t, "u_draft_1", "draftuser")
	defer cleanupUser(t, user.ID)

	text := "draft text"
	// Create
	draft := &model.NoteDraft{
		ID:         "draft_1",
		UserID:     user.ID,
		Text:       &text,
		Visibility: "public",
	}
	require.NoError(t, repo.Create(draft))
	defer testDB.Exec(`DELETE FROM "note_draft" WHERE id = ?`, draft.ID)

	// FindByIDAndUser
	found, err := repo.FindByIDAndUser("draft_1", user.ID)
	require.NoError(t, err)
	assert.Equal(t, "draft text", *found.Text)

	// FindByIDAndUser - not found
	_, err = repo.FindByIDAndUser("ghost", user.ID)
	assert.Error(t, err)

	// FindByID (= ownership 非考慮、worker 経路 / #1040)
	foundByID, err := repo.FindByID("draft_1")
	require.NoError(t, err)
	assert.Equal(t, "draft text", *foundByID.Text)
	_, err = repo.FindByID("ghost")
	assert.Error(t, err)

	// ListByUser
	list, err := repo.ListByUser(user.ID, "", "", nil, 10)
	require.NoError(t, err)
	assert.Len(t, list, 1)

	// CountScheduledByUser (#1040): 通常 draft は isActuallyScheduled=false なので 0
	scheduled, err := repo.CountScheduledByUser(user.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), scheduled)

	// ListByUser - default limit
	list2, err := repo.ListByUser(user.ID, "", "", nil, 0)
	require.NoError(t, err)
	assert.Len(t, list2, 1)

	// ListByUser pagination + scheduled filter (#1538). draft.ID は aidx で
	// keyset pagination する。untilId=自身の id 未満 → 該当 0、scheduled=true
	// フィルタ → 通常 draft (isActuallyScheduled=false) は除外。
	only := list[0]
	emptyByUntil, err := repo.ListByUser(user.ID, "", only.ID, nil, 10)
	require.NoError(t, err)
	assert.Empty(t, emptyByUntil, "untilId=自身の id では < cursor に該当無し")

	emptyBySince, err := repo.ListByUser(user.ID, only.ID, "", nil, 10)
	require.NoError(t, err)
	assert.Empty(t, emptyBySince, "sinceId=自身の id では > cursor に該当無し")

	wantScheduled := true
	noScheduled, err := repo.ListByUser(user.ID, "", "", &wantScheduled, 10)
	require.NoError(t, err)
	assert.Empty(t, noScheduled, "scheduled=true フィルタで通常 draft は除外")

	wantNotScheduled := false
	stillThere, err := repo.ListByUser(user.ID, "", "", &wantNotScheduled, 10)
	require.NoError(t, err)
	assert.Len(t, stillThere, 1, "scheduled=false フィルタは通常 draft を含む")

	// Update
	newText := "updated text"
	found.Text = &newText
	require.NoError(t, repo.Update(found))
	updated, err := repo.FindByIDAndUser("draft_1", user.ID)
	require.NoError(t, err)
	assert.Equal(t, "updated text", *updated.Text)

	// CountByUser
	count, err := repo.CountByUser(user.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	// Delete: 該当行が存在するので RowsAffected = 1
	rows, err := repo.Delete("draft_1", user.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), rows)
	_, err = repo.FindByIDAndUser("draft_1", user.ID)
	assert.Error(t, err)

	// CountByUser after delete
	count, err = repo.CountByUser(user.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)

	// Delete on already-removed draft: 0 rows affected, no error。
	// handler 側の "no such draft" 判定はこの 0 を見て 404 を返す。
	rows, err = repo.Delete("draft_1", user.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), rows)
}
