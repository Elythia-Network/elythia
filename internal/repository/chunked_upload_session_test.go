package repository

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func newChunkedSession(id, userID string, expiresAt time.Time) *model.ChunkedUploadSession {
	now := time.Now()
	return &model.ChunkedUploadSession{
		ID:        id,
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: expiresAt,
		UserID:    userID,
		Name:      "video.bin",
		TotalSize: 100,
		ChunkSize: 5 * 1024 * 1024,
		AccessKey: "ak-" + id,
		Parts:     datatypes.JSON("[]"),
	}
}

func insertChunkedSession(t *testing.T, repo ChunkedUploadSessionRepository, id, userID string, expiresAt time.Time) *model.ChunkedUploadSession {
	t.Helper()
	s := newChunkedSession(id, userID, expiresAt)
	require.NoError(t, repo.Create(s))
	t.Cleanup(func() { _ = repo.Delete(id) })
	return s
}

func TestChunkedUploadSessionRepository_CreateAndFind(t *testing.T) {
	repo := NewChunkedUploadSessionRepository(testDB)
	u := insertTestUser(t, "cus_find", "cusfind")
	defer cleanupUser(t, u.ID)

	s := insertChunkedSession(t, repo, "cus1", u.ID, time.Now().Add(time.Hour))

	got, err := repo.FindByID(s.ID)
	require.NoError(t, err)
	assert.Equal(t, u.ID, got.UserID)
	assert.Equal(t, int64(100), got.TotalSize)
	assert.Nil(t, got.UploadID, "uploadId は最初の append まで NULL")
	assert.False(t, got.Finishing)

	_, err = repo.FindByID("nope")
	assert.True(t, IsNotFound(err))
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

// uploadId は `uploadId IS NULL` を条件に一度だけ確定できる。並行した最初の
// append が両方 CreateMultipartUpload を通しても、記録できるのは片方だけ。
func TestChunkedUploadSessionRepository_SetUploadIDIsClaimedOnce(t *testing.T) {
	repo := NewChunkedUploadSessionRepository(testDB)
	u := insertTestUser(t, "cus_set", "cusset")
	defer cleanupUser(t, u.ID)
	s := insertChunkedSession(t, repo, "cus_set1", u.ID, time.Now().Add(time.Hour))

	ok, err := repo.SetUploadID(s.ID, "upload-a", "video/mp4", time.Now())
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = repo.SetUploadID(s.ID, "upload-b", "video/mp4", time.Now())
	require.NoError(t, err)
	assert.False(t, ok, "second claim must lose")

	got, err := repo.FindByID(s.ID)
	require.NoError(t, err)
	require.NotNil(t, got.UploadID)
	assert.Equal(t, "upload-a", *got.UploadID)
	assert.Equal(t, "video/mp4", *got.ContentType)
}

// CommitPart は receivedChunks が期待値と一致するときだけ通る。同じ index を
// 送った 2 本目は弾かれる (= append の順序が構造的に守られる)。
func TestChunkedUploadSessionRepository_CommitPartIsGuarded(t *testing.T) {
	repo := NewChunkedUploadSessionRepository(testDB)
	u := insertTestUser(t, "cus_commit", "cuscommit")
	defer cleanupUser(t, u.ID)
	s := insertChunkedSession(t, repo, "cus_commit1", u.ID, time.Now().Add(time.Hour))

	parts := datatypes.JSON(`[{"index":0,"etag":"e1","size":40,"sha256":"h1"}]`)
	ok, err := repo.CommitPart(s.ID, 0, parts, 40, time.Now())
	require.NoError(t, err)
	assert.True(t, ok)

	// 同じ expected で 2 回目は通らない。
	ok, err = repo.CommitPart(s.ID, 0, parts, 40, time.Now())
	require.NoError(t, err)
	assert.False(t, ok)

	got, err := repo.FindByID(s.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, got.ReceivedChunks)
	assert.Equal(t, int64(40), got.ReceivedBytes)

	// 先の index も飛ばせない。
	ok, err = repo.CommitPart(s.ID, 3, parts, 200, time.Now())
	require.NoError(t, err)
	assert.False(t, ok)
}

// finish 中のセッションには append できない。
func TestChunkedUploadSessionRepository_CommitPartBlockedWhileFinishing(t *testing.T) {
	repo := NewChunkedUploadSessionRepository(testDB)
	u := insertTestUser(t, "cusfinblk", "cusfinblk")
	defer cleanupUser(t, u.ID)
	s := insertChunkedSession(t, repo, "cusfinblk1", u.ID, time.Now().Add(time.Hour))

	ok, err := repo.ClaimFinish(s.ID, time.Now())
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = repo.CommitPart(s.ID, 0, datatypes.JSON("[]"), 10, time.Now())
	require.NoError(t, err)
	assert.False(t, ok)
}

// finish の claim は 1 本だけ通る。これが DriveFile の二重作成を防ぐ。
func TestChunkedUploadSessionRepository_ClaimAndReleaseFinish(t *testing.T) {
	repo := NewChunkedUploadSessionRepository(testDB)
	u := insertTestUser(t, "cus_claim", "cusclaim")
	defer cleanupUser(t, u.ID)
	s := insertChunkedSession(t, repo, "cus_claim1", u.ID, time.Now().Add(time.Hour))

	ok, err := repo.ClaimFinish(s.ID, time.Now())
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = repo.ClaimFinish(s.ID, time.Now())
	require.NoError(t, err)
	assert.False(t, ok, "concurrent finish must lose the claim")

	require.NoError(t, repo.ReleaseFinish(s.ID, time.Now()))
	ok, err = repo.ClaimFinish(s.ID, time.Now())
	require.NoError(t, err)
	assert.True(t, ok, "released claim must be retryable")
}

// 期限切れセッションは同時数・未完了バイト数のどちらにも数えない。数えると
// 放置セッションで枠が埋まったまま復旧しなくなる。
func TestChunkedUploadSessionRepository_QuotaCountersIgnoreExpired(t *testing.T) {
	repo := NewChunkedUploadSessionRepository(testDB)
	u := insertTestUser(t, "cus_quota", "cusquota")
	other := insertTestUser(t, "cus_quota2", "cusquota2")
	defer cleanupUser(t, u.ID)
	defer cleanupUser(t, other.ID)

	now := time.Now()
	insertChunkedSession(t, repo, "cus_q1", u.ID, now.Add(time.Hour))
	insertChunkedSession(t, repo, "cus_q2", u.ID, now.Add(time.Hour))
	insertChunkedSession(t, repo, "cus_q3", u.ID, now.Add(-time.Hour)) // 期限切れ
	insertChunkedSession(t, repo, "cus_q4", other.ID, now.Add(time.Hour))

	n, err := repo.CountActiveByUser(u.ID, now)
	require.NoError(t, err)
	assert.EqualValues(t, 2, n)

	total, err := repo.PendingBytesByUser(u.ID, now)
	require.NoError(t, err)
	assert.EqualValues(t, 200, total, "totalSize 100 x 2 (期限切れと他人は除く)")

	// 0 行でも NULL ではなく 0 が返ること (COALESCE)。
	total, err = repo.PendingBytesByUser("no-such-user", now)
	require.NoError(t, err)
	assert.EqualValues(t, 0, total)
}

func TestChunkedUploadSessionRepository_ListExpired(t *testing.T) {
	repo := NewChunkedUploadSessionRepository(testDB)
	u := insertTestUser(t, "cus_exp", "cusexp")
	defer cleanupUser(t, u.ID)

	now := time.Now()
	insertChunkedSession(t, repo, "cus_e1", u.ID, now.Add(-2*time.Hour))
	insertChunkedSession(t, repo, "cus_e2", u.ID, now.Add(-time.Hour))
	insertChunkedSession(t, repo, "cus_e3", u.ID, now.Add(time.Hour))

	got, err := repo.ListExpired(now, 100)
	require.NoError(t, err)
	ids := map[string]bool{}
	for _, s := range got {
		ids[s.ID] = true
	}
	assert.True(t, ids["cus_e1"])
	assert.True(t, ids["cus_e2"])
	assert.False(t, ids["cus_e3"], "unexpired sessions must not be listed")

	// limit が効くこと。GC が 1 回で無制限に舐めないための bound。
	got, err = repo.ListExpired(now, 1)
	require.NoError(t, err)
	assert.Len(t, got, 1)

	// limit <= 0 は既定値に倒れる (呼び出し側の指定漏れで無制限にしない)。
	got, err = repo.ListExpired(now, 0)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(got), 2)
}

func TestChunkedUploadSessionRepository_Delete(t *testing.T) {
	repo := NewChunkedUploadSessionRepository(testDB)
	u := insertTestUser(t, "cus_del", "cusdel")
	defer cleanupUser(t, u.ID)
	s := insertChunkedSession(t, repo, "cus_del1", u.ID, time.Now().Add(time.Hour))

	require.NoError(t, repo.Delete(s.ID))
	_, err := repo.FindByID(s.ID)
	assert.True(t, IsNotFound(err))
	// 冪等: 既に消えていてもエラーにしない。
	assert.NoError(t, repo.Delete(s.ID))
}

// user 削除で行が CASCADE 消滅しないこと。消えると S3 の未完了マルチパート
// アップロードが AbortMultipartUpload されないまま孤児になり課金が残る。
func TestChunkedUploadSessionRepository_SurvivesUserDeletion(t *testing.T) {
	repo := NewChunkedUploadSessionRepository(testDB)
	u := insertTestUser(t, "cus_cascade", "cuscascade")
	s := insertChunkedSession(t, repo, "cus_cascade1", u.ID, time.Now().Add(time.Hour))

	cleanupUser(t, u.ID)

	got, err := repo.FindByID(s.ID)
	require.NoError(t, err, "session must outlive its user so the GC can abort the multipart upload")
	assert.Equal(t, u.ID, got.UserID)
}
