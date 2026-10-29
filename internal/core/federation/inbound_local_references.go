package federation

import (
	"errors"
	"fmt"
	"net/url"
	"sort"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/localfederation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// ErrInboundLocalReferenceUnavailable is a permanent policy rejection for an
// inbound reference to a local account or note that cannot be federated.
var ErrInboundLocalReferenceUnavailable = errors.New("inbound local reference is unavailable")
var ErrInboundLocalReferenceFailure = errors.New("inbound local reference check failed")

type externalReferenceForUserObserver interface {
	ObserveExternalReferenceForUser(user *model.User, reason, host string) (bool, error)
}

func inboundActorHost(uri string) string {
	parsed, err := url.Parse(uri)
	if err != nil || parsed == nil {
		return ""
	}
	return parsed.Host
}

func (r *Resolver) SetLocalFederationObserver(observer activitypub.LocalFederationObserver) {
	r.localFederationObserver = observer
}

// ObserveLocalInboxPathTarget checks and records the local user named by an
// inbox path before signature or body admission completes.
func (r *Resolver) ObserveLocalInboxPathTarget(userID, host string) error {
	if userID == "" || r == nil || r.userRepo == nil || r.localFederationObserver == nil {
		return nil
	}
	if r.localReferenceCache.hasUser(userID, r.clock()) {
		return nil
	}
	return r.ObserveLocalInboxBodyReferences([]string{userID}, nil, host)
}

// ObserveLocalInboxBodyReferences validates all referenced rows before making
// first-reference writes. Note metadata and users are loaded in bounded batch
// queries; cached entries contain only confirmed, available positive results.
func (r *Resolver) ObserveLocalInboxBodyReferences(userIDs, noteIDs []string, host string) error {
	if r == nil || r.localFederationObserver == nil || r.userRepo == nil {
		return nil
	}
	now := r.clock()
	userIDsToResolve := make(map[string]struct{}, len(userIDs))
	for _, id := range userIDs {
		if id != "" && !r.localReferenceCache.hasUser(id, now) {
			userIDsToResolve[id] = struct{}{}
		}
	}

	notesToConfirm := make(map[string]string, len(noteIDs))
	var coldNoteIDs []string
	for _, id := range noteIDs {
		if id == "" {
			continue
		}
		if authorID, ok := r.localReferenceCache.noteAuthor(id, now); ok {
			if !r.localReferenceCache.hasUser(authorID, now) {
				userIDsToResolve[authorID] = struct{}{}
			}
			continue
		}
		coldNoteIDs = append(coldNoteIDs, id)
	}
	if len(coldNoteIDs) != 0 && r.noteRepo != nil {
		notes, err := r.noteRepo.FindManyLocalReferenceMetadataByIDs(coldNoteIDs)
		if err != nil {
			return fmt.Errorf("%w: lookup local inbox note references: %w", ErrInboundLocalReferenceFailure, err)
		}
		for _, note := range notes {
			if note == nil || note.UserHost != nil {
				continue
			}
			if note.LocalOnly {
				return ErrInboundLocalReferenceUnavailable
			}
			notesToConfirm[note.ID] = note.UserID
			if note.UserID != "" && !r.localReferenceCache.hasUser(note.UserID, now) {
				userIDsToResolve[note.UserID] = struct{}{}
			}
		}
	} else if len(coldNoteIDs) != 0 {
		return fmt.Errorf("%w: note repository is unavailable", ErrInboundLocalReferenceFailure)
	}

	ids := make([]string, 0, len(userIDsToResolve))
	for id := range userIDsToResolve {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	resolved := make(map[string]*model.User, len(ids))
	if len(ids) > 0 {
		users, err := r.userRepo.FindManyByIDs(ids)
		if err != nil {
			return fmt.Errorf("%w: lookup local inbox user references: %w", ErrInboundLocalReferenceFailure, err)
		}
		for _, user := range users {
			if user != nil {
				resolved[user.ID] = user
			}
		}
	}
	toRecord := make([]*model.User, 0, len(resolved))
	for _, id := range ids {
		user := resolved[id]
		if user == nil || !user.IsLocal() {
			continue
		}
		if !r.localFederationObserver.IsExternallyAvailable(user) {
			return ErrInboundLocalReferenceUnavailable
		}
		if user.FirstExternalReferenceAt == nil {
			toRecord = append(toRecord, user)
		} else {
			r.localReferenceCache.rememberUser(id, now)
		}
	}
	if err := r.confirmInboundLocalNotes(notesToConfirm); err != nil {
		return err
	}

	for _, user := range toRecord {
		if err := r.recordLocalReference(user, "inbox", host); err != nil {
			return err
		}
		r.localReferenceCache.rememberUser(user.ID, r.clock())
	}
	if err := r.confirmInboundLocalNotes(notesToConfirm); err != nil {
		return err
	}
	return nil
}

func (r *Resolver) confirmInboundLocalNotes(expected map[string]string) error {
	if len(expected) == 0 {
		return nil
	}
	ids := make([]string, 0, len(expected))
	for id := range expected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	notes, err := r.noteRepo.FindManyLocalReferenceMetadataByIDs(ids)
	if err != nil {
		return fmt.Errorf("%w: confirm local inbox notes: %w", ErrInboundLocalReferenceFailure, err)
	}
	byID := make(map[string]*model.Note, len(notes))
	for _, note := range notes {
		if note != nil {
			byID[note.ID] = note
		}
	}
	for _, id := range ids {
		note := byID[id]
		if note == nil || note.UserHost != nil || note.LocalOnly || note.UserID != expected[id] {
			return ErrInboundLocalReferenceUnavailable
		}
		if r.localReferenceCache.hasUser(note.UserID, r.clock()) {
			r.localReferenceCache.rememberNote(id, note.UserID, r.clock())
		}
	}
	return nil
}

func (r *Resolver) recordLocalReference(user *model.User, reason, host string) error {
	var err error
	if observer, ok := r.localFederationObserver.(externalReferenceForUserObserver); ok {
		_, err = observer.ObserveExternalReferenceForUser(user, reason, host)
	} else {
		_, err = r.localFederationObserver.ObserveExternalReference(user.ID, reason, host)
	}
	if errors.Is(err, localfederation.ErrFederationDisabled) {
		return ErrInboundLocalReferenceUnavailable
	}
	if err != nil {
		return fmt.Errorf("%w: observe local user: %w", ErrInboundLocalReferenceFailure, err)
	}
	return nil
}

// RequireInboundLocalUser applies the same policy at the worker's actual
// resolution point, including local URI forms outside HTTP preflight's strict
// canonical subset.
func (r *Resolver) RequireInboundLocalUser(user *model.User, host string) error {
	if r == nil || user == nil || !user.IsLocal() || r.localFederationObserver == nil {
		return nil
	}
	now := r.clock()
	if r.localReferenceCache.hasUser(user.ID, now) {
		return nil
	}
	if !r.localFederationObserver.IsExternallyAvailable(user) {
		return ErrInboundLocalReferenceUnavailable
	}
	if user.FirstExternalReferenceAt != nil {
		r.localReferenceCache.rememberUser(user.ID, now)
		return nil
	}
	if err := r.recordLocalReference(user, "inbox", host); err != nil {
		return err
	}
	r.localReferenceCache.rememberUser(user.ID, r.clock())
	return nil
}

// RequireInboundLocalNote checks a resolved local note and its author before
// the worker can use the note as an interaction target.
func (r *Resolver) RequireInboundLocalNote(note *model.Note, host string) error {
	if r == nil || note == nil || note.UserHost != nil || r.localFederationObserver == nil {
		return nil
	}
	if note.LocalOnly {
		return ErrInboundLocalReferenceUnavailable
	}
	if authorID, ok := r.localReferenceCache.noteAuthor(note.ID, r.clock()); ok && authorID == note.UserID && r.localReferenceCache.hasUser(authorID, r.clock()) {
		return nil
	}
	if r.noteRepo == nil {
		return fmt.Errorf("%w: note repository is unavailable", ErrInboundLocalReferenceFailure)
	}
	expected := map[string]string{note.ID: note.UserID}
	if err := r.confirmInboundLocalNotes(expected); err != nil {
		return err
	}
	author := note.User
	if author == nil {
		var err error
		author, err = r.userRepo.FindByID(note.UserID)
		if repository.IsNotFound(err) {
			return ErrInboundLocalReferenceUnavailable
		}
		if err != nil {
			return fmt.Errorf("%w: lookup local inbox note author: %w", ErrInboundLocalReferenceFailure, err)
		}
	}
	if err := r.RequireInboundLocalUser(author, host); err != nil {
		return err
	}
	return r.confirmInboundLocalNotes(expected)
}
