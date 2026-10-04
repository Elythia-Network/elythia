package federation_test

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	corefederation "github.com/shiroha-a/mk/internal/core/federation"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeWebFinger struct {
	uri  string
	err  error
	call struct{ username, host string }
}

func (f *fakeWebFinger) LookupActorURI(username, host string) (string, error) {
	f.call.username = username
	f.call.host = host
	return f.uri, f.err
}

type fakeActorResolver struct {
	user *model.User
	err  error
	uri  string
}

func (f *fakeActorResolver) ResolveActor(uri string) (*model.User, error) {
	f.uri = uri
	return f.user, f.err
}

func (f *fakeActorResolver) RefreshActor(uri string) (*model.User, error) {
	f.uri = uri
	return f.user, f.err
}

// LocalUserIDFromURI mirrors Resolver.LocalUserIDFromURI for the fixed local
// host "local.example".
func (f *fakeActorResolver) LocalUserIDFromURI(uri string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || u.Host != "local.example" {
		return "", false
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) < 3 || parts[1] != "users" {
		return "", true
	}
	return parts[2], true
}

func TestRemoteUserResolver_ResolveByUsernameHost_Success(t *testing.T) {
	wf := &fakeWebFinger{uri: "https://remote.example/users/alice"}
	host := "remote.example"
	ar := &fakeActorResolver{user: &model.User{ID: "uR", Username: "alice", Host: &host}}
	userRepo := testutil.NewMockUserRepository()

	r := corefederation.NewRemoteUserResolver(wf, ar, userRepo, "local.example")
	got, err := r.ResolveByUsernameHost("alice", "remote.example")
	require.NoError(t, err)
	assert.Equal(t, "uR", got.ID)
	assert.Equal(t, "alice", wf.call.username)
	assert.Equal(t, "remote.example", wf.call.host)
	assert.Equal(t, "https://remote.example/users/alice", ar.uri)
}

func TestRemoteUserResolver_ResolveByUsernameHost_WebFingerError(t *testing.T) {
	wf := &fakeWebFinger{err: errors.New("dial failed")}
	ar := &fakeActorResolver{}
	userRepo := testutil.NewMockUserRepository()

	r := corefederation.NewRemoteUserResolver(wf, ar, userRepo, "local.example")
	_, err := r.ResolveByUsernameHost("alice", "remote.example")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "webfinger lookup")
	assert.Empty(t, ar.uri)
}

func TestRemoteUserResolver_ResolveByUsernameHost_ResolverError(t *testing.T) {
	wf := &fakeWebFinger{uri: "https://remote.example/users/alice"}
	ar := &fakeActorResolver{err: errors.New("bad actor doc")}
	userRepo := testutil.NewMockUserRepository()

	r := corefederation.NewRemoteUserResolver(wf, ar, userRepo, "local.example")
	_, err := r.ResolveByUsernameHost("alice", "remote.example")
	require.Error(t, err)
}

func TestRemoteUserResolver_ResolveByUsernameHost_LocalHostShortCircuits(t *testing.T) {
	wf := &fakeWebFinger{err: errors.New("should not be called")}
	ar := &fakeActorResolver{err: errors.New("should not be called")}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", UsernameLower: "alice"}

	r := corefederation.NewRemoteUserResolver(wf, ar, userRepo, "local.example")
	// 大文字でも EqualFold で短絡する。
	got, err := r.ResolveByUsernameHost("alice", "LOCAL.EXAMPLE")
	require.NoError(t, err)
	assert.Equal(t, "u1", got.ID)
	assert.Equal(t, struct{ username, host string }{}, wf.call)
	assert.Empty(t, ar.uri)
}

func TestRemoteUserResolver_ResolveByUsernameHost_LocalHostWithMiss(t *testing.T) {
	// localHost 短絡後に local DB でも見つからなければ error を伝搬する。
	wf := &fakeWebFinger{}
	ar := &fakeActorResolver{}
	userRepo := testutil.NewMockUserRepository()

	r := corefederation.NewRemoteUserResolver(wf, ar, userRepo, "local.example")
	_, err := r.ResolveByUsernameHost("ghost", "local.example")
	require.Error(t, err)
}

func TestRemoteUserResolver_ResolveByUsernameHost_EmptyArgs(t *testing.T) {
	r := corefederation.NewRemoteUserResolver(&fakeWebFinger{}, &fakeActorResolver{}, nil, "")
	_, err := r.ResolveByUsernameHost("", "remote.example")
	require.Error(t, err)
	_, err = r.ResolveByUsernameHost("alice", "")
	require.Error(t, err)
}

func TestRemoteUserResolver_ResolveByUsernameHost_NilWebFinger(t *testing.T) {
	r := corefederation.NewRemoteUserResolver(nil, &fakeActorResolver{}, nil, "")
	_, err := r.ResolveByUsernameHost("alice", "remote.example")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

func TestRemoteUserResolver_ResolveByUsernameHost_LocalHostWithoutUserRepo(t *testing.T) {
	r := corefederation.NewRemoteUserResolver(&fakeWebFinger{}, &fakeActorResolver{}, nil, "local.example")
	_, err := r.ResolveByUsernameHost("alice", "local.example")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "userRepo")
}

type stubRemoteUserHostBlocker struct {
	blocked    map[string]bool
	notAllowed map[string]bool
}

func (s stubRemoteUserHostBlocker) IsBlocked(host string) bool { return s.blocked[host] }
func (s stubRemoteUserHostBlocker) IsAllowed(host string) bool { return !s.notAllowed[host] }

// A host the instance does not federate with is skipped before the WebFinger
// request (#3330); other hosts still go through.
func TestRemoteUserResolver_ResolveByUsernameHost_SkipsBlockedHost(t *testing.T) {
	host := "ok.example"
	for _, tc := range []struct {
		name, host string
		wantSkip   bool
	}{
		{"blocked", "blocked.example", true},
		{"not allowed", "outside.example", true},
		{"allowed", "ok.example", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wf := &fakeWebFinger{uri: "https://ok.example/users/alice"}
			ar := &fakeActorResolver{user: &model.User{ID: "uR", Host: &host}}
			r := corefederation.NewRemoteUserResolver(wf, ar, testutil.NewMockUserRepository(), "local.example")
			r.SetHostBlockChecker(stubRemoteUserHostBlocker{
				blocked:    map[string]bool{"blocked.example": true},
				notAllowed: map[string]bool{"outside.example": true},
			})
			_, err := r.ResolveByUsernameHost("alice", tc.host)
			if tc.wantSkip {
				require.ErrorIs(t, err, corefederation.ErrRemoteUserHostNotAllowed)
				assert.Empty(t, wf.call.host, "WebFinger を投げない")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.host, wf.call.host)
		})
	}
}
