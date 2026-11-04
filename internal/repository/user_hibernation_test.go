package repository

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func followerHibernated(t *testing.T, id string) bool {
	t.Helper()
	var v bool
	require.NoError(t, testDB.Raw(`SELECT "isFollowerHibernated" FROM "following" WHERE "id" = ?`, id).Row().Scan(&v))
	return v
}

func userHibernated(t *testing.T, id string) bool {
	t.Helper()
	var v bool
	require.NoError(t, testDB.Raw(`SELECT "isHibernated" FROM "user" WHERE "id" = ?`, id).Row().Scan(&v))
	return v
}

// 戻ってきた休眠中の利用者は、自分の印と、自分がフォロワーである行の印が外れる。
// 休眠していない利用者では何も書かない (#4019、本家 UserService.updateLastActiveDate)。
func TestWakeFromHibernation(t *testing.T) {
	for _, id := range []string{"hib4019a", "hib4019b", "hib4019c", "hib4019d"} {
		seedUser(t, id)
	}
	follows := map[string][2]string{
		"hibf4019ab": {"hib4019a", "hib4019b"}, // a がフォロワー
		"hibf4019ac": {"hib4019a", "hib4019c"}, // a がフォロワー
		"hibf4019ba": {"hib4019b", "hib4019a"}, // a がフォロー先 (外さない)
		"hibf4019db": {"hib4019d", "hib4019b"}, // 休眠していない d がフォロワー
	}
	for id, f := range follows {
		require.NoError(t, testDB.Exec(
			`INSERT INTO "following" ("id", "followerId", "followeeId") VALUES (?, ?, ?)`, id, f[0], f[1]).Error)
	}
	t.Cleanup(func() {
		for id := range follows {
			testDB.Exec(`DELETE FROM "following" WHERE "id" = ?`, id)
		}
	})

	repos := map[string]UserRepository{
		"plain":  NewUserRepository(testDB),
		"cached": NewCachedUserRepository(NewUserRepository(testDB)),
	}
	for name, repo := range repos {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, testDB.Exec(`UPDATE "user" SET "isHibernated" = ("id" = 'hib4019a') WHERE "id" LIKE 'hib4019%'`).Error)
			require.NoError(t, testDB.Exec(`UPDATE "following" SET "isFollowerHibernated" = true WHERE "id" LIKE 'hibf4019%'`).Error)

			woke, err := repo.WakeFromHibernation("hib4019a")
			require.NoError(t, err)
			assert.True(t, woke)
			assert.False(t, userHibernated(t, "hib4019a"))
			assert.False(t, followerHibernated(t, "hibf4019ab"))
			assert.False(t, followerHibernated(t, "hibf4019ac"))
			assert.True(t, followerHibernated(t, "hibf4019ba"), "フォロー先としての行は外さない")

			woke, err = repo.WakeFromHibernation("hib4019a")
			require.NoError(t, err)
			assert.False(t, woke, "起きている利用者は起こしたことにしない")

			// 休眠していない利用者は、自分がフォロワーの行に印が残っていても書かない
			woke, err = repo.WakeFromHibernation("hib4019d")
			require.NoError(t, err)
			assert.False(t, woke)
			assert.True(t, followerHibernated(t, "hibf4019db"))

			woke, err = repo.WakeFromHibernation("hib4019zz")
			require.NoError(t, err)
			assert.False(t, woke, "無い利用者は起こしたことにしない")
		})
	}
}

// キャッシュ付きの repository は、起こした後に休眠中の古い行を返さない。
func TestCachedWakeFromHibernation_InvalidatesCache(t *testing.T) {
	seedUser(t, "hib4019e")
	require.NoError(t, testDB.Exec(`UPDATE "user" SET "isHibernated" = true WHERE "id" = 'hib4019e'`).Error)
	repo := NewCachedUserRepository(NewUserRepository(testDB))
	before, err := repo.FindByID("hib4019e")
	require.NoError(t, err)
	require.True(t, before.IsHibernated)

	woke, err := repo.WakeFromHibernation("hib4019e")
	require.NoError(t, err)
	require.True(t, woke)

	after, err := repo.FindByID("hib4019e")
	require.NoError(t, err)
	assert.False(t, after.IsHibernated, "起こした後にキャッシュの古い行を返している")
}
