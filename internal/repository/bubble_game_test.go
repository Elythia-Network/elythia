package repository

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanupBubbleGame(t *testing.T, id string) {
	t.Helper()
	testDB.Exec(`DELETE FROM "bubble_game_record" WHERE id = ?`, id)
}

func TestBubbleGameRepository_CreateAndRanking(t *testing.T) {
	repo := NewBubbleGameRepository(testDB)
	u := insertTestUser(t, "bg_u1", "bgu1")
	defer cleanupUser(t, u.ID)

	r1 := &model.BubbleGameRecord{
		ID: "bg1", UserID: u.ID, SeededAt: time.Now(), Seed: "s1",
		GameVersion: 1, GameMode: "normal", Score: 100,
	}
	r2 := &model.BubbleGameRecord{
		ID: "bg2", UserID: u.ID, SeededAt: time.Now(), Seed: "s2",
		GameVersion: 1, GameMode: "normal", Score: 200,
	}
	require.NoError(t, repo.Create(r1))
	defer cleanupBubbleGame(t, r1.ID)
	require.NoError(t, repo.Create(r2))
	defer cleanupBubbleGame(t, r2.ID)

	ranking, err := repo.Ranking("normal", 10)
	require.NoError(t, err)
	require.True(t, len(ranking) >= 2)
	assert.True(t, ranking[0].Score >= ranking[1].Score)
}

func TestBubbleGameRepository_Ranking_DefaultLimit(t *testing.T) {
	repo := NewBubbleGameRepository(testDB)
	ranking, err := repo.Ranking("nonexistent", 0)
	require.NoError(t, err)
	assert.Empty(t, ranking)
}
