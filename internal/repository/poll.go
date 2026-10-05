package repository

import (
	"fmt"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// PollRepository provides data access for polls.
type PollRepository interface {
	Create(poll *model.Poll) error
	FindByNoteID(noteID string) (*model.Poll, error)
	IncrementVote(noteID string, choice int, delta int) error
	// UpdateVotes overwrites the whole votes array for a poll. inbound
	// Update(Question) からリモート poll の vote 数を refresh するのに使う
	// (upstream ApQuestionService.updateQuestion、#1779)。
	UpdateVotes(noteID string, votes []int64) error
	// ListExpiredUnnotified returns polls whose expiresAt has passed and have
	// not yet been pollEnded-notified. Used by core/poll.ExpiryWorker (#690)。
	// limit caps the batch so a long backlog (e.g. after restart) doesn't
	// dominate one tick.
	ListExpiredUnnotified(now time.Time, limit int) ([]*model.Poll, error)
	// MarkNotified stamps poll.notifiedAt for the given note so subsequent
	// ticker scans skip it. idempotent — calling twice with the same time is
	// harmless.
	MarkNotified(noteID string, t time.Time) error
	// ListRecommendation returns note ids of local public unexpired polls the
	// viewer has not authored, not voted on, and whose author is not in
	// mutedUserIDs. When excludeChannels is true, channel polls are excluded
	// entirely (channelId IS NULL). Ordered by noteId DESC, paginated by
	// limit/offset. Mirrors upstream notes/polls/recommendation (#1538, #1765).
	ListRecommendation(viewerID string, mutedUserIDs []string, excludeChannels bool, limit, offset int) ([]string, error)
}

type pollRepository struct {
	db *gorm.DB
}

// NewPollRepository creates a new PollRepository.
func NewPollRepository(db *gorm.DB) PollRepository {
	return &pollRepository{db: db}
}

func (r *pollRepository) Create(poll *model.Poll) error {
	return r.db.Create(poll).Error
}

// FindByNoteID returns the poll attached to noteID.
func (r *pollRepository) FindByNoteID(noteID string) (*model.Poll, error) {
	if !storable(noteID) {
		return nil, ErrNotFound
	}
	var p model.Poll
	if err := r.db.Where("\"noteId\" = ?", noteID).First(&p).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

// IncrementVote bumps poll.votes[choice] by delta. PostgreSQLの配列は1始まりのため
// SQLでは choice+1 を用いる。GORMの UpdateColumn は配列インデックスを取れない
// ため、ここではraw SQLで更新する。choiceは整数なので文字列展開でも安全。
func (r *pollRepository) IncrementVote(noteID string, choice int, delta int) error {
	pgChoice := choice + 1
	q := fmt.Sprintf(`UPDATE "poll" SET votes[%d] = votes[%d] + ? WHERE "noteId" = ?`, pgChoice, pgChoice)
	return r.db.Exec(q, delta, noteID).Error
}

// UpdateVotes overwrites poll.votes for noteID with the given counts.
func (r *pollRepository) UpdateVotes(noteID string, votes []int64) error {
	return r.db.Model(&model.Poll{}).
		Where(`"noteId" = ?`, noteID).
		UpdateColumn("votes", model.Int64Array(votes)).Error
}

// ListExpiredUnnotified returns up to `limit` polls with expiresAt < now and
// notifiedAt IS NULL. partial index IDX_poll_expired_unnotified が migration
// で作られている前提。
func (r *pollRepository) ListExpiredUnnotified(now time.Time, limit int) ([]*model.Poll, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows []*model.Poll
	if err := r.db.
		Where(`"expiresAt" IS NOT NULL AND "expiresAt" < ? AND "notifiedAt" IS NULL`, now).
		Order(`"expiresAt" ASC`).
		Limit(limit).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// MarkNotified stamps poll.notifiedAt = t for the given note. concurrent
// ticker や retry でも単純な UPDATE 1 行なので冪等。
func (r *pollRepository) MarkNotified(noteID string, t time.Time) error {
	return r.db.Model(&model.Poll{}).
		Where(`"noteId" = ?`, noteID).
		Update("notifiedAt", t).Error
}

// ListRecommendation implements the upstream notes/polls/recommendation query:
// local (userHost IS NULL) public unexpired polls not authored by the viewer,
// not already voted on (NOT EXISTS poll_vote), authored by a non-muted user,
// optionally excluding given channels; ordered by noteId DESC. OR-conditions are
// parenthesized so they bind correctly against the other ANDed clauses.
func (r *pollRepository) ListRecommendation(viewerID string, mutedUserIDs []string, excludeChannels bool, limit, offset int) ([]string, error) {
	if limit <= 0 {
		limit = 10
	}
	q := r.db.Model(&model.Poll{}).
		Where(`"userHost" IS NULL`).
		Where(`"userId" <> ?`, viewerID).
		Where(`"noteVisibility" = ?`, string(model.NoteVisibilityPublic)).
		Where(`("expiresAt" IS NULL OR "expiresAt" > ?)`, time.Now()).
		Where(`NOT EXISTS (SELECT 1 FROM "poll_vote" pv WHERE pv."noteId" = "poll"."noteId" AND pv."userId" = ?)`, viewerID)
	if len(mutedUserIDs) > 0 {
		q = q.Where(`"userId" NOT IN ?`, mutedUserIDs)
	}
	if excludeChannels {
		// upstream recommendation.ts:91-93 は excludeChannels=true で
		// channelId IS NULL (= 全 channel poll 除外) を適用する。
		q = q.Where(`"channelId" IS NULL`)
	}
	q = q.Order(`"noteId" DESC`).Limit(limit)
	if offset > 0 {
		q = q.Offset(offset)
	}
	var ids []string
	if err := q.Pluck(`"noteId"`, &ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}
