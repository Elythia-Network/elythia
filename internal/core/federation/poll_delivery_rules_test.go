package federation

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/notedelivery"
	"github.com/elythia-network/elythia/internal/model"
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

// Update(Question) carries the whole note, so it goes only where the Create
// went (#4199).
func TestPollDeliveryHook_OnLocalPollUpdated_RulesFilterFollowers(t *testing.T) {
	hook, enq, userRepo, keypairRepo, followingRepo, _ := newHookSetupWithFollowing(t)
	seedSigner(t, userRepo, keypairRepo, "author")
	followingRepo.RemoteInboxes["author"] = []string{"https://a.example/inbox", "https://b.example/inbox"}
	hook.SetNoteDeliveryRules(pollRules(t, "n1", plugin.DeliveryTargets{Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"a.example"}}))

	hook.OnLocalPollUpdated(&model.Note{ID: "n1", UserID: "author", HasPoll: true, Visibility: model.NoteVisibilityPublic})
	require.Len(t, enq.delivers, 1)
	assert.Equal(t, "https://a.example/inbox", enq.delivers[0].Inbox)
}

func TestPollDeliveryHook_OnLocalPollUpdated_RulesFilterSpecified(t *testing.T) {
	hook, enq, userRepo, keypairRepo, _, _ := newHookSetupWithFollowing(t)
	seedSigner(t, userRepo, keypairRepo, "author")
	seedRemoteUser(userRepo, "allowed", "a.example", "https://a.example/users/allowed/inbox")
	seedRemoteUser(userRepo, "denied", "b.example", "https://b.example/users/denied/inbox")
	hook.SetNoteDeliveryRules(pollRules(t, "n1", plugin.DeliveryTargets{Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"b.example"}}))

	hook.OnLocalPollUpdated(&model.Note{
		ID: "n1", UserID: "author", HasPoll: true,
		Visibility:     model.NoteVisibilitySpecified,
		VisibleUserIDs: model.StringArray{"allowed", "denied"},
	})
	require.Len(t, enq.delivers, 1)
	assert.Equal(t, "https://a.example/users/allowed/inbox", enq.delivers[0].Inbox)
}
