package federation_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/activitypub"
	corefederation "github.com/shiroha-a/mk/internal/core/federation"
	corenote "github.com/shiroha-a/mk/internal/core/note"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

// routeFetcher serves actor documents by URI and counts fetches per URI.
type routeFetcher struct {
	mu     sync.Mutex
	bodies map[string]string
	errs   map[string]error
	delay  time.Duration
	calls  map[string]int
}

func newRouteFetcher() *routeFetcher {
	return &routeFetcher{bodies: map[string]string{}, errs: map[string]error{}, calls: map[string]int{}}
}

func (f *routeFetcher) FetchObject(uri string) ([]byte, error) {
	f.mu.Lock()
	f.calls[uri]++
	body, ok := f.bodies[uri]
	err := f.errs[uri]
	delay := f.delay
	f.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("not found")
	}
	return []byte(body), nil
}

func (f *routeFetcher) count(uri string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[uri]
}

// total counts fetches of every URI except the note author's.
func (f *routeFetcher) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for uri, c := range f.calls {
		if uri != mentionAuthorURI {
			n += c
		}
	}
	return n
}

func actorDoc(uri, username string) string {
	doc := map[string]any{
		"@context":          "https://www.w3.org/ns/activitystreams",
		"id":                uri,
		"type":              "Person",
		"preferredUsername": username,
		"inbox":             uri + "/inbox",
		"publicKey": map[string]any{
			"id":           uri + "#main-key",
			"owner":        uri,
			"publicKeyPem": "-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----",
		},
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

const mentionAuthorURI = "https://remote.example/users/alice"

// newMentionFetchResolver returns a Resolver whose note author alice is
// already stored, and a fetcher that serves alice's document.
func newMentionFetchResolver(t *testing.T) (*corefederation.Resolver, *testutil.MockUserRepository, *routeFetcher) {
	t.Helper()
	repo := testutil.NewMockUserRepository()
	host := "remote.example"
	uri := mentionAuthorURI
	now := time.Now()
	repo.Users["uAlice"] = &model.User{ID: "uAlice", Username: "alice", UsernameLower: "alice", Host: &host, URI: &uri, LastFetchedAt: &now}
	f := newRouteFetcher()
	f.bodies[mentionAuthorURI] = actorDoc(mentionAuthorURI, "alice")
	idGen, _ := id.NewGenerator("aidx")
	r := corefederation.NewResolver(repo, testutil.NewMockNoteRepository(), activitypub.NewURLBuilder("https://example.com"), f, idGen)
	return r, repo, f
}

// noteBody builds an inbound Note by alice with the given Mention hrefs and
// audience.
func noteBody(t *testing.T, noteID string, mentions, to, cc []string) []byte {
	t.Helper()
	tags := make([]any, 0, len(mentions))
	for _, h := range mentions {
		tags = append(tags, map[string]any{"type": "Mention", "href": h})
	}
	doc := map[string]any{
		"@context":     "https://www.w3.org/ns/activitystreams",
		"id":           "https://remote.example/notes/" + noteID,
		"type":         "Note",
		"attributedTo": mentionAuthorURI,
		"content":      "hi",
		"to":           to,
		"cc":           cc,
		"tag":          tags,
	}
	b, err := json.Marshal(doc)
	require.NoError(t, err)
	return b
}

func userIDByURI(t *testing.T, repo *testutil.MockUserRepository, uri string) string {
	t.Helper()
	u, err := repo.FindByURI(uri)
	require.NoError(t, err, uri)
	return u.ID
}

// A Mention of a remote actor that is not in the DB is fetched and stored, and
// the note mentions it (upstream ApMentionService.extractApMentions →
// resolvePerson). Previously the mention was dropped.
func TestIngestNote_FetchesUnknownMentionedActor(t *testing.T) {
	r, repo, f := newMentionFetchResolver(t)
	const carol = "https://other.example/users/carol"
	const carolFeatured = carol + "/collections/featured"
	doc := actorDoc(carol, "carol")
	f.bodies[carol] = doc[:len(doc)-1] + `,"featured":"` + carolFeatured + `"}`
	f.bodies[carolFeatured] = `{"@context":"https://www.w3.org/ns/activitystreams","id":"` + carolFeatured + `","type":"OrderedCollection","orderedItems":[]}`
	idGen, _ := id.NewGenerator("aidx")
	r.SetPinningRepo(testutil.NewMockUserNotePiningRepository(), idGen)

	note, err := r.IngestNote(noteBody(t, "m1", []string{carol, carol}, []string{activitypub.Public}, nil))
	require.NoError(t, err)
	carolID := userIDByURI(t, repo, carol)
	assert.Contains(t, []string(note.Mentions), carolID)
	assert.Equal(t, 1, f.count(carol), "同じ href は 1 回だけ取りに行く")
	assert.Zero(t, f.count(carolFeatured), "メンションで取り込んだ actor の featured は取らない")
}

// A specified note's recipient that is not in the DB is fetched and can read
// the note (upstream ApAudienceService.parseAudience → resolvePerson).
func TestIngestNote_FetchesUnknownSpecifiedRecipient(t *testing.T) {
	r, repo, f := newMentionFetchResolver(t)
	const dave = "https://other.example/users/dave"
	f.bodies[dave] = actorDoc(dave, "dave")

	note, err := r.IngestNote(noteBody(t, "m2", nil, nil, []string{dave}))
	require.NoError(t, err)
	require.Equal(t, model.NoteVisibilitySpecified, note.Visibility)
	daveID := userIDByURI(t, repo, dave)
	assert.Contains(t, []string(note.VisibleUserIDs), daveID)
	assert.Equal(t, 1, f.count(dave))
}

// Recipients of a non-specified note are not fetched (mk-go only resolves the
// audience of a specified note, see docs/divergence.md).
func TestIngestNote_DoesNotFetchAudienceOfPublicNote(t *testing.T) {
	r, _, f := newMentionFetchResolver(t)
	const dave = "https://other.example/users/dave"
	f.bodies[dave] = actorDoc(dave, "dave")

	_, err := r.IngestNote(noteBody(t, "m3", nil, []string{activitypub.Public}, []string{dave}))
	require.NoError(t, err)
	assert.Zero(t, f.count(dave))
}

// Known, local and blocked actors are not fetched.
func TestIngestNote_SkipsKnownLocalAndBlockedMentions(t *testing.T) {
	r, repo, f := newMentionFetchResolver(t)
	host := "other.example"
	known := "https://other.example/users/known"
	repo.Users["uKnown"] = &model.User{ID: "uKnown", Username: "known", UsernameLower: "known", Host: &host, URI: &known}
	repo.Users["uLocal"] = &model.User{ID: "uLocal", Username: "local", UsernameLower: "local"}
	const blocked = "https://blocked.example/users/x"
	f.bodies[blocked] = actorDoc(blocked, "x")
	r.SetHostBlockChecker(stubRemoteUserHostBlocker{blocked: map[string]bool{"blocked.example": true}})

	note, err := r.IngestNote(noteBody(t, "m4",
		[]string{known, "https://example.com/users/uLocal", "https://example.com/notes/n1", blocked, ""},
		[]string{activitypub.Public}, nil))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"uKnown", "uLocal"}, []string(note.Mentions))
	assert.Zero(t, f.total(), "取りに行かない")
}

// A note with more distinct Mentions than the mention limit is rejected
// anyway, so nothing is fetched for it.
func TestIngestNote_TooManyMentionsFetchesNothing(t *testing.T) {
	r, _, f := newMentionFetchResolver(t)
	var mentions []string
	for i := range corenote.DefaultMentionLimit + 1 {
		u := fmt.Sprintf("https://other.example/users/u%d", i)
		f.bodies[u] = actorDoc(u, fmt.Sprintf("u%d", i))
		mentions = append(mentions, u)
	}
	_, err := r.IngestNote(noteBody(t, "m5", mentions, []string{activitypub.Public}, nil))
	require.ErrorIs(t, err, corenote.ErrContainsTooManyMentions)
	assert.Zero(t, f.total())
}

// At most the author's mentionLimit (the default here) unknown actors are
// fetched for one note.
func TestIngestNote_CapsUnknownActorFetches(t *testing.T) {
	r, _, f := newMentionFetchResolver(t)
	var cc []string
	for i := range corenote.DefaultMentionLimit + 5 {
		// 取得は失敗させる (解決できた数で上限に掛からないように)。
		cc = append(cc, fmt.Sprintf("https://h%d.example/users/x", i))
	}
	_, err := r.IngestNote(noteBody(t, "m6", nil, nil, cc))
	require.NoError(t, err)
	assert.Equal(t, corenote.DefaultMentionLimit, f.total())
}

// Once a host turned out to be unreachable, its other actors are skipped.
func TestIngestNote_SkipsRestOfUnreachableHost(t *testing.T) {
	r, repo, f := newMentionFetchResolver(t)
	dead := []string{"https://dead.example/users/a", "https://dead.example/users/b", "https://dead.example/users/c"}
	for _, u := range dead {
		f.errs[u] = dialError(u)
	}
	const ok = "https://other.example/users/ok"
	f.bodies[ok] = actorDoc(ok, "ok")
	note, err := r.IngestNote(noteBody(t, "m7", append(dead, ok), []string{activitypub.Public}, nil))
	require.NoError(t, err)
	assert.Equal(t, 1, f.count(dead[0]))
	assert.Zero(t, f.count(dead[1]))
	assert.Zero(t, f.count(dead[2]))
	assert.Contains(t, []string(note.Mentions), userIDByURI(t, repo, ok))

	// 文書の不備 (到達はできた) ではホストを諦めない。
	r2, _, f2 := newMentionFetchResolver(t)
	bad := []string{"https://bad.example/users/a", "https://bad.example/users/b"}
	for _, u := range bad {
		f2.bodies[u] = `{"not":"an actor"}`
	}
	_, err = r2.IngestNote(noteBody(t, "m8", bad, []string{activitypub.Public}, nil))
	require.NoError(t, err)
	assert.Equal(t, 1, f2.count(bad[0]))
	assert.Equal(t, 1, f2.count(bad[1]))
}

// No new fetch starts after the wall-clock budget.
func TestIngestNote_UnknownActorFetchBudget(t *testing.T) {
	corefederation.SetInboundMentionFetchBudget(t, 10*time.Millisecond)
	r, _, f := newMentionFetchResolver(t)
	f.delay = 30 * time.Millisecond
	var mentions []string
	for i := range 3 {
		mentions = append(mentions, fmt.Sprintf("https://h%d.example/users/x", i))
	}
	_, err := r.IngestNote(noteBody(t, "m9", mentions, []string{activitypub.Public}, nil))
	require.NoError(t, err)
	assert.Equal(t, 1, f.total())
}

// A DB failure while checking whether an actor is known stops the fetches
// (a persistent failure then fails the ingest so the inbox job is retried).
func TestIngestNote_UnknownActorLookupFailureStops(t *testing.T) {
	t.Run("transient", func(t *testing.T) {
		r, repo, f := newMentionFetchResolver(t)
		const carol = "https://other.example/users/carol"
		f.bodies[carol] = actorDoc(carol, "carol")
		failed := false
		repo.FindByURIHook = func(uri string) error {
			if uri == carol && !failed {
				failed = true
				return errors.New("db down")
			}
			return nil
		}
		_, err := r.IngestNote(noteBody(t, "m10", []string{carol}, []string{activitypub.Public}, nil))
		require.NoError(t, err)
		assert.Zero(t, f.count(carol), "照合に失敗したら取りに行かない")
	})
	t.Run("persistent", func(t *testing.T) {
		r, repo, f := newMentionFetchResolver(t)
		const carol = "https://other.example/users/carol"
		f.bodies[carol] = actorDoc(carol, "carol")
		repo.FindByURIHook = func(uri string) error {
			if uri == carol {
				return errors.New("db down")
			}
			return nil
		}
		_, err := r.IngestNote(noteBody(t, "m11", []string{carol}, []string{activitypub.Public}, nil))
		require.Error(t, err)
		assert.Zero(t, f.count(carol))
	})
}

// Entries that are skipped without a fetch (blocked host, local URI, known
// actor) do not use up the per-note fetch limit.
func TestIngestNote_SkippedEntriesDoNotUseFetchLimit(t *testing.T) {
	for _, kind := range []string{"blocked", "local", "known"} {
		t.Run(kind, func(t *testing.T) {
			r, repo, f := newMentionFetchResolver(t)
			r.SetHostBlockChecker(stubRemoteUserHostBlocker{blocked: map[string]bool{"blocked.example": true}})
			var cc []string
			for i := range corenote.DefaultMentionLimit {
				var u string
				switch kind {
				case "blocked":
					u = fmt.Sprintf("https://blocked.example/users/x%d", i)
				case "local":
					u = fmt.Sprintf("https://example.com/notes/n%d", i)
				case "known":
					u = fmt.Sprintf("https://known.example/users/k%d", i)
					host := "known.example"
					uri := u
					now := time.Now()
					id := fmt.Sprintf("uK%d", i)
					repo.Users[id] = &model.User{ID: id, Username: fmt.Sprintf("k%d", i), UsernameLower: fmt.Sprintf("k%d", i), Host: &host, URI: &uri, LastFetchedAt: &now}
				}
				cc = append(cc, u)
			}
			const last = "https://other.example/users/last"
			f.bodies[last] = actorDoc(last, "last")
			cc = append(cc, last)
			_, err := r.IngestNote(noteBody(t, "m12"+kind, nil, nil, cc))
			if kind == "known" {
				// 既知の 20 人 + last で宛先が上限を超える。弾かれても取得は済んでいる。
				require.ErrorIs(t, err, corenote.ErrContainsTooManyMentions)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, 1, f.count(last))
		})
	}
}

// When every fetch slot is taken by other inbox workers, the note is ingested
// with the DB lookup only (the behavior before unknown actors were fetched),
// and a slot is returned after each note so later notes can fetch again.
func TestIngestNote_UnknownActorFetchSlots(t *testing.T) {
	r, repo, f := newMentionFetchResolver(t)
	host := "other.example"
	known := "https://other.example/users/known"
	repo.Users["uKnown"] = &model.User{ID: "uKnown", Username: "known", UsernameLower: "known", Host: &host, URI: &known}
	const carol = "https://other.example/users/carol"
	f.bodies[carol] = actorDoc(carol, "carol")

	release := r.HoldMentionFetchSlots()
	note, err := r.IngestNote(noteBody(t, "s1", []string{carol, known}, []string{activitypub.Public}, nil))
	release()
	require.NoError(t, err)
	assert.Equal(t, []string{"uKnown"}, []string(note.Mentions))
	assert.Zero(t, f.count(carol), "枠が無ければ取りに行かない")

	// 枠は 1 ノートごとに返る。枠の数より多くのノートを続けて取り込めること。
	for i := range corefederation.InboundMentionFetchSlots + 1 {
		u := fmt.Sprintf("https://other.example/users/n%d", i)
		f.bodies[u] = actorDoc(u, fmt.Sprintf("n%d", i))
		_, err := r.IngestNote(noteBody(t, fmt.Sprintf("s2-%d", i), []string{u}, []string{activitypub.Public}, nil))
		require.NoError(t, err)
		assert.Equal(t, 1, f.count(u), "note %d", i)
	}
}

// A failed fetch is remembered for a while across notes: the same URI is not
// fetched again, nor any actor on a host that was unreachable; another URI on
// a reachable host still is. After the TTL they are tried again.
func TestIngestNote_UnknownActorFailureMemory(t *testing.T) {
	r, _, f := newMentionFetchResolver(t)
	now := time.Now()
	r.SetClock(func() time.Time { return now })
	const deadA, deadB = "https://dead.example/users/a", "https://dead.example/users/b"
	for _, u := range []string{deadA, deadB} {
		f.errs[u] = dialError(u)
	}
	const badA, badB = "https://bad.example/users/a", "https://bad.example/users/b"
	f.bodies[badA] = `{"not":"an actor"}`
	f.bodies[badB] = actorDoc(badB, "b")

	_, err := r.IngestNote(noteBody(t, "f1", []string{deadA, badA}, []string{activitypub.Public}, nil))
	require.NoError(t, err)
	require.Equal(t, 1, f.count(deadA))
	require.Equal(t, 1, f.count(badA))

	_, err = r.IngestNote(noteBody(t, "f2", []string{deadA, deadB, badA, badB}, []string{activitypub.Public}, nil))
	require.NoError(t, err)
	assert.Equal(t, 1, f.count(deadA), "失敗した URI は覚えている")
	assert.Zero(t, f.count(deadB), "到達できなかったホストは覚えている")
	assert.Equal(t, 1, f.count(badA), "失敗した URI は覚えている")
	assert.Equal(t, 1, f.count(badB), "文書の不備ではホストを諦めない")

	now = now.Add(corefederation.InboundMentionFailureTTL + time.Second)
	_, err = r.IngestNote(noteBody(t, "f3", []string{deadA, badA}, []string{activitypub.Public}, nil))
	require.NoError(t, err)
	assert.Equal(t, 2, f.count(deadA), "期限が過ぎたら試し直す")
	assert.Equal(t, 2, f.count(badA))
}

// dialError is what the HTTP client returns when it cannot connect to uri.
func dialError(uri string) error {
	return &url.Error{Op: "Get", URL: uri, Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}}
}

// A Mention with a scheme other than http(s) is not fetched, and cannot make
// the instance remember a healthy host as unreachable.
func TestIngestNote_NonHTTPMentionIsNotFetched(t *testing.T) {
	r, repo, f := newMentionFetchResolver(t)
	const ftp = "ftp://healthy.example/users/nobody"
	f.errs[ftp] = &url.Error{Op: "Get", URL: ftp, Err: errors.New(`unsupported protocol scheme "ftp"`)}
	const real = "https://healthy.example/users/real"
	f.bodies[real] = actorDoc(real, "real")

	_, err := r.IngestNote(noteBody(t, "x1", []string{ftp}, []string{activitypub.Public}, nil))
	require.NoError(t, err)
	assert.Zero(t, f.count(ftp))

	note, err := r.IngestNote(noteBody(t, "x2", []string{real}, []string{activitypub.Public}, nil))
	require.NoError(t, err)
	assert.Equal(t, 1, f.count(real))
	assert.Contains(t, []string(note.Mentions), userIDByURI(t, repo, real))
}

// Only a DNS / connect / TLS failure or timeout of a request to the mentioned
// host itself marks that host unreachable. A failure at a redirect target, or
// a non-network *url.Error (redirect limit, unsupported scheme), only marks
// the URI.
func TestIngestNote_HostMarkedUnreachableOnlyForItsOwnNetworkFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      func(uri string) error
		wantSkip bool
	}{
		{"connect failure", dialError, true},
		{"dns failure", func(u string) error {
			return &url.Error{Op: "Get", URL: u, Err: &net.DNSError{Err: "no such host", Name: "x"}}
		}, true},
		{"tls failure", func(u string) error {
			return &url.Error{Op: "Get", URL: u, Err: x509.UnknownAuthorityError{}}
		}, true},
		{"tls hostname mismatch", func(u string) error {
			return &url.Error{Op: "Get", URL: u, Err: x509.HostnameError{Host: "target.example"}}
		}, true},
		{"tls invalid certificate", func(u string) error {
			return &url.Error{Op: "Get", URL: u, Err: x509.CertificateInvalidError{Reason: x509.Expired}}
		}, true},
		{"tls verification failure", func(u string) error {
			return &url.Error{Op: "Get", URL: u, Err: &tls.CertificateVerificationError{Err: errors.New("bad chain")}}
		}, true},
		{"tls record failure", func(u string) error {
			return &url.Error{Op: "Get", URL: u, Err: tls.RecordHeaderError{Msg: "bad"}}
		}, true},
		{"timeout", func(u string) error {
			return &url.Error{Op: "Get", URL: u, Err: context.DeadlineExceeded}
		}, true},
		{"failure at redirect target", func(string) error {
			return dialError("https://elsewhere.example/landing")
		}, false},
		{"redirect limit", func(u string) error {
			return &url.Error{Op: "Get", URL: u, Err: errors.New("stopped after 10 redirects")}
		}, false},
		{"not a url error", func(string) error { return errors.New("boom") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, f := newMentionFetchResolver(t)
			const a, b = "https://target.example/users/a", "https://target.example/users/b"
			f.errs[a] = tc.err(a)
			f.bodies[b] = actorDoc(b, "b")
			_, err := r.IngestNote(noteBody(t, "h1", []string{a}, []string{activitypub.Public}, nil))
			require.NoError(t, err)
			require.Equal(t, 1, f.count(a))
			_, err = r.IngestNote(noteBody(t, "h2", []string{b}, []string{activitypub.Public}, nil))
			require.NoError(t, err)
			if tc.wantSkip {
				assert.Zero(t, f.count(b), "ホストごと覚える")
			} else {
				assert.Equal(t, 1, f.count(b), "ホストは覚えない")
			}
		})
	}
}

// countingPolicies is a RolePolicyProvider with a fixed mentionLimit that
// counts lookups.
type countingPolicies struct {
	limit any
	calls int
}

func (p *countingPolicies) GetUserPolicies(string) map[string]any {
	p.calls++
	return map[string]any{"mentionLimit": p.limit}
}

// The fetch limits follow the author's mentionLimit policy, like the limit
// check of the ingest: a role above the default fetches more than 20 unknown
// actors, a role below it fetches nothing for a note over it, and a note with
// no mentions or recipients never looks the policy up.
func TestIngestNote_UnknownActorFetchFollowsMentionLimitPolicy(t *testing.T) {
	newMentions := func(f *routeFetcher, prefix string, n int) []string {
		var out []string
		for i := range n {
			u := fmt.Sprintf("https://%s%d.example/users/x", prefix, i)
			f.bodies[u] = actorDoc(u, "x")
			out = append(out, u)
		}
		return out
	}
	t.Run("raised limit fetches more than the default", func(t *testing.T) {
		r, _, f := newMentionFetchResolver(t)
		p := &countingPolicies{limit: float64(30)}
		r.SetRolePolicyProvider(p)
		mentions := newMentions(f, "up", corenote.DefaultMentionLimit+5)
		note, err := r.IngestNote(noteBody(t, "p1", mentions, []string{activitypub.Public}, nil))
		require.NoError(t, err)
		assert.Equal(t, corenote.DefaultMentionLimit+5, f.total())
		assert.Len(t, []string(note.Mentions), corenote.DefaultMentionLimit+5)
	})
	t.Run("raised limit caps attempts", func(t *testing.T) {
		r, _, f := newMentionFetchResolver(t)
		r.SetRolePolicyProvider(&countingPolicies{limit: float64(22)})
		var cc []string
		for i := range 30 {
			cc = append(cc, fmt.Sprintf("https://cap%d.example/users/x", i))
		}
		_, err := r.IngestNote(noteBody(t, "p2", nil, nil, cc))
		require.NoError(t, err)
		assert.Equal(t, 22, f.total())
	})
	t.Run("lowered limit fetches nothing for a note over it", func(t *testing.T) {
		r, _, f := newMentionFetchResolver(t)
		r.SetRolePolicyProvider(&countingPolicies{limit: float64(2)})
		mentions := newMentions(f, "low", 3)
		_, err := r.IngestNote(noteBody(t, "p3", mentions, []string{activitypub.Public}, nil))
		require.ErrorIs(t, err, corenote.ErrContainsTooManyMentions)
		assert.Zero(t, f.total())
	})
	t.Run("no mentions never looks the policy up", func(t *testing.T) {
		r, _, _ := newMentionFetchResolver(t)
		p := &countingPolicies{limit: float64(30)}
		r.SetRolePolicyProvider(p)
		_, err := r.IngestNote(noteBody(t, "p4", nil, []string{activitypub.Public}, nil))
		require.NoError(t, err)
		assert.Zero(t, p.calls)
	})
	t.Run("policy looked up once", func(t *testing.T) {
		r, _, f := newMentionFetchResolver(t)
		p := &countingPolicies{limit: float64(30)}
		r.SetRolePolicyProvider(p)
		mentions := newMentions(f, "once", 3)
		_, err := r.IngestNote(noteBody(t, "p5", mentions, []string{activitypub.Public}, nil))
		require.NoError(t, err)
		// 取得の上限と取り込みの上限判定で同じ値を使い、1 回だけ引く。
		assert.Equal(t, 1, p.calls)
	})
}
