package repository

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/model"
)

// リモートが自由に決められる値がそのまま入る、残りの text 列と上限 (#2726)。
// upstream の MiNoteReaction / MiAbuseUserReport も同じ 260 / 2048。
var remoteTextColumns = []struct {
	table  string
	column string
	max    int
}{
	{"note_reaction", "reaction", 260},
	{"abuse_user_report", "comment", 2048},
}

func TestRemoteTextColumnLimits(t *testing.T) {
	for _, tc := range remoteTextColumns {
		var n int
		require.NoError(t, testDB.Raw(`SELECT character_maximum_length FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`,
			tc.table, tc.column).Scan(&n).Error)
		assert.Equal(t, tc.max, n,
			"%s.%s の列長が変わっている (reactionMaxRunes / abuseReportCommentMaxRunes も直すこと)",
			tc.table, tc.column)
	}
}

// 上限ちょうどの値が実際に書けること (#2726)。
func TestRemoteTextColumnLimitsAcceptMaxLengthValues(t *testing.T) {
	reporter := insertTestUser(t, "u_rtc_1", "rtcuser1")
	defer cleanupUser(t, reporter.ID)
	target := insertTestUser(t, "u_rtc_2", "rtcuser2")
	defer cleanupUser(t, target.ID)

	note := &model.Note{
		ID:         "n_rtc_1",
		UserID:     target.ID,
		Visibility: model.NoteVisibilityPublic,
		Reactions:  datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, testDB.Create(note).Error)
	defer cleanupNote(t, note.ID)

	// **絵文字で埋める。** byte で数える実装なら 4 倍になって入らない。
	require.NoError(t, NewNoteReactionRepository(testDB).Create(&model.NoteReaction{
		ID: "nr_rtc_1", UserID: reporter.ID, NoteID: note.ID,
		Reaction: strings.Repeat("\U0001F600", 260),
	}))
	defer testDB.Exec(`DELETE FROM "note_reaction" WHERE id = ?`, "nr_rtc_1")

	require.NoError(t, NewAbuseReportRepository(testDB).Create(&model.AbuseUserReport{
		ID: "ar_rtc_1", TargetUserID: target.ID, ReporterID: reporter.ID,
		Comment: strings.Repeat("あ", 2048),
	}))
	defer testDB.Exec(`DELETE FROM "abuse_user_report" WHERE id = ?`, "ar_rtc_1")

	var stored string
	require.NoError(t, testDB.Raw(
		`SELECT "reaction" FROM "note_reaction" WHERE id = ?`, "nr_rtc_1").Scan(&stored).Error)
	assert.Equal(t, 260, len([]rune(stored)))
	require.NoError(t, testDB.Raw(
		`SELECT "comment" FROM "abuse_user_report" WHERE id = ?`, "ar_rtc_1").Scan(&stored).Error)
	assert.Equal(t, 2048, len([]rune(stored)))
}
