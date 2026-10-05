package repository

import (
	"time"

	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/model"
)

// EmojiApplicationQuotaResetRepository reads and writes
// `emoji_application_quota_reset` rows (#2962).
type EmojiApplicationQuotaResetRepository interface {
	Create(row *model.EmojiApplicationQuotaReset) error
	// LatestByUser returns the most recent reset, or nil when the user has
	// never had one.
	//
	// **無い状態と障害を区別する。** nil を「リセット無し」に丸めると、DB 障害の
	// ときに枠が満杯のまま見え (読み取り側)、あるいはリセットが無かったことに
	// されて申請が弾かれる (作成側)。err はそのまま返す。
	LatestByUser(userID string) (*model.EmojiApplicationQuotaReset, error)
}

type emojiApplicationQuotaResetRepository struct {
	db *gorm.DB
}

// NewEmojiApplicationQuotaResetRepository constructs the GORM-backed repository.
func NewEmojiApplicationQuotaResetRepository(db *gorm.DB) EmojiApplicationQuotaResetRepository {
	return &emojiApplicationQuotaResetRepository{db: db}
}

func (r *emojiApplicationQuotaResetRepository) Create(row *model.EmojiApplicationQuotaReset) error {
	return r.db.Create(row).Error
}

func (r *emojiApplicationQuotaResetRepository) LatestByUser(userID string) (*model.EmojiApplicationQuotaReset, error) {
	var rows []model.EmojiApplicationQuotaReset
	// **`First` を使わない。** 見つからないときに `ErrRecordNotFound` を返すので、
	// 呼び出し側が「無い」と「壊れている」を毎回ほどくことになる。ここは
	// 「無い = nil」が自然な問い合わせ。
	if err := r.db.
		Where(`"userId" = ?`, userID).
		// **id でも切る (レビュー L4)。** 同一時刻の 2 行があると、どちらが
		// 「最後のリセット」として表示されるかがプラン依存になる。境界の値は
		// 同じなので枠の計算は変わらないが、理由と実行者が入れ替わって見える。
		Order(`"createdAt" DESC, "id" DESC`).
		Limit(1).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// quotaWindowSince returns the start of the window, taking a manual reset into
// account (#2962).
//
// **`max(期間の開始, リセット)` を採る。** リセットは「それ以前の申請を数えない」
// という意味なので、窓の開始より後ろにあるときだけ効く。逆に窓の開始より古い
// リセットは既に窓の外なので何もしない。
func quotaWindowSince(now time.Time, d time.Duration, resetAt time.Time) time.Time {
	since := now.Add(-d)
	if resetAt.After(since) {
		return resetAt
	}
	return since
}
