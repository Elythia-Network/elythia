package testutil

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// TestMockDriveFileRepository_OrphanSelection は #2744 で追加した、mock 側の
// orphan 選択条件を固定するテスト。
//
// production の orphanWhere (internal/repository/drive_file.go) は #2721 で
// `"userHost" IS NULL` を獲得したが、mock 側 (mock_drive.go の DeleteOrphans /
// ListOrphans) は追随していなかった。その間 mock は「リモートの owner 無し行が
// admin cleanup で消える」という **production では起きない挙動**を許しており、
// #2721 が塞いだ穴そのものを mock が再現していた。
//
// **これは drift detector ではない。** 本 test は mock しか触らないので、
// production 側の orphanWhere に 4 つ目の述語が入っても緑のまま通る。
//
// **mock 側の** userHost guard を削る変異は、本 test だけが捕まえる。
// **production 側の** 同じ guard (orphanWhere) を削る変異を捕まえるのは、DB を
// 使う internal/repository/drive_file_limits_test.go の
// TestDriveFile_OrphanCleanupKeepsUnownedRemoteFiles で、こちらは orphan fixture
// に userHost を設定している。両者は別の guard を見ているので、**片方が緑でも
// もう片方は守られない** (#2754)。
//
// emoji guard のほうは、mock 側を削ると internal/api/admin の DriveCleanup 系
// test も一緒に落ちる (あちらが mock を使うため)。
func TestMockDriveFileRepository_OrphanSelection(t *testing.T) {
	host := "remote.example"
	owner := "user1"

	newRepo := func() *MockDriveFileRepository {
		repo := NewMockDriveFileRepository()
		// local orphan: userId NULL / userHost NULL -> 削除される
		repo.Files["local-orphan"] = &model.DriveFile{ID: "local-orphan", URL: "https://example.test/a.png"}
		// remote orphan: userId NULL / userHost あり -> #2721 により残す
		repo.Files["remote-orphan"] = &model.DriveFile{ID: "remote-orphan", URL: "https://example.test/b.png", UserHost: &host}
		// owned: userId あり -> 対象外
		repo.Files["owned"] = &model.DriveFile{ID: "owned", URL: "https://example.test/c.png", UserID: &owner}
		return repo
	}

	t.Run("DeleteOrphans keeps unowned remote files", func(t *testing.T) {
		repo := newRepo()

		n, err := repo.DeleteOrphans()
		require.NoError(t, err)

		assert.Equal(t, int64(1), n, "削除されるのは local orphan の 1 件だけ")
		assert.NotContains(t, repo.Files, "local-orphan", "local orphan は削除される")
		assert.Contains(t, repo.Files, "remote-orphan", "owner 無しでも remote は残す (#2721)")
		assert.Contains(t, repo.Files, "owned", "owner ありは対象外")
	})

	t.Run("ListOrphans mirrors DeleteOrphans selection", func(t *testing.T) {
		repo := newRepo()
		// emoji 次元も踏ませる。これが無いと ListOrphans 側の emoji guard を
		// 削る変異をこの test が素通りする (mirror を名乗る以上は見る)。
		repo.Files["emoji-referenced"] = &model.DriveFile{ID: "emoji-referenced", URL: "https://example.test/e.png"}
		repo.EmojiReferencedURLs = map[string]bool{"https://example.test/e.png": true}

		rows, err := repo.ListOrphans(100)
		require.NoError(t, err)

		ids := make([]string, 0, len(rows))
		for _, f := range rows {
			ids = append(ids, f.ID)
		}
		assert.ElementsMatch(t, []string{"local-orphan"}, ids,
			"ListOrphans は DeleteOrphans と同じ条件で選ぶ")
		assert.Len(t, repo.Files, 4, "ListOrphans は行を削除しない")
	})

	// emoji guard が userHost 条件の追加で壊れていないこと (#722 の回帰)。
	//
	// **fixture に remote を入れないこと。** newRepo() をそのまま使うと
	// remote-orphan は userHost guard で守られているので、userHost 側を壊した
	// 変異でもこの subtest が落ちる。「emoji guard が壊れた」という誤った
	// 診断になり、直す場所を間違わせる。ここは emoji guard だけを見る。
	t.Run("emoji guard still applies to local orphans", func(t *testing.T) {
		repo := NewMockDriveFileRepository()
		repo.Files["local-orphan"] = &model.DriveFile{ID: "local-orphan", URL: "https://example.test/a.png"}
		repo.Files["local-orphan-2"] = &model.DriveFile{ID: "local-orphan-2", URL: "https://example.test/d.png"}
		repo.EmojiReferencedURLs = map[string]bool{"https://example.test/a.png": true}

		n, err := repo.DeleteOrphans()
		require.NoError(t, err)

		assert.Equal(t, int64(1), n, "emoji 非参照の local orphan だけが消える")
		assert.Contains(t, repo.Files, "local-orphan", "emoji が参照する local orphan は消さない")
		assert.NotContains(t, repo.Files, "local-orphan-2")
	})
}
