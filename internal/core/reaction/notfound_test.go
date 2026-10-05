package reaction

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func (r *dbFailingNoteRepo) FindByIDWithRelations(string) (*model.Note, error) {
	return nil, r.err
}

// **DB 障害を ErrNoteNotFound に丸めないこと** (#2799)。
//
// **materialize より前で分ける。** not-found でない error は materialize しても
// 直らないのに、`EnsureNote` がリモート fetch を走らせる。DB 断中は 1 リクエスト
// ごとに outbound HTTP が 1 発出る。
func TestReactionCreate_DBFailureIsNotNoteNotFound(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	idGen, _ := id.NewGenerator("aidx")
	svc := NewService(
		&dbFailingNoteRepo{MockNoteRepository: testutil.NewMockNoteRepository(), err: dbErr},
		testutil.NewMockNoteReactionRepository(), testutil.NewMockEmojiRepository(),
		testutil.NewMockFollowingRepository(), idGen)
	// **materialize が走らないことまで見る。** guard を materialize の後ろに
	// 置くと error の種別は同じなので、呼び出し回数を見ないと変異が生き残る。
	mat := &countingNoteMaterializer{}
	svc.SetNoteMaterializer(mat)

	_, err := svc.Create(&model.User{ID: "u1"}, "n1", "👍")
	assert.Zero(t, mat.asked, "DB 障害で remote fetch を走らせている")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNoteNotFound),
		"DB 障害が not-found に丸められている")
	assert.ErrorIs(t, err, dbErr, "元の error がそのまま返るべき")
}
