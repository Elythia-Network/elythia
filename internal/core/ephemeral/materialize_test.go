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

// fakeNotes is the database side of materialization.
type fakeNotes struct {
	rows      map[string]*model.Note
	createErr error
	created   []string
}

func newFakeNotes() *fakeNotes { return &fakeNotes{rows: map[string]*model.Note{}} }

func (f *fakeNotes) FindByID(id string) (*model.Note, error) {
	if n, ok := f.rows[id]; ok {
		return n, nil
	}
	return nil, errors.New("not found")
}

func (f *fakeNotes) Create(n *model.Note) error {
	if f.createErr != nil {
		return f.createErr
	}
	f.rows[n.ID] = n
	f.created = append(f.created, n.ID)
	return nil
}

// fakeActor records the ID it was asked to reuse.
type fakeActor struct {
	askedURI string
	askedID  string
	err      error
}

func (f *fakeActor) MaterializeActor(uri, preassignedID string) (*model.User, error) {
	f.askedURI, f.askedID = uri, preassignedID
	if f.err != nil {
		return nil, f.err
	}
	host := "remote.example"
	return &model.User{ID: preassignedID, Username: "alice", Host: &host, URI: &uri}, nil
}

func newMaterializer(t *testing.T) (*ephemeral.Materializer, *ephemeral.Store, *fakeNotes, *fakeActor) {
	t.Helper()
	s := newStore(t, time.Minute)
	notes := newFakeNotes()
	actor := &fakeActor{}
	return ephemeral.NewMaterializer(s, notes, actor), s, notes, actor
}

func TestMaterializer_EnsureNote_PromotesFromRedis(t *testing.T) {
	m, store, notes, actor := newMaterializer(t)
	ctx := context.Background()
	uri := "https://remote.example/notes/1"
	actorURI := "https://remote.example/users/alice"
	require.NoError(t, store.PutNote(ctx, sampleNote("n1", uri, "u1"), sampleUser("u1", actorURI)))

	got, err := m.EnsureNote(ctx, "n1")
	require.NoError(t, err)
	require.NotNil(t, got)

	assert.Equal(t, []string{"n1"}, notes.created, "DB 行が作られること")
	assert.Equal(t, actorURI, actor.askedURI, "著者も起こすこと")

	// ephemeral 側は落ちること (残すと timeline で二重に出る)。
	left, err := store.GetNote(ctx, "n1")
	require.NoError(t, err)
	assert.Nil(t, left)
}

// **最重要**: materialize しても ID が変わらないこと。
//
// 変わると Redis に残っている既存ノートが古い ID を指したままになり、ミュート
// したのにタイムラインから消えない状態が TTL 切れまで続く。
func TestMaterializer_PreservesIDs(t *testing.T) {
	m, store, notes, actor := newMaterializer(t)
	ctx := context.Background()
	require.NoError(t, store.PutNote(ctx,
		sampleNote("note-id-kept", "https://remote.example/notes/1", "user-id-kept"),
		sampleUser("user-id-kept", "https://remote.example/users/alice")))

	got, err := m.EnsureNote(ctx, "note-id-kept")
	require.NoError(t, err)

	assert.Equal(t, "note-id-kept", got.ID, "ノート ID が据え置かれること")
	assert.Contains(t, notes.rows, "note-id-kept")
	assert.Equal(t, "user-id-kept", actor.askedID, "著者 ID も据え置いて渡すこと")
}

// 既に DB にあるノートでは Redis を引かない (ホットパスの追加コストが無い)。
func TestMaterializer_ExistingDBRowShortCircuits(t *testing.T) {
	m, _, notes, actor := newMaterializer(t)
	notes.rows["db1"] = &model.Note{ID: "db1"}

	got, err := m.EnsureNote(context.Background(), "db1")
	require.NoError(t, err)
	assert.Equal(t, "db1", got.ID)
	assert.Empty(t, notes.created, "既存行では Create しない")
	assert.Empty(t, actor.askedURI, "著者解決も走らない")
}

func TestMaterializer_MissingEverywhere(t *testing.T) {
	m, _, _, _ := newMaterializer(t)
	_, err := m.EnsureNote(context.Background(), "ghost")
	assert.ErrorIs(t, err, ephemeral.ErrNoteNotFound)
}

func TestMaterializer_EmptyID(t *testing.T) {
	m, _, _, _ := newMaterializer(t)
	_, err := m.EnsureNote(context.Background(), "")
	assert.ErrorIs(t, err, ephemeral.ErrNoteNotFound)
}

// 著者が TTL 切れで引けないノートは起こせない (FK が張れないため)。
func TestMaterializer_AuthorMissing(t *testing.T) {
	m, store, notes, _ := newMaterializer(t)
	ctx := context.Background()
	require.NoError(t, store.PutNote(ctx, sampleNote("n1", "https://remote.example/notes/1", "u1"),
		sampleUser("u1", "https://remote.example/users/alice")))
	require.NoError(t, testRedis.Client.Del(ctx, "example.com:ephUser:u1").Err())

	_, err := m.EnsureNote(ctx, "n1")
	assert.ErrorIs(t, err, ephemeral.ErrNoteNotFound)
	assert.Empty(t, notes.created, "著者を起こせないなら note も作らない")
}

// 著者の materialize に失敗したらノートも作らない (FK 違反を避ける)。
func TestMaterializer_ActorFailureAbortsNote(t *testing.T) {
	m, store, notes, actor := newMaterializer(t)
	ctx := context.Background()
	actor.err = errors.New("actor fetch failed")
	require.NoError(t, store.PutNote(ctx, sampleNote("n1", "https://remote.example/notes/1", "u1"),
		sampleUser("u1", "https://remote.example/users/alice")))

	_, err := m.EnsureNote(ctx, "n1")
	assert.Error(t, err)
	assert.Empty(t, notes.created)
}

// 競合で先に別経路が作っていたら、それを正として返す。
func TestMaterializer_CreateRaceReturnsExisting(t *testing.T) {
	m, store, notes, _ := newMaterializer(t)
	ctx := context.Background()
	require.NoError(t, store.PutNote(ctx, sampleNote("n1", "https://remote.example/notes/1", "u1"),
		sampleUser("u1", "https://remote.example/users/alice")))
	notes.createErr = errors.New("duplicate key")
	notes.rows["n1"] = &model.Note{ID: "n1", Text: strptr("won the race")}

	got, err := m.EnsureNote(ctx, "n1")
	require.NoError(t, err)
	assert.Equal(t, "won the race", *got.Text)
}

// --- EnsureUser (ノートを伴わない契機) ---

// ミュート / ブロック / 通報は user への FK だけを要求する。
func TestMaterializer_EnsureUser(t *testing.T) {
	m, store, notes, actor := newMaterializer(t)
	ctx := context.Background()
	actorURI := "https://remote.example/users/alice"
	require.NoError(t, store.PutNote(ctx, sampleNote("n1", "https://remote.example/notes/1", "u1"),
		sampleUser("u1", actorURI)))

	got, err := m.EnsureUser(ctx, "u1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "u1", got.ID, "著者 ID が据え置かれること")
	assert.Equal(t, actorURI, actor.askedURI)
	assert.Empty(t, notes.created, "ノート行は作らない")
}

func TestMaterializer_EnsureUser_Missing(t *testing.T) {
	m, _, _, _ := newMaterializer(t)
	_, err := m.EnsureUser(context.Background(), "ghost")
	assert.ErrorIs(t, err, ephemeral.ErrNoteNotFound)
}

// store 未配線では単なる DB 参照に縮退する (機能未使用時)。
func TestMaterializer_NoStoreDegradesToDB(t *testing.T) {
	notes := newFakeNotes()
	notes.rows["db1"] = &model.Note{ID: "db1"}
	m := ephemeral.NewMaterializer(nil, notes, &fakeActor{})

	got, err := m.EnsureNote(context.Background(), "db1")
	require.NoError(t, err)
	assert.Equal(t, "db1", got.ID)

	_, err = m.EnsureNote(context.Background(), "ghost")
	assert.ErrorIs(t, err, ephemeral.ErrNoteNotFound)
}

func TestMaterializer_NilSafe(t *testing.T) {
	var m *ephemeral.Materializer
	_, err := m.EnsureNote(context.Background(), "x")
	assert.ErrorIs(t, err, ephemeral.ErrNoteNotFound)
	_, err = m.EnsureUser(context.Background(), "x")
	assert.ErrorIs(t, err, ephemeral.ErrNoteNotFound)
}
