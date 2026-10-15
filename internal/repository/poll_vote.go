package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// PollVoteRepository provides data access for the `poll_vote` table.
type PollVoteRepository interface {
	Create(v *model.PollVote) error
	// CreateIfNotVoted inserts v unless the user already voted on the note
	// (any choice when single, the same choice otherwise), atomically against
	// concurrent votes by the same user. Reports whether v was inserted.
	CreateIfNotVoted(v *model.PollVote, single bool) (bool, error)
	FindByUserAndChoice(userID, noteID string, choice int) (*model.PollVote, error)
	CountByUserAndNote(userID, noteID string) (int64, error)
	ListByNoteID(noteID string) ([]*model.PollVote, error)
	// FindByUserAndNoteIDs batch-fetches the viewer's votes across multiple
	// notes so entity.NoteFieldResolver can populate Poll.choices[i].IsVoted
	// in one query (#690). 戻り値は noteID → 投票した choice index list。
	// 空 noteIDs 入力は空 map を返す (no DB call)。
	FindByUserAndNoteIDs(userID string, noteIDs []string) (map[string][]int, error)
}

type pollVoteRepository struct {
	db *gorm.DB
}

// NewPollVoteRepository creates a new PollVoteRepository.
func NewPollVoteRepository(db *gorm.DB) PollVoteRepository {
	return &pollVoteRepository{db: db}
}

func (r *pollVoteRepository) Create(v *model.PollVote) error {
	return r.db.Create(v).Error
}

// pollVoteLockNamespace keeps the advisory lock key of CreateIfNotVoted from
// colliding with other features that lock on a hashed string.
const pollVoteLockNamespace = 690

// CreateIfNotVoted inserts v unless the user has already voted on the note:
// for any choice when single is true, for the same choice otherwise. It
// reports whether the row was inserted.
//
// 同時に届いた投票も含めて、単一選択では1人1票にする。単一選択は
// (userId, noteId)では一意にできない(複数選択の投票は同じ組で複数行を持つ)ので、
// 一意制約ではなく、利用者とノートの組のアドバイザリロックで数える側ごと
// 直列にする。トランザクション版なので、commit / rollbackで自動的に外れる。
func (r *pollVoteRepository) CreateIfNotVoted(v *model.PollVote, single bool) (bool, error) {
	created := false
	err := r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(?, hashtext(?))`,
			pollVoteLockNamespace, v.UserID+":"+v.NoteID).Error; err != nil {
			return err
		}
		q := tx.Model(&model.PollVote{}).Where(`"userId" = ? AND "noteId" = ?`, v.UserID, v.NoteID)
		if !single {
			q = q.Where("choice = ?", v.Choice)
		}
		var n int64
		if err := q.Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		if err := tx.Create(v).Error; err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

// FindByUserAndChoice returns the vote for the given (user, note, choice) triple.
func (r *pollVoteRepository) FindByUserAndChoice(userID, noteID string, choice int) (*model.PollVote, error) {
	if !storable(userID) || !storable(noteID) {
		return nil, ErrNotFound
	}
	var v model.PollVote
	if err := r.db.
		Where("\"userId\" = ? AND \"noteId\" = ? AND choice = ?", userID, noteID, choice).
		First(&v).Error; err != nil {
		return nil, err
	}
	return &v, nil
}

// CountByUserAndNote returns the number of votes the user has cast on the note.
// 単一選択の重複検知に使用する。
func (r *pollVoteRepository) CountByUserAndNote(userID, noteID string) (int64, error) {
	var count int64
	if err := r.db.Model(&model.PollVote{}).
		Where("\"userId\" = ? AND \"noteId\" = ?", userID, noteID).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// ListByNoteID returns all votes for a note (used for entity packing).
func (r *pollVoteRepository) ListByNoteID(noteID string) ([]*model.PollVote, error) {
	var rows []*model.PollVote
	if err := r.db.Where("\"noteId\" = ?", noteID).
		Order("id ASC").
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// FindByUserAndNoteIDs returns the viewer's votes grouped by noteID. Used by
// entity.NoteFieldResolver to mark Poll.choices[i].IsVoted (#690).
func (r *pollVoteRepository) FindByUserAndNoteIDs(userID string, noteIDs []string) (map[string][]int, error) {
	if userID == "" || len(noteIDs) == 0 {
		return map[string][]int{}, nil
	}
	var rows []*model.PollVote
	if err := r.db.
		Where("\"userId\" = ? AND \"noteId\" IN ?", userID, noteIDs).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string][]int, len(rows))
	for _, v := range rows {
		out[v.NoteID] = append(out[v.NoteID], v.Choice)
	}
	return out, nil
}
