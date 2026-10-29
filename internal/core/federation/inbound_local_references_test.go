package federation

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type inboundReferenceObserver struct {
	observed  int
	onObserve func()
}

func (o *inboundReferenceObserver) IsExternallyAvailable(user *model.User) bool {
	return user != nil && user.IsLocal() && user.FederationStatus == model.FederationStatusOn && !user.IsSuspended && !user.IsDeleted
}

func (o *inboundReferenceObserver) ObserveExternalReference(userID, reason, host string) (bool, error) {
	o.observed++
	if o.onObserve != nil {
		o.onObserve()
	}
	return true, nil
}

type countingInboundNoteRepository struct {
	*testutil.MockNoteRepository
	metadataReads int
}

func (r *countingInboundNoteRepository) FindManyLocalReferenceMetadataByIDs(ids []string) ([]*model.Note, error) {
	r.metadataReads++
	return r.MockNoteRepository.FindManyLocalReferenceMetadataByIDs(ids)
}

type countingInboundUserRepository struct {
	*testutil.MockUserRepository
	batchReads int
}

type failingInboundUserRepository struct {
	*testutil.MockUserRepository
	batchErr error
}

func (r *failingInboundUserRepository) FindManyByIDs(ids []string) ([]*model.User, error) {
	if r.batchErr != nil {
		return nil, r.batchErr
	}
	return r.MockUserRepository.FindManyByIDs(ids)
}

type inboundMapFetcher map[string][]byte

func (f inboundMapFetcher) FetchObject(uri string) ([]byte, error) {
	if body, ok := f[uri]; ok {
		return body, nil
	}
	return nil, errors.New("not found")
}

func inboundReferenceActor() []byte {
	return []byte(`{"@context":"https://www.w3.org/ns/activitystreams","id":"https://remote.example/users/alice","type":"Person","preferredUsername":"alice","inbox":"https://remote.example/users/alice/inbox","publicKey":{"id":"https://remote.example/users/alice#main-key","owner":"https://remote.example/users/alice","publicKeyPem":"PEM"}}`)
}

type failingInboundNoteRepository struct {
	*testutil.MockNoteRepository
	failID string
	err    error
}

func (r *failingInboundNoteRepository) FindByID(id string) (*model.Note, error) {
	if id == r.failID {
		return nil, r.err
	}
	return r.MockNoteRepository.FindByID(id)
}

func (r *countingInboundUserRepository) FindManyByIDs(ids []string) ([]*model.User, error) {
	r.batchReads++
	return r.MockUserRepository.FindManyByIDs(ids)
}

func TestObserveLocalInboxBodyReferencesCachesConfirmedNoteAndAuthor(t *testing.T) {
	users := testutil.NewMockUserRepository()
	users.Users["author"] = &model.User{ID: "author", FederationStatus: model.FederationStatusOn}
	notes := &countingInboundNoteRepository{MockNoteRepository: testutil.NewMockNoteRepository()}
	notes.Notes["note"] = &model.Note{ID: "note", UserID: "author"}
	observer := &inboundReferenceObserver{}
	r := &Resolver{userRepo: users, noteRepo: notes, localFederationObserver: observer, clock: time.Now}

	require.NoError(t, r.ObserveLocalInboxBodyReferences(nil, []string{"note"}, "remote.example"))
	require.Equal(t, 1, observer.observed)
	firstReads := notes.metadataReads
	require.GreaterOrEqual(t, firstReads, 2)

	require.NoError(t, r.ObserveLocalInboxBodyReferences(nil, []string{"note"}, "remote.example"))
	require.Equal(t, firstReads, notes.metadataReads)
}

func TestObserveLocalInboxBodyReferencesBatchesColdReferencesAndReusesWarmCache(t *testing.T) {
	users := &countingInboundUserRepository{MockUserRepository: testutil.NewMockUserRepository()}
	notes := &countingInboundNoteRepository{MockNoteRepository: testutil.NewMockNoteRepository()}
	observer := &inboundReferenceObserver{}
	var noteIDs []string
	for i := 0; i < 64; i++ {
		userID := fmt.Sprintf("author-%02d", i)
		noteID := fmt.Sprintf("note-%02d", i)
		users.Users[userID] = &model.User{ID: userID, FederationStatus: model.FederationStatusOn}
		notes.Notes[noteID] = &model.Note{ID: noteID, UserID: userID}
		noteIDs = append(noteIDs, noteID)
	}
	r := &Resolver{userRepo: users, noteRepo: notes, localFederationObserver: observer, clock: time.Now}

	require.NoError(t, r.ObserveLocalInboxBodyReferences(nil, noteIDs, "remote.example"))
	assert.Equal(t, 1, users.batchReads)
	assert.LessOrEqual(t, notes.metadataReads, 3)
	firstUserReads, firstNoteReads := users.batchReads, notes.metadataReads

	require.NoError(t, r.ObserveLocalInboxBodyReferences(nil, noteIDs, "remote.example"))
	assert.Equal(t, firstUserReads, users.batchReads)
	assert.Equal(t, firstNoteReads, notes.metadataReads)
}

func TestObserveLocalInboxBodyReferencesRechecksLocalOnlyAfterObservation(t *testing.T) {
	users := testutil.NewMockUserRepository()
	users.Users["author"] = &model.User{ID: "author", FederationStatus: model.FederationStatusOn}
	notes := &countingInboundNoteRepository{MockNoteRepository: testutil.NewMockNoteRepository()}
	note := &model.Note{ID: "note", UserID: "author"}
	notes.Notes[note.ID] = note
	observer := &inboundReferenceObserver{onObserve: func() { note.LocalOnly = true }}
	r := &Resolver{userRepo: users, noteRepo: notes, localFederationObserver: observer, clock: time.Now}

	require.ErrorIs(t, r.ObserveLocalInboxBodyReferences(nil, []string{"note"}, "remote.example"), ErrInboundLocalReferenceUnavailable)
	require.Equal(t, 1, observer.observed)
	_, cached := r.localReferenceCache.noteAuthor(note.ID, time.Now())
	require.False(t, cached)
}

func TestObserveLocalInboxBodyReferencesDoesNotCacheMissingUsers(t *testing.T) {
	users := testutil.NewMockUserRepository()
	observer := &inboundReferenceObserver{}
	r := &Resolver{userRepo: users, localFederationObserver: observer, clock: time.Now}

	require.NoError(t, r.ObserveLocalInboxBodyReferences([]string{"author"}, nil, "remote.example"))
	users.Users["author"] = &model.User{ID: "author", FederationStatus: model.FederationStatusOn}
	require.NoError(t, r.ObserveLocalInboxBodyReferences([]string{"author"}, nil, "remote.example"))
	assert.Equal(t, 1, observer.observed)
}

func TestResolveNoteForInboxScopesLocalReferencePolicyToWorker(t *testing.T) {
	for _, status := range []model.FederationStatus{model.FederationStatusOff, model.FederationStatusPending, model.FederationStatusOn} {
		t.Run(string(status), func(t *testing.T) {
			users := testutil.NewMockUserRepository()
			users.Users["author"] = &model.User{ID: "author", FederationStatus: status}
			notes := testutil.NewMockNoteRepository()
			notes.Notes["note"] = &model.Note{ID: "note", UserID: "author"}
			observer := &inboundReferenceObserver{}
			urls := activitypub.NewURLBuilder("https://example.com")
			idGen, err := id.NewGenerator("aidx")
			require.NoError(t, err)
			r := NewResolverNilDeps(users, notes, urls, nil, idGen)
			r.SetLocalFederationObserver(observer)

			// The same Resolver is used by native API reads; those do not record or
			// reject a local note based on inbox-only policy.
			got, err := r.ResolveNote("https://example.com/notes/note")
			require.NoError(t, err)
			require.Equal(t, "note", got.ID)
			assert.Empty(t, observer.observed)

			got, err = r.ResolveNoteForInbox("https://example.com/notes/note", "remote.example")
			if status == model.FederationStatusOn {
				require.NoError(t, err)
				require.Equal(t, "note", got.ID)
				assert.Equal(t, 1, observer.observed)
			} else {
				require.ErrorIs(t, err, ErrInboundLocalReferenceUnavailable)
				assert.Nil(t, got)
				assert.Zero(t, observer.observed)
			}
		})
	}
}

func TestIngestNoteForInboxRejectsUnavailableLocalQuoteAndReplyBeforeSaving(t *testing.T) {
	for _, field := range []string{"quoteUrl", "inReplyTo"} {
		t.Run(field, func(t *testing.T) {
			users := testutil.NewMockUserRepository()
			remoteHost, remoteURI := "remote.example", "https://remote.example/users/alice"
			now := time.Now()
			users.Users["alice"] = &model.User{ID: "alice", Username: "alice", UsernameLower: "alice", Host: &remoteHost, URI: &remoteURI, LastFetchedAt: &now}
			users.Users["bob"] = &model.User{ID: "bob", FederationStatus: model.FederationStatusOff}
			notes := testutil.NewMockNoteRepository()
			notes.Notes["target"] = &model.Note{ID: "target", UserID: "bob"}
			observer := &inboundReferenceObserver{}
			emojis := testutil.NewMockEmojiRepository()
			files := testutil.NewMockDriveFileRepository()
			idGen, err := id.NewGenerator("aidx")
			require.NoError(t, err)
			r := NewResolverNilDeps(users, notes, activitypub.NewURLBuilder("https://example.com"), inboundMapFetcher{remoteURI: inboundReferenceActor()}, idGen)
			r.SetLocalFederationObserver(observer)
			r.SetEmojiRepo(emojis)
			r.SetDriveFileRepo(files)

			extra := ""
			if field == "quoteUrl" {
				extra = `,"tag":[{"type":"Emoji","name":":star:","icon":{"type":"Image","url":"https://remote.example/star.png"}}],"attachment":[{"type":"Document","mediaType":"image/png","url":"https://remote.example/image.png"}]`
			}
			body := []byte(fmt.Sprintf(`{"@context":"https://www.w3.org/ns/activitystreams","type":"Note","id":"https://remote.example/notes/inbound-%s","attributedTo":"%s","to":["%s"],"content":"hello"%s,"%s":"https://example.com/notes/target"}`,
				field, remoteURI, activitypub.Public, extra, field))
			_, _, err = r.IngestNoteWithCreatedForInbox(body, remoteURI, remoteHost)
			require.ErrorIs(t, err, ErrInboundLocalReferenceUnavailable)
			assert.Len(t, notes.Notes, 1, "the rejected Note must not be saved")
			assert.Zero(t, observer.observed)
			assert.Empty(t, emojis.Emojis, "rejected quote must not upsert Emoji rows")
			assert.Empty(t, files.Files, "rejected quote must not ingest attachments")
		})
	}
}

func TestIngestNoteForInboxPropagatesNestedLocalReferenceLookupFailures(t *testing.T) {
	for _, reference := range []string{"mention", "reply"} {
		t.Run(reference, func(t *testing.T) {
			lookupErr := errors.New("injected lookup failure")
			users := &failingInboundUserRepository{MockUserRepository: testutil.NewMockUserRepository(), batchErr: lookupErr}
			remoteHost, remoteURI := "remote.example", "https://remote.example/users/alice"
			now := time.Now()
			users.Users["alice"] = &model.User{ID: "alice", Username: "alice", UsernameLower: "alice", Host: &remoteHost, URI: &remoteURI, LastFetchedAt: &now}
			users.Users["bob"] = &model.User{ID: "bob", FederationStatus: model.FederationStatusOn}
			notes := &failingInboundNoteRepository{MockNoteRepository: testutil.NewMockNoteRepository(), failID: "target", err: lookupErr}
			fetcher := inboundMapFetcher{remoteURI: inboundReferenceActor()}
			quoteURI := "https://remote.example/notes/quoted"
			quote := `{"@context":"https://www.w3.org/ns/activitystreams","type":"Note","id":"` + quoteURI + `","attributedTo":"` + remoteURI + `","to":["` + activitypub.Public + `"],"content":"quoted"`
			if reference == "mention" {
				quote += `,"tag":[{"type":"Mention","href":"https://example.com/x/bob"}]}`
			} else {
				quote += `,"inReplyTo":"https://example.com/notes/target"}`
			}
			fetcher[quoteURI] = []byte(quote)
			idGen, err := id.NewGenerator("aidx")
			require.NoError(t, err)
			r := NewResolverNilDeps(users, notes, activitypub.NewURLBuilder("https://example.com"), fetcher, idGen)
			r.SetLocalFederationObserver(&inboundReferenceObserver{})
			outer := []byte(`{"@context":"https://www.w3.org/ns/activitystreams","type":"Note","id":"https://remote.example/notes/outer","attributedTo":"` + remoteURI + `","to":["` + activitypub.Public + `"],"content":"outer","quoteUrl":"` + quoteURI + `"}`)

			_, _, err = r.IngestNoteWithCreatedForInbox(outer, remoteURI, remoteHost)
			require.ErrorIs(t, err, ErrInboundLocalReferenceFailure)
			require.ErrorIs(t, err, lookupErr)
			assert.Empty(t, notes.Notes, "neither the quoted Note nor its outer Note may be saved")
		})
	}
}

func TestLocalReferenceCacheExpiresAndBoundsPositiveEntries(t *testing.T) {
	cache := &localReferenceCache{}
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	cache.rememberUser("oldest", now)
	for i := 0; i < localReferenceCacheCap; i++ {
		cache.rememberUser(fmt.Sprintf("user-%d", i), now.Add(time.Duration(i+1)*time.Nanosecond))
	}
	assert.False(t, cache.hasUser("oldest", now.Add(time.Second)), "the oldest entry must be evicted at capacity")
	assert.True(t, cache.hasUser("user-4095", now.Add(time.Second)))
	assert.False(t, cache.hasUser("user-4095", now.Add(localReferenceCacheTTL+time.Second)), "positive entries must expire")
	assert.False(t, cache.hasUser("missing", now), "negative lookups are not cached")
}
