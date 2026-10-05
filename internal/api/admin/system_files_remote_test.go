package admin_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/model"
)

// TestDriveFiles_SystemTokenExcludesUnownedRemote は #2753 を固定する。
//
// `userId IS NULL` の集合は、もはや local な system 資産だけではない。
// 著者が materialize されていないリモート添付は owner 無しで保存され
// (#2717)、DeleteOrphanRemoteUsers (#2340) が親 user を消した行も
// ON DELETE SET NULL で NULL になる。
//
// それらを「emoji copy / import zip の保管先」として一覧に出すと、管理者が
// 説明を信じて消したときに**表示中の note の画像が消える**。cleanup 側は
// #2721 で同じ行を除外済みで、admin UI からの手動削除だけが経路として
// 残っていた。
func TestDriveFiles_SystemTokenExcludesUnownedRemote(t *testing.T) {
	host := "remote.example"
	h, _ := setupDriveFileHandler(t,
		// local な system file (emoji copy / import zip 相当)
		&model.DriveFile{ID: "d_local_sys", UserID: nil, Type: "image/png"},
		// owner 無しのリモート添付 (#2717 / #2340 由来)。**表示中の note が参照しうる**
		&model.DriveFile{ID: "d_remote_unowned", UserID: nil, UserHost: &host, Type: "image/png"},
	)

	body := fmt.Sprintf(`{"userId":%q,"limit":10}`, apiadmin.SystemUserIDToken)
	rec := doPost(h.DriveFiles, body, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)

	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	gotIDs := make(map[string]bool, len(rows))
	for _, r := range rows {
		gotIDs[r["id"].(string)] = true
	}
	assert.True(t, gotIDs["d_local_sys"], "local な system file は出る")
	assert.False(t, gotIDs["d_remote_unowned"],
		"owner 無しのリモート添付を system file として見せない (#2753)")
}

// 除外した行が origin=remote に出ることは、mock ではなく実 SQL 側で固定して
// ある (internal/repository の TestDriveFileRepository_ListForAdmin)。
// ここで mock 経由の確認を重ねても、既存の TestDriveFiles_FiltersByOrigin と
// 同じことを見るだけで増分が無い。
