package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/model"
)

// migratedAccountDeleter is the part of the admin API handler
// "elythia cherrypick convert" deletes accounts with.
type migratedAccountDeleter interface {
	DeleteMigratedAccount(user *model.User) error
}

// migratedNoteDeleter is the part of the note delete service
// "elythia cherrypick convert" deletes notes with.
type migratedNoteDeleter interface {
	DeleteAs(actor *model.User, isModerator bool, noteID string) error
}

// ErrAccountCleanupNotQueued is wrapped by DeleteMigratedAccount when the
// account was flagged deleted but its cascade deletion could not be queued.
var ErrAccountCleanupNotQueued = apiadmin.ErrCascadeNotQueued

// errMigrationNotWired is returned when the server was built without the
// routes (tests that call newServer without setupRoutes).
var errMigrationNotWired = errors.New("server: the delete paths are not wired")

// DeleteMigratedAccount deletes the local account userID the way
// admin/delete-account does, for "elythia cherrypick convert" (#4248).
func (s *Server) DeleteMigratedAccount(userID string) error {
	if s.migratedAccounts == nil {
		return errMigrationNotWired
	}
	user, err := s.userRepo.FindByID(userID)
	if err != nil {
		return fmt.Errorf("server: find user %s: %w", userID, err)
	}
	return s.migratedAccounts.DeleteMigratedAccount(user)
}

// DeleteMigratedNote deletes noteID as its author, the way notes/delete does,
// for "elythia cherrypick convert" (#4248). Delete is delivered to the
// federation through the queue.
//
// CherryPick の予約削除は、投稿した本人の削除として動く (モデレーターの削除では
// ない) ので、本人として消す。
func (s *Server) DeleteMigratedNote(noteID string) error {
	if s.migratedNotes == nil {
		return errMigrationNotWired
	}
	var userIDs []string
	if err := s.db.Raw(`SELECT "userId" FROM note WHERE id = ?`, noteID).Scan(&userIDs).Error; err != nil {
		return fmt.Errorf("server: find note %s: %w", noteID, err)
	}
	if len(userIDs) != 1 {
		return fmt.Errorf("server: no such note %s", noteID)
	}
	author, err := s.userRepo.FindByID(userIDs[0])
	if err != nil {
		return fmt.Errorf("server: find the author of note %s: %w", noteID, err)
	}
	return s.migratedNotes.DeleteAs(author, false, noteID)
}

// WaitNoteDeletes waits until the federation hooks of the note deletes have
// finished queueing their deliveries, or ctx ends.
//
// 投稿の削除は、連合への配送のジョブを裏の goroutine で積む (#4216)。止める
// (Shutdown) 前にここで待たないと、件数が多いときに停止の待ち (bgwork.MaxWait) を
// 超えた分が捨てられ、削除が連合先へ届かない。待つのは削除の配送の処理だけで、
// 世代の箱の全体 (プラグインの常駐の処理を含む) は待たない。
func (s *Server) WaitNoteDeletes(ctx context.Context) error {
	if s.noteDeleteRunner == nil {
		return nil
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !s.noteDeleteRunner.idle() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("server: note delete deliveries still being queued: %w", ctx.Err())
		case <-ticker.C:
		}
	}
	return nil
}

// BackgroundDropped returns how many pieces of background work were given up
// because the server was stopping.
func (s *Server) BackgroundDropped() int64 { return s.background.Dropped() }
