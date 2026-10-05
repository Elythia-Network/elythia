package notes

import (
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
)

// resolveFiles は内部ヘルパーで package 内テストから直接叩けるので、
// 各分岐を個別にカバーする:
//  1. driveFileRepo == nil → 早期 return
//  2. ノート群に fileIDs が無い → 早期 return
//  3. 正常経路: DriveFile が見つかって packed に入る
func TestResolveFiles_NilRepo(t *testing.T) {
	h, _ := newTestHandler(t)
	notes := []entity.NoteEntity{
		{FileIDs: model.StringArray{"f1"}},
	}
	h.fieldResolver().ResolveFiles(notes)
	// driveFileRepo 未設定なので Files は埋まらない
	assert.Nil(t, notes[0].Files)
}

func TestResolveFiles_NoFileIDs(t *testing.T) {
	h, _ := newTestHandler(t)
	h.SetDriveFileRepo(testutil.NewMockDriveFileRepository())
	notes := []entity.NoteEntity{
		{FileIDs: model.StringArray{}},
	}
	h.fieldResolver().ResolveFiles(notes)
	assert.Nil(t, notes[0].Files)
}

func TestResolveFiles_Populates(t *testing.T) {
	h, _ := newTestHandler(t)
	fileRepo := testutil.NewMockDriveFileRepository()
	alice := "alice"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &alice, Name: "hello.png", Type: "image/png", URL: "https://example.com/f1"}
	fileRepo.Files["f2"] = &model.DriveFile{ID: "f2", UserID: &alice, Name: "world.png", Type: "image/png", URL: "https://example.com/f2"}
	h.SetDriveFileRepo(fileRepo)

	notes := []entity.NoteEntity{
		{FileIDs: model.StringArray{"f1", "f2"}},
		{FileIDs: model.StringArray{"f1"}},
	}
	h.fieldResolver().ResolveFiles(notes)

	// 1 本目は 2 ファイルパックされているはず
	require := assert.New(t)
	require.Len(notes[0].Files, 2)
	require.Len(notes[1].Files, 1)
}

// Renote / Reply の embed にも Files が埋まる (#416)。
func TestResolveFiles_EmbeddedRenoteAndReply(t *testing.T) {
	h, _ := newTestHandler(t)
	fileRepo := testutil.NewMockDriveFileRepository()
	alice := "alice"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &alice, Name: "outer.png", Type: "image/png", URL: "https://example.com/f1"}
	fileRepo.Files["f2"] = &model.DriveFile{ID: "f2", UserID: &alice, Name: "renote.png", Type: "image/png", URL: "https://example.com/f2"}
	fileRepo.Files["f3"] = &model.DriveFile{ID: "f3", UserID: &alice, Name: "reply.png", Type: "image/png", URL: "https://example.com/f3"}
	h.SetDriveFileRepo(fileRepo)

	notes := []entity.NoteEntity{{
		FileIDs: model.StringArray{"f1"},
		Renote:  &entity.NoteEntity{FileIDs: model.StringArray{"f2"}},
		Reply:   &entity.NoteEntity{FileIDs: model.StringArray{"f3"}},
	}}
	h.fieldResolver().ResolveFiles(notes)

	assert.Len(t, notes[0].Files, 1)
	assert.Len(t, notes[0].Renote.Files, 1)
	assert.Len(t, notes[0].Reply.Files, 1)
}

// Renote のみ、Reply のみでも問題なく動く。nil entity は populateNoteFiles で
// 早期 return される。
func TestResolveFiles_EmbeddedRenoteOnly(t *testing.T) {
	h, _ := newTestHandler(t)
	fileRepo := testutil.NewMockDriveFileRepository()
	alice := "alice"
	fileRepo.Files["f2"] = &model.DriveFile{ID: "f2", UserID: &alice, Name: "renote.png", Type: "image/png", URL: "https://example.com/f2"}
	h.SetDriveFileRepo(fileRepo)

	notes := []entity.NoteEntity{{
		Renote: &entity.NoteEntity{FileIDs: model.StringArray{"f2"}},
	}}
	h.fieldResolver().ResolveFiles(notes)
	assert.Len(t, notes[0].Renote.Files, 1)
}

// 未知の fileID が含まれる場合は無視される (map lookup miss 経路)。
func TestResolveFiles_UnknownFileIDsIgnored(t *testing.T) {
	h, _ := newTestHandler(t)
	fileRepo := testutil.NewMockDriveFileRepository()
	alice := "alice"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &alice, Name: "hello.png", Type: "image/png", URL: "https://example.com/f1"}
	h.SetDriveFileRepo(fileRepo)

	notes := []entity.NoteEntity{
		{FileIDs: model.StringArray{"f1", "missing"}},
	}
	h.fieldResolver().ResolveFiles(notes)
	assert.Len(t, notes[0].Files, 1)
}
