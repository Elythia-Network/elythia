package repository

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanupPromo(t *testing.T, noteID string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "promo_read" WHERE "noteId" = ?`, noteID)
	testDB.Exec(`DELETE FROM "promo_note" WHERE "noteId" = ?`, noteID)
}

func TestPromoNoteRepository_Create(t *testing.T) {
	repo := NewPromoNoteRepository(testDB)
	u := insertTestUser(t, "promo_u1", "pu1")
	defer cleanupUser(t, u.ID)
	n := insertTestNote(t, "promo_n1", u.ID)
	defer cleanupNote(t, n.ID)

	promo := &model.PromoNote{NoteID: n.ID, UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)}
	require.NoError(t, repo.Create(promo))
	defer cleanupPromo(t, n.ID)

	// idempotent (ON CONFLICT DoNothing)
	require.NoError(t, repo.Create(promo))
}

func TestPromoNoteRepository_ListActive(t *testing.T) {
	repo := NewPromoNoteRepository(testDB)
	u := insertTestUser(t, "promo_u2", "pu2")
	defer cleanupUser(t, u.ID)
	n1 := insertTestNote(t, "promo_a1", u.ID)
	defer cleanupNote(t, n1.ID)
	n2 := insertTestNote(t, "promo_a2", u.ID)
	defer cleanupNote(t, n2.ID)

	now := time.Now()
	require.NoError(t, repo.Create(&model.PromoNote{NoteID: n1.ID, UserID: u.ID, ExpiresAt: now.Add(time.Hour)}))
	defer cleanupPromo(t, n1.ID)
	require.NoError(t, repo.Create(&model.PromoNote{NoteID: n2.ID, UserID: u.ID, ExpiresAt: now.Add(-time.Hour)}))
	defer cleanupPromo(t, n2.ID)

	active, err := repo.ListActive(now)
	require.NoError(t, err)

	ids := map[string]bool{}
	for _, p := range active {
		ids[p.NoteID] = true
	}
	assert.True(t, ids[n1.ID], "active promo should appear")
	assert.False(t, ids[n2.ID], "expired promo should not appear")
}

func TestPromoReadRepository_MarkAndCheck(t *testing.T) {
	readRepo := NewPromoReadRepository(testDB)
	noteRepo := NewPromoNoteRepository(testDB)
	u := insertTestUser(t, "promo_r1", "pr1")
	defer cleanupUser(t, u.ID)
	n := insertTestNote(t, "promo_rn", u.ID)
	defer cleanupNote(t, n.ID)

	require.NoError(t, noteRepo.Create(&model.PromoNote{NoteID: n.ID, UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)}))
	defer cleanupPromo(t, n.ID)

	assert.False(t, readRepo.IsRead(u.ID, n.ID))

	require.NoError(t, readRepo.MarkRead(&model.PromoRead{ID: "pr1", UserID: u.ID, NoteID: n.ID}))
	assert.True(t, readRepo.IsRead(u.ID, n.ID))

	// idempotent
	require.NoError(t, readRepo.MarkRead(&model.PromoRead{ID: "pr2", UserID: u.ID, NoteID: n.ID}))
}

func TestPromoNoteRepository_Exists(t *testing.T) {
	repo := NewPromoNoteRepository(testDB)
	u := insertTestUser(t, "promo_e", "pe")
	defer cleanupUser(t, u.ID)
	n := insertTestNote(t, "promo_en", u.ID)
	defer cleanupNote(t, n.ID)

	exists, err := repo.Exists(n.ID)
	require.NoError(t, err)
	assert.False(t, exists)

	require.NoError(t, repo.Create(&model.PromoNote{NoteID: n.ID, UserID: u.ID, ExpiresAt: time.Now().Add(time.Hour)}))
	defer cleanupPromo(t, n.ID)

	exists, err = repo.Exists(n.ID)
	require.NoError(t, err)
	assert.True(t, exists)
}
