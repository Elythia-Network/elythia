package user_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

// 期間と止め方 (#3466) を実 DB の user_profile へ書く。null は NULL、0 は 0
// として残し、省略した項目は変えない。止め方の列は既定で request になる。
func TestUpdateProfile_DB_FollowApproval(t *testing.T) {
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	repo := repository.NewUserRepository(db)
	svc := user.NewService(repo, repository.NewNoteRepository(db), repository.NewUserNotePiningRepository(db), gen)

	const uid = "fauser0001"
	require.NoError(t, db.Exec(`INSERT INTO "user" (id, "updatedAt", username, "usernameLower") VALUES (?, NOW(), ?, ?)`, uid, uid, uid).Error)
	require.NoError(t, db.Exec(`INSERT INTO "user_profile" ("userId") VALUES (?)`, uid).Error)
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "user_profile" WHERE "userId" = ?`, uid)
		db.Exec(`DELETE FROM "user" WHERE id = ?`, uid)
	})
	read := func() *model.UserProfile {
		t.Helper()
		var p model.UserProfile
		require.NoError(t, db.Where(`"userId" = ?`, uid).First(&p).Error)
		return &p
	}
	assert.Equal(t, model.FollowApprovalActionRequest, read().FollowApprovalAction, "列の既定は本家と同じ動き")

	local, remote := 43200, 0
	lp, rp := &local, &remote
	action := model.FollowApprovalActionSilentRequest
	_, err = svc.UpdateProfile(uid, user.UpdateInput{FollowApprovalLocalSeconds: &lp, FollowApprovalRemoteSeconds: &rp, FollowApprovalAction: &action})
	require.NoError(t, err)
	p := read()
	require.NotNil(t, p.FollowApprovalLocalSeconds)
	require.NotNil(t, p.FollowApprovalRemoteSeconds)
	assert.Equal(t, 43200, *p.FollowApprovalLocalSeconds)
	assert.Equal(t, 0, *p.FollowApprovalRemoteSeconds, "0 は NULL にしない")
	assert.Equal(t, action, p.FollowApprovalAction)

	var clear *int
	_, err = svc.UpdateProfile(uid, user.UpdateInput{FollowApprovalLocalSeconds: &clear})
	require.NoError(t, err)
	p = read()
	assert.Nil(t, p.FollowApprovalLocalSeconds, "null は NULL")
	require.NotNil(t, p.FollowApprovalRemoteSeconds, "省略は不変")
	assert.Equal(t, 0, *p.FollowApprovalRemoteSeconds)
	assert.Equal(t, action, p.FollowApprovalAction, "省略は不変")
}
