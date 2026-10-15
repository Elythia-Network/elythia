package poll_test

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/core/poll"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// racingVoteRepo passes the pre-checks (no vote seen yet) but loses the
// insert, as when another vote by the same user commits in between.
type racingVoteRepo struct {
	*testutil.MockPollVoteRepository
	createErr error
	single    *bool
}

func (r *racingVoteRepo) CreateIfNotVoted(_ *model.PollVote, single bool) (bool, error) {
	r.single = &single
	return false, r.createErr
}

func TestVote_ConcurrentDuplicateRejected(t *testing.T) {
	cases := []struct {
		name     string
		multiple bool
		repoErr  error
	}{
		{name: "single choice lost the race", multiple: false},
		{name: "multiple choice lost the race", multiple: true},
		{name: "insert failure is returned", multiple: false, repoErr: errors.New("db down")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			noteRepo := testutil.NewMockNoteRepository()
			pollRepo := testutil.NewMockPollRepository()
			voteRepo := &racingVoteRepo{MockPollVoteRepository: testutil.NewMockPollVoteRepository(), createErr: tc.repoErr}
			idGen, _ := id.NewGenerator("aidx")
			svc := poll.NewService(noteRepo, pollRepo, voteRepo, nil, idGen)
			pub := &stubEventPub{}
			svc.SetEventPublisher(pub)
			seedPollNote(noteRepo, pollRepo, "n1", "author", tc.multiple, nil)

			err := svc.Vote(&model.User{ID: "viewer"}, "n1", 1)
			if tc.repoErr != nil {
				require.ErrorIs(t, err, tc.repoErr)
			} else {
				require.ErrorIs(t, err, poll.ErrAlreadyVoted)
			}
			require.NotNil(t, voteRepo.single)
			assert.Equal(t, !tc.multiple, *voteRepo.single)
			assert.Equal(t, int64(0), pollRepo.Polls["n1"].Votes[1], "a rejected vote must not be counted")
			assert.Zero(t, pub.calls, "a rejected vote must not be streamed")
		})
	}
}

func TestMockPollVoteRepository_CreateIfNotVoted(t *testing.T) {
	repo := testutil.NewMockPollVoteRepository()
	ok, err := repo.CreateIfNotVoted(&model.PollVote{ID: "v1", UserID: "u", NoteID: "n", Choice: 0}, true)
	require.NoError(t, err)
	assert.True(t, ok)
	ok, _ = repo.CreateIfNotVoted(&model.PollVote{ID: "v2", UserID: "u", NoteID: "n", Choice: 1}, true)
	assert.False(t, ok, "single choice: second vote rejected")
	ok, _ = repo.CreateIfNotVoted(&model.PollVote{ID: "v3", UserID: "u", NoteID: "n", Choice: 1}, false)
	assert.True(t, ok, "multiple choice: another choice accepted")
}
