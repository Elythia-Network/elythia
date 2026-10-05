package repository

import (
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// BubbleVersusMatchBase is the part of a versus record that is known as soon as
// the match exists. Every write carries it so that whichever write comes first
// creates the row.
type BubbleVersusMatchBase struct {
	ID        string
	User1ID   string
	User2ID   string
	GameMode  string
	Seed      string
	StartedAt time.Time
}

// BubbleVersusReport is one player's report as stored in the record.
type BubbleVersusReport struct {
	Score       int
	Frame       int
	Reason      string
	GameVersion *int
	Logs        datatypes.JSON
}

// BubbleVersusRepository persists finished 1:1 bubble game matches (#3232).
type BubbleVersusRepository interface {
	// SaveReport stores the report of player slot (0 = user1, 1 = user2),
	// creating the row from base if it does not exist yet.
	SaveReport(base BubbleVersusMatchBase, slot int, report BubbleVersusReport) error
	// Finish records the outcome, creating the row from base if needed.
	Finish(base BubbleVersusMatchBase, endedAt time.Time, winnerID *string, reason string) error
	FindByID(id string) (*model.BubbleGameVersusRecord, error)
	// ListByUser returns finished matches of userID, newest first, without the
	// logs and the seed. When publicOnly is set, only matches both players made
	// public are returned.
	ListByUser(userID string, publicOnly bool, untilID string, limit int) ([]*model.BubbleGameVersusRecord, error)
	// SetPublic sets userID's own public flag on the match. It reports whether
	// userID took part in the match.
	SetPublic(id, userID string, public bool) (bool, error)
	// DeleteExpired removes matches that ended before cutoff, and matches that
	// never ended and started before cutoff.
	DeleteExpired(cutoff time.Time) (int64, error)
}

type bubbleVersusRepository struct {
	db *gorm.DB
}

// NewBubbleVersusRepository creates a new BubbleVersusRepository.
func NewBubbleVersusRepository(db *gorm.DB) BubbleVersusRepository {
	return &bubbleVersusRepository{db: db}
}

func baseRecord(base BubbleVersusMatchBase) model.BubbleGameVersusRecord {
	return model.BubbleGameVersusRecord{
		ID: base.ID, User1ID: base.User1ID, User2ID: base.User2ID,
		GameMode: base.GameMode, Seed: base.Seed, StartedAt: base.StartedAt,
	}
}

// 列名は slot ごとに固定の一覧で持つ。書式で組み立てると sqlbind-check の
// 対象になるうえ、slot の値の検査を書き忘れたときに任意の列名が作れてしまう。
var bubbleVersusReportColumns = [2][]string{
	{"user1Score", "user1Frame", "user1Reason", "user1GameVersion", "user1Logs"},
	{"user2Score", "user2Frame", "user2Reason", "user2GameVersion", "user2Logs"},
}

func (r *bubbleVersusRepository) SaveReport(base BubbleVersusMatchBase, slot int, report BubbleVersusReport) error {
	if slot != 0 && slot != 1 {
		return gorm.ErrInvalidField
	}
	row := baseRecord(base)
	score, frame, reason := report.Score, report.Frame, report.Reason
	if slot == 0 {
		row.User1Score, row.User1Frame, row.User1Reason = &score, &frame, &reason
		row.User1GameVersion, row.User1Logs = report.GameVersion, report.Logs
	} else {
		row.User2Score, row.User2Frame, row.User2Reason = &score, &frame, &reason
		row.User2GameVersion, row.User2Logs = report.GameVersion, report.Logs
	}
	// 行があれば自分の列だけを上書きする。相手の列と終局の列には触らない
	// (報告と終局の確定は順不同で届く)。
	return r.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns(bubbleVersusReportColumns[slot]),
	}).Create(&row).Error
}

func (r *bubbleVersusRepository) Finish(base BubbleVersusMatchBase, endedAt time.Time, winnerID *string, reason string) error {
	row := baseRecord(base)
	row.EndedAt, row.WinnerID, row.Reason = &endedAt, winnerID, &reason
	return r.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"endedAt", "winnerId", "reason"}),
	}).Create(&row).Error
}

func (r *bubbleVersusRepository) FindByID(id string) (*model.BubbleGameVersusRecord, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var rec model.BubbleGameVersusRecord
	if err := r.db.Preload("User1").Preload("User2").Where(`"id" = ?`, id).First(&rec).Error; err != nil {
		return nil, err
	}
	return &rec, nil
}

func (r *bubbleVersusRepository) ListByUser(userID string, publicOnly bool, untilID string, limit int) ([]*model.BubbleGameVersusRecord, error) {
	if !storable(userID) {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	// OR は括弧で閉じて書く。GORM は OR を含む条件を自分でも括弧に包むが
	// (DryRun で確認)、後ろに AND をつなぐ条件の優先順位を GORM の振る舞いに
	// 預けない。
	// 一覧は操作の記録とシードを読まない。記録は 1 局で最大約 2 MiB あり、
	// 一覧の画面では使わない (handler は見せない対局を飛ばした分を引き直すので、
	// 読む行数はさらに増える)。
	q := r.db.Preload("User1").Preload("User2").
		Omit("user1Logs", "user2Logs", "seed").
		Where(`("user1Id" = ? OR "user2Id" = ?)`, userID, userID).
		Where(`"endedAt" IS NOT NULL`)
	if publicOnly {
		q = q.Where(`"user1Public" AND "user2Public"`)
	}
	if untilID != "" {
		q = q.Where(`"id" < ?`, untilID)
	}
	var recs []*model.BubbleGameVersusRecord
	if err := q.Order(`"id" DESC`).Limit(limit).Find(&recs).Error; err != nil {
		return nil, err
	}
	return recs, nil
}

func (r *bubbleVersusRepository) SetPublic(id, userID string, public bool) (bool, error) {
	if !storable(id) || !storable(userID) {
		return false, nil
	}
	// 自分の側の列だけを変える。どちらの側かは行の user1Id / user2Id で決まる。
	res := r.db.Model(&model.BubbleGameVersusRecord{}).
		Where(`"id" = ? AND "user1Id" = ?`, id, userID).Update("user1Public", public)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected > 0 {
		return true, nil
	}
	res = r.db.Model(&model.BubbleGameVersusRecord{}).
		Where(`"id" = ? AND "user2Id" = ?`, id, userID).Update("user2Public", public)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func (r *bubbleVersusRepository) DeleteExpired(cutoff time.Time) (int64, error) {
	res := r.db.Where(`"endedAt" < ? OR ("endedAt" IS NULL AND "startedAt" < ?)`, cutoff, cutoff).
		Delete(&model.BubbleGameVersusRecord{})
	return res.RowsAffected, res.Error
}
