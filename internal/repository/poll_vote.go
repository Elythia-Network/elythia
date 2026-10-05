package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// PollVoteRepository provides data access for the `poll_vote` table.
type PollVoteRepository interface {
	Create(v *model.PollVote) error
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
