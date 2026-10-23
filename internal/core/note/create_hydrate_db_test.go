package note_test

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var createDB *gorm.DB

func TestMain(m *testing.M) {
	if db, err := testutil.OpenTestDB(); err == nil {
		testutil.ApplyMigrations(db)
		createDB = db
	}
	os.Exit(m.Run())
}

func requireCreateDB(t *testing.T) *gorm.DB {
	t.Helper()
	if createDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	return createDB
}

// comparableNote renders what the hooks and the packer read from a created
// note: the row, its poll, and the packed entity (which carries the author).
func comparableNote(t *testing.T, n *model.Note, idGen id.Generator) map[string]any {
	t.Helper()
	require.NotNil(t, n)
	row := *n
	row.User, row.Poll, row.Renote, row.Reply = nil, nil, nil, nil
	out := map[string]any{}
	// 作者は packed の user (UserLite) で比べる。行そのもの (notesCount /
	// updatedAt) は作成の後に非同期で増えるので、読み直した時刻で変わる。
	for k, v := range map[string]any{"row": row, "packed": (*entity.Packer)(nil).PackNote(n, idGen)} {
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		var m any
		require.NoError(t, json.Unmarshal(raw, &m))
		out[k] = m
	}
	if n.Poll != nil {
		p := *n.Poll
		p.Note = nil
		// timestamptz はマイクロ秒で保存されるので、比較はミリ秒 (API に出す精度) で行う。
		if p.ExpiresAt != nil {
			e := p.ExpiresAt.UTC().Truncate(time.Millisecond)
			p.ExpiresAt = &e
		}
		raw, err := json.Marshal(p)
		require.NoError(t, err)
		var m any
		require.NoError(t, json.Unmarshal(raw, &m))
		out["poll"] = m
	} else {
		out["poll"] = nil
	}
	return out
}

// A note that is neither a reply nor a renote is handed to the hooks as
// inserted, without being read again (#3611). It must equal what reading it
// back gives: the columns the DB fills on INSERT, the poll, and the author as
// stored (not the possibly stale authenticated user).
func TestCreateService_PlainNoteMatchesReread(t *testing.T) {
	db := requireCreateDB(t)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	noteRepo := repository.NewNoteRepository(db)
	pollRepo := repository.NewPollRepository(db)
	userRepo := repository.NewCachedUserRepository(repository.NewUserRepository(db))
	svc := note.NewCreateService(noteRepo, pollRepo, idGen, nil)
	svc.SetUserRepo(userRepo)

	const uid = "u_create3611"
	cleanup := func() {
		db.Exec(`DELETE FROM "poll" WHERE "userId" = ?`, uid)
		db.Exec(`DELETE FROM "note" WHERE "userId" = ?`, uid)
		db.Exec(`DELETE FROM "user" WHERE id = ?`, uid)
	}
	cleanup()
	t.Cleanup(cleanup)
	stored := &model.User{ID: uid, Username: "create3611", UsernameLower: "create3611", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	name := "stored name"
	stored.Name = &name
	require.NoError(t, db.Create(stored).Error)

	// 認証で使った利用者 (token のキャッシュ) は古い名前を持っているとする。
	stale := *stored
	oldName := "stale name"
	stale.Name = &oldName

	str := func(s string) *string { return &s }
	expires := time.Now().Add(time.Hour)
	accept := "likeOnly"
	cases := []struct {
		name string
		in   note.CreateInput
	}{
		{"text only", note.CreateInput{Text: str("hello #tag3611"), Visibility: model.NoteVisibilityPublic}},
		{"cw, home, local only, reaction acceptance", note.CreateInput{Text: str("body"), CW: str("cw"), Visibility: model.NoteVisibilityHome, LocalOnly: true, ReactionAcceptance: &accept}},
		{"specified", note.CreateInput{Text: str("secret"), Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: []string{uid}}},
		{"poll with expiry", note.CreateInput{Text: str("vote"), Visibility: model.NoteVisibilityPublic, Poll: &note.PollInput{Choices: []string{"a", "b"}, Multiple: true, ExpiresAt: &expires}}},
		{"poll without expiry", note.CreateInput{Text: str("vote2"), Visibility: model.NoteVisibilityFollowers, Poll: &note.PollInput{Choices: []string{"x"}}}},
	}
	for _, tc := range cases {
		in := tc.in
		in.User = &stale
		got, err := svc.Create(in)
		require.NoError(t, err, tc.name)
		reread, err := noteRepo.FindByIDWithRelations(got.ID)
		require.NoError(t, err, tc.name)
		assert.Equal(t, comparableNote(t, reread, idGen), comparableNote(t, got, idGen), tc.name)
		require.NotNil(t, got.User, tc.name)
		assert.Equal(t, "stored name", *got.User.Name, "%s: the author is read from the DB", tc.name)
		assert.Equal(t, in.Poll != nil, got.Poll != nil, tc.name)
	}

	// 返信は従来どおり読み直し、返信先を relation ごと持つ。
	parent, err := svc.Create(note.CreateInput{User: &stale, Text: str("parent"), Visibility: model.NoteVisibilityPublic})
	require.NoError(t, err)
	reply, err := svc.Create(note.CreateInput{User: &stale, Text: str("child"), Visibility: model.NoteVisibilityPublic, ReplyID: &parent.ID})
	require.NoError(t, err)
	require.NotNil(t, reply.Reply)
	require.NotNil(t, reply.Reply.User)
	reread, err := noteRepo.FindByIDWithRelations(reply.ID)
	require.NoError(t, err)
	assert.Equal(t, comparableNote(t, reread, idGen), comparableNote(t, reply, idGen))

	// リノートと引用も読み直し、引用先を 2 段目まで持つ (review M-3)。
	// 引用 (quote) は投票付きの parent を引き、純粋なリノート (boost) はその引用を
	// 引く。boost の renote.renote が埋まっていないと、引用先が「削除された
	// ノート」になる。
	polled, err := svc.Create(note.CreateInput{User: &stale, Text: str("polled"), Visibility: model.NoteVisibilityPublic, Poll: &note.PollInput{Choices: []string{"p", "q"}}})
	require.NoError(t, err)
	quote, err := svc.Create(note.CreateInput{User: &stale, Text: str("quote"), Visibility: model.NoteVisibilityPublic, RenoteID: &polled.ID})
	require.NoError(t, err)
	boost, err := svc.Create(note.CreateInput{User: &stale, Visibility: model.NoteVisibilityPublic, RenoteID: &quote.ID})
	require.NoError(t, err)
	for _, n := range []*model.Note{quote, boost} {
		require.NotNil(t, n.Renote, n.ID)
		require.NotNil(t, n.Renote.User, n.ID)
		reread, err := noteRepo.FindByIDWithRelations(n.ID)
		require.NoError(t, err)
		assert.Equal(t, comparableNote(t, reread, idGen), comparableNote(t, n, idGen), n.ID)
	}
	require.NotNil(t, quote.Renote.Poll, "the quoted note's poll")
	require.NotNil(t, boost.Renote.Renote, "depth-2 renote of a boost")
	require.NotNil(t, boost.Renote.Renote.User)
	require.NotNil(t, boost.Renote.Renote.Poll)
}
