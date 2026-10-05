package federation_test

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// countingWebFinger is a concurrency-safe WebFinger fake that counts lookups and
// can block until gate is closed.
type countingWebFinger struct {
	uri   string
	err   error
	gate  chan struct{}
	calls atomic.Int64
	mu    sync.Mutex
	last  struct{ username, host string }
}

func (f *countingWebFinger) LookupActorURI(username, host string) (string, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.last.username, f.last.host = username, host
	f.mu.Unlock()
	if f.gate != nil {
		<-f.gate
	}
	return f.uri, f.err
}

// refreshingActorResolver records RefreshActor calls and reports a canned
// result. RefreshActor reads the row back from repo by URI like the real one.
type refreshingActorResolver struct {
	fakeActorResolver
	repo       *testutil.MockUserRepository
	refreshErr error
	refreshed  []string
	mu         sync.Mutex
}

func (f *refreshingActorResolver) RefreshActor(uri string) (*model.User, error) {
	f.mu.Lock()
	f.refreshed = append(f.refreshed, uri)
	f.mu.Unlock()
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	return f.repo.FindByURI(uri)
}

const resyncNow = "2026-10-03T12:00:00Z"

func resyncClock(t *testing.T) func() time.Time {
	t.Helper()
	now, err := time.Parse(time.RFC3339, resyncNow)
	require.NoError(t, err)
	return func() time.Time { return now }
}

// newResyncFixture stores a remote user alice@remote.example whose
// lastFetchedAt is age before the fixed clock (nil age = never fetched).
func newResyncFixture(t *testing.T, age *time.Duration, uri string) (*corefederation.RemoteUserResolver, *countingWebFinger, *refreshingActorResolver, *testutil.MockUserRepository, *model.User) {
	t.Helper()
	repo := testutil.NewMockUserRepository()
	host := "remote.example"
	u := &model.User{ID: "uA", Username: "Alice", UsernameLower: "alice", Host: &host, URI: &uri}
	if age != nil {
		ts := resyncClock(t)().Add(-*age)
		u.LastFetchedAt = &ts
	}
	repo.Users[u.ID] = u
	wf := &countingWebFinger{uri: uri}
	ar := &refreshingActorResolver{repo: repo}
	r := corefederation.NewRemoteUserResolver(wf, ar, repo, "local.example")
	r.SetClock(resyncClock(t))
	return r, wf, ar, repo, u
}

func dur(d time.Duration) *time.Duration { return &d }

const aliceURI = "https://remote.example/users/alice"

// A user fetched within the last 24 hours is returned without any request
// (upstream resolveUser returns the existing row).
func TestResyncIfStale_FreshUserIsReturnedAsIs(t *testing.T) {
	for _, age := range []time.Duration{time.Hour, corefederation.RemoteUserResyncInterval} {
		r, wf, ar, _, u := newResyncFixture(t, dur(age), aliceURI)
		assert.False(t, r.NeedsResync(u), "age=%v", age)
		got, err := r.ResyncIfStale(u)
		require.NoError(t, err)
		assert.Same(t, u, got)
		assert.Zero(t, wf.calls.Load())
		assert.Empty(t, ar.refreshed)
	}
}

// A local user never needs a re-sync.
func TestResyncIfStale_LocalUserIsReturnedAsIs(t *testing.T) {
	r, wf, _, _, _ := newResyncFixture(t, nil, aliceURI)
	local := &model.User{ID: "uL", Username: "me", UsernameLower: "me"}
	assert.False(t, r.NeedsResync(local))
	assert.False(t, r.NeedsResync(nil))
	got, err := r.ResyncIfStale(local)
	require.NoError(t, err)
	assert.Same(t, local, got)
	assert.Zero(t, wf.calls.Load())
}

// A stale user (older than 24 hours, or never fetched) is re-synced: WebFinger
// for the acct, lastFetchedAt advanced, the actor re-fetched, and the re-read
// row returned (upstream RemoteUserResolveService.ts:97-137).
func TestResyncIfStale_StaleUserIsResynced(t *testing.T) {
	for name, age := range map[string]*time.Duration{
		"older than 24h": dur(corefederation.RemoteUserResyncInterval + time.Second),
		"never fetched":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			r, wf, ar, repo, u := newResyncFixture(t, age, aliceURI)
			require.True(t, r.NeedsResync(u))
			got, err := r.ResyncIfStale(u)
			require.NoError(t, err)
			assert.Equal(t, "uA", got.ID)
			assert.Equal(t, int64(1), wf.calls.Load())
			assert.Equal(t, "alice", wf.last.username)
			assert.Equal(t, "remote.example", wf.last.host)
			assert.Equal(t, []string{aliceURI}, ar.refreshed)
			require.NotNil(t, repo.Users["uA"].LastFetchedAt)
			assert.True(t, repo.Users["uA"].LastFetchedAt.Equal(resyncClock(t)()))
			assert.Equal(t, aliceURI, *repo.Users["uA"].URI)
		})
	}
}

// When the acct now points at another actor URI on the same host, the stored
// uri is corrected before the re-fetch (upstream "uri missmatch").
func TestResyncIfStale_FixesMovedActorURIOnSameHost(t *testing.T) {
	r, wf, ar, repo, u := newResyncFixture(t, nil, aliceURI)
	const moved = "https://remote.example/ap/actors/42"
	wf.uri = moved
	got, err := r.ResyncIfStale(u)
	require.NoError(t, err)
	assert.Equal(t, "uA", got.ID)
	assert.Equal(t, moved, *repo.Users["uA"].URI)
	assert.Equal(t, []string{moved}, ar.refreshed)
}

// A self link on another host is refused (upstream throws `Invalid uri`) and
// the stored uri is left alone. lastFetchedAt is still advanced, since upstream
// updates it before trying.
func TestResyncIfStale_RejectsActorURIOnAnotherHost(t *testing.T) {
	for _, href := range []string{
		"https://evil.example/users/alice",
		"https://local.example/users/uL",
		"::not a url",
	} {
		r, wf, ar, repo, u := newResyncFixture(t, nil, aliceURI)
		wf.uri = href
		_, err := r.ResyncIfStale(u)
		require.ErrorIs(t, err, corefederation.ErrRemoteUserURIMismatch, href)
		assert.Equal(t, aliceURI, *repo.Users["uA"].URI)
		assert.Empty(t, ar.refreshed)
		assert.NotNil(t, repo.Users["uA"].LastFetchedAt)
	}
}

// A failed WebFinger fails the re-sync, but lastFetchedAt was advanced first,
// so the next call within 24 hours does not try again.
func TestResyncIfStale_FailureAdvancesLastFetchedAt(t *testing.T) {
	r, wf, ar, repo, u := newResyncFixture(t, nil, aliceURI)
	wf.err = errors.New("dial failed")
	_, err := r.ResyncIfStale(u)
	require.Error(t, err)
	assert.Empty(t, ar.refreshed)

	again, err := r.ResyncIfStale(repo.Users["uA"])
	require.NoError(t, err)
	assert.Equal(t, "uA", again.ID)
	assert.Equal(t, int64(1), wf.calls.Load(), "24 時間以内は試し直さない")
}

// A failed actor re-fetch fails the re-sync (upstream updatePerson throws).
func TestResyncIfStale_RefreshFailureIsReported(t *testing.T) {
	r, _, ar, _, u := newResyncFixture(t, nil, aliceURI)
	ar.refreshErr = errors.New("actor down")
	_, err := r.ResyncIfStale(u)
	require.Error(t, err)
	assert.Equal(t, []string{aliceURI}, ar.refreshed)
}

// A host the instance does not federate with is not contacted.
func TestResyncIfStale_SkipsBlockedHost(t *testing.T) {
	r, wf, _, repo, u := newResyncFixture(t, nil, aliceURI)
	r.SetHostBlockChecker(stubRemoteUserHostBlocker{blocked: map[string]bool{"remote.example": true}})
	_, err := r.ResyncIfStale(u)
	require.ErrorIs(t, err, corefederation.ErrRemoteUserHostNotAllowed)
	assert.Zero(t, wf.calls.Load())
	assert.NotNil(t, repo.Users["uA"].LastFetchedAt)
}

// A DB failure while advancing lastFetchedAt stops before any request.
func TestResyncIfStale_DBFailures(t *testing.T) {
	t.Run("re-read", func(t *testing.T) {
		r, wf, _, repo, u := newResyncFixture(t, nil, aliceURI)
		repo.FindErr = errors.New("db down")
		_, err := r.ResyncIfStale(u)
		require.Error(t, err)
		assert.Zero(t, wf.calls.Load())
	})
	t.Run("row vanished", func(t *testing.T) {
		r, wf, _, repo, u := newResyncFixture(t, nil, aliceURI)
		delete(repo.Users, "uA")
		_, err := r.ResyncIfStale(u)
		require.Error(t, err)
		assert.Zero(t, wf.calls.Load())
	})
	t.Run("unconfigured", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		r := corefederation.NewRemoteUserResolver(nil, nil, repo, "")
		host := "remote.example"
		_, err := r.ResyncIfStale(&model.User{ID: "x", Host: &host})
		require.Error(t, err)
	})
}

// barrierUserRepo returns a copy of the row from FindByID, like a real DB, and
// holds every reader until want readers arrived (or a short timeout), so
// concurrent callers all read the row before any of them advances it.
type barrierUserRepo struct {
	*testutil.MockUserRepository
	mu      sync.Mutex
	want    int
	arrived int
	all     chan struct{}
}

func (b *barrierUserRepo) FindByID(id string) (*model.User, error) {
	b.mu.Lock()
	u, err := b.MockUserRepository.FindByID(id)
	var cp *model.User
	if err == nil {
		c := *u
		cp = &c
	}
	b.arrived++
	if b.arrived == b.want {
		close(b.all)
	}
	b.mu.Unlock()
	select {
	case <-b.all:
	case <-time.After(300 * time.Millisecond):
	}
	return cp, err
}

func (b *barrierUserRepo) UpdateUser(id string, fields map[string]any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.MockUserRepository.UpdateUser(id, fields)
}

// Concurrent re-syncs of one acct share a single WebFinger request even when
// every caller read the stale row before any of them advanced lastFetchedAt.
func TestResyncIfStale_ConcurrentCallsShareOneRequest(t *testing.T) {
	const n = 8
	mock := testutil.NewMockUserRepository()
	host := "remote.example"
	uri := aliceURI
	mock.Users["uA"] = &model.User{ID: "uA", Username: "Alice", UsernameLower: "alice", Host: &host, URI: &uri}
	repo := &barrierUserRepo{MockUserRepository: mock, want: n, all: make(chan struct{})}
	wf := &countingWebFinger{uri: aliceURI}
	ar := &refreshingActorResolver{repo: mock}
	r := corefederation.NewRemoteUserResolver(wf, ar, repo, "local.example")
	r.SetClock(resyncClock(t))

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stale := model.User{ID: "uA", Username: "Alice", UsernameLower: "alice", Host: &host, URI: &uri}
			_, errs[i] = r.ResyncIfStale(&stale)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int64(1), wf.calls.Load())
}

// A call that arrives after a re-sync finished, still holding the stale row,
// re-reads the row, sees it fresh and does nothing.
func TestResyncIfStale_StaleCopyAfterResyncDoesNothing(t *testing.T) {
	r, wf, ar, _, u := newResyncFixture(t, nil, aliceURI)
	snapshot := *u
	_, err := r.ResyncIfStale(u)
	require.NoError(t, err)
	require.Equal(t, int64(1), wf.calls.Load())

	stale := snapshot
	require.True(t, r.NeedsResync(&stale))
	_, err = r.ResyncIfStale(&stale)
	require.NoError(t, err)
	assert.Equal(t, int64(1), wf.calls.Load())
	assert.Len(t, ar.refreshed, 1)
}

// When another row already holds the new actor URI, the uri is not moved onto
// this row (two rows would share one actor).
func TestResyncIfStale_RefusesURIHeldByAnotherRow(t *testing.T) {
	r, wf, ar, repo, u := newResyncFixture(t, nil, aliceURI)
	const moved = "https://remote.example/ap/actors/42"
	host := "remote.example"
	other := moved
	repo.Users["uOther"] = &model.User{ID: "uOther", Username: "other", UsernameLower: "other", Host: &host, URI: &other}
	wf.uri = moved
	_, err := r.ResyncIfStale(u)
	require.ErrorIs(t, err, corefederation.ErrRemoteUserURIMismatch)
	assert.Equal(t, aliceURI, *repo.Users["uA"].URI)
	assert.Empty(t, ar.refreshed)

	t.Run("lookup failure", func(t *testing.T) {
		r, wf, ar, repo, u := newResyncFixture(t, nil, aliceURI)
		wf.uri = moved
		repo.FindByURIErr = errors.New("db down")
		_, err := r.ResyncIfStale(u)
		require.Error(t, err)
		assert.NotErrorIs(t, err, corefederation.ErrRemoteUserURIMismatch)
		assert.Equal(t, aliceURI, *repo.Users["uA"].URI)
		assert.Empty(t, ar.refreshed)
	})
}

// Concurrent resolutions of one unknown acct share a single WebFinger request.
func TestResolveByUsernameHost_ConcurrentCallsShareOneRequest(t *testing.T) {
	host := "remote.example"
	wf := &countingWebFinger{uri: aliceURI, gate: make(chan struct{})}
	ar := &fakeActorResolver{user: &model.User{ID: "uR", Host: &host}}
	r := corefederation.NewRemoteUserResolver(wf, ar, testutil.NewMockUserRepository(), "local.example")
	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "alice"
			if i%2 == 1 {
				name = "ALICE"
			}
			got, err := r.ResolveByUsernameHost(name, "remote.example")
			assert.NoError(t, err)
			if assert.NotNil(t, got) {
				assert.Equal(t, "uR", got.ID)
			}
		}(i)
	}
	require.Eventually(t, func() bool { return wf.calls.Load() == 1 }, 2*time.Second, time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	close(wf.gate)
	wg.Wait()
	assert.Equal(t, int64(1), wf.calls.Load())
}

// A WebFinger self link pointing at this instance resolves to the local user
// (upstream RemoteUserResolveService.ts:77-91) instead of failing.
func TestResolveByUsernameHost_SelfLinkToLocalUser(t *testing.T) {
	newFixture := func(t *testing.T, href string) (*corefederation.RemoteUserResolver, *fakeActorResolver, *testutil.MockUserRepository) {
		t.Helper()
		repo := testutil.NewMockUserRepository()
		repo.Users["uL"] = &model.User{ID: "uL", Username: "me", UsernameLower: "me"}
		remoteHost := "remote.example"
		repo.Users["uR"] = &model.User{ID: "uR", Username: "r", UsernameLower: "r", Host: &remoteHost}
		repo.Users["uD"] = &model.User{ID: "uD", Username: "gone", UsernameLower: "gone", IsDeleted: true}
		ar := &fakeActorResolver{err: errors.New("must not resolve a local URI remotely")}
		r := corefederation.NewRemoteUserResolver(&fakeWebFinger{uri: href}, ar, repo, "local.example")
		return r, ar, repo
	}
	t.Run("existing local user", func(t *testing.T) {
		r, ar, _ := newFixture(t, "https://local.example/users/uL")
		got, err := r.ResolveByUsernameHost("alias", "remote.example")
		require.NoError(t, err)
		assert.Equal(t, "uL", got.ID)
		assert.Empty(t, ar.uri)
	})
	for name, href := range map[string]string{
		"missing user":       "https://local.example/users/nobody",
		"deleted user":       "https://local.example/users/uD",
		"remote row id":      "https://local.example/users/uR",
		"not a users uri":    "https://local.example/notes/n1",
		"users without id":   "https://local.example/users",
		"empty id":           "https://local.example/users/",
		"scheme is not used": "http://local.example/users/nobody",
	} {
		t.Run(name, func(t *testing.T) {
			r, ar, _ := newFixture(t, href)
			_, err := r.ResolveByUsernameHost("alias", "remote.example")
			require.ErrorIs(t, err, corefederation.ErrLocalUserNotFound)
			assert.Empty(t, ar.uri)
		})
	}
	t.Run("db failure", func(t *testing.T) {
		r, _, repo := newFixture(t, "https://local.example/users/uL")
		repo.FindErr = errors.New("db down")
		_, err := r.ResolveByUsernameHost("alias", "remote.example")
		require.Error(t, err)
		assert.NotErrorIs(t, err, corefederation.ErrLocalUserNotFound)
	})
	t.Run("no user repo", func(t *testing.T) {
		ar := &fakeActorResolver{}
		r := corefederation.NewRemoteUserResolver(&fakeWebFinger{uri: "https://local.example/users/uL"}, ar, nil, "local.example")
		_, err := r.ResolveByUsernameHost("alias", "remote.example")
		require.ErrorIs(t, err, corefederation.ErrLocalUserNotFound)
	})
}
