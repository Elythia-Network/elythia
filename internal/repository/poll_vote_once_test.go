package repository

import (
	"fmt"
	"sync"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func countPollVotes(t *testing.T, userID, noteID string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, testDB.Model(&model.PollVote{}).
		Where(`"userId" = ? AND "noteId" = ?`, userID, noteID).Count(&n).Error)
	return n
}

func TestPollVoteRepository_CreateIfNotVoted_Sequential(t *testing.T) {
	repo := NewPollVoteRepository(testDB)
	user := insertTestUser(t, "u_pvo_1", "pvo1")
	defer cleanupUser(t, user.ID)
	setupPollNote(t, "n_pvo_1", user.ID)
	defer cleanupNote(t, "n_pvo_1")
	defer testDB.Exec(`DELETE FROM "poll_vote" WHERE "noteId" = ?`, "n_pvo_1")

	cases := []struct {
		name   string
		id     string
		choice int
		single bool
		want   bool
	}{
		{name: "first vote", id: "pvo_1", choice: 0, single: true, want: true},
		{name: "single rejects another choice", id: "pvo_2", choice: 1, single: true, want: false},
		{name: "multiple rejects the same choice", id: "pvo_3", choice: 0, single: false, want: false},
		{name: "multiple accepts another choice", id: "pvo_4", choice: 2, single: false, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created, err := repo.CreateIfNotVoted(&model.PollVote{
				ID: tc.id, UserID: user.ID, NoteID: "n_pvo_1", Choice: tc.choice,
			}, tc.single)
			require.NoError(t, err)
			assert.Equal(t, tc.want, created)
		})
	}
	assert.Equal(t, int64(2), countPollVotes(t, user.ID, "n_pvo_1"))
}

// 同じ利用者が単一選択の投票へ違う選択肢を同時に送っても、1票だけ入る。
func TestPollVoteRepository_CreateIfNotVoted_ConcurrentSingle(t *testing.T) {
	repo := NewPollVoteRepository(testDB)
	user := insertTestUser(t, "u_pvo_2", "pvo2")
	defer cleanupUser(t, user.ID)

	const notes = 10
	const voters = 3 // 選択肢 A / B / C
	for i := 0; i < notes; i++ {
		noteID := fmt.Sprintf("n_pvo_c%d", i)
		setupPollNote(t, noteID, user.ID)
		defer cleanupNote(t, noteID)
		defer testDB.Exec(`DELETE FROM "poll_vote" WHERE "noteId" = ?`, noteID)
	}

	var wg sync.WaitGroup
	results := make([][voters]bool, notes)
	errs := make(chan error, notes*voters)
	for i := 0; i < notes; i++ {
		for c := 0; c < voters; c++ {
			wg.Add(1)
			go func(i, c int) {
				defer wg.Done()
				created, err := repo.CreateIfNotVoted(&model.PollVote{
					ID:     fmt.Sprintf("pvo_c%d_%d", i, c),
					UserID: user.ID, NoteID: fmt.Sprintf("n_pvo_c%d", i), Choice: c,
				}, true)
				if err != nil {
					errs <- err
					return
				}
				results[i][c] = created
			}(i, c)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	for i := 0; i < notes; i++ {
		createdCount := 0
		for _, ok := range results[i] {
			if ok {
				createdCount++
			}
		}
		assert.Equal(t, 1, createdCount, "note %d", i)
		assert.Equal(t, int64(1), countPollVotes(t, user.ID, fmt.Sprintf("n_pvo_c%d", i)), "note %d", i)
	}
}

func TestPollVoteRepository_CreateIfNotVoted_InsertError(t *testing.T) {
	repo := NewPollVoteRepository(testDB)
	// 存在しないノートへの投票は外部キーで落ちる。エラーは握り潰さない。
	created, err := repo.CreateIfNotVoted(&model.PollVote{
		ID: "pvo_fk", UserID: "u_pvo_missing", NoteID: "n_pvo_missing", Choice: 0,
	}, true)
	require.Error(t, err)
	assert.False(t, created)
}
