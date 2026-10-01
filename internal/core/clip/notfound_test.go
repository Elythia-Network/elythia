package clip_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/clip"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

// failingClipRepo makes every clip lookup look like a database failure.
type failingClipRepo struct {
	*testutil.MockClipRepository
	err error
}

func (r *failingClipRepo) FindByID(string) (*model.Clip, error) { return nil, r.err }

// failingNoteRepo makes every note lookup look like a database failure.
type failingNoteRepo struct {
	*testutil.MockNoteRepository
	err error
}

func (r *failingNoteRepo) FindByID(string) (*model.Note, error) { return nil, r.err }

// **DB 障害を ErrClipNotFound / ErrNoteNotFound に丸めないこと** (#2799)。
//
// handler は sentinel を 4xx にするので、service で潰すと接続断が
// 「そんなクリップは無い」として返る。
func TestClipService_DBFailureIsNotNotFound(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	idGen, _ := id.NewGenerator("aidx")

	newClipFailing := func() *clip.Service {
		return clip.NewService(
			&failingClipRepo{MockClipRepository: testutil.NewMockClipRepository(), err: dbErr},
			testutil.NewMockClipNoteRepository(), testutil.NewMockNoteRepository(), idGen)
	}

	for _, tt := range []struct {
		name string
		run  func(*clip.Service) error
	}{
		{"Show", func(s *clip.Service) error { _, err := s.Show("u1", "c1"); return err }},
		{"Get", func(s *clip.Service) error { _, err := s.Get("c1"); return err }},
		{"Update", func(s *clip.Service) error {
			name := "n"
			_, err := s.Update("u1", "c1", clip.UpdateInput{Name: &name})
			return err
		}},
		{"Delete", func(s *clip.Service) error { return s.Delete("u1", "c1") }},
		{"AddNote", func(s *clip.Service) error { return s.AddNote("u1", "c1", "n1") }},
		{"RemoveNote", func(s *clip.Service) error { return s.RemoveNote("u1", "c1", "n1") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.run(newClipFailing())
			require.Error(t, err)
			assert.False(t, errors.Is(err, clip.ErrClipNotFound),
				"DB 障害が not-found に丸められている")
			assert.ErrorIs(t, err, dbErr, "元の error がそのまま返るべき")
		})
	}

	// note 側の lookup も同じ (#2799)。clip は引けるが note が引けないケース。
	t.Run("AddNote/note lookup", func(t *testing.T) {
		repo := testutil.NewMockClipRepository()
		require.NoError(t, repo.Create(&model.Clip{ID: "c1", UserID: "u1", Name: "c"}))
		svc := clip.NewService(repo, testutil.NewMockClipNoteRepository(),
			&failingNoteRepo{MockNoteRepository: testutil.NewMockNoteRepository(), err: dbErr}, idGen)

		err := svc.AddNote("u1", "c1", "n1")
		require.Error(t, err)
		assert.False(t, errors.Is(err, clip.ErrNoteNotFound),
			"DB 障害が not-found に丸められている")
	})
}

// failingPairRepo makes the (clip, note) pair lookup look like a database
// failure while every other clip-note operation keeps working.
type failingPairRepo struct {
	*testutil.MockClipNoteRepository
	err error
}

func (r *failingPairRepo) FindByPair(string, string) (*model.ClipNote, error) {
	return nil, r.err
}

func (r *failingPairRepo) DeleteByPair(string, string) (int64, error) {
	return 0, r.err
}

// **pair lookup の DB 障害を「入っていない」に倒さないこと** (#2799)。
//
// この 2 つは gate の射程外なので、テストが唯一の回帰検知になる。`AddNote` の
// 重複チェックは `err == nil` だけを見る形、`RemoveNote` は削除件数 0 と
// error を区別しない形で、どちらも「IsNotFound を 4xx に潰す」形ではないため
// 述語に掛からない。
func TestClipService_PairLookupDBFailure(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	idGen, _ := id.NewGenerator("aidx")

	newSvc := func(t *testing.T) (*clip.Service, *testutil.MockClipNoteRepository) {
		t.Helper()
		clipRepo := testutil.NewMockClipRepository()
		require.NoError(t, clipRepo.Create(&model.Clip{ID: "c1", UserID: "u1", Name: "c"}))
		noteRepo := testutil.NewMockNoteRepository()
		noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u2",
			Visibility: model.NoteVisibilityPublic}
		pairRepo := testutil.NewMockClipNoteRepository()
		svc := clip.NewService(clipRepo,
			&failingPairRepo{MockClipNoteRepository: pairRepo, err: dbErr}, noteRepo, idGen)
		return svc, pairRepo
	}

	// **重複チェックを skip して二重に足さない。** `err == nil && dup != nil` の
	// 形だと、接続断のあいだ「まだ clip していない」と判断して行が増える。
	t.Run("AddNote", func(t *testing.T) {
		svc, pairRepo := newSvc(t)
		err := svc.AddNote("u1", "c1", "n1")
		require.Error(t, err)
		assert.ErrorIs(t, err, dbErr, "元の error がそのまま返るべき")
		assert.False(t, errors.Is(err, clip.ErrAlreadyClipped))
		assert.Empty(t, pairRepo.Notes, "DB 障害中に行を足している")
	})

	// **削除の DB 障害を NO_SUCH_NOTE にしない。** 4xx になると接続断が
	// 「clip に入っていない」として返り、原因が見えなくなる。
	t.Run("RemoveNote", func(t *testing.T) {
		svc, _ := newSvc(t)
		err := svc.RemoveNote("u1", "c1", "n1")
		require.Error(t, err, "DB 障害が silent success になっている")
		assert.ErrorIs(t, err, dbErr)
		assert.False(t, errors.Is(err, clip.ErrNoteNotFound))
	})
}
