package poll_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/poll"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// countingNoteMaterializer records how many times it was asked to fetch a
// remote note.
type countingNoteMaterializer struct{ asked int }

func (m *countingNoteMaterializer) EnsureNote(context.Context, string) (*model.Note, error) {
	m.asked++
	return nil, errors.New("unreachable")
}

type dbFailingNoteRepo struct {
	*testutil.MockNoteRepository
	err error
}

func (r *dbFailingNoteRepo) FindByIDWithUser(string) (*model.Note, error) { return nil, r.err }

type dbFailingPollRepo struct {
	*testutil.MockPollRepository
	err error
}

func (r *dbFailingPollRepo) FindByNoteID(string) (*model.Poll, error) { return nil, r.err }

// **DB 障害を ErrNoteNotFound / ErrNoPoll に丸めないこと** (#2799)。
//
// **materialize より前で分ける。** not-found でない error は materialize しても
// 直らないのに、`EnsureNote` がリモート fetch を走らせる。
func TestVote_DBFailureIsNotNotFound(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	idGen, _ := id.NewGenerator("aidx")
	me := &model.User{ID: "u1"}

	t.Run("note lookup", func(t *testing.T) {
		svc := poll.NewService(
			&dbFailingNoteRepo{MockNoteRepository: testutil.NewMockNoteRepository(), err: dbErr},
			testutil.NewMockPollRepository(), testutil.NewMockPollVoteRepository(), nil, idGen)
		// **materialize が走らないことまで見る** (上の理由と同じ)。
		mat := &countingNoteMaterializer{}
		svc.SetNoteMaterializer(mat)

		err := svc.Vote(me, "n1", 0)
		assert.Zero(t, mat.asked, "DB 障害で remote fetch を走らせている")
		require.Error(t, err)
		assert.False(t, errors.Is(err, poll.ErrNoteNotFound),
			"DB 障害が not-found に丸められている")
		assert.ErrorIs(t, err, dbErr)
	})

	t.Run("poll lookup", func(t *testing.T) {
		noteRepo := testutil.NewMockNoteRepository()
		// **HasPoll を立てる。** false だと poll lookup の手前で ErrNoPoll が
		// 返り、guard に到達しない。
		noteRepo.Notes["n1"] = &model.Note{
			ID: "n1", UserID: "u2", Visibility: model.NoteVisibilityPublic, HasPoll: true,
		}
		svc := poll.NewService(noteRepo, &dbFailingPollRepo{
			MockPollRepository: testutil.NewMockPollRepository(), err: dbErr,
		}, testutil.NewMockPollVoteRepository(), nil, idGen)

		err := svc.Vote(me, "n1", 0)
		require.Error(t, err)
		assert.False(t, errors.Is(err, poll.ErrNoPoll),
			"DB 障害が「投票が無い」に化けている")
	})
}
