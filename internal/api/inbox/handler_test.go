package inbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	emiddleware "github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubFetcher returns canned actor JSON.
type stubFetcher struct {
	body []byte
}

func (s *stubFetcher) FetchObject(_ string) ([]byte, error) {
	return s.body, nil
}

func actorBody(pubKeyPEM string) string {
	return `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"name": "Alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"publicKey": {
			"id": "https://remote.example/users/alice#main-key",
			"owner": "https://remote.example/users/alice",
			"publicKeyPem": "` + escapeJSON(pubKeyPEM) + `"
		}
	}`
}

func escapeJSON(s string) string {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r == '\n' {
			out = append(out, '\\', 'n')
		} else {
			out = append(out, byte(r))
		}
	}
	return string(out)
}

func newHandler(t *testing.T, pubKeyPEM string) (*Handler, *testutil.MockUserRepository, *testutil.MockFollowingRepository) {
	t.Helper()
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	idGen, _ := id.NewGenerator("aidx")
	urls := activitypub.NewURLBuilder("https://example.com")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(actorBody(pubKeyPEM))}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	processor := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)
	return NewHandler(resolver, processor), repo, followingRepo
}

func newPost(t *testing.T, body []byte) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "https://example.com/inbox", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

func TestInbox_HappyPathFollow(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	h, repo, followingRepo := newHandler(t, pub)

	// 受信側 bob を登録
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)

	c, rec := newPost(t, body)
	// Echoはリクエストボディをコピーするのでサイン用にreqに直接アクセス
	req := c.Request()
	digest := activitypub.SHA256Digest(body)
	require.NoError(t, activitypub.SignRequest(req, key, digest, []string{"(request-target)", "date", "host", "digest"}))
	// Hostヘッダはサーバ側で復元する必要がある
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Len(t, followingRepo.Followings, 1)
}

func TestInbox_UnsupportedActivity(t *testing.T) {
	priv, pub, _ := activitypub.GenerateRSAKeypair()
	key, _ := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	h, _, _ := newHandler(t, pub)

	body := []byte(`{"type":"Like","actor":"https://remote.example/users/alice"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(body), []string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	// Unsupportedでも 202 Accepted
	assert.Equal(t, http.StatusAccepted, rec.Code)
}

func TestInbox_BadJSON(t *testing.T) {
	priv, pub, _ := activitypub.GenerateRSAKeypair()
	key, _ := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	h, _, _ := newHandler(t, pub)

	body := []byte(`{not json`)
	c, rec := newPost(t, body)
	req := c.Request()
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(body), []string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestInbox_MissingSignature(t *testing.T) {
	_, pub, _ := activitypub.GenerateRSAKeypair()
	h, _, _ := newHandler(t, pub)

	body := []byte(`{"type":"Follow","actor":"x"}`)
	c, rec := newPost(t, body)
	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestInbox_InvalidSignature(t *testing.T) {
	_, pub, _ := activitypub.GenerateRSAKeypair()
	priv2, _, _ := activitypub.GenerateRSAKeypair()
	wrongKey, _ := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv2)

	h, _, _ := newHandler(t, pub)

	body := []byte(`{"type":"Follow","actor":"x"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	require.NoError(t, activitypub.SignRequest(req, wrongKey, activitypub.SHA256Digest(body), []string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestInbox_HostHeaderEmpty(t *testing.T) {
	priv, pub, _ := activitypub.GenerateRSAKeypair()
	key, _ := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	h, _, _ := newHandler(t, pub)

	body := []byte(`{"type":"Like","actor":"https://remote.example/users/alice"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(body), []string{"(request-target)", "date", "host", "digest"}))
	// Host を消す → handler 側で req.Host から復元される
	req.Header.Del("Host")
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusAccepted, rec.Code)
}

// readErrReader is an io.Reader that returns an error.
type readErrReader struct{}

func (readErrReader) Read(_ []byte) (int, error) { return 0, assertErrSentinel }

var assertErrSentinel = newSentinelErr("read fail")

type sentinelErr struct{ msg string }

func (e *sentinelErr) Error() string { return e.msg }
func newSentinelErr(s string) error  { return &sentinelErr{msg: s} }

// errorFetcher always errors so the resolver fails.
type errorFetcher struct{}

func (errorFetcher) FetchObject(_ string) ([]byte, error) {
	return nil, assertErrSentinel
}

func TestInbox_ResolverError(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	idGen, _ := id.NewGenerator("aidx")
	urls := activitypub.NewURLBuilder("https://example.com")
	resolver := federation.NewResolver(repo, noteRepo, urls, errorFetcher{}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	processor := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)
	h := NewHandler(resolver, processor)

	priv, _, _ := activitypub.GenerateRSAKeypair()
	key, _ := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)

	body := []byte(`{"type":"Follow","actor":"x"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(body), []string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestInbox_PublicKeyMissing(t *testing.T) {
	// Pre-populate user with a URI so resolver hits cached path. The fetcher
	// errors out → refreshPublicKey silently fails → keys map stays empty.
	// PublicKeyForActor then returns an error inside verifySignature.
	repo := testutil.NewMockUserRepository()
	uri := "https://remote.example/users/alice"
	repo.Users["alice"] = &model.User{ID: "alice", Username: "alice", URI: &uri}

	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	idGen, _ := id.NewGenerator("aidx")
	urls := activitypub.NewURLBuilder("https://example.com")
	resolver := federation.NewResolver(repo, noteRepo, urls, errorFetcher{}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	processor := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)
	h := NewHandler(resolver, processor)

	priv, _, _ := activitypub.GenerateRSAKeypair()
	key, _ := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(body), []string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// stubBlocker is a HostBlockChecker that returns true for matching host names.
// disallowed simulates federation mode "specified" / "none". 既定 (zero-value)
// では allow-all。
type stubBlocker struct {
	blocked     map[string]bool
	disallowed  map[string]bool
	fedDisabled bool
}

func (s *stubBlocker) IsBlocked(host string) bool { return s.blocked[host] }
func (s *stubBlocker) IsAllowed(host string) bool { return !s.disallowed[host] }
func (s *stubBlocker) FederationDisabled() bool   { return s.fedDisabled }

func TestInbox_BlockedHost(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	h, repo, _ := newHandler(t, pub)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}
	h.SetHostBlockChecker(&stubBlocker{blocked: map[string]bool{"remote.example": true}})

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	digest := activitypub.SHA256Digest(body)
	require.NoError(t, activitypub.SignRequest(req, key, digest, []string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// federation: specified で allowlist 外 host からの inbound は 403 で reject
// される (#536)。blocked と独立した経路。
// actorBody は固定で host=remote.example を返すので disallowed もそれに合わせる。
func TestInbox_DisallowedHost(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	h, repo, _ := newHandler(t, pub)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}
	// blocked は空のまま、disallowed (= specified モードで allowlist 外) を
	// シミュレート。これだけで 403 reject されること = allowlist enforcement
	// が deliver path と独立に inbox path にも入ったこと。
	h.SetHostBlockChecker(&stubBlocker{disallowed: map[string]bool{"remote.example": true}})

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	digest := activitypub.SHA256Digest(body)
	require.NoError(t, activitypub.SignRequest(req, key, digest, []string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"federation policy excluded host must be rejected with 403")
}

// stubInstanceTracker captures MarkRequestReceived calls.
type stubInstanceTracker struct {
	hosts []string
}

func (s *stubInstanceTracker) MarkRequestReceived(host string) error {
	s.hosts = append(s.hosts, host)
	return nil
}

func TestInbox_TouchesInstance(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	h, repo, _ := newHandler(t, pub)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}
	tracker := &stubInstanceTracker{}
	h.SetInstanceTracker(tracker)

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	digest := activitypub.SHA256Digest(body)
	require.NoError(t, activitypub.SignRequest(req, key, digest, []string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusAccepted, rec.Code)
	require.Len(t, tracker.hosts, 1)
	assert.Equal(t, "remote.example", tracker.hosts[0])
}

func TestInbox_TouchesInstanceTrackerNoOpWithoutTracker(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	h, repo, _ := newHandler(t, pub)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	digest := activitypub.SHA256Digest(body)
	require.NoError(t, activitypub.SignRequest(req, key, digest, []string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusAccepted, rec.Code)
}

// stubChartHook captures inbox chart events.
type stubChartHook struct {
	hosts []string
}

func (s *stubChartHook) OnInboxReceived(host string) {
	s.hosts = append(s.hosts, host)
}

func TestInbox_FiresChartHook(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	h, repo, _ := newHandler(t, pub)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}
	hook := &stubChartHook{}
	h.SetChartHook(hook)

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	digest := activitypub.SHA256Digest(body)
	require.NoError(t, activitypub.SignRequest(req, key, digest, []string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusAccepted, rec.Code)
	require.Len(t, hook.hosts, 1)
	assert.Equal(t, "remote.example", hook.hosts[0])
}

func TestInbox_BlockedHostDoesNotApplyWithoutChecker(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	h, repo, _ := newHandler(t, pub)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	digest := activitypub.SHA256Digest(body)
	require.NoError(t, activitypub.SignRequest(req, key, digest, []string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.NotEqual(t, http.StatusForbidden, rec.Code)
}

func TestInbox_BodyReadError(t *testing.T) {
	_, pub, _ := activitypub.GenerateRSAKeypair()
	h, _, _ := newHandler(t, pub)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "https://example.com/inbox", readErrReader{})
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// recordingEnqueuer captures EnqueueInbox calls so async-mode tests can
// assert that the handler dispatches the activity to the queue instead of
// running Process synchronously (#534).
type recordingEnqueuer struct {
	calls []queue.InboxPayload
	err   error
}

func (r *recordingEnqueuer) EnqueueInbox(_ context.Context, p queue.InboxPayload) error {
	if r.err != nil {
		return r.err
	}
	r.calls = append(r.calls, p)
	return nil
}

// SetEnqueuer 後は Inbox は Process / verifySignature を呼ばずに
// EnqueueInbox に委譲する (#565)。followingRepo に書き込みが起きないこと、
// enqueuer が body と signature 関連 header を受け取ること、HTTP は 202
// Accepted を返すことを確認する。worker 側の verify に必要な Method /
// Path / Headers (Signature / Date / Host / Digest) が payload に含まれて
// いることも検証。
func TestInbox_AsyncMode_DispatchesToQueue(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	h, repo, followingRepo := newHandler(t, pub)
	enq := &recordingEnqueuer{}
	h.SetEnqueuer(enq)

	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	digest := activitypub.SHA256Digest(body)
	require.NoError(t, activitypub.SignRequest(req, key, digest, []string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusAccepted, rec.Code)
	require.Len(t, enq.calls, 1, "activity should be enqueued, not processed synchronously")
	got := enq.calls[0]
	assert.JSONEq(t, string(body), string(got.Body))
	assert.Equal(t, http.MethodPost, got.Method)
	assert.Equal(t, "/inbox", got.Path)
	assert.NotEmpty(t, got.Headers["Signature"], "Signature header must be captured")
	assert.NotEmpty(t, got.Headers["Date"], "Date header must be captured")
	assert.NotEmpty(t, got.Headers["Digest"], "Digest header must be captured")
	assert.Empty(t, followingRepo.Followings, "Process should NOT have been invoked synchronously")
}

// 非同期経路は query 込みの RequestURI を積む。worker と同じく placeholder
// host で組み直した request で、送信側の署名 (path + query) が検証できること
// (本家 7c9c38c04a)。URL.Path だけを積むと query が落ちて署名が合わない。
func TestInbox_AsyncMode_PathKeepsQuery(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	h, _, _ := newHandler(t, pub)
	enq := &recordingEnqueuer{}
	h.SetEnqueuer(enq)

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "https://example.com/inbox?x=1", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(body), []string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Len(t, enq.calls, 1)
	got := enq.calls[0]
	assert.Equal(t, "/inbox?x=1", got.Path)

	rebuilt, err := http.NewRequest(got.Method, "http://placeholder"+got.Path, bytes.NewReader(got.Body))
	require.NoError(t, err)
	for k, v := range got.Headers {
		rebuilt.Header.Set(k, v)
	}
	require.NoError(t, activitypub.VerifyRequest(rebuilt, pub))
}

// signature ヘッダ無しは即 401。RSA verify は走らないので fast path 成立。
func TestInbox_AsyncMode_RejectsMissingSignatureHeader(t *testing.T) {
	_, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	h, _, _ := newHandler(t, pub)
	enq := &recordingEnqueuer{}
	h.SetEnqueuer(enq)

	body := []byte(`{"type":"Follow"}`)
	c, rec := newPost(t, body)
	c.Request().Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, enq.calls, "no enqueue when signature header is missing")
}

// queue 障害時は 500 を返して上流に retry を促す (202 で握り潰すと
// activity が silent drop される)。
func TestInbox_AsyncMode_EnqueueFailureReturns500(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	h, repo, _ := newHandler(t, pub)
	h.SetEnqueuer(&recordingEnqueuer{err: errors.New("redis down")})

	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	digest := activitypub.SHA256Digest(body)
	require.NoError(t, activitypub.SignRequest(req, key, digest, []string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// --- #1949 inbox admission (digest body integrity + host binding) ---

func TestInbox_TamperedBodyRejected(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)
	h, _, _ := newHandler(t, pub)

	original := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	tampered := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/eve"}`)

	c, rec := newPost(t, tampered)
	req := c.Request()
	// Digest は original の hash で署名するが、reader には tampered を流す。
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(original), []string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestInbox_DigestNotSignedRejected(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)
	h, _, _ := newHandler(t, pub)

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	// Digest header は正しく付くが、署名対象ヘッダに digest を含めない。
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(body), []string{"(request-target)", "date", "host"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestInbox_HostMismatchRejected(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)
	h, _, _ := newHandler(t, pub)
	h.SetExpectedHost("example.com")

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(body), []string{"(request-target)", "date", "host", "digest"}))
	req.Host = "evil.example"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestInbox_HostSignedAndMatchAccepted(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)
	h, repo, followingRepo := newHandler(t, pub)
	h.SetExpectedHost("example.com")

	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(body), []string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Len(t, followingRepo.Followings, 1)
}

// #2069 (upstream #17558): bodyHasActor は actor を持つ object のみ true。
func TestBodyHasActor(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"string actor", `{"type":"Create","actor":"https://r/u/a"}`, true},
		{"embedded object actor", `{"type":"Create","actor":{"id":"https://r/u/a"}}`, true},
		{"no actor key", `{"type":"Create","object":{}}`, false},
		{"null actor", `{"type":"Create","actor":null}`, false},
		{"not an object (array)", `[{"actor":"x"}]`, false},
		{"not json", `garbage`, false},
		{"empty actor string is present", `{"actor":""}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, bodyHasActor([]byte(tc.body)))
		})
	}
}

// #1958: inbox route の 64KB body limit。upstream ActivityPubServerService の
// bodyLimit: 1024*64 相当。署名検証前に巨大 body を弾く DoS 対策で、超過時は
// upstream fastify と同じ 413 を返す。
func TestInbox_BodyLimit(t *testing.T) {
	_, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	h, _, _ := newHandler(t, pub)

	e := echo.New()
	// router.go と同じ wiring (BodyLimit("64KiB")=65536 + handler)。
	e.POST("/inbox", h.Inbox, emiddleware.BodyLimit("64KiB"))

	serve := func(body []byte, chunked bool) int {
		req := httptest.NewRequest(http.MethodPost, "https://example.com/inbox", bytes.NewReader(body))
		req.Header.Set("Host", "example.com")
		if chunked {
			// Content-Length を消して limitedReader 経路 (read 中の 413) を通す。
			req.ContentLength = -1
		}
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}

	big := bytes.Repeat([]byte("a"), 64*1024+1)   // 64KB 超
	atLimit := bytes.Repeat([]byte("a"), 64*1024) // ちょうど 64KB (許容)
	small := []byte(`{"actor":"https://remote.example/users/x"}`)

	// Content-Length > 64KB → BodyLimit が即時 413 (handler 未到達)。
	assert.Equal(t, http.StatusRequestEntityTooLarge, serve(big, false), "Content-Length 超過は 413")
	// chunked (Content-Length 不明) で 64KB 超 → handler が limitedReader の 413 を伝播。
	assert.Equal(t, http.StatusRequestEntityTooLarge, serve(big, true), "chunked 超過も 413")
	// ちょうど 64KB は通過 (413 にならない、署名なしで admission が 401 になるが 413 ではない)。
	assert.NotEqual(t, http.StatusRequestEntityTooLarge, serve(atLimit, false), "64KB ちょうどは許容")
	// 小さい body は 413 にならない。
	assert.NotEqual(t, http.StatusRequestEntityTooLarge, serve(small, false))
}

// #1959: federation mode が "none" のとき inbox は最前段で 403 を返す (upstream
// ActivityPubServerService.inbox の federation==='none' gate)。署名の有無に依らず、
// body を読む前に gate する (per-actor IsAllowed より前)。
func TestInbox_FederationDisabled(t *testing.T) {
	_, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	h, _, _ := newHandler(t, pub)
	h.SetHostBlockChecker(&stubBlocker{fedDisabled: true})

	// 署名なしの素の body でも、federation gate が先に効いて 403 (401 ではない)。
	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	c, rec := newPost(t, body)
	c.Request().Host = "example.com"
	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusForbidden, rec.Code, "federation=none は最前段で 403")
}

// federation 有効時 (fedDisabled=false) は gate を素通りし通常の admission に進む
// (gate が誤って常時 403 にしないことの確認)。
func TestInbox_FederationEnabled_NotGated(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)
	h, repo, _ := newHandler(t, pub)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}
	h.SetHostBlockChecker(&stubBlocker{fedDisabled: false})

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)
	c, rec := newPost(t, body)
	req := c.Request()
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(body), []string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"
	require.NoError(t, h.Inbox(c))
	assert.NotEqual(t, http.StatusForbidden, rec.Code, "federation 有効時は gate を素通り")
}

// 署名済みリクエストの再投函が時間で止まること。**Date は署名対象なので
// 差し替えれば署名が壊れる** — 古い Date のまま署名して、捕まえた配送をそのまま
// 投げ直す形を再現する。
//
// 単体の検査は activitypub 側にあるが、ここは**配線**を固定する。
// VerifyInboxAdmission の引数は date も digest も string なので、順序を取り違えても
// コンパイルは通り、digest 側が壊れないほうの取り違えは他のテストで気づけない。
func TestInbox_RejectsStaleSignedDate(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	h, repo, followingRepo := newHandler(t, pub)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)

	c, rec := newPost(t, body)
	req := c.Request()
	// SignRequest は Date が既にあれば触らないので、古い値のまま署名される。
	req.Header.Set("Date", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(body),
		[]string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Empty(t, followingRepo.Followings, "弾いた活動が処理されてはいけない")
}

// **未署名の `X-Date` で clockSkew を迂回できないこと (#3037)。**
//
// `X-Date` は普通どの peer も署名しないので、捕まえたリクエストに新しい値を
// 足すだけで検査を通せた — 署名は元の `Date` に対して作られているのでそのまま
// 通り、`Digest` も body も変わらないので他の検査も全部通る。つまり一度
// 盗聴できた配送を**永久に再投函**できた。
func TestInbox_UnsignedXDateDoesNotBypassSkew(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	h, repo, followingRepo := newHandler(t, pub)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)

	c, rec := newPost(t, body)
	req := c.Request()
	req.Header.Set("Date", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(body),
		[]string{"(request-target)", "date", "host", "digest"}))
	req.Host = "example.com"
	// 署名した**後**に足す = 署名対象ではない。
	req.Header.Set("X-Date", time.Now().UTC().Format(http.TimeFormat))

	require.NoError(t, h.Inbox(c))
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "未署名の X-Date で再投函が通っている")
	assert.Empty(t, followingRepo.Followings, "弾いた活動が処理されてはいけない")
}

// **署名済みの `X-Date` は従来どおり優先される。** これが無いと
// 「X-Date を一律で無視する」実装でも上のテストが通る。署名しているなら
// 値を差し替えられない (署名が壊れる) ので、優先しても窓は閉じたまま。
func TestInbox_SignedXDateIsStillHonoured(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	h, repo, _ := newHandler(t, pub)
	bobURI := "https://example.com/users/bob"
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", URI: &bobURI}

	body := []byte(`{"type":"Follow","actor":"https://remote.example/users/alice","object":"https://example.com/users/bob"}`)

	c, rec := newPost(t, body)
	req := c.Request()
	// Date は古いまま、X-Date に現在時刻を入れて**両方署名する**。
	req.Header.Set("Date", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
	req.Header.Set("X-Date", time.Now().UTC().Format(http.TimeFormat))
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(body),
		[]string{"(request-target)", "date", "x-date", "host", "digest"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	assert.NotEqual(t, http.StatusUnauthorized, rec.Code, "署名済みの X-Date を無視している")
}

// **`X-Date` を署名する peer の配送が worker 経路でも通ること (#3037 レビュー)。**
//
// `captureSignatureHeaders` が `X-Date` を運ばないと、worker 側の
// `buildSigningString` が `missing required header "x-date"` で落ち、**署名が
// 正しいのに drop される**。handler の同期経路だけを見るテストでは気付けない。
func TestCaptureSignatureHeaders_CarriesXDate(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/inbox", nil)
	req.Header.Set("Signature", "sig")
	req.Header.Set("Date", "d")
	req.Header.Set("X-Date", "x")
	req.Host = "example.com"

	got := captureSignatureHeaders(req)

	assert.Equal(t, "x", got["X-Date"], "署名対象の X-Date が worker へ渡らない")
	assert.Equal(t, "d", got["Date"])
	assert.Equal(t, "example.com", got["Host"])
}

// **署名の headers= に挙がったヘッダーは worker まで運ぶ (#3498)。**
//
// Mastodon はフォロワー限定の投稿の配送に `Collection-Synchronization` を付けて
// 署名する。決め打ちの一覧だけを運ぶと、worker が署名を組み立て直せず
// `missing required header "collection-synchronization"` で捨てていた。
// 受付は通るので、worker と同じ形で組み直して検証するところまで見る。
func TestInbox_AsyncMode_CarriesEverySignedHeader(t *testing.T) {
	priv, pub, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/alice#main-key", priv)
	require.NoError(t, err)

	h, _, _ := newHandler(t, pub)
	enq := &recordingEnqueuer{}
	h.SetEnqueuer(enq)

	body := []byte(`{"type":"Create","actor":"https://remote.example/users/alice","object":{"type":"Note","id":"https://remote.example/notes/1","attributedTo":"https://remote.example/users/alice","to":["https://remote.example/users/alice/followers"],"content":"x"}}`)
	c, rec := newPost(t, body)
	req := c.Request()
	req.Header.Set("Content-Type", "application/activity+json")
	req.Header.Set("Collection-Synchronization",
		`collectionId="https://remote.example/users/alice/followers", url="https://remote.example/users/alice/followers_synchronization", digest="abc"`)
	require.NoError(t, activitypub.SignRequest(req, key, activitypub.SHA256Digest(body),
		[]string{"(request-target)", "host", "date", "digest", "content-type", "collection-synchronization"}))
	req.Host = "example.com"

	require.NoError(t, h.Inbox(c))
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Len(t, enq.calls, 1)
	got := enq.calls[0]
	assert.NotEmpty(t, got.Headers["Collection-Synchronization"], "署名対象の Collection-Synchronization が worker へ渡らない")

	rebuilt, err := http.NewRequest(got.Method, "http://placeholder"+got.Path, bytes.NewReader(got.Body))
	require.NoError(t, err)
	for k, v := range got.Headers {
		rebuilt.Header.Set(k, v)
	}
	require.NoError(t, activitypub.VerifyRequest(rebuilt, pub), "worker と同じ形で組み直した要求の署名が通らない")
}

// 署名の headers= は相手の申告なので、運ぶ数と大きさに上限を付ける (#3498)。
// 疑似ヘッダー ((request-target) など) と、署名に挙がっていないヘッダーは運ばない。
func TestCaptureSignatureHeaders_BoundsExtraHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/inbox", nil)
	req.Host = "example.com"
	names := []string{"(request-target)", "(created)", "date"}
	for i := 0; i < 40; i++ {
		name := fmt.Sprintf("x-extra-%02d", i)
		names = append(names, name)
		req.Header.Set(name, "v")
	}
	req.Header.Set("X-Not-Signed", "v")
	req.Header.Set("Date", "d")
	req.Header.Set("Signature", `keyId="https://remote.example/users/alice#main-key",algorithm="rsa-sha256",headers="`+
		strings.Join(names, " ")+`",signature="c2ln"`)

	got := captureSignatureHeaders(req)

	extra := 0
	for k := range got {
		if strings.HasPrefix(k, "X-Extra-") {
			extra++
		}
	}
	assert.Equal(t, maxSignedExtraHeaders, extra, "署名に挙がった追加のヘッダーの数が上限で止まっていない")
	assert.Equal(t, "v", got["X-Extra-00"], "先頭の追加のヘッダーが運ばれていない")
	assert.NotContains(t, got, "X-Not-Signed", "署名に挙がっていないヘッダーまで運んでいる")
	assert.NotContains(t, got, "(Request-Target)")
	assert.NotContains(t, got, "(Created)")

	// 大きさの上限: 1 つで上限を超える値は運ばない。
	big := httptest.NewRequest(http.MethodPost, "/inbox", nil)
	big.Header.Set("X-Huge", strings.Repeat("a", maxSignedExtraHeaderBytes+1))
	big.Header.Set("Signature", `keyId="k",headers="date x-huge",signature="c2ln"`)
	assert.NotContains(t, captureSignatureHeaders(big), "X-Huge", "大きさの上限を超える値を運んでいる")
}
