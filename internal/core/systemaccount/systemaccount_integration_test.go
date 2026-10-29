package systemaccount_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/systemaccount"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/require"
)

func TestFetch_PersistsExternallyReferencedFederationState(t *testing.T) {
	db, err := testutil.OpenTestDB()
	if err != nil {
		t.Skipf("PostgreSQL test DB unavailable: %v", err)
	}
	testutil.ApplyMigrations(db)
	const kind = "fedstate"
	const username = kind + ".actor"
	defer func() {
		db.Exec(`DELETE FROM "system_account" WHERE type = ?`, kind)
		db.Exec(`DELETE FROM "user_keypair_extra" WHERE "userId" IN (SELECT id FROM "user" WHERE "usernameLower" = ?)`, username)
		db.Exec(`DELETE FROM "user_keypair" WHERE "userId" IN (SELECT id FROM "user" WHERE "usernameLower" = ?)`, username)
		db.Exec(`DELETE FROM "user_profile" WHERE "userId" IN (SELECT id FROM "user" WHERE "usernameLower" = ?)`, username)
		db.Exec(`DELETE FROM "user" WHERE "usernameLower" = ?`, username)
	}()

	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	users := repository.NewUserRepository(db)
	svc := systemaccount.NewService(users, repository.NewUserKeypairRepository(db), repository.NewSystemAccountRepository(db), idGen)

	created, err := svc.Fetch(kind)
	require.NoError(t, err)
	persisted, err := users.FindByID(created.ID)
	require.NoError(t, err)
	require.Equal(t, model.FederationStatusOn, persisted.FederationStatus)
	require.NotNil(t, persisted.FederationEnabledAt)
	require.NotNil(t, persisted.FirstExternalReferenceAt)
	require.NotNil(t, persisted.FirstExternalReferenceReason)
	require.Equal(t, "system-account", *persisted.FirstExternalReferenceReason)
}
