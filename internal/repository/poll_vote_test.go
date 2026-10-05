package repository

import (
	"context"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func cleanupPollVote(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "poll_vote" WHERE id = ?`, id)
}

func setupPollNote(t *testing.T, noteID, userID string) {
	t.Helper()
	noteRepo := NewNoteRepository(testDB)
	pollRepo := NewPollRepository(testDB)
	n := &model.Note{
		ID: noteID, UserID: userID,
		Visibility: model.NoteVisibilityPublic,
		HasPoll:    true,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, noteRepo.Create(n))

	p := &model.Poll{
		NoteID:         noteID,
		Multiple:       false,
		Choices:        model.StringArray{"A", "B", "C"},
		Votes:          model.Int64Array{0, 0, 0},
		NoteVisibility: model.NoteVisibilityPublic,
		UserID:         userID,
	}
	require.NoError(t, pollRepo.Create(p))
}

func TestPollVoteRepository_CreateAndFind(t *testing.T) {
	repo := NewPollVoteRepository(testDB)
	user := insertTestUser(t, "u_pv_1", "pv1")
	defer cleanupUser(t, user.ID)
	setupPollNote(t, "n_pv_1", user.ID)
	defer cleanupNote(t, "n_pv_1")

	v := &model.PollVote{
		ID: "pv_1", UserID: user.ID, NoteID: "n_pv_1", Choice: 1,
	}
	require.NoError(t, repo.Create(v))
	defer cleanupPollVote(t, v.ID)

	found, err := repo.FindByUserAndChoice(user.ID, "n_pv_1", 1)
	require.NoError(t, err)
	assert.Equal(t, "pv_1", found.ID)

	count, err := repo.CountByUserAndNote(user.ID, "n_pv_1")
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	rows, err := repo.ListByNoteID("n_pv_1")
	require.NoError(t, err)
	assert.Len(t, rows, 1)
}

func TestPollVoteRepository_NotFound(t *testing.T) {
	repo := NewPollVoteRepository(testDB)
	_, err := repo.FindByUserAndChoice("nope", "nope", 0)
	assert.Error(t, err)
}

func TestPollVoteRepository_QueryErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewPollVoteRepository(testDB.WithContext(ctx))

	err := repo.Create(&model.PollVote{ID: "x"})
	assert.Error(t, err)
	_, err = repo.FindByUserAndChoice("a", "b", 0)
	assert.Error(t, err)
	_, err = repo.CountByUserAndNote("a", "b")
	assert.Error(t, err)
	_, err = repo.ListByNoteID("a")
	assert.Error(t, err)
	_, err = repo.FindByUserAndNoteIDs("a", []string{"b"})
	assert.Error(t, err)
}

// TestPollVoteRepository_FindByUserAndNoteIDs covers the batch lookup added
// in #690 for entity.NoteFieldResolver to populate Poll.choices[i].IsVoted.
func TestPollVoteRepository_FindByUserAndNoteIDs(t *testing.T) {
	repo := NewPollVoteRepository(testDB)
	user := insertTestUser(t, "u_fbun_1", "fbunuser")
	defer cleanupUser(t, user.ID)
	setupPollNote(t, "n_fbun_1", user.ID)
	defer cleanupNote(t, "n_fbun_1")
	setupPollNote(t, "n_fbun_2", user.ID)
	defer cleanupNote(t, "n_fbun_2")

	// 空入力は空 map を返す (no DB call)
	out, err := repo.FindByUserAndNoteIDs("", []string{"x"})
	require.NoError(t, err)
	assert.Empty(t, out)
	out, err = repo.FindByUserAndNoteIDs(user.ID, nil)
	require.NoError(t, err)
	assert.Empty(t, out)

	// 同 user が n_fbun_1 で 2 票 + n_fbun_2 で 1 票
	require.NoError(t, repo.Create(&model.PollVote{ID: "v_fbun_1", UserID: user.ID, NoteID: "n_fbun_1", Choice: 0}))
	require.NoError(t, repo.Create(&model.PollVote{ID: "v_fbun_2", UserID: user.ID, NoteID: "n_fbun_1", Choice: 2}))
	require.NoError(t, repo.Create(&model.PollVote{ID: "v_fbun_3", UserID: user.ID, NoteID: "n_fbun_2", Choice: 1}))
	defer cleanupPollVote(t, "v_fbun_1")
	defer cleanupPollVote(t, "v_fbun_2")
	defer cleanupPollVote(t, "v_fbun_3")

	// 別 user の vote は混ざらない
	other := insertTestUser(t, "u_fbun_2", "fbunother")
	defer cleanupUser(t, other.ID)
	require.NoError(t, repo.Create(&model.PollVote{ID: "v_fbun_4", UserID: other.ID, NoteID: "n_fbun_1", Choice: 1}))
	defer cleanupPollVote(t, "v_fbun_4")

	got, err := repo.FindByUserAndNoteIDs(user.ID, []string{"n_fbun_1", "n_fbun_2", "n_fbun_no_votes"})
	require.NoError(t, err)
	assert.ElementsMatch(t, []int{0, 2}, got["n_fbun_1"])
	assert.Equal(t, []int{1}, got["n_fbun_2"])
	_, has := got["n_fbun_no_votes"]
	assert.False(t, has, "note with no votes must not appear in result map")
}

func TestPollRepository_FindByNoteIDAndIncrementVote(t *testing.T) {
	pollRepo := NewPollRepository(testDB)
	user := insertTestUser(t, "u_pi_1", "pi1")
	defer cleanupUser(t, user.ID)
	setupPollNote(t, "n_pi_1", user.ID)
	defer cleanupNote(t, "n_pi_1")

	p, err := pollRepo.FindByNoteID("n_pi_1")
	require.NoError(t, err)
	assert.Equal(t, []int64{0, 0, 0}, []int64(p.Votes))

	require.NoError(t, pollRepo.IncrementVote("n_pi_1", 1, 2))
	p, err = pollRepo.FindByNoteID("n_pi_1")
	require.NoError(t, err)
	assert.Equal(t, int64(2), p.Votes[1])
}

func TestPollRepository_QueryErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	repo := NewPollRepository(testDB.WithContext(ctx))

	_, err := repo.FindByNoteID("a")
	assert.Error(t, err)
	err = repo.IncrementVote("a", 0, 1)
	assert.Error(t, err)
}
