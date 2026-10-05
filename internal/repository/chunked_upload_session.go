package repository

import (
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// ChunkedUploadSessionRepository provides data access for the
// `chunked_upload_session` table (#2313).
type ChunkedUploadSessionRepository interface {
	Create(s *model.ChunkedUploadSession) error
	// FindByID returns the session, or gorm.ErrRecordNotFound. Ownership and
	// expiry are the caller's responsibility.
	FindByID(id string) (*model.ChunkedUploadSession, error)
	// SetUploadID records the backend multipart upload id and the sniffed
	// content type, guarded by `uploadId IS NULL` so that two concurrent first
	// appends cannot both claim the session. The loser gets ok=false and must
	// abort the multipart upload it created.
	//
	// これは UploadPart より前に、CreateMultipartUpload の直後に呼ぶこと。
	// 逆順にすると失敗時に「DB から追跡できない未完了マルチパートアップロード」
	// が残り、GC が abort できないまま課金が積み上がる。
	SetUploadID(id, uploadID, contentType string, now time.Time) (bool, error)
	// CommitPart records one appended part. It is guarded by
	// `receivedChunks = expectedChunks` so two concurrent appends of the same
	// index cannot both advance the session; the loser gets ok=false.
	CommitPart(id string, expectedChunks int, parts datatypes.JSON, receivedBytes int64, now time.Time) (bool, error)
	// ClaimFinish flips `finishing` false -> true. ok=false means another
	// request already claimed the session, which is how double-finish is
	// prevented.
	ClaimFinish(id string, now time.Time) (bool, error)
	// ReleaseFinish clears the claim so a failed finish can be retried.
	ReleaseFinish(id string, now time.Time) error
	Delete(id string) error
	// CountActiveByUser counts non-expired sessions owned by userID.
	CountActiveByUser(userID string, now time.Time) (int64, error)
	// PendingBytesByUser sums the declared totalSize of the user's non-expired
	// sessions. Used to reserve drive capacity so that opening many sessions
	// cannot bypass driveCapacityMb.
	PendingBytesByUser(userID string, now time.Time) (int64, error)
	// ListExpired returns sessions past their expiry, oldest first, so the GC
	// can abort their multipart uploads before deleting the rows.
	ListExpired(now time.Time, limit int) ([]*model.ChunkedUploadSession, error)
}

type chunkedUploadSessionRepository struct {
	db *gorm.DB
}

// NewChunkedUploadSessionRepository constructs the default repository.
func NewChunkedUploadSessionRepository(db *gorm.DB) ChunkedUploadSessionRepository {
	return &chunkedUploadSessionRepository{db: db}
}

func (r *chunkedUploadSessionRepository) Create(s *model.ChunkedUploadSession) error {
	return r.db.Create(s).Error
}

func (r *chunkedUploadSessionRepository) FindByID(id string) (*model.ChunkedUploadSession, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var s model.ChunkedUploadSession
	if err := r.db.Where(`"id" = ?`, id).First(&s).Error; err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *chunkedUploadSessionRepository) SetUploadID(id, uploadID, contentType string, now time.Time) (bool, error) {
	res := r.db.Model(&model.ChunkedUploadSession{}).
		Where(`"id" = ? AND "uploadId" IS NULL`, id).
		Updates(map[string]any{
			"uploadId":    uploadID,
			"contentType": contentType,
			"updatedAt":   now,
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

func (r *chunkedUploadSessionRepository) CommitPart(id string, expectedChunks int, parts datatypes.JSON, receivedBytes int64, now time.Time) (bool, error) {
	res := r.db.Model(&model.ChunkedUploadSession{}).
		Where(`"id" = ? AND "receivedChunks" = ? AND "finishing" = false`, id, expectedChunks).
		Updates(map[string]any{
			"parts":          parts,
			"receivedBytes":  receivedBytes,
			"receivedChunks": expectedChunks + 1,
			"updatedAt":      now,
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

func (r *chunkedUploadSessionRepository) ClaimFinish(id string, now time.Time) (bool, error) {
	res := r.db.Model(&model.ChunkedUploadSession{}).
		Where(`"id" = ? AND "finishing" = false`, id).
		Updates(map[string]any{"finishing": true, "updatedAt": now})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

func (r *chunkedUploadSessionRepository) ReleaseFinish(id string, now time.Time) error {
	return r.db.Model(&model.ChunkedUploadSession{}).
		Where(`"id" = ?`, id).
		Updates(map[string]any{"finishing": false, "updatedAt": now}).Error
}

func (r *chunkedUploadSessionRepository) Delete(id string) error {
	return r.db.Where(`"id" = ?`, id).Delete(&model.ChunkedUploadSession{}).Error
}

func (r *chunkedUploadSessionRepository) CountActiveByUser(userID string, now time.Time) (int64, error) {
	var n int64
	err := r.db.Model(&model.ChunkedUploadSession{}).
		Where(`"userId" = ? AND "expiresAt" > ?`, userID, now).
		Count(&n).Error
	return n, err
}

func (r *chunkedUploadSessionRepository) PendingBytesByUser(userID string, now time.Time) (int64, error) {
	// COALESCE で 0 行のときに NULL が返るのを避ける (Scan 先が int64 なので
	// NULL だと変換に失敗する)。
	var total int64
	err := r.db.Model(&model.ChunkedUploadSession{}).
		Select(`COALESCE(SUM("totalSize"), 0)`).
		Where(`"userId" = ? AND "expiresAt" > ?`, userID, now).
		Scan(&total).Error
	return total, err
}

func (r *chunkedUploadSessionRepository) ListExpired(now time.Time, limit int) ([]*model.ChunkedUploadSession, error) {
	if limit <= 0 {
		limit = 100
	}
	var out []*model.ChunkedUploadSession
	err := r.db.Where(`"expiresAt" <= ?`, now).
		Order(`"expiresAt" ASC`).
		Limit(limit).
		Find(&out).Error
	return out, err
}
