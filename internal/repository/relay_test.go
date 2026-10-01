package repository

import (
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelayRepository_CRUD(t *testing.T) {
	repo := NewRelayRepository(testDB)
	t.Cleanup(func() {
		testDB.Exec(`DELETE FROM "relay" WHERE id IN (?, ?)`, "rel_1", "rel_2")
	})

	require.NoError(t, repo.Create(&model.Relay{ID: "rel_1", Inbox: "https://remote.example/inbox-a", Status: "requesting"}))
	require.NoError(t, repo.Create(&model.Relay{ID: "rel_2", Inbox: "https://remote.example/inbox-b", Status: "accepted"}))

	found, err := repo.FindByID("rel_1")
	require.NoError(t, err)
	assert.Equal(t, "https://remote.example/inbox-a", found.Inbox)

	all, err := repo.List()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(all), 2)

	accepted, err := repo.ListByStatus("accepted")
	require.NoError(t, err)
	var hasRel2 bool
	for _, r := range accepted {
		if r.ID == "rel_2" {
			hasRel2 = true
		}
	}
	assert.True(t, hasRel2)

	changed, err := repo.UpdateStatusFrom("rel_1", "requesting", "accepted")
	require.NoError(t, err)
	assert.True(t, changed)
	found, err = repo.FindByID("rel_1")
	require.NoError(t, err)
	assert.Equal(t, "accepted", found.Status)

	// requesting でなくなった行は動かない (本家 updateRequestingRelayStatus)。
	changed, err = repo.UpdateStatusFrom("rel_1", "requesting", "rejected")
	require.NoError(t, err)
	assert.False(t, changed)
	found, err = repo.FindByID("rel_1")
	require.NoError(t, err)
	assert.Equal(t, "accepted", found.Status)

	require.NoError(t, repo.Delete("rel_1"))
	_, err = repo.FindByID("rel_1")
	assert.Error(t, err)
}

func TestRelayRepository_FindByID_NotFound(t *testing.T) {
	repo := NewRelayRepository(testDB)
	_, err := repo.FindByID("rel_nonexistent")
	assert.Error(t, err)
}
