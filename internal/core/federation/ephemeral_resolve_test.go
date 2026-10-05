package federation_test

import (
	"context"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSink captures what the ephemeral path would write to Redis.
type fakeSink struct {
	notes    map[string]*model.Note
	authors  map[string]*model.User
	byURI    map[string]string
	putErr   error
	dropped  []string
	disabled bool
}

func newFakeSink() *fakeSink {
	return &fakeSink{
		notes:   map[string]*model.Note{},
		authors: map[string]*model.User{},
		byURI:   map[string]string{},
	}
}

func (f *fakeSink) Enabled() bool { return !f.disabled }

func (f *fakeSink) PutNote(_ context.Context, n *model.Note, author *model.User) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.notes[n.ID] = n
	f.authors[author.ID] = author
	if author.URI != nil {
		f.byURI[*author.URI] = author.ID
	}
	return nil
}

func (f *fakeSink) UserIDByURI(_ context.Context, uri string) (string, error) {
	return f.byURI[uri], nil
}

func (f *fakeSink) GetUser(_ context.Context, id string) (*model.User, error) {
	return f.authors[id], nil
}

func (f *fakeSink) NoteIDByURI(_ context.Context, uri string) (string, error) {
	for id, n := range f.notes {
		if n.URI != nil && *n.URI == uri {
			return id, nil
		}
	}
	return "", nil
}

func (f *fakeSink) GetNote(_ context.Context, id string) (*model.Note, error) {
	return f.notes[id], nil
}

func (f *fakeSink) DropNote(_ context.Context, id, _ string) error {
	delete(f.notes, id)
	f.dropped = append(f.dropped, id)
	return nil
}

// docFetcher serves a different document per URI so that a note fetch and the
// subsequent actor fetch can both be satisfied.
type docFetcher struct{ docs map[string]string }

func (d *docFetcher) FetchObject(uri string) ([]byte, error) {
	if body, ok := d.docs[uri]; ok {
		return []byte(body), nil
	}
	return nil, assertNoDoc{uri}
}

type assertNoDoc struct{ uri string }

func (e assertNoDoc) Error() string { return "no fixture for " + e.uri }

// ephResolverDocs builds a Resolver serving documents per URI.
func ephResolverDocs(t *testing.T, docs map[string]string) (*federation.Resolver, *fakeSink, *testutil.MockNoteRepository, *testutil.MockUserRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := federation.NewResolver(userRepo, noteRepo, urls, &docFetcher{docs: docs}, idGen)
	sink := newFakeSink()
	r.SetEphemeralSink(sink)
	return r, sink, noteRepo, userRepo
}

// ephResolver builds a Resolver whose fetcher returns the given document for
// every request, with the ephemeral sink wired.
func ephResolver(t *testing.T, body string) (*federation.Resolver, *fakeSink, *testutil.MockNoteRepository, *testutil.MockUserRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := federation.NewResolver(userRepo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	sink := newFakeSink()
	r.SetEphemeralSink(sink)
	return r, sink, noteRepo, userRepo
}

const ephActorDoc = `{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": "https://remote.example/users/alice",
	"type": "Person",
	"preferredUsername": "alice",
	"inbox": "https://remote.example/users/alice/inbox",
	"publicKey": {"publicKeyPem": "PEM"}
}`

// TestResolveNoteEphemeral_DoesNotTouchNoteRepo は本機能の核心。
// リレー由来の投稿が DB に 1 行も入らないこと。
func TestResolveNoteEphemeral_DoesNotTouchNoteRepo(t *testing.T) {
	doc := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/notes/1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "relay hello",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	r, sink, noteRepo, userRepo := ephResolverDocs(t, map[string]string{
		"https://remote.example/notes/1":     doc,
		"https://remote.example/users/alice": ephActorDoc,
	})

	note, err := r.ResolveNoteEphemeral("https://remote.example/notes/1")
	require.NoError(t, err)
	require.NotNil(t, note)

	assert.Empty(t, noteRepo.Notes, "ephemeral 経路では note 行を作らない")
	require.Len(t, sink.notes, 1, "ephemeral store に入っていること")
	assert.Equal(t, note.ID, sink.notes[note.ID].ID)

	// 著者も DB に入れない。実測でリレー購読後は note と user がほぼ 1:1 で
	// 増えるため、著者を止めないと肥大化を抑える目的が半分しか達成できない。
	assert.Empty(t, userRepo.Users, "著者も DB 行を作らないこと")
	require.Len(t, sink.authors, 1, "著者は ephemeral store に入ること")
}

// 既に DB に在る URI は DB 行をそのまま返す (直接配送で来ていた場合)。
func TestResolveNoteEphemeral_ExistingDBRowWins(t *testing.T) {
	r, sink, noteRepo, _ := ephResolver(t, ephActorDoc)
	uri := "https://remote.example/notes/1"
	noteRepo.Notes["existing"] = &model.Note{ID: "existing", UserID: "u1", URI: &uri}

	got, err := r.ResolveNoteEphemeral(uri)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "existing", got.ID)
	assert.Empty(t, sink.notes, "DB に在るなら ephemeral には入れない")
}

// sink 未配線なら従来どおり DB 経路にフォールバックする。
func TestResolveNoteEphemeral_NoSinkFallsBackToDB(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(userRepo, noteRepo, urls, &stubFetcher{body: []byte(ephActorDoc)}, idGen)

	uri := "https://remote.example/notes/1"
	noteRepo.Notes["existing"] = &model.Note{ID: "existing", UserID: "u1", URI: &uri}

	got, err := r.ResolveNoteEphemeral(uri)
	require.NoError(t, err)
	assert.Equal(t, "existing", got.ID)
}

// --- 検証が ephemeral モードでも働くこと ---
//
// ここが抜けると、リレー経由が連合の安全性検証を迂回する経路になる。
// 検証は通常経路と共通のコードを通しているが、モード追加で分岐が入って
// いないことを実際に確かめる。

func TestResolveNoteEphemeral_RejectsHostMismatch(t *testing.T) {
	// note.id の host と attributedTo の host が食い違う cross-host forge。
	doc := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/notes/1",
		"type": "Note",
		"attributedTo": "https://evil.example/users/mallory",
		"content": "forged",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	r, sink, noteRepo, _ := ephResolver(t, doc)

	_, err := r.ResolveNoteEphemeral("https://remote.example/notes/1")
	assert.Error(t, err, "host 不一致は ephemeral でも拒否されること")
	assert.Empty(t, sink.notes)
	assert.Empty(t, noteRepo.Notes)
}

func TestResolveNoteEphemeral_RejectsMissingContext(t *testing.T) {
	// fetch した standalone document は AS @context 必須 (#1828)。
	doc := `{
		"id": "https://remote.example/notes/1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "no context"
	}`
	r, sink, _, _ := ephResolver(t, doc)

	_, err := r.ResolveNoteEphemeral("https://remote.example/notes/1")
	assert.Error(t, err, "@context 欠落は ephemeral でも拒否されること")
	assert.Empty(t, sink.notes)
}

func TestResolveNoteEphemeral_RejectsFragmentURI(t *testing.T) {
	r, sink, _, _ := ephResolver(t, ephActorDoc)

	_, err := r.ResolveNoteEphemeral("https://remote.example/notes/1#frag")
	assert.Error(t, err, "fragment 付き URI は ephemeral でも拒否されること")
	assert.Empty(t, sink.notes)
}

func TestResolveNoteEphemeral_RejectsRequestHostMismatch(t *testing.T) {
	// 要求した URI の host と返ってきた id の host が違う (redirect 誘導)。
	doc := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://other.example/notes/1",
		"type": "Note",
		"attributedTo": "https://other.example/users/alice",
		"content": "redirected",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	r, sink, _, _ := ephResolver(t, doc)

	_, err := r.ResolveNoteEphemeral("https://remote.example/notes/1")
	assert.Error(t, err, "request host 不一致は ephemeral でも拒否されること")
	assert.Empty(t, sink.notes)
}

// 著者解決の順序: DB に居ればその実 ID を使う。
// これを飛ばすと、ミュート済みの著者が ephemeral 側の別 ID で並び続ける。
func TestResolveNoteAuthor_PrefersExistingDBUser(t *testing.T) {
	r, sink, noteRepo, userRepo := ephResolver(t, ephActorDoc)
	actorURI := "https://remote.example/users/alice"
	host := "remote.example"
	userRepo.Users["real"] = &model.User{ID: "real", Username: "alice", Host: &host, URI: &actorURI}

	noteURI := "https://remote.example/notes/1"
	noteRepo.Notes["n"] = &model.Note{ID: "n", UserID: "real", URI: &noteURI}

	got, err := r.ResolveNoteEphemeral(noteURI)
	require.NoError(t, err)
	assert.Equal(t, "real", got.UserID)
	assert.Empty(t, sink.notes)
}

// --- 重複解消 ---

// stubTimelineRemover records which IDs were pulled out of the FTT lists.
type stubTimelineRemover struct{ removed []string }

func (s *stubTimelineRemover) RemoveNoteID(noteID string, _ *model.User, _ string, _ *string) {
	s.removed = append(s.removed, noteID)
}

// 同じ投稿が先に relay 経由で ephemeral に入り、後から直接配送で DB に入った
// 場合、ephemeral 側を落とさないとタイムラインに 2 回出る。
func TestIngestNote_DropsSupersededEphemeral(t *testing.T) {
	noteURI := "https://remote.example/notes/1"
	actorURI := "https://remote.example/users/alice"
	doc := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "` + noteURI + `",
		"type": "Note",
		"attributedTo": "` + actorURI + `",
		"content": "relay first",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	r, sink, noteRepo, _ := ephResolverDocs(t, map[string]string{
		noteURI:  doc,
		actorURI: ephActorDoc,
	})
	remover := &stubTimelineRemover{}
	r.SetEphemeralTimelineRemover(remover)

	// 1. relay 経由で ephemeral に入る
	ephNote, err := r.ResolveNoteEphemeral(noteURI)
	require.NoError(t, err)
	require.Len(t, sink.notes, 1)
	assert.Empty(t, noteRepo.Notes)

	// 2. 同じ投稿が直接配送で届く
	dbNote, err := r.IngestNote([]byte(doc))
	require.NoError(t, err)
	require.NotNil(t, dbNote)

	assert.NotEmpty(t, noteRepo.Notes, "DB 行が作られること")
	assert.Empty(t, sink.notes, "ephemeral 側は落とされること")
	assert.Contains(t, remover.removed, ephNote.ID, "FTT からも旧 ID が除かれること")
}

// ephemeral に居ない URI では余計な削除をしない。
func TestIngestNote_NoEphemeralEntryIsNoop(t *testing.T) {
	noteURI := "https://remote.example/notes/2"
	actorURI := "https://remote.example/users/alice"
	doc := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "` + noteURI + `",
		"type": "Note",
		"attributedTo": "` + actorURI + `",
		"content": "direct only",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	r, sink, _, _ := ephResolverDocs(t, map[string]string{noteURI: doc, actorURI: ephActorDoc})
	remover := &stubTimelineRemover{}
	r.SetEphemeralTimelineRemover(remover)

	_, err := r.IngestNote([]byte(doc))
	require.NoError(t, err)
	assert.Empty(t, sink.dropped)
	assert.Empty(t, remover.removed)
}

// 機能が無効なら sink が配線されていても DB 経路に倒れること。
// これが効かないと「既定では挙動が変わらない」という約束が破れる。
func TestResolveNoteEphemeral_DisabledFallsBackToDB(t *testing.T) {
	noteURI := "https://remote.example/notes/1"
	actorURI := "https://remote.example/users/alice"
	doc := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "` + noteURI + `",
		"type": "Note",
		"attributedTo": "` + actorURI + `",
		"content": "disabled",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	r, sink, noteRepo, _ := ephResolverDocs(t, map[string]string{noteURI: doc, actorURI: ephActorDoc})
	sink.disabled = true

	note, err := r.ResolveNoteEphemeral(noteURI)
	require.NoError(t, err)
	require.NotNil(t, note)
	assert.NotEmpty(t, noteRepo.Notes, "無効時は従来どおり DB に入ること")
	assert.Empty(t, sink.notes, "無効時は ephemeral に入れないこと")
}

// --- MaterializeActor (ID 据え置き) ---

// materialize では ephemeral 時に採番した ID をそのまま使う。
// 新規採番にすると、Redis に残っている既存ノートが古い ID を指したままになり
// ミュートが効かなくなる。
func TestMaterializeActor_ReusesPreassignedID(t *testing.T) {
	actorURI := "https://remote.example/users/alice"
	r, _, _, userRepo := ephResolverDocs(t, map[string]string{actorURI: ephActorDoc})

	got, err := r.MaterializeActor(actorURI, "preassigned-id")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "preassigned-id", got.ID, "採番済み ID が使われること")
	assert.Contains(t, userRepo.Users, "preassigned-id")
}

// ID を渡さなければ通常どおり新規採番する。
func TestMaterializeActor_GeneratesIDWhenNotPreassigned(t *testing.T) {
	actorURI := "https://remote.example/users/alice"
	r, _, _, _ := ephResolverDocs(t, map[string]string{actorURI: ephActorDoc})

	got, err := r.MaterializeActor(actorURI, "")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.NotEmpty(t, got.ID)
	assert.NotEqual(t, "preassigned-id", got.ID)
}

// 既に DB に在る actor は ID を上書きせずそのまま返す。
func TestMaterializeActor_ExistingRowWins(t *testing.T) {
	actorURI := "https://remote.example/users/alice"
	r, _, _, userRepo := ephResolverDocs(t, map[string]string{actorURI: ephActorDoc})
	host := "remote.example"
	userRepo.Users["already"] = &model.User{ID: "already", Username: "alice", Host: &host, URI: &actorURI}

	got, err := r.MaterializeActor(actorURI, "different-id")
	require.NoError(t, err)
	assert.Equal(t, "already", got.ID, "既存行の ID を勝手に変えないこと")
}

func TestMaterializeActor_FetchFailure(t *testing.T) {
	r, _, _, _ := ephResolverDocs(t, map[string]string{})
	_, err := r.MaterializeActor("https://remote.example/users/ghost", "x")
	assert.Error(t, err)
}

// 著者解決の 2 番目の分岐: ephemeral store の URI 逆引きで ID を再利用する。
// これが無いと投稿ごとに別 ID を採番して同一人物が別人として並ぶ。
func TestResolveNoteAuthor_ReusesEphemeralUserID(t *testing.T) {
	actorURI := "https://remote.example/users/alice"
	noteURI := "https://remote.example/notes/2"
	doc := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "` + noteURI + `",
		"type": "Note",
		"attributedTo": "` + actorURI + `",
		"content": "second post",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	r, sink, _, _ := ephResolverDocs(t, map[string]string{noteURI: doc, actorURI: ephActorDoc})

	// 1 件目で採番された著者 ID を store に記録しておく。
	host := "remote.example"
	sink.authors["existing-author"] = &model.User{ID: "existing-author", Host: &host, URI: &actorURI}
	sink.byURI[actorURI] = "existing-author"

	note, err := r.ResolveNoteEphemeral(noteURI)
	require.NoError(t, err)
	assert.Equal(t, "existing-author", note.UserID, "2 件目も同じ著者 ID を使うこと")
}

// --- 著者の ephemeral 化 ---

// リレー由来の著者も DB に入れない。
// 実測でリレー購読後は note と user がほぼ 1:1 で増えるため、著者を止めないと
// 肥大化を抑える目的が半分しか達成できない。
func TestResolveNoteEphemeral_AuthorStaysOutOfDB(t *testing.T) {
	noteURI := "https://remote.example/notes/1"
	actorURI := "https://remote.example/users/alice"
	doc := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "` + noteURI + `",
		"type": "Note",
		"attributedTo": "` + actorURI + `",
		"content": "relay only",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	r, sink, noteRepo, userRepo := ephResolverDocs(t, map[string]string{noteURI: doc, actorURI: ephActorDoc})

	note, err := r.ResolveNoteEphemeral(noteURI)
	require.NoError(t, err)
	require.NotNil(t, note)

	assert.Empty(t, noteRepo.Notes, "note 行を作らない")
	assert.Empty(t, userRepo.Users, "user 行も作らない")
	require.Len(t, sink.notes, 1)
	require.Len(t, sink.authors, 1)
	// ノートと著者の ID が対応していること。
	assert.Equal(t, note.UserID, sink.authors[note.UserID].ID)
}

// 同じ著者の 2 件目は ephemeral store の逆引きで同じ ID を再利用する。
// これが崩れると同一人物が別人として並ぶ。
func TestResolveNoteEphemeral_SecondNoteReusesAuthorID(t *testing.T) {
	actorURI := "https://remote.example/users/alice"
	docs := map[string]string{actorURI: ephActorDoc}
	for _, id := range []string{"1", "2"} {
		docs["https://remote.example/notes/"+id] = `{
			"@context": "https://www.w3.org/ns/activitystreams",
			"id": "https://remote.example/notes/` + id + `",
			"type": "Note",
			"attributedTo": "` + actorURI + `",
			"content": "post ` + id + `",
			"to": ["https://www.w3.org/ns/activitystreams#Public"]
		}`
	}
	r, sink, _, userRepo := ephResolverDocs(t, docs)

	n1, err := r.ResolveNoteEphemeral("https://remote.example/notes/1")
	require.NoError(t, err)
	n2, err := r.ResolveNoteEphemeral("https://remote.example/notes/2")
	require.NoError(t, err)

	assert.Equal(t, n1.UserID, n2.UserID, "同じ著者は同じ ID を持つこと")
	assert.Len(t, sink.authors, 1, "著者が重複して作られないこと")
	assert.Empty(t, userRepo.Users)
}

// 既に DB に居る著者 (フォロー済み / ミュート済み等) は実 ID を使う。
// ここを飛ばすとミュートが効かなくなる。
func TestResolveNoteEphemeral_ExistingDBAuthorWins(t *testing.T) {
	noteURI := "https://remote.example/notes/1"
	actorURI := "https://remote.example/users/alice"
	doc := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "` + noteURI + `",
		"type": "Note",
		"attributedTo": "` + actorURI + `",
		"content": "from a followed user",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	r, sink, _, userRepo := ephResolverDocs(t, map[string]string{noteURI: doc, actorURI: ephActorDoc})
	host := "remote.example"
	userRepo.Users["known"] = &model.User{ID: "known", Username: "alice", Host: &host, URI: &actorURI}

	note, err := r.ResolveNoteEphemeral(noteURI)
	require.NoError(t, err)
	assert.Equal(t, "known", note.UserID, "DB に居る著者の実 ID を使うこと")
	assert.Len(t, sink.notes, 1, "ノート自体は ephemeral のまま")
}

// --- Create 転送経路 (Mastodon 系リレー) ---

// stubRelayChecker reports the given URIs as subscribed relay actors.
type stubRelayChecker struct{ relays map[string]bool }

func (s *stubRelayChecker) IsRelayActor(actor *model.User) bool {
	if actor == nil || actor.URI == nil {
		return false
	}
	return s.relays[*actor.URI]
}

func relayProcessor(t *testing.T, docs map[string]string, relayURI string) (
	*federation.Processor, *fakeSink, *testutil.MockNoteRepository, *testutil.MockUserRepository,
) {
	t.Helper()
	r, sink, noteRepo, userRepo := ephResolverDocs(t, docs)
	followingSvc := corefollowing.NewService(userRepo, testutil.NewMockFollowingRepository(),
		testutil.NewMockFollowRequestRepository(), mustIDGen(t))
	p := federation.NewProcessor(r, followingSvc, nil, nil, userRepo, noteRepo)
	p.SetRelayActorChecker(&stubRelayChecker{relays: map[string]bool{relayURI: true}})
	return p, sink, noteRepo, userRepo
}

func mustIDGen(t *testing.T) id.Generator {
	t.Helper()
	g, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return g
}

const relayCreateBody = `{
	"type": "Create",
	"actor": "https://remote.example/users/alice",
	"object": {
		"type": "Note",
		"id": "https://remote.example/notes/1",
		"attributedTo": "https://remote.example/users/alice",
		"content": "forwarded by a mastodon relay",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}
}`

// リレーが転送した Create では、ノートも著者も DB に入らないこと。
// 本番実測で残っていた DB user 増加の直接の原因。
func TestProcessWithSigner_RelayForwardedCreateStaysEphemeral(t *testing.T) {
	relayURI := "https://relay.example/actor"
	p, sink, noteRepo, userRepo := relayProcessor(t, map[string]string{
		"https://remote.example/users/alice": ephActorDoc,
	}, relayURI)

	host := "relay.example"
	signer := &model.User{ID: "relay1", Host: &host, URI: &relayURI}

	require.NoError(t, p.ProcessWithSigner([]byte(relayCreateBody), signer))

	assert.Empty(t, noteRepo.Notes, "note 行を作らない")
	assert.Empty(t, userRepo.Users, "著者の user 行も作らない")
	assert.Len(t, sink.notes, 1, "ephemeral store に入ること")
	assert.Len(t, sink.authors, 1)
}

// 直接配送の Create は従来どおり DB に入ること。
func TestProcessWithSigner_DirectCreateGoesToDB(t *testing.T) {
	p, sink, noteRepo, _ := relayProcessor(t, map[string]string{
		"https://remote.example/users/alice": ephActorDoc,
	}, "https://relay.example/actor")

	// 署名者が著者本人 = 直接配送。
	authorURI := "https://remote.example/users/alice"
	host := "remote.example"
	signer := &model.User{ID: "alice", Host: &host, URI: &authorURI}

	require.NoError(t, p.ProcessWithSigner([]byte(relayCreateBody), signer))

	assert.NotEmpty(t, noteRepo.Notes, "直接配送は DB に入ること")
	assert.Empty(t, sink.notes, "ephemeral には入れないこと")
}

// signer が取れない経路では従来どおり DB に入ること (fail-safe)。
func TestProcess_WithoutSignerGoesToDB(t *testing.T) {
	p, sink, noteRepo, _ := relayProcessor(t, map[string]string{
		"https://remote.example/users/alice": ephActorDoc,
	}, "https://relay.example/actor")

	require.NoError(t, p.Process([]byte(relayCreateBody)))

	assert.NotEmpty(t, noteRepo.Notes, "判定できないときは DB に倒すこと")
	assert.Empty(t, sink.notes)
}

// --- リレー由来の記録 (#2340) ---

// recordingRelayMarker captures which users were recorded as relay-derived.
type recordingRelayMarker struct {
	marked []string
	err    error
}

func (m *recordingRelayMarker) MarkObserved(userID string) error {
	if m.err != nil {
		return m.err
	}
	m.marked = append(m.marked, userID)
	return nil
}

// ResolveActorViaRelay は新規作成時に印を付ける。
// 孤児掃除の対象をリレー由来に限定するために要る。
func TestResolveActorViaRelay_MarksNewUser(t *testing.T) {
	actorURI := "https://remote.example/users/alice"
	r, _, _, userRepo := ephResolverDocs(t, map[string]string{actorURI: ephActorDoc})
	marker := &recordingRelayMarker{}
	r.SetRelayObservedMarker(marker)

	got, err := r.ResolveActorViaRelay(actorURI)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []string{got.ID}, marker.marked, "新規作成時に記録すること")
	assert.Contains(t, userRepo.Users, got.ID)
}

// 既に DB に在る行には印を付けない。
// リレー購読前から居る行やプロフィール閲覧で解決された行を巻き込まないため。
func TestResolveActorViaRelay_DoesNotMarkExisting(t *testing.T) {
	actorURI := "https://remote.example/users/alice"
	r, _, _, userRepo := ephResolverDocs(t, map[string]string{actorURI: ephActorDoc})
	host := "remote.example"
	userRepo.Users["known"] = &model.User{ID: "known", Username: "alice", Host: &host, URI: &actorURI}
	marker := &recordingRelayMarker{}
	r.SetRelayObservedMarker(marker)

	got, err := r.ResolveActorViaRelay(actorURI)
	require.NoError(t, err)
	assert.Equal(t, "known", got.ID)
	assert.Empty(t, marker.marked, "既存行には印を付けないこと")
}

// 記録の失敗は actor 解決を壊さない (ベストエフォート)。
func TestResolveActorViaRelay_MarkerFailureIsBestEffort(t *testing.T) {
	actorURI := "https://remote.example/users/alice"
	r, _, _, _ := ephResolverDocs(t, map[string]string{actorURI: ephActorDoc})
	r.SetRelayObservedMarker(&recordingRelayMarker{err: errors.New("db down")})

	got, err := r.ResolveActorViaRelay(actorURI)
	require.NoError(t, err)
	assert.NotNil(t, got)
}

func TestResolveActorViaRelay_FetchFailure(t *testing.T) {
	r, _, _, _ := ephResolverDocs(t, map[string]string{})
	r.SetRelayObservedMarker(&recordingRelayMarker{})
	_, err := r.ResolveActorViaRelay("https://remote.example/users/ghost")
	assert.Error(t, err)
}

// marker 未配線でも動く。
func TestResolveActorViaRelay_NoMarkerWired(t *testing.T) {
	actorURI := "https://remote.example/users/alice"
	r, _, _, _ := ephResolverDocs(t, map[string]string{actorURI: ephActorDoc})
	got, err := r.ResolveActorViaRelay(actorURI)
	require.NoError(t, err)
	assert.NotNil(t, got)
}

// --- 機能無効時の入口 (#2338) ---

// 無効なら ephemeral 用の入口も通常経路に倒れる。
func TestEphemeralEntryPoints_DisabledFallBackToDB(t *testing.T) {
	noteURI := "https://remote.example/notes/1"
	actorURI := "https://remote.example/users/alice"
	doc := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "` + noteURI + `",
		"type": "Note",
		"attributedTo": "` + actorURI + `",
		"content": "disabled",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	r, sink, noteRepo, userRepo := ephResolverDocs(t, map[string]string{noteURI: doc, actorURI: ephActorDoc})
	sink.disabled = true

	u, err := r.ResolveActorEphemeral(actorURI)
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.NotEmpty(t, userRepo.Users, "無効時は DB に入ること")

	n, _, err := r.IngestNoteEphemeral([]byte(doc), actorURI)
	require.NoError(t, err)
	require.NotNil(t, n)
	assert.NotEmpty(t, noteRepo.Notes, "無効時は DB に入ること")
	assert.Empty(t, sink.notes)
}

// sink 未配線でも同様。
func TestEphemeralEntryPoints_NoSink(t *testing.T) {
	actorURI := "https://remote.example/users/alice"
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(userRepo, noteRepo, urls, &docFetcher{docs: map[string]string{actorURI: ephActorDoc}}, idGen)

	u, err := r.ResolveActorEphemeral(actorURI)
	require.NoError(t, err)
	assert.NotNil(t, u)
}

// note.id の前後に空白がある document を取り込めること。WHATWG URL は先頭
// C0/空白を落とすので upstream は素通しするが、生値のまま host 検証すると
// scheme も host も取れず id host mismatch で落ちていた (#2662)。
func TestResolveNoteEphemeral_PaddedNoteIDPassesHostGate(t *testing.T) {
	doc := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": " https://remote.example/notes/1 ",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "padded id",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	r, sink, _, _ := ephResolverDocs(t, map[string]string{
		"https://remote.example/notes/1":     doc,
		"https://remote.example/users/alice": ephActorDoc,
	})

	note, err := r.ResolveNoteEphemeral("https://remote.example/notes/1")
	require.NoError(t, err, "空白付き id でも取り込めること")
	require.NotNil(t, note)
	require.NotNil(t, note.URI)
	assert.Equal(t, "https://remote.example/notes/1", *note.URI, "保存値も正規化済みであること")
	assert.Len(t, sink.notes, 1)
}

// #2751: note を作らずに終わる経路 (AP poll vote) が ResolveNote から
// `(nil, nil)` で出ていた。戻り値を nil check せず `note.ID` を読む呼び出し側が
// panic しうるので、ErrInvalidNote に倒す。
func TestResolveNote_PollVoteReturnsError(t *testing.T) {
	const pollURI = "https://remote.example/notes/poll"
	const voteURI = "https://remote.example/notes/vote"
	docs := map[string]string{
		"https://remote.example/users/alice": ephActorDoc,
		voteURI: `{
			"@context": "https://www.w3.org/ns/activitystreams",
			"id": "` + voteURI + `",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"inReplyTo": "` + pollURI + `",
			"name": "A",
			"to": ["https://www.w3.org/ns/activitystreams#Public"]
		}`,
	}
	r, _, noteRepo, userRepo := ephResolverDocs(t, docs)

	// 投票先の poll note を local に用意する。
	userRepo.Users["author"] = &model.User{ID: "author", Username: "author"}
	uri := pollURI
	require.NoError(t, noteRepo.Create(&model.Note{
		ID: "pollnote", UserID: "author", URI: &uri,
		Visibility: model.NoteVisibilityPublic, HasPoll: true,
	}))
	pollRepo := testutil.NewMockPollRepository()
	require.NoError(t, pollRepo.Create(&model.Poll{
		NoteID: "pollnote", Choices: []string{"A", "B"}, Votes: []int64{0, 0},
	}))
	r.SetPollRepo(pollRepo)
	r.SetPollVoter(&stubPollVoter{})

	note, created, err := r.ResolveNoteWithCreated(voteURI)
	assert.Nil(t, note)
	assert.False(t, created)
	assert.Error(t, err, "note を作らない経路は (nil, nil) ではなく error にする")
}

// **ephemeral 経路も DB 障害では止まる** (#3121)。
//
// ここは Redis だけで完結できるので倒すこともできるが、倒すと「DB が落ちて
// いるあいだだけ、ミュート済み / materialize 済みの著者を引けず別 ID の
// ephemeral 行としてタイムラインに戻る」— DB を先に引く設計 (#2332) が崩れる。
func TestIngestNoteEphemeral_DBFailureStopsIngest(t *testing.T) {
	noteURI := "https://remote.example/notes/relay1"
	actorURI := "https://remote.example/users/alice"
	doc := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "` + noteURI + `",
		"type": "Note",
		"attributedTo": "` + actorURI + `",
		"content": "relayed",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	boom := errors.New("connection refused")

	t.Run("dedup の lookup", func(t *testing.T) {
		r, sink, noteRepo, _ := ephResolverDocs(t, map[string]string{noteURI: doc, actorURI: ephActorDoc})
		noteRepo.FindByURIErr = boom
		_, _, err := r.IngestNoteEphemeral([]byte(doc), actorURI)
		require.ErrorIs(t, err, boom)
		assert.Empty(t, sink.notes, "確かめられないまま Redis へ置いている")
	})

	// **sink に既知の著者が居る状態で測る。** 居ないと fetch 側の guard
	// (`resolveActorOnceWithID`) が代わりに止めてしまい、この分岐を外しても
	// 緑のままになる (変異検証で実測)。
	t.Run("著者の lookup", func(t *testing.T) {
		r, sink, _, userRepo := ephResolverDocs(t, map[string]string{noteURI: doc, actorURI: ephActorDoc})
		host := "remote.example"
		sink.byURI[actorURI] = "eph-alice"
		sink.authors["eph-alice"] = &model.User{ID: "eph-alice", Username: "alice", Host: &host, URI: &actorURI}
		userRepo.FindByURIErr = boom

		_, _, err := r.IngestNoteEphemeral([]byte(doc), actorURI)
		require.ErrorIs(t, err, federation.ErrLookupUnavailable)
		assert.Empty(t, sink.notes, "DB を引けないまま ephemeral 側の著者で確定させている")
	})
}
