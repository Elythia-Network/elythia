package maintenance

import (
	"os"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var testDB *gorm.DB

func TestMain(m *testing.M) {
	db, err := testutil.OpenTestDB()
	if err != nil {
		panic("failed to open test DB: " + err.Error())
	}
	testDB = db
	testutil.ApplyMigrations(testDB)
	os.Exit(m.Run())
}

func seedBackfillUser(t *testing.T, id string) {
	t.Helper()
	token := "tok_" + id
	require.NoError(t, testDB.Create(&model.User{
		ID: id, Username: id, UsernameLower: id, Token: &token,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "user" WHERE id = ?`, id) })
}

func seedBackfillNote(t *testing.T, id, userID string, tags []string) {
	t.Helper()
	require.NoError(t, testDB.Create(&model.Note{
		ID: id, UserID: userID, Visibility: model.NoteVisibilityPublic, Tags: model.StringArray(tags),
	}).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "note" WHERE id = ?`, id) })
}

func tagsOf(t *testing.T, id string) []string {
	t.Helper()
	var n model.Note
	require.NoError(t, testDB.Select("id", "tags").First(&n, "id = ?", id).Error)
	return []string(n.Tags)
}

// #2013: backfill は uppercase/全角 tag を NFKC+lowercase 正規化し、既に正規化済の
// note と空 tag note は変更しない。
func TestBackfillNoteTagsBatch(t *testing.T) {
	seedBackfillUser(t, "bf_u1")
	seedBackfillNote(t, "bf_n1", "bf_u1", []string{"Misskey", "ＴＯＫＹＯ"}) // uppercase + 全角
	seedBackfillNote(t, "bf_n2", "bf_u1", []string{"already"})          // 正規化済
	seedBackfillNote(t, "bf_n3", "bf_u1", []string{})                   // 空 (filter で除外)

	res, err := BackfillNoteTagsBatch(testDB, "", 100, false)
	require.NoError(t, err)
	assert.Equal(t, 2, res.Scanned, "非空 tag note のみ scan (n1, n2)")
	assert.Equal(t, 1, res.Updated, "n1 のみ正規化で変更")

	assert.Equal(t, []string{"misskey", "tokyo"}, tagsOf(t, "bf_n1"), "n1 が NFKC+lowercase 正規化 (#2013)")
	assert.Equal(t, []string{"already"}, tagsOf(t, "bf_n2"), "n2 は不変")
}

// #2013: dry-run は更新件数を数えるが書き込まない。
func TestBackfillNoteTagsBatch_DryRun(t *testing.T) {
	seedBackfillUser(t, "bfd_u1")
	seedBackfillNote(t, "bfd_n1", "bfd_u1", []string{"Upper"})

	res, err := BackfillNoteTagsBatch(testDB, "", 100, true)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Updated, "dry-run でも変更予定を数える")
	assert.Equal(t, []string{"Upper"}, tagsOf(t, "bfd_n1"), "dry-run は書き込まない (#2013)")
}

// #2013: keyset 再開。fromID より大きい id のみ処理する。
func TestBackfillNoteTagsBatch_Resumable(t *testing.T) {
	seedBackfillUser(t, "bfr_u1")
	seedBackfillNote(t, "bfr_a", "bfr_u1", []string{"AAA"})
	seedBackfillNote(t, "bfr_b", "bfr_u1", []string{"BBB"})

	// fromID="bfr_a" は bfr_a を除外し bfr_b のみ処理。
	res, err := BackfillNoteTagsBatch(testDB, "bfr_a", 100, false)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Scanned)
	assert.Equal(t, "bfr_b", res.LastID)
	assert.Equal(t, []string{"AAA"}, tagsOf(t, "bfr_a"), "fromID 以下は触らない")
	assert.Equal(t, []string{"bbb"}, tagsOf(t, "bfr_b"), "fromID より大は正規化")
}

// #2013: batchSize<=0 は 1000 に default 化する。空文字列 tag のみの note は
// 正規化で全 drop され tags が空配列にクリアされる (newTags==nil → '{}')。
func TestBackfillNoteTagsBatch_DefaultBatchAndEmptyClear(t *testing.T) {
	seedBackfillUser(t, "bfe_u1")
	seedBackfillNote(t, "bfe_n1", "bfe_u1", []string{""}) // 空文字列 element

	// batchSize=0 → default。
	res, err := BackfillNoteTagsBatch(testDB, "bfe_", 0, false)
	require.NoError(t, err)
	require.Equal(t, 1, res.Scanned)
	assert.Equal(t, 1, res.Updated)
	assert.Empty(t, tagsOf(t, "bfe_n1"), "全 drop で tags が空配列にクリアされる (#2013)")
}
