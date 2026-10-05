package ephemeral_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/ephemeral"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeNoteLookup stands in for the database fallback used to resolve quote
// targets that are not in Redis.
type fakeNoteLookup struct {
	notes map[string]*model.Note
	err   error
	calls [][]string
}

func (f *fakeNoteLookup) FindManyByIDsWithUser(ids []string) ([]*model.Note, error) {
	f.calls = append(f.calls, ids)
	if f.err != nil {
		return nil, f.err
	}
	out := []*model.Note{}
	for _, id := range ids {
		if n, ok := f.notes[id]; ok {
			out = append(out, n)
		}
	}
	return out, nil
}

func putNote(t *testing.T, s *ephemeral.Store, id, uri string, author *model.User) *model.Note {
	t.Helper()
	n := &model.Note{ID: id, URI: strptr(uri), UserID: author.ID}
	require.NoError(t, s.PutNote(context.Background(), n, author))
	return n
}

func ephAuthor(id string) *model.User {
	return &model.User{ID: id, Username: id, URI: strptr("https://remote.example/users/" + id)}
}

// 引用先もリレー由来 (= Redis 側) のとき、Renote が埋まる。埋まらないと
// frontend が引用先を「削除された投稿」として描画する (#2397)。
func TestStore_GetNotes_ResolvesQuoteTargetFromRedis(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	author := ephAuthor("alice")

	quoted := putNote(t, s, "quoted1", "https://remote.example/notes/quoted1", author)
	quoting := &model.Note{
		ID: "quoting1", URI: strptr("https://remote.example/notes/quoting1"),
		UserID: author.ID, RenoteID: &quoted.ID,
	}
	require.NoError(t, s.PutNote(ctx, quoting, author))

	got, err := s.GetNotes(ctx, []string{"quoting1"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].Renote, "引用先が埋まっていないと削除ノート表示になる")
	assert.Equal(t, "quoted1", got[0].Renote.ID)
	// 引用先の著者も要る。PackNote は renote.user を読む。
	require.NotNil(t, got[0].Renote.User)
	assert.Equal(t, "alice", got[0].Renote.User.ID)
}

// 引用先が DB 側 (= リレー由来でない投稿を引用) のとき、fallback で埋まる。
func TestStore_GetNotes_ResolvesQuoteTargetFromDB(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	author := ephAuthor("alice")

	dbNote := &model.Note{ID: "dbnote1", UserID: "bob", User: &model.User{ID: "bob"}}
	lookup := &fakeNoteLookup{notes: map[string]*model.Note{"dbnote1": dbNote}}
	s.SetNoteLookup(lookup)

	quoting := &model.Note{
		ID: "quoting2", URI: strptr("https://remote.example/notes/quoting2"),
		UserID: author.ID, RenoteID: strptr("dbnote1"),
	}
	require.NoError(t, s.PutNote(ctx, quoting, author))

	got, err := s.GetNotes(ctx, []string{"quoting2"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].Renote)
	assert.Equal(t, "dbnote1", got[0].Renote.ID)
}

// Redis で引けたぶんは DB に問い合わせない。両方に投げると引用の多い
// タイムラインで無駄なクエリが増える。
func TestStore_GetNotes_SkipsDBWhenRedisHasQuoteTarget(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	author := ephAuthor("alice")
	lookup := &fakeNoteLookup{notes: map[string]*model.Note{}}
	s.SetNoteLookup(lookup)

	quoted := putNote(t, s, "quoted3", "https://remote.example/notes/quoted3", author)
	quoting := &model.Note{
		ID: "quoting3", URI: strptr("https://remote.example/notes/quoting3"),
		UserID: author.ID, RenoteID: &quoted.ID,
	}
	require.NoError(t, s.PutNote(ctx, quoting, author))

	_, err := s.GetNotes(ctx, []string{"quoting3"})
	require.NoError(t, err)
	assert.Empty(t, lookup.calls, "Redis で解決できた引用先を DB へ引きに行かない")
}

// 引用先が本当に存在しない場合は nil のまま。無い引用先を捏造しない
// (従来どおり削除ノート表示になるのが正しい)。
func TestStore_GetNotes_MissingQuoteTargetStaysNil(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	author := ephAuthor("alice")
	s.SetNoteLookup(&fakeNoteLookup{notes: map[string]*model.Note{}})

	quoting := &model.Note{
		ID: "quoting4", URI: strptr("https://remote.example/notes/quoting4"),
		UserID: author.ID, RenoteID: strptr("gone"),
	}
	require.NoError(t, s.PutNote(ctx, quoting, author))

	got, err := s.GetNotes(ctx, []string{"quoting4"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Nil(t, got[0].Renote)
}

// DB fallback が失敗しても note 自体は返す。引用先が引けないことで
// タイムラインごと落とさない。
func TestStore_GetNotes_QuoteLookupErrorKeepsNote(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	author := ephAuthor("alice")
	s.SetNoteLookup(&fakeNoteLookup{err: errors.New("db down")})

	quoting := &model.Note{
		ID: "quoting5", URI: strptr("https://remote.example/notes/quoting5"),
		UserID: author.ID, RenoteID: strptr("dbnote9"),
	}
	require.NoError(t, s.PutNote(ctx, quoting, author))

	got, err := s.GetNotes(ctx, []string{"quoting5"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Nil(t, got[0].Renote)
}

// NoteLookup 未配線でも Redis 側の引用先は埋まる。
func TestStore_GetNotes_QuoteTargetWithoutLookup(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	author := ephAuthor("alice")

	quoted := putNote(t, s, "quoted6", "https://remote.example/notes/quoted6", author)
	quoting := &model.Note{
		ID: "quoting6", URI: strptr("https://remote.example/notes/quoting6"),
		UserID: author.ID, RenoteID: &quoted.ID,
	}
	require.NoError(t, s.PutNote(ctx, quoting, author))

	got, err := s.GetNotes(ctx, []string{"quoting6"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].Renote)
	assert.Equal(t, "quoted6", got[0].Renote.ID)
}

// 引用の無いノートでは DB へ問い合わせない。
func TestStore_GetNotes_NoQuoteNoLookup(t *testing.T) {
	s := newStore(t, time.Minute)
	ctx := context.Background()
	author := ephAuthor("alice")
	lookup := &fakeNoteLookup{notes: map[string]*model.Note{}}
	s.SetNoteLookup(lookup)

	putNote(t, s, "plain1", "https://remote.example/notes/plain1", author)

	got, err := s.GetNotes(ctx, []string{"plain1"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Empty(t, lookup.calls)
}
