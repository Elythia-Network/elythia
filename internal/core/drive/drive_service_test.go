package drive_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/core/drive"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/misc/imagedecode"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errStub = errors.New("stub error")

// brokenStorage points the LocalStorage rootDir at an existing file so that
// any Put/Delete operation fails.
func brokenStorage(t *testing.T) *drive.LocalStorage {
	t.Helper()
	tmp := t.TempDir()
	conflict := filepath.Join(tmp, "conflict")
	require.NoError(t, os.WriteFile(conflict, []byte("x"), 0o644))
	return drive.NewLocalStorage(conflict, "")
}

func newSvc(t *testing.T) (*drive.Service, *testutil.MockDriveFileRepository, *testutil.MockDriveFolderRepository) {
	t.Helper()
	fileRepo := testutil.NewMockDriveFileRepository()
	folderRepo := testutil.NewMockDriveFolderRepository()
	folderRepo.FilesRef = fileRepo
	storage := drive.NewLocalStorage(t.TempDir(), "https://example.com/files")
	idGen, _ := id.NewGenerator("aidx")
	svc := drive.NewService(fileRepo, folderRepo, storage, idGen)
	return svc, fileRepo, folderRepo
}

// --- Upload ---

// TestUpload_NilUserSystemFile guards that Upload accepts User == nil and
// produces a system-owned drive file (UserID/UserHost both nil), matching
// Misskey TS uploadFromUrl({user: null}). Used by EmojiCopy (#670) to store
// remote emoji images without binding them to the operator's account.
func TestUpload_NilUserSystemFile(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	f, err := svc.Upload(context.Background(), drive.UploadInput{Body: []byte("hello"), Name: "x.bin"})
	require.NoError(t, err)
	require.NotNil(t, f)
	assert.Nil(t, f.UserID)
	assert.Nil(t, f.UserHost)
	assert.Len(t, fileRepo.Files, 1)
}

func TestUpload_HappyPath(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	user := &model.User{ID: "u1"}
	f, err := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: []byte("hello"), Name: "hello.txt"})
	require.NoError(t, err)
	assert.Equal(t, "hello.txt", f.Name)
	assert.Len(t, fileRepo.Files, 1)
}

// stubMainStreamPublisher captures PublishMainEvent calls.
type stubMainStreamPublisher struct {
	calls []mainEventCall
}

type mainEventCall struct {
	userID    string
	eventType string
	body      any
}

func (s *stubMainStreamPublisher) PublishMainEvent(userID, eventType string, body any) {
	s.calls = append(s.calls, mainEventCall{userID, eventType, body})
}

func TestUpload_PublishesDriveFileCreatedOnMain(t *testing.T) {
	svc, _, _ := newSvc(t)
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)

	user := &model.User{ID: "u1"}
	f, err := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: []byte("hello"), Name: "hello.txt"})
	require.NoError(t, err)

	require.Len(t, pub.calls, 1)
	assert.Equal(t, "u1", pub.calls[0].userID)
	assert.Equal(t, "driveFileCreated", pub.calls[0].eventType)
	// bodyはentity.PackDriveFileでpackされたfileのはず。file IDが一致。
	body, ok := pub.calls[0].body.(entity.DriveFileEntity)
	require.True(t, ok)
	assert.Equal(t, f.ID, body.ID)
	assert.Equal(t, "hello.txt", body.Name)
}

func TestUpload_NoMainPublisher_NoEmit(t *testing.T) {
	svc, _, _ := newSvc(t)
	// SetMainStreamPublisherを呼ばない状態でもUpload自体は成功する。
	_, err := svc.Upload(context.Background(), drive.UploadInput{User: &model.User{ID: "u1"}, Body: []byte("hi"), Name: "hi.txt"})
	require.NoError(t, err)
}

func TestUpload_DedupByMD5(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	user := &model.User{ID: "u1"}
	first, _ := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: []byte("data"), Name: "a"})
	second, _ := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: []byte("data"), Name: "b"})
	assert.Equal(t, first.ID, second.ID)
	assert.Len(t, fileRepo.Files, 1)
}

func TestUpload_ForceSkipsDedup(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	user := &model.User{ID: "u1"}
	_, err := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: []byte("d"), Name: "a"})
	require.NoError(t, err)
	_, err = svc.Upload(context.Background(), drive.UploadInput{User: user, Body: []byte("d"), Name: "a", Force: true})
	require.NoError(t, err)
	assert.Len(t, fileRepo.Files, 2)
}

func TestUpload_FolderNotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	folderID := "ghost"
	_, err := svc.Upload(context.Background(), drive.UploadInput{User: &model.User{ID: "u1"}, Body: []byte("x"), FolderID: &folderID})
	require.ErrorIs(t, err, drive.ErrFolderNotFound)
}

// 他人所有の宛先 folder は「不在」扱いで ErrFolderNotFound、ACCESS_DENIED に
// しない (folder 存在 oracle を閉じる、#1908)。
func TestUpload_OtherOwnerFolderNotFound(t *testing.T) {
	svc, _, folderRepo := newSvc(t)
	other := "other"
	folderRepo.Folders["fid"] = &model.DriveFolder{ID: "fid", UserID: &other}
	folderID := "fid"
	_, err := svc.Upload(context.Background(), drive.UploadInput{User: &model.User{ID: "u1"}, Body: []byte("x"), FolderID: &folderID})
	require.ErrorIs(t, err, drive.ErrFolderNotFound)
}

// failingFileRepo causes Create to fail.
type failingFileRepo struct {
	*testutil.MockDriveFileRepository
}

func (f *failingFileRepo) Create(_ *model.DriveFile) error {
	return errStub
}

func TestUpload_RepoCreateError(t *testing.T) {
	repo := &failingFileRepo{MockDriveFileRepository: testutil.NewMockDriveFileRepository()}
	idGen, _ := id.NewGenerator("aidx")
	svc := drive.NewService(repo, testutil.NewMockDriveFolderRepository(), drive.NewLocalStorage(t.TempDir(), ""), idGen)
	_, err := svc.Upload(context.Background(), drive.UploadInput{User: &model.User{ID: "u1"}, Body: []byte("x")})
	assert.ErrorIs(t, err, errStub)
}

func TestUpload_StoragePutError(t *testing.T) {
	storage := brokenStorage(t)
	idGen, _ := id.NewGenerator("aidx")
	svc := drive.NewService(testutil.NewMockDriveFileRepository(), testutil.NewMockDriveFolderRepository(), storage, idGen)
	_, err := svc.Upload(context.Background(), drive.UploadInput{User: &model.User{ID: "u1"}, Body: []byte("x")})
	assert.Error(t, err)
}

// errReader implements io.Reader and always returns an error.
type errReader struct{}

func (errReader) Read(_ []byte) (int, error) { return 0, errStub }

func TestUpload_AccessKeyGenError(t *testing.T) {
	restore := drive.SetRandReaderForTest(errReader{})
	defer restore()

	svc, _, _ := newSvc(t)
	_, err := svc.Upload(context.Background(), drive.UploadInput{User: &model.User{ID: "u1"}, Body: []byte("x")})
	assert.Error(t, err)
}

// --- Show / FindByHash ---

func TestShow_NilUser(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.Show(nil, "x")
	assert.Error(t, err)
}

func TestShow_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.Show(&model.User{ID: "u1"}, "ghost")
	require.ErrorIs(t, err, drive.ErrFileNotFound)
}

func TestShow_OtherUserDenied(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	other := "other"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &other}
	_, err := svc.Show(&model.User{ID: "u1"}, "f1")
	require.ErrorIs(t, err, drive.ErrAccessDenied)
}

// moderator なら所有者でなくても見られる (upstream Misskey と一致)。
// リモート添付メディアの詳細表示は file.userId が remote user ID 固定
// になるので、この経路がないと管理者でも 403 で見られない。
type fakeMod struct {
	moderators map[string]bool
	policies   map[string]map[string]any // userID -> policies (nil = no override)
}

func (f *fakeMod) IsModerator(userID string) bool { return f.moderators[userID] }

func (f *fakeMod) GetUserPolicies(userID string) map[string]any {
	if f.policies == nil {
		return nil
	}
	return f.policies[userID]
}

// #1769: Capacity は driveCapacityMb role policy から算出する
// (1024*1024*driveCapacityMb)。roleChecker 未配線時は default 100 MiB。
func TestCapacity_FromPolicy(t *testing.T) {
	svc, _, _ := newSvc(t)
	// roleChecker 未配線 → default 100 MiB。
	assert.Equal(t, int64(100*1024*1024), svc.Capacity("u1"))
	// policy override は反映される。
	svc.SetRoleChecker(&fakeMod{policies: map[string]map[string]any{"u1": {"driveCapacityMb": 500}}})
	assert.Equal(t, int64(500*1024*1024), svc.Capacity("u1"))
	// override の無い user は default 100 MiB に fallback。
	assert.Equal(t, int64(100*1024*1024), svc.Capacity("u2"))
	// **小数の policy でも gate と同じ値を返す。** 素の `.(int)` だと型
	// アサーションに失敗して既定の 100MB を返し、実際の上限 (gate 側は
	// 小数を尊重する) と表示がずれる (#2611)。
	svc.SetRoleChecker(&fakeMod{policies: map[string]map[string]any{"u1": {"driveCapacityMb": 1.5}}})
	assert.Equal(t, int64(1.5*1024*1024), svc.Capacity("u1"))
}

func TestCapacity_MaxPolicySaturatesPositive(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetRoleChecker(&fakeMod{policies: map[string]map[string]any{
		"u1": {"driveCapacityMb": math.MaxFloat64},
	}})
	assert.Equal(t, int64(math.MaxInt64), svc.Capacity("u1"))
}

func TestShow_ModeratorBypass(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	other := "other"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &other}
	svc.SetRoleChecker(&fakeMod{moderators: map[string]bool{"u1": true}})
	got, err := svc.Show(&model.User{ID: "u1"}, "f1")
	require.NoError(t, err)
	assert.Equal(t, "f1", got.ID)
}

// moderator でない一般ユーザーは roleChecker が wire されていても
// 引き続き 403。
func TestShow_NonModeratorStillDenied(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	other := "other"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &other}
	svc.SetRoleChecker(&fakeMod{moderators: map[string]bool{}})
	_, err := svc.Show(&model.User{ID: "u1"}, "f1")
	require.ErrorIs(t, err, drive.ErrAccessDenied)
}

func TestFindByHash_NilUser(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.FindByHash(nil, "x")
	assert.Error(t, err)
}

func TestFindByHash_NotFound(t *testing.T) {
	// upstream find-by-hash は不一致でもエラーにせず空配列を返す (#1564)。
	svc, _, _ := newSvc(t)
	got, err := svc.FindByHash(&model.User{ID: "u1"}, "ghost")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestFindByHash_Found(t *testing.T) {
	// 同一 md5 の複数 file を全件返す (upstream findBy({md5,userId})、#1564)。
	svc, fileRepo, _ := newSvc(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, MD5: "abc"}
	fileRepo.Files["f2"] = &model.DriveFile{ID: "f2", UserID: &uid, MD5: "abc"}
	fileRepo.Files["f3"] = &model.DriveFile{ID: "f3", UserID: &uid, MD5: "other"}
	got, err := svc.FindByHash(&model.User{ID: "u1"}, "abc")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "f1", got[0].ID)
	assert.Equal(t, "f2", got[1].ID)
}

// --- Update ---

func TestUpdate_HappyPath(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, Name: "old"}

	newName := "new"
	cmt := "comment"
	cmtPtr := &cmt
	sensitive := true
	got, err := svc.Update(&model.User{ID: "u1"}, "f1", drive.UpdateInput{
		Name:        &newName,
		Comment:     &cmtPtr,
		IsSensitive: &sensitive,
	})
	require.NoError(t, err)
	assert.Equal(t, "new", got.Name)
}

func TestUpdate_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.Update(&model.User{ID: "u1"}, "ghost", drive.UpdateInput{})
	require.ErrorIs(t, err, drive.ErrFileNotFound)
}

// moderator は他ユーザーの file を Update できない (#499 privilege
// escalation 回帰防止)。Show() は moderator bypass を許す一方で、
// write 経路は owner-only に保たれていることを保証する。
func TestUpdate_ModeratorCannotEditOthersFile(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	other := "other"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &other}
	svc.SetRoleChecker(&fakeMod{moderators: map[string]bool{"u1": true}})

	newName := "renamed.png"
	_, err := svc.Update(&model.User{ID: "u1"}, "f1", drive.UpdateInput{Name: &newName})
	require.ErrorIs(t, err, drive.ErrAccessDenied)
}

// 同様に Delete も moderator bypass の対象外。
func TestDelete_ModeratorCannotDeleteOthersFile(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	other := "other"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &other}
	svc.SetRoleChecker(&fakeMod{moderators: map[string]bool{"u1": true}})

	err := svc.Delete(&model.User{ID: "u1"}, "f1")
	require.ErrorIs(t, err, drive.ErrAccessDenied)
}

// --- #1028: role policy gate (alwaysMarkNsfw / uploadableFileTypes) ---

// alwaysMarkNsfw=true の user の upload は IsSensitive=true で保存される。
func TestUpload_AlwaysMarkNsfwForcesSensitive(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	svc.SetRoleChecker(&fakeMod{
		policies: map[string]map[string]any{
			"u1": {"alwaysMarkNsfw": true},
		},
	})

	user := &model.User{ID: "u1"}
	f, err := svc.Upload(context.Background(), drive.UploadInput{
		User: user, Body: []byte("img"), Name: "x.txt", IsSensitive: false,
	})
	require.NoError(t, err)
	assert.True(t, f.IsSensitive, "role policy alwaysMarkNsfw=true で強制 sensitive")
	assert.Len(t, fileRepo.Files, 1)
}

// uploadableFileTypes allowlist に match しない mime は ErrUnallowedFileType。
func TestUpload_UploadableFileTypesRejectsMismatch(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetRoleChecker(&fakeMod{
		policies: map[string]map[string]any{
			"u1": {"uploadableFileTypes": []string{"image/*"}},
		},
	})
	// AnalyseFile は body から mime を検出する。"hello" は text/plain になる
	// ので image/* allowlist に match しない。
	user := &model.User{ID: "u1"}
	_, err := svc.Upload(context.Background(), drive.UploadInput{
		User: user, Body: []byte("hello text body content"), Name: "x.txt",
	})
	require.ErrorIs(t, err, drive.ErrUnallowedFileType)
}

// uploadableFileTypes に wildcard ("*") があれば全許可。
func TestUpload_UploadableFileTypesWildcardAllows(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetRoleChecker(&fakeMod{
		policies: map[string]map[string]any{
			"u1": {"uploadableFileTypes": []string{"*"}},
		},
	})
	user := &model.User{ID: "u1"}
	_, err := svc.Upload(context.Background(), drive.UploadInput{
		User: user, Body: []byte("hello"), Name: "x.txt",
	})
	require.NoError(t, err)
}

// moderator は uploadableFileTypes allowlist を bypass する。
func TestUpload_ModeratorBypassesUploadableFileTypes(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetRoleChecker(&fakeMod{
		moderators: map[string]bool{"u1": true},
		policies: map[string]map[string]any{
			"u1": {"uploadableFileTypes": []string{"image/*"}},
		},
	})
	user := &model.User{ID: "u1"}
	_, err := svc.Upload(context.Background(), drive.UploadInput{
		User: user, Body: []byte("hello text body"), Name: "x.txt",
	})
	require.NoError(t, err, "moderator は allowlist mismatch でも通る")
}

// policies map が nil (= test fixture / wire 未配線) のときは gate skip。
func TestUpload_RoleCheckerNoPoliciesSkipsGate(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetRoleChecker(&fakeMod{}) // policies nil
	user := &model.User{ID: "u1"}
	_, err := svc.Upload(context.Background(), drive.UploadInput{
		User: user, Body: []byte("anything"), Name: "x.txt",
	})
	require.NoError(t, err)
}

// --- #1029 PR-2: maxFileSizeMb / driveCapacityMb gate ---

// maxFileSizeMb 超過の upload は ErrMaxFileSizeExceeded で reject される。
func TestUpload_MaxFileSizeExceeded(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetRoleChecker(&fakeMod{
		policies: map[string]map[string]any{
			"u1": {"maxFileSizeMb": 1}, // = 1MB cap
		},
	})
	user := &model.User{ID: "u1"} // Host == nil = local user
	_, err := svc.Upload(context.Background(), drive.UploadInput{
		User: user, Body: make([]byte, 2*1024*1024), Name: "x.bin",
	})
	require.ErrorIs(t, err, drive.ErrMaxFileSizeExceeded)
}

func TestUpload_MaxFileSizePolicySaturatesPositive(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetRoleChecker(&fakeMod{policies: map[string]map[string]any{
		"u1": {"maxFileSizeMb": math.MaxFloat64},
	}})
	_, err := svc.Upload(context.Background(), drive.UploadInput{
		User: &model.User{ID: "u1"}, Body: []byte("x"), Name: "x.txt",
	})
	require.NoError(t, err)
}

// driveCapacityMb 超過 (= 既存 usage + 新 file > capacity) は ErrNoFreeSpace。
func TestUpload_NoFreeSpace(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	owner := "u1"
	// 既に 1MB 使用中、capacity 1MB → 新規 upload はサイズ不問で reject。
	fileRepo.Files["existing"] = &model.DriveFile{ID: "existing", UserID: &owner, Size: 1024 * 1024}
	svc.SetRoleChecker(&fakeMod{
		policies: map[string]map[string]any{
			"u1": {"driveCapacityMb": 1, "maxFileSizeMb": 100}, // size gate は余裕、capacity が tight
		},
	})
	user := &model.User{ID: "u1"}
	_, err := svc.Upload(context.Background(), drive.UploadInput{
		User: user, Body: []byte("hello world data text"), Name: "x.txt",
	})
	require.ErrorIs(t, err, drive.ErrNoFreeSpace)
}

func TestUpload_UsageAdditionCannotOverflowCapacityGate(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	owner := "u1"
	fileRepo.Files["existing"] = &model.DriveFile{ID: "existing", UserID: &owner, Size: math.MaxInt}
	svc.SetRoleChecker(&fakeMod{policies: map[string]map[string]any{
		"u1": {"driveCapacityMb": 1},
	}})
	_, err := svc.Upload(context.Background(), drive.UploadInput{
		User: &model.User{ID: "u1"}, Body: []byte("x"), Name: "x.txt",
	})
	require.ErrorIs(t, err, drive.ErrNoFreeSpace)
}

// remote user は本 gate を skip する (= mk-go に expireOldFile が未実装な為)。
func TestUpload_RemoteUserBypassesCapacityGates(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetRoleChecker(&fakeMod{
		policies: map[string]map[string]any{
			"u1": {"maxFileSizeMb": 1, "driveCapacityMb": 1},
		},
	})
	remoteHost := "remote.example"
	user := &model.User{ID: "u1", Host: &remoteHost}
	_, err := svc.Upload(context.Background(), drive.UploadInput{
		User: user, Body: make([]byte, 2*1024*1024), Name: "x.bin",
	})
	require.NoError(t, err, "remote user は drive policy gate を skip する")
}

// limit 内 (= size <= maxFileSizeMb かつ usage+size <= driveCapacityMb) なら通る。
func TestUpload_DriveLimitsPassUnderThreshold(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetRoleChecker(&fakeMod{
		policies: map[string]map[string]any{
			"u1": {"maxFileSizeMb": 1, "driveCapacityMb": 1},
		},
	})
	user := &model.User{ID: "u1"}
	_, err := svc.Upload(context.Background(), drive.UploadInput{
		User: user, Body: []byte("small body"), Name: "x.txt",
	})
	require.NoError(t, err)
}

// maxFileSizeMb / driveCapacityMb が 0 (= unset) なら gate skip。upstream
// `if (policies.maxFileSizeMb)` で 0 falsy = 無制限。
func TestUpload_DriveLimitsZeroSkipsGate(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetRoleChecker(&fakeMod{
		policies: map[string]map[string]any{
			"u1": {"maxFileSizeMb": 0, "driveCapacityMb": 0},
		},
	})
	user := &model.User{ID: "u1"}
	_, err := svc.Upload(context.Background(), drive.UploadInput{
		User: user, Body: make([]byte, 2*1024*1024), Name: "x.bin",
	})
	require.NoError(t, err, "policy 0 は upstream falsy 相当で gate skip")
}

// usageErrRepo は UsageByUser だけ error を返す stub。driveCapacityMb gate
// で DB error が transient に起きた場合、usage=0 で握り潰さず internal
// error として伝播することを確認する (#1041 review feedback)。
type usageErrRepo struct {
	*testutil.MockDriveFileRepository
}

func (r *usageErrRepo) UsageByUser(_ string) (int64, error) {
	return 0, errors.New("transient db error")
}

func TestUpload_DriveCapacityUsageErrorPropagates(t *testing.T) {
	fileRepo := &usageErrRepo{MockDriveFileRepository: testutil.NewMockDriveFileRepository()}
	storage := drive.NewLocalStorage(t.TempDir(), "")
	idGen, _ := id.NewGenerator("aidx")
	svc := drive.NewService(fileRepo, testutil.NewMockDriveFolderRepository(), storage, idGen)
	svc.SetRoleChecker(&fakeMod{
		policies: map[string]map[string]any{
			"u1": {"driveCapacityMb": 1},
		},
	})
	user := &model.User{ID: "u1"}
	_, err := svc.Upload(context.Background(), drive.UploadInput{
		User: user, Body: []byte("hello"), Name: "x.txt",
	})
	require.Error(t, err)
	// wrap で原因を保持していることを確認 (handler 側は default 500 経路に流す)
	assert.Contains(t, err.Error(), "calc drive usage")
	// 同 error は ErrNoFreeSpace でも MaxFileSize でもないので errors.Is で識別不能
	assert.False(t, errors.Is(err, drive.ErrNoFreeSpace))
}

// system file (User == nil) は role policy gate の対象外 (= emoji copy 等)。
func TestUpload_SystemFileBypassesPolicy(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetRoleChecker(&fakeMod{
		policies: map[string]map[string]any{
			"u1": {"uploadableFileTypes": []string{"image/*"}},
		},
	})
	// User == nil は system file 経路、policy 対象外。
	_, err := svc.Upload(context.Background(), drive.UploadInput{
		Body: []byte("anything"), Name: "x.txt",
	})
	require.NoError(t, err)
}

// alwaysMarkNsfw=true の user は自分の file の isSensitive=false 化を拒否される。
func TestUpdate_AlwaysMarkNsfwBlocksUnsensitive(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	owner := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &owner, IsSensitive: true}
	svc.SetRoleChecker(&fakeMod{
		policies: map[string]map[string]any{
			"u1": {"alwaysMarkNsfw": true},
		},
	})

	notSensitive := false
	_, err := svc.Update(&model.User{ID: "u1"}, "f1", drive.UpdateInput{IsSensitive: &notSensitive})
	require.ErrorIs(t, err, drive.ErrCannotUnmarkSensitive)
}

// alwaysMarkNsfw=true でも sensitive=true への変更は通常通り許可される。
// gate 対象は sensitive=false への "clear" のみで、sensitive=true 化や既存
// sensitive 維持は gate 対象外 (= NSFW marker を強化する方向への変更は妨げない)。
func TestUpdate_AlwaysMarkNsfwAllowsSettingSensitive(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	owner := "u1"
	// sensitive=false の file を sensitive=true に上げる経路 (= NSFW 強化)。
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &owner, IsSensitive: false}
	svc.SetRoleChecker(&fakeMod{
		policies: map[string]map[string]any{
			"u1": {"alwaysMarkNsfw": true},
		},
	})

	sensitive := true
	_, err := svc.Update(&model.User{ID: "u1"}, "f1", drive.UpdateInput{IsSensitive: &sensitive})
	require.NoError(t, err)
}

// alwaysMarkNsfw=true で既に sensitive な file の sensitive=true 維持も通常通り。
// gate condition (`!*in.IsSensitive && f.IsSensitive`) が false なので通過する
// regression guard (= "sensitive→sensitive の noop でも 403 で死ぬ" を防ぐ)。
func TestUpdate_AlwaysMarkNsfwAllowsKeepingSensitive(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	owner := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &owner, IsSensitive: true}
	svc.SetRoleChecker(&fakeMod{
		policies: map[string]map[string]any{
			"u1": {"alwaysMarkNsfw": true},
		},
	})

	sensitive := true
	_, err := svc.Update(&model.User{ID: "u1"}, "f1", drive.UpdateInput{IsSensitive: &sensitive})
	require.NoError(t, err)
}

// policy false の user は通常通り isSensitive を clear できる。
func TestUpdate_NoAlwaysMarkNsfwAllowsUnsensitive(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	owner := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &owner, IsSensitive: true}
	svc.SetRoleChecker(&fakeMod{}) // policies nil = gate skip

	notSensitive := false
	_, err := svc.Update(&model.User{ID: "u1"}, "f1", drive.UpdateInput{IsSensitive: &notSensitive})
	require.NoError(t, err)
}

// mimeAllowedByPolicy / normalizeMimePatterns の direct unit test。pattern
// マッチング (wildcard / prefix / exact / empty / 型不一致) を保証する。
func TestMimeAllowedByPolicy_PatternMatching(t *testing.T) {
	tests := []struct {
		name    string
		mime    string
		raw     any
		allowed bool
	}{
		{"wildcard *", "image/png", []string{"*"}, true},
		{"wildcard */*", "video/mp4", []string{"*/*"}, true},
		{"prefix image/* matches", "image/jpeg", []string{"image/*"}, true},
		{"prefix image/* mismatches", "video/mp4", []string{"image/*"}, false},
		{"exact match", "application/json", []string{"application/json"}, true},
		{"no match", "application/zip", []string{"image/*", "text/*"}, false},
		{"empty string entries are skipped", "image/png", []string{"", "image/*"}, true},
		{"nil policy is fail-soft allow", "image/png", nil, true},
		{"empty list is fail-soft allow", "image/png", []string{}, true},
		{"[]any from JSON unmarshal", "image/png", []any{"image/*"}, true},
		{"[]any with non-string entries skipped", "image/png", []any{42, "image/*"}, true},
		{"wrong type fail-soft allow", "image/png", "image/*", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.allowed, drive.MimeAllowedByPolicyForTest(tc.mime, tc.raw))
		})
	}
}

func TestUpdate_FolderNotFound(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	uid := "u1"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid}
	folderID := "ghost"
	folderPtr := &folderID
	_, err := svc.Update(&model.User{ID: "u1"}, "f1", drive.UpdateInput{FolderID: &folderPtr})
	require.ErrorIs(t, err, drive.ErrFolderNotFound)
}

// 他人所有の宛先 folder は upstream files/update 同様「不在」扱いで
// ErrFolderNotFound (→ NO_SUCH_FOLDER ea8fb7a5)、ACCESS_DENIED にしない (#1908)。
func TestUpdate_OtherOwnerFolderNotFound(t *testing.T) {
	svc, fileRepo, folderRepo := newSvc(t)
	uid := "u1"
	other := "other"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid}
	folderRepo.Folders["fid"] = &model.DriveFolder{ID: "fid", UserID: &other}

	folderID := "fid"
	folderPtr := &folderID
	_, err := svc.Update(&model.User{ID: "u1"}, "f1", drive.UpdateInput{FolderID: &folderPtr})
	require.ErrorIs(t, err, drive.ErrFolderNotFound)
}

func TestUpdate_FolderNullable(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	uid := "u1"
	folderID := "x"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, FolderID: &folderID}

	var nilFolder *string
	_, err := svc.Update(&model.User{ID: "u1"}, "f1", drive.UpdateInput{FolderID: &nilFolder})
	require.NoError(t, err)
}

// failingUpdateFileRepo causes Update to fail.
type failingUpdateFileRepo struct {
	*testutil.MockDriveFileRepository
}

func (f *failingUpdateFileRepo) Update(_ string, _ map[string]any) error {
	return errStub
}

func TestUpdate_RepoUpdateError(t *testing.T) {
	mock := testutil.NewMockDriveFileRepository()
	uid := "u1"
	mock.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid}
	repo := &failingUpdateFileRepo{MockDriveFileRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := drive.NewService(repo, testutil.NewMockDriveFolderRepository(), drive.NewLocalStorage(t.TempDir(), ""), idGen)
	newName := "x"
	_, err := svc.Update(&model.User{ID: "u1"}, "f1", drive.UpdateInput{Name: &newName})
	assert.ErrorIs(t, err, errStub)
}

// --- Delete ---

func TestDelete_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	err := svc.Delete(&model.User{ID: "u1"}, "ghost")
	require.ErrorIs(t, err, drive.ErrFileNotFound)
}

func TestDelete_HappyPath(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	user := &model.User{ID: "u1"}
	created, err := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: []byte("x"), Name: "x"})
	require.NoError(t, err)
	require.NoError(t, svc.Delete(user, created.ID))
	assert.Empty(t, fileRepo.Files)
}

func TestDelete_StorageError(t *testing.T) {
	storage := brokenStorage(t)
	mock := testutil.NewMockDriveFileRepository()
	uid := "u1"
	key := "abc"
	mock.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, AccessKey: &key}
	idGen, _ := id.NewGenerator("aidx")
	svc := drive.NewService(mock, testutil.NewMockDriveFolderRepository(), storage, idGen)
	err := svc.Delete(&model.User{ID: "u1"}, "f1")
	assert.Error(t, err)
}

// --- Folders ---

func TestCreateFolder_NilUser(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.CreateFolder(nil, "x", nil)
	assert.Error(t, err)
}

func TestCreateFolder_HappyPath(t *testing.T) {
	svc, _, folderRepo := newSvc(t)
	f, err := svc.CreateFolder(&model.User{ID: "u1"}, "Hello", nil)
	require.NoError(t, err)
	assert.Equal(t, "Hello", f.Name)
	assert.Len(t, folderRepo.Folders, 1)
}

func TestCreateFolder_ParentNotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	pid := "ghost"
	_, err := svc.CreateFolder(&model.User{ID: "u1"}, "x", &pid)
	require.ErrorIs(t, err, drive.ErrFolderNotFound)
}

// 他人所有 parent は upstream folders/create 同様「不在」扱いで
// ErrFolderNotFound (=> NO_SUCH_FOLDER)、ACCESS_DENIED にしない (#1831)。
func TestCreateFolder_ParentOtherOwnerNotFound(t *testing.T) {
	svc, _, folderRepo := newSvc(t)
	other := "other"
	folderRepo.Folders["p"] = &model.DriveFolder{ID: "p", UserID: &other}
	pid := "p"
	_, err := svc.CreateFolder(&model.User{ID: "u1"}, "x", &pid)
	require.ErrorIs(t, err, drive.ErrFolderNotFound)
}

// failingCreateFolderRepo causes Create to fail.
type failingCreateFolderRepo struct {
	*testutil.MockDriveFolderRepository
}

func (f *failingCreateFolderRepo) Create(_ *model.DriveFolder) error {
	return errStub
}

func TestCreateFolder_RepoError(t *testing.T) {
	mock := testutil.NewMockDriveFolderRepository()
	repo := &failingCreateFolderRepo{MockDriveFolderRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := drive.NewService(testutil.NewMockDriveFileRepository(), repo, drive.NewLocalStorage(t.TempDir(), ""), idGen)
	_, err := svc.CreateFolder(&model.User{ID: "u1"}, "x", nil)
	assert.ErrorIs(t, err, errStub)
}

func TestShowFolder_NilUser(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.ShowFolder(nil, "x")
	assert.Error(t, err)
}

func TestShowFolder_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.ShowFolder(&model.User{ID: "u1"}, "ghost")
	require.ErrorIs(t, err, drive.ErrFolderNotFound)
}

// 他人所有 folder は upstream folders/{show,update,delete} 同様「不在」扱いで
// ErrFolderNotFound (=> 404 NO_SUCH_FOLDER)。owner mismatch を 403 にすると
// folder ID 存在 oracle になる (#1831)。
func TestShowFolder_OtherOwnerNotFound(t *testing.T) {
	svc, _, folderRepo := newSvc(t)
	other := "other"
	folderRepo.Folders["p"] = &model.DriveFolder{ID: "p", UserID: &other}
	_, err := svc.ShowFolder(&model.User{ID: "u1"}, "p")
	require.ErrorIs(t, err, drive.ErrFolderNotFound)
}

func TestUpdateFolder_HappyPath(t *testing.T) {
	svc, _, folderRepo := newSvc(t)
	uid := "u1"
	folderRepo.Folders["p"] = &model.DriveFolder{ID: "p", Name: "Old", UserID: &uid}

	newName := "New"
	got, err := svc.UpdateFolder(&model.User{ID: "u1"}, "p", drive.UpdateFolderInput{Name: &newName})
	require.NoError(t, err)
	assert.Equal(t, "New", got.Name)
}

func TestUpdateFolder_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, err := svc.UpdateFolder(&model.User{ID: "u1"}, "ghost", drive.UpdateFolderInput{})
	require.ErrorIs(t, err, drive.ErrFolderNotFound)
}

func TestUpdateFolder_ParentNotFound(t *testing.T) {
	svc, _, folderRepo := newSvc(t)
	uid := "u1"
	folderRepo.Folders["c"] = &model.DriveFolder{ID: "c", UserID: &uid}
	pid := "ghost"
	pidPtr := &pid
	_, err := svc.UpdateFolder(&model.User{ID: "u1"}, "c", drive.UpdateFolderInput{ParentID: &pidPtr})
	// #977: target 不在の ErrFolderNotFound と区別する distinct error。
	require.ErrorIs(t, err, drive.ErrParentFolderNotFound)
}

// 他人所有 parent は upstream folders/update 同様「不在」扱いで
// ErrParentFolderNotFound (=> NO_SUCH_PARENT_FOLDER)、ACCESS_DENIED にしない
// (#1831、update.ts は parent を findOneBy({id,userId}) で引き noSuchParentFolder)。
func TestUpdateFolder_ParentOtherOwnerNotFound(t *testing.T) {
	svc, _, folderRepo := newSvc(t)
	uid := "u1"
	other := "other"
	folderRepo.Folders["c"] = &model.DriveFolder{ID: "c", UserID: &uid}
	folderRepo.Folders["p"] = &model.DriveFolder{ID: "p", UserID: &other}
	pid := "p"
	pidPtr := &pid
	_, err := svc.UpdateFolder(&model.User{ID: "u1"}, "c", drive.UpdateFolderInput{ParentID: &pidPtr})
	require.ErrorIs(t, err, drive.ErrParentFolderNotFound)
}

func TestUpdateFolder_NullParent(t *testing.T) {
	svc, _, folderRepo := newSvc(t)
	uid := "u1"
	pid := "p"
	folderRepo.Folders["c"] = &model.DriveFolder{ID: "c", UserID: &uid, ParentID: &pid}

	var nilParent *string
	_, err := svc.UpdateFolder(&model.User{ID: "u1"}, "c", drive.UpdateFolderInput{ParentID: &nilParent})
	require.NoError(t, err)
}

type failingUpdateFolderRepo struct {
	*testutil.MockDriveFolderRepository
}

func (f *failingUpdateFolderRepo) Update(_ string, _ map[string]any) error {
	return errStub
}

func TestUpdateFolder_RepoError(t *testing.T) {
	mock := testutil.NewMockDriveFolderRepository()
	uid := "u1"
	mock.Folders["c"] = &model.DriveFolder{ID: "c", UserID: &uid}
	repo := &failingUpdateFolderRepo{MockDriveFolderRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := drive.NewService(testutil.NewMockDriveFileRepository(), repo, drive.NewLocalStorage(t.TempDir(), ""), idGen)
	newName := "x"
	_, err := svc.UpdateFolder(&model.User{ID: "u1"}, "c", drive.UpdateFolderInput{Name: &newName})
	assert.ErrorIs(t, err, errStub)
}

func TestDeleteFolder_NotFound(t *testing.T) {
	svc, _, _ := newSvc(t)
	err := svc.DeleteFolder(&model.User{ID: "u1"}, "ghost")
	require.ErrorIs(t, err, drive.ErrFolderNotFound)
}

func TestDeleteFolder_NotEmpty(t *testing.T) {
	svc, fileRepo, folderRepo := newSvc(t)
	uid := "u1"
	folderRepo.Folders["p"] = &model.DriveFolder{ID: "p", UserID: &uid}
	pid := "p"
	fileRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, FolderID: &pid}
	err := svc.DeleteFolder(&model.User{ID: "u1"}, "p")
	require.ErrorIs(t, err, drive.ErrFolderNotEmpty)
}

func TestDeleteFolder_HappyPath(t *testing.T) {
	svc, _, folderRepo := newSvc(t)
	uid := "u1"
	folderRepo.Folders["p"] = &model.DriveFolder{ID: "p", UserID: &uid}
	require.NoError(t, svc.DeleteFolder(&model.User{ID: "u1"}, "p"))
	assert.Empty(t, folderRepo.Folders)
}

type failingHasChildrenRepo struct {
	*testutil.MockDriveFolderRepository
}

func (f *failingHasChildrenRepo) HasChildren(_ string) (bool, error) {
	return false, errStub
}

func TestDeleteFolder_HasChildrenError(t *testing.T) {
	mock := testutil.NewMockDriveFolderRepository()
	uid := "u1"
	mock.Folders["p"] = &model.DriveFolder{ID: "p", UserID: &uid}
	repo := &failingHasChildrenRepo{MockDriveFolderRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := drive.NewService(testutil.NewMockDriveFileRepository(), repo, drive.NewLocalStorage(t.TempDir(), ""), idGen)
	err := svc.DeleteFolder(&model.User{ID: "u1"}, "p")
	assert.ErrorIs(t, err, errStub)
}

// --- Streaming publisher hooks (Step K-7) ----------------------------------

// stubDriveStreamPublisher records every PublishDriveEvent call.
type stubDriveStreamPublisher struct {
	events []string // "<userID>:<eventType>"
	bodies []any    // body passed for each call (#2002)
}

func (s *stubDriveStreamPublisher) PublishDriveEvent(userID, eventType string, body any) {
	s.events = append(s.events, userID+":"+eventType)
	s.bodies = append(s.bodies, body)
}

func TestUpload_PublishesStreamingEvent(t *testing.T) {
	svc, _, _ := newSvc(t)
	pub := &stubDriveStreamPublisher{}
	svc.SetStreamingPublisher(pub)
	user := &model.User{ID: "u1"}
	_, err := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: []byte("hi"), Name: "x.txt"})
	require.NoError(t, err)
	assert.Equal(t, []string{"u1:fileCreated"}, pub.events)
	// #2002: fileCreated は packed DriveFileEntity (self) を送る (raw *model.DriveFile ではない)。
	require.Len(t, pub.bodies, 1)
	_, ok := pub.bodies[0].(entity.DriveFileEntity)
	assert.True(t, ok, "fileCreated body は DriveFileEntity (#2002)")
}

func TestUpdate_PublishesStreamingEvent(t *testing.T) {
	svc, _, _ := newSvc(t)
	user := &model.User{ID: "u1"}
	f, err := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: []byte("hi"), Name: "x.txt"})
	require.NoError(t, err)

	pub := &stubDriveStreamPublisher{}
	svc.SetStreamingPublisher(pub)
	name := "renamed.txt"
	_, err = svc.Update(user, f.ID, drive.UpdateInput{Name: &name})
	require.NoError(t, err)
	assert.Equal(t, []string{"u1:fileUpdated"}, pub.events)
	// #2002: fileUpdated も packed DriveFileEntity (self)。
	require.Len(t, pub.bodies, 1)
	_, ok := pub.bodies[0].(entity.DriveFileEntity)
	assert.True(t, ok, "fileUpdated body は DriveFileEntity (#2002)")
}

func TestDelete_PublishesStreamingEvent(t *testing.T) {
	svc, _, _ := newSvc(t)
	user := &model.User{ID: "u1"}
	f, err := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: []byte("hi"), Name: "x.txt"})
	require.NoError(t, err)

	pub := &stubDriveStreamPublisher{}
	svc.SetStreamingPublisher(pub)
	require.NoError(t, svc.Delete(user, f.ID))
	assert.Equal(t, []string{"u1:fileDeleted"}, pub.events)
	// #2002: fileDeleted は file id 文字列のみ (upstream publishDriveStream(_, 'fileDeleted', file.id))。
	require.Len(t, pub.bodies, 1)
	assert.Equal(t, f.ID, pub.bodies[0], "fileDeleted body は file id 文字列 (#2002)")
}

// stubChartHook captures chart hook fires from the drive service.
type stubChartHook struct {
	uploaded []string // file ids
	deleted  []string
}

func (s *stubChartHook) OnFileUploaded(f *model.DriveFile) { s.uploaded = append(s.uploaded, f.ID) }
func (s *stubChartHook) OnFileDeleted(f *model.DriveFile)  { s.deleted = append(s.deleted, f.ID) }

func TestUpload_FiresChartHook(t *testing.T) {
	svc, _, _ := newSvc(t)
	hook := &stubChartHook{}
	svc.SetChartHook(hook)
	user := &model.User{ID: "u1"}
	f, err := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: []byte("hi"), Name: "x.txt"})
	require.NoError(t, err)
	assert.Equal(t, []string{f.ID}, hook.uploaded)
}

func TestDelete_FiresChartHook(t *testing.T) {
	svc, _, _ := newSvc(t)
	user := &model.User{ID: "u1"}
	f, err := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: []byte("hi"), Name: "x.txt"})
	require.NoError(t, err)

	hook := &stubChartHook{}
	svc.SetChartHook(hook)
	require.NoError(t, svc.Delete(user, f.ID))
	assert.Equal(t, []string{f.ID}, hook.deleted)
}

// failingFindByIDRepo wraps the mock to make FindByID fail when called the
// second time (after the initial create succeeds in Update). 1 回目の
// findOwnedFile() (#499 で Show() から差し替え済) は成功させ、2 回目
// (Update 後の reload) で失敗させる。
type failingFindByIDRepo struct {
	*testutil.MockDriveFileRepository
	calls int
}

func (r *failingFindByIDRepo) FindByID(id string) (*model.DriveFile, error) {
	r.calls++
	if r.calls > 1 {
		return nil, errStub
	}
	return r.MockDriveFileRepository.FindByID(id)
}

func TestUpdate_FindByIDReloadError(t *testing.T) {
	mock := testutil.NewMockDriveFileRepository()
	uid := "u1"
	mock.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, Name: "x"}
	repo := &failingFindByIDRepo{MockDriveFileRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := drive.NewService(repo, testutil.NewMockDriveFolderRepository(), drive.NewLocalStorage(t.TempDir(), ""), idGen)
	name := "renamed"
	_, err := svc.Update(&model.User{ID: "u1"}, "f1", drive.UpdateInput{Name: &name})
	assert.ErrorIs(t, err, errStub)
}

// failingDeleteFileRepo causes fileRepo.Delete to fail.
type failingDeleteFileRepo struct {
	*testutil.MockDriveFileRepository
}

func (r *failingDeleteFileRepo) Delete(_ *model.DriveFile) error { return errStub }

func TestDelete_FileRepoDeleteError(t *testing.T) {
	mock := testutil.NewMockDriveFileRepository()
	uid := "u1"
	mock.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &uid, Name: "x"}
	repo := &failingDeleteFileRepo{MockDriveFileRepository: mock}
	idGen, _ := id.NewGenerator("aidx")
	svc := drive.NewService(repo, testutil.NewMockDriveFolderRepository(), drive.NewLocalStorage(t.TempDir(), ""), idGen)
	err := svc.Delete(&model.User{ID: "u1"}, "f1")
	assert.ErrorIs(t, err, errStub)
}

// ---------------------------------------------------------------------------
// Image processing integration tests (Step R)
// ---------------------------------------------------------------------------

// testJPEGBytes creates a minimal JPEG image for testing.
func testJPEGBytes(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x * 50), uint8(y * 50), 100, 255})
		}
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
	return buf.Bytes()
}

// testPNGBytes creates a minimal PNG image for testing.
func testPNGBytes(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x * 30), uint8(y * 30), 200, 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func TestUpload_WithImageProcessor_JPEG(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	svc.SetImageProcessor(drive.NewDefaultImageProcessor())

	user := &model.User{ID: "u1"}
	jpegData := testJPEGBytes(800, 600)
	f, err := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: jpegData, Name: "photo.jpg"})
	require.NoError(t, err)
	assert.Len(t, fileRepo.Files, 1)

	// サムネイルが生成されている
	assert.NotNil(t, f.ThumbnailURL)
	assert.NotNil(t, f.ThumbnailAccessKey)

	// BlurHashが設定されている
	assert.NotNil(t, f.Blurhash)
	assert.NotEmpty(t, *f.Blurhash)

	// Propertiesに寸法が含まれている
	assert.NotEmpty(t, f.Properties)
	var props map[string]int
	err = json.Unmarshal(f.Properties, &props)
	require.NoError(t, err)
	assert.Equal(t, 800, props["width"])
	assert.Equal(t, 600, props["height"])
}

func TestUpload_WithImageProcessor_PNG_LargeWebpublic(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetImageProcessor(drive.NewDefaultImageProcessor())

	user := &model.User{ID: "u1"}
	// 2048超えの PNG → webpublic 生成
	pngData := testPNGBytes(3000, 2000)
	f, err := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: pngData, Name: "big.png"})
	require.NoError(t, err)

	assert.NotNil(t, f.ThumbnailURL)
	assert.NotNil(t, f.WebpublicURL)
	assert.NotNil(t, f.WebpublicType)
	assert.Equal(t, "image/png", *f.WebpublicType) // PNG は PNG で出力
}

func TestUpload_WithImageProcessor_SmallJPEG_NoWebpublic(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetImageProcessor(drive.NewDefaultImageProcessor())

	user := &model.User{ID: "u1"}
	// 2048以下 + EXIF なし → webpublic 不要
	jpegData := testJPEGBytes(400, 300)
	f, err := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: jpegData, Name: "small.jpg"})
	require.NoError(t, err)

	assert.NotNil(t, f.ThumbnailURL) // サムネイルは生成
	assert.Nil(t, f.WebpublicURL)    // webpublic は不要
}

func TestUpload_WithoutImageProcessor_TextFile(t *testing.T) {
	svc, _, _ := newSvc(t)
	// ImageProcessor 未設定

	user := &model.User{ID: "u1"}
	f, err := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: []byte("hello"), Name: "test.txt"})
	require.NoError(t, err)

	assert.Nil(t, f.ThumbnailURL)
	assert.Nil(t, f.WebpublicURL)
	assert.Nil(t, f.Blurhash)
}

func TestUpload_WithImageProcessor_NonImage(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetImageProcessor(drive.NewDefaultImageProcessor())

	user := &model.User{ID: "u1"}
	f, err := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: []byte("not an image"), Name: "data.bin"})
	require.NoError(t, err)

	// テキストは画像処理対象外
	assert.Nil(t, f.ThumbnailURL)
	assert.Nil(t, f.Blurhash)
}

// stubVideoProcessor always returns a fake thumbnail.
type stubVideoProcessor struct {
	thumbnail *drive.ProcessedImage
}

func (s *stubVideoProcessor) GenerateThumbnail(_ context.Context, _ []byte, _ string) (*drive.ProcessedImage, error) {
	return s.thumbnail, nil
}

func TestUpload_WithVideoProcessor(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetVideoProcessor(&stubVideoProcessor{
		thumbnail: &drive.ProcessedImage{
			Data:     []byte("fake-thumb"),
			MimeType: "image/webp",
		},
	})

	user := &model.User{ID: "u1"}
	// video/mp4 のMIMEを強制するため、バイナリに MP4 ヘッダを付与
	mp4Header := []byte{0x00, 0x00, 0x00, 0x20, 0x66, 0x74, 0x79, 0x70} // ftyp box
	f, err := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: mp4Header, Name: "clip.mp4"})
	require.NoError(t, err)

	// http.DetectContentType が video/mp4 を返す場合のみサムネイル生成
	// (返さない場合は application/octet-stream になるのでサムネイルなし)
	if f.Type == "video/mp4" {
		assert.NotNil(t, f.ThumbnailURL)
	}
}

func TestDelete_CleansUpThumbnailAndWebpublic(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	svc.SetImageProcessor(drive.NewDefaultImageProcessor())

	user := &model.User{ID: "u1"}
	// 2048 超えの JPEG → thumbnail + webpublic 生成
	jpegData := testJPEGBytes(3000, 2000)
	f, err := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: jpegData, Name: "large.jpg"})
	require.NoError(t, err)
	require.NotNil(t, f.ThumbnailAccessKey)
	require.NotNil(t, f.WebpublicAccessKey)

	// 削除前のファイル数を確認
	assert.Len(t, fileRepo.Files, 1)

	// 削除
	require.NoError(t, svc.Delete(user, f.ID))
	assert.Empty(t, fileRepo.Files)
}

func TestGenerateAlts_VideoPath(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetVideoProcessor(&stubVideoProcessor{
		thumbnail: &drive.ProcessedImage{Data: []byte("thumb"), MimeType: "image/webp"},
	})
	thumb, wp, bh := svc.GenerateAltsForTest([]byte("video"), "video/mp4")
	assert.NotNil(t, thumb)
	assert.Nil(t, wp)
	assert.Nil(t, bh)
}

func TestGenerateAlts_VideoPath_NilResult(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetVideoProcessor(&stubVideoProcessor{thumbnail: nil})
	thumb, _, _ := svc.GenerateAltsForTest([]byte("video"), "video/mp4")
	assert.Nil(t, thumb)
}

func TestGenerateAlts_NoProcessors(t *testing.T) {
	svc, _, _ := newSvc(t)
	thumb, wp, bh := svc.GenerateAltsForTest([]byte("data"), "image/jpeg")
	assert.Nil(t, thumb)
	assert.Nil(t, wp)
	assert.Nil(t, bh)
}

func TestGenerateAlts_NonImageNonVideo(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetImageProcessor(drive.NewDefaultImageProcessor())
	svc.SetVideoProcessor(&stubVideoProcessor{})
	thumb, wp, bh := svc.GenerateAltsForTest([]byte("text"), "text/plain")
	assert.Nil(t, thumb)
	assert.Nil(t, wp)
	assert.Nil(t, bh)
}

func TestUpload_ImageProcessor_RepoCreateErrorRollback(t *testing.T) {
	repo := &failingFileRepo{MockDriveFileRepository: testutil.NewMockDriveFileRepository()}
	idGen, _ := id.NewGenerator("aidx")
	tmpDir := t.TempDir()
	storage := drive.NewLocalStorage(tmpDir, "https://example.com/files")
	svc := drive.NewService(repo, testutil.NewMockDriveFolderRepository(), storage, idGen)
	svc.SetImageProcessor(drive.NewDefaultImageProcessor())

	user := &model.User{ID: "u1"}
	// 2048超え → thumbnail + webpublic 両方生成 → Create失敗 → 全ロールバック
	jpegData := testJPEGBytes(3000, 2000)
	_, err := svc.Upload(context.Background(), drive.UploadInput{User: user, Body: jpegData, Name: "photo.jpg"})
	assert.Error(t, err)

	// storage のファイルがロールバックされていることを確認
	entries, _ := os.ReadDir(tmpDir)
	assert.Empty(t, entries)
}

// #1772: DeleteAllByHost は対象ホストの file を row 削除し、usage chart を per-file
// で減算する (local / 他ホストは残す)。
func TestDeleteAllByHost(t *testing.T) {
	svc, fileRepo, _ := newSvc(t)
	hook := &stubChartHook{}
	svc.SetChartHook(hook)
	host := "remote.example"
	other := "other.example"
	require.NoError(t, fileRepo.Create(&model.DriveFile{ID: "rf1", UserHost: &host}))
	require.NoError(t, fileRepo.Create(&model.DriveFile{ID: "rf2", UserHost: &host}))
	require.NoError(t, fileRepo.Create(&model.DriveFile{ID: "lf"}))                   // local (UserHost nil)
	require.NoError(t, fileRepo.Create(&model.DriveFile{ID: "of", UserHost: &other})) // 他ホスト

	n, err := svc.DeleteAllByHost(host)
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
	assert.NotContains(t, fileRepo.Files, "rf1")
	assert.NotContains(t, fileRepo.Files, "rf2")
	assert.Contains(t, fileRepo.Files, "lf", "local file は残る")
	assert.Contains(t, fileRepo.Files, "of", "他ホスト file は残る")
	assert.ElementsMatch(t, []string{"rf1", "rf2"}, hook.deleted, "usage chart を per-file で減算")
}

func TestDeleteAllByHost_EmptyHost(t *testing.T) {
	svc, _, _ := newSvc(t)
	n, err := svc.DeleteAllByHost("")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
}

// fakeObjectStorage stands in for a non-local backend. 実 S3 client は
// テストから叩けないので、「ローカル FS ではない Storage」であることだけを
// 満たす最小実装を使う。
type fakeObjectStorage struct {
	objects map[string][]byte
	deleted []string
}

func newFakeObjectStorage() *fakeObjectStorage {
	return &fakeObjectStorage{objects: map[string][]byte{}}
}

func (f *fakeObjectStorage) Put(accessKey string, body io.Reader) (string, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	f.objects[accessKey] = b
	return "https://cdn.example.test/" + accessKey, nil
}

func (f *fakeObjectStorage) Get(accessKey string) (io.ReadCloser, error) {
	b, ok := f.objects[accessKey]
	if !ok {
		return nil, drive.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *fakeObjectStorage) Delete(accessKey string) error {
	delete(f.objects, accessKey)
	f.deleted = append(f.deleted, accessKey)
	return nil
}

func newSvcWithStorage(t *testing.T, st drive.Storage) (*drive.Service, *testutil.MockDriveFileRepository) {
	t.Helper()
	fileRepo := testutil.NewMockDriveFileRepository()
	folderRepo := testutil.NewMockDriveFolderRepository()
	folderRepo.FilesRef = fileRepo
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return drive.NewService(fileRepo, folderRepo, st, idGen), fileRepo
}

// #2315: storedInternal は「実際にローカルへ書いたか」を反映する。true 固定
// だった頃は、オブジェクトストレージへ保存したファイルまで「ローカル保存」と
// 記録され `/files/:accessKey` がローカルを見て 404 になっていた。
func TestUpload_StoredInternalReflectsBackend(t *testing.T) {
	localSvc, _ := newSvcWithStorage(t, drive.NewLocalStorage(t.TempDir(), "https://example.com/files"))
	f, err := localSvc.Upload(context.Background(), drive.UploadInput{Body: []byte("local"), Name: "a.bin"})
	require.NoError(t, err)
	assert.True(t, f.StoredInternal, "ローカル保存なら true")

	objStore := newFakeObjectStorage()
	objSvc, _ := newSvcWithStorage(t, objStore)
	f2, err := objSvc.Upload(context.Background(), drive.UploadInput{Body: []byte("remote"), Name: "b.bin"})
	require.NoError(t, err)
	assert.False(t, f2.StoredInternal, "オブジェクトストレージ保存なら false")
	require.NotNil(t, f2.AccessKey)
	assert.Contains(t, objStore.objects, *f2.AccessKey, "実体が object storage 側にあること")
}

// MetaStorage 越しでも同じ判定になること (= 設定変更が storedInternal に効く)。
func TestUpload_StoredInternalFollowsMetaStorage(t *testing.T) {
	current := &model.Meta{ID: "x"} // object storage 無効
	st := drive.NewMetaStorage(func() (*model.Meta, error) { return current, nil }, t.TempDir(), "https://example.com/files")
	svc, _ := newSvcWithStorage(t, st)

	f, err := svc.Upload(context.Background(), drive.UploadInput{Body: []byte("local"), Name: "a.bin"})
	require.NoError(t, err)
	assert.True(t, f.StoredInternal)
}

// storedInternal=true な行 (= object storage 有効化より前に保存されたファイル)
// の削除は、primary がオブジェクトストレージでもローカルへ届くこと。届かないと
// ローカルに孤児が残り続ける。
func TestDelete_RoutesByStoredInternal(t *testing.T) {
	localDir := t.TempDir()
	local := drive.NewLocalStorage(localDir, "https://example.com/files")
	objStore := newFakeObjectStorage()

	svc, fileRepo := newSvcWithStorage(t, objStore)
	svc.SetLocalStorage(local)

	// 切替前に保存されたローカルファイル。
	_, err := local.Put("legacy-key", bytes.NewReader([]byte("old")))
	require.NoError(t, err)
	uid := "u1"
	legacy := "legacy-key"
	require.NoError(t, fileRepo.Create(&model.DriveFile{
		ID: "f1", UserID: &uid, AccessKey: &legacy, StoredInternal: true, Size: 3,
	}))

	require.NoError(t, svc.Delete(&model.User{ID: uid}, "f1"))
	_, err = local.Get("legacy-key")
	assert.ErrorIs(t, err, drive.ErrObjectNotFound, "ローカルの実体が消えること")
	assert.Empty(t, objStore.deleted, "object storage 側は触らない")

	// 切替後に保存されたファイルは object storage 側から消す。
	objStore.objects["new-key"] = []byte("new")
	newKey := "new-key"
	require.NoError(t, fileRepo.Create(&model.DriveFile{
		ID: "f2", UserID: &uid, AccessKey: &newKey, StoredInternal: false, Size: 3,
	}))
	require.NoError(t, svc.Delete(&model.User{ID: uid}, "f2"))
	assert.Contains(t, objStore.deleted, "new-key")
}

// localStorage 未配線なら primary に倒す (object storage 無効な既存 wiring を
// 壊さない)。
func TestDelete_WithoutLocalStorageFallsBackToPrimary(t *testing.T) {
	objStore := newFakeObjectStorage()
	svc, fileRepo := newSvcWithStorage(t, objStore)

	objStore.objects["k"] = []byte("x")
	uid := "u1"
	ak := "k"
	require.NoError(t, fileRepo.Create(&model.DriveFile{
		ID: "f1", UserID: &uid, AccessKey: &ak, StoredInternal: true, Size: 1,
	}))
	require.NoError(t, svc.Delete(&model.User{ID: uid}, "f1"))
	assert.Contains(t, objStore.deleted, "k")
}

// blockingImageProcessor reports the peak number of concurrent calls.
//
// **画像で測る。** 枠が守っているのは「同時に確保される中間バッファの本数」で、
// その根拠 (`imagedecode` の pixel cap / `processImage` の 4 回デコード) は
// すべて画像の話。動画で測っていた 1 稿目は、**画像だけ枠を外す変異が素通り
// する**ことを敵対的レビューで実測された。
type blockingImageProcessor struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	release  chan struct{}
}

func (b *blockingImageProcessor) enter() {
	b.mu.Lock()
	b.inFlight++
	if b.inFlight > b.peak {
		b.peak = b.inFlight
	}
	b.mu.Unlock()
	<-b.release
	b.mu.Lock()
	b.inFlight--
	b.mu.Unlock()
}

func (b *blockingImageProcessor) GetDimensions(_ []byte, _ string) (int, int, error) {
	b.enter()
	return 1, 1, nil
}
func (b *blockingImageProcessor) CalculateBlurhash(_ []byte, _ string) (string, error) {
	return "", nil
}
func (b *blockingImageProcessor) GenerateThumbnail(_ []byte, _ string) (*drive.ProcessedImage, error) {
	return nil, nil
}
func (b *blockingImageProcessor) GenerateWebpublic(_ []byte, _ string) (*drive.ProcessedImage, error) {
	return nil, nil
}

// blockingVideoProcessor blocks so tests can observe whether the video path
// takes a slot.
type blockingVideoProcessor struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	release  chan struct{}
}

func (b *blockingVideoProcessor) GenerateThumbnail(_ context.Context, _ []byte, _ string) (*drive.ProcessedImage, error) {
	b.mu.Lock()
	b.inFlight++
	if b.inFlight > b.peak {
		b.peak = b.inFlight
	}
	b.mu.Unlock()
	<-b.release
	b.mu.Lock()
	b.inFlight--
	b.mu.Unlock()
	return &drive.ProcessedImage{Data: []byte("thumb"), MimeType: "image/webp"}, nil
}

// waitForInFlight waits until the probe reports at least n concurrent calls.
func waitForInFlight(get func() int, n int) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if get() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// **同時実行枠が実際に効いていること (画像経路)。**
//
// デコードは 1 枚で数百 MB を確保しうるので、枠が無いと認証済みの利用者が
// 並行アップロードを投げるだけで確保量を掛け算できる。Go の大確保失敗は
// `throw("out of memory")` で recover できないため、プロセスごと落ちる。
func TestGenerateAlts_ConcurrencyIsBounded(t *testing.T) {
	svc, _, _ := newSvc(t)
	proc := &blockingImageProcessor{release: make(chan struct{})}
	svc.SetImageProcessor(proc)
	svc.SetMediaProcessingConcurrency(1)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.GenerateAltsCtxForTest(context.Background(), []byte("img"), "image/png")
		}()
	}
	waitForInFlight(func() int {
		proc.mu.Lock()
		defer proc.mu.Unlock()
		return proc.inFlight
	}, 1)
	time.Sleep(50 * time.Millisecond)
	close(proc.release)
	wg.Wait()

	proc.mu.Lock()
	defer proc.mu.Unlock()
	assert.Equal(t, 1, proc.peak, "枠を超えて同時に処理している (peak=%d)", proc.peak)
}

// 枠を広げれば同時に走る。**これが無いと「常に 1 本ずつ」でもテストが通る。**
func TestGenerateAlts_ConcurrencyHonoursTheConfiguredSize(t *testing.T) {
	svc, _, _ := newSvc(t)
	proc := &blockingImageProcessor{release: make(chan struct{})}
	svc.SetImageProcessor(proc)
	svc.SetMediaProcessingConcurrency(3)

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.GenerateAltsCtxForTest(context.Background(), []byte("img"), "image/png")
		}()
	}
	waitForInFlight(func() int {
		proc.mu.Lock()
		defer proc.mu.Unlock()
		return proc.inFlight
	}, 3)
	close(proc.release)
	wg.Wait()

	proc.mu.Lock()
	defer proc.mu.Unlock()
	assert.Equal(t, 3, proc.peak, "枠を広げても 1 本ずつになっている")
}

// **動画は枠を取らない。** ffmpeg は別プロセスで Go ヒープの中間バッファを
// 抱えず、`exec.Command` は context も timeout も持たないので、枠の内側に
// 入れると動画 1 本が画像処理を全部止める。
func TestGenerateAlts_VideoDoesNotTakeASlot(t *testing.T) {
	svc, _, _ := newSvc(t)
	proc := &blockingVideoProcessor{release: make(chan struct{})}
	svc.SetVideoProcessor(proc)
	svc.SetMediaProcessingConcurrency(1)

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.GenerateAltsCtxForTest(context.Background(), []byte("video"), "video/mp4")
		}()
	}
	waitForInFlight(func() int {
		proc.mu.Lock()
		defer proc.mu.Unlock()
		return proc.inFlight
	}, 3)
	close(proc.release)
	wg.Wait()

	proc.mu.Lock()
	defer proc.mu.Unlock()
	assert.Equal(t, 3, proc.peak, "ffmpeg が枠を占有している (枠 1 で 1 本ずつになっている)")
}

// 枠待ちの間に呼び出し元が諦めたら処理しない (best-effort の契約どおり nil)。
func TestGenerateAlts_CancelledWhileWaiting(t *testing.T) {
	svc, _, _ := newSvc(t)
	proc := &blockingImageProcessor{release: make(chan struct{})}
	svc.SetImageProcessor(proc)
	svc.SetMediaProcessingConcurrency(1)

	holder := make(chan struct{})
	go func() {
		defer close(holder)
		svc.GenerateAltsCtxForTest(context.Background(), []byte("img"), "image/png")
	}()
	waitForInFlight(func() int {
		proc.mu.Lock()
		defer proc.mu.Unlock()
		return proc.inFlight
	}, 1)

	// **待たずに済む形で呼ぶ。** guard が無いと processor の中で止まるので、
	// 直接呼ぶとテストごとデッドロックして「タイムアウトで落ちた」という
	// 読みにくい失敗になる。別 goroutine + 期限で「戻ってこないこと自体」を
	// 失敗として扱う。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan bool, 1)
	go func() { done <- svc.GenerateAltsCtxForTest(ctx, []byte("img"), "image/png") }()

	select {
	case processed := <-done:
		assert.False(t, processed, "ctx が切れているのに処理へ進んでいる")
	case <-time.After(2 * time.Second):
		t.Error("ctx が切れているのに枠を待ち続けている (processor まで進んだ可能性)")
	}

	close(proc.release)
	<-holder
}

// 既定の枠は GOMAXPROCS の半分 (最低 1)。0 以下を渡したら既定へ戻す。
//
// **「1 以上 4 以下」のような緩い範囲では固定にならない。** GOMAXPROCS 全部に
// 戻す変異 (= 無制限と区別が付かなくなる形) がコア数次第で素通りする。
// media proxy の枠 (#3032) と同じ規則であることまで書く。
func TestMediaProcessingConcurrency_Defaults(t *testing.T) {
	want := runtime.GOMAXPROCS(0) / 2
	if want < 1 {
		want = 1
	}
	def := drive.DefaultMediaProcessingConcurrencyForTest()
	assert.Equal(t, want, def, "既定が GOMAXPROCS/2 (最低 1) でない")

	svc, _, _ := newSvc(t)
	assert.Equal(t, def, svc.MediaProcessingSlotsForTest(), "NewService が枠を配線していない")

	svc.SetMediaProcessingConcurrency(2)
	assert.Equal(t, 2, svc.MediaProcessingSlotsForTest())

	svc.SetMediaProcessingConcurrency(0)
	assert.Equal(t, def, svc.MediaProcessingSlotsForTest(), "0 以下は既定へ戻す")
}

// bmpHeaderForTest builds a 54-byte BMP header declaring w x h at 24bpp,
// with no pixel data at all.
//
// **データを付けないのが要点。** ヘッダだけ見て弾けていれば、その先を読みに
// 行かないので、判定がデコードより前にあることを直接確かめられる。
func bmpHeaderForTest(w, h int32) []byte {
	var b bytes.Buffer
	b.WriteString("BM")
	_ = binary.Write(&b, binary.LittleEndian, uint32(54))
	_ = binary.Write(&b, binary.LittleEndian, uint32(0))
	_ = binary.Write(&b, binary.LittleEndian, uint32(54))
	_ = binary.Write(&b, binary.LittleEndian, uint32(40))
	_ = binary.Write(&b, binary.LittleEndian, w)
	_ = binary.Write(&b, binary.LittleEndian, h)
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, uint16(24))
	_ = binary.Write(&b, binary.LittleEndian, uint32(0))
	_ = binary.Write(&b, binary.LittleEndian, uint32(0))
	_ = binary.Write(&b, binary.LittleEndian, int32(2835))
	_ = binary.Write(&b, binary.LittleEndian, int32(2835))
	_ = binary.Write(&b, binary.LittleEndian, uint32(0))
	_ = binary.Write(&b, binary.LittleEndian, uint32(0))
	return b.Bytes()
}

// **アップロード経路にも pixel cap が効いていること。**
//
// `decodeImage` は `internal/misc/imagedecode` へ委譲しているので、cap は
// media proxy と共通のものが効く。**その委譲が外れると、ローカルアップロード
// だけ 54 バイトのヘッダで数 GB を確保できる状態に戻る** (Go の大確保失敗は
// `throw("out of memory")` で recover できないのでプロセスごと落ちる)。
// 4 つの入口すべてを見るのは、どれか 1 つだけ直接 `imaging.Decode` に
// 戻される形を捕まえるため。
func TestImageProcessor_RefusesOversizedHeader(t *testing.T) {
	p := drive.NewDefaultImageProcessor()
	// 54 バイトで 8.6GB (46341^2 x 4 byte) を要求するヘッダ。
	data := bmpHeaderForTest(46341, 46341)
	require.Len(t, data, 54)

	// **cap の sentinel まで見る。** 「何かのエラー」で満足すると、委譲を外して
	// 素の `imaging.Decode` に戻しても (54 バイトしか無いので) デコード失敗で
	// エラーにはなり、テストが通ってしまう。
	_, _, err := p.GetDimensions(data, "image/bmp")
	require.Error(t, err, "GetDimensions が巨大なヘッダを受けている")
	assert.ErrorIs(t, err, imagedecode.ErrTooManyPixels)

	_, err = p.CalculateBlurhash(data, "image/bmp")
	require.Error(t, err, "CalculateBlurhash が巨大なヘッダを受けている")
	assert.ErrorIs(t, err, imagedecode.ErrTooManyPixels)

	// サムネイル / webpublic はデコード失敗を `nil, nil` に倒す契約 (best-effort)
	// なので、ここで見られるのは「画像を作らずに戻る」ことまで。ラスタを
	// 確保しないことは上の 2 つが sentinel で押さえている。
	thumb, err := p.GenerateThumbnail(data, "image/bmp")
	assert.NoError(t, err)
	assert.Nil(t, thumb, "巨大なヘッダからサムネイルを作っている")

	wp, err := p.GenerateWebpublic(data, "image/bmp")
	assert.NoError(t, err)
	assert.Nil(t, wp, "巨大なヘッダから webpublic を作っている")
}

// **普通の画像はこれまでどおり通る。** これが無いと「BMP を常に拒否する」
// 実装でも上のテストが通る。
func TestImageProcessor_OrdinaryImageStillProcesses(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	img.Set(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))

	p := drive.NewDefaultImageProcessor()
	w, h, err := p.GetDimensions(buf.Bytes(), "image/png")
	require.NoError(t, err)
	assert.Equal(t, 16, w)
	assert.Equal(t, 16, h)
}

// **列に入らないファイル名を弾く (#3037)。**
//
// `drive_file.name` は varchar(256)。NUL も不正な UTF-8 も列幅超過も、そのまま
// INSERT / UPDATE すると PostgreSQL がクエリごと落とす (22021 / 22001) ので、
// **認証済みの利用者が 1 文字で 500 を起こせていた**。#3022 と同じく既存の
// 述語 (`ValidateFileName`) に畳んで既存の 400 に落とす。
func TestValidateFileName_RejectsUnstorable(t *testing.T) {
	for name, in := range map[string]string{
		"NUL":            "a\x00b.png",
		"invalid UTF-8":  "a\x80b.png",
		"lone surrogate": "a\xed\xa0\x80b.png",
		"too long (201)": strings.Repeat("a", 201),
	} {
		t.Run(name, func(t *testing.T) {
			assert.False(t, drive.ValidateFileName(in), "列に入らない名前を受け入れている")
		})
	}
}

// **普通の名前は通ったまま。** これが無いと「常に拒否する」実装でも上が通る。
func TestValidateFileName_OrdinaryNamesStillPass(t *testing.T) {
	for _, in := range []string{
		"hello.png",
		"日本語のファイル名.jpg",
		"絵文字\U0001F600.png",
		strings.Repeat("あ", 200),
	} {
		assert.True(t, drive.ValidateFileName(in), "正当な名前を弾いている: %q", in)
	}
}

// **読み切る前に上限を引けること (#3037)。**
//
// `Upload` の中にも同じ判定があるが、あそこへ届く時点で本体は全部メモリに
// 載っている。既定では policy が 30MB なのに `config.maxFileSize` が 250MB
// なので、30MB しか保存できない利用者が 250MB を確保させられた。
func TestMaxUploadBytes(t *testing.T) {
	svc, _, _ := newSvc(t)
	local := &model.User{ID: "u1"}
	remoteHost := "remote.example"
	remote := &model.User{ID: "u2", Host: &remoteHost}

	// roleChecker 未配線 → 上限なし。
	_, ok := svc.MaxUploadBytes(local)
	assert.False(t, ok, "role が未配線なのに上限を作っている")

	svc.SetRoleChecker(&fakeMod{policies: map[string]map[string]any{
		"u1": {"maxFileSizeMb": 30},
		"u2": {"maxFileSizeMb": 30},
		"u3": {"maxFileSizeMb": 0},
		"u4": {"maxFileSizeMb": 1.5},
	}})

	got, ok := svc.MaxUploadBytes(local)
	require.True(t, ok)
	assert.Equal(t, int64(30*1024*1024), got)

	// 小数の policy も gate と同じ値になる (`Capacity` と同じ理由、#2611)。
	got, ok = svc.MaxUploadBytes(&model.User{ID: "u4"})
	require.True(t, ok)
	assert.Equal(t, int64(1.5*1024*1024), got)

	// 上限を作らない側。**`Upload` の gate と対象を揃える** — あちらも
	// system file と remote user は見ない。
	for _, tt := range []struct {
		name string
		user *model.User
	}{
		{"system file (user なし)", nil},
		{"remote user", remote},
		{"policy が 0", &model.User{ID: "u3"}},
		{"policy の無い user", &model.User{ID: "u9"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, ok := svc.MaxUploadBytes(tt.user)
			assert.False(t, ok, "上限を作ってはいけない相手に上限を作っている")
		})
	}
}

// --- #3037 レビュー 2 周目: メタデータを落とせない画像は受け取らない ---

// webpWithExif builds a RIFF/WEBP container of at least size bytes that carries
// an EXIF chunk, without needing a real encoder.
func webpWithExif(size int) []byte {
	exif := []byte("EXIF")
	payload := []byte("Exif\x00\x00II*\x00\x08\x00\x00\x00")
	pad := size
	if pad < 64 {
		pad = 64
	}
	b := make([]byte, 0, pad+64)
	b = append(b, "RIFF"...)
	b = append(b, 0, 0, 0, 0) // size は判定に使われないので 0 のままでよい
	b = append(b, "WEBP"...)
	// VP8 チャンクで嵩を作る。
	b = append(b, "VP8 "...)
	b = append(b, 0, 0, 0, 0)
	b = append(b, make([]byte, pad)...)
	// EXIF チャンク。
	b = append(b, exif...)
	b = append(b, byte(len(payload)), 0, 0, 0)
	b = append(b, payload...)
	return b
}

// **デコードできない画像を受け取ると、原本がそのまま公開側へ出る。**
//
// `SandboxedDecoderMaxBytes` (32MiB) は #3037 が新しく入れた上限で、これに
// 当たると `GenerateWebpublic` が `nil, nil` を返す。webpublic が無いと
// `GetPublicURL` は原本へ落ちるので、**EXIF の GPS がそのまま公開される** —
// この PR が `entity/drive.go` 側で塞いだ穴と同じもの。しかも
// `NormalizeImageForDetection` も同じ理由で失敗するので、センシティブ判定も
// fail-open で `false` になる。
//
// 発火するのは `maxFileSizeMb >= 33` の構成だけ (既定は 30)。
func TestUpload_RejectsImagesWhoseMetadataCannotBeStripped(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetRoleChecker(&fakeMod{policies: map[string]map[string]any{
		"u1": {"maxFileSizeMb": 64},
	}})
	body := webpWithExif(33 << 20)

	_, err := svc.Upload(context.Background(), drive.UploadInput{
		User: &model.User{ID: "u1"}, Body: body, Name: "x.webp",
	})

	require.ErrorIs(t, err, drive.ErrUndecodableImage,
		"メタデータを落とせない画像を受け取っている (原本が公開側に出る)")
}

// **拒否したファイルの実体を残さない (#3037 レビュー 3 周目)。**
//
// guard が `backend.Put` の後ろにあると、本体だけがストレージに残る。
// `drive_file` 行は作られないので `UsageByUser` にも `DeleteOrphans`
// (行ベース) にも乗らず、**恒久的にリークする** — 実測で拒否 1 回あたり
// 34,603,050 バイトが残った。認証済みの利用者が何度でも叩ける。
func TestUpload_RejectedImageLeavesNothingInStorage(t *testing.T) {
	dir := t.TempDir()
	storage := drive.NewLocalStorage(dir, "https://example.com/files")
	svc, fileRepo := newSvcWithStorage(t, storage)
	svc.SetRoleChecker(&fakeMod{policies: map[string]map[string]any{
		"u1": {"maxFileSizeMb": 64},
	}})

	_, err := svc.Upload(context.Background(), drive.UploadInput{
		User: &model.User{ID: "u1"}, Body: webpWithExif(33 << 20), Name: "x.webp",
	})
	require.ErrorIs(t, err, drive.ErrUndecodableImage)

	assert.Empty(t, fileRepo.Files, "拒否したのに drive_file 行がある")
	var left []string
	require.NoError(t, filepath.Walk(dir, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if !info.IsDir() {
			left = append(left, path)
		}
		return nil
	}))
	assert.Empty(t, left, "拒否したファイルの実体がストレージに残っている (恒久的にリークする)")
}

// **上限の内側なら従来どおり通る。** これが無いと「WebP を常に拒否する」
// 実装でも上のテストが通る。
func TestUpload_AcceptsImagesUnderTheDecoderSizeCap(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetRoleChecker(&fakeMod{policies: map[string]map[string]any{
		"u1": {"maxFileSizeMb": 64},
	}})
	body := webpWithExif(1 << 20)

	_, err := svc.Upload(context.Background(), drive.UploadInput{
		User: &model.User{ID: "u1"}, Body: body, Name: "x.webp",
	})

	require.NotErrorIs(t, err, drive.ErrUndecodableImage, "上限の内側を弾いている")
}

// **落とすものが無い画像は通す。** 原本を出しても漏れないので、上限に当たる
// だけで拒否すると受け取れるものを不必要に減らす。
func TestUpload_AcceptsLargeImagesWithoutMetadata(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetRoleChecker(&fakeMod{policies: map[string]map[string]any{
		"u1": {"maxFileSizeMb": 64},
	}})
	// EXIF チャンクを持たない WebP。
	body := make([]byte, 0, 33<<20)
	body = append(body, "RIFF"...)
	body = append(body, 0, 0, 0, 0)
	body = append(body, "WEBP"...)
	body = append(body, "VP8 "...)
	body = append(body, 0, 0, 0, 0)
	body = append(body, make([]byte, 33<<20)...)

	_, err := svc.Upload(context.Background(), drive.UploadInput{
		User: &model.User{ID: "u1"}, Body: body, Name: "x.webp",
	})

	require.NotErrorIs(t, err, drive.ErrUndecodableImage, "落とすものが無い画像を弾いている")
}

// **AVIF は中身に関わらず拒否する (#3037 レビュー 3 周目)。**
//
// `hasStrippableMetadata` は AVIF に false を返す — 「AVIF は寸法に関わらず
// webpublic を作る枝が別にある」ことが前提だが、**デコードできなければその枝も
// 動かない**。AVIF の原本は Mastodon / MS Edge が表示できないので、通すと
// 壊れた添付になる。2 周目はこの枝を足したのにテストを置いておらず、
// **条件を消しても緑のままだった**。
func TestUpload_RejectsOversizedAVIFEvenWithoutMetadata(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetRoleChecker(&fakeMod{policies: map[string]map[string]any{
		"u1": {"maxFileSizeMb": 64},
	}})

	_, err := svc.Upload(context.Background(), drive.UploadInput{
		User: &model.User{ID: "u1"}, Body: avifWithoutMetadata(33 << 20), Name: "x.avif",
	})

	require.ErrorIs(t, err, drive.ErrUndecodableImage,
		"メタデータの無い巨大 AVIF を受け取っている (原本は Mastodon / Edge で表示できない)")
}

// **上限の内側の AVIF は通す。** これが無いと「AVIF を常に拒否する」実装でも
// 上のテストが通る。
func TestUpload_AcceptsAVIFUnderTheDecoderSizeCap(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetRoleChecker(&fakeMod{policies: map[string]map[string]any{
		"u1": {"maxFileSizeMb": 64},
	}})

	_, err := svc.Upload(context.Background(), drive.UploadInput{
		User: &model.User{ID: "u1"}, Body: avifWithoutMetadata(1 << 20), Name: "x.avif",
	})

	require.NotErrorIs(t, err, drive.ErrUndecodableImage, "上限の内側の AVIF を弾いている")
}

// avifWithoutMetadata builds an ISOBMFF container with the `avif` brand and no
// EXIF/XMP box, padded to at least size bytes.
func avifWithoutMetadata(size int) []byte {
	pad := size
	if pad < 64 {
		pad = 64
	}
	b := make([]byte, 0, pad+32)
	// ftyp box: size(4) + "ftyp" + major brand + minor version + compatible brand.
	b = append(b, 0, 0, 0, 20)
	b = append(b, "ftyp"...)
	b = append(b, "avif"...)
	b = append(b, 0, 0, 0, 0)
	b = append(b, "avif"...)
	// mdat box に嵩を持たせる (中身は読まれない)。
	b = append(b, 0, 0, 0, 8)
	b = append(b, "mdat"...)
	b = append(b, make([]byte, pad)...)
	return b
}
