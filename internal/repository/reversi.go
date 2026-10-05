package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// ReversiRepository handles reversi_game persistence.
type ReversiRepository interface {
	Create(game *model.ReversiGame) error
	FindByID(id string) (*model.ReversiGame, error)
	Update(game *model.ReversiGame) error
	// UpdateReadyState atomically sets one player's ready flag with a
	// pre-start guard and returns the freshly reloaded row. Returns
	// (nil, nil) when the guard fails (game already started/ended or the
	// row is gone). ローカルWSと連合inboxの並行readyがfull-row Saveで互いの
	// フラグを上書きするlost update (#1626) を防ぐため、自分のカラムだけを
	// 単一UPDATEで書く。
	UpdateReadyState(gameID string, user1 bool, ready bool) (*model.ReversiGame, error)
	// MarkStarted atomically claims the not-started -> started transition,
	// persisting the start fields (black / isStarted / startedAt / crc32)
	// only when the row is not yet started. Returns whether this caller won
	// the claim. 並行UpdateReadyの両者がboth-readyを観測してStartGameに
	// 二重到達したとき、startedイベントを一度だけ発火させるための排他 (#1626)。
	MarkStarted(game *model.ReversiGame) (bool, error)
	ListByUser(userID string, limit int) ([]*model.ReversiGame, error)
	// FindPendingInvitation returns the newest not-yet-started invitation
	// from inviter to invitee, or nil when there is none.
	//
	// **一覧の窓で代用しない。** 招待の重複排除を `ListByUser(invitee, 20)` の
	// 走査で済ませていたため、**別の actor が 20 件流し込むだけで重複排除が
	// 破れ**、同じ相手からの再送が新しい行を作れるようになっていた
	// (1 ペアあたりの上限が消える)。条件を SQL に渡せば窓に依存しない。
	FindPendingInvitation(inviteeID, inviterID string) (*model.ReversiGame, error)
	// ListByUserCursor returns user's games (User1 or User2) with keyset
	// pagination via sinceID / untilID。limit は上限 (0 → 10)。並び順は
	// paginationOrder に従う (sinceID 単独指定のみ id 昇順)。
	ListByUserCursor(userID, sinceID, untilID string, limit int) ([]*model.ReversiGame, error)
	ListActive() ([]*model.ReversiGame, error)
	// ListStartedCursor returns only started games with keyset pagination。
	// "my=false" な /reversi/games 用。
	ListStartedCursor(sinceID, untilID string, limit int) ([]*model.ReversiGame, error)
	Delete(id string) error
	// DeleteOutdatedGames removes not-yet-started games whose id is older than
	// thresholdID. Mirrors upstream reversiService.cleanOutdatedGames (id <
	// idService.gen(now-10min) AND isStarted=false): abandoned matchmaking
	// games never started within ~10 minutes are pruned by the clean cron
	// (#1563). Returns rows deleted.
	DeleteOutdatedGames(thresholdID string) (int64, error)
}

type reversiRepository struct {
	db *gorm.DB
}

// NewReversiRepository creates a new ReversiRepository.
func NewReversiRepository(db *gorm.DB) ReversiRepository {
	return &reversiRepository{db: db}
}

func (r *reversiRepository) Create(game *model.ReversiGame) error {
	return r.db.Create(game).Error
}

func (r *reversiRepository) DeleteOutdatedGames(thresholdID string) (int64, error) {
	res := r.db.Where(`"id" < ? AND "isStarted" = ?`, thresholdID, false).Delete(&model.ReversiGame{})
	return res.RowsAffected, res.Error
}

func (r *reversiRepository) FindByID(id string) (*model.ReversiGame, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var game model.ReversiGame
	if err := r.db.Preload("User1").Preload("User2").Where(`"id" = ?`, id).First(&game).Error; err != nil {
		return nil, err
	}
	return &game, nil
}

// FindByFederationID resolves the reversi game row identified by a federation
// session ID (populated when a match crosses an AP boundary). Used by the
// inbox processor to route incoming Invite/Join/Update/Leave activities.
func (r *reversiRepository) FindByFederationID(federationID string) (*model.ReversiGame, error) {
	if !storable(federationID) {
		return nil, ErrNotFound
	}
	var game model.ReversiGame
	if err := r.db.Preload("User1").Preload("User2").
		Where(`"federationId" = ?`, federationID).
		First(&game).Error; err != nil {
		return nil, err
	}
	return &game, nil
}

func (r *reversiRepository) Update(game *model.ReversiGame) error {
	return r.db.Save(game).Error
}

func (r *reversiRepository) UpdateReadyState(gameID string, user1 bool, ready bool) (*model.ReversiGame, error) {
	column := "user2Ready"
	if user1 {
		column = "user1Ready"
	}
	// started/ended後のready変更は無視する (本家gameReadyと同じセマンティクス)。
	// guardをUPDATE自体に含めることで、pre-checkとの間のTOCTOUも閉じる。
	res := r.db.Model(&model.ReversiGame{}).
		Where(`"id" = ? AND "isStarted" = false AND "isEnded" = false`, gameID).
		UpdateColumn(column, ready)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, nil
	}
	// 書き込み後に読み直すことで、相手側の並行更新がcommit済みなら必ず観測
	// できる。両者の更新がどう交錯しても、少なくとも後にre-readした側は
	// both-ready=trueを見る。
	return r.FindByID(gameID)
}

func (r *reversiRepository) MarkStarted(game *model.ReversiGame) (bool, error) {
	res := r.db.Model(&model.ReversiGame{}).
		Where(`"id" = ? AND "isStarted" = false`, game.ID).
		Updates(map[string]any{
			"black":     game.Black,
			"isStarted": true,
			"startedAt": game.StartedAt,
			"crc32":     game.CRC32,
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func (r *reversiRepository) ListByUser(userID string, limit int) ([]*model.ReversiGame, error) {
	if limit <= 0 {
		limit = 10
	}
	var games []*model.ReversiGame
	if err := r.db.Preload("User1").Preload("User2").
		Where(`"user1Id" = ? OR "user2Id" = ?`, userID, userID).
		Order(`"id" DESC`).Limit(limit).Find(&games).Error; err != nil {
		return nil, err
	}
	return games, nil
}

func (r *reversiRepository) FindPendingInvitation(inviteeID, inviterID string) (*model.ReversiGame, error) {
	if !storable(inviteeID) || !storable(inviterID) {
		// この関数は「無い」を `(nil, nil)` で表す (下の `IsNotFound` 分岐と同じ)。
		// ここだけ error を返すと呼び出し側の扱いが分かれる (#3025)。
		return nil, nil
	}
	var game model.ReversiGame
	err := r.db.Preload("User1").Preload("User2").
		Where(`"user1Id" = ? AND "user2Id" = ? AND "isStarted" = false AND "isEnded" = false`,
			inviterID, inviteeID).
		Order(`"id" DESC`).First(&game).Error
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		// DB 障害を not-found に丸めない (#2792)。
		return nil, err
	}
	return &game, nil
}

func (r *reversiRepository) ListActive() ([]*model.ReversiGame, error) {
	var games []*model.ReversiGame
	if err := r.db.Preload("User1").Preload("User2").
		Where(`"isEnded" = false`).
		Order(`"id" DESC`).Limit(50).Find(&games).Error; err != nil {
		return nil, err
	}
	return games, nil
}

func (r *reversiRepository) ListByUserCursor(userID, sinceID, untilID string, limit int) ([]*model.ReversiGame, error) {
	if limit <= 0 {
		limit = 10
	}
	q := r.db.Preload("User1").Preload("User2").
		Where(`"user1Id" = ? OR "user2Id" = ?`, userID, userID)
	if sinceID != "" {
		q = q.Where(`"id" > ?`, sinceID)
	}
	if untilID != "" {
		q = q.Where(`"id" < ?`, untilID)
	}
	var games []*model.ReversiGame
	// upstream reversi/games.ts は makePaginationQuery を使う (#2713)。
	if err := q.Order(paginationOrder(sinceID, untilID, `"id"`)).Limit(limit).Find(&games).Error; err != nil {
		return nil, err
	}
	return games, nil
}

func (r *reversiRepository) ListStartedCursor(sinceID, untilID string, limit int) ([]*model.ReversiGame, error) {
	if limit <= 0 {
		limit = 10
	}
	q := r.db.Preload("User1").Preload("User2").Where(`"isStarted" = true`)
	if sinceID != "" {
		q = q.Where(`"id" > ?`, sinceID)
	}
	if untilID != "" {
		q = q.Where(`"id" < ?`, untilID)
	}
	var games []*model.ReversiGame
	// upstream reversi/games.ts は makePaginationQuery を使う (#2713)。
	if err := q.Order(paginationOrder(sinceID, untilID, `"id"`)).Limit(limit).Find(&games).Error; err != nil {
		return nil, err
	}
	return games, nil
}

func (r *reversiRepository) Delete(id string) error {
	return r.db.Where(`"id" = ?`, id).Delete(&model.ReversiGame{}).Error
}
