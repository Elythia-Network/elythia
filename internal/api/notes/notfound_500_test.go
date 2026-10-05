package notes

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// dbFailingNoteRepo makes every note lookup look like a database failure.
type dbFailingNoteRepo struct {
	*testutil.MockNoteRepository
	err error
}

func (r *dbFailingNoteRepo) FindByID(string) (*model.Note, error) { return nil, r.err }

// unrenote は対象 note と renote 行を引くので、両方を落とす。
func (r *dbFailingNoteRepo) ListRenotesByUser(string, string) ([]*model.Note, error) {
	return nil, r.err
}

// newDBFailingHandler builds a Handler whose note lookups all fail with a
// database error.
func newDBFailingHandler(t *testing.T, err error) *Handler {
	t.Helper()
	failing := &dbFailingNoteRepo{MockNoteRepository: testutil.NewMockNoteRepository(), err: err}
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	return NewHandler(failing,
		corenote.NewCreateService(failing, pollRepo, idGen, nil),
		corenote.NewDeleteService(failing),
		corenote.NewQueryService(failing, nil),
		nil, nil, nil, nil, idGen)
}

// **DB 障害を「そんなノートは無い」にしない** (#2792)。
//
// note を引く endpoint をまとめて固定する。ここが無いと guard を巻き戻しても
// CI が緑のまま通る。
func TestNotes_DBFailureIsNot4xx(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	me := &model.User{ID: "u1"}

	for _, tt := range []struct {
		name string
		run  func(*Handler) int
	}{
		{"notes/favorites/delete", func(h *Handler) int {
			return postExtra(h.FavoritesDelete, `{"noteId":"n1"}`, me).Code
		}},
		{"notes/unrenote", func(h *Handler) int {
			return postExtra(h.Unrenote, `{"noteId":"n1"}`, me).Code
		}},
		{"notes/thread-muting/create", func(h *Handler) int {
			return postExtra(h.ThreadMutingCreate, `{"noteId":"n1"}`, me).Code
		}},
		{"notes/thread-muting/delete", func(h *Handler) int {
			return postExtra(h.ThreadMutingDelete, `{"noteId":"n1"}`, me).Code
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, http.StatusInternalServerError, tt.run(newDBFailingHandler(t, dbErr)),
				"DB 障害が 4xx に化けている (#2792)")
		})
	}
}

// **DB 障害を「そんなノートは無い」にしない** (#2792)。
func TestTranslate_DBFailureIsNot4xx(t *testing.T) {
	// **Handler 構築時に repo を差し替える。** notes には後付けの setter が無い。
	failing := &dbFailingNoteRepo{
		MockNoteRepository: testutil.NewMockNoteRepository(),
		err:                errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"),
	}
	pollRepo := testutil.NewMockPollRepository()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(failing,
		corenote.NewCreateService(failing, pollRepo, idGen, nil),
		corenote.NewDeleteService(failing),
		corenote.NewQueryService(failing, nil),
		nil, nil, nil, nil, idGen)

	rec := postExtra(h.Translate, `{"noteId":"n1","targetLang":"en"}`, &model.User{ID: "u1"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code,
		"DB 障害が 4xx に化けている (#2792)")
}

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
// `RequireVisible` は not-found と非可視を `ErrNoteNotFound` に集約し、接続断だけ
// raw error を返す。種別を見ずに materialize すると、DB 断のあいだ 1 リクエスト
// ごとに outbound の remote-note fetch が 1 発出る。sentinel のときは従来どおり
// 走ることも同時に固定する (走らなくすると ephemeral note の取り込みが壊れる)。
func TestMaterializeIfMissing_SkipsOnDBFailure(t *testing.T) {
	t.Run("DB 障害では走らない", func(t *testing.T) {
		// **`FindByIDWithUser` を落とす。** `RequireVisible` が引くのはこちらで、
		// `FindByID` だけ落としても mock が not-found を返して sentinel 経路に
		// なる (実際にそうなって空振りした)。
		failing := &dbFailingWithUserRepo{
			MockNoteRepository: testutil.NewMockNoteRepository(),
			err:                errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"),
		}
		pollRepo := testutil.NewMockPollRepository()
		idGen, _ := id.NewGenerator("aidx")
		h := NewHandler(failing,
			corenote.NewCreateService(failing, pollRepo, idGen, nil),
			corenote.NewDeleteService(failing),
			corenote.NewQueryService(failing, nil),
			nil, nil, nil, nil, idGen)
		mat := &countingMaterializer{}
		h.SetNoteMaterializer(mat)

		_, err := h.queryService.RequireVisible(&model.User{ID: "u1"}, "n1")
		assert.False(t, h.materializeIfMissing("n1", err))
		assert.Zero(t, mat.asked, "DB 障害で remote fetch を走らせている")
	})

	t.Run("not-found では走る", func(t *testing.T) {
		repo := testutil.NewMockNoteRepository()
		pollRepo := testutil.NewMockPollRepository()
		idGen, _ := id.NewGenerator("aidx")
		h := NewHandler(repo,
			corenote.NewCreateService(repo, pollRepo, idGen, nil),
			corenote.NewDeleteService(repo),
			corenote.NewQueryService(repo, nil),
			nil, nil, nil, nil, idGen)
		mat := &countingMaterializer{}
		h.SetNoteMaterializer(mat)

		_, err := h.queryService.RequireVisible(&model.User{ID: "u1"}, "missing")
		h.materializeIfMissing("missing", err)
		assert.Equal(t, 1, mat.asked, "ephemeral note の取り込みが走らなくなっている")
	})
}

// dbFailingQueryRepo fails the lookups State / Conversation go through.
type dbFailingQueryRepo struct {
	*testutil.MockNoteRepository
	err error
}

func (r *dbFailingQueryRepo) FindByIDWithUser(string) (*model.Note, error) { return nil, r.err }

func (r *dbFailingQueryRepo) FindByIDWithRelations(string) (*model.Note, error) {
	return nil, r.err
}

// **service を直しても handler で潰れる形を塞ぐ** (#2799)。
//
// `State` / `Conversation` は #2799 で raw DB error も返すようになったが、handler
// は種別を見ずに NO_SUCH_NOTE へ潰していた。gate は repository の lookup しか
// 見ないので、この形は静的検査では捕まらない。
func TestNotes_ServiceErrorNotLaundered(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	me := &model.User{ID: "u1"}

	newH := func() *Handler {
		failing := &dbFailingQueryRepo{
			MockNoteRepository: testutil.NewMockNoteRepository(), err: dbErr,
		}
		pollRepo := testutil.NewMockPollRepository()
		idGen, _ := id.NewGenerator("aidx")
		return NewHandler(failing,
			corenote.NewCreateService(failing, pollRepo, idGen, nil),
			corenote.NewDeleteService(failing),
			corenote.NewQueryService(failing, nil),
			nil, nil, nil, nil, idGen)
	}

	for _, tt := range []struct {
		name string
		run  func(*Handler) int
	}{
		{"notes/state", func(h *Handler) int {
			return postExtra(h.State, `{"noteId":"n1"}`, me).Code
		}},
		{"notes/conversation", func(h *Handler) int {
			return postExtra(h.Conversation, `{"noteId":"n1"}`, me).Code
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, http.StatusInternalServerError, tt.run(newH()),
				"DB 障害が 4xx に化けている (#2799)")
		})
	}
}
