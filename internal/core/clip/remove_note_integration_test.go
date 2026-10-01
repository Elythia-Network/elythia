package clip_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/shiroha-a/mk/internal/core/clip"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/testutil"
)

// 本家 4682d44cae: 同じ (clip, note) を並行に remove しても、実際に消せた
// 1 回だけが clippedCount を減らし、残りは NO_SUCH_NOTE になる。
// FindByPair → Delete の 2 段だと、両方が見つけて両方減らし負になりうる。
func TestRemoveNote_ConcurrentRemovesDecrementOnce(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)

	token := "tok_u_clrace"
	user := &model.User{
		ID: "u_clrace", Username: "clrace", UsernameLower: "clrace",
		Token: &token, AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, db.Create(user).Error)
	t.Cleanup(func() { db.Exec(`DELETE FROM "user" WHERE "id" = ?`, user.ID) })

	note := &model.Note{
		ID: "n_clrace", UserID: user.ID, Visibility: model.NoteVisibilityPublic,
		Reactions: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, db.Create(note).Error)
	t.Cleanup(func() { db.Exec(`DELETE FROM "note" WHERE "id" = ?`, note.ID) })

	c := &model.Clip{ID: "clp_clrace", UserID: user.ID, Name: "race"}
	require.NoError(t, db.Create(c).Error)
	t.Cleanup(func() { db.Exec(`DELETE FROM "clip" WHERE "id" = ?`, c.ID) })

	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	svc := clip.NewService(repository.NewClipRepository(db), repository.NewClipNoteRepository(db),
		repository.NewNoteRepository(db), idGen)
	require.NoError(t, svc.AddNote(user.ID, c.ID, note.ID))

	clippedCount := func() int16 {
		var n model.Note
		require.NoError(t, db.Select(`"clippedCount"`).Where(`"id" = ?`, note.ID).First(&n).Error)
		return n.ClippedCount
	}
	require.Equal(t, int16(1), clippedCount())

	const workers = 8
	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		errs  = make([]error, workers)
	)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = svc.RemoveNote(user.ID, c.ID, note.ID)
		}()
	}
	close(start)
	wg.Wait()

	var ok, noSuch int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, clip.ErrNoteNotFound):
			noSuch++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, ok, "exactly one remove should succeed")
	assert.Equal(t, workers-1, noSuch)
	assert.Equal(t, int16(0), clippedCount(), "clippedCount must be decremented exactly once")
}
