package federation

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/testutil"
)

// joinedWaiters counts the calls that joined an in-flight note resolve.
func (r *Resolver) joinedWaiters() int {
	g := &r.resolveNoteGroup
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, c := range g.m {
		n += len(c.waiters)
	}
	return n
}

// Two relay Announces of the same new note that share one resolve: only one
// of them is told the note was newly stored, so the note is published once.
func TestResolveNoteEphemeralForInbox_SharedResolveReportsCreatedOnce(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/rin"
		noteURI  = "https://remote.example/notes/r1"
	)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	fetcher := &gatedDocFetcher{
		docs: map[string]string{
			noteURI:  plainNoteDoc(noteURI, actorURI),
			actorURI: featuredActorDoc("remote.example", "rin", ""),
		},
		gatedURI: noteURI,
		reached:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	noteRepo := testutil.NewMockNoteRepository()
	r := NewResolverNilDeps(testutil.NewMockUserRepository(), noteRepo, activitypub.NewURLBuilder("https://example.com"), fetcher, idGen)

	type result struct {
		created bool
		err     error
	}
	results := make(chan result, 2)
	resolve := func() {
		_, created, err := r.ResolveNoteEphemeralForInbox(noteURI, "remote.example")
		results <- result{created, err}
	}
	go resolve()
	<-fetcher.reached
	go resolve()
	// 2 つ目が先頭の解決に相乗りしてから放す (相乗りしないまま先頭が終わると、
	// 2 つ目は既存の行を引くだけで、ここで確かめたい形にならない)。
	require.Eventually(t, func() bool { return r.joinedWaiters() == 1 }, 5*time.Second, time.Millisecond)
	close(fetcher.release)

	created := 0
	for range 2 {
		select {
		case res := <-results:
			require.NoError(t, res.err)
			if res.created {
				created++
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the resolves did not finish")
		}
	}
	assert.Equal(t, 1, created, "only one of the joined calls reports the note as new")
	assert.Len(t, noteRepo.Notes, 1)
}

// A call that discards created (Like, Undo(Like), featured, nested resolves)
// must not take the claim. When it leads a resolve that an Announce joined,
// the Announce is still told the note is new.
func TestResolveNote_DiscardingCallDoesNotTakeCreated(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/rin"
		noteURI  = "https://remote.example/notes/r1"
	)
	for _, tc := range []struct {
		name    string
		claimer func(r *Resolver) (bool, error)
	}{
		{name: "relay Announce", claimer: func(r *Resolver) (bool, error) {
			_, created, err := r.ResolveNoteEphemeralForInbox(noteURI, "remote.example")
			return created, err
		}},
		{name: "Announce", claimer: func(r *Resolver) (bool, error) {
			_, created, err := r.ResolveNoteWithCreatedForInbox(noteURI, "remote.example")
			return created, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// どちらが先に結果を受け取るかは goroutine の順で決まる。捨てる側が
			// claim を取る誤りを、偶然の順で見逃さないよう何度か回す。
			for range 20 {
				idGen, err := id.NewGenerator("aidx")
				require.NoError(t, err)
				fetcher := &gatedDocFetcher{
					docs: map[string]string{
						noteURI:  plainNoteDoc(noteURI, actorURI),
						actorURI: featuredActorDoc("remote.example", "rin", ""),
					},
					gatedURI: noteURI,
					reached:  make(chan struct{}),
					release:  make(chan struct{}),
				}
				noteRepo := testutil.NewMockNoteRepository()
				r := NewResolverNilDeps(testutil.NewMockUserRepository(), noteRepo, activitypub.NewURLBuilder("https://example.com"), fetcher, idGen)

				discarded := make(chan error, 1)
				go func() {
					// Like と同じ入口。先頭で取得を始める。
					_, err := r.ResolveNoteForInbox(noteURI, "remote.example")
					discarded <- err
				}()
				<-fetcher.reached
				type result struct {
					created bool
					err     error
				}
				claimed := make(chan result, 1)
				go func() {
					created, err := tc.claimer(r)
					claimed <- result{created, err}
				}()
				require.Eventually(t, func() bool { return r.joinedWaiters() == 1 }, 5*time.Second, time.Millisecond)
				close(fetcher.release)

				select {
				case err := <-discarded:
					require.NoError(t, err)
				case <-time.After(5 * time.Second):
					t.Fatal("the discarding resolve did not finish")
				}
				select {
				case res := <-claimed:
					require.NoError(t, res.err)
					require.True(t, res.created, "the Announce that joined is told the note is new")
				case <-time.After(5 * time.Second):
					t.Fatal("the Announce resolve did not finish")
				}
				require.Len(t, noteRepo.Notes, 1)
			}
		})
	}
}
