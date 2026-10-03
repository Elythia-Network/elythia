package note_test

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/note"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

// fakeRemoteUserResolver stands in for core/federation.RemoteUserResolver
// (WebFinger + actor fetch). Known accts are "username@host" (lower case);
// resolving one upserts the user into repo like the real resolver does.
type fakeRemoteUserResolver struct {
	mu    sync.Mutex
	repo  *testutil.MockUserRepository
	known map[string]string // acct -> user ID
	calls []string
	panic string // acct that panics
	// block, when non-nil, makes every resolve wait until it is closed.
	block chan struct{}
	// unreachable hosts fail with a transport error (*url.Error).
	unreachable map[string]bool
}

func (f *fakeRemoteUserResolver) ResolveByUsernameHost(username, host string) (*model.User, error) {
	acct := username + "@" + host
	f.mu.Lock()
	f.calls = append(f.calls, acct)
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	if acct == f.panic {
		panic("boom")
	}
	if f.unreachable[host] {
		return nil, fmt.Errorf("webfinger lookup: %w", &url.Error{Op: "Get", URL: "https://" + host, Err: errors.New("i/o timeout")})
	}
	id, ok := f.known[acct]
	if !ok {
		return nil, errors.New("webfinger failed")
	}
	h := host
	u := &model.User{ID: id, Username: username, UsernameLower: strings.ToLower(username), Host: &h}
	f.mu.Lock()
	f.repo.Users[id] = u
	f.mu.Unlock()
	return u, nil
}

func (f *fakeRemoteUserResolver) sortedCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.calls...)
	sort.Strings(out)
	return out
}

func newRemoteMentionService(t *testing.T) (*note.CreateService, *testutil.MockUserRepository, *testutil.MockNoteRepository, *fakeRemoteUserResolver) {
	t.Helper()
	svc, repo, noteRepo := newMentionTestService(t)
	f := &fakeRemoteUserResolver{repo: repo, known: map[string]string{}}
	svc.SetRemoteUserResolver(f)
	return svc, repo, noteRepo, f
}

// A mention of a remote user that is not in the DB is resolved via WebFinger
// (upstream extractMentionedUsers → RemoteUserResolveService.resolveUser), in
// mention order, and a failed fetch is dropped (`.catch(() => null)`).
func TestCreateService_MentionOfUnknownRemoteUserIsFetched(t *testing.T) {
	svc, repo, _, f := newRemoteMentionService(t)
	known := "known.example"
	addUser(repo, "known-id", "known", &known)
	addUser(repo, "local-id", "local", nil)
	f.known["carol@remote.example"] = "carol-id"

	text := "@Carol@remote.example @known@known.example @ghost@remote.example @local @nobody"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err)
	assert.Equal(t, []string{"carol-id", "known-id", "local-id"}, []string(created.Mentions))
	// DB にある利用者とローカルの利用者は取りに行かない。
	assert.Equal(t, []string{"carol@remote.example", "ghost@remote.example"}, f.sortedCalls())
}

// The same remote user written in different cases is fetched once.
func TestCreateService_RemoteMentionFetchedOncePerAcct(t *testing.T) {
	svc, _, _, f := newRemoteMentionService(t)
	f.known["carol@remote.example"] = "carol-id"

	text := "@Carol@remote.example @carol@remote.example"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err)
	assert.Equal(t, []string{"carol-id"}, []string(created.Mentions))
	assert.Equal(t, []string{"carol@remote.example"}, f.sortedCalls())
}

// A remote author's host-less mention and `@user@<own host>` are not fetched:
// the latter is a local user and does not go to WebFinger.
func TestCreateService_RemoteMentionNotFetchedForOwnHost(t *testing.T) {
	svc, _, _, f := newRemoteMentionService(t)
	svc.SetLocalHost("local.example")

	text := "@nobody@local.example"
	_, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err)
	assert.Empty(t, f.sortedCalls())
}

// Fetched users count toward the mention limit, like upstream which checks the
// limit after extractMentionedUsers.
func TestCreateService_FetchedRemoteMentionsCountTowardLimit(t *testing.T) {
	svc, _, _, f := newRemoteMentionService(t)
	svc.SetRolePolicyProvider(&stubRolePolicies{byUser: map[string]map[string]any{
		"author": {"mentionLimit": 1},
	}})
	f.known["a@remote.example"] = "a-id"
	f.known["b@remote.example"] = "b-id"

	text := "@a@remote.example @b@remote.example"
	_, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	assert.ErrorIs(t, err, note.ErrContainsTooManyMentions)
}

// When the DB-resolved mentions already exceed the limit, nothing is fetched:
// fetching can only add users, so the outcome is the same as upstream's.
func TestCreateService_RemoteMentionNotFetchedWhenAlreadyOverLimit(t *testing.T) {
	svc, repo, _, f := newRemoteMentionService(t)
	svc.SetRolePolicyProvider(&stubRolePolicies{byUser: map[string]map[string]any{
		"author": {"mentionLimit": 1},
	}})
	addUser(repo, "a-id", "a", nil)
	addUser(repo, "b-id", "b", nil)

	text := "@a @b @c@remote.example"
	_, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	assert.ErrorIs(t, err, note.ErrContainsTooManyMentions)
	assert.Empty(t, f.sortedCalls())
}

// The fetch runs after the request's validation, so a request that fails
// validation (here: a missing reply target) causes no outbound request.
func TestCreateService_RemoteMentionNotFetchedForInvalidRequest(t *testing.T) {
	svc, _, _, f := newRemoteMentionService(t)
	f.known["carol@remote.example"] = "carol-id"

	text := "@carol@remote.example"
	missing := "missing"
	_, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text, ReplyID: &missing})
	assert.ErrorIs(t, err, note.ErrReplyTargetNotFound)
	assert.Empty(t, f.sortedCalls())
}

// A failed DB lookup for a host is not turned into a WebFinger request.
func TestCreateService_RemoteMentionNotFetchedOnDBError(t *testing.T) {
	svc, _, _ := newCreateService(t)
	mock := testutil.NewMockUserRepository()
	svc.SetUserRepo(&flakyUserRepo{MockUserRepository: mock, failHost: "remote.example"})
	f := &fakeRemoteUserResolver{repo: mock, known: map[string]string{"carol@remote.example": "carol-id"}}
	svc.SetRemoteUserResolver(f)

	text := "@carol@remote.example"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err)
	assert.Empty(t, []string(created.Mentions))
	assert.Empty(t, f.sortedCalls())
}

// noExtractMentions skips the fetch too.
func TestCreateService_RemoteMentionNotFetchedWithNoExtractMentions(t *testing.T) {
	svc, _, _, f := newRemoteMentionService(t)
	f.known["carol@remote.example"] = "carol-id"

	text := "@carol@remote.example"
	_, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text, NoExtractMentions: true})
	require.NoError(t, err)
	assert.Empty(t, f.sortedCalls())
}

// A panicking resolver drops that mention instead of failing the post, and the
// fetches of many mentions all complete.
func TestCreateService_RemoteMentionFetchPanicAndMany(t *testing.T) {
	svc, _, _, f := newRemoteMentionService(t)
	f.panic = "bad@remote.example"
	var text string
	var want []string
	for i := 0; i < 10; i++ {
		name := "u" + strPtr254Str(i)
		f.known[name+"@remote.example"] = name + "-id"
		want = append(want, name+"-id")
		text += "@" + name + "@remote.example "
	}
	text += "@bad@remote.example"
	expires := time.Now().Add(time.Hour)
	created, err := svc.Create(note.CreateInput{
		User: &model.User{ID: "author"},
		Text: &text,
		Poll: &note.PollInput{Choices: []string{"x", "y"}, ExpiresAt: &expires},
	})
	require.NoError(t, err)
	assert.Equal(t, want, []string(created.Mentions))
	assert.Len(t, f.sortedCalls(), 11)
}

// Upstream extracts hashtags from the text, the CW and the poll choices
// combined (NoteCreateService.create combinedTokens).
func TestCreateService_HashtagsFromPollChoices(t *testing.T) {
	svc, _, _ := newCreateService(t)
	text := "text #a"
	cw := "cw #b"
	expires := time.Now().Add(time.Hour)
	created, err := svc.Create(note.CreateInput{
		User: &model.User{ID: "author"},
		Text: &text,
		CW:   &cw,
		Poll: &note.PollInput{Choices: []string{"#C choice", "#a again"}, ExpiresAt: &expires},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b", "c"}, []string(created.Tags))

	t.Run("noExtractHashtags", func(t *testing.T) {
		created, err := svc.Create(note.CreateInput{
			User:              &model.User{ID: "author"},
			Poll:              &note.PollInput{Choices: []string{"#c", "x"}, ExpiresAt: &expires},
			NoExtractHashtags: true,
		})
		require.NoError(t, err)
		assert.Empty(t, []string(created.Tags))
	})
}

func setMentionLimit(svc *note.CreateService, limit int) {
	svc.SetRolePolicyProvider(&stubRolePolicies{byUser: map[string]map[string]any{
		"author": {"mentionLimit": limit},
	}})
}

// The number of accts fetched is capped by what is left of the author's
// mentionLimit: every fetchable acct is counted as if it resolved, and a note
// over the limit is rejected without any outbound request.
func TestCreateService_RemoteMentionFetchCappedByMentionLimit(t *testing.T) {
	text := "@a @x@remote.example @y@remote.example"

	t.Run("over the remaining budget is rejected without fetching", func(t *testing.T) {
		svc, repo, _, f := newRemoteMentionService(t)
		addUser(repo, "a-id", "a", nil)
		f.known["x@remote.example"] = "x-id"
		setMentionLimit(svc, 2)
		_, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
		assert.ErrorIs(t, err, note.ErrContainsTooManyMentions)
		assert.Empty(t, f.sortedCalls())
	})

	t.Run("within the budget is fetched", func(t *testing.T) {
		svc, repo, _, f := newRemoteMentionService(t)
		addUser(repo, "a-id", "a", nil)
		f.known["x@remote.example"] = "x-id"
		setMentionLimit(svc, 3)
		created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
		require.NoError(t, err)
		assert.Equal(t, []string{"a-id", "x-id"}, []string(created.Mentions))
		assert.Equal(t, []string{"x@remote.example", "y@remote.example"}, f.sortedCalls())
	})
}

// With many mentions and a resolver that never answers, the post returns
// within the overall deadline instead of waiting for every fetch.
func TestCreateService_RemoteMentionFetchHasOverallDeadline(t *testing.T) {
	svc, _, _, f := newRemoteMentionService(t)
	setMentionLimit(svc, 1000)
	f.block = make(chan struct{})
	t.Cleanup(func() { close(f.block) })
	svc.SetRemoteMentionFetchTimeoutForTest(200 * time.Millisecond)

	var text string
	for i := 0; i < 40; i++ {
		text += fmt.Sprintf("@u%d@h%d.example ", i, i)
	}
	start := time.Now()
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Empty(t, []string(created.Mentions), "締め切りに間に合わなかった分は解決できなかった扱い")
	assert.Less(t, elapsed, 2*time.Second)
	assert.LessOrEqual(t, len(f.sortedCalls()), note.RemoteMentionFetchConcurrency,
		"締め切りの後は新しい取得を始めない")
}

// Once a host turns out to be unreachable, the rest of that host's mentions are
// skipped; a plain "no such account" failure does not skip the host.
func TestCreateService_RemoteMentionSkipsUnreachableHost(t *testing.T) {
	svc, _, _, f := newRemoteMentionService(t)
	setMentionLimit(svc, 1000)
	f.unreachable = map[string]bool{"dead.example": true}
	f.known["ok@alive.example"] = "ok-id"

	var text string
	for i := 0; i < 10; i++ {
		text += fmt.Sprintf("@d%d@dead.example ", i)
	}
	for i := 0; i < 6; i++ {
		text += fmt.Sprintf("@missing%d@alive.example ", i)
	}
	text += "@ok@alive.example"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err)
	assert.Equal(t, []string{"ok-id"}, []string(created.Mentions))

	dead, alive := 0, 0
	for _, c := range f.sortedCalls() {
		if strings.HasSuffix(c, "@dead.example") {
			dead++
		} else {
			alive++
		}
	}
	assert.LessOrEqual(t, dead, note.RemoteMentionFetchConcurrency, "到達できないホストの残りは取りに行かない")
	assert.Equal(t, 7, alive, "存在しないアカウントの失敗ではホストを省かない")
}

// Remote hosts are normalized (lower case + punycode) before counting and
// fetching, like upstream resolveUser's toPuny: the same acct spelled with a
// different host case is one acct, fetched once, under the normalized host.
func TestCreateService_RemoteMentionHostNormalized(t *testing.T) {
	svc, _, _, f := newRemoteMentionService(t)
	setMentionLimit(svc, 1)
	f.known["a@remote.example"] = "a-id"

	text := "@a@Remote.example @a@remote.example"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err, "綴りが違うだけの同じ acct は 1 件と数える")
	assert.Equal(t, []string{"a-id"}, []string(created.Mentions))
	assert.Equal(t, []string{"a@remote.example"}, f.sortedCalls())

	// MFM の mention は ASCII のホストしか読まないが、リモートの作者のホスト
	// (ホスト無しの mention の解決先) は IDN でありうる。
	t.Run("IDN host is fetched in punycode", func(t *testing.T) {
		svc, _, _, f := newRemoteMentionService(t)
		idn := "パイ.example"
		svc.ResolveMentionUserIDsForTest([]note.Mention{{Username: "b"}}, &idn)
		assert.Equal(t, []string{"b@xn--eckve.example"}, f.sortedCalls())
	})
}

// The pre-fetch cap counts the reply target's author and a specified note's
// recipients together with the accts to fetch (upstream's mentionedUsers).
func TestCreateService_RemoteMentionCapCountsReplyAuthorAndRecipients(t *testing.T) {
	const limit = 3
	unknown := func(n int) string {
		var s string
		for i := 0; i < n; i++ {
			s += fmt.Sprintf("@u%d@remote.example ", i)
		}
		return s
	}

	t.Run("reply author", func(t *testing.T) {
		for _, tc := range []struct {
			accts   int
			wantErr bool
		}{{limit, true}, {limit - 1, false}} {
			svc, _, noteRepo, f := newRemoteMentionService(t)
			setMentionLimit(svc, limit)
			noteRepo.Notes["other"] = &model.Note{ID: "other", UserID: "someone", Visibility: model.NoteVisibilityPublic}
			replyID := "other"
			text := unknown(tc.accts)
			_, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text, ReplyID: &replyID})
			if tc.wantErr {
				assert.ErrorIs(t, err, note.ErrContainsTooManyMentions, "返信先の作者 + 未知の acct %d 件", tc.accts)
				assert.Empty(t, f.sortedCalls())
			} else {
				assert.NoError(t, err)
				assert.Len(t, f.sortedCalls(), tc.accts)
			}
		}
	})

	t.Run("specified recipients", func(t *testing.T) {
		for _, tc := range []struct {
			accts   int
			wantErr bool
		}{{limit, true}, {limit - 1, false}} {
			svc, _, _, f := newRemoteMentionService(t)
			setMentionLimit(svc, limit)
			text := unknown(tc.accts)
			_, err := svc.Create(note.CreateInput{
				User: &model.User{ID: "author"}, Text: &text,
				Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: []string{"recipient"},
			})
			if tc.wantErr {
				assert.ErrorIs(t, err, note.ErrContainsTooManyMentions, "宛先 + 未知の acct %d 件", tc.accts)
				assert.Empty(t, f.sortedCalls())
			} else {
				assert.NoError(t, err)
				assert.Len(t, f.sortedCalls(), tc.accts)
			}
		}
	})
}
