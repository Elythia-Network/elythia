package clips

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// dbFailingWithUserRepo fails the lookup RequireVisible actually uses.
type dbFailingWithUserRepo struct {
	*testutil.MockNoteRepository
	err error
}

func (r *dbFailingWithUserRepo) FindByIDWithUser(string) (*model.Note, error) {
	return nil, r.err
}

// countingMaterializer records how many remote fetches it was asked to make.
type countingMaterializer struct{ asked int }

func (m *countingMaterializer) EnsureNote(context.Context, string) (*model.Note, error) {
	m.asked++
	return nil, errors.New("unreachable")
}

// **DB 障害では materialize を走らせない** (#2799)。
//
// `clips/add-note` は `RequireVisible` の err を種別を見ずに materialize へ渡して
// いた。接続断のあいだ 1 リクエストごとに outbound の remote-note fetch が 1 発
// 出る。sentinel のときは従来どおり走ることも同時に固定する。
func TestClipsMaterializeIfMissing_SkipsOnDBFailure(t *testing.T) {
	me := &model.User{ID: "u1"}

	t.Run("DB 障害では走らない", func(t *testing.T) {
		failing := &dbFailingWithUserRepo{
			MockNoteRepository: testutil.NewMockNoteRepository(),
			err:                errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"),
		}
		h, _, _, _ := newHandler(t)
		h.SetQueryService(corenote.NewQueryService(failing, nil))
		mat := &countingMaterializer{}
		h.SetNoteMaterializer(mat)

		_, err := h.queryService.RequireVisible(me, "n1")
		assert.False(t, h.materializeIfMissing("n1", err))
		assert.Zero(t, mat.asked, "DB 障害で remote fetch を走らせている")
	})

	t.Run("not-found では走る", func(t *testing.T) {
		h, _, _, _ := newHandler(t)
		h.SetQueryService(corenote.NewQueryService(testutil.NewMockNoteRepository(), nil))
		mat := &countingMaterializer{}
		h.SetNoteMaterializer(mat)

		_, err := h.queryService.RequireVisible(me, "missing")
		h.materializeIfMissing("missing", err)
		assert.Equal(t, 1, mat.asked, "ephemeral note の取り込みが走らなくなっている")
	})
}
