package federation_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// availabilityObserver reports the listed users as not externally available.
type availabilityObserver struct{ unavailable map[string]bool }

func (o *availabilityObserver) IsExternallyAvailable(user *model.User) bool {
	return !o.unavailable[user.ID]
}

func (o *availabilityObserver) ObserveExternalReference(string, string, string) (bool, error) {
	return true, nil
}

type followerKeysFixture struct {
	keys      *federation.LocalFollowerKeys
	users     *testutil.MockUserRepository
	following *testutil.MockFollowingRepository
	keypairs  *testutil.MockUserKeypairRepository
	blocking  *testutil.MockBlockingRepository
	observer  *availabilityObserver
	actor     *model.User
}

func newFollowerKeysFixture(t *testing.T) *followerKeysFixture {
	t.Helper()
	f := &followerKeysFixture{
		users:     testutil.NewMockUserRepository(),
		following: testutil.NewMockFollowingRepository(),
		keypairs:  testutil.NewMockUserKeypairRepository(),
		blocking:  testutil.NewMockBlockingRepository(),
		observer:  &availabilityObserver{unavailable: map[string]bool{}},
	}
	host, uri := "remote.example", "https://remote.example/users/alice"
	f.actor = &model.User{ID: "alice", Username: "alice", Host: &host, URI: &uri}
	f.users.Users[f.actor.ID] = f.actor
	f.keys = federation.NewLocalFollowerKeys(f.following, f.users, f.keypairs, f.blocking,
		activitypub.NewURLBuilder("https://example.com"), f.observer)
	return f
}

// addFollower makes a local user with a keypair who follows the actor.
func (f *followerKeysFixture) addFollower(t *testing.T, id string) *model.User {
	t.Helper()
	privPEM, _, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	u := &model.User{ID: id, Username: id}
	f.users.Users[id] = u
	require.NoError(t, f.keypairs.Create(&model.UserKeypair{UserID: id, PrivateKey: privPEM}))
	require.NoError(t, f.following.Create(&model.Following{ID: "f-" + id, FollowerID: id, FolloweeID: f.actor.ID}))
	return u
}

func keyIDOf(key *activitypub.PrivateKey) string {
	if key == nil {
		return ""
	}
	return key.KeyID
}

func TestLocalFollowerKeys_PicksFirstUsableFollower(t *testing.T) {
	f := newFollowerKeysFixture(t)
	f.addFollower(t, "b")
	f.addFollower(t, "a")
	assert.Equal(t, "https://example.com/users/a#main-key", keyIDOf(f.keys.KeyForFollowerOf(f.actor)), "follower id order")
}

func TestLocalFollowerKeys_SkipsUnusableFollowers(t *testing.T) {
	cases := map[string]func(t *testing.T, f *followerKeysFixture, a *model.User){
		"suspended":      func(_ *testing.T, _ *followerKeysFixture, a *model.User) { a.IsSuspended = true },
		"deleted":        func(_ *testing.T, _ *followerKeysFixture, a *model.User) { a.IsDeleted = true },
		"not federated":  func(_ *testing.T, f *followerKeysFixture, a *model.User) { f.observer.unavailable[a.ID] = true },
		"no keypair":     func(_ *testing.T, f *followerKeysFixture, a *model.User) { delete(f.keypairs.Keypairs, a.ID) },
		"bad keypair":    func(_ *testing.T, f *followerKeysFixture, a *model.User) { f.keypairs.Keypairs[a.ID].PrivateKey = "x" },
		"user not found": func(_ *testing.T, f *followerKeysFixture, a *model.User) { delete(f.users.Users, a.ID) },
		"follower blocks actor": func(t *testing.T, f *followerKeysFixture, a *model.User) {
			require.NoError(t, f.blocking.Create(&model.Blocking{ID: "b1", BlockerID: a.ID, BlockeeID: f.actor.ID}))
		},
		"actor blocks follower": func(t *testing.T, f *followerKeysFixture, a *model.User) {
			require.NoError(t, f.blocking.Create(&model.Blocking{ID: "b1", BlockerID: f.actor.ID, BlockeeID: a.ID}))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFollowerKeysFixture(t)
			a := f.addFollower(t, "a")
			f.addFollower(t, "b")
			require.Equal(t, "https://example.com/users/a#main-key", keyIDOf(f.keys.KeyForFollowerOf(f.actor)), "baseline")
			mutate(t, f, a)
			assert.Equal(t, "https://example.com/users/b#main-key", keyIDOf(f.keys.KeyForFollowerOf(f.actor)))
		})
	}
}

func TestLocalFollowerKeys_ReturnsNil(t *testing.T) {
	t.Run("no follower", func(t *testing.T) {
		f := newFollowerKeysFixture(t)
		assert.Nil(t, f.keys.KeyForFollowerOf(f.actor))
	})
	t.Run("block check fails", func(t *testing.T) {
		f := newFollowerKeysFixture(t)
		f.addFollower(t, "a")
		f.blocking.ExistsErr = errors.New("db down")
		assert.Nil(t, f.keys.KeyForFollowerOf(f.actor))
	})
	t.Run("local actor", func(t *testing.T) {
		f := newFollowerKeysFixture(t)
		f.addFollower(t, "a")
		f.actor.Host = nil
		assert.Nil(t, f.keys.KeyForFollowerOf(f.actor))
	})
	t.Run("nil", func(t *testing.T) {
		var k *federation.LocalFollowerKeys
		assert.Nil(t, k.KeyForFollowerOf(&model.User{ID: "x"}))
		assert.Nil(t, newFollowerKeysFixture(t).keys.KeyForFollowerOf(nil))
	})
	// 候補を見るのは先頭の 16 人まで。
	t.Run("only the first candidates are looked at", func(t *testing.T) {
		f := newFollowerKeysFixture(t)
		for i := 0; i < 17; i++ {
			u := f.addFollower(t, fmt.Sprintf("u%02d", i))
			if i < 16 {
				u.IsSuspended = true
			}
		}
		assert.Nil(t, f.keys.KeyForFollowerOf(f.actor))
		f.users.Users["u15"].IsSuspended = false
		assert.Equal(t, "https://example.com/users/u15#main-key", keyIDOf(f.keys.KeyForFollowerOf(f.actor)))
	})
}

// recordingFollowerKeys hands out a fixed key and counts the calls.
type recordingFollowerKeys struct {
	key   *activitypub.PrivateKey
	calls []string
}

func (r *recordingFollowerKeys) KeyForFollowerOf(actor *model.User) *activitypub.PrivateKey {
	r.calls = append(r.calls, actor.ID)
	return r.key
}

const hiddenOwnNoteURI = "https://remote.example/notes/own-hidden"

const hiddenOwnFollowersNote = `{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": "` + hiddenOwnNoteURI + `",
	"type": "Note",
	"attributedTo": "https://remote.example/users/alice",
	"content": "secret",
	"to": ["https://remote.example/users/alice/followers"]
}`

func hiddenOwnAnnounce(id string) []byte {
	return []byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/` + id + `",
		"actor": "https://remote.example/users/alice",
		"object": "` + hiddenOwnNoteURI + `",
		"to": ["https://remote.example/users/alice/followers"]
	}`)
}

func renoteOf(env *fullProcessorEnv, targetURI string) *model.Note {
	target, err := env.noteRepo.FindByURI(targetURI)
	if err != nil {
		return nil
	}
	for _, n := range env.noteRepo.Notes {
		if n.RenoteID != nil && *n.RenoteID == target.ID {
			return n
		}
	}
	return nil
}

// A locked account's renote of its own followers-only note, which this server
// has not seen and cannot fetch with the instance actor, is ingested by
// retrying with a local follower's key (#3916).
func TestProcess_AnnounceRetriesHiddenTargetWithFollowerKey(t *testing.T) {
	f := &hiddenNoteFetcher{hidden: map[string]string{hiddenOwnNoteURI: hiddenOwnFollowersNote}}
	env := newFullProcessorFetcher(t, f)
	keys := &recordingFollowerKeys{key: newViewerKey(t)}
	env.processor.SetFollowerFetchKeys(keys)

	require.NoError(t, env.processor.Process(hiddenOwnAnnounce("a1")))

	renote := renoteOf(env, hiddenOwnNoteURI)
	require.NotNil(t, renote, "the renote is ingested")
	assert.Equal(t, model.NoteVisibilityFollowers, renote.Visibility)
	assert.Equal(t, []string{hiddenOwnNoteURI}, f.keyed)
	assert.Len(t, keys.calls, 1)
}

func TestProcess_AnnounceWithoutFollowerKeyIsSkipped(t *testing.T) {
	t.Run("not wired", func(t *testing.T) {
		f := &hiddenNoteFetcher{hidden: map[string]string{hiddenOwnNoteURI: hiddenOwnFollowersNote}}
		env := newFullProcessorFetcher(t, f)
		require.NoError(t, env.processor.Process(hiddenOwnAnnounce("a2")))
		assert.Nil(t, renoteOf(env, hiddenOwnNoteURI))
		assert.Empty(t, f.keyed)
	})
	t.Run("no usable follower", func(t *testing.T) {
		f := &hiddenNoteFetcher{hidden: map[string]string{hiddenOwnNoteURI: hiddenOwnFollowersNote}}
		env := newFullProcessorFetcher(t, f)
		keys := &recordingFollowerKeys{}
		env.processor.SetFollowerFetchKeys(keys)
		require.NoError(t, env.processor.Process(hiddenOwnAnnounce("a3")))
		assert.Nil(t, renoteOf(env, hiddenOwnNoteURI))
		assert.Empty(t, f.keyed)
		assert.Len(t, keys.calls, 1)
	})
}

// The follower is looked up only when a retry is needed.
func TestProcess_AnnounceOfVisibleTargetDoesNotLookUpFollowers(t *testing.T) {
	f := &hiddenNoteFetcher{public: map[string]string{hiddenOwnNoteURI: hiddenOwnFollowersNote}}
	env := newFullProcessorFetcher(t, f)
	keys := &recordingFollowerKeys{key: newViewerKey(t)}
	env.processor.SetFollowerFetchKeys(keys)
	require.NoError(t, env.processor.Process(hiddenOwnAnnounce("a4")))
	require.NotNil(t, renoteOf(env, hiddenOwnNoteURI))
	assert.Empty(t, keys.calls)
	assert.Empty(t, f.keyed)
}

// An inbound note quoting a followers-only note this server cannot fetch is
// ingested without retrying the quote target with a follower's key: ingest
// would not link a followers-only quote target anyway, so the retry would only
// show a follower's actor to the remote server (#3916).
func TestProcess_CreateDoesNotRetryHiddenQuoteTarget(t *testing.T) {
	create := []byte(`{
		"type": "Create",
		"id": "https://remote.example/notes/quoting/activity",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/notes/quoting",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "quote",
			"to": ["https://remote.example/users/alice/followers"],
			"_misskey_quote": "` + hiddenOwnNoteURI + `"
		}
	}`)
	f := &hiddenNoteFetcher{hidden: map[string]string{hiddenOwnNoteURI: hiddenOwnFollowersNote}}
	env := newFullProcessorFetcher(t, f)
	keys := &recordingFollowerKeys{key: newViewerKey(t)}
	env.processor.SetFollowerFetchKeys(keys)
	require.NoError(t, env.processor.Process(create))
	_, err := env.noteRepo.FindByURI("https://remote.example/notes/quoting")
	require.NoError(t, err, "the quoting note is ingested")
	assert.Contains(t, f.plain, hiddenOwnNoteURI, "the quote target is tried with the instance actor (the test's premise)")
	assert.Empty(t, f.keyed)
	assert.Empty(t, keys.calls)
}

// An Announce whose target is on another host than the announcer is not
// retried with the announcer's follower's key.
func TestProcess_AnnounceOfAnotherHostsNoteIsNotRetried(t *testing.T) {
	const third = "https://third.example/notes/x"
	f := &hiddenNoteFetcher{hidden: map[string]string{third: hiddenOwnFollowersNote}}
	env := newFullProcessorFetcher(t, f)
	keys := &recordingFollowerKeys{key: newViewerKey(t)}
	env.processor.SetFollowerFetchKeys(keys)
	require.NoError(t, env.processor.Process([]byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/other-host",
		"actor": "https://remote.example/users/alice",
		"object": "`+third+`",
		"to": ["https://remote.example/users/alice/followers"]
	}`)))
	assert.Contains(t, f.plain, third, "the target is tried with the instance actor (the test's premise)")
	assert.Empty(t, f.keyed)
	assert.Empty(t, keys.calls)
}

// An Announce a relay forwarded (the HTTP signer is a relay) is not retried,
// the same as a Create a relay forwarded.
func TestProcess_RelayForwardedAnnounceIsNotRetried(t *testing.T) {
	relayHost, relayURI := "relay.example", "https://relay.example/actor"
	relay := &model.User{ID: "relay", Username: "relay", Host: &relayHost, URI: &relayURI}
	for _, isRelay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", isRelay), func(t *testing.T) {
			f := &hiddenNoteFetcher{hidden: map[string]string{hiddenOwnNoteURI: hiddenOwnFollowersNote}}
			env := newFullProcessorFetcher(t, f)
			env.processor.SetFollowerFetchKeys(&recordingFollowerKeys{key: newViewerKey(t)})
			if isRelay {
				env.processor.SetRelayActorChecker(onlyRelay{id: relay.ID})
			}
			require.NoError(t, env.processor.ProcessWithSigner(hiddenOwnAnnounce("relay"), relay))
			if isRelay {
				assert.Empty(t, f.keyed)
				assert.Nil(t, renoteOf(env, hiddenOwnNoteURI))
			} else {
				assert.Equal(t, []string{hiddenOwnNoteURI}, f.keyed, "a signer that is not a relay is retried (the test's premise)")
			}
		})
	}
}

// onlyRelay reports only the user with id as a relay actor.
type onlyRelay struct{ id string }

func (o onlyRelay) IsRelayActor(u *model.User) bool { return u != nil && u.ID == o.id }

// A reply to a note this server does not have is ingested without the link, as
// before: Elythia does not fetch unknown reply targets, so nothing is retried.
func TestProcess_CreateReplyToHiddenNoteIsIngested(t *testing.T) {
	f := &hiddenNoteFetcher{hidden: map[string]string{hiddenOwnNoteURI: hiddenOwnFollowersNote}}
	env := newFullProcessorFetcher(t, f)
	env.processor.SetFollowerFetchKeys(&recordingFollowerKeys{key: newViewerKey(t)})
	require.NoError(t, env.processor.Process([]byte(`{
		"type": "Create",
		"id": "https://remote.example/notes/reply/activity",
		"actor": "https://remote.example/users/alice",
		"object": {
			"id": "https://remote.example/notes/reply",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "reply",
			"to": ["https://remote.example/users/alice/followers"],
			"inReplyTo": "`+hiddenOwnNoteURI+`"
		}
	}`)))
	reply, err := env.noteRepo.FindByURI("https://remote.example/notes/reply")
	require.NoError(t, err)
	assert.Nil(t, reply.ReplyID)
	assert.Empty(t, f.keyed)
}

var _ = http.StatusOK

// The quote target of an Announce target is not retried either: only the
// Announce target itself is (#3916). A followers-only quote target would not
// be linked anyway.
func TestProcess_AnnounceTargetsQuoteIsNotRetried(t *testing.T) {
	const quoting = "https://remote.example/notes/quoting-own"
	f := &hiddenNoteFetcher{
		public: map[string]string{quoting: `{
			"@context": "https://www.w3.org/ns/activitystreams",
			"id": "` + quoting + `",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "quote",
			"to": ["https://www.w3.org/ns/activitystreams#Public"],
			"_misskey_quote": "` + hiddenOwnNoteURI + `"
		}`},
		hidden: map[string]string{hiddenOwnNoteURI: hiddenOwnFollowersNote},
	}
	env := newFullProcessorFetcher(t, f)
	keys := &recordingFollowerKeys{key: newViewerKey(t)}
	env.processor.SetFollowerFetchKeys(keys)
	require.NoError(t, env.processor.Process([]byte(`{
		"type": "Announce",
		"id": "https://remote.example/announces/quoting-own",
		"actor": "https://remote.example/users/alice",
		"object": "`+quoting+`",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`)))
	require.NotNil(t, renoteOf(env, quoting), "the Announce of the public note is ingested (the test's premise)")
	assert.Contains(t, f.plain, hiddenOwnNoteURI, "the quote target is tried with the instance actor (the test's premise)")
	assert.Empty(t, f.keyed)
	assert.Empty(t, keys.calls)
}
