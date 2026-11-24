package maintenance

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedStaff adds, all with approved = false and no signup reason: the root
// user root1, an administrator adm1, a moderator mod1, a user whose moderator
// role has expired exp1 and a plain member mem1. A conditional moderator role
// is also created.
func seedStaff(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, sql := range []string{
		`INSERT INTO "user" (id, username, "usernameLower", approved) VALUES
			('root1', 'Root', 'root', false), ('adm1', 'adm', 'adm', false), ('mod1', 'mod', 'mod', false),
			('exp1', 'exp', 'exp', false), ('mem1', 'mem', 'mem', false)`,
		`UPDATE meta SET "rootUserId" = 'root1'`,
		`INSERT INTO role (id, "updatedAt", "lastUsedAt", name, description, "isAdministrator", "isModerator") VALUES
			('ra', now(), now(), 'admins', '', true, false), ('rm', now(), now(), 'mods', '', false, true)`,
		`INSERT INTO role (id, "updatedAt", "lastUsedAt", name, description, "isModerator", target) VALUES
			('rc', now(), now(), 'auto mods', '', true, 'conditional')`,
		`INSERT INTO role_assignment (id, "userId", "roleId", "expiresAt") VALUES
			('a1', 'adm1', 'ra', NULL), ('a2', 'mod1', 'rm', now() + interval '1 day'), ('a3', 'exp1', 'rm', now() - interval '1 day')`,
	} {
		require.NoError(t, db.Exec(sql).Error)
	}
}

// 初期ユーザーと、管理者・モデレーターのロールを持つ利用者は、承認されていない
// 利用者に数えない (凍結・削除の対象にしない)。期限の切れたロールは数えない。
// 名前は数えた人だけを並べ、除いた人は別に並べる。
func TestInspectCherryPick_LeavesStaffOut(t *testing.T) {
	db := openCherryPickDB(t)
	requireApproval(t, db, "")
	seedStaff(t, db)

	r := inspect(t, db)
	assert.EqualValues(t, 2, r.UnapprovedMembers)
	assert.Equal(t, []string{"exp", "mem"}, r.UnapprovedMemberNames)
	assert.Equal(t, []string{"adm", "mod", "Root"}, r.UnapprovedStaff)
	assert.Equal(t, []string{"auto mods"}, r.ConditionalStaffRoles)

	ids, err := CherryPickUnapprovedMemberIDs(context.Background(), db)
	require.NoError(t, err)
	assert.Equal(t, []string{"exp1", "mem1"}, ids, "消す対象にも入らない")
}

// 名前は先頭の CherryPickListLimit 人だけ。件数は全員。
func TestInspectCherryPick_ListsTheFirstUnapprovedMembers(t *testing.T) {
	db := openCherryPickDB(t)
	requireApproval(t, db, "")
	require.NoError(t, db.Exec(`INSERT INTO "user" (id, username, "usernameLower", approved)
		SELECT 'u' || i, 'user' || lpad(i::text, 3, '0'), 'user' || lpad(i::text, 3, '0'), false FROM generate_series(1, 25) i`).Error)

	r := inspect(t, db)
	assert.EqualValues(t, 25, r.UnapprovedMembers)
	require.Len(t, r.UnapprovedMemberNames, CherryPickListLimit)
	assert.Equal(t, "user001", r.UnapprovedMemberNames[0])
	assert.Equal(t, "user020", r.UnapprovedMemberNames[CherryPickListLimit-1])
}

// 凍結・承認は、初期ユーザーと管理者・モデレーターに触らない。
func TestApplyCherryPickSQL_UnapprovedMembersLeaveStaffAlone(t *testing.T) {
	for _, action := range []string{CherryPickSuspend, CherryPickApprove} {
		t.Run(action, func(t *testing.T) {
			db := openCherryPickDB(t)
			requireApproval(t, db, "")
			seedStaff(t, db)
			require.NoError(t, ApplyCherryPickSQL(context.Background(), db, CherryPickPlan{UnapprovedAction: action, Now: cherryPickNow}))

			var touched []string
			col := `"isSuspended"`
			if action == CherryPickApprove {
				col = "approved"
			}
			require.NoError(t, db.Raw(`SELECT id FROM "user" WHERE `+col+` ORDER BY id`).Scan(&touched).Error)
			assert.Equal(t, []string{"exp1", "mem1"}, touched)
		})
	}
}
