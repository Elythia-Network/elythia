package drive_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	drive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
)

// #1564 parity sweep のサービス層テスト群。

// --- ValidateFileName ---

func TestValidateFileName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  bool
	}{
		{"plain", "photo.jpg", true},
		{"japanese", "写真.png", true},
		{"boundary 200 runes", strings.Repeat("あ", 200), true},
		{"201 runes", strings.Repeat("あ", 201), false},
		{"empty", "", false},
		{"spaces only", "   ", false},
		{"backslash", `a\b.png`, false},
		{"slash", "a/b.png", false},
		{"dotdot", "a..png", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, drive.ValidateFileName(tc.input))
		})
	}
}

func TestUpdate_InvalidFileName(t *testing.T) {
	// upstream DriveService.updateFile は validateFileName 失敗で
	// InvalidFileNameError を投げる (#1564)。
	svc, fileRepo, _ := newSvc(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, Name: "old"}
	bad := "a/b"
	_, err := svc.Update(&model.User{ID: "u1"}, "f1", drive.UpdateInput{Name: &bad})
	require.ErrorIs(t, err, drive.ErrInvalidFileName)
	assert.Equal(t, "old", fileRepo.Files["f1"].Name, "検証失敗時は書き換えない")
}

// --- ShowByURL ---

func TestShowByURL_OwnerMatchesAllURLColumns(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	uid := "u1"
	web := "https://example.com/files/web"
	thumb := "https://example.com/files/thumb"
	fileRepo.Files["f1"] = &model.DriveFile{
		ID: "f1", UserID: &uid,
		URL:          "https://example.com/files/orig",
		WebpublicURL: &web,
		ThumbnailURL: &thumb,
	}
	for _, u := range []string{"https://example.com/files/orig", web, thumb} {
		got, err := svc.ShowByURL(&model.User{ID: "u1"}, u)
		require.NoError(t, err, u)
		assert.Equal(t, "f1", got.ID)
	}
}

func TestShowByURL_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.ShowByURL(&model.User{ID: "u1"}, "https://example.com/none")
	require.ErrorIs(t, err, drive.ErrFileNotFound)
}

func TestShowByURL_OtherOwnerDenied_ModeratorAllowed(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	other := "u2"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &other, URL: "https://example.com/files/x"}
	_, err := svc.ShowByURL(&model.User{ID: "u1"}, "https://example.com/files/x")
	require.ErrorIs(t, err, drive.ErrAccessDenied)

	svc.SetRoleChecker(&fakeMod{moderators: map[string]bool{"u1": true}})
	got, err := svc.ShowByURL(&model.User{ID: "u1"}, "https://example.com/files/x")
	require.NoError(t, err)
	assert.Equal(t, "f1", got.ID)
}

// --- UpdateFolder recursive nesting ---

func TestUpdateFolder_SelfParent_RecursiveNesting(t *testing.T) {
	svc, _, folderRepo := newSvc(t)
	uid := "u1"
	folderRepo.Folders["a"] = &model.DriveFolder{ID: "a", UserID: &uid}
	pid := "a"
	pp := &pid
	_, err := svc.UpdateFolder(&model.User{ID: "u1"}, "a", drive.UpdateFolderInput{ParentID: &pp})
	require.ErrorIs(t, err, drive.ErrRecursiveNesting)
}

func TestUpdateFolder_AncestorCycle_RecursiveNesting(t *testing.T) {
	// a の下に b、b の下に c がある状態で a の親を c にすると循環。
	svc, _, folderRepo := newSvc(t)
	uid := "u1"
	aID := "a"
	bID := "b"
	folderRepo.Folders["a"] = &model.DriveFolder{ID: "a", UserID: &uid}
	folderRepo.Folders["b"] = &model.DriveFolder{ID: "b", UserID: &uid, ParentID: &aID}
	folderRepo.Folders["c"] = &model.DriveFolder{ID: "c", UserID: &uid, ParentID: &bID}
	cid := "c"
	cp := &cid
	_, err := svc.UpdateFolder(&model.User{ID: "u1"}, "a", drive.UpdateFolderInput{ParentID: &cp})
	require.ErrorIs(t, err, drive.ErrRecursiveNesting)
}

func TestUpdateFolder_ValidReparent_OK(t *testing.T) {
	svc, _, folderRepo := newSvc(t)
	uid := "u1"
	folderRepo.Folders["a"] = &model.DriveFolder{ID: "a", UserID: &uid}
	folderRepo.Folders["b"] = &model.DriveFolder{ID: "b", UserID: &uid}
	bid := "b"
	bp := &bid
	got, err := svc.UpdateFolder(&model.User{ID: "u1"}, "a", drive.UpdateFolderInput{ParentID: &bp})
	require.NoError(t, err)
	require.NotNil(t, got.ParentID)
	assert.Equal(t, "b", *got.ParentID)
}

func TestUpdateFolder_ExistingCycleData_NoInfiniteLoop(t *testing.T) {
	// 旧 mk-go は循環を作れてしまっていたため、既に x⇄y の循環がある DB
	// でも祖先 walk が無限 loop しないこと (visited set guard、#1564)。
	svc, _, folderRepo := newSvc(t)
	uid := "u1"
	xID := "x"
	yID := "y"
	folderRepo.Folders["x"] = &model.DriveFolder{ID: "x", UserID: &uid, ParentID: &yID}
	folderRepo.Folders["y"] = &model.DriveFolder{ID: "y", UserID: &uid, ParentID: &xID}
	folderRepo.Folders["a"] = &model.DriveFolder{ID: "a", UserID: &uid}
	xp := &xID
	// a は循環 chain (x→y→x) の外なので walk は visited guard で打ち切られ、
	// 循環なし扱いで更新が通る。
	got, err := svc.UpdateFolder(&model.User{ID: "u1"}, "a", drive.UpdateFolderInput{ParentID: &xp})
	require.NoError(t, err)
	require.NotNil(t, got.ParentID)
	assert.Equal(t, "x", *got.ParentID)
}

// --- Folder stream events ---

type folderEvent struct {
	userID    string
	eventType string
	body      any
}

type fakeFolderPublisher struct{ events []folderEvent }

func (f *fakeFolderPublisher) PublishDriveFolderEvent(userID, eventType string, body any) {
	f.events = append(f.events, folderEvent{userID, eventType, body})
}

func TestFolderLifecycle_PublishesDriveStreamEvents(t *testing.T) {
	// upstream folders/{create,update,delete} は publishDriveStream で
	// folderCreated/folderUpdated (packed folder) / folderDeleted (id) を
	// 発火する (#1564)。
	svc, _, _ := newSvc(t)
	pub := &fakeFolderPublisher{}
	svc.SetFolderStreamingPublisher(pub)

	f, err := svc.CreateFolder(&model.User{ID: "u1"}, "docs", nil)
	require.NoError(t, err)
	newName := "renamed"
	_, err = svc.UpdateFolder(&model.User{ID: "u1"}, f.ID, drive.UpdateFolderInput{Name: &newName})
	require.NoError(t, err)
	require.NoError(t, svc.DeleteFolder(&model.User{ID: "u1"}, f.ID))

	require.Len(t, pub.events, 3)
	assert.Equal(t, "folderCreated", pub.events[0].eventType)
	created, ok := pub.events[0].body.(entity.DriveFolderEntity)
	require.True(t, ok, "created body は packed folder")
	assert.Equal(t, f.ID, created.ID)
	assert.NotEmpty(t, created.CreatedAt)

	assert.Equal(t, "folderUpdated", pub.events[1].eventType)
	updated, ok := pub.events[1].body.(entity.DriveFolderEntity)
	require.True(t, ok)
	assert.Equal(t, "renamed", updated.Name)

	assert.Equal(t, "folderDeleted", pub.events[2].eventType)
	assert.Equal(t, f.ID, pub.events[2].body, "deleted body は folder id 文字列")
	for _, ev := range pub.events {
		assert.Equal(t, "u1", ev.userID)
	}
}

func TestFolderLifecycle_NoPublisherIsNoop(t *testing.T) {
	svc, _, _ := newSvc(t)
	f, err := svc.CreateFolder(&model.User{ID: "u1"}, "docs", nil)
	require.NoError(t, err)
	require.NoError(t, svc.DeleteFolder(&model.User{ID: "u1"}, f.ID))
}
