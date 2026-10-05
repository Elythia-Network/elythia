package federation_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/activitypub"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

func openResyncDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)
	return db
}

// insertRemoteUser stores a remote user row and removes it after the test.
func insertRemoteUser(t *testing.T, db *gorm.DB, userID, username, host, uri string, lastFetchedAt *time.Time) {
	t.Helper()
	u := &model.User{
		ID: userID, Username: username, UsernameLower: username, Host: &host, URI: &uri,
		LastFetchedAt: lastFetchedAt, AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, db.Create(u).Error)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "user_profile" WHERE "userId" = ?`, userID)
		db.Exec(`DELETE FROM "user_publickey" WHERE "userId" = ?`, userID)
		db.Exec(`DELETE FROM "user" WHERE "id" = ?`, userID)
	})
}

// A stale remote user whose acct now points at a new actor URI on the same
// host is re-synced against the real DB: the uri column is corrected, the
// profile re-fetched from the new URI and lastFetchedAt advanced, even when
// the user cache remembered the new URI as missing.
func TestResyncIfStale_DB_FixesURIAndRefreshes(t *testing.T) {
	db := openResyncDB(t)
	const (
		oldURI = "https://resync.example/users/rs_alice"
		newURI = "https://resync.example/ap/actors/rs_alice"
	)
	stale := time.Now().Add(-48 * time.Hour)
	insertRemoteUser(t, db, "rsAlice0000000000001", "rs_alice", "resync.example", oldURI, &stale)

	userRepo := repository.NewCachedUserRepository(repository.NewUserRepository(db))
	// 新しい URI を「無い」と cache に覚えさせておく。
	_, err := userRepo.FindByURI(newURI)
	require.Error(t, err)

	doc := actorDoc(newURI, "rs_alice")
	f := newRouteFetcher()
	f.bodies[newURI] = doc[:len(doc)-1] + `,"name":"Resynced Alice"}`
	idGen, _ := id.NewGenerator("aidx")
	resolver := corefederation.NewResolver(userRepo, repository.NewNoteRepository(db), activitypub.NewURLBuilder("https://local.example"), f, idGen)
	wf := &countingWebFinger{uri: newURI}
	r := corefederation.NewRemoteUserResolver(wf, resolver, userRepo, "local.example")

	u, err := userRepo.FindByID("rsAlice0000000000001")
	require.NoError(t, err)
	before := time.Now()
	got, err := r.ResyncIfStale(u)
	require.NoError(t, err)
	assert.Equal(t, "rsAlice0000000000001", got.ID)

	var row model.User
	require.NoError(t, db.Where(`"id" = ?`, "rsAlice0000000000001").First(&row).Error)
	require.NotNil(t, row.URI)
	assert.Equal(t, newURI, *row.URI)
	require.NotNil(t, row.Name)
	assert.Equal(t, "Resynced Alice", *row.Name)
	require.NotNil(t, row.LastFetchedAt)
	assert.False(t, row.LastFetchedAt.Before(before.Add(-time.Second)))
	assert.Equal(t, 1, f.count(newURI))
	assert.Zero(t, f.count(oldURI))
}

// An inbound note mentioning an actor that is not in the DB stores that actor
// and the mention against the real DB.
func TestIngestNote_DB_FetchesUnknownMentionedActor(t *testing.T) {
	db := openResyncDB(t)
	recent := time.Now()
	insertRemoteUser(t, db, "rsAuthor000000000001", "alice", "remote.example", mentionAuthorURI, &recent)
	const carol = "https://other-db.example/users/rs_carol"
	t.Cleanup(func() {
		var ids []string
		db.Raw(`SELECT "id" FROM "user" WHERE "uri" = ?`, carol).Scan(&ids)
		for _, id := range ids {
			db.Exec(`DELETE FROM "user_profile" WHERE "userId" = ?`, id)
			db.Exec(`DELETE FROM "user_publickey" WHERE "userId" = ?`, id)
			db.Exec(`DELETE FROM "user" WHERE "id" = ?`, id)
		}
	})

	userRepo := repository.NewUserRepository(db)
	noteRepo := repository.NewNoteRepository(db)
	f := newRouteFetcher()
	f.bodies[mentionAuthorURI] = actorDoc(mentionAuthorURI, "alice")
	f.bodies[carol] = actorDoc(carol, "rs_carol")
	idGen, _ := id.NewGenerator("aidx")
	r := corefederation.NewResolver(userRepo, noteRepo, activitypub.NewURLBuilder("https://local.example"), f, idGen)

	note, err := r.IngestNote(noteBody(t, "rsdb1", []string{carol}, []string{activitypub.Public}, nil))
	require.NoError(t, err)
	t.Cleanup(func() { db.Exec(`DELETE FROM "note" WHERE "id" = ?`, note.ID) })

	carolRow, err := userRepo.FindByURI(carol)
	require.NoError(t, err)
	var stored model.Note
	require.NoError(t, db.Where(`"id" = ?`, note.ID).First(&stored).Error)
	assert.Contains(t, []string(stored.Mentions), carolRow.ID)
}
