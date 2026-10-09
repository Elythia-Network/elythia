package repository

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

func silentFollowIDs(rows []*model.SilentFollow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// 同じ組は 1 行にまとめ、id を最後に記録したものへ書き換える。一覧は受け手
// ごとに新しい順 (#3466)。
func TestSilentFollowRepository_RecordAndList(t *testing.T) {
	repo := NewSilentFollowRepository(testDB)
	for _, u := range []string{"sfee01", "sfer01", "sfer02", "sfer03", "sfee02"} {
		seedUser(t, u)
	}
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "silent_follow" WHERE "followeeId" IN ('sfee01', 'sfee02')`) })

	require.NoError(t, repo.Record(&model.SilentFollow{ID: "sf0001", FollowerID: "sfer01", FolloweeID: "sfee01"}))
	require.NoError(t, repo.Record(&model.SilentFollow{ID: "sf0002", FollowerID: "sfer02", FolloweeID: "sfee01"}))
	require.NoError(t, repo.Record(&model.SilentFollow{ID: "sf0003", FollowerID: "sfer03", FolloweeID: "sfee01"}))
	// 別の受け手の行は混ざらない。
	require.NoError(t, repo.Record(&model.SilentFollow{ID: "sf0004", FollowerID: "sfer01", FolloweeID: "sfee02"}))
	// sfer01 がもう一度来た: 行を増やさず id を新しくする。
	require.NoError(t, repo.Record(&model.SilentFollow{ID: "sf0005", FollowerID: "sfer01", FolloweeID: "sfee01"}))

	rows, err := repo.ListByFollowee("sfee01", 10, "", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"sf0005", "sf0003", "sf0002"}, silentFollowIDs(rows))
	assert.Equal(t, "sfer01", rows[0].FollowerID)

	rows, err = repo.ListByFollowee("sfee01", 2, "", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"sf0005", "sf0003"}, silentFollowIDs(rows), "limit で切る")

	rows, err = repo.ListByFollowee("sfee01", 10, "", "sf0005")
	require.NoError(t, err)
	assert.Equal(t, []string{"sf0003", "sf0002"}, silentFollowIDs(rows), "untilId より古いもの")

	rows, err = repo.ListByFollowee("sfee01", 10, "sf0002", "")
	require.NoError(t, err)
	assert.Equal(t, []string{"sf0003", "sf0005"}, silentFollowIDs(rows), "sinceId だけなら古い順")

	rows, err = repo.ListByFollowee("sfee01", 0, "", "")
	require.NoError(t, err)
	assert.Len(t, rows, 3, "範囲外の limit は既定に倒す")

	rows, err = repo.ListByFollowee("sfee\x0001", 10, "", "")
	require.NoError(t, err)
	assert.Empty(t, rows, "NUL を含む id は引かずに空を返す")
}

// どちらかの利用者が消えたら行も消える (FK の ON DELETE CASCADE)。
func TestSilentFollowRepository_CascadeOnUserDelete(t *testing.T) {
	repo := NewSilentFollowRepository(testDB)
	seedUser(t, "sfee11")
	seedUser(t, "sfer11")
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "silent_follow" WHERE "followeeId" = 'sfee11'`) })
	require.NoError(t, repo.Record(&model.SilentFollow{ID: "sf0011", FollowerID: "sfer11", FolloweeID: "sfee11"}))

	require.NoError(t, testDB.Exec(`DELETE FROM "user" WHERE id = ?`, "sfer11").Error)
	rows, err := repo.ListByFollowee("sfee11", 10, "", "")
	require.NoError(t, err)
	assert.Empty(t, rows)
}
