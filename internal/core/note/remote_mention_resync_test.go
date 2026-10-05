package note_test

import (
	"errors"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/model"
)

// resyncingRemoteUserResolver adds the stale-user re-sync of
// core/federation.RemoteUserResolver to fakeRemoteUserResolver.
type resyncingRemoteUserResolver struct {
	*fakeRemoteUserResolver
	mu       sync.Mutex
	stale    map[string]bool  // user ID -> due for re-sync
	fail     map[string]error // user ID -> re-sync error
	block    chan struct{}
	nilUser  map[string]bool // user ID -> re-sync returns (nil, nil)
	resynced []string
}

func (r *resyncingRemoteUserResolver) NeedsResync(u *model.User) bool { return r.stale[u.ID] }

func (r *resyncingRemoteUserResolver) ResyncIfStale(u *model.User) (*model.User, error) {
	r.mu.Lock()
	r.resynced = append(r.resynced, u.ID)
	r.mu.Unlock()
	if r.block != nil {
		<-r.block
	}
	if err := r.fail[u.ID]; err != nil {
		return nil, err
	}
	if r.nilUser[u.ID] {
		return nil, nil
	}
	return u, nil
}

func (r *resyncingRemoteUserResolver) resyncedIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.resynced...)
}

func newResyncMentionService(t *testing.T) (*note.CreateService, *resyncingRemoteUserResolver, func(id, username, host string)) {
	t.Helper()
	svc, repo, _, f := newRemoteMentionService(t)
	rs := &resyncingRemoteUserResolver{fakeRemoteUserResolver: f, stale: map[string]bool{}, fail: map[string]error{}}
	svc.SetRemoteUserResolver(rs)
	add := func(id, username, host string) {
		h := host
		addUser(repo, id, username, &h)
	}
	return svc, rs, add
}

// A mention of a stored remote user whose data is older than 24 hours re-syncs
// that user once per acct before the note is created, as upstream
// extractMentionedUsers → resolveUser does; a fresh one is not re-synced.
func TestCreateService_StaleMentionedUserIsResynced(t *testing.T) {
	svc, rs, add := newResyncMentionService(t)
	add("stale-id", "stale", "remote.example")
	add("fresh-id", "fresh", "remote.example")
	rs.stale["stale-id"] = true

	text := "@stale@remote.example @Stale@remote.example @fresh@remote.example"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err)
	assert.Equal(t, []string{"stale-id", "fresh-id"}, []string(created.Mentions))
	assert.Equal(t, []string{"stale-id"}, rs.resyncedIDs())
	assert.Empty(t, rs.sortedCalls(), "DB にある利用者は新規の解決に回さない")
}

// A failed re-sync drops the mention: upstream resolveUser throws and
// extractMentionedUsers turns it into null.
func TestCreateService_FailedResyncDropsMention(t *testing.T) {
	svc, rs, add := newResyncMentionService(t)
	add("stale-id", "stale", "remote.example")
	add("ok-id", "ok", "other.example")
	rs.stale["stale-id"] = true
	rs.stale["ok-id"] = true
	rs.fail["stale-id"] = errors.New("webfinger failed")

	text := "@stale@remote.example @ok@other.example"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err)
	assert.Equal(t, []string{"ok-id"}, []string(created.Mentions))
}

// Once a host turned out to be unreachable, a stale user on it is not tried
// and keeps its stored row (the failure was not confirmed for it).
func TestCreateService_ResyncSkippedOnUnreachableHostKeepsMention(t *testing.T) {
	svc, rs, add := newResyncMentionService(t)
	rs.unreachable = map[string]bool{"dead.example": true}
	add("stale-id", "stale", "dead.example")
	rs.stale["stale-id"] = true
	// 並列数を 1 にできないので、到達不能の印が付くまで再同期の起動を待たせる
	// 代わりに、未知の acct を並列数ぶん先に並べて 1 巡目を埋める。
	text := ""
	for i := range note.RemoteMentionFetchConcurrency {
		text += fmt.Sprintf("@ghost%d@dead.example ", i)
	}
	text += "@stale@dead.example"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err)
	assert.Equal(t, []string{"stale-id"}, []string(created.Mentions))
	assert.Empty(t, rs.resyncedIDs())
}

// A transport failure of the re-sync itself drops the mention like any other
// failed re-sync.
func TestCreateService_ResyncTransportFailureDropsMention(t *testing.T) {
	svc, rs, add := newResyncMentionService(t)
	add("stale-id", "stale", "remote.example")
	rs.stale["stale-id"] = true
	rs.fail["stale-id"] = &url.Error{Op: "Get", URL: "https://remote.example", Err: errors.New("i/o timeout")}

	text := "@stale@remote.example"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err)
	assert.Empty(t, []string(created.Mentions))
}

// A re-sync still running at the overall deadline keeps the stored user.
func TestCreateService_ResyncPastDeadlineKeepsMention(t *testing.T) {
	svc, rs, add := newResyncMentionService(t)
	svc.SetRemoteMentionFetchTimeoutForTest(30 * time.Millisecond)
	add("stale-id", "stale", "remote.example")
	rs.stale["stale-id"] = true
	rs.block = make(chan struct{})
	t.Cleanup(func() { close(rs.block) })

	text := "@stale@remote.example"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err)
	assert.Equal(t, []string{"stale-id"}, []string(created.Mentions))
}

// A resolver without re-sync support only resolves unknown accts.
func TestCreateService_ResolverWithoutResyncKeepsStoredMention(t *testing.T) {
	svc, repo, _, f := newRemoteMentionService(t)
	h := "remote.example"
	addUser(repo, "stored-id", "stored", &h)
	text := "@stored@remote.example"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err)
	assert.Equal(t, []string{"stored-id"}, []string(created.Mentions))
	assert.Empty(t, f.sortedCalls())
}

// A re-sync that returns no user is a failure, and a local user is never
// re-synced even if the resolver would say so.
func TestCreateService_ResyncEdgeCases(t *testing.T) {
	svc, rs, add := newResyncMentionService(t)
	add("nil-id", "nil", "remote.example")
	rs.stale["nil-id"] = true
	rs.nilUser = map[string]bool{"nil-id": true}
	rs.stale["local-id"] = true
	addUser(rs.repo, "local-id", "local", nil)

	text := "@nil@remote.example @local"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err)
	assert.Equal(t, []string{"local-id"}, []string(created.Mentions))
	assert.Equal(t, []string{"nil-id"}, rs.resyncedIDs())
}
