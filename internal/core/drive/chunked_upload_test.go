package drive_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chunkedFixture bundles everything a chunked upload test needs.
type chunkedFixture struct {
	svc      *drive.Service
	storage  *mockMultipartStorage
	sessions *mockChunkedSessionRepo
	files    *testutil.MockDriveFileRepository
	folders  *testutil.MockDriveFolderRepository
	roles    *fakeMod
	settings drive.ChunkedUploadSettings
	user     *model.User
}

const testChunkSize = 5 * 1024 * 1024

// defaultChunkedPolicies mirrors the shipped defaults so tests only override
// what they are actually exercising.
func defaultChunkedPolicies() map[string]any {
	return map[string]any{
		role.PolicyCanUseChunkedUpload:                true,
		role.PolicyChunkedUploadMaxConcurrentSessions: 4,
		role.PolicyChunkedUploadMaxPendingMb:          1024,
		"maxFileSizeMb":                               100,
		"driveCapacityMb":                             1024,
	}
}

func newChunkedFixture(t *testing.T) *chunkedFixture {
	t.Helper()
	fileRepo := testutil.NewMockDriveFileRepository()
	folderRepo := testutil.NewMockDriveFolderRepository()
	folderRepo.FilesRef = fileRepo
	storage := newMockMultipartStorage()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	svc := drive.NewService(fileRepo, folderRepo, storage, idGen)
	roles := &fakeMod{policies: map[string]map[string]any{"u1": defaultChunkedPolicies()}}
	svc.SetRoleChecker(roles)

	sessions := newMockChunkedSessionRepo()
	f := &chunkedFixture{
		svc:      svc,
		storage:  storage,
		sessions: sessions,
		files:    fileRepo,
		folders:  folderRepo,
		roles:    roles,
		settings: drive.ChunkedUploadSettings{
			Enabled:                true,
			ChunkSize:              testChunkSize,
			SessionTTL:             time.Hour,
			MaxSessionsPerUser:     8,
			MaxPendingBytesPerUser: 2048 * 1024 * 1024,
		},
		user: &model.User{ID: "u1"},
	}
	svc.SetChunkedUpload(sessions, func() drive.ChunkedUploadSettings { return f.settings })
	return f
}

func (f *chunkedFixture) start(t *testing.T, size int64) *model.ChunkedUploadSession {
	t.Helper()
	sess, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user,
		Name: "video.bin",
		Size: size,
	})
	require.NoError(t, err)
	return sess
}

// ---------------------------------------------------------------------------
// Capability / availability
// ---------------------------------------------------------------------------

func TestChunkedUploadCapability_UnwiredIsUnavailable(t *testing.T) {
	svc, _, _ := newSvc(t)
	_, ok := svc.ChunkedUploadCapability()
	assert.False(t, ok, "unwired service must not advertise chunked upload")
}

// ローカルストレージ構成は MultipartStorage を満たさないので、設定で有効に
// しても機能は出ない。/api/meta の能力告知が出ない = frontend は単発
// アップロードに倒れる。
func TestChunkedUploadCapability_LocalStorageIsUnavailable(t *testing.T) {
	svc, _, _ := newSvc(t)
	svc.SetChunkedUpload(newMockChunkedSessionRepo(), func() drive.ChunkedUploadSettings {
		return drive.ChunkedUploadSettings{Enabled: true, ChunkSize: testChunkSize}
	})
	_, ok := svc.ChunkedUploadCapability()
	assert.False(t, ok)
}

func TestChunkedUploadCapability_DisabledInMeta(t *testing.T) {
	f := newChunkedFixture(t)
	f.settings.Enabled = false
	_, ok := f.svc.ChunkedUploadCapability()
	assert.False(t, ok)
}

func TestChunkedUploadCapability_Available(t *testing.T) {
	f := newChunkedFixture(t)
	settings, ok := f.svc.ChunkedUploadCapability()
	require.True(t, ok)
	assert.Equal(t, int64(testChunkSize), settings.ChunkSize)
}

// 未対応構成では start / append / finish / abort のいずれも通らない。
func TestChunkedUpload_UnavailableRejectsEveryEndpoint(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, testChunkSize)
	f.settings.Enabled = false

	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{User: f.user, Size: 1})
	assert.ErrorIs(t, err, drive.ErrChunkedUploadUnavailable)
	_, err = f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(10))
	assert.ErrorIs(t, err, drive.ErrChunkedUploadUnavailable)
	_, err = f.svc.FinishChunkedUpload(context.Background(), f.user, sess.ID)
	assert.ErrorIs(t, err, drive.ErrChunkedUploadUnavailable)
}

func TestChunkedUploadSettingsFromMeta_Clamps(t *testing.T) {
	// 範囲外の値が DB に入っていても、S3 の最小パートサイズを割らない値に
	// 丸められること。
	s := drive.ChunkedUploadSettingsFromMeta(&model.Meta{
		ChunkedUploadEnabled:             true,
		ChunkedUploadChunkSizeMb:         1,
		ChunkedUploadSessionTTLMinutes:   99999,
		ChunkedUploadMaxSessionsPerUser:  8,
		ChunkedUploadMaxPendingMbPerUser: 2048,
	})
	assert.Equal(t, int64(drive.MinChunkSizeMb)*1024*1024, s.ChunkSize)
	assert.Equal(t, time.Duration(drive.MaxSessionTTLMinutes)*time.Minute, s.SessionTTL)

	// 未設定 (0) は既定値に倒れる。
	s = drive.ChunkedUploadSettingsFromMeta(&model.Meta{ChunkedUploadEnabled: true})
	assert.Equal(t, int64(drive.DefaultChunkSizeMb)*1024*1024, s.ChunkSize)
	assert.Equal(t, time.Duration(drive.DefaultSessionTTLMinutes)*time.Minute, s.SessionTTL)

	assert.Equal(t, drive.ChunkedUploadSettings{}, drive.ChunkedUploadSettingsFromMeta(nil))
}

// ---------------------------------------------------------------------------
// Start
// ---------------------------------------------------------------------------

func TestStartChunkedUpload_HappyPath(t *testing.T) {
	f := newChunkedFixture(t)
	sess, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Name: "video.bin", Size: 3 * testChunkSize,
	})
	require.NoError(t, err)
	assert.Equal(t, "u1", sess.UserID)
	assert.Equal(t, int64(3*testChunkSize), sess.TotalSize)
	assert.Equal(t, int64(testChunkSize), sess.ChunkSize)
	assert.NotEmpty(t, sess.AccessKey)
	assert.Nil(t, sess.UploadID, "start must not touch object storage")
	assert.True(t, sess.ExpiresAt.After(time.Now()))
	assert.Equal(t, 0, f.storage.OpenUploads())
}

// chunkSize は start でセッションに固定される。進行中に admin が設定を変えても
// パートサイズが揺れない (R2 の均一パートサイズ要求を壊さない)。
func TestStartChunkedUpload_PinsChunkSize(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, 2*testChunkSize)
	f.settings.ChunkSize = 32 * 1024 * 1024
	assert.Equal(t, int64(testChunkSize), f.sessions.Get(sess.ID).ChunkSize)
}

func TestStartChunkedUpload_RejectsSystemUser(t *testing.T) {
	f := newChunkedFixture(t)
	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{Size: 1})
	assert.ErrorIs(t, err, drive.ErrChunkedUploadNotAllowed)
}

func TestStartChunkedUpload_RejectsNonPositiveSize(t *testing.T) {
	f := newChunkedFixture(t)
	for _, size := range []int64{0, -1} {
		_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{User: f.user, Size: size})
		assert.ErrorIs(t, err, drive.ErrInvalidUploadSize)
	}
}

// パート数が backend の上限を超える宣言は、受け取り始める前に弾く。
func TestStartChunkedUpload_RejectsTooManyParts(t *testing.T) {
	f := newChunkedFixture(t)
	f.roles.policies["u1"]["maxFileSizeMb"] = 0 // サイズ gate を外して parts 上限だけを見る
	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Size: int64(drive.MaxMultipartParts)*testChunkSize + 1,
	})
	assert.ErrorIs(t, err, drive.ErrInvalidUploadSize)
}

func TestStartChunkedUpload_MaxInt64SizeCannotOverflowPartCount(t *testing.T) {
	f := newChunkedFixture(t)
	f.roles.policies["u1"]["maxFileSizeMb"] = 0
	f.roles.policies["u1"]["driveCapacityMb"] = 0
	f.roles.policies["u1"][role.PolicyChunkedUploadMaxPendingMb] = 0
	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Size: math.MaxInt64,
	})
	assert.ErrorIs(t, err, drive.ErrInvalidUploadSize)
}

func TestStartChunkedUpload_MaxPoliciesRemainPositive(t *testing.T) {
	f := newChunkedFixture(t)
	f.roles.policies["u1"]["maxFileSizeMb"] = math.MaxFloat64
	f.roles.policies["u1"]["driveCapacityMb"] = math.MaxFloat64
	f.roles.policies["u1"][role.PolicyChunkedUploadMaxPendingMb] = math.MaxFloat64
	f.settings.MaxPendingBytesPerUser = 0
	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Size: testChunkSize,
	})
	require.NoError(t, err)
}

func TestStartChunkedUpload_DriveCapacitySumCannotOverflow(t *testing.T) {
	f := newChunkedFixture(t)
	uid := "u1"
	require.NoError(t, f.files.Create(&model.DriveFile{ID: "f1", UserID: &uid, Size: math.MaxInt}))
	f.roles.policies["u1"]["maxFileSizeMb"] = 0
	f.roles.policies["u1"]["driveCapacityMb"] = 1
	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Size: 1,
	})
	assert.ErrorIs(t, err, drive.ErrNoFreeSpace)
}

func TestStartChunkedUpload_PendingSumCannotOverflow(t *testing.T) {
	f := newChunkedFixture(t)
	f.roles.policies["u1"]["maxFileSizeMb"] = 0
	f.roles.policies["u1"]["driveCapacityMb"] = 0
	f.roles.policies["u1"][role.PolicyChunkedUploadMaxPendingMb] = 1
	f.settings.MaxPendingBytesPerUser = 0
	require.NoError(t, f.sessions.Create(&model.ChunkedUploadSession{
		ID: "pending", UserID: "u1", TotalSize: math.MaxInt64, ExpiresAt: time.Now().Add(time.Hour),
	}))
	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Size: 1,
	})
	assert.ErrorIs(t, err, drive.ErrPendingUploadLimitExceeded)
}

// policy が引けない構成では fail-closed。gate が効かない状態で素通しにしない。
func TestStartChunkedUpload_FailClosedWithoutPolicies(t *testing.T) {
	f := newChunkedFixture(t)

	f.roles.policies = nil
	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{User: f.user, Size: 1})
	assert.ErrorIs(t, err, drive.ErrChunkedUploadNotAllowed)

	f.svc.SetRoleChecker(nil)
	_, err = f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{User: f.user, Size: 1})
	assert.ErrorIs(t, err, drive.ErrChunkedUploadNotAllowed)
}

func TestStartChunkedUpload_PolicyDenied(t *testing.T) {
	f := newChunkedFixture(t)
	f.roles.policies["u1"][role.PolicyCanUseChunkedUpload] = false
	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{User: f.user, Size: 1})
	assert.ErrorIs(t, err, drive.ErrChunkedUploadNotAllowed)
}

func TestStartChunkedUpload_MaxFileSize(t *testing.T) {
	f := newChunkedFixture(t)
	f.roles.policies["u1"]["maxFileSizeMb"] = 10
	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Size: 11 * 1024 * 1024,
	})
	assert.ErrorIs(t, err, drive.ErrMaxFileSizeExceeded)
}

// 未完了セッションの申告分を容量計算に含めないと、残容量ぎりぎりのセッションを
// 複数開くだけで driveCapacityMb を丸ごと迂回できる。
func TestStartChunkedUpload_PendingCountsAgainstDriveCapacity(t *testing.T) {
	f := newChunkedFixture(t)
	f.roles.policies["u1"]["driveCapacityMb"] = 20
	f.roles.policies["u1"][role.PolicyChunkedUploadMaxPendingMb] = 1024

	f.start(t, 15*1024*1024)

	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Size: 10 * 1024 * 1024,
	})
	assert.ErrorIs(t, err, drive.ErrNoFreeSpace)
}

func TestStartChunkedUpload_ExistingUsageCountsAgainstDriveCapacity(t *testing.T) {
	f := newChunkedFixture(t)
	f.roles.policies["u1"]["driveCapacityMb"] = 20
	uid := "u1"
	require.NoError(t, f.files.Create(&model.DriveFile{ID: "f1", UserID: &uid, Size: 15 * 1024 * 1024}))

	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Size: 10 * 1024 * 1024,
	})
	assert.ErrorIs(t, err, drive.ErrNoFreeSpace)
}

func TestStartChunkedUpload_PendingLimit(t *testing.T) {
	f := newChunkedFixture(t)
	f.roles.policies["u1"][role.PolicyChunkedUploadMaxPendingMb] = 20
	f.start(t, 15*1024*1024)

	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Size: 10 * 1024 * 1024,
	})
	assert.ErrorIs(t, err, drive.ErrPendingUploadLimitExceeded)
}

// role policy がインスタンス設定より大きくても、サーバー側の cap が効く。
func TestStartChunkedUpload_ServerCapsPendingBytes(t *testing.T) {
	f := newChunkedFixture(t)
	f.roles.policies["u1"][role.PolicyChunkedUploadMaxPendingMb] = 100000
	f.settings.MaxPendingBytesPerUser = 20 * 1024 * 1024
	f.start(t, 15*1024*1024)

	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Size: 10 * 1024 * 1024,
	})
	assert.ErrorIs(t, err, drive.ErrPendingUploadLimitExceeded)
}

func TestStartChunkedUpload_ConcurrentSessionLimit(t *testing.T) {
	f := newChunkedFixture(t)
	f.roles.policies["u1"][role.PolicyChunkedUploadMaxConcurrentSessions] = 2
	f.start(t, testChunkSize)
	f.start(t, testChunkSize)

	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Size: testChunkSize,
	})
	assert.ErrorIs(t, err, drive.ErrTooManyUploadSessions)
}

func TestStartChunkedUpload_ServerCapsConcurrentSessions(t *testing.T) {
	f := newChunkedFixture(t)
	f.roles.policies["u1"][role.PolicyChunkedUploadMaxConcurrentSessions] = 1000
	f.settings.MaxSessionsPerUser = 1
	f.start(t, testChunkSize)

	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Size: testChunkSize,
	})
	assert.ErrorIs(t, err, drive.ErrTooManyUploadSessions)
}

// 期限切れセッションは同時数・未完了バイト数のどちらにも数えない。数えると
// 放置セッションで枠が埋まったまま復旧しない。
func TestStartChunkedUpload_ExpiredSessionsDoNotConsumeQuota(t *testing.T) {
	f := newChunkedFixture(t)
	f.roles.policies["u1"][role.PolicyChunkedUploadMaxConcurrentSessions] = 1
	sess := f.start(t, testChunkSize)

	stored := f.sessions.Get(sess.ID)
	stored.ExpiresAt = time.Now().Add(-time.Minute)
	require.NoError(t, f.sessions.Create(stored))

	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Size: testChunkSize,
	})
	assert.NoError(t, err)
}

func TestStartChunkedUpload_FolderOwnership(t *testing.T) {
	f := newChunkedFixture(t)
	other := "u2"
	require.NoError(t, f.folders.Create(&model.DriveFolder{ID: "fo1", UserID: &other}))

	folderID := "fo1"
	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Size: testChunkSize, FolderID: &folderID,
	})
	assert.ErrorIs(t, err, drive.ErrFolderNotFound, "others' folder must be indistinguishable from missing")

	missing := "nope"
	_, err = f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Size: testChunkSize, FolderID: &missing,
	})
	assert.ErrorIs(t, err, drive.ErrFolderNotFound)
}

// ---------------------------------------------------------------------------
// Append
// ---------------------------------------------------------------------------

func TestAppendChunk_HappyPath(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, testChunkSize+100)

	res, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(testChunkSize))
	require.NoError(t, err)
	assert.Equal(t, 1, res.Next)
	assert.Equal(t, int64(testChunkSize), res.ReceivedBytes)
	assert.False(t, res.Completed)

	res, err = f.svc.AppendChunk(context.Background(), f.user, sess.ID, 1, chunkBytes(100))
	require.NoError(t, err)
	assert.True(t, res.Completed)
	assert.Equal(t, int64(testChunkSize+100), res.ReceivedBytes)

	stored := f.sessions.Get(sess.ID)
	require.NotNil(t, stored.UploadID)
	assert.Equal(t, 2, stored.ReceivedChunks)
}

// Content-Type は最初のパートの先頭バイトから決める。クライアント申告は使わない。
// S3Storage.Put と同じく BrowserSafeContentType を通し、非 browser-safe を
// octet-stream に矯正する (#2106 H4 の stored XSS 対策を分割経路でも維持)。
func TestAppendChunk_SniffsContentTypeAndCoercesUnsafe(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, 40)
	html := append([]byte("<html><body>x</body></html>"), chunkBytes(13)...)

	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, html)
	require.NoError(t, err)
	assert.Equal(t, "application/octet-stream", f.storage.ContentType)
	assert.Equal(t, "application/octet-stream", *f.sessions.Get(sess.ID).ContentType)
}

// uploadableFileTypes は最初のパートで判定する。全部受け取ってから弾くのは無駄。
func TestAppendChunk_UnallowedFileTypeOnFirstChunk(t *testing.T) {
	f := newChunkedFixture(t)
	f.roles.policies["u1"]["uploadableFileTypes"] = []string{"image/*"}
	sess := f.start(t, 100)

	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(100))
	assert.ErrorIs(t, err, drive.ErrUnallowedFileType)
	assert.Equal(t, 0, f.storage.OpenUploads(), "rejected type must not leave a multipart upload behind")
}

func TestAppendChunk_ModeratorBypassesFileTypePolicy(t *testing.T) {
	f := newChunkedFixture(t)
	f.roles.moderators = map[string]bool{"u1": true}
	f.roles.policies["u1"]["uploadableFileTypes"] = []string{"image/*"}
	sess := f.start(t, 100)

	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(100))
	assert.NoError(t, err)
}

// 他人のセッション ID を指定しても、存在しない場合と区別できない応答にする。
func TestAppendChunk_OtherUsersSessionIsNotFound(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, testChunkSize)

	_, err := f.svc.AppendChunk(context.Background(), &model.User{ID: "u2"}, sess.ID, 0, chunkBytes(testChunkSize))
	assert.ErrorIs(t, err, drive.ErrUploadSessionNotFound)

	_, err = f.svc.AppendChunk(context.Background(), f.user, "no-such-session", 0, chunkBytes(testChunkSize))
	assert.ErrorIs(t, err, drive.ErrUploadSessionNotFound)

	_, err = f.svc.AppendChunk(context.Background(), nil, sess.ID, 0, chunkBytes(testChunkSize))
	assert.ErrorIs(t, err, drive.ErrUploadSessionNotFound)
}

func TestAppendChunk_ExpiredSessionIsNotFound(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, testChunkSize)
	stored := f.sessions.Get(sess.ID)
	stored.ExpiresAt = time.Now().Add(-time.Second)
	require.NoError(t, f.sessions.Create(stored))

	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(testChunkSize))
	assert.ErrorIs(t, err, drive.ErrUploadSessionNotFound)
}

// 順序前後・欠番は構造的に起きない。期待 index を返してクライアントに再同期
// させる。
func TestAppendChunk_OutOfOrderIndex(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, 2*testChunkSize)

	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 1, chunkBytes(testChunkSize))
	var idxErr *drive.ChunkIndexError
	require.ErrorAs(t, err, &idxErr)
	assert.Equal(t, 0, idxErr.Expected)
	assert.Contains(t, idxErr.Error(), "expected 0")
}

// 同一 index の再送は、内容が一致していれば冪等に成功する (応答を取りこぼした
// クライアントの再試行を許容する)。
func TestAppendChunk_IdenticalResendIsIdempotent(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, 2*testChunkSize)
	chunk := chunkBytes(testChunkSize)

	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunk)
	require.NoError(t, err)

	res, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunk)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Next)
	assert.Equal(t, int64(testChunkSize), res.ReceivedBytes, "resend must not double-count")
	assert.Equal(t, 1, f.sessions.Get(sess.ID).ReceivedChunks)
}

// 内容の違う再送は拒否する。黙って受けると組み上がるファイルが静かに変わる。
func TestAppendChunk_DifferentResendIsRejected(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, 2*testChunkSize)

	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(testChunkSize))
	require.NoError(t, err)

	other := append(chunkBytes(testChunkSize-1), 'b')
	_, err = f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, other)
	assert.ErrorIs(t, err, drive.ErrChunkContentMismatch)

	// サイズ違いも同様。
	_, err = f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(testChunkSize-1))
	assert.ErrorIs(t, err, drive.ErrChunkContentMismatch)
}

// 最終チャンク以外は chunkSize ちょうどを要求する。S3 の最小パートサイズと
// R2 の均一パートサイズ要求を同時に満たすため。
func TestAppendChunk_NonFinalChunkMustBeExactSize(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, 2*testChunkSize)

	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(testChunkSize-1))
	assert.ErrorIs(t, err, drive.ErrInvalidChunkSize)
}

func TestAppendChunk_EmptyChunkRejected(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, testChunkSize)

	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, nil)
	assert.ErrorIs(t, err, drive.ErrInvalidChunkSize)
}

// 申告サイズを超える送信は打ち切る。これが無いと「1 バイトと申告して無制限に
// 送る」でサイズ・容量 gate を丸ごと迂回できる。
func TestAppendChunk_OvershootingDeclaredSizeIsRejected(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, 100)

	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(101))
	assert.ErrorIs(t, err, drive.ErrMaxFileSizeExceeded)

	// 分割して積み上げても超えられない。
	sess2 := f.start(t, testChunkSize+10)
	_, err = f.svc.AppendChunk(context.Background(), f.user, sess2.ID, 0, chunkBytes(testChunkSize))
	require.NoError(t, err)
	_, err = f.svc.AppendChunk(context.Background(), f.user, sess2.ID, 1, chunkBytes(11))
	assert.ErrorIs(t, err, drive.ErrMaxFileSizeExceeded)
}

// CreateMultipartUpload に成功した直後に記録できなかった場合は、こちらが作った
// upload を abort する。放置すると DB から辿れない未完了アップロードが残り、
// GC が回収できないまま課金が積み上がる。
func TestAppendChunk_AbortsOrphanWhenUploadIDRaceLost(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, testChunkSize)

	// service が CreateMultipartUpload を終えて SetUploadID を呼ぶ直前に、
	// 並行した別の最初の append が先に確定させた状況を作る。
	f.sessions.OnSetUploadID = func() {
		_, err := f.sessions.SetUploadID(sess.ID, "someone-elses", "text/plain", time.Now())
		require.NoError(t, err)
	}

	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(testChunkSize))
	assert.ErrorIs(t, err, drive.ErrUploadSessionBusy)
	assert.Equal(t, 0, f.storage.OpenUploads(), "the losing request must abort the upload it created")
	assert.Len(t, f.storage.Aborted, 1)
}

// 記録に失敗した場合も同様に abort する。
func TestAppendChunk_AbortsOrphanWhenRecordingFails(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, testChunkSize)
	f.sessions.OnSetUploadID = func() { f.sessions.SetUploadIDErr = errors.New("db down") }

	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(testChunkSize))
	require.Error(t, err)
	assert.Equal(t, 0, f.storage.OpenUploads())
	assert.Len(t, f.storage.Aborted, 1)
}

// ---------------------------------------------------------------------------
// Finish
// ---------------------------------------------------------------------------

func TestFinishChunkedUpload_HappyPath(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, testChunkSize+7)
	first := chunkBytes(testChunkSize)
	last := chunkBytes(7)

	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, first)
	require.NoError(t, err)
	_, err = f.svc.AppendChunk(context.Background(), f.user, sess.ID, 1, last)
	require.NoError(t, err)

	file, err := f.svc.FinishChunkedUpload(context.Background(), f.user, sess.ID)
	require.NoError(t, err)
	require.NotNil(t, file)
	// 中身はテキストなので拡張子補正で .txt が付く (upstream と同じ)。
	assert.Equal(t, "video.bin.txt", file.Name)
	assert.Equal(t, testChunkSize+7, file.Size)
	require.NotNil(t, file.AccessKey)
	assert.Equal(t, sess.AccessKey, *file.AccessKey)
	assert.Equal(t, "https://cdn.example.com/"+sess.AccessKey, file.URL)
	// 分割アップロードは MultipartStorage を要求するので backend は必ず
	// オブジェクトストレージ側。storedInternal は false でなければならない
	// (true だと /files/:accessKey がローカルを見て 404 になる、#2315)。
	assert.False(t, file.StoredInternal)

	// 組み上がった実体がチャンクの連結と一致する。
	obj, ok := f.storage.Object(sess.AccessKey)
	require.True(t, ok)
	assert.Equal(t, append(append([]byte(nil), first...), last...), obj)

	// 本体は multipart で置かれているので Put で再送していない。
	assert.NotContains(t, f.storage.Puts, sess.AccessKey, "finish must not re-upload the body")
	assert.Equal(t, 0, f.sessions.Count(), "session row must be removed")
	assert.Equal(t, 0, f.storage.OpenUploads())
}

func TestFinishChunkedUpload_IncompleteIsRejected(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, testChunkSize+7)
	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(testChunkSize))
	require.NoError(t, err)

	_, err = f.svc.FinishChunkedUpload(context.Background(), f.user, sess.ID)
	assert.ErrorIs(t, err, drive.ErrIncompleteUpload)
	assert.Equal(t, 1, f.sessions.Count(), "an incomplete session must survive so it can be resumed")
}

// 何も送っていないセッションの finish は失敗する (uploadId が無い)。
func TestFinishChunkedUpload_WithoutAnyChunk(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, testChunkSize)
	_, err := f.svc.FinishChunkedUpload(context.Background(), f.user, sess.ID)
	assert.ErrorIs(t, err, drive.ErrIncompleteUpload)
}

func TestFinishChunkedUpload_OtherUserIsNotFound(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, 10)
	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(10))
	require.NoError(t, err)

	_, err = f.svc.FinishChunkedUpload(context.Background(), &model.User{ID: "u2"}, sess.ID)
	assert.ErrorIs(t, err, drive.ErrUploadSessionNotFound)
	assert.Equal(t, 1, f.sessions.Count())
}

// finish の同時実行で DriveFile が二重作成されないこと。
func TestFinishChunkedUpload_DoubleFinish(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, 10)
	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(10))
	require.NoError(t, err)

	// claim だけ先に奪っておく (= もう 1 本の finish が処理中の状態)。
	ok, err := f.sessions.ClaimFinish(sess.ID, time.Now())
	require.NoError(t, err)
	require.True(t, ok)

	_, err = f.svc.FinishChunkedUpload(context.Background(), f.user, sess.ID)
	assert.ErrorIs(t, err, drive.ErrUploadSessionBusy)
}

// complete が失敗したらセッションは残し、claim も戻してリトライ可能にする。
// ここで行を消すと未完了マルチパートアップロードが追跡不能になる。
func TestFinishChunkedUpload_CompleteFailureKeepsSession(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, 10)
	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(10))
	require.NoError(t, err)

	f.storage.CompleteErr = errors.New("network down")
	_, err = f.svc.FinishChunkedUpload(context.Background(), f.user, sess.ID)
	require.Error(t, err)
	assert.Equal(t, 1, f.sessions.Count())
	assert.False(t, f.sessions.Get(sess.ID).Finishing, "claim must be released so finish can be retried")

	f.storage.CompleteErr = nil
	_, err = f.svc.FinishChunkedUpload(context.Background(), f.user, sess.ID)
	assert.NoError(t, err)
}

// 記録した ETag と storage の実体がずれていれば complete が失敗し、壊れた
// ファイルは作られない。
func TestFinishChunkedUpload_EtagMismatchFails(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, 10)
	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(10))
	require.NoError(t, err)

	// storage 側のパートだけ差し替える (DB の ETag はそのまま)。
	stored := f.sessions.Get(sess.ID)
	_, err = f.storage.UploadPart(context.Background(), stored.AccessKey, *stored.UploadID, 1, chunkBytes(9))
	require.NoError(t, err)

	_, err = f.svc.FinishChunkedUpload(context.Background(), f.user, sess.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "etag mismatch")
	_, ok := f.storage.Object(stored.AccessKey)
	assert.False(t, ok, "no object may be assembled from mismatched parts")
}

// 読み戻したバイト数が申告と違えば失敗させ、object も消す。
func TestFinishChunkedUpload_ReadBackMismatch(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, 10)
	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(10))
	require.NoError(t, err)

	f.storage.GetErr = errors.New("gone")
	_, err = f.svc.FinishChunkedUpload(context.Background(), f.user, sess.ID)
	require.Error(t, err)
	assert.Equal(t, 0, f.sessions.Count())
	assert.Contains(t, f.storage.Deletes, sess.AccessKey)
}

// 重複排除で既存ファイルが返ったら、今組み上げた object は誰からも参照されない
// ので消す。放置すると課金だけ残る。
func TestFinishChunkedUpload_DedupDeletesAssembledObject(t *testing.T) {
	f := newChunkedFixture(t)
	body := chunkBytes(10)

	sess1 := f.start(t, 10)
	_, err := f.svc.AppendChunk(context.Background(), f.user, sess1.ID, 0, body)
	require.NoError(t, err)
	first, err := f.svc.FinishChunkedUpload(context.Background(), f.user, sess1.ID)
	require.NoError(t, err)

	sess2 := f.start(t, 10)
	_, err = f.svc.AppendChunk(context.Background(), f.user, sess2.ID, 0, body)
	require.NoError(t, err)
	second, err := f.svc.FinishChunkedUpload(context.Background(), f.user, sess2.ID)
	require.NoError(t, err)

	assert.Equal(t, first.ID, second.ID, "identical content must deduplicate")
	assert.Contains(t, f.storage.Deletes, sess2.AccessKey)
	_, ok := f.storage.Object(sess2.AccessKey)
	assert.False(t, ok)
	assert.Equal(t, 0, f.sessions.Count())
}

// finish 時の Upload が gate で弾いたら、組み上げた object を残さない。
func TestFinishChunkedUpload_UploadRejectionCleansUp(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, 10)
	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(10))
	require.NoError(t, err)

	// append を通したあとに policy を厳しくして、finish 側の gate を踏ませる。
	f.roles.policies["u1"]["uploadableFileTypes"] = []string{"image/*"}

	_, err = f.svc.FinishChunkedUpload(context.Background(), f.user, sess.ID)
	assert.ErrorIs(t, err, drive.ErrUnallowedFileType)
	assert.Equal(t, 0, f.sessions.Count())
	_, ok := f.storage.Object(sess.AccessKey)
	assert.False(t, ok, "rejected upload must not leave an object behind")
}

// ---------------------------------------------------------------------------
// Abort
// ---------------------------------------------------------------------------

func TestAbortChunkedUpload_AbortsAndDeletes(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, 2*testChunkSize)
	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(testChunkSize))
	require.NoError(t, err)
	uploadID := *f.sessions.Get(sess.ID).UploadID

	require.NoError(t, f.svc.AbortChunkedUpload(context.Background(), f.user, sess.ID))
	assert.Contains(t, f.storage.Aborted, uploadID)
	assert.Equal(t, 0, f.sessions.Count())
	assert.Equal(t, 0, f.storage.OpenUploads())
}

func TestAbortChunkedUpload_OtherUserIsNotFound(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, testChunkSize)
	err := f.svc.AbortChunkedUpload(context.Background(), &model.User{ID: "u2"}, sess.ID)
	assert.ErrorIs(t, err, drive.ErrUploadSessionNotFound)
	assert.Equal(t, 1, f.sessions.Count())
}

// 期限切れでも abort は通す。拒む理由が無く、通した方が未完了アップロードが
// 早く消える。
func TestAbortChunkedUpload_ExpiredSessionStillAbortable(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, testChunkSize)
	stored := f.sessions.Get(sess.ID)
	stored.ExpiresAt = time.Now().Add(-time.Hour)
	require.NoError(t, f.sessions.Create(stored))

	require.NoError(t, f.svc.AbortChunkedUpload(context.Background(), f.user, sess.ID))
	assert.Equal(t, 0, f.sessions.Count())
}

// abort できないまま行を消すと追跡不能な孤児になるので、行は残す。
func TestAbortChunkedUpload_BackendFailureKeepsSession(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, 2*testChunkSize)
	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(testChunkSize))
	require.NoError(t, err)

	f.storage.AbortErr = errors.New("nope")
	assert.Error(t, f.svc.AbortChunkedUpload(context.Background(), f.user, sess.ID))
	assert.Equal(t, 1, f.sessions.Count())
}

// ---------------------------------------------------------------------------
// GC
// ---------------------------------------------------------------------------

func TestGCChunkedUploads_ReclaimsExpired(t *testing.T) {
	f := newChunkedFixture(t)
	expired := f.start(t, 2*testChunkSize)
	_, err := f.svc.AppendChunk(context.Background(), f.user, expired.ID, 0, chunkBytes(testChunkSize))
	require.NoError(t, err)
	uploadID := *f.sessions.Get(expired.ID).UploadID

	stored := f.sessions.Get(expired.ID)
	stored.ExpiresAt = time.Now().Add(-time.Hour)
	require.NoError(t, f.sessions.Create(stored))

	alive := f.start(t, testChunkSize)

	n, err := f.svc.GCChunkedUploads(context.Background(), time.Now(), 100)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Contains(t, f.storage.Aborted, uploadID)
	assert.Nil(t, f.sessions.Get(expired.ID))
	assert.NotNil(t, f.sessions.Get(alive.ID), "unexpired sessions must be left alone")
}

// abort に失敗した行は残して次回再試行する。消すと未完了アップロードが
// 追跡不能になり課金だけ残る。
func TestGCChunkedUploads_AbortFailureKeepsRow(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, 2*testChunkSize)
	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(testChunkSize))
	require.NoError(t, err)

	stored := f.sessions.Get(sess.ID)
	stored.ExpiresAt = time.Now().Add(-time.Hour)
	require.NoError(t, f.sessions.Create(stored))

	f.storage.AbortErr = errors.New("nope")
	n, err := f.svc.GCChunkedUploads(context.Background(), time.Now(), 100)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.NotNil(t, f.sessions.Get(sess.ID))
}

// まだ何も送っていない (uploadId が無い) 期限切れセッションは、storage を
// 触らずに行だけ消す。
func TestGCChunkedUploads_SessionWithoutUploadID(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, testChunkSize)
	stored := f.sessions.Get(sess.ID)
	stored.ExpiresAt = time.Now().Add(-time.Hour)
	require.NoError(t, f.sessions.Create(stored))

	n, err := f.svc.GCChunkedUploads(context.Background(), time.Now(), 100)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Empty(t, f.storage.Aborted)
}

func TestGCChunkedUploads_UnwiredIsNoop(t *testing.T) {
	svc, _, _ := newSvc(t)
	n, err := svc.GCChunkedUploads(context.Background(), time.Now(), 10)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestGCChunkedUploads_ListError(t *testing.T) {
	f := newChunkedFixture(t)
	f.sessions.ListErr = errors.New("db down")
	_, err := f.svc.GCChunkedUploads(context.Background(), time.Now(), 10)
	assert.Error(t, err)
}

// 最終チャンクでも chunkSize を超えさせない。超えられると「全体を 1 チャンクで
// 送る」が通り、operator が chunkSize を絞った意味が失われる。
func TestAppendChunk_ChunkNeverExceedsChunkSize(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, testChunkSize+1)

	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(testChunkSize+1))
	assert.ErrorIs(t, err, drive.ErrInvalidChunkSize)
}

// finish が走っている最中のセッションは、期限切れでも GC が触らない。撃つと
// 正常な完了を横から壊す。
func TestGCChunkedUploads_SkipsInFlightFinish(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, 2*testChunkSize)
	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(testChunkSize))
	require.NoError(t, err)

	now := time.Now()
	stored := f.sessions.Get(sess.ID)
	stored.ExpiresAt = now.Add(-time.Hour)
	stored.Finishing = true
	stored.UpdatedAt = now.Add(-time.Minute) // 直近まで動いていた
	require.NoError(t, f.sessions.Create(stored))

	n, err := f.svc.GCChunkedUploads(context.Background(), now, 100)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.NotNil(t, f.sessions.Get(sess.ID))
	assert.Empty(t, f.storage.Aborted)
}

// 猶予を過ぎた (= finish が落ちたまま放置された) セッションは回収する。
func TestGCChunkedUploads_ReclaimsStuckFinish(t *testing.T) {
	f := newChunkedFixture(t)
	sess := f.start(t, 2*testChunkSize)
	_, err := f.svc.AppendChunk(context.Background(), f.user, sess.ID, 0, chunkBytes(testChunkSize))
	require.NoError(t, err)

	now := time.Now()
	stored := f.sessions.Get(sess.ID)
	stored.ExpiresAt = now.Add(-time.Hour)
	stored.Finishing = true
	stored.UpdatedAt = now.Add(-time.Hour)
	require.NoError(t, f.sessions.Create(stored))

	n, err := f.svc.GCChunkedUploads(context.Background(), now, 100)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Len(t, f.storage.Aborted, 1)
}

// **小数の policy でもゲートが効くこと。**
//
// policy の数値は小数を取りうる (role_service.go の maxNumber が
// 「小数はそのまま float64 で返す」と明記している)。素の `.(int)` で読むと
// float64 で型アサーションに失敗し、**上限違反で弾くのではなく上限そのものが
// 消える**。通常アップロードは policyNumber で float を受けているので、
// ここが int だけだと経路差になる (#2611)。
func TestStartChunkedUpload_FractionalMaxFileSize(t *testing.T) {
	f := newChunkedFixture(t)
	// 0.5MB 上限に対し 1MB を要求する。
	f.roles.policies["u1"]["maxFileSizeMb"] = 0.5
	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Size: 1024 * 1024,
	})
	assert.ErrorIs(t, err, drive.ErrMaxFileSizeExceeded)
}

func TestStartChunkedUpload_FractionalDriveCapacity(t *testing.T) {
	f := newChunkedFixture(t)
	f.roles.policies["u1"]["driveCapacityMb"] = 1.5
	uid := "u1"
	require.NoError(t, f.files.Create(&model.DriveFile{ID: "f1", UserID: &uid, Size: 1024 * 1024}))

	// 既存 1MB + 新規 1MB = 2MB > 1.5MB。
	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Size: 1024 * 1024,
	})
	assert.ErrorIs(t, err, drive.ErrNoFreeSpace)
}

func TestStartChunkedUpload_FractionalPendingLimit(t *testing.T) {
	f := newChunkedFixture(t)
	// 他のゲートに先に引っかからないよう広げておく。
	f.roles.policies["u1"]["maxFileSizeMb"] = 100
	f.roles.policies["u1"]["driveCapacityMb"] = 1024
	f.roles.policies["u1"][role.PolicyChunkedUploadMaxPendingMb] = 1.5
	f.start(t, 1024*1024)

	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Size: 1024 * 1024,
	})
	assert.ErrorIs(t, err, drive.ErrPendingUploadLimitExceeded)
}

// 個数の policy も小数を取りうる。int に丸めると 0.5 が 0 になり
// `limit > 0` を抜けて gate ごと消えるので、float のまま比較する。
func TestStartChunkedUpload_FractionalConcurrentSessions(t *testing.T) {
	f := newChunkedFixture(t)
	f.roles.policies["u1"][role.PolicyChunkedUploadMaxConcurrentSessions] = 1.5
	f.start(t, 1024)
	// active=1 に対し上限 1.5 なのでまだ開ける。
	f.start(t, 1024)

	// active=2 >= 1.5 で弾く。
	_, err := f.svc.StartChunkedUpload(context.Background(), drive.StartChunkedUploadInput{
		User: f.user, Size: 1024,
	})
	assert.ErrorIs(t, err, drive.ErrTooManyUploadSessions)
}
