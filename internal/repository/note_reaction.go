package repository

import (
	"errors"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// ErrDuplicateReaction is returned by NoteReactionRepository.Create when the
// (userId, noteId) unique constraint is violated.
var ErrDuplicateReaction = errors.New("user has already reacted to this note")

// NoteReactionRepository provides data access for note_reaction rows.
type NoteReactionRepository interface {
	Create(r *model.NoteReaction) error
	// Delete removes the reaction row and returns the number of affected rows so
	// callers can detect concurrent deletes (#2106 L40: avoid double count-decrement).
	Delete(r *model.NoteReaction) (int64, error)
	FindByPair(userID, noteID string) (*model.NoteReaction, error)
	// FindByUserAndNoteIDs returns reactions for a user across multiple notes.
	// noteIDをキーとしたmapを返す。
	FindByUserAndNoteIDs(userID string, noteIDs []string) (map[string]*model.NoteReaction, error)
	ListByNoteID(noteID string, untilID, sinceID string, limit int, reactions []string) ([]*model.NoteReaction, error)
	// ListReactionPairs returns up to `limit` "<userId>/<reaction>" pairs for a
	// note (oldest first), mirroring upstream's reactionAndUserPairCache capped at
	// PER_NOTE_REACTION_USER_PAIR_CACHE_MAX. Used by the streaming note publisher
	// to fill the renote embed pair cache for per-subscriber renote.myReaction
	// (#2067)。note_reaction 行は reaction 時に即 persist されるため count buffer 中の
	// reaction も含まれる。User は preload しない (userId だけ要るため)。
	ListReactionPairs(noteID string, limit int) ([]string, error)
	// ListByUserID returns reactions made by the given user, with User and Note
	// preloaded for packing. upstream Misskey TS の users/reactions endpoint
	// 用 (reactor 視点の reaction list)。viewerID は閲覧者 (空文字列で匿名) で、
	// upstream の generateVisibilityQuery と同じく viewer が見られない note
	// (followers/specified) の reaction を SQL 段階で除外する。
	ListByUserID(userID, viewerID, untilID, sinceID string, limit int) ([]*model.NoteReaction, error)
}

type noteReactionRepository struct {
	db *gorm.DB
}

// NewNoteReactionRepository creates a new NoteReactionRepository.
func NewNoteReactionRepository(db *gorm.DB) NoteReactionRepository {
	return &noteReactionRepository{db: db}
}

func (r *noteReactionRepository) Create(rec *model.NoteReaction) error {
	if err := r.db.Create(rec).Error; err != nil {
		// PostgreSQLの一意制約違反 (23505) を専用エラーに変換する
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrDuplicateReaction
		}
		return err
	}
	return nil
}

func (r *noteReactionRepository) Delete(rec *model.NoteReaction) (int64, error) {
	result := r.db.Delete(rec)
	return result.RowsAffected, result.Error
}

func (r *noteReactionRepository) FindByPair(userID, noteID string) (*model.NoteReaction, error) {
	if !storable(userID) || !storable(noteID) {
		return nil, ErrNotFound
	}
	var rec model.NoteReaction
	if err := r.db.Where("\"userId\" = ? AND \"noteId\" = ?", userID, noteID).First(&rec).Error; err != nil {
		return nil, err
	}
	return &rec, nil
}

func (r *noteReactionRepository) FindByUserAndNoteIDs(userID string, noteIDs []string) (map[string]*model.NoteReaction, error) {
	if len(noteIDs) == 0 {
		return map[string]*model.NoteReaction{}, nil
	}
	var rows []*model.NoteReaction
	if err := r.db.Where("\"userId\" = ? AND \"noteId\" IN ?", userID, noteIDs).Find(&rows).Error; err != nil {
		return nil, err
	}
	m := make(map[string]*model.NoteReaction, len(rows))
	for _, row := range rows {
		m[row.NoteID] = row
	}
	return m, nil
}

// ListByNoteID returns reactions for the given noteID, optionally filtered by
// reaction string. Uses keyset pagination with paginationOrder (DESC by
// default; ASC when only sinceID is supplied, upstream parity).
func (r *noteReactionRepository) ListByNoteID(noteID string, untilID, sinceID string, limit int, reactions []string) ([]*model.NoteReaction, error) {
	// 列に入らない値はどの行とも一致しえない (#3025)。**引く前に弾く** —
	// 比較の右辺に載せるとクエリごと落ちて 500 になる。
	//
	// **`reactions` は `IN` = OR で畳むが、ここは要素ごとに落とさない。** 呼び出し元
	// (`reaction.Service.List`) が渡すのは利用者が指定した 1 つの型の派生形だけなので、
	// 要素ごとに落とすと**残り 0 件でフィルタが消えて全件返る**ほうへ倒れる。
	// 「絞ったつもりで全部出る」より「一致しえないので空」が安全側。
	if !storable(noteID) || !allStorable(reactions) {
		return nil, nil
	}
	var rows []*model.NoteReaction
	q := r.db.Preload("User").Where("\"noteId\" = ?", noteID)
	if len(reactions) == 1 {
		q = q.Where("reaction = ?", reactions[0])
	} else if len(reactions) > 1 {
		q = q.Where("reaction IN ?", reactions)
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if err := q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *noteReactionRepository) ListReactionPairs(noteID string, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	var rows []struct {
		UserID   string `gorm:"column:userId"`
		Reaction string `gorm:"column:reaction"`
	}
	// User を preload せず userId / reaction の 2 列だけを引く (publish path の軽量化)。
	if err := r.db.Model(&model.NoteReaction{}).
		Select(`"userId", reaction`).
		Where(`"noteId" = ?`, noteID).
		Order("id").
		Limit(limit).
		Scan(&rows).Error; err != nil {
		return nil, err
	}
	pairs := make([]string, 0, len(rows))
	for _, row := range rows {
		pairs = append(pairs, row.UserID+"/"+row.Reaction)
	}
	return pairs, nil
}

// ListByUserID returns reactions made by the given user. upstream Misskey TS
// の users/reactions endpoint で使う reactor 視点の list。User / Note を
// preload して pack 時に追加 query が走らないようにする。pagination は
// ListByNoteID と同じ keyset 方式 (paginationOrder で DESC 既定)。
func (r *noteReactionRepository) ListByUserID(userID, viewerID, untilID, sinceID string, limit int) ([]*model.NoteReaction, error) {
	var rows []*model.NoteReaction
	// Note.User も preload する。users/reactions は note を完全 shape (PackNotes)
	// で返すため、note author が無いと misskey_dart の Note.user (UserLite) 解析が
	// null で落ちる (#1227)。
	q := r.db.Preload("User").Preload("Note").Preload("Note.User").Where("\"userId\" = ?", userID)
	// upstream の generateVisibilityQuery 相当を相関 EXISTS で SQL に push down
	// する。post-fetch filter だと viewer が見られない note が除外された分
	// 1 ページが limit 未満になりページネーションが途切れるため、LIMIT 前に
	// 絞る。条件は core/note.CanSeeNote と一致させる (= timeline の FilterVisible
	// と同じ可視性定義。upstream の mentions / replyUserId は CanSeeNote 側に
	// 無いので合わせて含めない)。
	// 条件は core/note.CanSeeNote と一致 (#1454 で共通 helper に集約)。
	q = applyViewerVisibilityExists(q, `"note_reaction"."noteId"`, viewerID)
	// **凍結した利用者のノートを出さない。** upstream `users/reactions.ts` は
	// `generateBlockedHostQueryForNote` と並べて
	// `generateSuspendedUserQueryForNote` を掛けている。ここはリアクション先の
	// ノートを返すので、その著者が凍結されていれば出さない。
	//
	// **著者だけでなく返信先 / リノート元の著者も見る。** upstream の
	// `generateSuspendedUserQueryForNote` は `user` / `replyUser` / `renoteUser`
	// の 3 つを見ており、`reactions.ts` もその 3 つを join している。著者しか
	// 見ないと、**凍結した利用者のノートが「誰かの返信」として本文ごと出る**。
	// `note` 側の `applySuspendedAuthorExclusion` と同じ 3 列。
	suspendedAuthor := func(col string) string {
		return `NOT EXISTS (SELECT 1 FROM "user" su WHERE su."id" = ` + col + ` AND su."isSuspended" = true)`
	}
	q = q.Where(`NOT EXISTS (
		SELECT 1 FROM "note" sn
		WHERE sn."id" = "note_reaction"."noteId"
		  AND NOT (` +
		suspendedAuthor(`sn."userId"`) +
		` AND (sn."replyUserId" IS NULL OR ` + suspendedAuthor(`sn."replyUserId"`) + `)` +
		` AND (sn."renoteUserId" IS NULL OR ` + suspendedAuthor(`sn."renoteUserId"`) + `)))`)
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if err := q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}
