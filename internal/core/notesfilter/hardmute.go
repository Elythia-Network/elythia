// Package notesfilter centralises pre-pack filtering steps applied to a
// []*model.Note before serialization. The first user is the per-viewer
// hardMutedWords filter (#787) shared across timeline / search / channel /
// antenna / user-notes / clip / role / drive endpoints, but the package
// is the natural home for similar viewer-scoped filters added later
// (e.g. per-host muting, blocked-user enforcement) so they stay together.
package notesfilter

import (
	"github.com/elythia-network/elythia/internal/core/wordmute"
	"github.com/elythia-network/elythia/internal/model"
)

// ProfileLookup is the minimal subset of repository.UserRepository this
// package needs. Keeping the interface narrow lets handlers wire only the
// methods they actually use (the production UserRepository implementation
// satisfies it automatically) and makes test stubs trivial.
type ProfileLookup interface {
	FindProfileByUserID(userID string) (*model.UserProfile, error)
}

// ApplyHardMute drops notes that match the viewer's persisted
// hardMutedWords (jsonb on user_profile) before they reach PackNotes.
//
// Best-effort semantics: anonymous viewer / lookup not wired / profile
// fetch error / empty rule list all degrade to "return input untouched"
// so a transient failure never hides legitimate timeline traffic. The
// match target follows upstream check-word-mute.ts (cw + "\n" + text)
// and the viewer's own posts are exempted. Caller passes a lookup the
// handler already holds; one extra DB read per list endpoint is OK since
// streaming hot paths use MatchOne with pre-fetched rules.
func ApplyHardMute(lookup ProfileLookup, viewer *model.User, notes []*model.Note) []*model.Note {
	if viewer == nil || lookup == nil || len(notes) == 0 {
		return notes
	}
	profile, err := lookup.FindProfileByUserID(viewer.ID)
	if err != nil || profile == nil || len(profile.HardMutedWords) == 0 {
		return notes
	}
	rules := []byte(profile.HardMutedWords)
	out := notes[:0]
	for _, n := range notes {
		var text, cw string
		if n.Text != nil {
			text = *n.Text
		}
		if n.CW != nil {
			cw = *n.CW
		}
		if wordmute.MatchNote(rules, n.UserID, viewer.ID, text, cw) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// MatchOne is a single-note convenience used by streaming code paths that
// publish per-note. Unlike ApplyHardMute the caller already holds the
// pre-fetched rules JSON (the streaming worker keeps a per-connection
// cache) so we don't re-fetch the profile on every published note.
//
// Returns true when the note should be hidden from the viewer.
func MatchOne(rulesJSON []byte, viewerID string, n *model.Note) bool {
	if n == nil || len(rulesJSON) == 0 {
		return false
	}
	var text, cw string
	if n.Text != nil {
		text = *n.Text
	}
	if n.CW != nil {
		cw = *n.CW
	}
	return wordmute.MatchNote(rulesJSON, n.UserID, viewerID, text, cw)
}
