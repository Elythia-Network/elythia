package repository

import (
	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// UserKeypairRepository provides data access for the `user_keypair` table.
type UserKeypairRepository interface {
	Create(k *model.UserKeypair) error
	FindByUserID(userID string) (*model.UserKeypair, error)
	// NormalizePrivateKeysToPKCS8 rewrites PKCS#1-encoded private keys to
	// PKCS#8 in place. Returns the number of rows converted.
	NormalizePrivateKeysToPKCS8() (int, error)
}

type userKeypairRepository struct {
	db *gorm.DB
}

// NewUserKeypairRepository creates a new UserKeypairRepository.
func NewUserKeypairRepository(db *gorm.DB) UserKeypairRepository {
	return &userKeypairRepository{db: db}
}

func (r *userKeypairRepository) Create(k *model.UserKeypair) error {
	return r.db.Create(k).Error
}

func (r *userKeypairRepository) FindByUserID(userID string) (*model.UserKeypair, error) {
	if !storable(userID) {
		return nil, ErrNotFound
	}
	var k model.UserKeypair
	if err := r.db.First(&k, "\"userId\" = ?", userID).Error; err != nil {
		return nil, err
	}
	return &k, nil
}

// NormalizePrivateKeysToPKCS8 converts every `BEGIN RSA PRIVATE KEY` (PKCS#1)
// row to `BEGIN PRIVATE KEY` (PKCS#8) in place.
//
// mk-go は 000072 以前、ローカルユーザーの RSA 秘密鍵を PKCS#1 で発行していた
// (#2378)。mk-go 自身は両形式を読めるので動いてしまうが、**Misskey TS は
// PKCS#8 しか読めない** (署名は Rust 製 slacc の RsaKeyPair.fromPem で、
// PKCS#1 では `no items found` になる)。そのまま TS へ引き渡すと、そのユーザー
// の送信側の連合が全滅する。受信は動くので片方向だけ静かに壊れる。TS へ引き渡す
// こと (復路) は保証しなくなった (#3191) が、鍵の形式は本家と揃えておく。
//
// 変換するのは **PEM のエンコーディングだけ**で、鍵そのものは変わらない。
// 公開鍵も鍵 ID も不変なので、連合相手から見て何も変わらない。
//
// SQL では ASN.1 の再構成ができないため migration ファイルではなく起動時の
// backfill として実装している (instance.RecomputeFollowCounts と同じ方式)。
// 冪等なので何度実行してもよい。
func (r *userKeypairRepository) NormalizePrivateKeysToPKCS8() (int, error) {
	var rows []model.UserKeypair
	// PKCS#1 の行だけを対象にする。全件読むとユーザー数に比例して重くなる。
	if err := r.db.Where(`"privateKey" LIKE ?`, "%BEGIN RSA PRIVATE KEY%").Find(&rows).Error; err != nil {
		return 0, err
	}
	converted := 0
	for _, row := range rows {
		pkcs8, err := activitypub.ConvertRSAPrivateKeyToPKCS8(row.PrivateKey)
		if err != nil {
			// 1 行の破損で全体を止めない。該当ユーザーだけ変換されずに残る。
			slog.Warn("user_keypair: PKCS#8 変換に失敗", "userId", row.UserID, "err", err)
			continue
		}
		if err := r.db.Model(&model.UserKeypair{}).
			Where(`"userId" = ?`, row.UserID).
			Update("privateKey", pkcs8).Error; err != nil {
			slog.Warn("user_keypair: PKCS#8 書き戻しに失敗", "userId", row.UserID, "err", err)
			continue
		}
		converted++
	}
	return converted, nil
}
