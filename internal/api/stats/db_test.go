package stats

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

var testDB *gorm.DB

func init() {
	testDB = testutil.MustOpenTestDB()
	testutil.ApplyMigrations(testDB)
}

// instances / reactionsCount は chart ではなく DB を直接数える。stub では
// 通らない分岐なので実 DB で確かめる。
func TestStats_CountsFromDB(t *testing.T) {
	for _, tbl := range []string{"note_reaction", "note", "user", "instance"} {
		require.NoError(t, testDB.Exec(`DELETE FROM "`+tbl+`"`).Error)
	}

	got := callStats(t, NewHandler(testDB, nil, nil))
	assert.Equal(t, float64(0), got["instances"])
	assert.Equal(t, float64(0), got["reactionsCount"])

	require.NoError(t, testDB.Create(&model.Instance{
		ID: "stats1", Host: "a.example", FirstRetrievedAt: time.Now(),
	}).Error)
	require.NoError(t, testDB.Create(&model.Instance{
		ID: "stats2", Host: "b.example", FirstRetrievedAt: time.Now(),
	}).Error)
	// **note_reaction は user / note への FK を持つ。** 行だけ入れると
	// `note_reaction_userId_fkey` で落ちるので、参照先も作る。
	require.NoError(t, testDB.Create(&model.User{
		ID: "statsu1", Username: "statsu1", UsernameLower: "statsu1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}).Error)
	require.NoError(t, testDB.Create(&model.Note{
		ID: "statsn1", UserID: "statsu1", Visibility: model.NoteVisibilityPublic,
	}).Error)
	require.NoError(t, testDB.Create(&model.NoteReaction{
		ID: "react1", UserID: "statsu1", NoteID: "statsn1", Reaction: "👍",
	}).Error)

	got = callStats(t, NewHandler(testDB, nil, nil))
	assert.Equal(t, float64(2), got["instances"])
	assert.Equal(t, float64(1), got["reactionsCount"])
}
