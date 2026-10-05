package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// AccessTokenRepository provides data access for access tokens.
type AccessTokenRepository interface {
	FindByHash(hash string) (*model.AccessToken, error)
	// FindByHashOrToken は middleware 用の dual lookup。
	// upstream Misskey TS の AuthenticateService が
	//   WHERE hash = ? OR token = ?
	// で参照している pattern を 1 query に集約する。
	//
	//  - miauth/gen-token は hash = sha256(token) で保存 → hash 列で hit
	//  - auth/accept は hash = sha256(token + app.secret) で保存 → token 列で hit
	//
	// 2 経路を 1 query にすることで middleware ホットパスでの追加 round trip を
	// 避けつつ、app-issued token も miauth token も一律で resolve できる (#910)。
	FindByHashOrToken(hash, rawToken string) (*model.AccessToken, error)
	FindByID(id string) (*model.AccessToken, error)
	ListByUserID(userID string) ([]*model.AccessToken, error)
	// ListAuthorizedApps は /i/authorized-apps 用。upstream は
	// accessTokensRepository.find({where:{userId, appId: Not(IsNull())}, take,
	// skip, order:{id}}) で「app に紐づく token のみ」を limit/offset/sort 付きで
	// 返す (authorized-apps.ts)。App を Preload して呼び出し側が App entity を
	// 組み立てられるようにする。sortAsc=false で id DESC (= sort 'desc' / default)。
	ListAuthorizedApps(userID string, limit, offset int, sortAsc bool) ([]*model.AccessToken, error)
	// ListByUserIDPreloadApp は /i/apps 用に App を JOIN し、sort で
	// 順序を制御して返す。sort 値 (Misskey TS 互換):
	//   "+createdAt" → token.id DESC (新しい順)
	//   "-createdAt" → token.id ASC  (古い順)
	//   "+lastUsedAt" → lastUsedAt DESC
	//   "-lastUsedAt" → lastUsedAt ASC
	//   それ以外 ("" 含む) → token.id ASC (TS の default)
	ListByUserIDPreloadApp(userID, sort string) ([]*model.AccessToken, error)
	DeleteByID(id string) error
	// Create inserts a new access token. Used by the OAuth2 provider to mint a
	// token after a successful authorization-code exchange (#1899)。
	Create(token *model.AccessToken) error
}

type accessTokenRepository struct {
	db *gorm.DB
}

// NewAccessTokenRepository creates a new AccessTokenRepository.
func NewAccessTokenRepository(db *gorm.DB) AccessTokenRepository {
	return &accessTokenRepository{db: db}
}

func (r *accessTokenRepository) Create(token *model.AccessToken) error {
	return r.db.Create(token).Error
}

func (r *accessTokenRepository) FindByHash(hash string) (*model.AccessToken, error) {
	if !storable(hash) {
		return nil, ErrNotFound
	}
	var token model.AccessToken
	if err := r.db.Where("hash = ?", hash).Preload("User").First(&token).Error; err != nil {
		return nil, err
	}
	return &token, nil
}

func (r *accessTokenRepository) FindByHashOrToken(hash, rawToken string) (*model.AccessToken, error) {
	if !storable(hash) || !storable(rawToken) {
		return nil, ErrNotFound
	}
	var token model.AccessToken
	if err := r.db.
		Where(`"hash" = ? OR "token" = ?`, hash, rawToken).
		Preload("User").
		First(&token).Error; err != nil {
		return nil, err
	}
	return &token, nil
}

func (r *accessTokenRepository) FindByID(id string) (*model.AccessToken, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var token model.AccessToken
	if err := r.db.Where(`"id" = ?`, id).First(&token).Error; err != nil {
		return nil, err
	}
	return &token, nil
}

// ListByUserID returns all access tokens owned by the given user. Used
// by i/authorized-apps; ordering mirrors upstream (newest first).
func (r *accessTokenRepository) ListByUserID(userID string) ([]*model.AccessToken, error) {
	var tokens []*model.AccessToken
	if err := r.db.Where(`"userId" = ?`, userID).Order(`"id" DESC`).Find(&tokens).Error; err != nil {
		return nil, err
	}
	return tokens, nil
}

// ListAuthorizedApps は /i/authorized-apps 用。appId IS NOT NULL で raw token /
// miauth (appId NULL) を除外し、limit/offset/sort 付きで返す。Preload("App") で
// app entity を組み立てられるようにする。
func (r *accessTokenRepository) ListAuthorizedApps(userID string, limit, offset int, sortAsc bool) ([]*model.AccessToken, error) {
	order := `"id" DESC`
	if sortAsc {
		order = `"id" ASC`
	}
	q := r.db.
		Where(`"userId" = ? AND "appId" IS NOT NULL`, userID).
		Preload("App").
		Order(order).
		Limit(limit)
	if offset > 0 {
		q = q.Offset(offset)
	}
	var tokens []*model.AccessToken
	if err := q.Find(&tokens).Error; err != nil {
		return nil, err
	}
	return tokens, nil
}

// ListByUserIDPreloadApp は /i/apps 用。Preload("App") で MiAuth app メタを
// 同時取得し、sort パラメータで本家互換の順序付けを行う。
func (r *accessTokenRepository) ListByUserIDPreloadApp(userID, sort string) ([]*model.AccessToken, error) {
	q := r.db.Where(`"userId" = ?`, userID).Preload("App")
	switch sort {
	case "+createdAt":
		q = q.Order(`"id" DESC`)
	case "-createdAt":
		q = q.Order(`"id" ASC`)
	case "+lastUsedAt":
		q = q.Order(`"lastUsedAt" DESC`)
	case "-lastUsedAt":
		q = q.Order(`"lastUsedAt" ASC`)
	default:
		// TS の default は token.id ASC (古い順)。Misskey TS 本家:
		// packages/backend/src/server/api/endpoints/i/apps.ts:88
		q = q.Order(`"id" ASC`)
	}
	var tokens []*model.AccessToken
	if err := q.Find(&tokens).Error; err != nil {
		return nil, err
	}
	return tokens, nil
}

// DeleteByID removes an access token. i/revoke-token の権限チェックは呼び出し
// 側 (handler) の責務で、ここでは id 一致のみ扱う。
func (r *accessTokenRepository) DeleteByID(id string) error {
	if !storable(id) {
		// 一致する行が無いのと同じ扱い。GORM の Delete は 0 行でも error を
		// 返さないので、ここも nil を返す (#3025)。
		return nil
	}
	return r.db.Where(`"id" = ?`, id).Delete(&model.AccessToken{}).Error
}
