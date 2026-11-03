package notesfilter

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// countingLogger counts every statement GORM traces.
type countingLogger struct {
	logger.Interface
	n atomic.Int64
}

func (c *countingLogger) LogMode(logger.LogLevel) logger.Interface { return c }

func (c *countingLogger) Trace(context.Context, time.Time, func() (string, int64), error) {
	c.n.Add(1)
}

// TestApplyMuteBlockChannel_HydratedListIssuesNoQuery pins, against the real
// repository, that a list read through the hydrate (#3611) already carries the
// renote targets, so the nested mute/block check issues no SQL (#3965), and
// that the check still drops a renote whose target replies to a muted user.
func TestApplyMuteBlockChannel_HydratedListIssuesNoQuery(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)
	const prefix = "nfm3965_"
	cleanup := func() {
		db.Exec(`DELETE FROM "note" WHERE id LIKE ?`, prefix+"%")
		db.Exec(`DELETE FROM "user" WHERE id LIKE ?`, prefix+"%")
	}
	cleanup()
	t.Cleanup(cleanup)

	mkUser := func(name string) string {
		id := prefix + name
		require.NoError(t, db.Create(&model.User{ID: id, Username: id, UsernameLower: id, AvatarDecorations: datatypes.JSON([]byte("[]"))}).Error)
		return id
	}
	author, other, muted := mkUser("author"), mkUser("other"), mkUser("muted")
	mkNote := func(id string, n model.Note) {
		n.ID = prefix + id
		n.Visibility = model.NoteVisibility("public")
		require.NoError(t, db.Create(&n).Error)
	}
	mkNote("0mutedpost", model.Note{UserID: muted})
	mkNote("1replytomuted", model.Note{UserID: other, ReplyID: strptr(prefix + "0mutedpost"), ReplyUserID: strptr(muted)})
	mkNote("2cleanpost", model.Note{UserID: other})
	mkNote("3boostreply", model.Note{UserID: author, RenoteID: strptr(prefix + "1replytomuted"), RenoteUserID: strptr(other)})
	mkNote("4boostclean", model.Note{UserID: author, RenoteID: strptr(prefix + "2cleanpost"), RenoteUserID: strptr(other)})

	counter := &countingLogger{Interface: logger.Discard}
	repo := repository.NewNoteRepository(db.Session(&gorm.Session{Logger: counter}))
	notes, err := repo.ListByUserIDFiltered(author, author, "", "", 20, false, true, true, false)
	require.NoError(t, err)
	require.Len(t, notes, 2)

	sets := MuteBlockSets{MutedUserIDs: map[string]struct{}{muted: {}}}
	before := counter.n.Load()
	out, err := ApplyMuteBlockChannel(notes, sets, repo)
	require.NoError(t, err)
	assert.Zero(t, counter.n.Load()-before, "renote targets loaded by the hydrate must not be read again")
	require.Len(t, out, 1)
	assert.Equal(t, prefix+"4boostclean", out[0].ID)
}
