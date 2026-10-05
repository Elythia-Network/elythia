package repository

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/model"
)

// #2713: cursor 対応済みの repository が `sinceId` 単独指定で ASC にならず、
// 「cursor の直後 N 件」ではなく「最新 N 件」を返していた。
//
// upstream の makePaginationQuery (`core/QueryService.ts`) は sinceId があって
// untilId が無いとき ASC で order する。mk-go にも同じ規則の paginationOrder が
// あるのに、以下は `id DESC` を固定していた。
//
// **各ケースは cursor の後ろに 2 件以上置く。** 1 件だと ASC でも DESC でも
// 同じ結果になり、向きを検出できない。
//
// なお emoji の ListRemoteWithFilter は**寄せていない** — upstream
// list-remote.ts が makePaginationQuery の後に .orderBy で上書きするので、
// 固定 DESC が正しい。理由は当該関数の doc に書いてある。

func TestPagination_SinceOnlyIsAscending_DriveFolder(t *testing.T) {
	repo := NewDriveFolderRepository(testDB)
	user := insertTestUser(t, "u_pgasc_df", "pgascdf")
	defer cleanupUser(t, user.ID)
	uid := user.ID

	for _, id := range []string{"pgasc_df1", "pgasc_df2", "pgasc_df3"} {
		f := &model.DriveFolder{ID: id, Name: "n", UserID: &uid}
		require.NoError(t, repo.Create(f))
		defer cleanupDriveFolder(t, f.ID)
	}

	rows, err := repo.ListByUser(uid, nil, "", "pgasc_df1", 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, []string{"pgasc_df2", "pgasc_df3"},
		[]string{rows[0].ID, rows[1].ID}, "sinceId 単独は ASC (upstream drive/folders.ts)")
}

func TestPagination_SinceOnlyIsAscending_NoteDraft(t *testing.T) {
	repo := NewNoteDraftRepository(testDB)
	user := insertTestUser(t, "u_pgasc_nd", "pgascnd")
	defer cleanupUser(t, user.ID)
	text := "t"

	for _, id := range []string{"pgasc_nd1", "pgasc_nd2", "pgasc_nd3"} {
		d := &model.NoteDraft{ID: id, UserID: user.ID, Text: &text, Visibility: "public"}
		require.NoError(t, repo.Create(d))
		defer testDB.Exec(`DELETE FROM "note_draft" WHERE id = ?`, id)
	}

	rows, err := repo.ListByUser(user.ID, "pgasc_nd1", "", nil, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, []string{"pgasc_nd2", "pgasc_nd3"},
		[]string{rows[0].ID, rows[1].ID}, "sinceId 単独は ASC (upstream notes/drafts/list.ts)")
}

func TestPagination_SinceOnlyIsAscending_Reversi(t *testing.T) {
	repo := NewReversiRepository(testDB)
	u1 := insertTestUser(t, "u_pgasc_r1", "pgascr1")
	defer cleanupUser(t, u1.ID)
	u2 := insertTestUser(t, "u_pgasc_r2", "pgascr2")
	defer cleanupUser(t, u2.ID)

	for _, id := range []string{"pgasc_rv1", "pgasc_rv2", "pgasc_rv3"} {
		g := &model.ReversiGame{
			ID: id, User1ID: u1.ID, User2ID: u2.ID, IsStarted: true,
			Map: model.StringArray{"--------"}, BW: "random", TimeLimitForEachTurn: 90,
			Logs: datatypes.JSON("[]"),
		}
		require.NoError(t, repo.Create(g))
		defer testDB.Exec(`DELETE FROM "reversi_game" WHERE id = ?`, id)
	}

	t.Run("ListByUserCursor", func(t *testing.T) {
		rows, err := repo.ListByUserCursor(u1.ID, "pgasc_rv1", "", 10)
		require.NoError(t, err)
		require.Len(t, rows, 2)
		assert.Equal(t, []string{"pgasc_rv2", "pgasc_rv3"},
			[]string{rows[0].ID, rows[1].ID}, "sinceId 単独は ASC (upstream reversi/games.ts)")
	})
	t.Run("ListStartedCursor", func(t *testing.T) {
		rows, err := repo.ListStartedCursor("pgasc_rv1", "", 10)
		require.NoError(t, err)
		require.GreaterOrEqual(t, len(rows), 2)
		assert.Equal(t, "pgasc_rv2", rows[0].ID, "sinceId 単独は ASC")
	})
}

func TestPagination_SinceOnlyIsAscending_RegistrationTicket(t *testing.T) {
	repo := NewRegistrationTicketRepository(testDB)
	u := insertTestUser(t, "u_pgasc_rt", "pgascrt")
	defer cleanupUser(t, u.ID)

	for _, id := range []string{"pgasc_rt1", "pgasc_rt2", "pgasc_rt3"} {
		require.NoError(t, testDB.Create(&model.RegistrationTicket{
			ID: id, Code: id, CreatedByID: &u.ID,
		}).Error)
		defer testDB.Exec(`DELETE FROM "registration_ticket" WHERE id = ?`, id)
	}

	rows, err := repo.ListByCreator(u.ID, "pgasc_rt1", "", 50)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, []string{"pgasc_rt2", "pgasc_rt3"},
		[]string{rows[0].ID, rows[1].ID}, "sinceId 単独は ASC (upstream invite/list.ts)")
}

// TestPagination_SinceOnlyIsAscending_Chat は applyIDCursor を固定する。
// この helper は chat の repository メソッド 8 つが通る (grep applyIDCursor の
// 呼び出し site 数) ので、影響が最も広い。
func TestPagination_SinceOnlyIsAscending_Chat(t *testing.T) {
	repo := NewChatRepository(testDB)
	user := insertTestUser(t, "u_pgasc_ch", "pgascch")
	defer cleanupUser(t, user.ID)

	for _, id := range []string{"pgasc_cr1", "pgasc_cr2", "pgasc_cr3"} {
		require.NoError(t, repo.CreateRoom(&model.ChatRoom{ID: id, Name: "n", OwnerID: user.ID}))
		defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, id)
	}

	rows, err := repo.ListRoomsByOwner(user.ID, "pgasc_cr1", "", 30)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, []string{"pgasc_cr2", "pgasc_cr3"},
		[]string{rows[0].ID, rows[1].ID}, "sinceId 単独は ASC (upstream ChatService は makePaginationQuery)")
}

// TestPagination_SinceOnlyIsAscending_ChatByFile は、cursor ページングする
// chat 一覧のうち applyIDCursor を通らない唯一のものを固定する。
func TestPagination_SinceOnlyIsAscending_ChatByFile(t *testing.T) {
	repo := NewChatRepository(testDB)
	user := insertTestUser(t, "u_pgasc_cf", "pgasccf")
	defer cleanupUser(t, user.ID)
	room := &model.ChatRoom{ID: "pgasc_cfr", Name: "n", OwnerID: user.ID}
	require.NoError(t, repo.CreateRoom(room))
	defer testDB.Exec(`DELETE FROM "chat_room" WHERE id = ?`, room.ID)

	fileID := "pgasc_cf_file"
	for _, id := range []string{"pgasc_cf1", "pgasc_cf2", "pgasc_cf3"} {
		m := &model.ChatMessage{
			ID: id, FromUserID: user.ID, ToRoomID: &room.ID, FileID: &fileID,
			Reads: model.StringArray{}, Reactions: model.StringArray{},
		}
		require.NoError(t, repo.CreateMessage(m))
		defer testDB.Exec(`DELETE FROM "chat_message" WHERE id = ?`, id)
	}

	rows, err := repo.ListMessagesByFileID(fileID, "", "pgasc_cf1", 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, []string{"pgasc_cf2", "pgasc_cf3"},
		[]string{rows[0].ID, rows[1].ID}, "sinceId 単独は ASC (upstream drive/files/attached-chat-messages.ts)")
}

func TestPagination_SinceOnlyIsAscending_AbuseReport(t *testing.T) {
	repo := NewAbuseReportRepository(testDB)
	target := insertTestUser(t, "pgasc_ar_t", "pgascart")
	defer cleanupUser(t, target.ID)
	reporter := insertTestUser(t, "pgasc_ar_r", "pgascarr")
	defer cleanupUser(t, reporter.ID)
	for _, id := range []string{"pgasc_ar1", "pgasc_ar2", "pgasc_ar3"} {
		require.NoError(t, testDB.Create(&model.AbuseUserReport{
			ID: id, TargetUserID: "pgasc_ar_t", ReporterID: "pgasc_ar_r",
		}).Error)
		defer testDB.Exec(`DELETE FROM "abuse_user_report" WHERE id = ?`, id)
	}

	rows, err := repo.List(nil, "", "", "pgasc_ar1", "", 50)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, []string{"pgasc_ar2", "pgasc_ar3"},
		[]string{rows[0].ID, rows[1].ID}, "sinceId 単独は ASC (upstream admin/abuse-user-reports.ts)")
}

func TestPagination_SinceOnlyIsAscending_ModerationLog(t *testing.T) {
	repo := NewModerationLogRepository(testDB)
	moderator := insertTestUser(t, "pgasc_ml_u", "pgascmlu")
	defer cleanupUser(t, moderator.ID)
	for _, id := range []string{"pgasc_ml1", "pgasc_ml2", "pgasc_ml3"} {
		require.NoError(t, testDB.Create(&model.ModerationLog{
			ID: id, UserID: "pgasc_ml_u", Type: "suspend", Info: []byte(`{}`),
		}).Error)
		defer testDB.Exec(`DELETE FROM "moderation_log" WHERE id = ?`, id)
	}

	rows, err := repo.List(model.ModerationLogFilter{SinceID: "pgasc_ml1", Limit: 50})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, []string{"pgasc_ml2", "pgasc_ml3"},
		[]string{rows[0].ID, rows[1].ID}, "sinceId 単独は ASC (upstream admin/show-moderation-logs.ts)")
}

// TestPagination_SinceOnlyIsAscending_RoleAssignment は、規則を手書きで複製して
// いた ListByRole を paginationOrder に寄せた分を固定する。寄せる前は順序を見る
// テストが無く、リファクタが挙動を変えても気付けなかった。
func TestPagination_SinceOnlyIsAscending_RoleAssignment(t *testing.T) {
	roleRepo := NewRoleRepository(testDB)
	assignRepo := NewRoleAssignmentRepository(testDB)
	now := time.Now()
	role := &model.Role{
		ID: "pgasc_role", UpdatedAt: now, LastUsedAt: now, Name: "PgAsc",
		Target: model.RoleTargetManual, Policies: datatypes.JSON([]byte("{}")),
		CondFormula: datatypes.JSON([]byte("{}")),
	}
	require.NoError(t, roleRepo.Create(role))
	defer testDB.Exec(`DELETE FROM "role" WHERE id = ?`, role.ID)

	for i, id := range []string{"pgasc_ra1", "pgasc_ra2", "pgasc_ra3"} {
		u := insertTestUser(t, fmt.Sprintf("u_pgasc_ra%d", i), fmt.Sprintf("pgascra%d", i))
		defer cleanupUser(t, u.ID)
		require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: id, UserID: u.ID, RoleID: role.ID}))
		defer testDB.Exec(`DELETE FROM "role_assignment" WHERE id = ?`, id)
	}

	rows, err := assignRepo.ListByRole(role.ID, "", "pgasc_ra1", 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, []string{"pgasc_ra2", "pgasc_ra3"},
		[]string{rows[0].ID, rows[1].ID}, "sinceId 単独は ASC (upstream admin/roles/users.ts)")
}
