package federation_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/notedelivery"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/elythia-network/elythia/plugin"
)

// deliveryRules returns a registry whose single plugin restricts the notes in
// targets, failing for every note when fail is set.
func deliveryRules(t *testing.T, targets map[string]plugin.DeliveryTargets, fail bool) *notedelivery.Registry {
	t.Helper()
	r := notedelivery.New()
	require.NoError(t, r.Add("targets", plugin.NoteDeliveryRegistration{
		Prepare: func(context.Context, plugin.NoteDeliveryPrepareRequest) error { return nil },
		Targets: func(_ context.Context, ids []string) (map[string]plugin.DeliveryTargets, error) {
			if fail {
				return nil, errors.New("db down")
			}
			return targets, nil
		},
	}))
	return r
}

// remoteUser registers a remote user on host with a personal inbox.
func remoteUser(userRepo *testutil.MockUserRepository, id, host string) *model.User {
	inbox := "https://" + host + "/users/" + id + "/inbox"
	u := &model.User{ID: id, Username: id, UsernameLower: id, Host: &host, Inbox: &inbox}
	userRepo.Users[id] = u
	return u
}

func enqueuedInboxes(enq *stubEnqueuer) []string {
	out := make([]string, 0, len(enq.calls))
	for _, c := range enq.calls {
		out = append(out, c.Inbox)
	}
	return out
}

func TestNoteDeliveryHook_Rules_IncludeFiltersFollowersAndRelay(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes[author.ID] = []string{"https://a.example/inbox", "https://b.example/inbox"}
	relay := &fakeRelayBroadcaster{}
	hook.SetRelayBroadcaster(relay)

	note := makeNote(author.ID, model.NoteVisibilityPublic)
	hook.SetNoteDeliveryRules(deliveryRules(t, map[string]plugin.DeliveryTargets{
		note.ID: {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"a.example"}},
	}, false))

	hook.OnNoteCreated(note, author)
	assert.Equal(t, []string{"https://a.example/inbox"}, enqueuedInboxes(enq))
	assert.Empty(t, relay.calls, "a restricted note must not reach relays")
}

func TestNoteDeliveryHook_Rules_UnrestrictedNoteIsUnchanged(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes[author.ID] = []string{"https://a.example/inbox", "https://b.example/inbox"}
	relay := &fakeRelayBroadcaster{}
	hook.SetRelayBroadcaster(relay)
	hook.SetNoteDeliveryRules(deliveryRules(t, map[string]plugin.DeliveryTargets{
		"other-note": {Mode: plugin.DeliveryTargetsInclude},
	}, false))

	hook.OnNoteCreated(makeNote(author.ID, model.NoteVisibilityPublic), author)
	assert.ElementsMatch(t, []string{"https://a.example/inbox", "https://b.example/inbox"}, enqueuedInboxes(enq))
	assert.Len(t, relay.calls, 1)
}

func TestNoteDeliveryHook_Rules_ExcludeFiltersMentionsReplyAndQuote(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo, noteRepo := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	remoteUser(userRepo, "allowed", "a.example")
	remoteUser(userRepo, "denied", "b.example")
	remoteUser(userRepo, "replied", "b.example")
	noteRepo.Notes["parent"] = &model.Note{ID: "parent", UserID: "replied"}

	note := makeNote(author.ID, model.NoteVisibilityPublic)
	text := "hi"
	parent := "parent"
	note.Text = &text
	note.ReplyID = &parent
	note.Mentions = model.StringArray{"allowed", "denied"}
	hook.SetNoteDeliveryRules(deliveryRules(t, map[string]plugin.DeliveryTargets{
		note.ID: {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"b.example"}},
	}, false))

	hook.OnNoteCreated(note, author)
	assert.Equal(t, []string{"https://a.example/users/allowed/inbox"}, enqueuedInboxes(enq))
}

func TestNoteDeliveryHook_Rules_FiltersSpecified(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	remoteUser(userRepo, "allowed", "a.example")
	remoteUser(userRepo, "denied", "b.example")

	note := makeNote(author.ID, model.NoteVisibilitySpecified)
	note.VisibleUserIDs = model.StringArray{"allowed", "denied"}
	hook.SetNoteDeliveryRules(deliveryRules(t, map[string]plugin.DeliveryTargets{
		note.ID: {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"a.example"}},
	}, false))

	hook.OnNoteCreated(note, author)
	assert.Equal(t, []string{"https://a.example/users/allowed/inbox"}, enqueuedInboxes(enq))
}

func TestNoteDeliveryHook_Rules_FailureDeliversNowhere(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes[author.ID] = []string{"https://a.example/inbox"}
	remoteUser(userRepo, "mentioned", "a.example")
	relay := &fakeRelayBroadcaster{}
	hook.SetRelayBroadcaster(relay)
	hook.SetNoteDeliveryRules(deliveryRules(t, nil, true))

	note := makeNote(author.ID, model.NoteVisibilityPublic)
	note.Mentions = model.StringArray{"mentioned"}
	hook.OnNoteCreated(note, author)
	assert.Empty(t, enq.calls)
	assert.Empty(t, relay.calls)
}

func TestNoteDeliveryHook_Rules_FiltersAnnounce(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes[author.ID] = []string{"https://a.example/inbox", "https://b.example/inbox"}
	noteRepo.Notes["tgt"] = &model.Note{ID: "tgt", UserID: "other", Visibility: model.NoteVisibilityPublic}

	renote := makeNote(author.ID, model.NoteVisibilityPublic)
	target := "tgt"
	renote.RenoteID = &target
	hook.SetNoteDeliveryRules(deliveryRules(t, map[string]plugin.DeliveryTargets{
		renote.ID: {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"a.example"}},
	}, false))

	hook.OnNoteCreated(renote, author)
	require.Equal(t, []string{"https://b.example/inbox"}, enqueuedInboxes(enq))
	var got map[string]any
	require.NoError(t, json.Unmarshal(enq.calls[0].Body, &got))
	assert.Equal(t, "Announce", got["type"])
}

func TestNoteDeliveryHook_Rules_SendNoteUpdate(t *testing.T) {
	hook, enq, userRepo, followingRepo, keypairRepo, noteRepo := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	followingRepo.RemoteInboxes[author.ID] = []string{"https://a.example/inbox", "https://b.example/inbox"}
	remoteUser(userRepo, "bob", "b.example")
	noteRepo.Notes["bobnote"] = &model.Note{ID: "bobnote", UserID: "bob"}
	relay := &fakeRelayBroadcaster{}
	hook.SetRelayBroadcaster(relay)

	note := makeNote(author.ID, model.NoteVisibilityPublic)
	text, target := "look", "bobnote"
	note.Text, note.RenoteID = &text, &target
	hook.SetNoteDeliveryRules(deliveryRules(t, map[string]plugin.DeliveryTargets{
		note.ID: {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"a.example"}},
	}, false))

	require.NoError(t, hook.SendNoteUpdate(note, author))
	assert.Equal(t, []string{"https://a.example/inbox"}, enqueuedInboxes(enq))
	assert.Empty(t, relay.calls)
}

func TestNoteDeliveryHook_Rules_SendQuoteRequest(t *testing.T) {
	hook, enq, userRepo, _, keypairRepo, _ := newNoteDeliveryHook(t)
	author := makeLocalAuthor(t, userRepo, keypairRepo)
	bob := remoteUser(userRepo, "bob", "b.example")

	note := makeNote(author.ID, model.NoteVisibilityPublic)
	text, target := "look", "bobnote"
	note.Text, note.RenoteID = &text, &target

	hook.SetNoteDeliveryRules(deliveryRules(t, map[string]plugin.DeliveryTargets{
		note.ID: {Mode: plugin.DeliveryTargetsExclude, Hosts: []string{"b.example"}},
	}, false))
	require.NoError(t, hook.SendQuoteRequest(note, "https://b.example/notes/1", bob))
	assert.Empty(t, enq.calls, "the quoted author's host is excluded")

	hook.SetNoteDeliveryRules(deliveryRules(t, map[string]plugin.DeliveryTargets{
		note.ID: {Mode: plugin.DeliveryTargetsInclude, Hosts: []string{"b.example"}},
	}, false))
	require.NoError(t, hook.SendQuoteRequest(note, "https://b.example/notes/1", bob))
	assert.Equal(t, []string{*bob.Inbox}, enqueuedInboxes(enq))
}
