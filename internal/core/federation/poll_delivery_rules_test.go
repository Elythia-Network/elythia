package federation

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/notedelivery"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/elythia-network/elythia/plugin"
)

func pollRules(t *testing.T, noteID string, targets plugin.DeliveryTargets) *notedelivery.Registry {
	t.Helper()
	r := notedelivery.New()
	require.NoError(t, r.Add("targets", plugin.NoteDeliveryRegistration{
		Prepare: func(context.Context, plugin.NoteDeliveryPrepareRequest) error { return nil },
		Targets: func(context.Context, []string) (map[string]plugin.DeliveryTargets, error) {
			return map[string]plugin.DeliveryTargets{noteID: targets}, nil
		},
	}))
	return r
}

// notesWith is a note repository in which the notes ids exist.
func notesWith(ids ...string) *testutil.MockNoteRepository {
	notes := testutil.NewMockNoteRepository()
	for _, id := range ids {
		notes.Notes[id] = &model.Note{ID: id}
	}
	return notes
}

// Update(Question) carries the whole note, so it goes only where the Create
// went (#4199).
func TestPollDeliveryHook_OnLocalPollUpdated_RulesFilterFollowers(t *testing.T) {
	hook, enq, userRepo, keypairRepo, followingRepo, _ := newHookSetupWithRules(t, pollRules(t, "n1", plugin.DeliveryTargets{Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"a.example"}}), notesWith("n1"))
	seedSigner(t, userRepo, keypairRepo, "author")
	followingRepo.RemoteInboxes["author"] = []string{"https://a.example/inbox", "https://b.example/inbox"}

	hook.OnLocalPollUpdated(&model.Note{ID: "n1", UserID: "author", HasPoll: true, Visibility: model.NoteVisibilityPublic})
	require.Len(t, enq.delivers, 1)
	assert.Equal(t, "https://a.example/inbox", enq.delivers[0].Inbox)
}

func TestPollDeliveryHook_OnLocalPollUpdated_RulesFilterSpecified(t *testing.T) {
	hook, enq, userRepo, keypairRepo, _, _ := newHookSetupWithRules(t, pollRules(t, "n1", plugin.DeliveryTargets{Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"b.example"}}), notesWith("n1"))
	seedSigner(t, userRepo, keypairRepo, "author")
	seedRemoteUser(userRepo, "allowed", "a.example", "https://a.example/users/allowed/inbox")
	seedRemoteUser(userRepo, "denied", "b.example", "https://b.example/users/denied/inbox")

	hook.OnLocalPollUpdated(&model.Note{
		ID: "n1", UserID: "author", HasPoll: true,
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: model.StringArray{"allowed", "denied"},
	})
	require.Len(t, enq.delivers, 1)
	assert.Equal(t, "https://a.example/users/allowed/inbox", enq.delivers[0].Inbox)
}

// A note deleted while its Update(Question) waited for a slot has had its
// rule cleaned up, so reading no rule must not let the update reach every
// follower (#4202).
func TestPollDeliveryHook_OnLocalPollUpdated_DeletedNoteGoesNowhere(t *testing.T) {
	notes := testutil.NewMockNoteRepository()
	hook, enq, userRepo, keypairRepo, followingRepo, _ := newHookSetupWithRules(t, pollRules(t, "other", plugin.DeliveryTargets{Mode: plugin.DeliveryTargetsInclude}), notes)
	seedSigner(t, userRepo, keypairRepo, "author")
	followingRepo.RemoteInboxes["author"] = []string{"https://a.example/inbox", "https://b.example/inbox"}
	note := &model.Note{ID: "n1", UserID: "author", HasPoll: true, Visibility: model.NoteVisibilityPublic}

	hook.OnLocalPollUpdated(note)
	assert.Empty(t, enq.delivers, "a deleted note must not reach anyone")

	notes.ExistingOnPrimaryErr = errors.New("primary down")
	notes.Notes["n1"] = note
	hook.OnLocalPollUpdated(note)
	assert.Empty(t, enq.delivers, "a note whose existence cannot be checked must not reach anyone")

	notes.ExistingOnPrimaryErr = nil
	hook.OnLocalPollUpdated(note)
	assert.Len(t, enq.delivers, 2, "an unrestricted note that exists reaches every follower")
}

// A restricted rule read after the note was deleted may be what is left of the
// rules halfway through the clean-up, so it does not let the update out
// either (#4202).
func TestPollDeliveryHook_OnLocalPollUpdated_DeletedRestrictedNoteGoesNowhere(t *testing.T) {
	hook, enq, userRepo, keypairRepo, followingRepo, _ := newHookSetupWithRules(t, pollRules(t, "n1", plugin.DeliveryTargets{Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"b.example"}}), testutil.NewMockNoteRepository())
	seedSigner(t, userRepo, keypairRepo, "author")
	followingRepo.RemoteInboxes["author"] = []string{"https://a.example/inbox", "https://b.example/inbox"}

	hook.OnLocalPollUpdated(&model.Note{ID: "n1", UserID: "author", HasPoll: true, Visibility: model.NoteVisibilityPublic})
	assert.Empty(t, enq.delivers)
}
