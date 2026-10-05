package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestSigninRepository_CreateAndList(t *testing.T) {
	repo := NewSigninRepository(testDB)
	seedUser(t, "si_u1")
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "signin" WHERE "userId" = ?`, "si_u1") })

	for _, id := range []string{"si_1", "si_2", "si_3"} {
		require.NoError(t, repo.Create(&model.Signin{
			ID:      id,
			UserID:  "si_u1",
			IP:      "127.0.0.1",
			Headers: datatypes.JSON([]byte(`{}`)),
			Success: true,
		}))
	}

	// 全件
	all, err := repo.ListByUserID("si_u1", 10, "", "")
	require.NoError(t, err)
	assert.Len(t, all, 3)

	// untilID で絞り込み
	older, err := repo.ListByUserID("si_u1", 10, "si_3", "")
	require.NoError(t, err)
	assert.Len(t, older, 2)

	// sinceID で絞り込み
	newer, err := repo.ListByUserID("si_u1", 10, "", "si_1")
	require.NoError(t, err)
	assert.Len(t, newer, 2)
}
