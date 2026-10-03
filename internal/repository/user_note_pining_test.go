package repository

import (
	"context"
	"fmt"
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func insertTestNote(t *testing.T, id, userID string) *model.Note {
	t.Helper()
	return insertTestNoteOn(t, testDB, id, userID)
}

// insertTestNoteOn is insertTestNote against an explicit handle (兄弟 schema 用。
// insertTestUserOn と同じ理由、user_test.go を参照)。
func insertTestNoteOn(t *testing.T, db *gorm.DB, id, userID string) *model.Note {
	t.Helper()
	n := &model.Note{
		ID:         id,
		UserID:     userID,
		Visibility: model.NoteVisibilityPublic,
	}
	require.NoError(t, db.Create(n).Error)
	return n
}

func TestUserNotePiningRepository_Create_Find(t *testing.T) {
	repo := NewUserNotePiningRepository(testDB)
	user := insertTestUser(t, "u_pin_1", "pinuser1")
	defer cleanupUser(t, user.ID)
	note := insertTestNote(t, "n_pin_1", user.ID)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, note.ID)

	p := &model.UserNotePining{
		ID:     "pin_1",
		UserID: user.ID,
		NoteID: note.ID,
	}
	require.NoError(t, repo.Create(p))
	defer testDB.Exec(`DELETE FROM "user_note_pining" WHERE id = ?`, p.ID)

	found, err := repo.FindByPair(user.ID, note.ID)
	require.NoError(t, err)
	assert.Equal(t, p.ID, found.ID)
}

func TestUserNotePiningRepository_FindByPair_NotFound(t *testing.T) {
	repo := NewUserNotePiningRepository(testDB)
	_, err := repo.FindByPair("nope", "nope")
	assert.Error(t, err)
}

func TestUserNotePiningRepository_Delete(t *testing.T) {
	repo := NewUserNotePiningRepository(testDB)
	user := insertTestUser(t, "u_pin_2", "pinuser2")
	defer cleanupUser(t, user.ID)
	note := insertTestNote(t, "n_pin_2", user.ID)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, note.ID)

	p := &model.UserNotePining{ID: "pin_2", UserID: user.ID, NoteID: note.ID}
	require.NoError(t, repo.Create(p))

	require.NoError(t, repo.Delete(p))
	_, err := repo.FindByPair(user.ID, note.ID)
	assert.Error(t, err)
}

func TestUserNotePiningRepository_ListByUser_CountByUser(t *testing.T) {
	repo := NewUserNotePiningRepository(testDB)
	user := insertTestUser(t, "u_pin_3", "pinuser3")
	defer cleanupUser(t, user.ID)
	note1 := insertTestNote(t, "n_pin_3", user.ID)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, note1.ID)
	note2 := insertTestNote(t, "n_pin_4", user.ID)
	defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, note2.ID)

	require.NoError(t, repo.Create(&model.UserNotePining{ID: "pin_3", UserID: user.ID, NoteID: note1.ID}))
	defer testDB.Exec(`DELETE FROM "user_note_pining" WHERE id = ?`, "pin_3")
	require.NoError(t, repo.Create(&model.UserNotePining{ID: "pin_4", UserID: user.ID, NoteID: note2.ID}))
	defer testDB.Exec(`DELETE FROM "user_note_pining" WHERE id = ?`, "pin_4")

	rows, err := repo.ListByUser(user.ID)
	require.NoError(t, err)
	assert.Len(t, rows, 2)

	count, err := repo.CountByUser(user.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
}

func TestUserNotePiningRepository_CurrentPinReadsPrimary(t *testing.T) {
	replicaSchema := "repo_pin_primary_replica"
	require.NoError(t, testDB.Exec("CREATE SCHEMA IF NOT EXISTS "+replicaSchema).Error)
	t.Cleanup(func() { testDB.Exec("DROP SCHEMA IF EXISTS " + replicaSchema + " CASCADE") })
	require.NoError(t, testDB.Exec(fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS %s."user_note_pining" (LIKE "user_note_pining" INCLUDING ALL)`, replicaSchema)).Error)

	var primarySchema string
	require.NoError(t, testDB.Raw("SELECT current_schema()").Scan(&primarySchema).Error)
	gdb := openWithReplicaSchema(t, primarySchema, replicaSchema)
	repo := NewUserNotePiningRepository(gdb)

	user := insertTestUser(t, "upinprim", "pinprimary")
	t.Cleanup(func() { cleanupUser(t, user.ID) })
	note := insertTestNote(t, "npinprim", user.ID)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "note" WHERE id = ?`, note.ID) })
	pin := &model.UserNotePining{ID: "ppinprim", UserID: user.ID, NoteID: note.ID}
	require.NoError(t, repo.Create(pin))
	t.Cleanup(func() { testDB.Exec(`DELETE FROM "user_note_pining" WHERE id = ?`, pin.ID) })

	var replicaRows []*model.UserNotePining
	require.NoError(t, gdb.Where(`"userId" = ?`, user.ID).Find(&replicaRows).Error)
	require.Empty(t, replicaRows, "test premise: an ordinary SELECT is routed to the stale replica")

	found, err := repo.FindByPair(user.ID, note.ID)
	require.NoError(t, err)
	assert.Equal(t, pin.ID, found.ID)
	rows, err := repo.ListByUser(user.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, pin.ID, rows[0].ID)
}

func TestUserNotePiningRepository_QueryErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	db := testDB.WithContext(ctx)
	repo := NewUserNotePiningRepository(db)

	_, err := repo.ListByUser("a")
	assert.Error(t, err)

	_, err = repo.CountByUser("a")
	assert.Error(t, err)
}

// リモートの featured を取り込む経路は差分更新ではなく全置換にする (#2552)。
// **差分にすると、リモート側で外されたピンがこちらに残り続ける。**
func TestUserNotePiningRepository_ReplaceByUser(t *testing.T) {
	repo := NewUserNotePiningRepository(testDB)
	user := insertTestUser(t, "u_pin_rep", "pinrepuser")
	defer cleanupUser(t, user.ID)
	stale := insertTestNote(t, "n_pin_rep_stale", user.ID)
	fresh1 := insertTestNote(t, "n_pin_rep_1", user.ID)
	fresh2 := insertTestNote(t, "n_pin_rep_2", user.ID)
	defer testDB.Exec(`DELETE FROM "note" WHERE "userId" = ?`, user.ID)
	defer testDB.Exec(`DELETE FROM "user_note_pining" WHERE "userId" = ?`, user.ID)

	require.NoError(t, repo.Create(&model.UserNotePining{
		ID: "pin_rep_stale", UserID: user.ID, NoteID: stale.ID,
	}))

	require.NoError(t, repo.ReplaceByUser(user.ID, []*model.UserNotePining{
		{ID: "pin_rep_b", UserID: user.ID, NoteID: fresh1.ID},
		{ID: "pin_rep_a", UserID: user.ID, NoteID: fresh2.ID},
	}))

	rows, err := repo.ListByUser(user.ID)
	require.NoError(t, err)
	require.Len(t, rows, 2, "置換前のピンが残らないこと")
	// ListByUser は id の降順。取り込み側はこの順序に載せて並びを表現する。
	assert.Equal(t, "pin_rep_b", rows[0].ID)
	assert.Equal(t, "pin_rep_a", rows[1].ID)
}

// 空で置換するとピンが全部消えること (リモートが全部外した場合)。
func TestUserNotePiningRepository_ReplaceByUser_Empty(t *testing.T) {
	repo := NewUserNotePiningRepository(testDB)
	user := insertTestUser(t, "u_pin_rep_e", "pinrepempty")
	defer cleanupUser(t, user.ID)
	note := insertTestNote(t, "n_pin_rep_e", user.ID)
	defer testDB.Exec(`DELETE FROM "note" WHERE "userId" = ?`, user.ID)
	defer testDB.Exec(`DELETE FROM "user_note_pining" WHERE "userId" = ?`, user.ID)

	require.NoError(t, repo.Create(&model.UserNotePining{
		ID: "pin_rep_e", UserID: user.ID, NoteID: note.ID,
	}))
	require.NoError(t, repo.ReplaceByUser(user.ID, nil))

	count, err := repo.CountByUser(user.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

// 他のユーザーのピンを巻き込まないこと。
func TestUserNotePiningRepository_ReplaceByUser_ScopedToUser(t *testing.T) {
	repo := NewUserNotePiningRepository(testDB)
	mine := insertTestUser(t, "u_pin_rep_m", "pinrepmine")
	defer cleanupUser(t, mine.ID)
	other := insertTestUser(t, "u_pin_rep_o", "pinrepother")
	defer cleanupUser(t, other.ID)
	otherNote := insertTestNote(t, "n_pin_rep_o", other.ID)
	defer testDB.Exec(`DELETE FROM "note" WHERE "userId" IN (?, ?)`, mine.ID, other.ID)
	defer testDB.Exec(`DELETE FROM "user_note_pining" WHERE "userId" IN (?, ?)`, mine.ID, other.ID)

	require.NoError(t, repo.Create(&model.UserNotePining{
		ID: "pin_rep_o", UserID: other.ID, NoteID: otherNote.ID,
	}))
	require.NoError(t, repo.ReplaceByUser(mine.ID, nil))

	count, err := repo.CountByUser(other.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, count, "他ユーザーのピンは残ること")
}

// ListByUsers は複数の利用者のピン留めを 1 回で引き、id の降順で返す。他の
// 利用者のピン留めは混ぜない。
func TestUserNotePiningRepository_ListByUsers(t *testing.T) {
	repo := NewUserNotePiningRepository(testDB)
	reader, ok := repo.(UserNotePiningBatchReader)
	require.True(t, ok)
	u1 := insertTestUser(t, "u_pinb_1", "pinbuser1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "u_pinb_2", "pinbuser2")
	defer cleanupUser(t, u2.ID)
	u3 := insertTestUser(t, "u_pinb_3", "pinbuser3")
	defer cleanupUser(t, u3.ID)
	for _, n := range [][2]string{{"n_pinb_1", u1.ID}, {"n_pinb_2", u1.ID}, {"n_pinb_3", u2.ID}, {"n_pinb_4", u3.ID}} {
		insertTestNote(t, n[0], n[1])
		defer testDB.Exec(`DELETE FROM "note" WHERE id = ?`, n[0])
	}
	for _, p := range [][3]string{{"pinb_a", u1.ID, "n_pinb_1"}, {"pinb_c", u1.ID, "n_pinb_2"}, {"pinb_b", u2.ID, "n_pinb_3"}, {"pinb_d", u3.ID, "n_pinb_4"}} {
		require.NoError(t, repo.Create(&model.UserNotePining{ID: p[0], UserID: p[1], NoteID: p[2]}))
		defer testDB.Exec(`DELETE FROM "user_note_pining" WHERE id = ?`, p[0])
	}

	rows, err := reader.ListByUsers([]string{u1.ID, u2.ID})
	require.NoError(t, err)
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	assert.Equal(t, []string{"pinb_c", "pinb_b", "pinb_a"}, ids)

	empty, err := reader.ListByUsers(nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}
