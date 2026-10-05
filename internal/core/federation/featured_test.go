package federation_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingDocFetcher serves a document per URI and records every request, so a
// test can assert which collections were (and were not) fetched.
type countingDocFetcher struct {
	docs  map[string]string
	calls []string
}

func (d *countingDocFetcher) FetchObject(uri string) ([]byte, error) {
	d.calls = append(d.calls, uri)
	if body, ok := d.docs[uri]; ok {
		return []byte(body), nil
	}
	return nil, errors.New("no fixture for " + uri)
}

func (d *countingDocFetcher) fetched(uri string) bool {
	for _, c := range d.calls {
		if c == uri {
			return true
		}
	}
	return false
}

// featuredActor builds an actor document advertising a featured collection.
func featuredActor(host, name, featured string) string {
	base := fmt.Sprintf("https://%s/users/%s", host, name)
	featuredLine := ""
	if featured != "" {
		featuredLine = fmt.Sprintf("\t\"featured\": %q,\n", featured)
	}
	return fmt.Sprintf(`{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": %q,
	"type": "Person",
	"preferredUsername": %q,
	"inbox": %q,
%s	"publicKey": {
		"id": %q,
		"owner": %q,
		"publicKeyPem": "-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----"
	}
}`, base, name, base+"/inbox", featuredLine, base+"#main-key", base)
}

// featuredNote builds a Note document attributed to the given actor.
func featuredNote(noteURI, authorURI, body string) string {
	return fmt.Sprintf(`{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": %q,
	"type": "Note",
	"attributedTo": %q,
	"content": "<p>%s</p>",
	"to": ["https://www.w3.org/ns/activitystreams#Public"]
}`, noteURI, authorURI, body)
}

// featuredCollection builds a featured collection listing the given URIs.
func featuredCollection(collectionURI, apType string, itemURIs ...string) string {
	items := make([]string, 0, len(itemURIs))
	for _, u := range itemURIs {
		items = append(items, fmt.Sprintf("%q", u))
	}
	field := "orderedItems"
	if strings.EqualFold(apType, "Collection") {
		field = "items"
	}
	return fmt.Sprintf(`{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": %q,
	"type": %q,
	"%s": [%s]
}`, collectionURI, apType, field, strings.Join(items, ", "))
}

type featuredEnv struct {
	resolver *federation.Resolver
	users    *testutil.MockUserRepository
	notes    *testutil.MockNoteRepository
	pins     *testutil.MockUserNotePiningRepository
	fetcher  *countingDocFetcher
}

func newFeaturedEnv(t *testing.T, docs map[string]string) *featuredEnv {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	pinRepo := testutil.NewMockUserNotePiningRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	f := &countingDocFetcher{docs: docs}
	r := federation.NewResolver(userRepo, noteRepo, urls, f, idGen)
	r.SetPinningRepo(pinRepo, idGen)
	return &featuredEnv{resolver: r, users: userRepo, notes: noteRepo, pins: pinRepo, fetcher: f}
}

// pinnedNoteURIs returns the note URIs pinned for userID, in display order
// (user_note_pining は id の降順で読まれる)。
func (e *featuredEnv) pinnedNoteURIs(t *testing.T, userID string) []string {
	t.Helper()
	rows, err := e.pins.ListByUser(userID)
	require.NoError(t, err)
	uris := make([]string, 0, len(rows))
	for _, row := range rows {
		note, err := e.notes.FindByID(row.NoteID)
		require.NoError(t, err)
		require.NotNil(t, note.URI)
		uris = append(uris, *note.URI)
	}
	return uris
}

// 観測を始めた時点で既にピン留めされていた投稿を取り込むこと (#2552)。
// inbound の Add はピン留めされた瞬間にしか飛んで来ないので、これが無いと
// 観測より前のピン留めは永久に拾えない。
func TestUpdateFeatured_ImportsPinsOnFirstResolve(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/alice"
		featured = "https://remote.example/users/alice/collections/featured"
		note1    = "https://remote.example/notes/1"
		note2    = "https://remote.example/notes/2"
	)
	env := newFeaturedEnv(t, map[string]string{
		actorURI: featuredActor("remote.example", "alice", featured),
		featured: featuredCollection(featured, "OrderedCollection", note1, note2),
		note1:    featuredNote(note1, actorURI, "first"),
		note2:    featuredNote(note2, actorURI, "second"),
	})

	user, err := env.resolver.ResolveActor(actorURI)
	require.NoError(t, err)

	// コレクションの並びがそのまま表示順になること。
	assert.Equal(t, []string{note1, note2}, env.pinnedNoteURIs(t, user.ID))
}

// `items` を使う Collection も受けること (upstream は type に応じて片方だけ読む)。
func TestUpdateFeatured_AcceptsPlainCollection(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/bob"
		featured = "https://remote.example/users/bob/collections/featured"
		note1    = "https://remote.example/notes/b1"
	)
	env := newFeaturedEnv(t, map[string]string{
		actorURI: featuredActor("remote.example", "bob", featured),
		featured: featuredCollection(featured, "Collection", note1),
		note1:    featuredNote(note1, actorURI, "hi"),
	})

	user, err := env.resolver.ResolveActor(actorURI)
	require.NoError(t, err)
	assert.Equal(t, []string{note1}, env.pinnedNoteURIs(t, user.ID))
}

// 既に観測済みのユーザーは actor の更新時に埋まること (#2552 の遡り取り込み)。
func TestUpdateFeatured_BackfillsOnActorRefresh(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/carol"
		featured = "https://remote.example/users/carol/collections/featured"
		note1    = "https://remote.example/notes/c1"
	)
	env := newFeaturedEnv(t, map[string]string{
		actorURI: featuredActor("remote.example", "carol", featured),
		featured: featuredCollection(featured, "OrderedCollection", note1),
		note1:    featuredNote(note1, actorURI, "pinned"),
	})

	// featured を取り込む前の行を模す (この経路が無かった頃に作られた行)。
	host := "remote.example"
	uri := actorURI
	existing := &model.User{
		ID: "9existinguser0000000", Username: "carol", Host: &host, URI: &uri,
	}
	require.NoError(t, env.users.Create(existing))
	require.Empty(t, env.pinnedNoteURIs(t, existing.ID))

	// LastFetchedAt が nil なので shouldRefreshActor が true になり、refreshActor
	// 経由で featured が引かれる。
	user, err := env.resolver.ResolveActor(actorURI)
	require.NoError(t, err)
	require.Equal(t, existing.ID, user.ID)

	assert.Equal(t, []string{note1}, env.pinnedNoteURIs(t, existing.ID))
}

// featured の URL は actor 文書内の申告値なので別ホストを指せる。他人のサーバーの
// コレクションを自分のピン留めとして取り込ませないこと。
func TestUpdateFeatured_RejectsCrossHostCollection(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/mallory"
		featured = "https://victim.example/users/someone/collections/featured"
	)
	env := newFeaturedEnv(t, map[string]string{
		actorURI: featuredActor("remote.example", "mallory", featured),
	})

	user, err := env.resolver.ResolveActor(actorURI)
	require.NoError(t, err)

	assert.Empty(t, env.pinnedNoteURIs(t, user.ID))
	assert.False(t, env.fetcher.fetched(featured),
		"別ホストの featured は取得すらしないこと")
}

// コレクションの中身に他ホストの URI を混ぜてこられる。相手のホストに限ること。
func TestUpdateFeatured_SkipsCrossHostItems(t *testing.T) {
	const (
		actorURI  = "https://remote.example/users/dave"
		featured  = "https://remote.example/users/dave/collections/featured"
		foreign   = "https://victim.example/notes/secret"
		ownNote   = "https://remote.example/notes/d1"
		actorHost = "remote.example"
	)
	env := newFeaturedEnv(t, map[string]string{
		actorURI: featuredActor(actorHost, "dave", featured),
		featured: featuredCollection(featured, "OrderedCollection", foreign, ownNote),
		ownNote:  featuredNote(ownNote, actorURI, "mine"),
	})

	user, err := env.resolver.ResolveActor(actorURI)
	require.NoError(t, err)

	assert.Equal(t, []string{ownNote}, env.pinnedNoteURIs(t, user.ID))
	assert.False(t, env.fetcher.fetched(foreign), "他ホストの item は取得しないこと")
}

// ピン留めできるのは自分の投稿だけ。他人の投稿を自分のプロフィールに並べられない
// こと (upstream updateFeatured は著者を見ないので、そのぶん厳しい)。
func TestUpdateFeatured_SkipsNotesByOtherAuthors(t *testing.T) {
	const (
		actorURI   = "https://remote.example/users/erin"
		otherURI   = "https://remote.example/users/frank"
		featured   = "https://remote.example/users/erin/collections/featured"
		othersNote = "https://remote.example/notes/f1"
		ownNote    = "https://remote.example/notes/e1"
	)
	env := newFeaturedEnv(t, map[string]string{
		actorURI:   featuredActor("remote.example", "erin", featured),
		otherURI:   featuredActor("remote.example", "frank", ""),
		featured:   featuredCollection(featured, "OrderedCollection", othersNote, ownNote),
		othersNote: featuredNote(othersNote, otherURI, "not mine"),
		ownNote:    featuredNote(ownNote, actorURI, "mine"),
	})

	user, err := env.resolver.ResolveActor(actorURI)
	require.NoError(t, err)
	assert.Equal(t, []string{ownNote}, env.pinnedNoteURIs(t, user.ID))
}

// inline で type が分かるものは、取得する前に落とすこと。
func TestUpdateFeatured_SkipsNonNoteItems(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/grace"
		featured = "https://remote.example/users/grace/collections/featured"
		hashtag  = "https://remote.example/tags/misskey"
		ownNote  = "https://remote.example/notes/g1"
	)
	collection := fmt.Sprintf(`{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": %q,
	"type": "OrderedCollection",
	"orderedItems": [
		{"id": %q, "type": "Hashtag", "name": "#misskey"},
		{"id": %q, "type": "Note"}
	]
}`, featured, hashtag, ownNote)

	env := newFeaturedEnv(t, map[string]string{
		actorURI: featuredActor("remote.example", "grace", featured),
		featured: collection,
		ownNote:  featuredNote(ownNote, actorURI, "mine"),
	})

	user, err := env.resolver.ResolveActor(actorURI)
	require.NoError(t, err)

	assert.Equal(t, []string{ownNote}, env.pinnedNoteURIs(t, user.ID))
	assert.False(t, env.fetcher.fetched(hashtag), "Note でない item は取得しないこと")
}

// 上限は upstream の `.slice(0, 5)` と同値。
func TestUpdateFeatured_StopsAtLimit(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/heidi"
		featured = "https://remote.example/users/heidi/collections/featured"
	)
	docs := map[string]string{
		actorURI: featuredActor("remote.example", "heidi", featured),
	}
	uris := make([]string, 0, 8)
	for i := range 8 {
		u := fmt.Sprintf("https://remote.example/notes/h%d", i)
		uris = append(uris, u)
		docs[u] = featuredNote(u, actorURI, "n")
	}
	docs[featured] = featuredCollection(featured, "OrderedCollection", uris...)

	env := newFeaturedEnv(t, docs)
	user, err := env.resolver.ResolveActor(actorURI)
	require.NoError(t, err)

	assert.Equal(t, uris[:5], env.pinnedNoteURIs(t, user.ID))
	assert.False(t, env.fetcher.fetched(uris[5]), "上限を超えた item は取得しないこと")
}

// 取得に失敗したときは既存のピン留めを触らないこと。**空のコレクションと区別
// せずに置き換えると、相手が一時的に落ちているだけでピン留めが消える。**
func TestUpdateFeatured_KeepsExistingPinsWhenFetchFails(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/ivan"
		featured = "https://remote.example/users/ivan/collections/featured"
	)
	// featured の fixture を置かない = 取得失敗。
	env := newFeaturedEnv(t, map[string]string{
		actorURI: featuredActor("remote.example", "ivan", featured),
	})

	host := "remote.example"
	uri := actorURI
	existing := &model.User{ID: "9ivanuser00000000000", Username: "ivan", Host: &host, URI: &uri}
	require.NoError(t, env.users.Create(existing))
	require.NoError(t, env.pins.Create(&model.UserNotePining{
		ID: "9pin00000000000000000", UserID: existing.ID, NoteID: "9note0000000000000000",
	}))

	_, err := env.resolver.ResolveActor(actorURI)
	require.NoError(t, err)

	count, err := env.pins.CountByUser(existing.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "取得に失敗しただけで既存のピン留めを消さないこと")
}

// リモート側で外されたピンが残らないこと (差分更新ではなく全置換)。
func TestUpdateFeatured_ReplacesStalePins(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/judy"
		featured = "https://remote.example/users/judy/collections/featured"
		note1    = "https://remote.example/notes/j1"
	)
	env := newFeaturedEnv(t, map[string]string{
		actorURI: featuredActor("remote.example", "judy", featured),
		featured: featuredCollection(featured, "OrderedCollection", note1),
		note1:    featuredNote(note1, actorURI, "still pinned"),
	})

	host := "remote.example"
	uri := actorURI
	existing := &model.User{ID: "9judyuser00000000000", Username: "judy", Host: &host, URI: &uri}
	require.NoError(t, env.users.Create(existing))
	require.NoError(t, env.pins.Create(&model.UserNotePining{
		ID: "9stalepin000000000000", UserID: existing.ID, NoteID: "9stalenote00000000000",
	}))

	_, err := env.resolver.ResolveActor(actorURI)
	require.NoError(t, err)

	assert.Equal(t, []string{note1}, env.pinnedNoteURIs(t, existing.ID))
}

// ピン留めの保存に失敗しても actor の取得そのものは成立すること (best-effort)。
func TestUpdateFeatured_StoreFailureDoesNotBreakActorResolve(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/ken"
		featured = "https://remote.example/users/ken/collections/featured"
		note1    = "https://remote.example/notes/k1"
	)
	env := newFeaturedEnv(t, map[string]string{
		actorURI: featuredActor("remote.example", "ken", featured),
		featured: featuredCollection(featured, "OrderedCollection", note1),
		note1:    featuredNote(note1, actorURI, "pinned"),
	})
	env.pins.ReplaceErr = errors.New("boom")

	user, err := env.resolver.ResolveActor(actorURI)
	require.NoError(t, err)
	assert.Equal(t, "ken", user.Username)
}

// ピン留めの取り込みが入れ子にならないこと (#2552)。
//
// ピン留め → 引用先 → その著者 → その featured … と入れ子になると、1 段ごとに
// 5 分岐する取得の連鎖になる。ノート解決の内側で作られた actor では featured を
// 引かない。
func TestUpdateFeatured_DoesNotRecurseThroughQuotedAuthors(t *testing.T) {
	const (
		aliceURI     = "https://remote.example/users/alice"
		aliceFeat    = "https://remote.example/users/alice/collections/featured"
		alicePinned  = "https://remote.example/notes/a1"
		bobURI       = "https://other.example/users/bob"
		bobFeat      = "https://other.example/users/bob/collections/featured"
		bobQuoted    = "https://other.example/notes/b1"
		bobOwnPinned = "https://other.example/notes/b2"
	)
	// alice のピン留めが bob の投稿を引用している。bob は未観測。
	alicePinnedDoc := fmt.Sprintf(`{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": %q,
	"type": "Note",
	"attributedTo": %q,
	"content": "<p>quoting</p>",
	"_misskey_quote": %q,
	"to": ["https://www.w3.org/ns/activitystreams#Public"]
}`, alicePinned, aliceURI, bobQuoted)

	env := newFeaturedEnv(t, map[string]string{
		aliceURI:     featuredActor("remote.example", "alice", aliceFeat),
		aliceFeat:    featuredCollection(aliceFeat, "OrderedCollection", alicePinned),
		alicePinned:  alicePinnedDoc,
		bobURI:       featuredActor("other.example", "bob", bobFeat),
		bobFeat:      featuredCollection(bobFeat, "OrderedCollection", bobOwnPinned),
		bobQuoted:    featuredNote(bobQuoted, bobURI, "quoted"),
		bobOwnPinned: featuredNote(bobOwnPinned, bobURI, "bob pin"),
	})

	user, err := env.resolver.ResolveActor(aliceURI)
	require.NoError(t, err)

	assert.Equal(t, []string{alicePinned}, env.pinnedNoteURIs(t, user.ID))
	assert.False(t, env.fetcher.fetched(bobFeat),
		"ノート解決の内側で作られた actor の featured は引かないこと")
}

// featured を宣言していない actor では何もしないこと。
func TestUpdateFeatured_NoFeaturedDeclared(t *testing.T) {
	const actorURI = "https://remote.example/users/leo"
	env := newFeaturedEnv(t, map[string]string{
		actorURI: featuredActor("remote.example", "leo", ""),
	})

	user, err := env.resolver.ResolveActor(actorURI)
	require.NoError(t, err)
	assert.Empty(t, env.pinnedNoteURIs(t, user.ID))
}

// items / orderedItems が単一 object でも取り込む。さらに**片方がスカラーでも
// もう片方を巻き添えにしない**。この経路は decode の error を見て
// `nil, false` を返すので、`[]json.RawMessage` 決め打ちだと collection ごと
// 落ちて featured の取り込みが丸ごと空振りする (#2662)。
func TestUpdateFeatured_AcceptsSingleItemAndSurvivesScalarSibling(t *testing.T) {
	for _, tc := range []struct {
		name       string
		collection string
	}{
		{"single object orderedItems", `{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": "https://remote.example/users/pat/collections/featured",
	"type": "OrderedCollection",
	"orderedItems": {"id": "https://remote.example/notes/pat-1"}
}`},
		{"scalar items does not clobber orderedItems", `{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": "https://remote.example/users/pat/collections/featured",
	"type": "OrderedCollection",
	"items": "nonsense",
	"orderedItems": ["https://remote.example/notes/pat-1"]
}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const (
				actorURI = "https://remote.example/users/pat"
				featured = "https://remote.example/users/pat/collections/featured"
				noteURI  = "https://remote.example/notes/pat-1"
			)
			env := newFeaturedEnv(t, map[string]string{
				actorURI: featuredActor("remote.example", "pat", featured),
				featured: tc.collection,
				noteURI:  featuredNote(noteURI, actorURI, "pinned"),
			})
			user, err := env.resolver.ResolveActor(actorURI)
			require.NoError(t, err)
			assert.Equal(t, []string{noteURI}, env.pinnedNoteURIs(t, user.ID))
		})
	}
}

// collection と **item** の type が混在配列でも先頭要素で判定する (他の type
// 判定 5 実装と同じ head 方式)。`[]string` への一括 unmarshal だと
// `["Person", 42]` が空になり、**Note でない item をピン留めに混ぜてしまう**
// (#2662)。
func TestUpdateFeatured_MixedArrayCollectionType(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/quill"
		featured = "https://remote.example/users/quill/collections/featured"
		noteURI  = "https://remote.example/notes/quill-1"
	)
	collection := `{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": "` + featured + `",
	"type": ["OrderedCollection", 42],
	"orderedItems": ["` + noteURI + `"]
}`
	env := newFeaturedEnv(t, map[string]string{
		actorURI: featuredActor("remote.example", "quill", featured),
		featured: collection,
		noteURI:  featuredNote(noteURI, actorURI, "pinned"),
	})
	user, err := env.resolver.ResolveActor(actorURI)
	require.NoError(t, err)
	assert.Equal(t, []string{noteURI}, env.pinnedNoteURIs(t, user.ID))

	// item 側の type が混在配列でも先頭で判定する。`[]string` 一括 decode だと
	// 空になり、Note でない item が skip されずにピン留め候補に混ざる。
	const (
		actorURI2 = "https://remote.example/users/rex"
		featured2 = "https://remote.example/users/rex/collections/featured"
	)
	collection2 := `{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": "` + featured2 + `",
	"type": "OrderedCollection",
	"orderedItems": [{"id": "https://remote.example/notes/rex-1", "type": ["Person", 42]}]
}`
	env2 := newFeaturedEnv(t, map[string]string{
		actorURI2: featuredActor("remote.example", "rex", featured2),
		featured2: collection2,
		// URI 自体は解決できる Note にしておく。解決できないと「type を見て
		// skip した」のか「解決に失敗した」のか区別できない。
		"https://remote.example/notes/rex-1": featuredNote("https://remote.example/notes/rex-1", actorURI2, "not a pin"),
	})
	user2, err := env2.resolver.ResolveActor(actorURI2)
	require.NoError(t, err)
	assert.Empty(t, env2.pinnedNoteURIs(t, user2.ID), "Note でない item は採らない")
}

// collection の type が配列でもピン留めを取り込めること。この経路は
// activitypub.Normalize を通らない生 fetch なので、string 決め打ちだと
// **featured の取り込みが丸ごと落ちる** (#2662)。
func TestUpdateFeatured_AcceptsArrayCollectionType(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/opal"
		featured = "https://remote.example/users/opal/collections/featured"
		noteURI  = "https://remote.example/notes/opal-1"
	)
	collection := `{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": "` + featured + `",
	"type": ["OrderedCollection"],
	"orderedItems": ["` + noteURI + `"]
}`
	env := newFeaturedEnv(t, map[string]string{
		actorURI: featuredActor("remote.example", "opal", featured),
		featured: collection,
		noteURI:  featuredNote(noteURI, actorURI, "pinned"),
	})

	user, err := env.resolver.ResolveActor(actorURI)
	require.NoError(t, err)
	assert.Equal(t, []string{noteURI}, env.pinnedNoteURIs(t, user.ID))
}

// Collection でも OrderedCollection でもない文書は受け付けないこと。
func TestUpdateFeatured_RejectsNonCollection(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/mia"
		featured = "https://remote.example/users/mia/collections/featured"
	)
	env := newFeaturedEnv(t, map[string]string{
		actorURI: featuredActor("remote.example", "mia", featured),
		featured: featuredNote(featured, actorURI, "not a collection"),
	})

	user, err := env.resolver.ResolveActor(actorURI)
	require.NoError(t, err)
	assert.Empty(t, env.pinnedNoteURIs(t, user.ID))
}

// ピン留めのリポジトリが未配線なら取り込み自体を行わないこと。
func TestUpdateFeatured_NotWiredIsNoop(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/nina"
		featured = "https://remote.example/users/nina/collections/featured"
	)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	f := &countingDocFetcher{docs: map[string]string{
		actorURI: featuredActor("remote.example", "nina", featured),
	}}
	r := federation.NewResolver(userRepo, noteRepo, urls, f, idGen)

	_, err = r.ResolveActor(actorURI)
	require.NoError(t, err)
	assert.False(t, f.fetched(featured), "未配線なら featured を取得しないこと")
}

// 上の再帰テストが空振りしていないことの確認。引用先の解決が実際に bob を
// 作っていなければ、featured を引かないのは当たり前になってしまう。
func TestUpdateFeatured_RecursionGuardIsNotVacuous(t *testing.T) {
	const (
		aliceURI    = "https://remote.example/users/alice"
		aliceFeat   = "https://remote.example/users/alice/collections/featured"
		alicePinned = "https://remote.example/notes/a1"
		bobURI      = "https://other.example/users/bob"
		bobFeat     = "https://other.example/users/bob/collections/featured"
		bobQuoted   = "https://other.example/notes/b1"
	)
	alicePinnedDoc := fmt.Sprintf(`{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": %q,
	"type": "Note",
	"attributedTo": %q,
	"content": "<p>quoting</p>",
	"_misskey_quote": %q,
	"to": ["https://www.w3.org/ns/activitystreams#Public"]
}`, alicePinned, aliceURI, bobQuoted)

	env := newFeaturedEnv(t, map[string]string{
		aliceURI:    featuredActor("remote.example", "alice", aliceFeat),
		aliceFeat:   featuredCollection(aliceFeat, "OrderedCollection", alicePinned),
		alicePinned: alicePinnedDoc,
		bobURI:      featuredActor("other.example", "bob", bobFeat),
		bobFeat:     featuredCollection(bobFeat, "OrderedCollection"),
		bobQuoted:   featuredNote(bobQuoted, bobURI, "quoted"),
	})

	_, err := env.resolver.ResolveActor(aliceURI)
	require.NoError(t, err)

	// bob 自身は引用の解決で作られている (= 再帰の入口には到達している)。
	assert.True(t, env.fetcher.fetched(bobURI), "引用先の著者は解決されること")
	bob, err := env.users.FindByURI(bobURI)
	require.NoError(t, err, "引用先の著者が作られていること")
	require.NotNil(t, bob)
	// それでも bob の featured は引かない。
	assert.False(t, env.fetcher.fetched(bobFeat))
}

// 同じ URI が複数回並んでいても 1 件にすること。
func TestUpdateFeatured_DeduplicatesItems(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/olga"
		featured = "https://remote.example/users/olga/collections/featured"
		note1    = "https://remote.example/notes/o1"
	)
	env := newFeaturedEnv(t, map[string]string{
		actorURI: featuredActor("remote.example", "olga", featured),
		featured: featuredCollection(featured, "OrderedCollection", note1, note1),
		note1:    featuredNote(note1, actorURI, "once"),
	})

	user, err := env.resolver.ResolveActor(actorURI)
	require.NoError(t, err)
	assert.Equal(t, []string{note1}, env.pinnedNoteURIs(t, user.ID))
}

// 走査する件数に上限を置くこと。**上限が無いと、巨大なコレクションを置くだけで
// 取得を増幅させられる。**
func TestUpdateFeatured_BoundsScannedItems(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/pete"
		featured = "https://remote.example/users/pete/collections/featured"
		buried   = "https://remote.example/notes/buried"
	)
	// 先頭に解決できない item を大量に並べ、その後ろに Note を置く。
	items := make([]string, 0, 80)
	for i := range 80 {
		items = append(items, fmt.Sprintf("https://remote.example/missing/%d", i))
	}
	items = append(items, buried)

	env := newFeaturedEnv(t, map[string]string{
		actorURI: featuredActor("remote.example", "pete", featured),
		featured: featuredCollection(featured, "OrderedCollection", items...),
		buried:   featuredNote(buried, actorURI, "too deep"),
	})

	user, err := env.resolver.ResolveActor(actorURI)
	require.NoError(t, err)

	assert.Empty(t, env.pinnedNoteURIs(t, user.ID))
	assert.False(t, env.fetcher.fetched(buried), "上限より後ろは見ないこと")
	assert.Less(t, len(env.fetcher.calls), 60, "取得が item 数に比例しないこと")
}

// featured がそもそも JSON オブジェクトでないときは既存を触らないこと。
func TestUpdateFeatured_RejectsNonObjectDocument(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/quinn"
		featured = "https://remote.example/users/quinn/collections/featured"
	)
	env := newFeaturedEnv(t, map[string]string{
		actorURI: featuredActor("remote.example", "quinn", featured),
		featured: `["not", "an", "object"]`,
	})

	user, err := env.resolver.ResolveActor(actorURI)
	require.NoError(t, err)
	assert.Empty(t, env.pinnedNoteURIs(t, user.ID))
}

// 著者が「いま取り込んでいる投稿」をピン留めしていると、note の singleflight が
// **自分が握っている in-flight entry を自分で待つ**状態になり、永久に止まる
// (#2684)。本番の inbox キューで job が active に居座り続ける原因。
//
// 経路は ResolveNote(A) → ingestNoteWithCreated → resolveNoteAuthor →
// resolveActor → refreshActor → updateFeatured → resolveFeaturedNotes →
// resolveNoteDepth(A)。外側と内側で noteGroupKey が一致する。
//
// **タイムアウト付きで待つ。** 素直に呼ぶと go test のパッケージ timeout
// (10 分) まで待たされ、失敗の理由も分からなくなる。
func TestResolveNote_AuthorPinsTheNoteBeingIngested(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/dave"
		featured = "https://remote.example/users/dave/collections/featured"
		pinned   = "https://remote.example/notes/d1"
	)
	env := newFeaturedEnv(t, map[string]string{
		actorURI: featuredActor("remote.example", "dave", featured),
		featured: featuredCollection(featured, "OrderedCollection", pinned),
		pinned:   featuredNote(pinned, actorURI, "pinned by its own author"),
	})

	type result struct {
		note *model.Note
		err  error
	}
	done := make(chan result, 1)
	go func() {
		n, err := env.resolver.ResolveNote(pinned)
		done <- result{n, err}
	}()

	var note *model.Note
	select {
	case got := <-done:
		require.NoError(t, got.err)
		require.NotNil(t, got.note)
		require.NotNil(t, got.note.URI)
		assert.Equal(t, pinned, *got.note.URI)
		note = got.note
	case <-time.After(15 * time.Second):
		t.Fatal("ResolveNote が返らない: featured 経路が自分の in-flight entry を待っている (#2684)")
	}

	// **このピンはこの回では取りこぼす。** 取り込み中の note を再解決しに行けない
	// 以上、外側の ingest が終わってから追加する手立てが無い。既知の欠落として
	// 固定しておく (黙って直すと下の回復側の意味が消える)。
	assert.Empty(t, env.pinnedNoteURIs(t, note.UserID),
		"取り込み中だった自分のピンはこの回では入らない")

	// 次の actor 更新で拾い直されること。ここが通らないとピンが永久に欠ける。
	require.NoError(t, env.users.UpdateUser(note.UserID, map[string]any{"lastFetchedAt": (*time.Time)(nil)}))
	user, err := env.resolver.ResolveActor(actorURI)
	require.NoError(t, err)
	require.Equal(t, note.UserID, user.ID)
	assert.Equal(t, []string{pinned}, env.pinnedNoteURIs(t, user.ID),
		"次回の actor 更新でピンが入ること")
}

// 取得 URI と document の id が食い違う形 (別名 URL) でも止まらないこと
// (#2684 review HIGH-1)。
//
// resolveNoteOnce は取得 URI と id に **host の一致しか要求しない**ので、
// `/@user/xxx` と `/notes/xxx`、末尾スラッシュの有無などで両者はずれる。
// in-flight の判定を document id 側 (ingesting) で行うとここを取りこぼし、
// 正規形と同じデッドロックが残る。
func TestResolveNote_AuthorPinsTheNoteUnderAnAliasURI(t *testing.T) {
	cases := []struct {
		name     string
		user     string
		fetchURI string
		docID    string
	}{
		{"alias path", "erin", "https://remote.example/@erin/e1", "https://remote.example/notes/e1"},
		{"trailing slash", "frank", "https://remote.example/notes/f1/", "https://remote.example/notes/f1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actorURI := "https://remote.example/users/" + tc.user
			featured := actorURI + "/collections/featured"
			env := newFeaturedEnv(t, map[string]string{
				actorURI:    featuredActor("remote.example", tc.user, featured),
				featured:    featuredCollection(featured, "OrderedCollection", tc.fetchURI),
				tc.fetchURI: featuredNote(tc.docID, actorURI, "pinned under an alias"),
			})

			done := make(chan error, 1)
			go func() {
				_, err := env.resolver.ResolveNote(tc.fetchURI)
				done <- err
			}()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(15 * time.Second):
				t.Fatal("ResolveNote が返らない: 別名 URI で in-flight 判定を取りこぼしている (#2684)")
			}
		})
	}
}

// quoteNote builds a Note document that quotes another URI.
func quoteNote(noteID, authorURI, quoted string) string {
	return fmt.Sprintf(`{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": %q,
	"type": "Note",
	"attributedTo": %q,
	"content": "<p>quoting</p>",
	"_misskey_quote": %q,
	"to": ["https://www.w3.org/ns/activitystreams#Public"]
}`, noteID, authorURI, quoted)
}

// 引用が自分自身を指す形でも止まらないこと (#2684 review)。
//
// quote 側の cycle guard は ingesting (document id) しか見ていなかったので、
// 取得 URI と id が食い違う別名 URL では素通りし、featured と同じ
// 自己デッドロックになる。resolveQuoteURI 側の singleflight 鍵判定を
// 固定する (この分岐はレビュー時点でカバレッジ 0% だった)。
func TestResolveNote_SelfQuoteUnderAnAliasURI(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/gina"
		aliasURI = "https://remote.example/@gina/g1"
		canonURI = "https://remote.example/notes/g1"
	)
	env := newFeaturedEnv(t, map[string]string{
		actorURI: featuredActor("remote.example", "gina", ""),
		aliasURI: quoteNote(canonURI, actorURI, aliasURI),
	})

	done := make(chan error, 1)
	go func() {
		_, err := env.resolver.ResolveNote(aliasURI)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("ResolveNote が返らない: quote 側が別名 URI で in-flight 判定を取りこぼしている (#2684)")
	}
}

// 1 件が解決できなくても残りは取り込むこと。upstream は Promise.all の
// reject で全件を捨てる (all-or-nothing) が、mk-go は取り込める分を取り込む
// (docs/divergence.md)。ピン留めされた自分の投稿があるとその 1 件が skip
// されるので、この差が実際に効く。
func TestUpdateFeatured_ImportsTheRestWhenOneItemIsSkipped(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/hana"
		featured = "https://remote.example/users/hana/collections/featured"
		selfPin  = "https://remote.example/notes/h1"
		other    = "https://remote.example/notes/h2"
	)
	env := newFeaturedEnv(t, map[string]string{
		actorURI: featuredActor("remote.example", "hana", featured),
		featured: featuredCollection(featured, "OrderedCollection", selfPin, other),
		selfPin:  featuredNote(selfPin, actorURI, "the note being ingested"),
		other:    featuredNote(other, actorURI, "another pin"),
	})

	done := make(chan error, 1)
	go func() {
		_, err := env.resolver.ResolveNote(selfPin)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("ResolveNote が返らない (#2684)")
	}

	user, err := env.users.FindByURI(actorURI)
	require.NoError(t, err)
	// 取り込み中だった selfPin は落ちるが、other は入る。
	assert.Equal(t, []string{other}, env.pinnedNoteURIs(t, user.ID),
		"1 件 skip しても残りは取り込むこと")
}
