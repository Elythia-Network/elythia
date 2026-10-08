package repository

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

func readAccountCreatedAt(t *testing.T, userID string) *time.Time {
	t.Helper()
	var row model.User
	require.NoError(t, testDB.Where(`"id" = ?`, userID).First(&row).Error)
	return row.AccountCreatedAt
}

// 列が空のときだけ書き、既に入っている値 (actor の published など) は上書き
// しない (#3465)。
func TestSetAccountCreatedAtIfNull(t *testing.T) {
	seedUser(t, "acIfNull01")
	repos := map[string]UserRepository{
		"plain":  NewUserRepository(testDB),
		"cached": NewCachedUserRepository(NewUserRepository(testDB)),
	}
	first := time.Date(2019, 1, 2, 3, 4, 5, 0, time.UTC)
	second := time.Date(2021, 6, 7, 0, 0, 0, 0, time.UTC)
	for name, repo := range repos {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, testDB.Exec(`UPDATE "user" SET "accountCreatedAt" = NULL WHERE "id" = ?`, "acIfNull01").Error)

			set, err := repo.SetAccountCreatedAtIfNull("acIfNull01", first)
			require.NoError(t, err)
			assert.True(t, set, "NULL なら書く")
			got := readAccountCreatedAt(t, "acIfNull01")
			require.NotNil(t, got)
			assert.True(t, first.Equal(*got))

			set, err = repo.SetAccountCreatedAtIfNull("acIfNull01", second)
			require.NoError(t, err)
			assert.False(t, set, "既に入っていれば書かない")
			got = readAccountCreatedAt(t, "acIfNull01")
			require.NotNil(t, got)
			assert.True(t, first.Equal(*got), "既存の値を上書きしない")

			set, err = repo.SetAccountCreatedAtIfNull("acIfNullX9", first)
			require.NoError(t, err)
			assert.False(t, set, "無い行は書いたことにしない")
		})
	}
}

// キャッシュ付きの repository は、書いた後に古い行を返さない。
func TestCachedSetAccountCreatedAtIfNull_InvalidatesCache(t *testing.T) {
	seedUser(t, "acIfNullC1")
	repo := NewCachedUserRepository(NewUserRepository(testDB))
	before, err := repo.FindByID("acIfNullC1")
	require.NoError(t, err)
	require.Nil(t, before.AccountCreatedAt)

	created := time.Date(2019, 1, 2, 3, 4, 5, 0, time.UTC)
	set, err := repo.SetAccountCreatedAtIfNull("acIfNullC1", created)
	require.NoError(t, err)
	require.True(t, set)

	after, err := repo.FindByID("acIfNullC1")
	require.NoError(t, err)
	require.NotNil(t, after.AccountCreatedAt, "書いた後にキャッシュの古い行を返している")
	assert.True(t, created.Equal(*after.AccountCreatedAt))
}
