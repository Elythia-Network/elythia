package transfer_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/core/transfer"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// --- Test doubles ---

type fakeDriveSaver struct {
	uploads []drive.UploadInput
	err     error
}

func (f *fakeDriveSaver) Upload(_ context.Context, in drive.UploadInput) (*model.DriveFile, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.uploads = append(f.uploads, in)
	return &model.DriveFile{ID: "file_" + in.Name, Name: in.Name, Size: len(in.Body)}, nil
}

type fakeNotifier struct {
	calls []notification.CreateInput
}

func (f *fakeNotifier) Create(_ context.Context, in notification.CreateInput) (*notification.Notification, error) {
	f.calls = append(f.calls, in)
	return &notification.Notification{ID: "n1"}, nil
}

// --- Helpers ---

func newExportDeps(t *testing.T) (*fakeDriveSaver, *fakeNotifier, transfer.ExporterDeps, *model.User) {
	t.Helper()
	idGen, _ := id.NewGenerator("aidx")
	_ = idGen

	userRepo := testutil.NewMockUserRepository()
	user := &model.User{ID: "alice", Username: "alice", UsernameLower: "alice"}
	userRepo.Users[user.ID] = user
	userRepo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob"}
	remoteHost := "remote.example"
	userRepo.Users["carol"] = &model.User{ID: "carol", Username: "carol", UsernameLower: "carol", Host: &remoteHost}

	driveSaver := &fakeDriveSaver{}
	notifier := &fakeNotifier{}

	deps := transfer.ExporterDeps{
		UserRepo:         userRepo,
		NoteRepo:         testutil.NewMockNoteRepository(),
		PollRepo:         testutil.NewMockPollRepository(),
		FollowingRepo:    testutil.NewMockFollowingRepository(),
		BlockingRepo:     testutil.NewMockBlockingRepository(),
		MutingRepo:       testutil.NewMockMutingRepository(),
		NoteFavoriteRepo: testutil.NewMockNoteFavoriteRepository(),
		AntennaRepo:      testutil.NewMockAntennaRepository(),
		ClipRepo:         testutil.NewMockClipRepository(),
		ClipNoteRepo:     testutil.NewMockClipNoteRepository(),
		UserListRepo:     testutil.NewMockUserListRepository(),
		Drive:            driveSaver,
		Notifier:         notifier,
	}
	return driveSaver, notifier, deps, user
}

// --- Tests ---

// #1249: exportCompleted 通知の exportedEntity は misskey enum 値に変換される。
// mk-go の複数形 / kebab-case をそのまま出すと misskey_dart の enum decode で
// 落ちるため、各 export type が正しい単数形 / camelCase になることを確認する。
func TestExport_ExportedEntityMapsToMisskeyEnum(t *testing.T) {
	cases := map[string]string{
		transfer.ExportNotes:        "note",
		transfer.ExportFavorites:    "favorite",
		transfer.ExportUserLists:    "userList",
		transfer.ExportAntennas:     "antenna",
		transfer.ExportClips:        "clip",
		transfer.ExportMuting:       "muting",
		transfer.ExportCustomEmojis: "customEmoji",
		transfer.ExportFollowing:    "following",
		transfer.ExportBlocking:     "blocking",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			saver, notifier, deps, user := newExportDeps(t)
			deps.EmojiRepo = &fakeEmojiSource{}
			exporter := transfer.NewExporter(deps)
			_, err := exporter.Export(context.Background(), user.ID, in)
			require.NoError(t, err)
			require.NotEmpty(t, saver.uploads)
			require.Len(t, notifier.calls, 1)
			assert.Equal(t, want, notifier.calls[0].Extra["exportedEntity"])
		})
	}
}

func TestExport_UnsupportedType(t *testing.T) {
	_, _, deps, user := newExportDeps(t)
	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, "nope")
	require.Error(t, err)
	assert.ErrorIs(t, err, transfer.ErrUnsupportedType)
}

func TestExport_UserNotFound(t *testing.T) {
	_, _, deps, _ := newExportDeps(t)
	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), "ghost", transfer.ExportNotes)
	assert.Error(t, err)
}

func TestExport_Notes_EmptyReturnsJSONArray(t *testing.T) {
	saver, notifier, deps, user := newExportDeps(t)
	exporter := transfer.NewExporter(deps)

	f, err := exporter.Export(context.Background(), user.ID, transfer.ExportNotes)
	require.NoError(t, err)
	assert.NotNil(t, f)
	require.Len(t, saver.uploads, 1)
	assert.True(t, strings.HasPrefix(saver.uploads[0].Name, "notes-"))
	assert.True(t, strings.HasSuffix(saver.uploads[0].Name, ".json"))
	assert.Equal(t, []byte("[]"), saver.uploads[0].Body)
	require.Len(t, notifier.calls, 1)
	assert.Equal(t, notification.TypeExportCompleted, notifier.calls[0].Type)
	// exportedEntity は misskey enum 値 (notes → note) に変換される (#1249)。
	assert.Equal(t, "note", notifier.calls[0].Extra["exportedEntity"])
}

func TestExport_Notes_WithPoll(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	noteRepo := deps.NoteRepo.(*testutil.MockNoteRepository)
	pollRepo := deps.PollRepo.(*testutil.MockPollRepository)

	text := "hello"
	n := &model.Note{
		ID:         "n1",
		UserID:     user.ID,
		Text:       &text,
		Visibility: model.NoteVisibilityPublic,
		HasPoll:    true,
		FileIDs:    model.StringArray{"f1"},
	}
	noteRepo.Notes[n.ID] = n
	pollRepo.Polls[n.ID] = &model.Poll{NoteID: n.ID, Multiple: true, Choices: model.StringArray{"a", "b"}}

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportNotes)
	require.NoError(t, err)

	var arr []map[string]any
	require.NoError(t, json.Unmarshal(saver.uploads[0].Body, &arr))
	require.Len(t, arr, 1)
	assert.Equal(t, "n1", arr[0]["id"])
	assert.Equal(t, "hello", arr[0]["text"])
	assert.NotNil(t, arr[0]["poll"])
}

func TestExport_Following_CSV(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	followingRepo := deps.FollowingRepo.(*testutil.MockFollowingRepository)

	followingRepo.Followings["f1"] = &model.Following{
		ID: "f1", FollowerID: user.ID, FolloweeID: "bob", WithReplies: true,
	}
	followingRepo.Followings["f2"] = &model.Following{
		ID: "f2", FollowerID: user.ID, FolloweeID: "carol", WithReplies: false,
	}

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportFollowing)
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(saver.uploads[0].Name, ".csv"))
	body := string(saver.uploads[0].Body)
	assert.Contains(t, body, "bob,withReplies=true")
	assert.Contains(t, body, "carol@remote.example,withReplies=false")
}

func TestExport_Blocking_CSV(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	blockingRepo := deps.BlockingRepo.(*testutil.MockBlockingRepository)
	blockingRepo.Blockings["b1"] = &model.Blocking{ID: "b1", BlockerID: user.ID, BlockeeID: "bob"}

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportBlocking)
	require.NoError(t, err)
	assert.Equal(t, "bob\n", string(saver.uploads[0].Body))
}

func TestExport_Muting_OnlyPermanent(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	mutingRepo := deps.MutingRepo.(*testutil.MockMutingRepository)

	mutingRepo.Mutings["m1"] = &model.Muting{ID: "m1", MuterID: user.ID, MuteeID: "bob"}
	exp := time.Now().Add(1 * time.Hour)
	mutingRepo.Mutings["m2"] = &model.Muting{ID: "m2", MuterID: user.ID, MuteeID: "carol", ExpiresAt: &exp}

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportMuting)
	require.NoError(t, err)
	body := string(saver.uploads[0].Body)
	assert.Contains(t, body, "bob")
	assert.NotContains(t, body, "carol")
}

func TestExport_UserLists_CSV(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	listRepo := deps.UserListRepo.(*testutil.MockUserListRepository)

	list := &model.UserList{ID: "l1", UserID: user.ID, Name: "friends"}
	listRepo.Lists["l1"] = list
	_ = listRepo.AddMember(&model.UserListMembership{ID: "m1", UserListID: "l1", UserID: "bob"})

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportUserLists)
	require.NoError(t, err)
	assert.Contains(t, string(saver.uploads[0].Body), "friends,bob,withReplies=false")
}

func TestExport_Antennas_JSON(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	antennaRepo := deps.AntennaRepo.(*testutil.MockAntennaRepository)

	antennaRepo.Antennas["a1"] = &model.Antenna{
		ID: "a1", UserID: user.ID, Name: "test", Src: model.AntennaSourceAll,
		Keywords:        datatypes.JSON([]byte(`[["go"]]`)),
		ExcludeKeywords: datatypes.JSON([]byte(`[]`)),
	}

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportAntennas)
	require.NoError(t, err)
	var arr []map[string]any
	require.NoError(t, json.Unmarshal(saver.uploads[0].Body, &arr))
	require.Len(t, arr, 1)
	assert.Equal(t, "test", arr[0]["name"])
	assert.Equal(t, "all", arr[0]["src"])
}

func TestExport_Clips_JSON(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	clipRepo := deps.ClipRepo.(*testutil.MockClipRepository)
	clipNoteRepo := deps.ClipNoteRepo.(*testutil.MockClipNoteRepository)
	noteRepo := deps.NoteRepo.(*testutil.MockNoteRepository)

	clipRepo.Clips["c1"] = &model.Clip{ID: "c1", UserID: user.ID, Name: "clip1"}
	text := "clipped note"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: user.ID, Text: &text, Visibility: model.NoteVisibilityPublic}
	_ = clipNoteRepo.Create(&model.ClipNote{ID: "cn1", ClipID: "c1", NoteID: "n1"})

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportClips)
	require.NoError(t, err)
	var arr []map[string]any
	require.NoError(t, json.Unmarshal(saver.uploads[0].Body, &arr))
	require.Len(t, arr, 1)
	assert.Equal(t, "clip1", arr[0]["name"])
	notes := arr[0]["notes"].([]any)
	assert.Len(t, notes, 1)
}

func TestExport_Favorites_JSON(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	favRepo := deps.NoteFavoriteRepo.(*testutil.MockNoteFavoriteRepository)

	text := "fav note"
	n := &model.Note{ID: "n1", UserID: "bob", Text: &text, Visibility: model.NoteVisibilityPublic}
	favRepo.Favorites["fv1"] = &model.NoteFavorite{ID: "fv1", UserID: user.ID, NoteID: "n1", Note: n}

	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportFavorites)
	require.NoError(t, err)
	var arr []map[string]any
	require.NoError(t, json.Unmarshal(saver.uploads[0].Body, &arr))
	require.Len(t, arr, 1)
	note := arr[0]["note"].(map[string]any)
	assert.Equal(t, "fav note", note["text"])
}

func TestExport_DriveUploadError(t *testing.T) {
	saver, _, deps, user := newExportDeps(t)
	saver.err = errors.New("drive boom")
	exporter := transfer.NewExporter(deps)
	_, err := exporter.Export(context.Background(), user.ID, transfer.ExportNotes)
	assert.Error(t, err)
}
