package drive_test

import (
	"context"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordedModLog is one call to fakeModLogger.Log.
type recordedModLog struct {
	moderatorID string
	logType     moderationlog.LogType
	info        map[string]any
}

// fakeModLogger records moderation log writes synchronously.
type fakeModLogger struct {
	logs []recordedModLog
}

func (f *fakeModLogger) Log(_ context.Context, moderatorID string, t moderationlog.LogType, info map[string]any) {
	f.logs = append(f.logs, recordedModLog{moderatorID: moderatorID, logType: t, info: info})
}

// failingUserLookup fails every lookup.
type failingUserLookup struct{}

func (failingUserLookup) FindByID(string) (*model.User, error) {
	return nil, errors.New("lookup failed")
}

// modWriteFixture is a drive service where "mod" is a moderator, "plain" is
// not, and "owner" (remote.example) owns file "f1".
type modWriteFixture struct {
	svc      *drive.Service
	files    *testutil.MockDriveFileRepository
	folders  *testutil.MockDriveFolderRepository
	roles    *fakeMod
	logs     *fakeModLogger
	pub      *stubDriveStreamPublisher
	mod      *model.User
	plain    *model.User
	ownerID  string
	ownerHst string
}

func newModWriteFixture(t *testing.T, sensitive bool) *modWriteFixture {
	t.Helper()
	svc, files, folders := newSvc(t)
	ownerID := "owner"
	host := "remote.example"
	users := testutil.NewMockUserRepository()
	users.Users[ownerID] = &model.User{ID: ownerID, Username: "alice", Host: &host}
	roles := &fakeMod{moderators: map[string]bool{"mod": true}}
	logs := &fakeModLogger{}
	pub := &stubDriveStreamPublisher{}
	svc.SetRoleChecker(roles)
	svc.SetModerationLog(logs, users)
	svc.SetStreamingPublisher(pub)
	files.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &ownerID, Name: "a.png", IsSensitive: sensitive}
	return &modWriteFixture{
		svc: svc, files: files, folders: folders, roles: roles, logs: logs, pub: pub,
		mod: &model.User{ID: "mod"}, plain: &model.User{ID: "plain"},
		ownerID: ownerID, ownerHst: host,
	}
}

func boolPtr(v bool) *bool { return &v }

func TestUpdate_ModeratorMarksOthersFileSensitive(t *testing.T) {
	fx := newModWriteFixture(t, false)

	got, err := fx.svc.Update(fx.mod, "f1", drive.UpdateInput{IsSensitive: boolPtr(true)})
	require.NoError(t, err)
	assert.True(t, got.IsSensitive)
	// イベントは操作した moderator ではなく所有者へ送る。
	assert.Equal(t, []string{"owner:fileUpdated"}, fx.pub.events)
	require.Len(t, fx.logs.logs, 1)
	assert.Equal(t, recordedModLog{
		moderatorID: "mod",
		logType:     moderationlog.LogMarkSensitiveDriveFile,
		info: map[string]any{
			"fileId":           "f1",
			"fileUserId":       &fx.ownerID,
			"fileUserUsername": "alice",
			"fileUserHost":     &fx.ownerHst,
		},
	}, fx.logs.logs[0])
}

func TestUpdate_ModeratorUnmarksOthersFileSensitive(t *testing.T) {
	fx := newModWriteFixture(t, true)

	got, err := fx.svc.Update(fx.mod, "f1", drive.UpdateInput{IsSensitive: boolPtr(false)})
	require.NoError(t, err)
	assert.False(t, got.IsSensitive)
	require.Len(t, fx.logs.logs, 1)
	assert.Equal(t, moderationlog.LogUnmarkSensitiveDriveFile, fx.logs.logs[0].logType)
}

// 値が変わらないときは、本家と同じくログを残さない。
func TestUpdate_ModeratorNoChangeLeavesNoLog(t *testing.T) {
	for _, sensitive := range []bool{true, false} {
		fx := newModWriteFixture(t, sensitive)
		_, err := fx.svc.Update(fx.mod, "f1", drive.UpdateInput{IsSensitive: boolPtr(sensitive)})
		require.NoError(t, err)
		assert.Empty(t, fx.logs.logs, "sensitive=%v", sensitive)

		_, err = fx.svc.Update(fx.mod, "f1", drive.UpdateInput{})
		require.NoError(t, err)
		assert.Empty(t, fx.logs.logs, "empty input, sensitive=%v", sensitive)
	}
}

// 所有者が自分のファイルを変えたときはモデレーションログを残さない。所有者が
// moderator でも同じ (本家の `file.userId !== updater.id`)。
func TestUpdate_OwnerModeratorLeavesNoLog(t *testing.T) {
	fx := newModWriteFixture(t, false)
	fx.roles.moderators["owner"] = true

	_, err := fx.svc.Update(&model.User{ID: "owner"}, "f1", drive.UpdateInput{IsSensitive: boolPtr(true)})
	require.NoError(t, err)
	assert.Empty(t, fx.logs.logs)
	require.NoError(t, fx.svc.Delete(&model.User{ID: "owner"}, "f1"))
	assert.Empty(t, fx.logs.logs)
}

// moderator でも、名前・フォルダ・説明文は変えられない。isSensitive と一緒に
// 指定しても拒否し、何も書き換えない。
func TestUpdate_ModeratorCannotChangeOtherFields(t *testing.T) {
	name := "renamed.png"
	emptyName := ""
	comment := "c"
	commentPtr := &comment
	var nilComment *string
	folderID := "fo1"
	folderPtr := &folderID
	var nilFolder *string
	cases := []struct {
		name string
		in   drive.UpdateInput
	}{
		{"name", drive.UpdateInput{Name: &name}},
		{"empty name", drive.UpdateInput{Name: &emptyName}},
		{"comment", drive.UpdateInput{Comment: &commentPtr}},
		{"comment null", drive.UpdateInput{Comment: &nilComment}},
		{"folderId", drive.UpdateInput{FolderID: &folderPtr}},
		{"folderId null", drive.UpdateInput{FolderID: &nilFolder}},
		{"name with isSensitive", drive.UpdateInput{Name: &name, IsSensitive: boolPtr(true)}},
		{"comment with isSensitive", drive.UpdateInput{Comment: &commentPtr, IsSensitive: boolPtr(true)}},
		{"folderId with isSensitive", drive.UpdateInput{FolderID: &folderPtr, IsSensitive: boolPtr(true)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newModWriteFixture(t, false)
			fx.folders.Folders["fo1"] = &model.DriveFolder{ID: "fo1", UserID: &fx.ownerID}

			_, err := fx.svc.Update(fx.mod, "f1", tc.in)
			require.ErrorIs(t, err, drive.ErrAccessDenied)
			f := fx.files.Files["f1"]
			assert.Equal(t, "a.png", f.Name)
			assert.False(t, f.IsSensitive)
			assert.Nil(t, f.Comment)
			assert.Nil(t, f.FolderID)
			assert.Empty(t, fx.logs.logs)
			assert.Empty(t, fx.pub.events)
		})
	}
}

// moderator でない利用者は、他人のファイルのセンシティブも変えられない。
func TestUpdate_NonModeratorCannotChangeOthersSensitive(t *testing.T) {
	fx := newModWriteFixture(t, false)

	_, err := fx.svc.Update(fx.plain, "f1", drive.UpdateInput{IsSensitive: boolPtr(true)})
	require.ErrorIs(t, err, drive.ErrAccessDenied)
	assert.False(t, fx.files.Files["f1"].IsSensitive)
	assert.Empty(t, fx.logs.logs)
}

// alwaysMarkNsfw は操作した moderator ではなく、所有者の policy で見る。
func TestUpdate_ModeratorUnmarkUsesOwnerAlwaysMarkNsfw(t *testing.T) {
	t.Run("owner has the policy", func(t *testing.T) {
		fx := newModWriteFixture(t, true)
		fx.roles.policies = map[string]map[string]any{"owner": {"alwaysMarkNsfw": true}}

		_, err := fx.svc.Update(fx.mod, "f1", drive.UpdateInput{IsSensitive: boolPtr(false)})
		require.ErrorIs(t, err, drive.ErrCannotUnmarkSensitive)
		assert.True(t, fx.files.Files["f1"].IsSensitive)
		assert.Empty(t, fx.logs.logs)
	})
	// 本家と同じく、値が変わらない (もともと非センシティブ) なら policy に
	// 掛からない。
	t.Run("owner has the policy but the file is not sensitive", func(t *testing.T) {
		fx := newModWriteFixture(t, false)
		fx.roles.policies = map[string]map[string]any{"owner": {"alwaysMarkNsfw": true}}

		_, err := fx.svc.Update(fx.mod, "f1", drive.UpdateInput{IsSensitive: boolPtr(false)})
		require.NoError(t, err)
		assert.Empty(t, fx.logs.logs)
	})
	t.Run("only the moderator has the policy", func(t *testing.T) {
		fx := newModWriteFixture(t, true)
		fx.roles.policies = map[string]map[string]any{"mod": {"alwaysMarkNsfw": true}}

		_, err := fx.svc.Update(fx.mod, "f1", drive.UpdateInput{IsSensitive: boolPtr(false)})
		require.NoError(t, err)
		assert.False(t, fx.files.Files["f1"].IsSensitive)
	})
}

func TestDelete_ModeratorDeletesOthersFile(t *testing.T) {
	fx := newModWriteFixture(t, false)
	hook := &stubChartHook{}
	fx.svc.SetChartHook(hook)

	require.NoError(t, fx.svc.Delete(fx.mod, "f1"))
	assert.NotContains(t, fx.files.Files, "f1")
	assert.Equal(t, []string{"owner:fileDeleted"}, fx.pub.events)
	assert.Equal(t, []string{"f1"}, hook.deleted)
	require.Len(t, fx.logs.logs, 1)
	assert.Equal(t, "mod", fx.logs.logs[0].moderatorID)
	assert.Equal(t, moderationlog.LogDeleteDriveFile, fx.logs.logs[0].logType)
	assert.Equal(t, "alice", fx.logs.logs[0].info["fileUserUsername"])
}

// system 所有のファイル (userId / userHost とも NULL) は、moderator でも
// センシティブの付け外しだけができ、削除はできない (#3678)。ログの fileUser* は
// null になり、イベントの宛先は無い。
func TestModeratorWrite_SystemOwnedFile(t *testing.T) {
	fx := newModWriteFixture(t, false)
	fx.files.Files["f1"].UserID = nil

	_, err := fx.svc.Update(fx.mod, "f1", drive.UpdateInput{IsSensitive: boolPtr(true)})
	require.NoError(t, err)
	assert.True(t, fx.files.Files["f1"].IsSensitive)
	require.ErrorIs(t, fx.svc.Delete(fx.mod, "f1"), drive.ErrAccessDenied)
	assert.Contains(t, fx.files.Files, "f1")

	// 所有者の居ないファイルの alwaysMarkNsfw は base の policy で見る。操作した
	// moderator が alwaysMarkNsfw でも外せる。
	fx.roles.policies = map[string]map[string]any{"mod": {"alwaysMarkNsfw": true}}
	_, err = fx.svc.Update(fx.mod, "f1", drive.UpdateInput{IsSensitive: boolPtr(false)})
	require.NoError(t, err)
	assert.False(t, fx.files.Files["f1"].IsSensitive)

	assert.Empty(t, fx.pub.events)
	require.Len(t, fx.logs.logs, 2)
	assert.Equal(t, moderationlog.LogMarkSensitiveDriveFile, fx.logs.logs[0].logType)
	assert.Equal(t, moderationlog.LogUnmarkSensitiveDriveFile, fx.logs.logs[1].logType)
	for _, l := range fx.logs.logs {
		assert.Nil(t, l.info["fileUserId"])
		assert.Nil(t, l.info["fileUserUsername"])
		assert.Nil(t, l.info["fileUserHost"])
	}

	// moderator でなければ、センシティブも変えられない。
	_, err = fx.svc.Update(fx.plain, "f1", drive.UpdateInput{IsSensitive: boolPtr(true)})
	require.ErrorIs(t, err, drive.ErrAccessDenied)
	require.ErrorIs(t, fx.svc.Delete(fx.plain, "f1"), drive.ErrAccessDenied)
}

// 所有者の居ないリモートの添付 (userHost 付き) は system 所有ではないので、
// 本家と同じく moderator は削除できる。moderator でなければできない。
func TestModeratorWrite_OwnerlessRemoteFile(t *testing.T) {
	fx := newModWriteFixture(t, false)
	fx.files.Files["f1"].UserID = nil
	fx.files.Files["f1"].UserHost = &fx.ownerHst

	require.ErrorIs(t, fx.svc.Delete(fx.plain, "f1"), drive.ErrAccessDenied)
	require.NoError(t, fx.svc.Delete(fx.mod, "f1"))
	assert.NotContains(t, fx.files.Files, "f1")
	assert.Empty(t, fx.pub.events)
	require.Len(t, fx.logs.logs, 1)
	assert.Equal(t, moderationlog.LogDeleteDriveFile, fx.logs.logs[0].logType)
	assert.Nil(t, fx.logs.logs[0].info["fileUserId"])
}

// 所有者の名前を引けなくても、操作は済んでいるのでログは残す。
func TestModeratorWrite_OwnerLookupFailureStillLogs(t *testing.T) {
	fx := newModWriteFixture(t, false)
	fx.svc.SetModerationLog(fx.logs, failingUserLookup{})

	_, err := fx.svc.Update(fx.mod, "f1", drive.UpdateInput{IsSensitive: boolPtr(true)})
	require.NoError(t, err)
	require.Len(t, fx.logs.logs, 1)
	assert.Equal(t, &fx.ownerID, fx.logs.logs[0].info["fileUserId"])
	assert.Nil(t, fx.logs.logs[0].info["fileUserUsername"])
}

// ログが配線されていなくても、操作自体は通る。
func TestModeratorWrite_WithoutModerationLog(t *testing.T) {
	svc, files, _ := newSvc(t)
	ownerID := "owner"
	files.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &ownerID}
	svc.SetRoleChecker(&fakeMod{moderators: map[string]bool{"mod": true}})
	assert.False(t, svc.HasModerationLog())
	svc.SetModerationLog(&fakeModLogger{}, nil)
	assert.False(t, svc.HasModerationLog())

	_, err := svc.Update(&model.User{ID: "mod"}, "f1", drive.UpdateInput{IsSensitive: boolPtr(true)})
	require.NoError(t, err)
	require.NoError(t, svc.Delete(&model.User{ID: "mod"}, "f1"))
}

func TestHasModerationLog(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetModerationLog(&fakeModLogger{}, testutil.NewMockUserRepository())
	assert.True(t, svc.HasModerationLog())
}

// 絵文字が画像として使っているファイルは、system 所有でなくても moderator には
// 消させない (#3678)。所有者本人は消せる。
func TestDelete_ModeratorCannotDeleteEmojiImage(t *testing.T) {
	fx := newModWriteFixture(t, false)
	fx.files.Files["f1"].URL = "https://example.com/files/f1"
	fx.files.EmojiReferencedURLs = map[string]bool{"https://example.com/files/f1": true}

	require.ErrorIs(t, fx.svc.Delete(fx.mod, "f1"), drive.ErrAccessDenied)
	assert.Contains(t, fx.files.Files, "f1")
	assert.Empty(t, fx.logs.logs)
	assert.Empty(t, fx.pub.events)

	// センシティブの付け外しは絵文字の画像でもできる。
	_, err := fx.svc.Update(fx.mod, "f1", drive.UpdateInput{IsSensitive: boolPtr(true)})
	require.NoError(t, err)

	require.NoError(t, fx.svc.Delete(&model.User{ID: "owner"}, "f1"))
	assert.NotContains(t, fx.files.Files, "f1")
}

// 参照を引けなければ消さない (fail-closed)。エラーはそのまま返す。
func TestDelete_ModeratorEmojiLookupFailure(t *testing.T) {
	fx := newModWriteFixture(t, false)
	fx.files.IsReferencedByEmojiErr = errStub

	require.ErrorIs(t, fx.svc.Delete(fx.mod, "f1"), errStub)
	assert.Contains(t, fx.files.Files, "f1")
	assert.Empty(t, fx.logs.logs)

	// 所有者本人の削除は参照を引かない。
	require.NoError(t, fx.svc.Delete(&model.User{ID: "owner"}, "f1"))
}
