package note

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

// dbFailingCreateNoteRepo makes the reply / renote target lookup look like a
// database failure.
type dbFailingCreateNoteRepo struct {
	*testutil.MockNoteRepository
	err error
}

func (r *dbFailingCreateNoteRepo) FindByIDWithUser(string) (*model.Note, error) {
	return nil, r.err
}

// countingNoteMaterializer records how many times it was asked to fetch.
type countingNoteMaterializer struct{ asked int }

func (m *countingNoteMaterializer) EnsureNote(context.Context, string) (*model.Note, error) {
	m.asked++
	return nil, errors.New("unreachable")
}

// **DB 障害を「返信先が無い」にしない** (#2799)。
//
// reply / renote 先の lookup は goroutine へ hoist されており、`if` は 130 行下の
// 別ブロックにある。gate は同じブロック内の後続 3 文しか見ないので**この形は
// 静的検査では捕まらない** — テストが唯一の回帰検知になる。書き込み系で最も
// 叩かれる endpoint なので、接続断が 400 NO_SUCH_REPLY_TARGET に化けると
// クライアントは「消えた」と判断する。
func TestCreate_DBFailureIsNotTargetNotFound(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	idGen, _ := id.NewGenerator("aidx")
	text := "hello"

	for _, tt := range []struct {
		name     string
		input    func(string) CreateInput
		sentinel error
	}{
		{"reply", func(id string) CreateInput {
			return CreateInput{User: &model.User{ID: "u1"}, Text: &text, ReplyID: &id}
		}, ErrReplyTargetNotFound},
		{"renote", func(id string) CreateInput {
			return CreateInput{User: &model.User{ID: "u1"}, Text: &text, RenoteID: &id}
		}, ErrRenoteTargetNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			failing := &dbFailingCreateNoteRepo{
				MockNoteRepository: testutil.NewMockNoteRepository(), err: dbErr,
			}
			svc := NewCreateService(failing, testutil.NewMockPollRepository(), idGen, nil)
			mat := &countingNoteMaterializer{}
			svc.SetNoteMaterializer(mat)

			target := "n1"
			_, err := svc.Create(tt.input(target))
			require.Error(t, err)
			assert.False(t, errors.Is(err, tt.sentinel),
				"DB 障害が not-found に丸められている")
			assert.ErrorIs(t, err, dbErr, "元の error がそのまま返るべき")
			assert.Zero(t, mat.asked, "DB 障害で ephemeral store を引いている")
		})
	}
}
