package repository

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// driveFileColumns は resolver が AP の添付から書く列と、その上限。
// migration/000001_initial.up.sql と一致させる。
var driveFileColumns = []struct {
	column string
	max    int
}{
	{"name", 256},
	{"comment", 512},
	{"type", 128},
	{"url", 1024},
	{"uri", 1024},
	{"thumbnailUrl", 512},
	{"blurhash", 128},
}

// 列の上限そのものを schema から固定する (#2723)。
//
// resolver 側の定数と独立に同じ数値が書かれているだけだと、揃って動かせば全部
// 緑になる。列が変わったらここが落ちる。
func TestDriveFile_ColumnLimits(t *testing.T) {
	for _, tc := range driveFileColumns {
		var n int
		require.NoError(t, testDB.Raw(`SELECT character_maximum_length FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'drive_file' AND column_name = ?`,
			tc.column).Scan(&n).Error)
		assert.Equal(t, tc.max, n,
			"drive_file.%s の列長が変わっている (internal/core/federation/resolver.go の定数も直すこと)", tc.column)
	}
}

// resolver が切っている長さで、実際の列制約を通ること (#2717)。
//
// mock は列制約を持たないので、resolver 側のテストだけでは
// 「varchar(256) に入る長さか」を確かめられない。上限そのものは
// migration/000001_initial.up.sql が持っているので、実 DB で突き合わせる。
func TestDriveFile_ColumnLimitsAcceptTruncatedValues(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	id := "dfl_limits_0000000000000000000a"
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "drive_file" WHERE id = ?`, id) })

	// resolver が通す最大長。`name` は URL の basename から作るので 256 まで
	// 伸びることは無いが、列の上限は 256 なのでそこで確かめる (#2723)。
	// **全角で埋める** — byte で切る実装だと 3 倍になって入らない。
	name := strings.Repeat("あ", 256)
	comment := strings.Repeat("あ", 512)
	host := "remote.example"

	err := repo.Create(&model.DriveFile{
		ID: id, Name: name, Comment: &comment, Type: "image/webp",
		URL: "https://media.example/files/limits.webp", UserHost: &host,
		IsLink: true,
	})
	require.NoError(t, err, "resolver が切っている長さが列に入らない")

	got, err := repo.FindByID(id)
	require.NoError(t, err)
	assert.Equal(t, 256, len([]rune(got.Name)))
	require.NotNil(t, got.Comment)
	assert.Equal(t, 512, len([]rune(*got.Comment)))
}

// userId が NULL の行を作れること (#2717)。
//
// 著者が materialize されていない添付は owner 無しで保存する。列は
// `varchar(32) REFERENCES "user"("id") ON DELETE SET NULL` で NULL 可。
func TestDriveFile_AcceptsNullOwner(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	id := "dfl_noowner_000000000000000000a"
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "drive_file" WHERE id = ?`, id) })

	host := "remote.example"
	require.NoError(t, repo.Create(&model.DriveFile{
		ID: id, Name: "file", Type: "image/webp",
		URL: "https://media.example/files/noowner.webp", UserHost: &host, IsLink: true,
	}))

	got, err := repo.FindByID(id)
	require.NoError(t, err)
	assert.Nil(t, got.UserID)
	require.NotNil(t, got.UserHost)
	assert.Equal(t, host, *got.UserHost)
}

// owner を落としたリモート添付が DeleteOrphans / ListOrphans の対象に
// ならないこと (#2721 review HIGH-1)。
//
// これらの行は orphan (userId IS NULL) ではあるが掃除対象ではない。理由は
// drive_file.go の orphanWhere の doc にある。
func TestDriveFile_OrphanCleanupKeepsUnownedRemoteFiles(t *testing.T) {
	repo := NewDriveFileRepository(testDB)
	remote := "dfl_orphan_remote_00000000000a"
	local := "dfl_orphan_local_000000000000a"
	t.Cleanup(func() {
		testDB.Exec(`DELETE FROM "drive_file" WHERE id IN (?, ?)`, remote, local)
	})

	host := "remote.example"
	require.NoError(t, repo.Create(&model.DriveFile{
		ID: remote, Name: "file", Type: "image/webp",
		URL: "https://media.example/files/keep.webp", UserHost: &host, IsLink: true,
	}))
	// local な owner 無し (emoji copy / import zip 相当) は従来どおり掃除対象。
	require.NoError(t, repo.Create(&model.DriveFile{
		ID: local, Name: "file", Type: "image/webp",
		URL: "https://example.com/files/sweep.webp",
	}))

	orphans, err := repo.ListOrphans(100)
	require.NoError(t, err)
	ids := make([]string, 0, len(orphans))
	for _, o := range orphans {
		ids = append(ids, o.ID)
	}
	assert.NotContains(t, ids, remote, "リモートの添付が掃除対象になっている")
	assert.Contains(t, ids, local, "local の owner 無しは従来どおり掃除対象")

	_, err = repo.DeleteOrphans()
	require.NoError(t, err)
	got, err := repo.FindByID(remote)
	require.NoError(t, err, "リモートの添付が DeleteOrphans で消えている")
	require.NotNil(t, got)
	_, err = repo.FindByID(local)
	assert.Error(t, err, "local の owner 無しが消えていない")
}
