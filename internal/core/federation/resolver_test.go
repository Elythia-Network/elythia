package federation_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/core/federation"
	corenote "github.com/shiroha-a/mk/internal/core/note"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"sort"
)

// stubFetcher returns canned bytes/error for FetchObject.
type stubFetcher struct {
	body []byte
	err  error
}

func (s *stubFetcher) FetchObject(_ string) ([]byte, error) {
	return s.body, s.err
}

// countingFetcher counts FetchObject calls so tests can assert that a refetch
// loop is not amplifying (#2662).
type countingFetcher struct {
	body  []byte
	err   error
	calls int
}

func (c *countingFetcher) FetchObject(_ string) ([]byte, error) {
	c.calls++
	return c.body, c.err
}

// blockingFetcher counts FetchObject calls and blocks until gate is closed.
// 並行呼び出しが singleflight に集約される瞬間を観測するためのテスト用 fetcher
// (#300 3-7)。
type blockingFetcher struct {
	body  []byte
	gate  chan struct{}
	calls atomic.Int64
}

func (b *blockingFetcher) FetchObject(_ string) ([]byte, error) {
	b.calls.Add(1)
	<-b.gate
	return b.body, nil
}

const sampleActor = `{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": "https://remote.example/users/alice",
	"type": "Person",
	"preferredUsername": "alice",
	"name": "Alice",
	"inbox": "https://remote.example/users/alice/inbox",
	"endpoints": {"sharedInbox": "https://remote.example/inbox"},
	"publicKey": {
		"id": "https://remote.example/users/alice#main-key",
		"owner": "https://remote.example/users/alice",
		"publicKeyPem": "-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----"
	}
}`

func newResolver(t *testing.T, body string, err error) (*federation.Resolver, *testutil.MockUserRepository) {
	t.Helper()
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	return federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body), err: err}, idGen), repo
}

func TestResolveActor_NewUser(t *testing.T) {
	r, repo := newResolver(t, sampleActor, nil)
	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.Equal(t, "alice", user.Username)
	assert.Len(t, repo.Users, 1)

	pem, err := r.PublicKeyForActor(user.ID)
	require.NoError(t, err)
	assert.Contains(t, pem, "FAKE")
}

// finalURLStubFetcher は redirect 後の最終 URL を制御できる test fetcher。
// resolver の unexported finalURLFetcher を構造的に満たす (#1820)。
type finalURLStubFetcher struct {
	body     []byte
	finalURL string
}

func (f *finalURLStubFetcher) FetchObject(_ string) ([]byte, error) {
	return f.body, nil
}

func (f *finalURLStubFetcher) FetchObjectWithFinalURL(_ string) ([]byte, string, error) {
	return f.body, f.finalURL, nil
}

func newResolverWithFetcher(t *testing.T, f federation.HTTPFetcher) (*federation.Resolver, *testutil.MockUserRepository) {
	t.Helper()
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	return federation.NewResolver(repo, noteRepo, urls, f, idGen), repo
}

// #1820: evil.example が victim(remote.example) になりすました actor を返しても、
// 取得元 host (finalURL) と actor.id host が一致しないので拒否する (object-spoofing)。
func TestResolveActor_HostSpoofRejected(t *testing.T) {
	// body の id は remote.example だが、実際に body を返したのは evil.example。
	f := &finalURLStubFetcher{body: []byte(sampleActor), finalURL: "https://evil.example/users/alice"}
	r, repo := newResolverWithFetcher(t, f)
	_, err := r.ResolveActor("https://evil.example/users/alice")
	require.Error(t, err, "id host != fetch host の actor は拒否されるべき")
	assert.Empty(t, repo.Users, "なりすまし actor を DB に作ってはいけない")
}

// #1820: 取得元 host と actor.id host が一致すれば従来どおり解決される。
func TestResolveActor_HostMatchAccepted(t *testing.T) {
	f := &finalURLStubFetcher{body: []byte(sampleActor), finalURL: "https://remote.example/users/alice"}
	r, repo := newResolverWithFetcher(t, f)
	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.Equal(t, "alice", user.Username)
	assert.Len(t, repo.Users, 1)
}

// #692: 新規 fetch で `_misskey_canChat` を chatScope に翻訳する。
// 欠落 / true は "everyone"、false は "none" にマップ。
func TestResolveActor_NewUserCanChatTranslation(t *testing.T) {
	cases := []struct {
		name     string
		bodyFlag string
		want     string
	}{
		{"missing", "", "everyone"},
		{"true", `, "_misskey_canChat": true`, "everyone"},
		{"false", `, "_misskey_canChat": false`, "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
				"id": "https://remote.example/users/alice",
				"type": "Person",
				"preferredUsername": "alice",
				"inbox": "https://remote.example/users/alice/inbox",
				"publicKey": {"publicKeyPem": "FAKE"}` + tc.bodyFlag + `
			}`
			r, _ := newResolver(t, body, nil)
			user, err := r.ResolveActor("https://remote.example/users/alice")
			require.NoError(t, err)
			assert.Equal(t, tc.want, user.ChatScope)
		})
	}
}

func TestResolveActor_NewUserIngestsIconBanner(t *testing.T) {
	// actor.icon / actor.image があれば avatarUrl / bannerUrl に取り込む。
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"icon": {"type": "Image", "url": "https://remote.example/avatar.png"},
		"image": {"type": "Image", "url": "https://remote.example/banner.png"},
		"publicKey": {"publicKeyPem": "FAKE"}
	}`
	r, _ := newResolver(t, body, nil)
	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	require.NotNil(t, user.AvatarURL)
	assert.Equal(t, "https://remote.example/avatar.png", *user.AvatarURL)
	require.NotNil(t, user.BannerURL)
	assert.Equal(t, "https://remote.example/banner.png", *user.BannerURL)
}

func TestResolveActor_NewUserIngestsManuallyApproves(t *testing.T) {
	// actor.manuallyApprovesFollowers が true のリモートアカウントは
	// IsLocked=true で保存する (follow 時に FollowRequest 経路に入るように
	// するため)。
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/locked",
		"type": "Person",
		"preferredUsername": "locked",
		"inbox": "https://remote.example/users/locked/inbox",
		"manuallyApprovesFollowers": true,
		"publicKey": {"publicKeyPem": "FAKE"}
	}`
	r, _ := newResolver(t, body, nil)
	user, err := r.ResolveActor("https://remote.example/users/locked")
	require.NoError(t, err)
	assert.True(t, user.IsLocked, "IsLocked should be true when actor.manuallyApprovesFollowers is true")
}

func TestResolveActor_NewUserWithoutManuallyApproves(t *testing.T) {
	// manuallyApprovesFollowers 省略時は IsLocked=false。
	r, _ := newResolver(t, sampleActor, nil)
	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.False(t, user.IsLocked)
}

func TestResolveActor_NewUserWithoutIconBanner(t *testing.T) {
	// icon / image を持たない actor は avatarUrl / bannerUrl を nil のまま。
	r, _ := newResolver(t, sampleActor, nil)
	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.Nil(t, user.AvatarURL)
	assert.Nil(t, user.BannerURL)
}

// --- #1022: remote actor summary → UserProfile.description ---

// AP `summary` (= リモートユーザの bio) を UserProfile.description に取り込む。
// profile 行も同時に作成される (= 旧実装は user 行のみ作成していた regression)。
// Mastodon 系の `<p>...</p>` ラップは mfm.FromHTML で MFM に変換される
// (生 HTML を保存すると frontend MFM render が escape してリテラル表示する
// drop-in regression を起こす、#1140)。
func TestResolveActor_NewUserIngestsDescription(t *testing.T) {
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"summary": "<p>Hello from remote!</p>",
		"publicKey": {"publicKeyPem": "FAKE"}
	}`
	r, repo := newResolver(t, body, nil)
	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	profile, err := repo.FindProfileByUserID(user.ID)
	require.NoError(t, err, "profile 行が作成される (back-fill 経路の前提)")
	require.NotNil(t, profile.Description)
	assert.Equal(t, "Hello from remote!", *profile.Description,
		"<p> wrap は mfm.FromHTML で MFM 段落区切りに変換され TrimSpace で消える")
}

// TestResolveActor_MastodonStyleSummaryConvertedToMFM guards #1140:
// Mastodon / Pleroma 系の actor.summary は典型的に `<p>...</p>` で wrap
// され、`<a href>` mention や `<br>` を含む。これらが mfm.FromHTML 経由で
// MFM に変換され、profile.description に純 MFM として保存されることを
// 確認 (生 HTML が残ると frontend MFM render が escape して `<p>` 等が
// リテラル表示される drop-in regression)。
func TestResolveActor_MastodonStyleSummaryConvertedToMFM(t *testing.T) {
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://mstdn.example/users/bob",
		"type": "Person",
		"preferredUsername": "bob",
		"inbox": "https://mstdn.example/users/bob/inbox",
		"summary": "<p>Hello!<br/>I&#39;m bob.</p><p>Follow me at <a href=\"https://example.com\">my site</a></p>",
		"publicKey": {"publicKeyPem": "FAKE"}
	}`
	r, repo := newResolver(t, body, nil)
	user, err := r.ResolveActor("https://mstdn.example/users/bob")
	require.NoError(t, err)
	profile, err := repo.FindProfileByUserID(user.ID)
	require.NoError(t, err)
	require.NotNil(t, profile.Description)
	// `<p>` → 段落区切り (\n\n) / `<br>` → \n / `<a>` → [text](url) MFM。
	desc := *profile.Description
	assert.NotContains(t, desc, "<p>", "<p> tag must be converted out, not stored raw")
	assert.NotContains(t, desc, "</p>", "</p> tag must be converted out")
	assert.NotContains(t, desc, "<a ", "<a> tag must be converted to MFM link syntax")
	assert.Contains(t, desc, "Hello!")
	assert.Contains(t, desc, "I'm bob.")
	assert.Contains(t, desc, "[my site](https://example.com)")
}

// `_misskey_summary` がある場合は AP `summary` より優先される (upstream
// ApPersonService と同じ logic、MFM が壊れずに保存される)。
func TestResolveActor_NewUserPrefersMisskeySummary(t *testing.T) {
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"summary": "<p>HTML summary</p>",
		"_misskey_summary": "**MFM summary**",
		"publicKey": {"publicKeyPem": "FAKE"}
	}`
	r, repo := newResolver(t, body, nil)
	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	profile, err := repo.FindProfileByUserID(user.ID)
	require.NoError(t, err)
	require.NotNil(t, profile.Description)
	assert.Equal(t, "**MFM summary**", *profile.Description)
}

// summary 無し actor も profile 行は作成され、Description は nil。
// 既存 production DB の back-fill 経路 (next refresh で追記) が成立する前提。
func TestResolveActor_NewUserNoSummary(t *testing.T) {
	r, repo := newResolver(t, sampleActor, nil)
	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	profile, err := repo.FindProfileByUserID(user.ID)
	require.NoError(t, err)
	assert.Nil(t, profile.Description, "summary 無しは Description nil")
}

// 2048 rune を超える summary は varchar(2048) 制約に合わせて truncate。
// rune 単位で切ることで UTF-8 multibyte 文字境界が壊れない。
func TestResolveActor_NewUserDescriptionTruncated(t *testing.T) {
	long := strings.Repeat("あ", 2100) // 2100 rune
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"summary": "` + long + `",
		"publicKey": {"publicKeyPem": "FAKE"}
	}`
	r, repo := newResolver(t, body, nil)
	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	profile, err := repo.FindProfileByUserID(user.ID)
	require.NoError(t, err)
	require.NotNil(t, profile.Description)
	assert.Equal(t, 2048, len([]rune(*profile.Description)))
}

func TestResolveActor_ExistingUser(t *testing.T) {
	r, repo := newResolver(t, sampleActor, nil)
	uri := "https://remote.example/users/alice"
	repo.Users["existing"] = &model.User{
		ID:       "existing",
		Username: "alice",
		URI:      &uri,
	}
	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	assert.Equal(t, "existing", user.ID)
	// 公開鍵キャッシュは refresh により埋まる
	pem, err := r.PublicKeyForActor("existing")
	require.NoError(t, err)
	assert.Contains(t, pem, "FAKE")
}

func TestResolveActor_FetchError(t *testing.T) {
	r, _ := newResolver(t, "", errors.New("network down"))
	_, err := r.ResolveActor("https://remote.example/users/x")
	assert.Error(t, err)
}

func TestResolveActor_BadJSON(t *testing.T) {
	r, _ := newResolver(t, "{not json", nil)
	_, err := r.ResolveActor("https://remote.example/users/x")
	require.ErrorIs(t, err, federation.ErrInvalidActor)
}

// 必須 field ごとに、**それ単独が欠けたときに**弾くことを確かめる。
// まとめて欠けた fixture 1 本では、どの検証が効いているのか固定できない。
// id の host は request host と揃える (揃えないと request-host binding が先に
// 弾いて ErrObjectHostMismatch になり、条件を見ていないテストになる)。
func TestResolveActor_MissingFields(t *testing.T) {
	const uri = "https://remote.example/users/x"
	tests := []struct {
		name string
		body string
	}{
		{"all missing", `{"@context":"https://www.w3.org/ns/activitystreams","id":"https://remote.example/users/x"}`},
		{"type only missing", `{"@context":"https://www.w3.org/ns/activitystreams","id":"https://remote.example/users/x","preferredUsername":"x","inbox":"https://remote.example/users/x/inbox"}`},
		{"username only missing", `{"@context":"https://www.w3.org/ns/activitystreams","id":"https://remote.example/users/x","type":"Person","inbox":"https://remote.example/users/x/inbox"}`},
		{"@context missing", `{"id":"https://remote.example/users/x","type":"Person","preferredUsername":"x"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := newResolver(t, tc.body, nil)
			_, err := r.ResolveActor(uri)
			require.ErrorIs(t, err, federation.ErrInvalidActor)
		})
	}

	// 全部揃っていれば通る (上の 4 ケースが「常に落ちる」だけの
	// テストになっていないことの裏取り)。
	r, _ := newResolver(t, `{"@context":"https://www.w3.org/ns/activitystreams","id":"https://remote.example/users/x","type":"Person","preferredUsername":"x","inbox":"https://remote.example/users/x/inbox","publicKey":{"publicKeyPem":"PEM"}}`, nil)
	_, err := r.ResolveActor(uri)
	require.NoError(t, err)
}

func TestResolveActor_BadHost(t *testing.T) {
	// scheme も host も持たない id。request host (x.example) と id host が
	// 一致しないため Strict request-host binding (#1828) が先に弾く。
	// (制御文字は `trimWHATWGURL` が落とすので、それでは弾かれない)
	body := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "://invalid",
		"preferredUsername": "alice",
		"inbox": "x"
	}`
	r, _ := newResolver(t, body, nil)
	_, err := r.ResolveActor("https://x.example/")
	require.ErrorIs(t, err, federation.ErrObjectHostMismatch)
}

// TestResolveActorAllowCrossHost_BadIDHostRejected exercises the relaxed
// (ap/show) path where the request-host binding is skipped: an actor with a
// valid type but an unparseable / hostless id is still rejected by the
// downstream hostFromURI check (#1828)。
func TestResolveActorAllowCrossHost_BadIDHostRejected(t *testing.T) {
	// type は valid なので fetchActor は通り、resolveActorOnce の hostFromURI で
	// ErrInvalidActor になる経路 (relaxed path) を踏む。
	body := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "mailto:alice@example.com",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "x"
	}`
	r, _ := newResolver(t, body, nil)
	_, err := r.ResolveActorAllowCrossHost("https://x.example/")
	require.ErrorIs(t, err, federation.ErrInvalidActor)
}

func TestResolveActor_EmptyHost(t *testing.T) {
	// mailto: parses but has no host。request host 不一致で Strict binding が弾く。
	body := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "mailto:alice@example.com",
		"preferredUsername": "alice",
		"inbox": "x"
	}`
	r, _ := newResolver(t, body, nil)
	_, err := r.ResolveActor("https://x.example/")
	require.ErrorIs(t, err, federation.ErrObjectHostMismatch)
}

// TestResolveActor_RejectsMissingContext pins the upstream invalid-response
// guard (72180409, #1828): a fetched actor without an ActivityStreams @context
// is rejected even when all other fields are valid.
func TestResolveActor_RejectsMissingContext(t *testing.T) {
	body := `{
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox"
	}`
	r, repo := newResolver(t, body, nil)
	_, err := r.ResolveActor("https://remote.example/users/alice")
	require.ErrorIs(t, err, federation.ErrInvalidActor)
	assert.Empty(t, repo.Users)
}

// TestResolveActor_RejectsNonASContext rejects a fetched actor whose @context
// is a non-ActivityStreams string.
func TestResolveActor_RejectsNonASContext(t *testing.T) {
	body := `{
		"@context": "https://example.com/other",
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox"
	}`
	r, _ := newResolver(t, body, nil)
	_, err := r.ResolveActor("https://remote.example/users/alice")
	require.ErrorIs(t, err, federation.ErrInvalidActor)
}

// TestResolveActor_AcceptsArrayContext accepts a fetched actor whose @context
// is an array that includes the ActivityStreams namespace (the common shape
// real implementations emit alongside the security vocabulary).
func TestResolveActor_AcceptsArrayContext(t *testing.T) {
	body := `{
		"@context": ["https://www.w3.org/ns/activitystreams", "https://w3id.org/security/v1"],
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"publicKey": {
			"id": "https://remote.example/users/alice#main-key",
			"owner": "https://remote.example/users/alice",
			"publicKeyPem": "-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----"
		}
	}`
	r, repo := newResolver(t, body, nil)
	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.Equal(t, "alice", user.Username)
	assert.Len(t, repo.Users, 1)
}

// TestResolveNote_RejectsMissingContext rejects a fetched note that lacks an
// ActivityStreams @context. The fetch path enforces @context while inbound
// delivery (IngestNote with an inlined object) intentionally does not.
func TestResolveNote_RejectsMissingContext(t *testing.T) {
	body := `{
		"id": "https://remote.example/notes/n1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "hello"
	}`
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	_, err := r.ResolveNote("https://remote.example/notes/n1")
	require.ErrorIs(t, err, federation.ErrInvalidNote)
}

// TestIngestNote_NoContextStillIngested confirms inbound delivery does NOT
// require @context on the inlined object (it lives on the outer activity).
func TestIngestNote_NoContextStillIngested(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	// attributedTo を先に解決できるよう actor を fetch 可能にしておく。
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	body := `{
		"id": "https://remote.example/notes/inlined",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "inlined"
	}`
	note, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	require.NotNil(t, note)
}

// TestResolveActor_RejectsFragmentURL pins the upstream b94fd5b1 guard (#1828):
// an actor URL with a `#` fragment cannot be dereferenced and is rejected
// before any fetch. (keyId fragments are stripped by ResolveActorByKeyID and
// therefore still resolve — see TestResolveActorByKeyID.)
func TestResolveActor_RejectsFragmentURL(t *testing.T) {
	r, repo := newResolver(t, sampleActor, nil)
	_, err := r.ResolveActor("https://remote.example/users/alice#frag")
	require.ErrorIs(t, err, federation.ErrResolveFragment)
	assert.Empty(t, repo.Users)
}

// TestResolveNote_RejectsFragmentURL pins the same guard for note resolution.
func TestResolveNote_RejectsFragmentURL(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleRemoteNote)}, idGen)
	_, err := r.ResolveNote("https://remote.example/notes/n1#frag")
	require.ErrorIs(t, err, federation.ErrResolveFragment)
}

// quoteChainFetcher serves an unbounded chain of notes where note N quotes
// note N+1, plus the shared author actor. Used to exercise the resolve
// recursion limit (#1828) — without the limit the chain would never terminate.
type quoteChainFetcher struct {
	actorBody string
	calls     atomic.Int64
}

func (f *quoteChainFetcher) FetchObject(uri string) ([]byte, error) {
	f.calls.Add(1)
	if strings.Contains(uri, "/users/") {
		return []byte(f.actorBody), nil
	}
	n := 0
	if i := strings.LastIndex(uri, "/"); i >= 0 {
		n, _ = strconv.Atoi(uri[i+1:])
	}
	body := fmt.Sprintf(`{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": %q,
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "n%d",
		"_misskey_quote": "https://remote.example/notes/%d"
	}`, uri, n, n+1)
	return []byte(body), nil
}

// TestResolveNote_RecursionLimit pins the upstream d592da9f guard (#1828): a
// note whose quote chain is unbounded is resolved up to resolveRecursionLimit
// (256) deep and then stops, rather than recursing forever. The top note is
// still created; the chain just truncates. We observe the bound via the fetch
// count (without the limit this test would not terminate).
func TestResolveNote_RecursionLimit(t *testing.T) {
	f := &quoteChainFetcher{actorBody: sampleActor}
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, f, idGen)

	note, err := r.ResolveNote("https://remote.example/notes/0")
	require.NoError(t, err)
	require.NotNil(t, note)
	// 上限 (256) + 著者 actor 1 回程度で頭打ちになり、際限なく fetch しないこと
	// (上限が無ければ chain は終わらない)。note/0..256 の 257 + actor 1 = 258 想定。
	assert.LessOrEqual(t, f.calls.Load(), int64(260))
	// 深く再帰したこと自体の確認: 上限近くの note は取り込まれている。
	_, err = noteRepo.FindByURI("https://remote.example/notes/200")
	require.NoError(t, err)
	// 上限を超えた先の note は ErrRecursionLimit で truncate され取り込まれない。
	_, err = noteRepo.FindByURI("https://remote.example/notes/300")
	require.Error(t, err)
}

// TestResolveActor_StrictRejectsCrossHostRedirect pins the upstream Strict
// request-url ↔ id binding (#1828): a federation-loop fetch whose original
// request host differs from the fetched actor's id host is rejected even when
// the final (post-redirect) host matches the id host. This blocks an
// attacker-controlled entry URI from redirecting to a victim actor.
func TestResolveActor_StrictRejectsCrossHostRedirect(t *testing.T) {
	f := &finalURLStubFetcher{body: []byte(sampleActor), finalURL: "https://remote.example/users/alice"}
	r, repo := newResolverWithFetcher(t, f)
	_, err := r.ResolveActor("https://attacker.example/users/alice")
	require.ErrorIs(t, err, federation.ErrObjectHostMismatch)
	assert.Empty(t, repo.Users)
}

// TestResolveActorAllowCrossHost_AllowsCrossHostRedirect: the user-initiated
// ap/show path relaxes the request-host binding (upstream CrossOrigin softfail),
// so a cross-host redirect resolves successfully (#1828)。
func TestResolveActorAllowCrossHost_AllowsCrossHostRedirect(t *testing.T) {
	f := &finalURLStubFetcher{body: []byte(sampleActor), finalURL: "https://remote.example/users/alice"}
	r, repo := newResolverWithFetcher(t, f)
	user, err := r.ResolveActorAllowCrossHost("https://attacker.example/users/alice")
	require.NoError(t, err)
	assert.Equal(t, "alice", user.Username)
	assert.Len(t, repo.Users, 1)
}

// crossHostDispatchFetcher serves the note for /notes/ URIs and the author actor
// for /users/ URIs, each with a controllable final URL, so note resolution
// (which resolves attributedTo) can be exercised end-to-end (#1828)。
type crossHostDispatchFetcher struct {
	noteBody, actorBody   string
	noteFinal, actorFinal string
}

func (f *crossHostDispatchFetcher) FetchObject(uri string) ([]byte, error) {
	b, _, err := f.FetchObjectWithFinalURL(uri)
	return b, err
}

func (f *crossHostDispatchFetcher) FetchObjectWithFinalURL(uri string) ([]byte, string, error) {
	if strings.Contains(uri, "/users/") {
		return []byte(f.actorBody), f.actorFinal, nil
	}
	return []byte(f.noteBody), f.noteFinal, nil
}

// TestResolveNote_StrictRejectsCrossHostRedirect mirrors the actor test for
// note resolution (#1828)。
func TestResolveNote_StrictRejectsCrossHostRedirect(t *testing.T) {
	f := &crossHostDispatchFetcher{
		noteBody:   sampleRemoteNote,
		noteFinal:  "https://remote.example/notes/n1",
		actorBody:  sampleActor,
		actorFinal: "https://remote.example/users/alice",
	}
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, f, idGen)
	_, err := r.ResolveNote("https://attacker.example/notes/x")
	require.ErrorIs(t, err, federation.ErrObjectHostMismatch)
	assert.Empty(t, noteRepo.Notes)
}

// TestResolveNoteAllowCrossHost_AllowsCrossHostRedirect: ap/show note lookup
// relaxes the request-host binding and resolves the note (and its author)
// across a cross-host redirect (#1828)。
func TestResolveNoteAllowCrossHost_AllowsCrossHostRedirect(t *testing.T) {
	f := &crossHostDispatchFetcher{
		noteBody:   sampleRemoteNote,
		noteFinal:  "https://remote.example/notes/n1",
		actorBody:  sampleActor,
		actorFinal: "https://remote.example/users/alice",
	}
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, f, idGen)
	note, err := r.ResolveNoteAllowCrossHost("https://attacker.example/notes/x")
	require.NoError(t, err)
	require.NotNil(t, note)
}

// failingUserRepo returns Create errors for the resolver.
type failingUserRepo struct {
	*testutil.MockUserRepository
}

func (f *failingUserRepo) Create(_ *model.User) error {
	return errors.New("create failed")
}

func TestResolveActor_RepoCreateError(t *testing.T) {
	mock := testutil.NewMockUserRepository()
	repo := &failingUserRepo{MockUserRepository: mock}
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	_, err := r.ResolveActor("https://remote.example/users/alice")
	assert.Error(t, err)
}

func TestResolveActorByKeyID(t *testing.T) {
	r, _ := newResolver(t, sampleActor, nil)
	user, err := r.ResolveActorByKeyID("https://remote.example/users/alice#main-key")
	require.NoError(t, err)
	assert.Equal(t, "alice", user.Username)
}

func TestPublicKeyForActor_Missing(t *testing.T) {
	r, _ := newResolver(t, sampleActor, nil)
	_, err := r.PublicKeyForActor("ghost")
	assert.Error(t, err)
}

// --- ResolveNote / IngestNote --------------------------------------------------

const sampleRemoteNote = `{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": "https://remote.example/notes/n1",
	"type": "Note",
	"attributedTo": "https://remote.example/users/alice",
	"content": "hello",
	"to": ["https://www.w3.org/ns/activitystreams#Public"],
	"cc": ["https://remote.example/users/alice/followers"],
	"summary": "cw",
	"sensitive": true
}`

// scriptedFetcher returns different bodies based on a counter so a single
// resolver call can resolve both an actor and a note.
type scriptedFetcher struct {
	bodies [][]byte
	idx    int
}

func (s *scriptedFetcher) FetchObject(_ string) ([]byte, error) {
	if s.idx >= len(s.bodies) {
		return nil, errors.New("no more bodies")
	}
	b := s.bodies[s.idx]
	s.idx++
	return b, nil
}

func TestResolveNote_LocalAlreadyStored(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["nlocal"] = &model.Note{ID: "nlocal", UserID: "u1"}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

	got, err := r.ResolveNote("https://example.com/notes/nlocal")
	require.NoError(t, err)
	assert.Equal(t, "nlocal", got.ID)
}

func TestResolveNote_LocalUnknown(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

	_, err := r.ResolveNote("https://example.com/notes/missing")
	assert.ErrorIs(t, err, federation.ErrInvalidNote)
}

func TestResolveNote_RemoteCached(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/n1"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", URI: &uri}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

	got, err := r.ResolveNote(uri)
	require.NoError(t, err)
	assert.Equal(t, "n1", got.ID)
}

func TestResolveNote_RemoteFetchAndIngest(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	// 最初の呼び出しで Note JSON、続いて attributedTo を解決するための actor JSON を返す
	fetcher := &scriptedFetcher{bodies: [][]byte{[]byte(sampleRemoteNote), []byte(sampleActor)}}
	r := federation.NewResolver(repo, noteRepo, urls, fetcher, idGen)

	got, err := r.ResolveNote("https://remote.example/notes/n1")
	require.NoError(t, err)
	assert.NotNil(t, got)
	require.NotNil(t, got.URI)
	assert.Equal(t, "https://remote.example/notes/n1", *got.URI)
	require.NotNil(t, got.Text)
	assert.Equal(t, "hello", *got.Text)
	require.NotNil(t, got.CW)
	assert.Equal(t, "cw", *got.CW)
	assert.Equal(t, model.NoteVisibilityPublic, got.Visibility)
}

func TestResolveNote_FetchError(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{err: errors.New("net down")}, idGen)
	_, err := r.ResolveNote("https://remote.example/notes/x")
	assert.Error(t, err)
}

func TestResolveNote_NoNoteRepo(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, nil, urls, &stubFetcher{}, idGen)
	_, err := r.ResolveNote("https://remote.example/notes/x")
	assert.ErrorIs(t, err, federation.ErrInvalidNote)
}

func TestIngestNote_BadJSON(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
	_, err := r.IngestNote([]byte(`{not json`))
	assert.ErrorIs(t, err, federation.ErrInvalidNote)
}

func TestIngestNote_MissingFields(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
	_, err := r.IngestNote([]byte(`{"id":"x"}`))
	assert.ErrorIs(t, err, federation.ErrInvalidNote)
}

func TestIngestNote_NoNoteRepo(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, nil, urls, &stubFetcher{}, idGen)
	_, err := r.IngestNote([]byte(sampleRemoteNote))
	assert.ErrorIs(t, err, federation.ErrInvalidNote)
}

func TestIngestNote_DedupOnExisting(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/n1"
	noteRepo.Notes["existing"] = &model.Note{ID: "existing", URI: &uri}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	got, err := r.IngestNote([]byte(sampleRemoteNote))
	require.NoError(t, err)
	assert.Equal(t, "existing", got.ID)
}

// IngestNoteWithCreated は新規 INSERT 経路で created=true、dedup hit で
// created=false を返す (#1156)。Processor 側で chart hook を created flag
// で gate するために使う。
func TestIngestNoteWithCreated_FreshIngest(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	got, created, err := r.IngestNoteWithCreated([]byte(sampleRemoteNote), "")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, created, "fresh ingest must report created=true so caller can fire chart hooks")
}

func TestIngestNoteWithCreated_DedupReturnsFalse(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/n1"
	noteRepo.Notes["existing"] = &model.Note{ID: "existing", URI: &uri}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	got, created, err := r.IngestNoteWithCreated([]byte(sampleRemoteNote), "")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "existing", got.ID)
	assert.False(t, created, "dedup hit must report created=false so caller can skip non-idempotent chart hooks")
}

// #1839: inbound Create で note の著者 (attributedTo) が配送 actor と異なる場合、
// なりすまし forge として拒否する (ErrNoteAttributionMismatch)。
func TestIngestNoteWithCreated_AttributionMismatchRejected(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	// note の著者は remote.example/users/alice だが、配送 actor は別人。
	_, _, err := r.IngestNoteWithCreated([]byte(sampleRemoteNote), "https://other.example/users/mallory")
	require.ErrorIs(t, err, federation.ErrNoteAttributionMismatch)
	assert.Empty(t, noteRepo.Notes, "なりすまし note を作ってはいけない")
}

// #1839: note の id host と attributedTo host が異なる場合も cross-host forge と
// して拒否する。
func TestIngestNoteWithCreated_CrossHostIDRejected(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	// id は a.example、attributedTo は b.example。
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", "id":"https://a.example/notes/x","type":"Note","attributedTo":"https://b.example/users/y","content":"forge"}`
	_, _, err := r.IngestNoteWithCreated([]byte(body), "")
	require.ErrorIs(t, err, federation.ErrNoteAttributionMismatch)
	assert.Empty(t, noteRepo.Notes)
}

// #1839: 著者が配送 actor 本人で id/attributedTo host が一致すれば従来どおり取り込む。
func TestIngestNoteWithCreated_LegitAttributionAccepted(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	got, created, err := r.IngestNoteWithCreated([]byte(sampleRemoteNote), "https://remote.example/users/alice")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, created)
}

func TestIngestNote_ResolveActorError(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{err: errors.New("actor down")}, idGen)
	_, err := r.IngestNote([]byte(sampleRemoteNote))
	assert.Error(t, err)
}

// failingNoteCreateRepo causes Create on noteRepo to fail.
type failingNoteCreateRepo struct {
	*testutil.MockNoteRepository
}

func (f *failingNoteCreateRepo) Create(_ *model.Note) error {
	return errors.New("create failed")
}

// #679: AP `tag` 配列の Hashtag entry を note.tags に取り込む。
func TestIngestNote_HashtagsFromTagArray(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/h1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "no inline tag here",
		"to": ["https://www.w3.org/ns/activitystreams#Public"],
		"tag": [
			{"type": "Hashtag", "name": "#golang", "href": "https://remote.example/tags/golang"},
			{"type": "Hashtag", "name": "#federation"},
			{"type": "Mention", "href": "https://remote.example/users/bob"}
		]
	}`
	note, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"golang", "federation"}, []string(note.Tags), "Hashtag entries の name から #prefix を剥がして格納")
}

// #679: text 由来の hashtag fallback。Mastodon 等で `tag` 配列が無い実装でも
// 本文 / CW から拾う。
func TestIngestNote_HashtagsFromTextFallback(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/h2",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "hello #golang and #federation world",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	note, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"golang", "federation"}, []string(note.Tags))
}

// #679: AP tag と text の両方に hashtag があれば case-insensitive で dedup される。
func TestIngestNote_HashtagsMergeAndDedup(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/h3",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "see #Golang and #news",
		"to": ["https://www.w3.org/ns/activitystreams#Public"],
		"tag": [
			{"type": "Hashtag", "name": "#golang"}
		]
	}`
	note, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	// "#golang" (tag 配列) と "#Golang" (text) は case-insensitive で同一視。
	// hashtag.Extract は first-seen casing を保つので tag 配列が先で `golang`。
	assert.ElementsMatch(t, []string{"golang", "news"}, []string(note.Tags))
}

// #679: CW (summary) 由来の hashtag も抽出される。本文が無く CW にだけ
// hashtag があるケース (sensitive note 等) で trends 集計に拾われないと
// 取りこぼしになる。
func TestIngestNote_HashtagsFromCW(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/h-cw",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "no inline tag",
		"summary": "important #news here",
		"sensitive": true,
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	note, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"news"}, []string(note.Tags))
}

// #679 review #2: `name` に `#` prefix が付いていない非標準実装からの
// Hashtag tag も defensive 補完によって正しく拾われる。upstream Misskey の
// renderHashtag では起きないが将来の AP 実装互換性を guard する。
func TestIngestNote_HashtagWithoutPrefixDefensive(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/h-noprefix",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "no inline tag",
		"to": ["https://www.w3.org/ns/activitystreams#Public"],
		"tag": [
			{"type": "Hashtag", "name": "golang"}
		]
	}`
	note, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	assert.Contains(t, []string(note.Tags), "golang", "name に # が無くても defensive 補完で抽出される")
}

// #679: Hashtag 無しなら note.Tags は nil/empty で残る。
func TestIngestNote_NoHashtags(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/h4",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "no tags here",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	note, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	assert.Empty(t, []string(note.Tags))
}

func TestIngestNote_NoteCreateError(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := &failingNoteCreateRepo{MockNoteRepository: testutil.NewMockNoteRepository()}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	_, err := r.IngestNote([]byte(sampleRemoteNote))
	assert.Error(t, err)
}

// stubPollVoter records IngestNote → AP poll vote routing.
type stubPollVoter struct {
	calls []struct {
		User   *model.User
		NoteID string
		Choice int
	}
	err error
}

func (s *stubPollVoter) Vote(user *model.User, noteID string, choice int) error {
	s.calls = append(s.calls, struct {
		User   *model.User
		NoteID string
		Choice int
	}{user, noteID, choice})
	return s.err
}

// TestIngestNote_APVoteRoutedToPollService covers the #690 fix where an
// inbound AP Note with `name` + `inReplyTo` to a poll-bearing local note is
// treated as a vote and routed to pollService.Vote, not persisted as a
// reply note.
func TestIngestNote_APVoteRoutedToPollService(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["pollNote"] = &model.Note{ID: "pollNote", UserID: "author", HasPoll: true, Visibility: model.NoteVisibilityPublic}
	pollRepo := testutil.NewMockPollRepository()
	require.NoError(t, pollRepo.Create(&model.Poll{
		NoteID: "pollNote", Choices: []string{"Apple", "Banana"}, Votes: []int64{0, 0},
	}))
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	r.SetPollRepo(pollRepo)
	voter := &stubPollVoter{}
	r.SetPollVoter(voter)

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/alice#votes/pollNote",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"name": "Banana",
		"inReplyTo": "https://example.com/notes/pollNote",
		"to": ["https://example.com/users/author"]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	assert.Nil(t, got, "AP vote must NOT create a note (caller skips fanout/notification)")
	require.Len(t, voter.calls, 1, "pollVoter.Vote must be called exactly once")
	assert.Equal(t, "pollNote", voter.calls[0].NoteID)
	assert.Equal(t, 1, voter.calls[0].Choice, "Banana = choice index 1")
}

// TestIngestNote_APVoteUnknownChoiceFallsThrough verifies that if the
// `name` does NOT match any poll choice, IngestNote falls through to
// regular reply processing instead of swallowing the note silently.
func TestIngestNote_APVoteUnknownChoiceFallsThrough(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["pollNote"] = &model.Note{ID: "pollNote", UserID: "author", HasPoll: true, Visibility: model.NoteVisibilityPublic}
	pollRepo := testutil.NewMockPollRepository()
	require.NoError(t, pollRepo.Create(&model.Poll{
		NoteID: "pollNote", Choices: []string{"Apple", "Banana"}, Votes: []int64{0, 0},
	}))
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	r.SetPollRepo(pollRepo)
	voter := &stubPollVoter{}
	r.SetPollVoter(voter)

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/alice#votes/pollNote",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"name": "Cherry",
		"inReplyTo": "https://example.com/notes/pollNote",
		"to": ["https://example.com/users/author"]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	require.NotNil(t, got, "unmatched choice falls through to reply note creation")
	assert.Empty(t, voter.calls)
	require.NotNil(t, got.ReplyID)
	assert.Equal(t, "pollNote", *got.ReplyID)
}

// TestIngestNote_NormalReplyToPollNotConfusedAsVote verifies that a reply
// note WITHOUT `name` is processed as a normal reply, not a vote.
func TestIngestNote_NormalReplyToPollNotConfusedAsVote(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["pollNote"] = &model.Note{ID: "pollNote", UserID: "author", HasPoll: true, Visibility: model.NoteVisibilityPublic}
	pollRepo := testutil.NewMockPollRepository()
	require.NoError(t, pollRepo.Create(&model.Poll{
		NoteID: "pollNote", Choices: []string{"Apple"}, Votes: []int64{0},
	}))
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	r.SetPollRepo(pollRepo)
	voter := &stubPollVoter{}
	r.SetPollVoter(voter)

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/normalreply",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "Just commenting on the poll",
		"inReplyTo": "https://example.com/notes/pollNote",
		"to": ["https://example.com/users/author"]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Empty(t, voter.calls, "normal reply must not invoke pollVoter")
	require.NotNil(t, got.ReplyID)
}

func TestIngestNote_ReplyToLocal(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["parent"] = &model.Note{ID: "parent", UserID: "uparent"}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/n2",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "reply",
		"inReplyTo": "https://example.com/notes/parent",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	require.NotNil(t, got.ReplyID)
	assert.Equal(t, "parent", *got.ReplyID)
}

func TestIngestNote_ReplyToRemote(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	parentURI := "https://remote.example/notes/parent"
	noteRepo.Notes["parent"] = &model.Note{ID: "parent", UserID: "uparent", URI: &parentURI}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/n3",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "reply",
		"inReplyTo": "https://remote.example/notes/parent",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	require.NotNil(t, got.ReplyID)
	assert.Equal(t, "parent", *got.ReplyID)
}

func TestIngestNote_SensitiveWithoutSummary(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/n4",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "nsfw",
		"sensitive": true,
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	require.NotNil(t, got.CW)
	assert.Equal(t, "", *got.CW)
}

// --- IngestNote visibility (ported from nekonoverse fetch_remote_note) --------

// makeNoteJSON builds a minimal AP Note with the given to/cc audience.
func makeNoteJSON(noteID string, to, cc []string) []byte {
	toJSON, _ := json.Marshal(to)
	ccJSON, _ := json.Marshal(cc)
	return []byte(fmt.Sprintf(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/%s",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "visibility test",
		"to": %s,
		"cc": %s
	}`, noteID, toJSON, ccJSON))
}

func TestIngestNote_PublicVisibility(t *testing.T) {
	// to=[Public], cc=[followers] → public
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := makeNoteJSON("vis-public", []string{"https://www.w3.org/ns/activitystreams#Public"}, []string{"https://remote.example/users/alice/followers"})
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	assert.Equal(t, model.NoteVisibilityPublic, got.Visibility)
}

func TestIngestNote_HomeVisibility(t *testing.T) {
	// to=[followers], cc=[Public] → home
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := makeNoteJSON("vis-home", []string{"https://remote.example/users/alice/followers"}, []string{"https://www.w3.org/ns/activitystreams#Public"})
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	assert.Equal(t, model.NoteVisibilityHome, got.Visibility)
}

// cc にのみ Public があり to に followers が無い shape も home に解決する。旧
// deriveVisibility は home 判定に followers-in-to を要求していたため specified に
// 落としていたが、upstream parseAudience は cc に Public があれば home とする (#1864)。
func TestIngestNote_CCOnlyPublicVisibility(t *testing.T) {
	// to=[], cc=[Public] → home
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := makeNoteJSON("vis-cc-public", []string{}, []string{"https://www.w3.org/ns/activitystreams#Public"})
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	assert.Equal(t, model.NoteVisibilityHome, got.Visibility)
}

func TestIngestNote_FollowersVisibility(t *testing.T) {
	// to=[followers], cc=[] → followers
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := makeNoteJSON("vis-followers", []string{"https://remote.example/users/alice/followers"}, []string{})
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	assert.Equal(t, model.NoteVisibilityFollowers, got.Visibility)
}

func TestIngestNote_DirectVisibility(t *testing.T) {
	// to=[specific user], cc=[] → specified
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := makeNoteJSON("vis-direct", []string{"https://remote.example/users/bob"}, []string{})
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	assert.Equal(t, model.NoteVisibilitySpecified, got.Visibility)
}

func TestIngestNote_EmptyAudienceVisibility(t *testing.T) {
	// to=[], cc=[] → specified (fallback)
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := makeNoteJSON("vis-empty", []string{}, []string{})
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	assert.Equal(t, model.NoteVisibilitySpecified, got.Visibility)
}

// --- IngestNote text fallback (source.content → _misskey_content → HTML→MFM) -

func TestIngestNote_TextFallback_SourceContent(t *testing.T) {
	// source.content (MFM mediaType) が最優先で使われる
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/source1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "<p>HTML content</p>",
		"source": {"content": "MFM source text", "mediaType": "text/x.misskeymarkdown"},
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	require.NotNil(t, got.Text)
	assert.Equal(t, "MFM source text", *got.Text)
}

func TestIngestNote_TextFallback_MisskeyContent(t *testing.T) {
	// source が無い場合は _misskey_content が使われる
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/mk1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "<p>HTML content</p>",
		"_misskey_content": "MFM via _misskey_content",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	require.NotNil(t, got.Text)
	assert.Equal(t, "MFM via _misskey_content", *got.Text)
}

func TestIngestNote_TextFallback_HTMLConversion(t *testing.T) {
	// source も _misskey_content も無い場合は HTML→MFM 変換
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/html1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "<p>paragraph1</p><p>paragraph2</p>",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	require.NotNil(t, got.Text)
	assert.Equal(t, "paragraph1\n\nparagraph2", *got.Text)
}

func TestIngestNote_TextFallback_Priority(t *testing.T) {
	// 全部揃っている場合は source.content が優先される
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/prio1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "<p>HTML</p>",
		"_misskey_content": "misskey content",
		"source": {"content": "source wins", "mediaType": "text/x.misskeymarkdown"},
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	require.NotNil(t, got.Text)
	assert.Equal(t, "source wins", *got.Text)
}

func TestIngestNote_TextFallback_SourceWrongMediaType(t *testing.T) {
	// source.mediaType が MFM でない場合は _misskey_content にフォールバック
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/wrongmt1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "<p>HTML</p>",
		"_misskey_content": "misskey fallback",
		"source": {"content": "plain text source", "mediaType": "text/plain"},
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	require.NotNil(t, got.Text)
	assert.Equal(t, "misskey fallback", *got.Text)
}

// --- UpdateRemoteNote (Step J) -----------------------------------------------

const remoteNoteUpdateBody = `{ "@context": "https://www.w3.org/ns/activitystreams", 
	"id": "https://remote.example/notes/n1",
	"type": "Note",
	"attributedTo": "https://remote.example/users/alice",
	"content": "edited content",
	"summary": "edited cw"
}`

func TestUpdateRemoteNote_HappyPath(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/n1"
	host := "remote.example"
	original := "original"
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", URI: &uri, UserID: "alice-id", UserHost: &host, Text: &original,
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

	got, err := r.UpdateRemoteNote([]byte(remoteNoteUpdateBody), "")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.Text)
	assert.Equal(t, "edited content", *got.Text)
	require.NotNil(t, got.CW)
	assert.Equal(t, "edited cw", *got.CW)
}

// #1819: Update の actor が note 著者と一致しない場合、本文を改ざんさせない
// (別 remote 著者の note URI を狙った Update(Note) を拒否、Question 経路と対称)。
func TestUpdateRemoteNote_ActorMismatchRejected(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/n1"
	host := "remote.example"
	authorURI := "https://remote.example/users/alice"
	original := "original"
	repo.Users["alice-id"] = &model.User{ID: "alice-id", URI: &authorURI, Host: &host}
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", URI: &uri, UserID: "alice-id", UserHost: &host, Text: &original,
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

	// 別 actor (mallory) が alice の note を Update しようとする → 拒否 (本文不変)。
	got, err := r.UpdateRemoteNote([]byte(remoteNoteUpdateBody), "https://evil.example/users/mallory")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.Text)
	assert.Equal(t, "original", *got.Text, "actor 不一致の Update は本文を改ざんしない")
}

// #1819: actor が note 著者と一致すれば従来どおり更新される。
func TestUpdateRemoteNote_ActorMatchProceeds(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/n1"
	host := "remote.example"
	authorURI := "https://remote.example/users/alice"
	original := "original"
	repo.Users["alice-id"] = &model.User{ID: "alice-id", URI: &authorURI, Host: &host}
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", URI: &uri, UserID: "alice-id", UserHost: &host, Text: &original,
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

	got, err := r.UpdateRemoteNote([]byte(remoteNoteUpdateBody), authorURI)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.Text)
	assert.Equal(t, "edited content", *got.Text, "actor 一致の Update は反映される")
}

// #679: UpdateRemoteNote が note.tags を再計算する。tag 配列が変わった場合に
// DB tags も追従すること。
func TestUpdateRemoteNote_RecalculatesHashtags(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/n1"
	host := "remote.example"
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", URI: &uri, UserID: "alice-id", UserHost: &host,
		Tags: []string{"old"},
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/n1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "edited and now tagged #news",
		"tag": [
			{"type": "Hashtag", "name": "#federation"}
		]
	}`
	got, err := r.UpdateRemoteNote([]byte(body), "")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.ElementsMatch(t, []string{"federation", "news"}, []string(got.Tags), "古い tags は捨て、tag 配列 + 本文 fallback で再構築")
}

// #1372: hashtag が消えて tags が非空→空に変わる場合、note.tags は非nilの空配列
// ('{}') でなければならない。nil の model.StringArray は Updates() 経由で SQL NULL に
// なり note.tags (NOT NULL) 制約に違反する。
func TestUpdateRemoteNote_ClearsTagsToEmptyNotNull(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/n1"
	host := "remote.example"
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", URI: &uri, UserID: "alice-id", UserHost: &host,
		Tags: []string{"old"},
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/n1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "edited, no more hashtags"
	}`
	got, err := r.UpdateRemoteNote([]byte(body), "")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.Tags, "tags は非nilの空配列でなければ SQL NULL で NOT NULL 違反 (#1372)")
	assert.Empty(t, []string(got.Tags))
}

func TestUpdateRemoteNote_NoNoteRepo(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, nil, urls, &stubFetcher{}, idGen)
	_, err := r.UpdateRemoteNote([]byte(remoteNoteUpdateBody), "")
	assert.ErrorIs(t, err, federation.ErrInvalidNote)
}

func TestUpdateRemoteNote_BadJSON(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
	_, err := r.UpdateRemoteNote([]byte(`{not json`), "")
	assert.ErrorIs(t, err, federation.ErrInvalidNote)
}

func TestUpdateRemoteNote_MissingID(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
	_, err := r.UpdateRemoteNote([]byte(`{"type":"Note"}`), "")
	assert.ErrorIs(t, err, federation.ErrInvalidNote)
}

func TestUpdateRemoteNote_NotFound(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
	got, err := r.UpdateRemoteNote([]byte(remoteNoteUpdateBody), "")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestUpdateRemoteNote_LocalNoteSkipped(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/n1"
	original := "original"
	// UserHost == nil → ローカルノート扱い
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", URI: &uri, UserID: "alice-id", Text: &original,
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

	got, err := r.UpdateRemoteNote([]byte(remoteNoteUpdateBody), "")
	require.NoError(t, err)
	require.NotNil(t, got)
	// Text は変わらない
	require.NotNil(t, got.Text)
	assert.Equal(t, "original", *got.Text)
}

func TestUpdateRemoteNote_EmptyContentNoOp(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/n1"
	host := "remote.example"
	original := "original"
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", URI: &uri, UserID: "alice-id", UserHost: &host, Text: &original,
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/n1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice"
	}`)
	got, err := r.UpdateRemoteNote(body, "")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.Text)
	assert.Equal(t, "original", *got.Text)
}

func TestUpdateRemoteNote_SensitiveWithoutSummary(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/n1"
	host := "remote.example"
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", URI: &uri, UserID: "alice-id", UserHost: &host,
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/n1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "nsfw",
		"sensitive": true
	}`)
	got, err := r.UpdateRemoteNote(body, "")
	require.NoError(t, err)
	require.NotNil(t, got.CW)
	assert.Equal(t, "", *got.CW)
}

// failingNoteUpdateRepo causes UpdateFields to fail.
type failingNoteUpdateRepo struct {
	*testutil.MockNoteRepository
}

func (f *failingNoteUpdateRepo) UpdateFields(_ string, _ map[string]any) error {
	return errors.New("update failed")
}

func TestUpdateRemoteNote_UpdateFieldsError(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	mock := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/n1"
	host := "remote.example"
	mock.Notes["n1"] = &model.Note{
		ID: "n1", URI: &uri, UserID: "alice-id", UserHost: &host,
	}
	noteRepo := &failingNoteUpdateRepo{MockNoteRepository: mock}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
	_, err := r.UpdateRemoteNote([]byte(remoteNoteUpdateBody), "")
	assert.Error(t, err)
}

// --- TTL cache (Step G) -------------------------------------------------------

func TestResolveActor_TTLRefresh(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://remote.example/users/alice"
	// LastFetchedAt が古いため refresh が走るはず
	old := time.Now().Add(-48 * time.Hour)
	repo.Users["existing"] = &model.User{
		ID:            "existing",
		Username:      "alice",
		URI:           &uri,
		LastFetchedAt: &old,
	}
	// 更新後の actor を返す
	updated := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"name": "Alice Refreshed",
		"inbox": "https://remote.example/users/alice/inbox-v2",
		"endpoints": {"sharedInbox": "https://remote.example/inbox-v2"},
		"publicKey": {"publicKeyPem": "REFRESHED"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(updated)}, idGen)
	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	require.NotNil(t, user.Name)
	assert.Equal(t, "Alice Refreshed", *user.Name)
	require.NotNil(t, user.Inbox)
	assert.Equal(t, "https://remote.example/users/alice/inbox-v2", *user.Inbox)
	require.NotNil(t, user.SharedInbox)
	assert.Equal(t, "https://remote.example/inbox-v2", *user.SharedInbox)

	pem, err := r.PublicKeyForActor("existing")
	require.NoError(t, err)
	assert.Contains(t, pem, "REFRESHED")
}

// refresh 時に actor.summary を UserProfile.description に同期する (#1022)。
// profile 行が既に存在するケース (= post-fix で新規取り込まれた user) は
// UpdateProfile 経由で description を上書きする。
func TestResolveActor_TTLRefreshUpdatesDescription(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://remote.example/users/alice"
	old := time.Now().Add(-48 * time.Hour)
	repo.Users["existing"] = &model.User{
		ID:            "existing",
		Username:      "alice",
		URI:           &uri,
		LastFetchedAt: &old,
	}
	oldDesc := "old bio"
	repo.Profiles["existing"] = &model.UserProfile{
		UserID:      "existing",
		Description: &oldDesc,
	}
	updated := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"summary": "new bio",
		"publicKey": {"publicKeyPem": "REFRESHED"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(updated)}, idGen)
	_, err := r.ResolveActor(uri)
	require.NoError(t, err)
	require.NotNil(t, repo.Profiles["existing"].Description)
	assert.Equal(t, "new bio", *repo.Profiles["existing"].Description)
}

// attachment はあるが PropertyValue が 1 件も無い actor (Mastodon が画像だけを
// 付けるケース等)。fields は nil ではなく "[]" にする。nil を渡すと jsonb 列が
// 空文字を受けて SQLSTATE 22P02 になり、UPDATE ごと落ちて description まで
// 巻き添えになる。**refresh 経路でしか踏まないので、更新まで通して見る。**
func TestResolveActor_ProfileExtrasNoPropertyValueAttachments(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://mstdn.example/users/liam"
	old := time.Now().Add(-48 * time.Hour)
	repo.Users["existing-liam"] = &model.User{
		ID: "existing-liam", Username: "liam", URI: &uri, LastFetchedAt: &old,
	}
	repo.Profiles["existing-liam"] = &model.UserProfile{
		UserID: "existing-liam", Fields: []byte("[]"),
	}
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://mstdn.example/users/liam",
		"type": "Person",
		"preferredUsername": "liam",
		"inbox": "https://mstdn.example/users/liam/inbox",
		"summary": "bio",
		"attachment": [
			{"type":"Image","url":"https://mstdn.example/img.png"},
			"https://mstdn.example/plain-string"
		],
		"publicKey": {"publicKeyPem": "PEM"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	_, err := r.ResolveActor(uri)
	require.NoError(t, err)

	p := repo.Profiles["existing-liam"]
	require.NotNil(t, p.Description, "description が巻き添えにならない")
	assert.JSONEq(t, "[]", string(p.Fields))
}

// PropertyValue でも name / value が string でなければ落とす。upstream の
// isPropertyValue は両方に string を要求する。
func TestResolveActor_ProfileExtrasRequiresStringNameValue(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://mstdn.example/users/mona"
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://mstdn.example/users/mona",
		"type": "Person",
		"preferredUsername": "mona",
		"inbox": "https://mstdn.example/users/mona/inbox",
		"attachment": [
			{"type":"PropertyValue","name":"NumValue","value":42},
			{"type":"PropertyValue","name":123,"value":"x"},
			{"type":"PropertyValue","name":"NoValue"},
			{"type":"PropertyValue","name":"  ","value":"blank name"},
			{"type":"PropertyValue","name":"Blank","value":"   "},
			{"type":"PropertyValue","name":"Good","value":"kept"}
		],
		"publicKey": {"publicKeyPem": "PEM"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	var fields []struct{ Name, Value string }
	require.NoError(t, json.Unmarshal(repo.Profiles[user.ID].Fields, &fields))
	require.Len(t, fields, 1, "string でない name / value と、trim 後に空になる entry は落とす")
	assert.Equal(t, "Good", fields[0].Name)
}

// _misskey_summary は mfm.FromHTML を通らないので、NUL を明示的に落とす必要が
// ある。ここが抜けると Misskey 系の actor で profile の書き込みごと落ちる。
func TestResolveActor_DescriptionStripsNULFromMisskeySummary(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://misskey.example/users/nina"
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://misskey.example/users/nina",
		"type": "Person",
		"preferredUsername": "nina",
		"inbox": "https://misskey.example/users/nina/inbox",
		"_misskey_summary": "bi\u0000o",
		"publicKey": {"publicKeyPem": "PEM"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	got := repo.Profiles[user.ID].Description
	require.NotNil(t, got)
	assert.Equal(t, "bio", *got)
}

// NUL を含む値で profile の書き込みごと落とさない (#2661)。JSON の NUL
// エスケープは正当な入力で、Go の decoder は実 NUL を作る。PostgreSQL の
// text 系列も jsonb もこれを拒否するので、同じ書き込みに乗っている
// description まで巻き添えになり、create 経路では profile 行が 1 行も
// 作られなくなる (以後の refresh も同じ create を繰り返して失敗し続ける)。
func TestResolveActor_ProfileExtrasStripsNUL(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://mstdn.example/users/ivan"
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://mstdn.example/users/ivan",
		"type": "Person",
		"preferredUsername": "ivan",
		"inbox": "https://mstdn.example/users/ivan/inbox",
		"summary": "bio",
		"vcard:Address": "To\u0000kyo",
		"attachment": [{"type":"PropertyValue","name":"We\u0000b","value":"exa\u0000mple.org"}],
		"publicKey": {"publicKeyPem": "PEM"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	user, err := r.ResolveActor(uri)
	require.NoError(t, err)

	p := repo.Profiles[user.ID]
	require.NotNil(t, p, "profile 行は作られる")
	require.NotNil(t, p.Description, "description が巻き添えにならない")
	require.NotNil(t, p.Location)
	assert.Equal(t, "Tokyo", *p.Location)
	assert.NotContains(t, *p.Location, "\x00")

	var fields []struct{ Name, Value string }
	require.NoError(t, json.Unmarshal(p.Fields, &fields))
	require.Len(t, fields, 1)
	assert.Equal(t, "Web", fields[0].Name)
	assert.Equal(t, "example.org", fields[0].Value)
	assert.NotContains(t, string(p.Fields), "\x00")
	// jsonb は NUL のエスケープ表現も拒否する。
	assert.NotContains(t, string(p.Fields), `\u0000`)
}

// `type` が配列の PropertyValue も拾う。upstream の getApType は配列なら
// 先頭要素を見るので、string 決め打ちだと取りこぼす。
func TestResolveActor_ProfileExtrasAcceptsArrayType(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://mstdn.example/users/judy"
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://mstdn.example/users/judy",
		"type": "Person",
		"preferredUsername": "judy",
		"inbox": "https://mstdn.example/users/judy/inbox",
		"attachment": [
			{"type":["PropertyValue"],"name":"Web","value":"example.org"},
			{"type":[],"name":"Empty","value":"x"},
			{"type":[123],"name":"NonString","value":"x"}
		],
		"publicKey": {"publicKeyPem": "PEM"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	var fields []struct{ Name, Value string }
	require.NoError(t, json.Unmarshal(repo.Profiles[user.ID].Fields, &fields))
	require.Len(t, fields, 1)
	assert.Equal(t, "Web", fields[0].Name)
}

// リモート側が追加項目を消したら、こちらでも消える。refresh のたびに無条件で
// 3 列を書く (upstream の updatePerson も同じ) ので、消えることまで固定する。
func TestResolveActor_TTLRefreshClearsRemovedProfileExtras(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://mstdn.example/users/karl"
	old := time.Now().Add(-48 * time.Hour)
	repo.Users["existing-karl"] = &model.User{
		ID: "existing-karl", Username: "karl", URI: &uri, LastFetchedAt: &old,
	}
	loc, bday := "Kyoto", "1988-01-02"
	repo.Profiles["existing-karl"] = &model.UserProfile{
		UserID:   "existing-karl",
		Location: &loc,
		Birthday: &bday,
		Fields:   []byte(`[{"name":"Old","value":"gone"}]`),
	}
	// 追加項目を持たない actor に差し替わった。
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://mstdn.example/users/karl",
		"type": "Person",
		"preferredUsername": "karl",
		"inbox": "https://mstdn.example/users/karl/inbox",
		"publicKey": {"publicKeyPem": "REFRESHED"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	_, err := r.ResolveActor(uri)
	require.NoError(t, err)

	p := repo.Profiles["existing-karl"]
	assert.Nil(t, p.Location, "リモートが消したらこちらも消す")
	assert.Nil(t, p.Birthday)
	assert.JSONEq(t, "[]", string(p.Fields))
}

// tag / attachment の要素も `"type": ["X"]` を受ける。document 全体は通るが、
// 要素単位で落とすと**添付が消えたノート**や**通知が飛ばないメンション**に
// なる (upstream は getApType / isDocument 経由なので全部通る、#2662)。
func TestRemoteTagAndAttachment_ArrayTypeElements(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://mstdn.example/users/uma"
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://mstdn.example/users/uma",
		"type": ["Person"],
		"preferredUsername": "uma",
		"inbox": "https://mstdn.example/users/uma/inbox",
		"tag": [
			{"type": ["Hashtag"], "name": "#arraytag"},
			{"type": ["PropertyValue"], "name": "Web", "value": "example.org"}
		],
		"attachment": [{"type": ["PropertyValue"], "name": "Pronoun", "value": "they/them"}],
		"publicKey": {"publicKeyPem": "PEM"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	user, err := r.ResolveActor(uri)
	require.NoError(t, err)

	assert.Contains(t, []string(user.Tags), "arraytag", "配列 type の Hashtag も user.tags に載る")

	var fields []struct{ Name, Value string }
	require.NoError(t, json.Unmarshal(repo.Profiles[user.ID].Fields, &fields))
	require.Len(t, fields, 1)
	assert.Equal(t, "Pronoun", fields[0].Name)

	// emoji tag と Note の添付も同じ罠を持つ。document は通るので
	// 「絵文字が :name: のまま」「添付が消えたノート」として表に出る。
	emojis := federation.ExtractEmojiTags([]any{
		map[string]any{
			"type": []any{"Emoji"},
			"name": ":blobcat:",
			"icon": map[string]any{"type": "Image", "url": "https://mstdn.example/e/blobcat.png"},
		},
	})
	require.Len(t, emojis, 1, "配列 type の Emoji tag を拾うこと")
	assert.Equal(t, "https://mstdn.example/e/blobcat.png", emojis[0].Icon.URL.String())

	docs := federation.ExtractAttachments([]any{
		map[string]any{"type": []any{"Document"}, "url": "https://mstdn.example/files/a.png"},
	}, false)
	require.Len(t, docs, 1, "配列 type の添付を拾うこと")
	assert.Equal(t, "https://mstdn.example/files/a.png", docs[0].URL)

	// upstream の validDocumentTypes は Page も含む (type.ts:263)。
	pages := federation.ExtractAttachments([]any{
		map[string]any{"type": "Page", "url": "https://mstdn.example/files/p.pdf"},
	}, false)
	require.Len(t, pages, 1, "Page 添付も取り込む")

	// Mention を落とすと通知が飛ばない。
	hrefs := federation.ExtractMentionTags([]any{
		map[string]any{"type": []any{"Mention"}, "href": "https://mstdn.example/users/vic"},
	})
	assert.Equal(t, []string{"https://mstdn.example/users/vic"}, hrefs,
		"配列 type の Mention を拾うこと")
}

// tag / url が単一 object でも actor が解決できる。attachment と同じ理由で
// `[]any` / `string` 決め打ちだと unmarshal ごと失敗していた (#2662)。
// JSON-LD compaction は @container: @set が無い term の単一要素配列を
// 素の値に潰すので、この形は構造的に出てくる。
func TestResolveActor_SingleObjectTagAndLinkURL(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://mstdn.example/users/tess"
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://mstdn.example/users/tess",
		"type": "Person",
		"preferredUsername": "tess",
		"inbox": "https://mstdn.example/users/tess/inbox",
		"name": "Tess",
		"tag": {"type":"Emoji","name":":wave:","icon":{"type":"Image","url":"https://mstdn.example/e/wave.png"}},
		"url": {"type":"Link","href":"https://mstdn.example/@tess"},
		"publicKey": {"publicKeyPem": "PEM"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	user, err := r.ResolveActor(uri)
	require.NoError(t, err, "actor 解決が通ること")
	require.NotNil(t, user.Name)
	assert.Equal(t, "Tess", *user.Name)
}

// 単一 object の attachment を持つ actor も解決できる。`[]any` 決め打ちだと
// json.Unmarshal ごと失敗して **その actor が一切連合できなかった** (#2662)。
func TestResolveActor_SingleObjectAttachment(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://mstdn.example/users/pat"
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://mstdn.example/users/pat",
		"type": "Person",
		"preferredUsername": "pat",
		"inbox": "https://mstdn.example/users/pat/inbox",
		"attachment": {"type":"PropertyValue","name":"Web","value":"example.org"},
		"publicKey": {"publicKeyPem": "PEM"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	user, err := r.ResolveActor(uri)
	require.NoError(t, err, "actor 解決が通ること")
	require.NotNil(t, user)

	// 単一 object も 1 件として拾う。
	var fields []struct{ Name, Value string }
	require.NoError(t, json.Unmarshal(repo.Profiles[user.ID].Fields, &fields))
	require.Len(t, fields, 1)
	assert.Equal(t, "Web", fields[0].Name)
}

// 非 string の vcard を持つ actor も解決できる (値は捨てる)。
func TestResolveActor_NonStringVcard(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://mstdn.example/users/quinn"
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://mstdn.example/users/quinn",
		"type": "Person",
		"preferredUsername": "quinn",
		"inbox": "https://mstdn.example/users/quinn/inbox",
		"summary": "bio",
		"vcard:bday": {"@value": "1990-05-03"},
		"vcard:Address": ["Tokyo"],
		"_misskey_summary": {"a": 1},
		"publicKey": {"publicKeyPem": "PEM"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	user, err := r.ResolveActor(uri)
	require.NoError(t, err, "actor 解決が通ること")

	p := repo.Profiles[user.ID]
	require.NotNil(t, p.Description, "読める field は取り込む")
	// JSON-LD の展開形は剥がして拾う。捨てると「読めたはずの値が黙って消える」。
	require.NotNil(t, p.Birthday)
	assert.Equal(t, "1990-05-03", *p.Birthday)
	require.NotNil(t, p.Location)
	assert.Equal(t, "Tokyo", *p.Location)
}

// name は user.name (varchar(128))。upstream は truncate(person.name, 128) を
// 必ず通す。素通しすると userRepo.Create ごと落ちて actor が作られない。
func TestResolveActor_TruncatesDisplayName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		wantLen int
		wantNil bool
	}{
		{"short", "Alice", 5, false},
		{"exactly 128", strings.Repeat("a", 128), 128, false},
		{"over limit", strings.Repeat("b", 200), 128, false},
		{"multibyte over limit", strings.Repeat("日本", 200), 128, false},
		{"empty", "", 0, true},
		{"nul only", `\u0000`, 0, true},
		{"nul embedded", `Al\u0000ice`, 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			noteRepo := testutil.NewMockNoteRepository()
			urls := activitypub.NewURLBuilder("https://example.com")
			idGen, _ := id.NewGenerator("aidx")
			uri := "https://mstdn.example/users/rene"
			body := `{ "@context": "https://www.w3.org/ns/activitystreams",
				"id": "https://mstdn.example/users/rene",
				"type": "Person",
				"preferredUsername": "rene",
				"inbox": "https://mstdn.example/users/rene/inbox",
				"name": "` + tc.in + `",
				"publicKey": {"publicKeyPem": "PEM"}
			}`
			r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
			user, err := r.ResolveActor(uri)
			require.NoError(t, err)
			if tc.wantNil {
				assert.Nil(t, user.Name)
				return
			}
			require.NotNil(t, user.Name)
			assert.Equal(t, tc.wantLen, len([]rune(*user.Name)))
			assert.NotContains(t, *user.Name, "\u0000", "NUL は残さない")
		})
	}
}

// preferredUsername は user.username / usernameLower (varchar(128) NOT NULL) に
// そのまま入る。upstream validateActor と同じ条件で弾く。素通しすると
// userRepo.Create が落ちて actor がまったく作られない。
func TestResolveActor_RejectsUnusableUsername(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		ok   bool
	}{
		{"simple", "alice", true},
		{"with dash and dot", "a-b.c_d", true},
		{"single char", "a", true},
		{"digits", "user123", true},
		{"empty", "", false},
		{"too long", strings.Repeat("a", 129), false},
		{"exactly 128", strings.Repeat("a", 128), true},
		{"leading dash", "-alice", false},
		{"trailing dot", "alice.", false},
		{"space", "al ice", false},
		{"at sign", "alice@example", false},
		{"nul", `al\u0000ice`, false},
		{"multibyte", "日本", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			noteRepo := testutil.NewMockNoteRepository()
			urls := activitypub.NewURLBuilder("https://example.com")
			idGen, _ := id.NewGenerator("aidx")
			uri := "https://mstdn.example/users/sam"
			body := `{ "@context": "https://www.w3.org/ns/activitystreams",
				"id": "https://mstdn.example/users/sam",
				"type": "Person",
				"preferredUsername": "` + tc.in + `",
				"inbox": "https://mstdn.example/users/sam/inbox",
				"publicKey": {"publicKeyPem": "PEM"}
			}`
			r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
			_, err := r.ResolveActor(uri)
			if tc.ok {
				assert.NoError(t, err)
				return
			}
			// mock も制約違反で error を返すので、error が返ること自体では
			// 「DB に触れる前に弾いた」と言えない。ErrInvalidActor で固定する。
			assert.ErrorIs(t, err, federation.ErrInvalidActor)
		})
	}
}

// リモート actor の location / birthday / fields を取り込む (#2661)。送信側
// (renderer) は vcard:Address / vcard:bday / PropertyValue を出していたのに、
// 受信側が description しか読んでおらず、本番のリモートユーザー 28265 件が
// 全て空だった。
func TestResolveActor_ImportsProfileExtras(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://mstdn.example/users/carol"
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://mstdn.example/users/carol",
		"type": "Person",
		"preferredUsername": "carol",
		"inbox": "https://mstdn.example/users/carol/inbox",
		"vcard:Address": "Tokyo",
		"vcard:bday": "1990-05-03",
		"attachment": [
			{"type": "PropertyValue", "name": "Web", "value": "<a href=\"https://carol.example\">carol.example</a>"},
			{"type": "PropertyValue", "name": "Pronouns", "value": "she/her"},
			{"type": "Note", "name": "ignored", "value": "not a PropertyValue"}
		],
		"publicKey": {"publicKeyPem": "PEM"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	user, err := r.ResolveActor(uri)
	require.NoError(t, err)

	p := repo.Profiles[user.ID]
	require.NotNil(t, p)
	require.NotNil(t, p.Location)
	assert.Equal(t, "Tokyo", *p.Location)
	require.NotNil(t, p.Birthday)
	assert.Equal(t, "1990-05-03", *p.Birthday)

	var fields []struct{ Name, Value string }
	require.NoError(t, json.Unmarshal(p.Fields, &fields))
	require.Len(t, fields, 2, "PropertyValue でない attachment は無視する")
	assert.Equal(t, "Web", fields[0].Name)
	// value は HTML で来るので MFM に変換する (description と同じ理由)。
	assert.NotContains(t, fields[0].Value, "<a ")
	assert.Contains(t, fields[0].Value, "carol.example")
	assert.Equal(t, "Pronouns", fields[1].Name)
	assert.Equal(t, "she/her", fields[1].Value)
}

// 追加項目が無い actor では空のまま。fields は jsonb 列なので null ではなく
// [] を書く (golden の fields は配列必須)。
func TestResolveActor_ProfileExtrasAbsent(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://mstdn.example/users/dave"
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://mstdn.example/users/dave",
		"type": "Person",
		"preferredUsername": "dave",
		"inbox": "https://mstdn.example/users/dave/inbox",
		"publicKey": {"publicKeyPem": "PEM"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	user, err := r.ResolveActor(uri)
	require.NoError(t, err)

	p := repo.Profiles[user.ID]
	require.NotNil(t, p)
	assert.Nil(t, p.Location)
	assert.Nil(t, p.Birthday)
	assert.JSONEq(t, "[]", string(p.Fields))
}

// 不正な vcard:bday は取り込まない。birthday 列は char(10) で、upstream も
// `^\d{4}-\d{2}-\d{2}` にマッチしたものだけを使う。
func TestResolveActor_ProfileExtrasRejectsBadBirthday(t *testing.T) {
	for _, tc := range []struct {
		name string
		bday string
		want *string
	}{
		{"empty", "", nil},
		{"not a date", "yesterday", nil},
		{"wrong order", "03-05-1990", nil},
		{"short year", "90-05-03", nil},
		// 先頭アンカーが効いていること。upstream の match も先頭からしか拾わない。
		{"prefixed", "born 1990-05-03", nil},
		{"leading space", " 1990-05-03", nil},
		{"date only", "1990-05-03", strPtr("1990-05-03")},
		// 時刻付きは先頭の日付だけを取る (upstream の match と同じ)。
		{"with time", "1990-05-03T00:00:00Z", strPtr("1990-05-03")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			noteRepo := testutil.NewMockNoteRepository()
			urls := activitypub.NewURLBuilder("https://example.com")
			idGen, _ := id.NewGenerator("aidx")
			uri := "https://mstdn.example/users/eve"
			body := `{ "@context": "https://www.w3.org/ns/activitystreams",
				"id": "https://mstdn.example/users/eve",
				"type": "Person",
				"preferredUsername": "eve",
				"inbox": "https://mstdn.example/users/eve/inbox",
				"vcard:bday": "` + tc.bday + `",
				"publicKey": {"publicKeyPem": "PEM"}
			}`
			r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
			user, err := r.ResolveActor(uri)
			require.NoError(t, err)
			got := repo.Profiles[user.ID].Birthday
			if tc.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, *tc.want, *got)
		})
	}
}

// location は trim して、空になったら NULL にする (divergence.md に書いた挙動)。
// upstream は `person['vcard:Address'] ?? null` でそのまま保存する。
func TestResolveActor_ProfileExtrasTrimsLocation(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want *string
	}{
		{"surrounding space", " Tokyo ", strPtr("Tokyo")},
		{"blank only", "   ", nil},
		{"empty", "", nil},
		// JSON のエスケープとして渡す (生のタブ / 改行は JSON 文字列に置けない)。
		{"tab and newline", `\tKyoto\n`, strPtr("Kyoto")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			noteRepo := testutil.NewMockNoteRepository()
			urls := activitypub.NewURLBuilder("https://example.com")
			idGen, _ := id.NewGenerator("aidx")
			uri := "https://mstdn.example/users/olga"
			body := `{ "@context": "https://www.w3.org/ns/activitystreams",
				"id": "https://mstdn.example/users/olga",
				"type": "Person",
				"preferredUsername": "olga",
				"inbox": "https://mstdn.example/users/olga/inbox",
				"vcard:Address": "` + tc.in + `",
				"publicKey": {"publicKeyPem": "PEM"}
			}`
			r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
			user, err := r.ResolveActor(uri)
			require.NoError(t, err)
			got := repo.Profiles[user.ID].Location
			if tc.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, *tc.want, *got)
		})
	}
}

// location は varchar(128)。**upstream は truncate しない**が、mk-go では
// 超過値をそのまま渡すと profile の書き込みごと落ちて description まで
// 巻き添えになるので切る。
func TestResolveActor_ProfileExtrasTruncatesLocation(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://mstdn.example/users/frank"
	long := strings.Repeat("あ", 200)
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://mstdn.example/users/frank",
		"type": "Person",
		"preferredUsername": "frank",
		"inbox": "https://mstdn.example/users/frank/inbox",
		"vcard:Address": "` + long + `",
		"publicKey": {"publicKeyPem": "PEM"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	got := repo.Profiles[user.ID].Location
	require.NotNil(t, got)
	// rune 単位で切る (byte で切ると多バイト文字が壊れる)。
	assert.Equal(t, 128, len([]rune(*got)))
}

// fields の件数上限。upstream の analyzeAttachments には上限が無いが、
// ローカルの i/update は maxItems: 16 なのでリモートも揃える。
func TestResolveActor_ProfileExtrasCapsFieldCount(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://mstdn.example/users/grace"
	var items []string
	for i := range 40 {
		items = append(items, fmt.Sprintf(`{"type":"PropertyValue","name":"n%d","value":"v%d"}`, i, i))
	}
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://mstdn.example/users/grace",
		"type": "Person",
		"preferredUsername": "grace",
		"inbox": "https://mstdn.example/users/grace/inbox",
		"attachment": [` + strings.Join(items, ",") + `],
		"publicKey": {"publicKeyPem": "PEM"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	var fields []struct{ Name, Value string }
	require.NoError(t, json.Unmarshal(repo.Profiles[user.ID].Fields, &fields))
	assert.Len(t, fields, 16)
}

// 既存 remote user も refresh で back-fill される。本番の 28265 件はこの経路で
// 埋まる (description が #1022 で同じ形をとったのと同様)。
func TestResolveActor_TTLRefreshBackfillsProfileExtras(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://mstdn.example/users/heidi"
	old := time.Now().Add(-48 * time.Hour)
	repo.Users["existing-heidi"] = &model.User{
		ID: "existing-heidi", Username: "heidi", URI: &uri, LastFetchedAt: &old,
	}
	// 取り込み以前に作られた profile 行 (追加項目が空)。
	desc := "bio"
	repo.Profiles["existing-heidi"] = &model.UserProfile{
		UserID: "existing-heidi", Description: &desc,
	}
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://mstdn.example/users/heidi",
		"type": "Person",
		"preferredUsername": "heidi",
		"inbox": "https://mstdn.example/users/heidi/inbox",
		"vcard:Address": "Osaka",
		"vcard:bday": "2001-12-24",
		"attachment": [{"type":"PropertyValue","name":"Site","value":"example.org"}],
		"publicKey": {"publicKeyPem": "REFRESHED"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	_, err := r.ResolveActor(uri)
	require.NoError(t, err)

	p := repo.Profiles["existing-heidi"]
	require.NotNil(t, p.Location)
	assert.Equal(t, "Osaka", *p.Location)
	require.NotNil(t, p.Birthday)
	assert.Equal(t, "2001-12-24", *p.Birthday)
	var fields []struct{ Name, Value string }
	require.NoError(t, json.Unmarshal(p.Fields, &fields))
	require.Len(t, fields, 1)
	assert.Equal(t, "Site", fields[0].Name)
}

// TestResolveActor_TTLRefreshConvertsHTMLDescription guards #1140 on the
// refresh path: 既存 remote actor が 旧 mk-go (生 HTML 保存) で取り込まれて
// いた場合、次の TTL refresh で MFM 化されて natural healing する。本 PR で
// fix した extractRemoteDescription は initial insert + refresh 両方から
// 呼ばれるので、symmetry で動作するが、明示的な regression guard としても
// refresh path を直接 cover する。
func TestResolveActor_TTLRefreshConvertsHTMLDescription(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://mstdn.example/users/bob"
	old := time.Now().Add(-48 * time.Hour)
	repo.Users["existing-bob"] = &model.User{
		ID:            "existing-bob",
		Username:      "bob",
		URI:           &uri,
		LastFetchedAt: &old,
	}
	// 既存 row には旧 mk-go 由来の生 HTML が入っている state を再現。
	staleDesc := "<p>old html bio</p>"
	repo.Profiles["existing-bob"] = &model.UserProfile{
		UserID:      "existing-bob",
		Description: &staleDesc,
	}
	updated := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://mstdn.example/users/bob",
		"type": "Person",
		"preferredUsername": "bob",
		"inbox": "https://mstdn.example/users/bob/inbox",
		"summary": "<p>fresh html bio from Mastodon</p>",
		"publicKey": {"publicKeyPem": "REFRESHED"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(updated)}, idGen)
	_, err := r.ResolveActor(uri)
	require.NoError(t, err)
	got := repo.Profiles["existing-bob"].Description
	require.NotNil(t, got)
	assert.Equal(t, "fresh html bio from Mastodon", *got,
		"refresh path は extractRemoteDescription 経由なので MFM 変換が効く")
	assert.NotContains(t, *got, "<p>", "natural healing で生 HTML が残らない")
}

// 既存 user で profile 行が無い (= 本 fix 以前に取り込まれた remote user) は
// refresh 経路で back-fill される (#1022)。production の漸進的な救済経路。
func TestResolveActor_TTLRefreshBackfillsProfile(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://remote.example/users/alice"
	old := time.Now().Add(-48 * time.Hour)
	host := "remote.example"
	repo.Users["existing"] = &model.User{
		ID:            "existing",
		Username:      "alice",
		Host:          &host,
		URI:           &uri,
		LastFetchedAt: &old,
	}
	// profile 行は意図的に未作成
	updated := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"summary": "back-filled bio",
		"publicKey": {"publicKeyPem": "REFRESHED"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(updated)}, idGen)
	_, err := r.ResolveActor(uri)
	require.NoError(t, err)
	profile, err := repo.FindProfileByUserID("existing")
	require.NoError(t, err, "back-fill で profile 行が作成される")
	require.NotNil(t, profile.Description)
	assert.Equal(t, "back-filled bio", *profile.Description)
}

func TestResolveActor_TTLRefreshUpdatesIconBanner(t *testing.T) {
	// refresh 時にリモートの icon / image が更新されていれば反映する。
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://remote.example/users/alice"
	old := time.Now().Add(-48 * time.Hour)
	oldAvatar := "https://remote.example/old-avatar.png"
	oldBanner := "https://remote.example/old-banner.png"
	repo.Users["existing"] = &model.User{
		ID:            "existing",
		Username:      "alice",
		URI:           &uri,
		AvatarURL:     &oldAvatar,
		BannerURL:     &oldBanner,
		LastFetchedAt: &old,
	}
	updated := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"icon": {"type": "Image", "url": "https://remote.example/new-avatar.png"},
		"image": {"type": "Image", "url": "https://remote.example/new-banner.png"},
		"publicKey": {"publicKeyPem": "REFRESHED"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(updated)}, idGen)
	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	require.NotNil(t, user.AvatarURL)
	assert.Equal(t, "https://remote.example/new-avatar.png", *user.AvatarURL)
	require.NotNil(t, user.BannerURL)
	assert.Equal(t, "https://remote.example/new-banner.png", *user.BannerURL)
}

func TestResolveActor_TTLRefreshPreservesIconWhenOmitted(t *testing.T) {
	// refresh 先の actor から icon が欠落していたら既存値を保持する (削除は追わない)。
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://remote.example/users/alice"
	old := time.Now().Add(-48 * time.Hour)
	oldAvatar := "https://remote.example/keep-avatar.png"
	repo.Users["existing"] = &model.User{
		ID:            "existing",
		Username:      "alice",
		URI:           &uri,
		AvatarURL:     &oldAvatar,
		LastFetchedAt: &old,
	}
	// icon / image を含まない更新 actor
	updated := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"publicKey": {"publicKeyPem": "REFRESHED"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(updated)}, idGen)
	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	require.NotNil(t, user.AvatarURL)
	assert.Equal(t, "https://remote.example/keep-avatar.png", *user.AvatarURL)
}

func TestResolveActor_TTLRefreshUpdatesIsLocked(t *testing.T) {
	// TTL refresh でリモートが manuallyApprovesFollowers を true→false or
	// false→true に切り替えたときにローカルの IsLocked も追従する。
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://remote.example/users/alice"
	old := time.Now().Add(-48 * time.Hour)
	repo.Users["existing"] = &model.User{
		ID:            "existing",
		Username:      "alice",
		URI:           &uri,
		IsLocked:      false,
		LastFetchedAt: &old,
	}
	updated := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"manuallyApprovesFollowers": true,
		"publicKey": {"publicKeyPem": "REFRESHED"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(updated)}, idGen)
	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	assert.True(t, user.IsLocked, "IsLocked should flip to true on refresh when remote opts in")
}

// TTL refresh で降ってきた name も truncate / NUL 除去を通す。作成経路だけ
// 直すと、既存ユーザーの refresh で **user.name の update が落ちて refresh
// 全体が失敗する** (#2662)。
//
// truncate と NUL 除去は**別のケースで**確かめる。300 文字 + NUL を 1 つの
// 入力にすると truncate が NUL ごと切り落とすので、NUL 除去を外しても
// アサーションが通ってしまう (真空になる)。
func TestResolveActor_TTLRefreshNormalizesName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		wantLen int
	}{
		{"truncates", strings.Repeat("z", 300), 128},
		{"strips NUL", "Al" + `\u0000` + "ice", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			noteRepo := testutil.NewMockNoteRepository()
			urls := activitypub.NewURLBuilder("https://example.com")
			idGen, _ := id.NewGenerator("aidx")
			uri := "https://remote.example/users/alice"
			old := time.Now().Add(-48 * time.Hour)
			original := "Original"
			repo.Users["existing"] = &model.User{
				ID:            "existing",
				Username:      "alice",
				URI:           &uri,
				Name:          &original,
				LastFetchedAt: &old,
			}
			updated := `{ "@context": "https://www.w3.org/ns/activitystreams",
				"id": "https://remote.example/users/alice",
				"type": "Person",
				"preferredUsername": "alice",
				"inbox": "https://remote.example/users/alice/inbox",
				"name": "` + tc.in + `",
				"publicKey": {"publicKeyPem": "REFRESHED"}
			}`
			r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(updated)}, idGen)
			user, err := r.ResolveActor(uri)
			require.NoError(t, err)
			require.NotNil(t, user.Name)
			assert.Equal(t, tc.wantLen, len([]rune(*user.Name)))
			assert.NotContains(t, *user.Name, "\u0000", "NUL は残さない")
		})
	}
}

func TestResolveActor_NoRefreshWhenFresh(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://remote.example/users/alice"
	// LastFetchedAt が十分新しいので refresh は走らない (refreshActor は呼ばれない)
	now := time.Now()
	originalName := "Original"
	repo.Users["existing"] = &model.User{
		ID:            "existing",
		Username:      "alice",
		URI:           &uri,
		Name:          &originalName,
		LastFetchedAt: &now,
	}
	// fetcher が何を返しても name は更新されないはず
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	// 公開鍵キャッシュは空 → refreshPublicKey 経路を通る
	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	require.NotNil(t, user.Name)
	assert.Equal(t, "Original", *user.Name)

	// publicKey は in-memory にキャッシュされたはず
	pem, err := r.PublicKeyForActor("existing")
	require.NoError(t, err)
	assert.Contains(t, pem, "FAKE")
}

func TestResolveActor_TTLRefreshFetchError(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://remote.example/users/alice"
	old := time.Now().Add(-48 * time.Hour)
	originalName := "Original"
	repo.Users["existing"] = &model.User{
		ID:            "existing",
		Username:      "alice",
		URI:           &uri,
		Name:          &originalName,
		LastFetchedAt: &old,
	}
	// fetcher エラーでも refresh はベストエフォートなので既存 user を返す
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{err: errors.New("net down")}, idGen)
	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	assert.Equal(t, "Original", *user.Name)
}

func TestPublicKeyForActor_DBFallback(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	pkRepo := testutil.NewMockUserPublickeyRepository()
	r.SetPublickeyRepo(pkRepo)

	// ResolveActorで公開鍵をin-memory + DBにキャッシュ
	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)

	// DBに永続化されていることを確認
	require.Len(t, pkRepo.Keys, 1)
	pk := pkRepo.Keys[user.ID]
	require.NotNil(t, pk)
	assert.Equal(t, "https://remote.example/users/alice#main-key", pk.KeyID)
	assert.Contains(t, pk.KeyPEM, "FAKE")

	// in-memoryキャッシュを期限切れにする
	r.SetClock(func() time.Time { return time.Now().Add(48 * time.Hour) })

	// DBフォールバックで復元できることを確認
	pem, err := r.PublicKeyForActor(user.ID)
	require.NoError(t, err)
	assert.Contains(t, pem, "FAKE")
}

func TestPublicKeyForActor_DBFallback_NilRepo(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	// publickeyRepo未設定

	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)

	// in-memoryキャッシュを期限切れにする
	r.SetClock(func() time.Time { return time.Now().Add(48 * time.Hour) })

	// DB無しなのでmiss
	_, err = r.PublicKeyForActor(user.ID)
	assert.Error(t, err)
}

func TestPublicKeyForActor_TTLExpiry(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	// 解決して publicKey をキャッシュする (現在時刻で fetched 扱い)
	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	pem, err := r.PublicKeyForActor(user.ID)
	require.NoError(t, err)
	assert.Contains(t, pem, "FAKE")

	// 時計を進める → expired として miss するはず
	r.SetClock(func() time.Time { return time.Now().Add(48 * time.Hour) })
	_, err = r.PublicKeyForActor(user.ID)
	assert.Error(t, err)
}

func TestResolver_SetClockNil(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	// nil を渡しても panic / 変更なし
	r.SetClock(nil)
	// 直後に呼んでもデフォルト clock のまま動く
	_, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
}

// stubInstanceTracker collects host registrations for assertions.
type stubInstanceTracker struct {
	hosts []string
}

func (s *stubInstanceTracker) RegisterFromHost(host string) (*model.Instance, error) {
	s.hosts = append(s.hosts, host)
	return nil, nil
}

func TestResolveActor_NotifiesInstanceTrackerOnCreate(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	tracker := &stubInstanceTracker{}
	r.SetInstanceTracker(tracker)

	_, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	require.Len(t, tracker.hosts, 1)
	assert.Equal(t, "remote.example", tracker.hosts[0])
}

func TestResolveActor_NotifiesInstanceTrackerOnRefresh(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://remote.example/users/alice"
	host := "remote.example"
	old := time.Now().Add(-48 * time.Hour)
	repo.Users["existing"] = &model.User{
		ID:            "existing",
		Username:      "alice",
		URI:           &uri,
		Host:          &host,
		LastFetchedAt: &old,
	}
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	tracker := &stubInstanceTracker{}
	r.SetInstanceTracker(tracker)

	_, err := r.ResolveActor(uri)
	require.NoError(t, err)
	require.Len(t, tracker.hosts, 1)
	assert.Equal(t, "remote.example", tracker.hosts[0])
}

func TestResolveActor_NoTrackerNoOp(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	r.SetInstanceTracker(nil)
	_, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
}

// stubChartHook captures chart hook fires from the federation resolver.
type stubChartHook struct {
	users []string // user IDs
}

func (s *stubChartHook) OnRemoteUserCreated(u *model.User) {
	s.users = append(s.users, u.ID)
}

func TestResolveActor_ChartHookFiresOnNewUser(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	hook := &stubChartHook{}
	r.SetChartHook(hook)

	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	require.Len(t, hook.users, 1)
	assert.Equal(t, user.ID, hook.users[0])
}

// stubHashtagHook captures hashtag hook fires from the federation resolver.
// #680: IngestNote / UpdateRemoteNote が note.Tags 非空時に呼ぶことを保証する。
type stubHashtagHook struct {
	calls        []hashtagHookCall
	usertagCalls []usertagHookCall
}

type hashtagHookCall struct {
	noteID   string
	authorID string
	tags     []string
	isLocal  bool
}

func (s *stubHashtagHook) OnNoteCreated(n *model.Note, a *model.User) {
	s.calls = append(s.calls, hashtagHookCall{
		noteID:   n.ID,
		authorID: a.ID,
		tags:     []string(n.Tags),
		isLocal:  a.IsLocal(),
	})
}

type usertagHookCall struct {
	userID  string
	isLocal bool
	oldTags []string
	newTags []string
}

func (s *stubHashtagHook) UpdateUsertags(userID string, isLocal bool, oldTags, newTags []string) {
	s.usertagCalls = append(s.usertagCalls, usertagHookCall{
		userID:  userID,
		isLocal: isLocal,
		oldTags: oldTags,
		newTags: newTags,
	})
}

// remote actor の新規取り込みで usertag 集計 hook が発火する (#1362)。
// old=nil / new=正規化済み tags / isLocal=false。
func TestResolveActor_UsertagHookFiresOnNewUser(t *testing.T) {
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/htagger",
		"type": "Person",
		"preferredUsername": "htagger",
		"inbox": "https://remote.example/users/htagger/inbox",
		"tag": [{"type": "Hashtag", "name": "#Golang"}],
		"publicKey": {"publicKeyPem": "FAKE"}
	}`
	r, _ := newResolver(t, body, nil)
	hook := &stubHashtagHook{}
	r.SetHashtagHook(hook)

	_, err := r.ResolveActor("https://remote.example/users/htagger")
	require.NoError(t, err)
	require.Len(t, hook.usertagCalls, 1)
	assert.False(t, hook.usertagCalls[0].isLocal, "remote → isLocal=false")
	assert.Nil(t, hook.usertagCalls[0].oldTags, "新規取り込みは old=nil")
	assert.Equal(t, []string{"golang"}, hook.usertagCalls[0].newTags)
}

// actor 再取得 (refreshActor) でも usertag 集計 hook が発火し、old/new tags が
// 渡る (#1362)。
func TestResolveActor_UsertagHookFiresOnRefresh(t *testing.T) {
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"tag": [{"type": "Hashtag", "name": "#New"}],
		"publicKey": {"publicKeyPem": "FAKE"}
	}`
	r, repo := newResolver(t, body, nil)
	hook := &stubHashtagHook{}
	r.SetHashtagHook(hook)
	uri := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["existing"] = &model.User{
		ID:       "existing",
		Username: "alice",
		URI:      &uri,
		Host:     &host,
		Tags:     []string{"old"},
	}
	_, err := r.ResolveActor(uri)
	require.NoError(t, err)
	require.Len(t, hook.usertagCalls, 1)
	assert.False(t, hook.usertagCalls[0].isLocal)
	assert.Equal(t, []string{"old"}, hook.usertagCalls[0].oldTags)
	assert.Equal(t, []string{"new"}, hook.usertagCalls[0].newTags)
}

// #1372: refreshActor で remote actor が hashtag を持たない場合、user.tags は
// 非nilの空配列 ('{}') に更新されなければならない。nil の model.StringArray は
// Updates() 経由で SQL NULL になり user.tags (NOT NULL) 制約に違反し、actor 更新
// (emojis / name / lastFetchedAt 等を含む atomic UPDATE) 全体が失敗する。
func TestResolveActor_RefreshClearsTagsToEmptyNotNull(t *testing.T) {
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"publicKey": {"publicKeyPem": "FAKE"}
	}`
	r, repo := newResolver(t, body, nil)
	uri := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["existing"] = &model.User{
		ID:       "existing",
		Username: "alice",
		URI:      &uri,
		Host:     &host,
		Tags:     []string{"old"},
	}
	_, err := r.ResolveActor(uri)
	require.NoError(t, err)
	require.NotNil(t, repo.Users["existing"].Tags, "tags は非nilの空配列でなければ SQL NULL で NOT NULL 違反 (#1372)")
	assert.Empty(t, []string(repo.Users["existing"].Tags))
}

func TestIngestNote_HashtagHookFiresOnRemoteIngest(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	hook := &stubHashtagHook{}
	r.SetHashtagHook(hook)

	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/hh1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "tagged #golang",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	note, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	require.Len(t, hook.calls, 1)
	assert.Equal(t, note.ID, hook.calls[0].noteID)
	assert.False(t, hook.calls[0].isLocal, "remote actor → isLocal=false")
	assert.Contains(t, hook.calls[0].tags, "golang")
}

func TestIngestNote_HashtagHookSkipsWhenNoTags(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	hook := &stubHashtagHook{}
	r.SetHashtagHook(hook)

	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/hh2",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "no tags here",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	_, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	assert.Empty(t, hook.calls, "hashtag 抽出が空なら hook は呼ばれない")
}

func TestUpdateRemoteNote_HashtagHookFiresOnTagsChange(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/hh3"
	host := "remote.example"
	noteRepo.Notes["hh3"] = &model.Note{
		ID: "hh3", URI: &uri, UserID: "alice-id", UserHost: &host,
		Tags: []string{"old"},
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
	hook := &stubHashtagHook{}
	r.SetHashtagHook(hook)

	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/hh3",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "edited and now tagged #news"
	}`
	_, err := r.UpdateRemoteNote([]byte(body), "")
	require.NoError(t, err)
	require.Len(t, hook.calls, 1)
	assert.Equal(t, "hh3", hook.calls[0].noteID)
	assert.Equal(t, "alice-id", hook.calls[0].authorID)
	assert.False(t, hook.calls[0].isLocal)
	assert.Contains(t, hook.calls[0].tags, "news")
}

func TestUpdateRemoteNote_HashtagHookSkipsWhenTagsUnchanged(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/hh4"
	host := "remote.example"
	noteRepo.Notes["hh4"] = &model.Note{
		ID: "hh4", URI: &uri, UserID: "alice-id", UserHost: &host,
		Tags: []string{"news"},
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
	hook := &stubHashtagHook{}
	r.SetHashtagHook(hook)

	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/hh4",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "edit body but tag unchanged #news"
	}`
	_, err := r.UpdateRemoteNote([]byte(body), "")
	require.NoError(t, err)
	assert.Empty(t, hook.calls, "tags が変化しなければ hook は呼ばれない")
}

func TestResolveActor_FreshButCacheMissFetchError(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://remote.example/users/alice"
	now := time.Now()
	repo.Users["existing"] = &model.User{
		ID:            "existing",
		Username:      "alice",
		URI:           &uri,
		LastFetchedAt: &now,
	}
	// publicKey は cache 空 → refreshPublicKey 経路に入るが fetcher はエラー
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{err: errors.New("net down")}, idGen)
	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	assert.Equal(t, "existing", user.ID)
	// fetch 失敗時もエラーは伝搬しない
	_, perr := r.PublicKeyForActor("existing")
	assert.Error(t, perr)
}

func TestResolver_SetActorTTL(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	// 0 / 負値は無視されデフォルト維持
	r.SetActorTTL(0)
	r.SetActorTTL(-1)
	r.SetActorTTL(time.Minute)

	// 1 分 TTL に設定し、LastFetchedAt が 5 分前のユーザーを refresh するか確認
	uri := "https://remote.example/users/alice"
	old := time.Now().Add(-5 * time.Minute)
	originalName := "Original"
	repo.Users["existing"] = &model.User{
		ID:            "existing",
		Username:      "alice",
		URI:           &uri,
		Name:          &originalName,
		LastFetchedAt: &old,
	}
	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	require.NotNil(t, user.Name)
	assert.Equal(t, "Alice", *user.Name) // sampleActor の name で上書き
}

func TestRefreshPublicKey_OnExistingUser_FetchError(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	uri := "https://remote.example/users/alice"
	repo.Users["existing"] = &model.User{ID: "existing", Username: "alice", URI: &uri}
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{err: errors.New("oops")}, idGen)
	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	assert.Equal(t, "existing", user.ID)
	// fetch失敗してもエラーは返さない
	_, err = r.PublicKeyForActor("existing")
	assert.Error(t, err)
}

// --- multi actor type support (#153) ---

// actorJSON renders a minimal actor JSON document with the given type.
func actorJSON(actorType string) string {
	return fmt.Sprintf(`{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/users/x",
		"type": %q,
		"preferredUsername": "x",
		"name": "X",
		"inbox": "https://remote.example/users/x/inbox",
		"endpoints": {"sharedInbox": "https://remote.example/inbox"},
		"publicKey": {
			"id": "https://remote.example/users/x#main-key",
			"owner": "https://remote.example/users/x",
			"publicKeyPem": "-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----"
		}
	}`, actorType)
}

func TestResolveActor_AllValidActorTypesAccepted(t *testing.T) {
	for _, typ := range activitypub.ValidActorTypes {
		t.Run(typ, func(t *testing.T) {
			r, repo := newResolver(t, actorJSON(typ), nil)
			user, err := r.ResolveActor("https://remote.example/users/x")
			require.NoError(t, err)
			assert.Equal(t, "x", user.Username)
			assert.Len(t, repo.Users, 1)
		})
	}
}

func TestResolveActor_InvalidActorTypeRejected(t *testing.T) {
	for _, typ := range []string{"Note", "Tombstone", "Article", ""} {
		t.Run(typ, func(t *testing.T) {
			r, _ := newResolver(t, actorJSON(typ), nil)
			_, err := r.ResolveActor("https://remote.example/users/x")
			require.ErrorIs(t, err, federation.ErrInvalidActor)
		})
	}
}

func TestResolveActor_ServiceTypeSetsIsBot(t *testing.T) {
	r, repo := newResolver(t, actorJSON("Service"), nil)
	user, err := r.ResolveActor("https://remote.example/users/x")
	require.NoError(t, err)
	assert.True(t, user.IsBot)
	// 永続化された user も isBot=true
	require.Len(t, repo.Users, 1)
	for _, u := range repo.Users {
		assert.True(t, u.IsBot)
	}
}

func TestResolveActor_ApplicationTypeSetsIsBot(t *testing.T) {
	r, _ := newResolver(t, actorJSON("Application"), nil)
	user, err := r.ResolveActor("https://remote.example/users/x")
	require.NoError(t, err)
	assert.True(t, user.IsBot)
}

func TestResolveActor_GroupTypeDoesNotSetIsBot(t *testing.T) {
	r, _ := newResolver(t, actorJSON("Group"), nil)
	user, err := r.ResolveActor("https://remote.example/users/x")
	require.NoError(t, err)
	assert.False(t, user.IsBot)
}

func TestResolveActor_OrganizationTypeDoesNotSetIsBot(t *testing.T) {
	r, _ := newResolver(t, actorJSON("Organization"), nil)
	user, err := r.ResolveActor("https://remote.example/users/x")
	require.NoError(t, err)
	assert.False(t, user.IsBot)
}

// #692: refresh で `_misskey_canChat: false` を受け取ったら chatScope を
// "none" に書き換える (DB / メモリ両方)。flag 欠落時は既存 chatScope を
// 保持する (連合先が一時的に field を消した場合の保護)。
func TestRefreshActor_UpdatesChatScopeFromCanChat(t *testing.T) {
	cases := []struct {
		name         string
		bodyFlag     string
		initialScope string
		want         string
	}{
		{"false_flag_sets_none", `, "_misskey_canChat": false`, "everyone", "none"},
		{"true_flag_sets_everyone", `, "_misskey_canChat": true`, "none", "everyone"},
		{"missing_keeps_existing", "", "everyone", "everyone"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			uri := "https://remote.example/users/x"
			// LastFetchedAt を TTL より十分過去にして shouldRefreshActor →
			// refreshActor 経路を強制発火させる (resolveActorOnce の cold path
			// 内で refresh が実際に走らないと canChat の取込みが行われない)。
			stale := time.Now().Add(-48 * time.Hour)
			repo.Users["existing"] = &model.User{
				ID:            "existing",
				Username:      "x",
				URI:           &uri,
				ChatScope:     tc.initialScope,
				LastFetchedAt: &stale,
			}
			body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
				"id": "https://remote.example/users/x",
				"type": "Person",
				"preferredUsername": "x",
				"inbox": "https://remote.example/users/x/inbox",
				"publicKey": {"publicKeyPem": "FAKE"}` + tc.bodyFlag + `
			}`
			noteRepo := testutil.NewMockNoteRepository()
			urls := activitypub.NewURLBuilder("https://example.com")
			idGen, _ := id.NewGenerator("aidx")
			r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
			user, err := r.ResolveActor(uri)
			require.NoError(t, err)
			assert.Equal(t, tc.want, user.ChatScope)
		})
	}
}

func TestRefreshActor_UpdatesIsBotOnTypeChange(t *testing.T) {
	// 既存 Person ユーザーが Service に切り替わった場合、refresh で IsBot=true
	// に追従する。
	repo := testutil.NewMockUserRepository()
	uri := "https://remote.example/users/x"
	stale := time.Now().Add(-48 * time.Hour) // TTL超過
	repo.Users["existing"] = &model.User{
		ID:            "existing",
		Username:      "x",
		URI:           &uri,
		IsBot:         false,
		LastFetchedAt: &stale,
	}
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(actorJSON("Service"))}, idGen)

	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	assert.True(t, user.IsBot)
}

// --- extractEmojiTags / upsertEmojis (#330) ----------------------------------

func TestExtractEmojiTags(t *testing.T) {
	t.Run("valid emoji tag", func(t *testing.T) {
		tags := []any{
			map[string]any{
				"type": "Emoji",
				"name": ":blobcat:",
				"icon": map[string]any{
					"type": "Image",
					"url":  "https://remote.example/emojis/blobcat.webp",
				},
				"id":      "https://remote.example/emojis/blobcat",
				"updated": "2025-01-01T00:00:00Z",
			},
		}
		got := federation.ExtractEmojiTags(tags)
		require.Len(t, got, 1)
		assert.Equal(t, "Emoji", got[0].Type)
		assert.Equal(t, ":blobcat:", got[0].Name)
		assert.Equal(t, "https://remote.example/emojis/blobcat.webp", got[0].Icon.URL.String())
		assert.Equal(t, "https://remote.example/emojis/blobcat", got[0].ID)
		assert.Equal(t, "2025-01-01T00:00:00Z", got[0].Updated)
		// #731: AP tag に `_misskey_license` が無いケースは License = nil
		assert.Nil(t, got[0].License, "no license wrapper → nil")
	})

	t.Run("emoji tag with _misskey_license", func(t *testing.T) {
		// #731: upstream renderEmoji は `_misskey_license: {freeText: ...}` で
		// license を federate する。mk-go の extractEmojiTags はこれを拾って
		// EmojiTag.License に non-nil の wrapper を入れる。
		tags := []any{
			map[string]any{
				"type": "Emoji",
				"name": ":licensed:",
				"icon": map[string]any{
					"type": "Image",
					"url":  "https://remote.example/emojis/licensed.webp",
				},
				"_misskey_license": map[string]any{
					"freeText": "CC-BY-4.0",
				},
			},
		}
		got := federation.ExtractEmojiTags(tags)
		require.Len(t, got, 1)
		require.NotNil(t, got[0].License)
		require.NotNil(t, got[0].License.FreeText)
		assert.Equal(t, "CC-BY-4.0", *got[0].License.FreeText)
	})

	t.Run("emoji tag _misskey_license null freeText keeps FreeText nil", func(t *testing.T) {
		// upstream renderEmoji は emoji.license=null でも wrapper を出す。
		// 3 状態を区別する設計 (#731):
		//   - wrapper 欠落 → License = nil ("license 情報が federate されて
		//     いない", 既存値温存)
		//   - wrapper あり + freeText=null → FreeText = nil ("license は明示
		//     的に未設定", NULL 上書き OK)
		//   - wrapper あり + freeText=string → 具体値
		// JSON null は string assertion が失敗するので FreeText は nil のまま。
		tags := []any{
			map[string]any{
				"type": "Emoji",
				"name": ":no_license:",
				"icon": map[string]any{
					"type": "Image",
					"url":  "https://remote.example/emojis/no_license.webp",
				},
				"_misskey_license": map[string]any{
					"freeText": nil,
				},
			},
		}
		got := federation.ExtractEmojiTags(tags)
		require.Len(t, got, 1)
		require.NotNil(t, got[0].License, "wrapper present even when freeText is null")
		assert.Nil(t, got[0].License.FreeText, "JSON null freeText → FreeText nil (explicit no license)")
	})

	t.Run("non-emoji tags filtered out", func(t *testing.T) {
		tags := []any{
			map[string]any{
				"type": "Mention",
				"name": "@alice",
				"href": "https://remote.example/users/alice",
			},
			map[string]any{
				"type": "Hashtag",
				"name": "#golang",
				"href": "https://remote.example/tags/golang",
			},
			map[string]any{
				"type": "Emoji",
				"name": ":valid:",
				"icon": map[string]any{"url": "https://remote.example/emojis/valid.png"},
			},
		}
		got := federation.ExtractEmojiTags(tags)
		require.Len(t, got, 1)
		assert.Equal(t, ":valid:", got[0].Name)
	})

	t.Run("missing icon URL skipped", func(t *testing.T) {
		tags := []any{
			map[string]any{
				"type": "Emoji",
				"name": ":noicon:",
				// icon未設定
			},
			map[string]any{
				"type": "Emoji",
				"name": ":emptyurl:",
				"icon": map[string]any{"url": ""},
			},
		}
		got := federation.ExtractEmojiTags(tags)
		assert.Len(t, got, 0)
	})

	t.Run("empty name skipped", func(t *testing.T) {
		tags := []any{
			map[string]any{
				"type": "Emoji",
				"name": "",
				"icon": map[string]any{"url": "https://remote.example/e.png"},
			},
		}
		got := federation.ExtractEmojiTags(tags)
		assert.Len(t, got, 0)
	})

	t.Run("nil and non-map entries skipped", func(t *testing.T) {
		tags := []any{
			nil,
			"string entry",
			42,
			map[string]any{
				"type": "Emoji",
				"name": ":ok:",
				"icon": map[string]any{"url": "https://remote.example/ok.png"},
			},
		}
		got := federation.ExtractEmojiTags(tags)
		require.Len(t, got, 1)
		assert.Equal(t, ":ok:", got[0].Name)
	})

	t.Run("empty tags returns nil", func(t *testing.T) {
		got := federation.ExtractEmojiTags(nil)
		assert.Nil(t, got)
		got = federation.ExtractEmojiTags([]any{})
		assert.Nil(t, got)
	})

	t.Run("multiple valid emojis", func(t *testing.T) {
		tags := []any{
			map[string]any{
				"type": "Emoji",
				"name": ":cat:",
				"icon": map[string]any{"url": "https://remote.example/cat.png"},
			},
			map[string]any{
				"type": "Emoji",
				"name": ":dog:",
				"icon": map[string]any{"url": "https://remote.example/dog.png"},
				"id":   "https://remote.example/emojis/dog",
			},
		}
		got := federation.ExtractEmojiTags(tags)
		require.Len(t, got, 2)
		assert.Equal(t, ":cat:", got[0].Name)
		assert.Equal(t, ":dog:", got[1].Name)
	})
}

func TestUpsertEmojis(t *testing.T) {
	t.Run("creates new emoji", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		emojiRepo := testutil.NewMockEmojiRepository()
		r.SetEmojiRepo(emojiRepo)

		tags := []activitypub.EmojiTag{
			{
				Type: "Emoji",
				Name: ":blobcat:",
				Icon: activitypub.Image{Type: "Image", URL: "https://remote.example/blobcat.webp"},
				ID:   "https://remote.example/emojis/blobcat",
			},
		}
		names := r.UpsertEmojis(tags, "remote.example")
		assert.Equal(t, []string{"blobcat"}, []string(names))

		// emojiRepoにcreateされたことを確認
		e, err := emojiRepo.FindByNameAndHost("blobcat", strPtr("remote.example"))
		require.NoError(t, err)
		assert.Equal(t, "blobcat", e.Name)
		assert.Equal(t, "https://remote.example/blobcat.webp", e.OriginalURL)
		assert.Equal(t, "https://remote.example/blobcat.webp", e.PublicURL)
		require.NotNil(t, e.URI)
		assert.Equal(t, "https://remote.example/emojis/blobcat", *e.URI)
		require.NotNil(t, e.Host)
		assert.Equal(t, "remote.example", *e.Host)
	})

	t.Run("populates license from _misskey_license", func(t *testing.T) {
		// #731: upstream Misskey TS が AP `_misskey_license.freeText` で
		// federate する license を新規 emoji 取り込み時に保存する。
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		emojiRepo := testutil.NewMockEmojiRepository()
		r.SetEmojiRepo(emojiRepo)

		licenseText := "CC-BY-4.0"
		tags := []activitypub.EmojiTag{
			{
				Type:    "Emoji",
				Name:    ":licensed:",
				Icon:    activitypub.Image{Type: "Image", URL: "https://remote.example/licensed.webp"},
				License: &activitypub.MisskeyLicense{FreeText: &licenseText},
			},
		}
		r.UpsertEmojis(tags, "remote.example")

		e, err := emojiRepo.FindByNameAndHost("licensed", strPtr("remote.example"))
		require.NoError(t, err)
		require.NotNil(t, e.License, "license should be populated from AP _misskey_license")
		assert.Equal(t, "CC-BY-4.0", *e.License)
	})

	t.Run("updates existing license when AP tag has different value", func(t *testing.T) {
		// #731: AP tag に新しい license が来たら既存値を上書きする経路。
		// pointerStringsEqual の "1 つ nil / 値変化" 分岐をカバー。
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		emojiRepo := testutil.NewMockEmojiRepository()
		r.SetEmojiRepo(emojiRepo)

		host := "remote.example"
		oldLicense := "old"
		emojiRepo.Emojis["chg@remote.example"] = &model.Emoji{
			ID: "ex", Name: "chg", Host: &host,
			OriginalURL: "https://remote.example/chg.webp",
			PublicURL:   "https://remote.example/chg.webp",
			License:     &oldLicense,
		}

		newLicense := "new-license"
		tags := []activitypub.EmojiTag{
			{
				Type:    "Emoji",
				Name:    ":chg:",
				Icon:    activitypub.Image{Type: "Image", URL: "https://remote.example/chg.webp"},
				License: &activitypub.MisskeyLicense{FreeText: &newLicense},
			},
		}
		r.UpsertEmojis(tags, host)

		e, err := emojiRepo.FindByNameAndHost("chg", &host)
		require.NoError(t, err)
		require.NotNil(t, e.License)
		assert.Equal(t, "new-license", *e.License, "license should be updated to new value")
	})

	t.Run("no-op when license unchanged", func(t *testing.T) {
		// #731: 同じ license が再 federate されても update 不要。
		// pointerStringsEqual が true を返す経路をカバー。
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		emojiRepo := testutil.NewMockEmojiRepository()
		r.SetEmojiRepo(emojiRepo)

		host := "remote.example"
		same := "same-license"
		emojiRepo.Emojis["nop@remote.example"] = &model.Emoji{
			ID: "ex", Name: "nop", Host: &host,
			OriginalURL: "https://remote.example/nop.webp",
			PublicURL:   "https://remote.example/nop.webp",
			License:     &same,
		}

		// 同じ license 値で再 federate
		sameAgain := "same-license"
		tags := []activitypub.EmojiTag{
			{
				Type:    "Emoji",
				Name:    ":nop:",
				Icon:    activitypub.Image{Type: "Image", URL: "https://remote.example/nop.webp"},
				License: &activitypub.MisskeyLicense{FreeText: &sameAgain},
			},
		}
		r.UpsertEmojis(tags, host)

		e, err := emojiRepo.FindByNameAndHost("nop", &host)
		require.NoError(t, err)
		require.NotNil(t, e.License)
		assert.Equal(t, "same-license", *e.License, "value unchanged")
	})

	t.Run("clears existing license when AP tag explicitly federates null freeText", func(t *testing.T) {
		// #731: wrapper あり + freeText=null は "license を明示的に解除" の
		// シグナル。既存 license を NULL で上書きする経路。pointerStringsEqual
		// の "片方 nil" 分岐をカバー。
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		emojiRepo := testutil.NewMockEmojiRepository()
		r.SetEmojiRepo(emojiRepo)

		host := "remote.example"
		oldLicense := "to-clear"
		emojiRepo.Emojis["clr@remote.example"] = &model.Emoji{
			ID: "ex", Name: "clr", Host: &host,
			OriginalURL: "https://remote.example/clr.webp",
			PublicURL:   "https://remote.example/clr.webp",
			License:     &oldLicense,
		}

		// freeText=nil (= JSON null 受信) で wrapper あり
		tags := []activitypub.EmojiTag{
			{
				Type:    "Emoji",
				Name:    ":clr:",
				Icon:    activitypub.Image{Type: "Image", URL: "https://remote.example/clr.webp"},
				License: &activitypub.MisskeyLicense{FreeText: nil},
			},
		}
		r.UpsertEmojis(tags, host)

		e, err := emojiRepo.FindByNameAndHost("clr", &host)
		require.NoError(t, err)
		assert.Nil(t, e.License, "explicit null freeText should clear existing license")
	})

	t.Run("preserves existing license when AP tag has no _misskey_license wrapper", func(t *testing.T) {
		// #731: AP tag の `_misskey_license` が欠落している場合、既存 emoji の
		// license は温存する (連合先が一時的に license export を停止しても
		// 上書きしない)。
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		emojiRepo := testutil.NewMockEmojiRepository()
		r.SetEmojiRepo(emojiRepo)

		host := "remote.example"
		old := "old-license"
		emojiRepo.Emojis["preserved@remote.example"] = &model.Emoji{
			ID: "existing", Name: "preserved", Host: &host,
			OriginalURL: "https://remote.example/preserved.webp",
			PublicURL:   "https://remote.example/preserved.webp",
			License:     &old,
		}

		// License: nil の AP tag (= wrapper 欠落)
		tags := []activitypub.EmojiTag{
			{
				Type: "Emoji",
				Name: ":preserved:",
				Icon: activitypub.Image{Type: "Image", URL: "https://remote.example/preserved.webp"},
			},
		}
		r.UpsertEmojis(tags, host)

		e, err := emojiRepo.FindByNameAndHost("preserved", &host)
		require.NoError(t, err)
		require.NotNil(t, e.License)
		assert.Equal(t, "old-license", *e.License, "existing license must be preserved when wrapper is missing")
	})

	t.Run("updates existing emoji URL", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		emojiRepo := testutil.NewMockEmojiRepository()
		r.SetEmojiRepo(emojiRepo)

		host := "remote.example"
		// 既存の絵文字をrepoに配置
		emojiRepo.Emojis["blobcat@remote.example"] = &model.Emoji{
			ID:          "existing-id",
			Name:        "blobcat",
			Host:        &host,
			OriginalURL: "https://remote.example/old-blobcat.webp",
			PublicURL:   "https://remote.example/old-blobcat.webp",
		}

		tags := []activitypub.EmojiTag{
			{
				Type: "Emoji",
				Name: ":blobcat:",
				Icon: activitypub.Image{URL: "https://remote.example/new-blobcat.webp"},
				ID:   "https://remote.example/emojis/blobcat",
			},
		}
		names := r.UpsertEmojis(tags, "remote.example")
		assert.Equal(t, []string{"blobcat"}, []string(names))

		// URLが更新されたことを確認
		e, err := emojiRepo.FindByNameAndHost("blobcat", &host)
		require.NoError(t, err)
		assert.Equal(t, "https://remote.example/new-blobcat.webp", e.OriginalURL)
	})

	t.Run("nil emojiRepo returns empty array", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		// emojiRepo未設定

		tags := []activitypub.EmojiTag{
			{Name: ":x:", Icon: activitypub.Image{URL: "https://example.com/x.png"}},
		}
		names := r.UpsertEmojis(tags, "example.com")
		assert.Empty(t, names)
	})

	t.Run("empty tags returns empty array", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		emojiRepo := testutil.NewMockEmojiRepository()
		r.SetEmojiRepo(emojiRepo)

		names := r.UpsertEmojis(nil, "example.com")
		assert.Empty(t, names)
	})

	t.Run("colon-only name skipped", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		emojiRepo := testutil.NewMockEmojiRepository()
		r.SetEmojiRepo(emojiRepo)

		// "::" → Trim(":") → "" → スキップ
		tags := []activitypub.EmojiTag{
			{Name: "::", Icon: activitypub.Image{URL: "https://example.com/x.png"}},
		}
		names := r.UpsertEmojis(tags, "example.com")
		assert.Empty(t, names)
	})

	t.Run("emoji without URI does not set URI field", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		emojiRepo := testutil.NewMockEmojiRepository()
		r.SetEmojiRepo(emojiRepo)

		tags := []activitypub.EmojiTag{
			{Name: ":nouri:", Icon: activitypub.Image{URL: "https://example.com/nouri.png"}},
		}
		names := r.UpsertEmojis(tags, "remote.example")
		assert.Equal(t, []string{"nouri"}, []string(names))

		e, err := emojiRepo.FindByNameAndHost("nouri", strPtr("remote.example"))
		require.NoError(t, err)
		assert.Nil(t, e.URI)
	})

	t.Run("duplicate names deduplicated", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		emojiRepo := testutil.NewMockEmojiRepository()
		r.SetEmojiRepo(emojiRepo)

		// 同名タグが複数 (リモート側で重複している場合)
		tags := []activitypub.EmojiTag{
			{Name: ":dup:", Icon: activitypub.Image{URL: "https://remote.example/a.png"}},
			{Name: ":dup:", Icon: activitypub.Image{URL: "https://remote.example/b.png"}},
		}
		names := r.UpsertEmojis(tags, "remote.example")
		assert.Equal(t, []string{"dup"}, []string(names), "重複nameは1件に集約される")
	})

	t.Run("create error excludes name from result", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		emojiRepo := testutil.NewMockEmojiRepository()
		emojiRepo.CreateErr = errors.New("simulated db error")
		r.SetEmojiRepo(emojiRepo)

		tags := []activitypub.EmojiTag{
			{Name: ":fail:", Icon: activitypub.Image{URL: "https://remote.example/fail.png"}},
		}
		names := r.UpsertEmojis(tags, "remote.example")
		assert.Empty(t, names, "create失敗時はnameを返さない")
	})

	t.Run("update error keeps name in result", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		emojiRepo := testutil.NewMockEmojiRepository()

		host := "remote.example"
		emojiRepo.Emojis["existing@remote.example"] = &model.Emoji{
			ID:          "existing-id",
			Name:        "existing",
			Host:        &host,
			OriginalURL: "https://remote.example/old.png",
			PublicURL:   "https://remote.example/old.png",
		}
		emojiRepo.UpdateErr = errors.New("simulated update error")
		r.SetEmojiRepo(emojiRepo)

		tags := []activitypub.EmojiTag{
			{Name: ":existing:", Icon: activitypub.Image{URL: "https://remote.example/new.png"}},
		}
		names := r.UpsertEmojis(tags, "remote.example")
		assert.Equal(t, []string{"existing"}, []string(names),
			"update失敗でも行は存在するためnameは返す")
	})

	t.Run("batch lookup handles multiple existing and new in one call", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		emojiRepo := testutil.NewMockEmojiRepository()
		r.SetEmojiRepo(emojiRepo)

		host := "remote.example"
		emojiRepo.Emojis["existing@remote.example"] = &model.Emoji{
			ID: "ex-id", Name: "existing", Host: &host,
			OriginalURL: "https://remote.example/existing.png",
			PublicURL:   "https://remote.example/existing.png",
		}

		tags := []activitypub.EmojiTag{
			{Name: ":existing:", Icon: activitypub.Image{URL: "https://remote.example/existing.png"}},
			{Name: ":newone:", Icon: activitypub.Image{URL: "https://remote.example/newone.png"}},
			{Name: ":another:", Icon: activitypub.Image{URL: "https://remote.example/another.png"}},
		}
		names := r.UpsertEmojis(tags, "remote.example")
		assert.ElementsMatch(t, []string{"existing", "newone", "another"}, []string(names))
		assert.Equal(t, []string{"existing", "newone", "another"}, []string(names),
			"順序は元のタグ順を維持")
	})
}

// sampleActorWithEmoji is a Person JSON with emoji tags in the tag array.
const sampleActorWithEmoji = `{ "@context": "https://www.w3.org/ns/activitystreams", 
	"id": "https://remote.example/users/alice",
	"type": "Person",
	"preferredUsername": "alice",
	"name": "Alice :blobcat:",
	"inbox": "https://remote.example/users/alice/inbox",
	"endpoints": {"sharedInbox": "https://remote.example/inbox"},
	"publicKey": {
		"id": "https://remote.example/users/alice#main-key",
		"owner": "https://remote.example/users/alice",
		"publicKeyPem": "-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----"
	},
	"tag": [
		{
			"type": "Emoji",
			"name": ":blobcat:",
			"icon": {"type": "Image", "url": "https://remote.example/emojis/blobcat.webp"},
			"id": "https://remote.example/emojis/blobcat"
		},
		{
			"type": "Mention",
			"name": "@bob",
			"href": "https://remote.example/users/bob"
		}
	]
}`

func TestResolveActor_EmojiTagExtraction(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActorWithEmoji)}, idGen)
	emojiRepo := testutil.NewMockEmojiRepository()
	r.SetEmojiRepo(emojiRepo)

	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)

	// user.Emojisに絵文字名 (コロンなし) が含まれることを確認
	require.Len(t, user.Emojis, 1)
	assert.Equal(t, "blobcat", user.Emojis[0])

	// emojiRepoに新規作成されたことを確認
	e, err := emojiRepo.FindByNameAndHost("blobcat", strPtr("remote.example"))
	require.NoError(t, err)
	assert.Equal(t, "blobcat", e.Name)
	assert.Equal(t, "https://remote.example/emojis/blobcat.webp", e.OriginalURL)
	require.NotNil(t, e.Host)
	assert.Equal(t, "remote.example", *e.Host)
}

func TestResolveActor_EmojiTagExtraction_NoEmojiRepo(t *testing.T) {
	// emojiRepoが未設定の場合、Emojisは空になるがエラーにはならない
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActorWithEmoji)}, idGen)
	// SetEmojiRepoを呼ばない

	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.Empty(t, user.Emojis)
}

func TestRefreshActor_EmojiTagExtraction(t *testing.T) {
	// TTL超過でrefreshが走る場合、Tag配列から絵文字が抽出されることを確認
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")

	uri := "https://remote.example/users/alice"
	host := "remote.example"
	stale := time.Now().Add(-48 * time.Hour)
	repo.Users["existing"] = &model.User{
		ID:            "existing",
		Username:      "alice",
		URI:           &uri,
		Host:          &host,
		LastFetchedAt: &stale,
	}

	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActorWithEmoji)}, idGen)
	emojiRepo := testutil.NewMockEmojiRepository()
	r.SetEmojiRepo(emojiRepo)

	user, err := r.ResolveActor(uri)
	require.NoError(t, err)

	// refreshActorでEmojisが更新されたことを確認
	require.Len(t, user.Emojis, 1)
	assert.Equal(t, "blobcat", user.Emojis[0])

	// emojiRepoに作成されたことを確認
	e, err := emojiRepo.FindByNameAndHost("blobcat", &host)
	require.NoError(t, err)
	assert.Equal(t, "blobcat", e.Name)
}

// sampleNoteWithEmoji is a Note JSON with emoji tags in the tag array.
const sampleNoteWithEmoji = `{ "@context": "https://www.w3.org/ns/activitystreams", 
	"id": "https://remote.example/notes/emoji1",
	"type": "Note",
	"attributedTo": "https://remote.example/users/alice",
	"content": "<p>Hello :blobcat: :partyparrot:</p>",
	"to": ["https://www.w3.org/ns/activitystreams#Public"],
	"tag": [
		{
			"type": "Emoji",
			"name": ":blobcat:",
			"icon": {"type": "Image", "url": "https://remote.example/emojis/blobcat.webp"},
			"id": "https://remote.example/emojis/blobcat"
		},
		{
			"type": "Emoji",
			"name": ":partyparrot:",
			"icon": {"type": "Image", "url": "https://remote.example/emojis/partyparrot.gif"},
			"id": "https://remote.example/emojis/partyparrot"
		},
		{
			"type": "Mention",
			"name": "@alice",
			"href": "https://remote.example/users/alice"
		}
	]
}`

func TestIngestNote_EmojiTagExtraction(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	// IngestNoteではまずNote JSONを処理してからactorを解決するためscriptedFetcherを使う
	// ただしIngestNoteは直接body []byteを受け取るので、actorの解決にだけfetcherが使われる
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActorWithEmoji)}, idGen)
	emojiRepo := testutil.NewMockEmojiRepository()
	r.SetEmojiRepo(emojiRepo)

	note, err := r.IngestNote([]byte(sampleNoteWithEmoji))
	require.NoError(t, err)

	// note.Emojisに絵文字名 (コロンなし) が含まれることを確認
	require.Len(t, note.Emojis, 2)
	assert.Contains(t, []string(note.Emojis), "blobcat")
	assert.Contains(t, []string(note.Emojis), "partyparrot")

	// emojiRepoに2つの絵文字が作成されたことを確認
	host := "remote.example"
	e1, err := emojiRepo.FindByNameAndHost("blobcat", &host)
	require.NoError(t, err)
	assert.Equal(t, "https://remote.example/emojis/blobcat.webp", e1.OriginalURL)

	e2, err := emojiRepo.FindByNameAndHost("partyparrot", &host)
	require.NoError(t, err)
	assert.Equal(t, "https://remote.example/emojis/partyparrot.gif", e2.OriginalURL)
}

func TestIngestNote_EmojiTagExtraction_NoEmojiRepo(t *testing.T) {
	// emojiRepoが未設定の場合でもIngestNoteは成功する (Emojisは空)
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	// SetEmojiRepoを呼ばない

	note, err := r.IngestNote([]byte(sampleNoteWithEmoji))
	require.NoError(t, err)
	assert.Empty(t, note.Emojis)
}

func TestUpdateRemoteNote_EmojiTagExtraction(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/emoji1"
	host := "remote.example"
	original := "original text"
	noteRepo.Notes["emoji1"] = &model.Note{
		ID: "emoji1", URI: &uri, UserID: "alice-id", UserHost: &host, Text: &original,
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
	emojiRepo := testutil.NewMockEmojiRepository()
	r.SetEmojiRepo(emojiRepo)

	updateBody := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/emoji1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "updated :blobcat:",
		"tag": [
			{
				"type": "Emoji",
				"name": ":blobcat:",
				"icon": {"type": "Image", "url": "https://remote.example/emojis/blobcat.webp"},
				"id": "https://remote.example/emojis/blobcat"
			}
		]
	}`)
	got, err := r.UpdateRemoteNote(updateBody, "")
	require.NoError(t, err)
	require.NotNil(t, got)

	// Emojisが更新されたことを確認
	require.Len(t, got.Emojis, 1)
	assert.Equal(t, "blobcat", got.Emojis[0])

	// emojiRepoに作成されたことを確認
	e, err := emojiRepo.FindByNameAndHost("blobcat", &host)
	require.NoError(t, err)
	assert.Equal(t, "blobcat", e.Name)
	assert.Equal(t, "https://remote.example/emojis/blobcat.webp", e.OriginalURL)
}

func TestUpdateRemoteNote_EmojiTagExtraction_ExistingEmoji(t *testing.T) {
	// 既存の絵文字がある場合、URLが変わっていればupdateされることを確認
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	uri := "https://remote.example/notes/emoji1"
	host := "remote.example"
	original := "original text"
	noteRepo.Notes["emoji1"] = &model.Note{
		ID: "emoji1", URI: &uri, UserID: "alice-id", UserHost: &host, Text: &original,
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
	emojiRepo := testutil.NewMockEmojiRepository()
	r.SetEmojiRepo(emojiRepo)

	// 既存の絵文字を配置
	emojiRepo.Emojis["blobcat@remote.example"] = &model.Emoji{
		ID:          "old-emoji-id",
		Name:        "blobcat",
		Host:        &host,
		OriginalURL: "https://remote.example/emojis/old-blobcat.webp",
		PublicURL:   "https://remote.example/emojis/old-blobcat.webp",
	}

	updateBody := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/emoji1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "updated :blobcat:",
		"tag": [
			{
				"type": "Emoji",
				"name": ":blobcat:",
				"icon": {"type": "Image", "url": "https://remote.example/emojis/new-blobcat.webp"},
				"id": "https://remote.example/emojis/blobcat"
			}
		]
	}`)
	got, err := r.UpdateRemoteNote(updateBody, "")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Emojis, 1)
	assert.Equal(t, "blobcat", got.Emojis[0])

	// URLが更新されたことを確認
	e, err := emojiRepo.FindByNameAndHost("blobcat", &host)
	require.NoError(t, err)
	assert.Equal(t, "https://remote.example/emojis/new-blobcat.webp", e.OriginalURL)
}

// strPtr is a helper that returns a pointer to its argument.
func strPtr(s string) *string { return &s }

// --- #378 attachment ingest --------------------------------------------------

// 単一 object の attachment を持つ Note も添付を取り込める。Mastodon など
// 1 件のときに配列で包まない実装があり、`[]any` 決め打ちだと **Note の
// unmarshal ごと失敗して取り込めなかった** (#2662)。
func TestNoteAttachment_SingleObject(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		expected int
	}{
		{"single object", `{"type":"Document","mediaType":"image/png","url":"https://r/a.png"}`, 1},
		{"array", `[{"type":"Document","url":"https://r/a.png"},{"type":"Image","url":"https://r/b.jpg"}]`, 2},
		{"absent", "", 0},
		{"null", "null", 0},
		{"unusable single object", `{"type":"Note","url":"https://r/x"}`, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"type":"Note","id":"https://r/notes/1","content":"hi"`
			if tc.raw != "" {
				body += `,"attachment":` + tc.raw
			}
			body += "}"

			var note activitypub.Note
			require.NoError(t, json.Unmarshal([]byte(body), &note), "Note 全体が読めること")
			assert.Equal(t, "hi", note.Content, "他の field を巻き込まない")
			assert.Len(t, federation.ExtractAttachments(note.Attachment, false), tc.expected)
		})
	}
}

func TestExtractAttachments(t *testing.T) {
	tests := []struct {
		name     string
		raw      []any
		expected int
	}{
		{
			name: "Document image is extracted",
			raw: []any{
				map[string]any{
					"type":      "Document",
					"mediaType": "image/png",
					"url":       "https://remote.example/files/cat.png",
					"name":      "cute cat",
					"sensitive": false,
				},
			},
			expected: 1,
		},
		{
			name: "Image / Audio / Video types accepted",
			raw: []any{
				map[string]any{"type": "Image", "url": "https://r/i.jpg"},
				map[string]any{"type": "Audio", "url": "https://r/a.mp3"},
				map[string]any{"type": "Video", "url": "https://r/v.mp4"},
			},
			expected: 3,
		},
		{
			name: "Unknown types skipped",
			raw: []any{
				map[string]any{"type": "Note", "url": "https://r/n"},
				map[string]any{"type": "Document", "url": "https://r/ok.png"},
			},
			expected: 1,
		},
		{
			name: "missing url skipped",
			raw: []any{
				map[string]any{"type": "Document"},
				map[string]any{"type": "Document", "url": ""},
			},
			expected: 0,
		},
		{
			name:     "empty input",
			raw:      nil,
			expected: 0,
		},
		{
			name: "non-map entries skipped",
			raw: []any{
				"https://r/string-not-object.png",
				42,
				map[string]any{"type": "Document", "url": "https://r/ok.png"},
			},
			expected: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := federation.ExtractAttachments(tt.raw, false)
			assert.Len(t, got, tt.expected)
		})
	}
}

// TestExtractAttachments_MetadataFields covers #460/#461: width / height /
// icon / _misskey_blurhash が remote から届いたときに Document に展開される
// ことを確認する。JSON unmarshal 後 width/height は float64 で来るので
// numberAsInt 経由で int に丸められる必要がある。
func TestExtractAttachments_MetadataFields(t *testing.T) {
	raw := []any{
		map[string]any{
			"type":              "Document",
			"mediaType":         "image/png",
			"url":               "https://r/cat.png",
			"width":             float64(640),
			"height":            float64(480),
			"_misskey_blurhash": "L6PZfSi_.AyE_3t7t7R**0o#DgR4",
			"icon": map[string]any{
				"type": "Image",
				"url":  "https://r/cat-thumb.png",
			},
		},
		map[string]any{
			// width/height/icon/blurhash いずれも欠落しても zero value で
			// 通過する (旧来 ingestion との互換)。
			"type": "Document",
			"url":  "https://r/no-meta.bin",
		},
	}
	got := federation.ExtractAttachments(raw, false)
	require.Len(t, got, 2)
	assert.Equal(t, 640, got[0].Width)
	assert.Equal(t, 480, got[0].Height)
	assert.Equal(t, "L6PZfSi_.AyE_3t7t7R**0o#DgR4", got[0].Blurhash)
	require.NotNil(t, got[0].Icon)
	assert.Equal(t, "https://r/cat-thumb.png", got[0].Icon.URL.String())
	// 欠落側
	assert.Equal(t, 0, got[1].Width)
	assert.Equal(t, 0, got[1].Height)
	assert.Empty(t, got[1].Blurhash)
	assert.Nil(t, got[1].Icon)
}

func TestUpsertAttachments(t *testing.T) {
	t.Run("creates new drive_file as link", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		drive := testutil.NewMockDriveFileRepository()
		r.SetDriveFileRepo(drive)

		userID := "u1"
		host := "remote.example"
		docs := []activitypub.Document{
			{Type: "Document", MediaType: "image/png", URL: "https://r/cat.png", Name: "cat", Sensitive: false},
			{Type: "Image", MediaType: "image/jpeg", URL: "https://r/dog.jpg", Sensitive: true},
		}
		ids := r.UpsertAttachments(docs, &userID, &host)
		require.Len(t, ids, 2)

		// 各 drive_file を引いて期待値を確認
		f1, err := drive.FindByURI("https://r/cat.png")
		require.NoError(t, err)
		assert.True(t, f1.IsLink, "remote attachment は link 形式")
		assert.Equal(t, "image/png", f1.Type)
		// name は URL の basename (upstream 一致)。AP の `name` は代替テキストなので
		// comment 側に入る (#2723)。
		assert.Equal(t, "cat.png", f1.Name)
		require.NotNil(t, f1.Comment)
		assert.Equal(t, "cat", *f1.Comment)
		require.NotNil(t, f1.UserHost)
		assert.Equal(t, "remote.example", *f1.UserHost)
		assert.Equal(t, "https://r/cat.png", f1.URL)

		f2, err := drive.FindByURI("https://r/dog.jpg")
		require.NoError(t, err)
		assert.True(t, f2.IsSensitive)
	})

	t.Run("dedup by URI: 既存 row を再利用する", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		drive := testutil.NewMockDriveFileRepository()
		r.SetDriveFileRepo(drive)

		uri := "https://r/dup.png"
		drive.Files["pre-existing"] = &model.DriveFile{
			ID: "pre-existing", URI: &uri, URL: uri, Type: "image/png", Name: "dup",
		}

		userID := "u1"
		host := "remote.example"
		ids := r.UpsertAttachments(
			[]activitypub.Document{{Type: "Document", URL: uri, Name: "dup"}},
			&userID, &host,
		)
		require.Len(t, ids, 1)
		assert.Equal(t, "pre-existing", ids[0], "既存 row の ID が返る")
		// drive_file が新規追加されていない
		assert.Len(t, drive.Files, 1)
	})

	t.Run("missing mediaType / name fallbacks", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		drive := testutil.NewMockDriveFileRepository()
		r.SetDriveFileRepo(drive)

		userID := "u1"
		host := "remote.example"
		ids := r.UpsertAttachments(
			[]activitypub.Document{{Type: "Document", URL: "https://r/no-meta.bin"}},
			&userID, &host,
		)
		require.Len(t, ids, 1)
		f, err := drive.FindByURI("https://r/no-meta.bin")
		require.NoError(t, err)
		assert.Equal(t, "application/octet-stream", f.Type)
		assert.Equal(t, "no-meta.bin", f.Name)
		assert.Nil(t, f.Comment, "AP name 空のとき comment は nil")
	})

	t.Run("nil driveFileRepo returns empty", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

		userID := "u1"
		host := "remote.example"
		ids := r.UpsertAttachments(
			[]activitypub.Document{{Type: "Document", URL: "https://r/x.png"}},
			&userID, &host,
		)
		assert.Empty(t, ids)
	})

	t.Run("Create error skips attachment but keeps processing", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		drive := &createErrDriveRepo{
			MockDriveFileRepository: testutil.NewMockDriveFileRepository(),
			err:                     errors.New("db down"),
		}
		r.SetDriveFileRepo(drive)

		userID := "u1"
		host := "remote.example"
		ids := r.UpsertAttachments(
			[]activitypub.Document{{Type: "Document", URL: "https://r/err.png"}},
			&userID, &host,
		)
		// Create が失敗した attachment は ID リストに含まれない
		assert.Empty(t, ids)
	})

	// SetAttachmentProbeClient setter が無 panic で呼べることを確認 (SSRF
	// 対策 #464)。実際の probe 経路は attachment_probe_test.go で
	// カバー済。
	t.Run("SetAttachmentProbeClient does not panic", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		client := &http.Client{Timeout: 1 * time.Second}
		r.SetAttachmentProbeClient(client)
		// nil 渡しでも panic しない (= invalidate 相当)
		r.SetAttachmentProbeClient(nil)
	})

	// #460/#461: width / height / icon.url / _misskey_blurhash が AP
	// Document に乗ってきた場合、drive_file の properties / thumbnailUrl
	// / blurhash に永続化される。
	t.Run("persists width/height/thumbnail/blurhash from AP Document", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		drive := testutil.NewMockDriveFileRepository()
		r.SetDriveFileRepo(drive)

		userID := "u1"
		host := "remote.example"
		ids := r.UpsertAttachments(
			[]activitypub.Document{{
				Type:      "Document",
				MediaType: "image/png",
				URL:       "https://r/cat.png",
				Width:     1280,
				Height:    720,
				Icon:      &activitypub.Image{Type: "Image", URL: "https://r/cat-thumb.png"},
				Blurhash:  "L6PZfSi_.AyE_3t7t7R**0o#DgR4",
			}},
			&userID, &host,
		)
		require.Len(t, ids, 1)
		f, err := drive.FindByURI("https://r/cat.png")
		require.NoError(t, err)
		require.NotNil(t, f.ThumbnailURL)
		assert.Equal(t, "https://r/cat-thumb.png", *f.ThumbnailURL)
		require.NotNil(t, f.Blurhash)
		assert.Equal(t, "L6PZfSi_.AyE_3t7t7R**0o#DgR4", *f.Blurhash)
		assert.JSONEq(t, `{"width":1280,"height":720}`, string(f.Properties))
	})

	// metadata が一部しか乗ってこないケース (width のみ等) でも、その
	// 部分だけ properties に入れて、抜けている方は埋めない。
	t.Run("partial metadata: width only persists width", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		drive := testutil.NewMockDriveFileRepository()
		r.SetDriveFileRepo(drive)

		userID := "u1"
		host := "remote.example"
		_ = r.UpsertAttachments(
			[]activitypub.Document{{
				Type: "Document", URL: "https://r/half.png", Width: 100,
			}},
			&userID, &host,
		)
		f, err := drive.FindByURI("https://r/half.png")
		require.NoError(t, err)
		assert.JSONEq(t, `{"width":100}`, string(f.Properties))
		assert.Nil(t, f.ThumbnailURL)
		assert.Nil(t, f.Blurhash)
	})

	// 全 metadata 欠落 (旧来 attachment と互換) では properties 空、
	// thumbnailUrl / blurhash 共に nil のまま。
	t.Run("no metadata leaves properties empty and thumbnail/blurhash nil", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		drive := testutil.NewMockDriveFileRepository()
		r.SetDriveFileRepo(drive)

		userID := "u1"
		host := "remote.example"
		_ = r.UpsertAttachments(
			[]activitypub.Document{{Type: "Document", URL: "https://r/bare.png"}},
			&userID, &host,
		)
		f, err := drive.FindByURI("https://r/bare.png")
		require.NoError(t, err)
		// Properties は zero value (nil JSON) のまま、DB default の `{}`
		// が DB 側で適用される
		assert.Empty(t, f.Properties)
		assert.Nil(t, f.ThumbnailURL)
		assert.Nil(t, f.Blurhash)
	})
}

// createErrDriveRepo wraps the mock and forces Create to error so the
// upsertAttachments error branch can be exercised.
type createErrDriveRepo struct {
	*testutil.MockDriveFileRepository
	err error
}

func (c *createErrDriveRepo) Create(_ *model.DriveFile) error { return c.err }

func TestCollectAttachedFileTypes(t *testing.T) {
	t.Run("nil driveFileRepo returns empty", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		assert.Empty(t, r.CollectAttachedFileTypes([]string{"f1"}))
	})

	t.Run("empty fileIDs returns empty", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		drive := testutil.NewMockDriveFileRepository()
		r.SetDriveFileRepo(drive)
		assert.Empty(t, r.CollectAttachedFileTypes(nil))
	})

	t.Run("returns MIME types in input order", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		drive := testutil.NewMockDriveFileRepository()
		r.SetDriveFileRepo(drive)
		drive.Files["a"] = &model.DriveFile{ID: "a", Type: "image/png"}
		drive.Files["b"] = &model.DriveFile{ID: "b", Type: "image/jpeg"}
		drive.Files["c"] = &model.DriveFile{ID: "c", Type: "video/mp4"}

		got := r.CollectAttachedFileTypes([]string{"c", "a", "b"})
		assert.Equal(t, []string{"video/mp4", "image/png", "image/jpeg"}, got)
	})

	t.Run("missing IDs are skipped", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		drive := testutil.NewMockDriveFileRepository()
		r.SetDriveFileRepo(drive)
		drive.Files["a"] = &model.DriveFile{ID: "a", Type: "image/png"}

		got := r.CollectAttachedFileTypes([]string{"a", "missing"})
		assert.Equal(t, []string{"image/png"}, got)
	})

	t.Run("FindByIDs error returns empty", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)
		r.SetDriveFileRepo(&findIDsErrDriveRepo{
			MockDriveFileRepository: testutil.NewMockDriveFileRepository(),
			err:                     errors.New("db down"),
		})
		assert.Empty(t, r.CollectAttachedFileTypes([]string{"a"}))
	})
}

// findIDsErrDriveRepo forces FindByIDs to error.
type findIDsErrDriveRepo struct {
	*testutil.MockDriveFileRepository
	err error
}

func (f *findIDsErrDriveRepo) FindByIDs(_ []string) ([]*model.DriveFile, error) {
	return nil, f.err
}

// --- #397 mention extraction / resolution / merge ---

func TestExtractMentionTags(t *testing.T) {
	tests := []struct {
		name     string
		raw      []any
		expected []string
	}{
		{
			name: "Mention href is extracted",
			raw: []any{
				map[string]any{"type": "Mention", "href": "https://a/users/alice"},
				map[string]any{"type": "Mention", "href": "https://b/users/bob", "name": "@bob@b"},
			},
			expected: []string{"https://a/users/alice", "https://b/users/bob"},
		},
		{
			name: "non-Mention types (Emoji/Hashtag) are skipped",
			raw: []any{
				map[string]any{"type": "Emoji", "href": "https://x/e"},
				map[string]any{"type": "Hashtag", "href": "https://x/t"},
				map[string]any{"type": "Mention", "href": "https://a/users/alice"},
			},
			expected: []string{"https://a/users/alice"},
		},
		{
			name: "empty href and non-map entries are skipped",
			raw: []any{
				map[string]any{"type": "Mention"},
				map[string]any{"type": "Mention", "href": ""},
				"not-an-object",
				42,
				map[string]any{"type": "Mention", "href": "https://a/users/alice"},
			},
			expected: []string{"https://a/users/alice"},
		},
		{
			name:     "empty input",
			raw:      nil,
			expected: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := federation.ExtractMentionTags(tt.raw)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestMergeMentionIDs(t *testing.T) {
	t.Run("dedup preserves input order", func(t *testing.T) {
		got := federation.MergeMentionIDs([]string{"a", "b"}, []string{"b", "c"})
		assert.Equal(t, []string{"a", "b", "c"}, []string(got))
	})
	t.Run("empty strings skipped", func(t *testing.T) {
		got := federation.MergeMentionIDs([]string{"", "a", ""}, []string{"", "b"})
		assert.Equal(t, []string{"a", "b"}, []string(got))
	})
	t.Run("both empty still returns non-nil", func(t *testing.T) {
		got := federation.MergeMentionIDs(nil, nil)
		require.NotNil(t, got)
		assert.Empty(t, got)
	})
	t.Run("dedup within single side", func(t *testing.T) {
		// a, b, a, b → a, b
		got := federation.MergeMentionIDs([]string{"a", "b", "a"}, []string{"b", "a"})
		assert.Equal(t, []string{"a", "b"}, []string(got))
	})
}

func TestResolveMentionedUserIDs(t *testing.T) {
	t.Run("local URI resolved via ExtractLocalUserID", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

		ids, err := r.ResolveMentionedUserIDs([]string{
			"https://example.com/users/alice",
			"https://example.com/users/bob/inbox", // 末尾サフィックス付き
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"alice", "bob"}, ids)
	})

	t.Run("known remote URI resolved via userRepo.FindByURI", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		host := "remote.example"
		uri := "https://remote.example/users/charlie"
		repo.Users["remote-charlie"] = &model.User{
			ID:   "remote-charlie",
			Host: &host,
			URI:  &uri,
		}
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

		ids, err := r.ResolveMentionedUserIDs([]string{uri})
		require.NoError(t, err)
		assert.Equal(t, []string{"remote-charlie"}, ids)
	})

	t.Run("unknown remote URI skipped without fetch", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

		ids, err := r.ResolveMentionedUserIDs([]string{"https://unknown.example/users/x"})
		require.NoError(t, err, "not-found で止めている")
		assert.Empty(t, ids)
	})

	t.Run("dedup", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

		ids, err := r.ResolveMentionedUserIDs([]string{
			"https://example.com/users/alice",
			"https://example.com/users/alice",
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"alice"}, ids)
	})

	t.Run("empty input", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

		ids, err := r.ResolveMentionedUserIDs(nil)
		require.NoError(t, err)
		assert.Empty(t, ids)
	})

	// **引けなかったものを「未知」に潰さない** (#3121)。潰すと mentions /
	// visibleUserIds が空のまま note が確定し、dedup があるので retry でも直らない。
	t.Run("lookup が引けないときは伝播する", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		boom := errors.New("connection refused")
		repo.FindByURIErr = boom
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen)

		_, err := r.ResolveMentionedUserIDs([]string{"https://remote.example/users/x"})
		require.ErrorIs(t, err, boom)
	})
}

func TestResolveTextMentionUserIDs(t *testing.T) {
	mkResolver := func() (*federation.Resolver, *testutil.MockUserRepository) {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		return federation.NewResolver(repo, noteRepo, urls, &stubFetcher{}, idGen), repo
	}
	t.Run("resolves both local and remote to IDs", func(t *testing.T) {
		r, repo := mkResolver()
		repo.Users["local-id"] = &model.User{ID: "local-id", Username: "alice", UsernameLower: "alice"}
		host := "remote.example"
		repo.Users["remote-id"] = &model.User{ID: "remote-id", Username: "bob", UsernameLower: "bob", Host: &host}

		ids, err := r.ResolveTextMentionUserIDs([]corenote.Mention{
			{Username: "alice"},
			{Username: "bob", Host: "remote.example"},
		}, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"local-id", "remote-id"}, ids)
	})
	t.Run("unknown user is skipped", func(t *testing.T) {
		r, _ := mkResolver()
		ids, err := r.ResolveTextMentionUserIDs([]corenote.Mention{{Username: "ghost"}}, nil)
		require.NoError(t, err, "not-found で止めている")
		assert.Empty(t, ids)
	})
	t.Run("empty input", func(t *testing.T) {
		r, _ := mkResolver()
		ids, err := r.ResolveTextMentionUserIDs(nil, nil)
		require.NoError(t, err)
		assert.Nil(t, ids)
	})
	t.Run("lookup が引けないときは伝播する", func(t *testing.T) {
		r, repo := mkResolver()
		boom := errors.New("connection refused")
		repo.FindByUsernameLowerFn = func(string, *string) (*model.User, error) { return nil, boom }
		_, err := r.ResolveTextMentionUserIDs([]corenote.Mention{{Username: "alice"}}, nil)
		require.ErrorIs(t, err, boom)
	})
	// #3330: 本家は `@user@<自インスタンスのホスト>` をローカルの利用者として
	// 解決する (RemoteUserResolveService.resolveUser)。大文字混じりでも同じ。
	t.Run("own host resolves to the local user", func(t *testing.T) {
		r, repo := mkResolver()
		repo.Users["local-id"] = &model.User{ID: "local-id", Username: "alice", UsernameLower: "alice"}
		remote := "remote.example"
		ids, err := r.ResolveTextMentionUserIDs([]corenote.Mention{
			{Username: "alice", Host: "Example.COM"},
		}, &remote)
		require.NoError(t, err)
		assert.Equal(t, []string{"local-id"}, ids)
	})
	// #3330: リモートの投稿のホスト無し `@bob` は書き手のホストの bob
	// (`m.host ?? user.host`)。ローカルの bob に当ててはいけない。
	t.Run("host-less mention of a remote note resolves on the author's host", func(t *testing.T) {
		r, repo := mkResolver()
		repo.Users["local-bob"] = &model.User{ID: "local-bob", Username: "bob", UsernameLower: "bob"}
		remote := "remote.example"
		repo.Users["remote-bob"] = &model.User{ID: "remote-bob", Username: "bob", UsernameLower: "bob", Host: &remote}
		ids, err := r.ResolveTextMentionUserIDs([]corenote.Mention{{Username: "bob"}}, &remote)
		require.NoError(t, err)
		assert.Equal(t, []string{"remote-bob"}, ids)

		other := "other.example"
		ids, err = r.ResolveTextMentionUserIDs([]corenote.Mention{{Username: "bob"}}, &other)
		require.NoError(t, err)
		assert.Empty(t, ids, "書き手のホストに居なければ解決しない (ローカルへ落とさない)")
	})
	// #3330: 本家は解決した利用者を ID で重複除去する。
	t.Run("dedups by resolved user ID", func(t *testing.T) {
		r, repo := mkResolver()
		repo.Users["local-id"] = &model.User{ID: "local-id", Username: "Alice", UsernameLower: "alice"}
		ids, err := r.ResolveTextMentionUserIDs([]corenote.Mention{
			{Username: "Alice"}, {Username: "alice"}, {Username: "ALICE", Host: "example.com"},
		}, nil)
		require.NoError(t, err)
		assert.Equal(t, []string{"local-id"}, ids)
	})
}

// TestIngestNote_SpecifiedDMPopulatesMentionsAndVisibleUserIDs covers the
// #397 fix end-to-end: a specified DM whose body has no @mention but whose
// AP `tag` array has a Mention to alice should still end up with alice in
// note.Mentions and note.VisibleUserIDs so /api/notes/mentions returns it
// and CanView lets alice read it.
func TestIngestNote_SpecifiedDMPopulatesMentionsAndVisibleUserIDs(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	// alice はローカル mk-A のユーザー (URI = https://example.com/users/alice)
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/dm1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "secret hello",
		"to": ["https://example.com/users/alice"],
		"cc": [],
		"tag": [
			{"type": "Mention", "href": "https://example.com/users/alice", "name": "@alice"}
		]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	assert.Equal(t, model.NoteVisibilitySpecified, got.Visibility)
	assert.Equal(t, []string{"alice"}, []string(got.Mentions),
		"AP tag Mention 由来でも mentions が埋まる")
	assert.Equal(t, []string{"alice"}, []string(got.VisibleUserIDs),
		"specified では VisibleUserIDs が AP to から埋まる")
}

func TestIngestNote_NonSpecifiedSkipsVisibleUserIDs(t *testing.T) {
	// public visibility なら VisibleUserIDs は埋めない (空のまま)。
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/pub1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "hello world",
		"to": ["https://www.w3.org/ns/activitystreams#Public"],
		"cc": [],
		"tag": [
			{"type": "Mention", "href": "https://example.com/users/alice", "name": "@alice"}
		]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	assert.Equal(t, model.NoteVisibilityPublic, got.Visibility)
	assert.Equal(t, []string{"alice"}, []string(got.Mentions))
	assert.Empty(t, []string(got.VisibleUserIDs))
}

func TestIngestNote_TagMentionsMergedWithTextMentions(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	// ローカル bob を MockUserRepository に登録 (FindByUsernameLower 解決用)。
	// MockUserRepository は usernameLower で lookup する。
	repo.Users["bob-local-id"] = &model.User{
		ID:            "bob-local-id",
		Username:      "bob",
		UsernameLower: "bob",
	}
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	// 本文 "@bob@example.com" (自インスタンスのホスト) → bob-local-id、tag → alice。
	// 両方が mentions に入ること。ホスト無しの "@bob" は書き手のホストの bob (#3330)。
	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/merge1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "hi @bob@example.com",
		"to": ["https://example.com/users/alice"],
		"cc": [],
		"tag": [
			{"type": "Mention", "href": "https://example.com/users/alice", "name": "@alice"}
		]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	mentions := []string(got.Mentions)
	assert.Contains(t, mentions, "alice")
	assert.Contains(t, mentions, "bob-local-id", "本文の @bob は user ID へ resolve される")
}

// #3330: a host-less `@bob` in a remote note means bob on the author's
// server (upstream `m.host ?? user.host`), never the local bob.
func TestIngestNote_HostlessTextMentionResolvesOnAuthorHost(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	repo.Users["bob-local-id"] = &model.User{ID: "bob-local-id", Username: "bob", UsernameLower: "bob"}
	remote := "remote.example"
	repo.Users["bob-remote-id"] = &model.User{ID: "bob-remote-id", Username: "bob", UsernameLower: "bob", Host: &remote}
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/notes/hostless1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "hi @bob",
		"to": ["https://www.w3.org/ns/activitystreams#Public"],
		"cc": []
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	assert.Equal(t, []string{"bob-remote-id"}, []string(got.Mentions))
}

// #3330: upstream NoteCreateService.create pushes the reply target's author
// into mentionedUsers unless it is the note's author (`user.id !==
// data.reply.userId`), then rejects when `Math.max(mentionedUsers.length,
// apMentionRawCount) > mentionLimit` (NoteCreateService.ts:611-631). With 20
// Mention tags (= the limit) a reply to someone else is 21 and rejected, while
// a reply to the author's own note stays at 20 and is accepted.
func TestIngestNote_MentionLimitCountsReplyTargetAuthor(t *testing.T) {
	mkBody := func(id, inReplyTo string) []byte {
		tags := make([]string, 0, corenote.DefaultMentionLimit)
		for i := 0; i < corenote.DefaultMentionLimit; i++ {
			tags = append(tags, fmt.Sprintf(`{"type": "Mention", "href": "https://example.com/users/local%d"}`, i))
		}
		return []byte(fmt.Sprintf(`{ "@context": "https://www.w3.org/ns/activitystreams",
			"id": "https://remote.example/notes/%s",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "reply",
			"inReplyTo": %q,
			"to": ["https://www.w3.org/ns/activitystreams#Public"],
			"cc": [],
			"tag": [%s]
		}`, id, inReplyTo, strings.Join(tags, ",")))
	}
	setup := func(t *testing.T) (*federation.Resolver, *testutil.MockNoteRepository, *model.User) {
		t.Helper()
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
		author, err := r.ResolveActor("https://remote.example/users/alice")
		require.NoError(t, err)
		return r, noteRepo, author
	}

	t.Run("reply to another user's local note is rejected", func(t *testing.T) {
		r, noteRepo, _ := setup(t)
		noteRepo.Notes["parent"] = &model.Note{ID: "parent", UserID: "someone-else"}
		_, err := r.IngestNote(mkBody("r1", "https://example.com/notes/parent"))
		require.ErrorIs(t, err, corenote.ErrContainsTooManyMentions)
	})
	t.Run("reply to another user's remote note is rejected", func(t *testing.T) {
		r, noteRepo, _ := setup(t)
		parentURI := "https://remote.example/notes/parent"
		noteRepo.Notes["parent"] = &model.Note{ID: "parent", UserID: "someone-else", URI: &parentURI}
		_, err := r.IngestNote(mkBody("r2", parentURI))
		require.ErrorIs(t, err, corenote.ErrContainsTooManyMentions)
	})
	t.Run("reply to the author's own note is accepted", func(t *testing.T) {
		r, noteRepo, author := setup(t)
		parentURI := "https://remote.example/notes/own"
		noteRepo.Notes["own"] = &model.Note{ID: "own", UserID: author.ID, URI: &parentURI}
		got, err := r.IngestNote(mkBody("r3", parentURI))
		require.NoError(t, err, "自分の投稿への返信は返信先の作者を数えない")
		require.NotNil(t, got.ReplyID)
		assert.Equal(t, "own", *got.ReplyID)
	})
}

// #3330: upstream counts the reply target's author and a specified note's
// recipients in mentionedUsers before comparing with mentionLimit, so a DM
// with more recipients than the limit is rejected even without Mention tags.
func TestIngestNote_MentionLimitCountsSpecifiedRecipients(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	mkBody := func(n int) []byte {
		to := make([]string, 0, n)
		for i := 0; i < n; i++ {
			to = append(to, fmt.Sprintf("%q", fmt.Sprintf("https://example.com/users/local%d", i)))
		}
		return []byte(fmt.Sprintf(`{ "@context": "https://www.w3.org/ns/activitystreams",
			"id": "https://remote.example/notes/dm%d",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "secret",
			"to": [%s],
			"cc": []
		}`, n, strings.Join(to, ",")))
	}
	_, err := r.IngestNote(mkBody(corenote.DefaultMentionLimit + 1))
	require.ErrorIs(t, err, corenote.ErrContainsTooManyMentions)

	got, err := r.IngestNote(mkBody(corenote.DefaultMentionLimit))
	require.NoError(t, err, "上限ちょうどは通る")
	assert.Len(t, []string(got.VisibleUserIDs), corenote.DefaultMentionLimit)
}

// An e-mail address in a remote note's text is not a mention in the MFM tree,
// so the local user named after its domain must not be mentioned (#3304).
func TestIngestNote_EmailAddressIsNotTextMention(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	repo.Users["example-local-id"] = &model.User{
		ID:            "example-local-id",
		Username:      "example",
		UsernameLower: "example",
	}
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/notes/mail1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "mail foo@example",
		"to": ["https://www.w3.org/ns/activitystreams#Public"],
		"cc": []
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	assert.Empty(t, []string(got.Mentions))
}

// TestUpdateRemoteNote_MentionsRecomputed confirms tag-derived mentions are
// recomputed on inbound Update even if text is unchanged (#397)。
func TestUpdateRemoteNote_MentionsRecomputed(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	// 既存リモートノート (mentions 空)
	host := "remote.example"
	uri := "https://remote.example/notes/edit1"
	existing := &model.Note{
		ID:       "n-edit1",
		UserID:   "remote-alice",
		UserHost: &host,
		URI:      &uri,
		Mentions: model.StringArray{},
	}
	noteRepo.Notes[existing.ID] = existing

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/edit1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "edited content",
		"tag": [
			{"type": "Mention", "href": "https://example.com/users/alice", "name": "@alice"}
		]
	}`)
	got, err := r.UpdateRemoteNote(body, "")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, []string{"alice"}, []string(got.Mentions))
}

// 同一 URI への並行 ResolveActor 呼び出しは singleflight で 1 回の HTTP
// fetch に collapse される (#300 3-7)。inbox 受信が同じ remote actor の
// activity を連続して取り込むケースを想定。
func TestResolveActor_DedupesConcurrentCalls(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	fetcher := &blockingFetcher{body: []byte(sampleActor), gate: make(chan struct{})}
	r := federation.NewResolver(repo, noteRepo, urls, fetcher, idGen)

	const N = 16
	var wg sync.WaitGroup
	results := make([]*model.User, N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = r.ResolveActor("https://remote.example/users/alice")
		}(i)
	}
	close(fetcher.gate)
	wg.Wait()

	for i := 0; i < N; i++ {
		require.NoError(t, errs[i], "call %d", i)
		require.NotNil(t, results[i])
		assert.Equal(t, "alice", results[i].Username)
	}
	// 16 並行 → 1 fetch に集約されることが目標。leader が 1 回完了すると
	// 別の wave の leader が立ち得るので寛容な上限を取る。
	assert.LessOrEqual(t, fetcher.calls.Load(), int64(2),
		"singleflight must collapse concurrent same-URI ResolveActor to ~1 fetch")
	// DB 上は単一 user 行になることも担保する (UNIQUE 衝突が発生しない)。
	assert.Len(t, repo.Users, 1)
}

// blockingNoteFetcher は FetchObject の calls を atomic で数える Note 用 fetcher。
// FetchObject の戻り値はユニコードな Note JSON である必要があるので、actor
// JSON を流用する resolver test のヘルパとは別に持つ。
type blockingNoteFetcher struct {
	body  []byte
	gate  chan struct{}
	calls atomic.Int64
}

func (b *blockingNoteFetcher) FetchObject(_ string) ([]byte, error) {
	b.calls.Add(1)
	<-b.gate
	return b.body, nil
}

// 同一 URI への並行 ResolveNote 呼び出しも同様に collapse される (#300 3-7)。
func TestResolveNote_DedupesConcurrentCalls(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	repo.Users["uA"] = &model.User{
		ID:       "uA",
		Username: "alice",
		Host:     ptrString("remote.example"),
		URI:      ptrString("https://remote.example/users/alice"),
	}
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/n1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "hello"
	}`)
	fetcher := &blockingNoteFetcher{body: body, gate: make(chan struct{})}
	r := federation.NewResolver(repo, noteRepo, urls, fetcher, idGen)

	const N = 16
	var wg sync.WaitGroup
	results := make([]*model.Note, N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = r.ResolveNote("https://remote.example/notes/n1")
		}(i)
	}
	close(fetcher.gate)
	wg.Wait()

	for i := 0; i < N; i++ {
		require.NoError(t, errs[i], "call %d", i)
		require.NotNil(t, results[i])
	}
	assert.LessOrEqual(t, fetcher.calls.Load(), int64(2),
		"singleflight must collapse concurrent same-URI ResolveNote to ~1 fetch")
}

func ptrString(s string) *string { return &s }

// stubPublickeyExtraRepo は FEP-521a Multikey 永続化用の in-memory mock。
// resolver の cacheAssertionMethods / PublicKeyForKeyID 経路を unit test
// から exercise するために使う (#1067 / #1070)。
type stubPublickeyExtraRepo struct {
	mu      sync.RWMutex
	entries map[string]*model.UserPublickeyExtra // keyed by keyID
	upserts []model.UserPublickeyExtra
	failErr error
	// findErr は FindByUserAndKeyID にだけ not-found ではない DB 障害を注入する
	// 口 (#3121)。failErr と分けてあるのは、既存のテストが Upsert /
	// ListByUserID の失敗だけを注入しているため。
	findErr error
}

func (s *stubPublickeyExtraRepo) Upsert(pk *model.UserPublickeyExtra) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failErr != nil {
		return s.failErr
	}
	if s.entries == nil {
		s.entries = make(map[string]*model.UserPublickeyExtra)
	}
	s.entries[pk.KeyID] = pk
	s.upserts = append(s.upserts, *pk)
	return nil
}

func (s *stubPublickeyExtraRepo) ListByUserID(userID string) ([]model.UserPublickeyExtra, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.failErr != nil {
		return nil, s.failErr
	}
	var out []model.UserPublickeyExtra
	for _, pk := range s.entries {
		if pk.UserID == userID {
			out = append(out, *pk)
		}
	}
	return out, nil
}

func (s *stubPublickeyExtraRepo) DeleteByKeyID(userID, keyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pk, ok := s.entries[keyID]; ok && pk.UserID == userID {
		delete(s.entries, keyID)
	}
	return nil
}

// 以下は repository.UserPublickeyExtraRepository interface 完全実装のための
// stub。resolver test では使わないが、同 package 内の deliver_service_test
// で stub を流用するために必要 (#1067 / #1071)。
func (s *stubPublickeyExtraRepo) FindByUserAndKeyID(userID, keyID string) (*model.UserPublickeyExtra, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.findErr != nil {
		return nil, s.findErr
	}
	if pk, ok := s.entries[keyID]; ok && pk.UserID == userID {
		return pk, nil
	}
	// production の gorm repository は miss 時 ErrRecordNotFound を返す。
	// PublicKeyForKeyID は ErrRecordNotFound を silent fallback、それ以外を warn と
	// 区別するので、stub も同じ semantic を返して fallback path を正確に再現する。
	return nil, gorm.ErrRecordNotFound
}

func (s *stubPublickeyExtraRepo) DeleteByUserID(userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for keyID, pk := range s.entries {
		if pk.UserID == userID {
			delete(s.entries, keyID)
		}
	}
	return nil
}

// stubPublickeyRepo は keys map race test 用の minimal な PublickeyStore
// 実装。本物の publickey_repo は GORM 経由なのでテストでは差し替える。
type stubPublickeyRepo struct {
	mu      sync.RWMutex
	entries map[string]*model.UserPublickey
	// findErr は not-found ではない DB 障害を注入する口 (#3121)。
	findErr error
}

func (s *stubPublickeyRepo) FindByUserID(userID string) (*model.UserPublickey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.findErr != nil {
		return nil, s.findErr
	}
	if pk, ok := s.entries[userID]; ok {
		return pk, nil
	}
	// production の gorm repository は miss 時 ErrRecordNotFound を返す。
	// PublicKeyForActor は not-found を「鍵が無い」、それ以外を
	// ErrLookupUnavailable と区別するので、stub も同じ semantic を返す (#3121)。
	return nil, gorm.ErrRecordNotFound
}

func (s *stubPublickeyRepo) Upsert(pk *model.UserPublickey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]*model.UserPublickey)
	}
	s.entries[pk.UserID] = pk
	return nil
}

// Ed25519 鍵を持つ remote actor を fetch して assertionMethod を
// user_publickey_extra に upsert する (#1067 / #1070)。
func TestResolveActor_PersistsAssertionMethod(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	mb, err := activitypub.EncodeEd25519Multikey(pub)
	require.NoError(t, err)

	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"publicKey": {
			"id": "https://remote.example/users/alice#main-key",
			"owner": "https://remote.example/users/alice",
			"publicKeyPem": "-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----"
		},
		"assertionMethod": [{
			"id": "https://remote.example/users/alice#ed25519-key",
			"type": "Multikey",
			"controller": "https://remote.example/users/alice",
			"publicKeyMultibase": "` + mb + `"
		}]
	}`

	r, _ := newResolver(t, body, nil)
	extra := &stubPublickeyExtraRepo{}
	r.SetPublickeyExtraRepo(extra)

	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)

	row, err := extra.FindByUserAndKeyID(user.ID, "https://remote.example/users/alice#ed25519-key")
	require.NoError(t, err)
	assert.Equal(t, user.ID, row.UserID)
	assert.Equal(t, model.AlgEd25519, row.Alg)
	assert.Contains(t, row.KeyPEM, "PUBLIC KEY")

	// round-trip: 保存された PEM から元の Ed25519 公開鍵を復元できる
	parsed, err := activitypub.ParseEd25519PublicKeyPEM(row.KeyPEM)
	require.NoError(t, err)
	assert.Equal(t, pub, parsed)
}

// 不正な Multikey (Multibase decode 失敗) が混ざっていても、正常な entry は
// upsert される (silently skip + warn log)。
func TestResolveActor_SkipsMalformedAssertionMethod(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	mb, err := activitypub.EncodeEd25519Multikey(pub)
	require.NoError(t, err)

	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"publicKey": {
			"id": "https://remote.example/users/alice#main-key",
			"owner": "https://remote.example/users/alice",
			"publicKeyPem": "-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----"
		},
		"assertionMethod": [
			{"id": "https://remote.example/users/alice#bad", "type": "Multikey", "controller": "x", "publicKeyMultibase": "INVALID"},
			{"id": "https://remote.example/users/alice#ed25519-key", "type": "Multikey", "controller": "x", "publicKeyMultibase": "` + mb + `"},
			{"id": "https://remote.example/users/alice#non-multikey", "type": "JsonWebKey", "controller": "x", "publicKeyMultibase": "z6Mk..."}
		]
	}`

	r, _ := newResolver(t, body, nil)
	extra := &stubPublickeyExtraRepo{}
	r.SetPublickeyExtraRepo(extra)

	_, err = r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)

	// 不正 entry / 非 Multikey type は skip され、正常な Ed25519 のみ upsert される
	rows, err := extra.ListByUserID(extra.upserts[0].UserID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "https://remote.example/users/alice#ed25519-key", rows[0].KeyID)
}

// PublicKeyForKeyID は user_publickey_extra (Ed25519 / Multikey) を最初に探し、
// miss なら user_publickey (RSA) を fallback で返す dual lookup を行う (#1070)。
func TestPublicKeyForKeyID_DualLookup(t *testing.T) {
	r, _ := newResolver(t, sampleActor, nil)
	pkRepo := &stubPublickeyRepo{entries: map[string]*model.UserPublickey{}}
	r.SetPublickeyRepo(pkRepo)
	extra := &stubPublickeyExtraRepo{}
	r.SetPublickeyExtraRepo(extra)

	// user_publickey に RSA primary key を seed
	require.NoError(t, pkRepo.Upsert(&model.UserPublickey{
		UserID: "alice", KeyID: "https://remote.example/users/alice#main-key",
		KeyPEM: "-----BEGIN PUBLIC KEY-----\nRSA-FAKE\n-----END PUBLIC KEY-----",
	}))
	// user_publickey_extra に Ed25519 を seed
	require.NoError(t, extra.Upsert(&model.UserPublickeyExtra{
		UserID: "alice", KeyID: "https://remote.example/users/alice#ed25519-key",
		KeyPEM: "-----BEGIN PUBLIC KEY-----\nED25519-FAKE\n-----END PUBLIC KEY-----",
		Alg:    model.AlgEd25519,
	}))

	// Ed25519 keyId 一致 → extra から返る
	pem, err := r.PublicKeyForKeyID("alice", "https://remote.example/users/alice#ed25519-key")
	require.NoError(t, err)
	assert.Contains(t, pem, "ED25519-FAKE")

	// RSA keyId (= extra に無し) → PublicKeyForActor fallback で RSA primary が返る
	pem, err = r.PublicKeyForKeyID("alice", "https://remote.example/users/alice#main-key")
	require.NoError(t, err)
	assert.Contains(t, pem, "RSA-FAKE")
}

// Fix A (security): cross-host な publicKey.id を持つ actor は fetchActor で
// reject される。これが無いと攻撃者 actor が publicKey.id に victim の keyId を
// 載せて自分の RSA 鍵を植え込み、LD-Signature 経路の global FindByKeyID 解決で
// victim を名乗る活動を verify 通過させられる (key confusion / 連合認証バイパス)。
func TestResolveActor_RejectsCrossHostPublicKeyID(t *testing.T) {
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://evil.example/users/mal",
		"type": "Person",
		"preferredUsername": "mal",
		"inbox": "https://evil.example/users/mal/inbox",
		"publicKey": {
			"id": "https://victim.example/users/alice#main-key",
			"owner": "https://victim.example/users/alice",
			"publicKeyPem": "-----BEGIN PUBLIC KEY-----\nATTACKER\n-----END PUBLIC KEY-----"
		}
	}`
	r, repo := newResolver(t, body, nil)
	_, err := r.ResolveActor("https://evil.example/users/mal")
	require.Error(t, err, "cross-host publicKey.id を持つ actor は拒否されるべき")
	assert.ErrorIs(t, err, federation.ErrObjectHostMismatch)
	assert.Empty(t, repo.Users, "拒否された actor を DB に作ってはいけない")
}

// Fix B (security): assertionMethod の keyId (am.ID) host が actor host と一致
// しない entry は保存されない。攻撃者 actor (evil.example) が victim ドメインの
// keyId を載せても user_publickey_extra に入らず、key confusion を成立させない。
// 同一 host の正規 entry は従来どおり保存される。
func TestResolveActor_SkipsCrossHostAssertionMethodKeyID(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	mb, err := activitypub.EncodeEd25519Multikey(pub)
	require.NoError(t, err)

	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://evil.example/users/mal",
		"type": "Person",
		"preferredUsername": "mal",
		"inbox": "https://evil.example/users/mal/inbox",
		"publicKey": {"id": "https://evil.example/users/mal#main-key", "owner": "https://evil.example/users/mal", "publicKeyPem": "-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----"},
		"assertionMethod": [
			{"id": "https://evil.example/users/mal#ed25519-key", "type": "Multikey", "controller": "https://evil.example/users/mal", "publicKeyMultibase": "` + mb + `"},
			{"id": "https://victim.example/users/alice#x9z", "type": "Multikey", "controller": "https://victim.example/users/alice", "publicKeyMultibase": "` + mb + `"}
		]
	}`
	r, _ := newResolver(t, body, nil)
	extra := &stubPublickeyExtraRepo{}
	r.SetPublickeyExtraRepo(extra)

	user, err := r.ResolveActor("https://evil.example/users/mal")
	require.NoError(t, err)

	// 同一 host の entry のみ保存され、cross-host の victim keyId は drop される
	rows, err := extra.ListByUserID(user.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "https://evil.example/users/mal#ed25519-key", rows[0].KeyID)

	// 植え込もうとした victim の keyId は user_publickey_extra に存在しない
	_, err = extra.FindByUserAndKeyID(user.ID, "https://victim.example/users/alice#x9z")
	require.Error(t, err, "cross-host keyId は保存されていないはず")
}

// Fix C (security, defense-in-depth): PublicKeyForKeyID は actorID-scope で鍵を
// 引く。万一 cross-host keyId が別 actor (mal) 配下に存在しても、victim を actorID
// に指定した lookup ではヒットせず、攻撃者鍵を返さない。RSA primary に fallback する。
func TestPublicKeyForKeyID_ActorScopedRejectsPlantedKey(t *testing.T) {
	r, _ := newResolver(t, sampleActor, nil)
	pkRepo := &stubPublickeyRepo{entries: map[string]*model.UserPublickey{}}
	r.SetPublickeyRepo(pkRepo)
	extra := &stubPublickeyExtraRepo{}
	r.SetPublickeyExtraRepo(extra)

	// victim alice の正規 RSA primary
	require.NoError(t, pkRepo.Upsert(&model.UserPublickey{
		UserID: "alice", KeyID: "https://victim.example/users/alice#main-key",
		KeyPEM: "-----BEGIN PUBLIC KEY-----\nALICE-RSA\n-----END PUBLIC KEY-----",
	}))
	// 攻撃者が mal 配下に alice の keyId で植え込んだ Ed25519 鍵 (Fix B を擦り抜けたと仮定)
	require.NoError(t, extra.Upsert(&model.UserPublickeyExtra{
		UserID: "mal", KeyID: "https://victim.example/users/alice#x9z",
		KeyPEM: "-----BEGIN PUBLIC KEY-----\nATTACKER\n-----END PUBLIC KEY-----",
		Alg:    model.AlgEd25519,
	}))

	// alice を actorID にした lookup は植え込み鍵を返さず、alice の RSA primary に fallback
	pem, err := r.PublicKeyForKeyID("alice", "https://victim.example/users/alice#x9z")
	require.NoError(t, err)
	assert.NotContains(t, pem, "ATTACKER", "他 actor 配下の植え込み鍵を返してはいけない")
	assert.Contains(t, pem, "ALICE-RSA")
}

// **恒久的に不正な actor で fetch を増幅させない。** #2662 で
// preferredUsername の検証を足したことで、検証前に取り込まれた既存行は
// refresh のたびに ErrInvalidActor になる。lastFetchedAt を進めないと
// shouldRefreshActor が永久に true で、inbound activity 1 件につき outbound
// fetch が 1 回走り続ける (相手は同じ document を返すので自然回復しない)。
func TestResolveActor_PermanentlyInvalidDocumentStopsRefetchLoop(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://remote.example/users/alice"
	old := time.Now().Add(-48 * time.Hour)
	repo.Users["existing"] = &model.User{
		ID:            "existing",
		Username:      "alice",
		URI:           &uri,
		LastFetchedAt: &old,
	}
	// preferredUsername が新条件を満たさない actor (検証導入前の既存行を模す)。
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "-alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"publicKey": {"publicKeyPem": "PEM"}
	}`
	counting := &countingFetcher{body: []byte(body)}
	r := federation.NewResolver(repo, noteRepo, urls, counting, idGen)

	for i := 0; i < 5; i++ {
		_, err := r.ResolveActor(uri)
		require.NoError(t, err, "既存行は返せること")
	}
	// 内訳: refreshActor (TTL 失効) 1 回 + refreshPublicKey の初回 1 回。
	// 以降は lastFetchedAt が進んでいるので refreshActor は走らず、
	// 鍵側は keyFetchBackoff が抑える。**回数が呼び出し数に比例しない**のが要点。
	assert.Equal(t, 2, counting.calls, "解決のたびに fetch しない")
	require.NotNil(t, repo.Users["existing"].LastFetchedAt)
	assert.True(t, repo.Users["existing"].LastFetchedAt.After(old),
		"lastFetchedAt が進んでいること")
}

// `user.avatarUrl` は varchar(1024)、`user.bannerUrl` は varchar(512)。
// 超過値を渡すと INSERT が SQLSTATE 22001 で落ち、**その actor が 1 行も
// 作られない**。URL は truncate すると壊れるだけなので落とす (#2662)。
func TestResolveActor_DropsOversizedMediaURLs(t *testing.T) {
	long := "https://remote.example/" + strings.Repeat("a", 1100)
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"icon": {"type": "Image", "url": "` + long + `"},
		"image": {"type": "Image", "url": "` + long + `"},
		"publicKey": {"publicKeyPem": "PEM"}
	}`
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)

	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err, "actor 自体は作る")
	assert.Nil(t, user.AvatarURL, "1024 超の avatarUrl は落とす")
	assert.Nil(t, user.BannerURL, "512 超の bannerUrl は落とす")

	// **境界ちょうどは通す。** `> max` を `>= max` にしても `> max+1` にしても
	// 落ちるように、1024 / 512 ちょうどと +1 の両方を見る。
	urlOfLen := func(n int) string {
		const prefix = "https://remote.example/"
		return prefix + strings.Repeat("c", n-len(prefix))
	}
	for _, tc := range []struct {
		name       string
		avatarLen  int
		bannerLen  int
		wantAvatar bool
		wantBanner bool
	}{
		{"exactly at limit", 1024, 512, true, true},
		{"one over limit", 1025, 513, false, false},
		{"avatar fits banner does not", 1024, 513, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			avatar := urlOfLen(tc.avatarLen)
			banner := urlOfLen(tc.bannerLen)
			body := `{ "@context": "https://www.w3.org/ns/activitystreams",
				"id": "https://remote.example/users/carol",
				"type": "Person",
				"preferredUsername": "carol",
				"inbox": "https://remote.example/users/carol/inbox",
				"icon": {"type": "Image", "url": "` + avatar + `"},
				"image": {"type": "Image", "url": "` + banner + `"},
				"publicKey": {"publicKeyPem": "PEM"}
			}`
			repo := testutil.NewMockUserRepository()
			r := federation.NewResolver(repo, testutil.NewMockNoteRepository(), urls, &stubFetcher{body: []byte(body)}, idGen)
			user, err := r.ResolveActor("https://remote.example/users/carol")
			require.NoError(t, err)
			assert.Equal(t, tc.wantAvatar, user.AvatarURL != nil, "avatarUrl")
			assert.Equal(t, tc.wantBanner, user.BannerURL != nil, "bannerUrl")
		})
	}

	// **NUL 入りの URL も落とす。** PostgreSQL の text は NUL を受け付けず、
	// 長さ超過と同じく INSERT / UPDATE ごと落ちる。create 経路では actor が
	// 1 行も作られず、refresh 経路では lastFetchedAt が進まないので fetch が
	// 増幅する。
	t.Run("nul in media url", func(t *testing.T) {
		nulBody := `{ "@context": "https://www.w3.org/ns/activitystreams",
			"id": "https://remote.example/users/dave",
			"type": "Person",
			"preferredUsername": "dave",
			"inbox": "https://remote.example/users/dave/inbox",
			"icon": {"type": "Image", "url": "https://remote.example/a\u0000.png"},
			"image": {"type": "Image", "url": "https://remote.example/b\u0000.png"},
			"publicKey": {"publicKeyPem": "PEM"}
		}`
		repo := testutil.NewMockUserRepository()
		r := federation.NewResolver(repo, testutil.NewMockNoteRepository(), urls, &stubFetcher{body: []byte(nulBody)}, idGen)
		user, err := r.ResolveActor("https://remote.example/users/dave")
		require.NoError(t, err, "actor 自体は作る")
		assert.Nil(t, user.AvatarURL)
		assert.Nil(t, user.BannerURL)
	})

	// 収まる長さは従来どおり入る。banner は 512 なので avatar より厳しい。
	fits := "https://remote.example/" + strings.Repeat("b", 400)
	body2 := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/users/bob",
		"type": "Person",
		"preferredUsername": "bob",
		"inbox": "https://remote.example/users/bob/inbox",
		"icon": {"type": "Image", "url": "` + fits + `"},
		"image": {"type": "Image", "url": "` + fits + `"},
		"publicKey": {"publicKeyPem": "PEM"}
	}`
	repo2 := testutil.NewMockUserRepository()
	r2 := federation.NewResolver(repo2, testutil.NewMockNoteRepository(), urls, &stubFetcher{body: []byte(body2)}, idGen)
	user2, err := r2.ResolveActor("https://remote.example/users/bob")
	require.NoError(t, err)
	require.NotNil(t, user2.AvatarURL)
	assert.Equal(t, fits, *user2.AvatarURL)
	require.NotNil(t, user2.BannerURL)
}

// **他 host の bare IRI 参照を「申告済み」と数えない。** 数えると、攻撃者
// actor が victim ドメインの keyId を参照に載せるだけでその行を purge から
// 守れてしまう (= ローテーション済みの鍵を延命できる)。行の作成側
// (`am.ID`) も host 検証済みだが、検証導入前に入った行に対する多重防御
// として参照側でも縛る (#2662)。
func TestResolveActor_CrossHostAssertionMethodRefDoesNotProtect(t *testing.T) {
	const crossHostKeyID = "https://evil.example/users/x#k1"
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"publicKey": {"publicKeyPem": "PEM"},
		"assertionMethod": ["` + crossHostKeyID + `"]
	}`
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	extra := &stubPublickeyExtraRepo{}
	r.SetPublickeyExtraRepo(extra)

	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)

	// 検証導入前に入りえた「他 host の keyId」の行を直接 seed する。
	require.NoError(t, extra.Upsert(&model.UserPublickeyExtra{
		UserID: user.ID, KeyID: crossHostKeyID, KeyPEM: "OLD",
	}))

	// もう一度 refresh させると、参照は host 不一致なので守られず purge される。
	r.SetActorTTL(time.Nanosecond)
	time.Sleep(2 * time.Nanosecond)
	_, err = r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)

	rows, _ := extra.ListByUserID(user.ID)
	assert.Empty(t, rows, "他 host の参照は purge から守らない")
}

// 鍵が読めない actor で fetch を増幅させない。この経路 (TTL 内かつ in-memory
// 鍵キャッシュが空) は解決のたびに走るので、「fetch は成功するが PEM が空」を
// 成功扱いにすると inbound activity 1 件につき outbound fetch が 1 回走り
// 続ける (#2662)。
func TestResolveActor_EmptyPublicKeyDoesNotAmplifyFetches(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"publicKeyPem unreadable", `{ "@context": "https://www.w3.org/ns/activitystreams",
			"id": "https://remote.example/users/alice",
			"type": "Person",
			"preferredUsername": "alice",
			"inbox": "https://remote.example/users/alice/inbox",
			"publicKey": {"id": "https://remote.example/users/alice#main-key", "publicKeyPem": {"a": 1}}
		}`},
		{"publicKey absent", `{ "@context": "https://www.w3.org/ns/activitystreams",
			"id": "https://remote.example/users/alice",
			"type": "Person",
			"preferredUsername": "alice",
			"inbox": "https://remote.example/users/alice/inbox"
		}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			noteRepo := testutil.NewMockNoteRepository()
			urls := activitypub.NewURLBuilder("https://example.com")
			idGen, _ := id.NewGenerator("aidx")
			counting := &countingFetcher{body: []byte(tc.body)}
			r := federation.NewResolver(repo, noteRepo, urls, counting, idGen)

			for i := 0; i < 10; i++ {
				_, err := r.ResolveActor("https://remote.example/users/alice")
				require.NoError(t, err)
			}
			// 内訳: 初回の actor fetch 1 回 + 鍵取り直しの初回 1 回。
			// 以降は keyFetchBackoff が抑える。
			assert.LessOrEqual(t, counting.calls, 2, "解決のたびに fetch しない")
		})
	}
}

// 鍵取得の backoff は「期限が切れたら再挑戦する」ものであって、恒久的に
// 諦めるものではない。あわせて期限切れ entry が map に残り続けないことも
// 見る (残ると単調増加する、#2662)。
func TestResolveActor_KeyFetchBackoffExpires(t *testing.T) {
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"publicKey": {"id": "https://remote.example/users/alice#main-key", "publicKeyPem": {"a": 1}}
	}`
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	counting := &countingFetcher{body: []byte(body)}
	r := federation.NewResolver(repo, noteRepo, urls, counting, idGen)

	base := time.Now()
	now := base
	r.SetClock(func() time.Time { return now })

	for i := 0; i < 5; i++ {
		_, err := r.ResolveActor("https://remote.example/users/alice")
		require.NoError(t, err)
	}
	first := counting.calls
	require.LessOrEqual(t, first, 2, "backoff 中は再 fetch しない")

	// backoff を超えたら 1 回だけ再挑戦する。
	now = base.Add(10 * time.Minute)
	for i := 0; i < 5; i++ {
		_, err := r.ResolveActor("https://remote.example/users/alice")
		require.NoError(t, err)
	}
	assert.Equal(t, first+1, counting.calls, "期限切れ後は 1 回だけ再挑戦する")
}

// 期限切れの backoff entry を残さない。map は外から観測できないので
// テスト用に件数だけ露出している (残ると単調増加する、#2662)。
func TestResolver_KeyFetchFailuresArePruned(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte("{}")}, idGen)

	base := time.Now()
	now := base
	r.SetClock(func() time.Time { return now })

	for i := 0; i < 5; i++ {
		r.MarkKeyFetchFailed(fmt.Sprintf("u%d", i))
	}
	require.Equal(t, 5, r.KeyFetchFailureCount())

	// backoff を過ぎたあとに 1 件足すと、古い 5 件は掃除される。
	now = base.Add(10 * time.Minute)
	r.MarkKeyFetchFailed("u-new")
	assert.Equal(t, 1, r.KeyFetchFailureCount(), "期限切れ entry が残らないこと")
}

// actor が申告する値 (`publicKey.id` / `assertionMethod[].id`) の host 検証も
// `www.` を同一視しない。同一視すると `www` サブドメインを名乗る actor が
// 親ドメインの keyId を宣言でき、keyId 単位の global lookup を使う経路で
// 別 actor を名乗れてしまう (#2662)。upstream も `punyHost` で比較する。
func TestResolveActor_KeyIDHostBindingRejectsWWW(t *testing.T) {
	newR := func(body string) *federation.Resolver {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		return federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	}
	doc := func(keyID string) string {
		return `{ "@context": "https://www.w3.org/ns/activitystreams",
			"id": "https://www.remote.example/users/evil",
			"type": "Person",
			"preferredUsername": "evil",
			"inbox": "https://www.remote.example/users/evil/inbox",
			"publicKey": {"id": "` + keyID + `", "owner": "x", "publicKeyPem": "PEM"}
		}`
	}
	const uri = "https://www.remote.example/users/evil"

	t.Run("parent domain keyID is rejected", func(t *testing.T) {
		r := newR(doc("https://remote.example/users/alice#main-key"))
		_, err := r.ResolveActor(uri)
		assert.ErrorIs(t, err, federation.ErrObjectHostMismatch)
	})
	t.Run("own host keyID is accepted", func(t *testing.T) {
		r := newR(doc("https://www.remote.example/users/evil#main-key"))
		_, err := r.ResolveActor(uri)
		assert.NoError(t, err)
	})
	t.Run("default port is normalized", func(t *testing.T) {
		r := newR(doc("https://www.remote.example:443/users/evil#main-key"))
		_, err := r.ResolveActor(uri)
		assert.NoError(t, err, "既定ポートは同一 host")
	})
}

// **host 不一致でも fetch を増幅させない。** `ErrObjectHostMismatch` は
// document の内容起因なので、相手が直さない限り何度取り直しても同じ。
// `ErrInvalidActor` だけを抑止対象にすると、`publicKey.id` / `inbox` の host が
// 合わない既存行で inbound activity 1 件につき outbound fetch が 1 回走り
// 続ける (#2662)。
func TestResolveActor_HostMismatchStopsRefetchLoop(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	uri := "https://www.remote.example/users/alice"
	old := time.Now().Add(-48 * time.Hour)
	repo.Users["existing"] = &model.User{
		ID:            "existing",
		Username:      "alice",
		URI:           &uri,
		LastFetchedAt: &old,
	}
	// publicKey.id が親ドメイン = sameDeliveryHost で弾かれる。
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://www.remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://www.remote.example/users/alice/inbox",
		"publicKey": {"id": "https://remote.example/users/alice#main-key", "owner": "x", "publicKeyPem": "PEM"}
	}`
	counting := &countingFetcher{body: []byte(body)}
	r := federation.NewResolver(repo, noteRepo, urls, counting, idGen)

	for i := 0; i < 5; i++ {
		_, err := r.ResolveActor(uri)
		require.NoError(t, err, "既存行は返せること")
	}
	assert.LessOrEqual(t, counting.calls, 2, "解決のたびに fetch しない")
	require.NotNil(t, repo.Users["existing"].LastFetchedAt)
	assert.True(t, repo.Users["existing"].LastFetchedAt.After(old))
}

// **WHATWG URL が許す形は落とさない。** upstream の host 検証は `new URL()` で、
// 前後の C0 制御文字 / 空白を除去し tab / CR / LF を全位置で除去する。Go の
// `net/url.Parse` はこれらをエラーにするので、そのままだと「末尾に改行が付いた
// inbox」を出す実装の actor が丸ごと reject される (#2662)。
func TestResolveActor_InboxWithWhitespaceIsAccepted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		inbox string
		ok    bool
	}{
		{"trailing newline", "https://remote.example/users/alice/inbox\n", true},
		{"leading space", " https://remote.example/users/alice/inbox", true},
		{"embedded tab", "https://remote.example/users/al\tice/inbox", true},
		{"embedded newline", "https://remote.example/users/al\nice/inbox", true},
		// 末尾空白は parse は通るが path が `%20` に化けるので、正規化しないと
		// 相手が 404 を返す。
		{"trailing space", "https://remote.example/users/alice/inbox ", true},
		// host が違うものは従来どおり弾く。
		{"other host with newline", "https://evil.example/inbox\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{ "@context": "https://www.w3.org/ns/activitystreams",
				"id": "https://remote.example/users/alice",
				"type": "Person",
				"preferredUsername": "alice",
				"inbox": ` + strconv.Quote(tc.inbox) + `,
				"publicKey": {"publicKeyPem": "PEM"}
			}`
			repo := testutil.NewMockUserRepository()
			r := federation.NewResolver(repo, testutil.NewMockNoteRepository(),
				activitypub.NewURLBuilder("https://example.com"), &stubFetcher{body: []byte(body)}, mustIDGen(t))
			user, err := r.ResolveActor("https://remote.example/users/alice")
			if !tc.ok {
				assert.ErrorIs(t, err, federation.ErrInvalidActor)
				return
			}
			require.NoError(t, err)
			// **保存値も正規化されていること。** 検査だけ緩めて生値を保存すると
			// actor は取り込めるのに配送が永久に成立しない。
			require.NotNil(t, user.Inbox)
			assert.Equal(t, "https://remote.example/users/alice/inbox", *user.Inbox)
			req, reqErr := http.NewRequest(http.MethodPost, *user.Inbox, nil)
			require.NoError(t, reqErr, "配送に使える URL であること")
			assert.Equal(t, "/users/alice/inbox", req.URL.Path, "%20 などに化けていないこと")
		})
	}
}

// **refresh 経路にも media URL のガードが要る。** create 経路だけ守ると、
// 既存行の refresh で atomic UPDATE ごと落ちて `lastFetchedAt` が進まず、
// inbound activity 1 件につき outbound fetch が 1 回走り続ける (#2662)。
func TestResolveActor_TTLRefreshDropsOversizedMediaURLs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		icon  string
		image string
	}{
		{"oversized", "https://remote.example/" + strings.Repeat("c", 1100), "https://remote.example/" + strings.Repeat("c", 600)},
		{"nul", "https://remote.example/a\u0000.png", "https://remote.example/b\u0000.png"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			noteRepo := testutil.NewMockNoteRepository()
			urls := activitypub.NewURLBuilder("https://example.com")
			idGen, _ := id.NewGenerator("aidx")
			uri := "https://remote.example/users/alice"
			old := time.Now().Add(-48 * time.Hour)
			okAvatar := "https://remote.example/ok-avatar.png"
			okBanner := "https://remote.example/ok-banner.png"
			repo.Users["existing"] = &model.User{
				ID: "existing", Username: "alice", URI: &uri,
				AvatarURL: &okAvatar, BannerURL: &okBanner, LastFetchedAt: &old,
			}
			body := `{ "@context": "https://www.w3.org/ns/activitystreams",
				"id": "https://remote.example/users/alice",
				"type": "Person",
				"preferredUsername": "alice",
				"inbox": "https://remote.example/users/alice/inbox",
				"icon": {"type": "Image", "url": ` + strconv.Quote(tc.icon) + `},
				"image": {"type": "Image", "url": ` + strconv.Quote(tc.image) + `},
				"publicKey": {"publicKeyPem": "PEM"}
			}`
			r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
			user, err := r.ResolveActor(uri)
			require.NoError(t, err)
			// 落とすだけで既存値は温存する (空値では上書きしない)。
			require.NotNil(t, user.AvatarURL)
			assert.Equal(t, okAvatar, *user.AvatarURL, "壊れた値で上書きしない")
			require.NotNil(t, user.BannerURL)
			assert.Equal(t, okBanner, *user.BannerURL)
		})
	}
}

// **配送先の正規化は inbox だけでは足りない。** `sharedInbox` は個別 inbox より
// 優先して使われるので、壊れるとそのホスト宛の配送が全部止まる。`publicKey.id`
// は `user_publickey.keyId` の一致キーになる (#2662)。
func TestResolveActor_NormalizesAllDeliveryURLs(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox\n",
		"sharedInbox": "https://remote.example/inbox\n",
		"endpoints": {"sharedInbox": "https://remote.example/endpoints\n"},
		"publicKey": {"id": "https://remote.example/users/alice#main-key\n", "publicKeyPem": "PEM"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	pkRepo := &stubPublickeyRepo{}
	r.SetPublickeyRepo(pkRepo)
	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)

	for name, got := range map[string]*string{"inbox": user.Inbox, "sharedInbox": user.SharedInbox} {
		require.NotNil(t, got, name)
		assert.NotContains(t, *got, "\n", name+" に制御文字が残っていないこと")
		_, reqErr := http.NewRequest(http.MethodPost, *got, nil)
		assert.NoError(t, reqErr, name+" が配送に使えること")
	}
	// sharedInbox は top-level が優先される。
	assert.Equal(t, "https://remote.example/inbox", *user.SharedInbox)

	row, err := pkRepo.FindByUserID(user.ID)
	require.NoError(t, err)
	assert.Equal(t, "https://remote.example/users/alice#main-key", row.KeyID,
		"keyId は HTTP ヘッダ由来の値と一致する形で保存する")

	// **top-level が優先されるので、endpoints 側は別ケースで見る。**
	// Mastodon / Misskey は `endpoints.sharedInbox` だけを publish するので、
	// 実運用ではこちらのほうが通る割合が高い。
	repo2 := testutil.NewMockUserRepository()
	body2 := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/users/bob",
		"type": "Person",
		"preferredUsername": "bob",
		"inbox": "https://remote.example/users/bob/inbox",
		"endpoints": {"sharedInbox": "https://remote.example/endpoints\n"},
		"publicKey": {"publicKeyPem": "PEM"}
	}`
	r2 := federation.NewResolver(repo2, testutil.NewMockNoteRepository(), urls, &stubFetcher{body: []byte(body2)}, idGen)
	user2, err := r2.ResolveActor("https://remote.example/users/bob")
	require.NoError(t, err)
	require.NotNil(t, user2.SharedInbox)
	assert.Equal(t, "https://remote.example/endpoints", *user2.SharedInbox)
	_, reqErr := http.NewRequest(http.MethodPost, *user2.SharedInbox, nil)
	assert.NoError(t, reqErr, "endpoints.sharedInbox が配送に使えること")
}

// `assertionMethod` の keyId も保存前に正規化する。生値のままだと HTTP ヘッダ
// 由来の keyId と一致しないゴミ行になり、しかも自分自身を purge から守る (#2662)。
func TestResolveActor_NormalizesAssertionMethodKeyID(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	mb, err := activitypub.EncodeEd25519Multikey(pub)
	require.NoError(t, err)

	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	// 末尾スペースは url.Parse を通ってしまうので、正規化しないと保存される。
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"publicKey": {"publicKeyPem": "PEM"},
		"assertionMethod": [{"id": "https://remote.example/users/alice#ed25519-key ", "type": "Multikey", "controller": "x", "publicKeyMultibase": "` + mb + `"}]
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	extra := &stubPublickeyExtraRepo{}
	r.SetPublickeyExtraRepo(extra)

	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	rows, _ := extra.ListByUserID(user.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, "https://remote.example/users/alice#ed25519-key", rows[0].KeyID,
		"keyId に空白を残さない")
}

// upstream の `attach.sensitive ??= note.sensitive`。添付側に読める値が無ければ
// note レベルを継ぐ。継がないと NSFW 宣言のノートの画像が非 NSFW で保存される。
func TestExtractAttachments_InheritsNoteSensitive(t *testing.T) {
	docs := federation.ExtractAttachments([]any{
		map[string]any{"type": "Image", "url": "https://r/a.png"},
		map[string]any{"type": "Image", "url": "https://r/b.png", "sensitive": false},
		map[string]any{"type": "Image", "url": "https://r/c.png", "sensitive": "true"},
		// `??=` は null / undefined のときだけ代入する。JSON の明示 null は
		// 「値が無い」側なので note レベルを継ぐ。
		map[string]any{"type": "Image", "url": "https://r/d.png", "sensitive": nil},
	}, true)
	require.Len(t, docs, 4)
	assert.True(t, docs[0].Sensitive, "添付側に無ければ note レベルを継ぐ")
	assert.False(t, docs[1].Sensitive, "添付側の明示 false が勝つ")
	assert.True(t, docs[2].Sensitive, "文字列 true も PostgreSQL 準拠で読む")
	assert.True(t, docs[3].Sensitive, "明示 null は値が無い扱いで note レベルを継ぐ")

	docs2 := federation.ExtractAttachments([]any{
		map[string]any{"type": "Image", "url": "https://r/a.png"},
	}, false)
	require.Len(t, docs2, 1)
	assert.False(t, docs2[0].Sensitive)
}

// 添付の `icon.type` も配列形を受ける (upstream `getApType` と同じ head 方式)。
// **現状 `Document.Icon.Type` を読む下流は無く**、thumbnail は
// `doc.Icon.URL` だけで決まる (resolver.go の upsertAttachments)。1 件目は
// その意味で予防的で、parse 結果を直接固定している (#2665)。
//
// **2 件目は消さないこと。** こちらが固定しているのは icon 固有の値ではなく
// 共有ヘルパー `apTypeOf` の head 方式で、これは添付本体の type 判定
// (`extractAttachments` の switch / PropertyValue 判定) と同じ関数。走査方式に
// すると `{"type": [42, "Image"]}` の添付が skip されずに取り込まれるように
// なる = 観測可能な挙動が変わるが、ツリー内でそれを落とすのはこのテストだけ。
func TestExtractAttachments_ArrayIconType(t *testing.T) {
	docs := federation.ExtractAttachments([]any{
		map[string]any{
			"type": "Document", "url": "https://r/a.mp4", "mediaType": "video/mp4",
			"icon": map[string]any{"type": []any{"Image"}, "url": "https://r/thumb.png"},
		},
		// 先頭が非 string なら空。走査方式にするとここが "Image" になるので、
		// head 方式かどうかはこの 2 件目でしか区別できない。
		map[string]any{
			"type": "Document", "url": "https://r/b.mp4", "mediaType": "video/mp4",
			"icon": map[string]any{"type": []any{42, "Image"}, "url": "https://r/thumb2.png"},
		},
	}, false)
	require.Len(t, docs, 2)
	require.NotNil(t, docs[0].Icon)
	assert.Equal(t, "Image", docs[0].Icon.Type.String(), "配列形の icon type を読む")
	assert.Equal(t, "https://r/thumb.png", docs[0].Icon.URLOrEmpty())
	require.NotNil(t, docs[1].Icon)
	assert.Empty(t, docs[1].Icon.Type.String(), "先頭が非 string なら type は空")
	assert.Equal(t, "https://r/thumb2.png", docs[1].Icon.URLOrEmpty(), "type が空でも thumbnail は残る")
}

// **`id` も正規化する。** upstream は `assertActivityMatchesUrl` が
// `new URL(activity.id)` を通すので末尾改行でも通る。落とすとその actor / Note が
// まったく取り込めない。`id` は全 field の中で最も必ず存在する URL なので、
// inbox に改行を付ける実装は id にも付ける (#2662)。
func TestResolveActor_NormalizesID(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/users/alice\n",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"publicKey": {"publicKeyPem": "PEM"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err, "actor を落とさない")
	require.NotNil(t, user.URI)
	assert.Equal(t, "https://remote.example/users/alice", *user.URI, "uri に制御文字を残さない")
}

// Note の `id` / `attributedTo` も同じ。`note.uri` にそのまま入るので、生値だと
// 後続の FindByURI / host 検証 / 配送先解決が全部ずれる (#2662)。
func TestIngestNote_NormalizesIDAndAttributedTo(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	actorURI := "https://remote.example/users/alice"
	actorBody := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"publicKey": {"publicKeyPem": "PEM"}
	}`
	r := federation.NewResolver(repo, noteRepo, urls, &docFetcher{docs: map[string]string{actorURI: actorBody}}, idGen)
	noteBody := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/notes/1\n",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice\n",
		"content": "hi",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`)
	note, err := r.IngestNote(noteBody)
	require.NoError(t, err, "Note を落とさない")
	assert.Equal(t, "https://remote.example/notes/1", *note.URI, "uri に制御文字を残さない")
}

// **読めない publicKeyPem で既存の鍵を壊さない。** `publicKeyPem` を寛容に
// 読むようにした (#2662) ことで、`{"@value": ...}` のような形の document が
// 「通るが値は空」で到達するようになった。空を書くと in-memory も DB も空に
// なり、**その actor からの inbound HTTP Signature が全て verify 失敗する**
// (refresh のたびに同じ空が書き直されるので自然回復しない)。
func TestResolveActor_UnreadablePublicKeyPemKeepsCachedKey(t *testing.T) {
	const pem = "-----BEGIN PUBLIC KEY-----\nGOOD\n-----END PUBLIC KEY-----"
	doc := func(pemField string) string {
		body := `{ "@context": "https://www.w3.org/ns/activitystreams",
			"id": "https://remote.example/users/alice",
			"type": "Person",
			"preferredUsername": "alice",
			"inbox": "https://remote.example/users/alice/inbox"`
		if pemField != "" {
			body += `, "publicKey": {"id": "https://remote.example/users/alice#main-key", "owner": "x", "publicKeyPem": ` + pemField + `}`
		}
		return body + "}"
	}

	for _, tc := range []struct {
		name   string
		second string
	}{
		// **JSON-LD の展開形は空にならない** (`APLenientString` が剥がして拾う)。
		// `{"@value": ...}` / `[pem]` を fixture に使うと「再取得に成功しただけ」の
		// 真空テストになるので、本当に読めない形だけを並べる。
		{"unreadable object", `{"a": 1}`},
		{"multi element array", "[" + strconv.Quote(pem) + ", " + strconv.Quote(pem) + "]"},
		{"number", `42`},
		{"bool", `true`},
		// これだけは別経路 (keyID が空なので DB 永続化を skip する) を通る。
		// 上の 4 つが空 PEM ガードをゲートしている。
		{"publicKey removed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			noteRepo := testutil.NewMockNoteRepository()
			urls := activitypub.NewURLBuilder("https://example.com")
			idGen, _ := id.NewGenerator("aidx")
			swappable := &swappableFetcher{body: doc(strconv.Quote(pem))}
			r := federation.NewResolver(repo, noteRepo, urls, swappable, idGen)
			pkRepo := &stubPublickeyRepo{}
			r.SetPublickeyRepo(pkRepo)
			r.SetActorTTL(time.Nanosecond)

			user, err := r.ResolveActor("https://remote.example/users/alice")
			require.NoError(t, err)
			got, err := r.PublicKeyForActor(user.ID)
			require.NoError(t, err)
			require.Equal(t, pem, got, "1 回目で鍵が入る")

			swappable.body = doc(tc.second)
			time.Sleep(2 * time.Nanosecond)
			_, err = r.ResolveActor("https://remote.example/users/alice")
			require.NoError(t, err, "actor 自体は取り込めること")

			got, err = r.PublicKeyForActor(user.ID)
			require.NoError(t, err)
			assert.Equal(t, pem, got, "鍵が空で上書きされていないこと")
			// DB 側も壊れていないこと (in-memory だけ守っても TTL 超過で
			// 空の DB 行に落ちる)。
			row, err := pkRepo.FindByUserID(user.ID)
			require.NoError(t, err)
			assert.Equal(t, pem, row.KeyPEM)
		})
	}
}

// inbox / sharedInbox の host は actor に縛る。upstream validateActor と同型で、
// inbox 不一致は Error、sharedInbox 不一致は破棄。ここを見ないと任意の
// リモート actor が第三者ホストを配送先として宣言できる (deliver 側は
// blocklist しか見ない)。#2662 で `endpoints` / `sharedInbox` の受理形を
// 広げたので、あわせて縛りを入れる。
func TestResolveActor_InboxHostBinding(t *testing.T) {
	base := func(inbox, shared, endpoints string) string {
		body := `{ "@context": "https://www.w3.org/ns/activitystreams",
			"id": "https://remote.example/users/alice",
			"type": "Person",
			"preferredUsername": "alice",
			"publicKey": {"publicKeyPem": "PEM"},
			"inbox": "` + inbox + `"`
		if shared != "" {
			body += `, "sharedInbox": "` + shared + `"`
		}
		if endpoints != "" {
			body += `, "endpoints": {"sharedInbox": "` + endpoints + `"}`
		}
		return body + "}"
	}
	newR := func(body string) *federation.Resolver {
		repo := testutil.NewMockUserRepository()
		noteRepo := testutil.NewMockNoteRepository()
		urls := activitypub.NewURLBuilder("https://example.com")
		idGen, _ := id.NewGenerator("aidx")
		return federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	}
	const uri = "https://remote.example/users/alice"

	t.Run("inbox on another host is rejected", func(t *testing.T) {
		r := newR(base("https://evil.example/inbox", "", ""))
		_, err := r.ResolveActor(uri)
		assert.ErrorIs(t, err, federation.ErrInvalidActor)
	})
	t.Run("missing inbox is rejected", func(t *testing.T) {
		r := newR(base("", "", ""))
		_, err := r.ResolveActor(uri)
		assert.ErrorIs(t, err, federation.ErrInvalidActor)
	})
	t.Run("sharedInbox on another host is dropped", func(t *testing.T) {
		r := newR(base("https://remote.example/users/alice/inbox", "https://evil.example/inbox", ""))
		user, err := r.ResolveActor(uri)
		require.NoError(t, err, "actor 自体は作る (upstream も破棄するだけ)")
		assert.Nil(t, user.SharedInbox)
	})
	t.Run("top-level sharedInbox is used when endpoints is absent", func(t *testing.T) {
		// upstream は `x.sharedInbox ?? x.endpoints?.sharedInbox` の順で見る。
		// endpoints しか見ないと top-level のみ publish する実装で束ね配送が
		// 効かない。
		r := newR(base("https://remote.example/users/alice/inbox", "https://remote.example/inbox", ""))
		user, err := r.ResolveActor(uri)
		require.NoError(t, err)
		require.NotNil(t, user.SharedInbox)
		assert.Equal(t, "https://remote.example/inbox", *user.SharedInbox)
	})
	t.Run("endpoints.sharedInbox on another host is dropped", func(t *testing.T) {
		r := newR(base("https://remote.example/users/alice/inbox", "", "https://evil.example/inbox"))
		user, err := r.ResolveActor(uri)
		require.NoError(t, err)
		assert.Nil(t, user.SharedInbox)
	})
	// **`www.` は同一視しない。** `normalizeMatchHost` (object-host binding 用) は
	// upstream の normalizeSynonymousSubdomain 相当で `www.` を剥がすが、
	// upstream の inbox 検証は `punyHost` しか通さない。同一視すると `www`
	// サブドメインが別管理下にある環境で outbound をそちらへ向けられる。
	t.Run("www subdomain inbox is rejected", func(t *testing.T) {
		r := newR(base("https://www.remote.example/users/alice/inbox", "", ""))
		_, err := r.ResolveActor(uri)
		assert.ErrorIs(t, err, federation.ErrInvalidActor)
	})
	t.Run("default port is normalized", func(t *testing.T) {
		r := newR(base("https://remote.example:443/users/alice/inbox", "", ""))
		_, err := r.ResolveActor(uri)
		assert.NoError(t, err, "既定ポートは同一 host")
	})

	t.Run("top-level wins over endpoints", func(t *testing.T) {
		// upstream は `x.sharedInbox ?? x.endpoints?.sharedInbox` の順。
		// 両方が別値で存在する document でしか順序を固定できない。
		r := newR(base("https://remote.example/users/alice/inbox",
			"https://remote.example/top", "https://remote.example/endpoints"))
		user, err := r.ResolveActor(uri)
		require.NoError(t, err)
		require.NotNil(t, user.SharedInbox)
		assert.Equal(t, "https://remote.example/top", *user.SharedInbox)
	})
	t.Run("same host is kept", func(t *testing.T) {
		r := newR(base("https://remote.example/users/alice/inbox", "", "https://remote.example/inbox"))
		user, err := r.ResolveActor(uri)
		require.NoError(t, err)
		require.NotNil(t, user.SharedInbox)
		assert.Equal(t, "https://remote.example/inbox", *user.SharedInbox)
	})
}

// **読めない assertionMethod で既存の鍵を purge しない。** purge は「actor が
// 申告しなかった keyId を消す」= rotation 追従なので、申告を正しく読めた場合に
// しか成立しない。読めない形で空リストになったのを「申告ゼロ」と解釈すると
// キャッシュ済みの Ed25519 鍵を全消しし、Ed25519 のみを publish する相手では
// inbound の署名検証が恒久的に失敗する (相手は同じ形を返し続けるので自然
// 回復しない、#2662)。
//
// 逆に、**読めた申告では purge が動かないと困る**。動かないと
// ローテーション済みの鍵で署名した activity が verify を通り続ける。
// そのため件数ではなく keyId をアサートし、初回に 2 本 seed して
// 「purge が走ったか」を観測できるようにする。
func TestResolveActor_UnreadableAssertionMethodKeepsCachedKeys(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	mb, err := activitypub.EncodeEd25519Multikey(pub)
	require.NoError(t, err)

	const (
		k1 = "https://remote.example/users/alice#k1"
		k2 = "https://remote.example/users/alice#k2"
		k3 = "https://remote.example/users/alice#k3"
	)
	key := func(id, typ string) string {
		return `{"id": "` + id + `", "type": ` + typ + `, "controller": "x", "publicKeyMultibase": "` + mb + `"}`
	}
	doc := func(assertionMethod string) string {
		body := `{ "@context": "https://www.w3.org/ns/activitystreams",
			"id": "https://remote.example/users/alice",
			"type": "Person",
			"preferredUsername": "alice",
			"inbox": "https://remote.example/users/alice/inbox",
			"publicKey": {"id": "https://remote.example/users/alice#main-key", "owner": "x", "publicKeyPem": "-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----"}`
		if assertionMethod != "" {
			body += `, "assertionMethod": ` + assertionMethod
		}
		return body + "}"
	}

	seed := "[" + key(k1, `"Multikey"`) + ", " + key(k2, `"Multikey"`) + "]"

	tests := []struct {
		name       string
		second     string
		wantKeyIDs []string
	}{
		// 読めない形 → purge しない (2 本とも残る)。
		{"string instead of array", `"nonsense"`, []string{k1, k2}},
		{"number", `42`, []string{k1, k2}},
		// 1 件でも読めれば拾う。読めない要素があるので purge はしない。
		// 一括 decode に戻すと k3 が入らない。
		{"one good one broken", "[" + key(k3, `"Multikey"`) + `, {"id": 123}]`, []string{k1, k2, k3}},
		// bare IRI は参照形式として読める → purge が走り k2 が消える。
		// 参照として扱わないと Unreadable になり k2 が残ってしまう。
		{"array of bare IRIs", `["` + k1 + `"]`, []string{k1}},
		// 末尾に空白がある bare IRI。WHATWG URL は落とすので upstream では
		// 同じ keyId を指す。正規化せずに突き合わせると k1 も守れず全消しに
		// なる (#2662)。
		{"padded bare IRI", `["` + k1 + ` "]`, []string{k1}},

		// `"type": ["Multikey"]` も正当な形。string 決め打ちだと Unreadable に
		// なって k3 が入らず、k1 / k2 も purge されない。
		{"array type multikey", "[" + key(k3, `["Multikey"]`) + "]", []string{k3}},
		// 正しい rotation。
		{"field removed", "", nil},
		{"explicit empty array", `[]`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			noteRepo := testutil.NewMockNoteRepository()
			urls := activitypub.NewURLBuilder("https://example.com")
			idGen, _ := id.NewGenerator("aidx")
			swappable := &swappableFetcher{body: doc(seed)}
			r := federation.NewResolver(repo, noteRepo, urls, swappable, idGen)
			extra := &stubPublickeyExtraRepo{}
			r.SetPublickeyExtraRepo(extra)
			r.SetActorTTL(time.Nanosecond)

			user, err := r.ResolveActor("https://remote.example/users/alice")
			require.NoError(t, err)
			rows, _ := extra.ListByUserID(user.ID)
			require.Len(t, rows, 2, "1 回目で 2 本入る")

			swappable.body = doc(tc.second)
			time.Sleep(2 * time.Nanosecond)
			_, err = r.ResolveActor("https://remote.example/users/alice")
			require.NoError(t, err, "actor 自体は取り込めること")

			rows, _ = extra.ListByUserID(user.ID)
			got := make([]string, 0, len(rows))
			for _, row := range rows {
				got = append(got, row.KeyID)
			}
			sort.Strings(got)
			if len(tc.wantKeyIDs) == 0 {
				assert.Empty(t, got)
			} else {
				assert.Equal(t, tc.wantKeyIDs, got)
			}
		})
	}
}

// 同じ actor を refresh する経路で stale keyId が削除されることを検証する。
// 1 回目 ResolveActor で 2 keys を seed → actorTTL を 0 に強制 → 2 回目で
// 1 key だけになった body を返す fetcher に切り替え → 旧 1 key が purge される。
func TestResolveActor_RefreshRemovesStaleAssertionMethod(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	mb, err := activitypub.EncodeEd25519Multikey(pub)
	require.NoError(t, err)

	bodyTwoKeys := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"publicKey": {"id": "https://remote.example/users/alice#main-key", "owner": "x", "publicKeyPem": "-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----"},
		"assertionMethod": [
			{"id": "https://remote.example/users/alice#old-key", "type": "Multikey", "controller": "x", "publicKeyMultibase": "` + mb + `"},
			{"id": "https://remote.example/users/alice#new-key", "type": "Multikey", "controller": "x", "publicKeyMultibase": "` + mb + `"}
		]
	}`
	bodyOneKey := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"publicKey": {"id": "https://remote.example/users/alice#main-key", "owner": "x", "publicKeyPem": "-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----"},
		"assertionMethod": [
			{"id": "https://remote.example/users/alice#new-key", "type": "Multikey", "controller": "x", "publicKeyMultibase": "` + mb + `"}
		]
	}`

	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	swappable := &swappableFetcher{body: bodyTwoKeys}
	r := federation.NewResolver(repo, noteRepo, urls, swappable, idGen)
	extra := &stubPublickeyExtraRepo{}
	r.SetPublickeyExtraRepo(extra)
	r.SetActorTTL(time.Nanosecond) // 即時 refresh をトリガするため極短 TTL

	// 1 回目: 2 keys が seed される
	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	rows, _ := extra.ListByUserID(user.ID)
	require.Len(t, rows, 2)

	// 2 回目: fetcher を 1 key body に差し替えて refresh をトリガ (TTL 失効済)
	swappable.body = bodyOneKey
	time.Sleep(2 * time.Nanosecond) // TTL を確実に超過させる
	_, err = r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)

	rows, _ = extra.ListByUserID(user.ID)
	require.Len(t, rows, 1, "stale #old-key は purge されて #new-key のみ残る")
	assert.Equal(t, "https://remote.example/users/alice#new-key", rows[0].KeyID)
}

// swappableFetcher は body を test 中に差し替えられる stubFetcher 派生。
type swappableFetcher struct {
	body string
}

func (s *swappableFetcher) FetchObject(_ string) ([]byte, error) {
	return []byte(s.body), nil
}

// publickeyExtraRepo 未配線でも PublicKeyForKeyID は PublicKeyForActor と
// 等価な挙動を保つ (= drop-in 互換)。
func TestPublicKeyForKeyID_WithoutExtraRepoFallsBackToActor(t *testing.T) {
	r, _ := newResolver(t, sampleActor, nil)
	pkRepo := &stubPublickeyRepo{entries: map[string]*model.UserPublickey{}}
	r.SetPublickeyRepo(pkRepo)
	require.NoError(t, pkRepo.Upsert(&model.UserPublickey{
		UserID: "alice", KeyID: "https://remote.example/users/alice#main-key",
		KeyPEM: "-----BEGIN PUBLIC KEY-----\nFALLBACK\n-----END PUBLIC KEY-----",
	}))

	pem, err := r.PublicKeyForKeyID("alice", "https://remote.example/users/alice#ed25519-key")
	require.NoError(t, err)
	assert.Contains(t, pem, "FALLBACK")
}

// 複数 goroutine が異なる actor の PublicKeyForActor を並行に呼ぶと、内部
// keys map が無ロックだと race detector / 実行時 panic が発火する。
// Devin review #555 FLAG-1 への対策として keysMu を入れたので、並行アクセス
// が clean に動くことを race detector で担保する。本テストは r.keys のみを
// 対象にして mock userRepo の race を巻き込まないように、PublickeyForActor
// 経路だけを叩く (DB fallback で r.keys 書き込みを発火させる)。
func TestResolver_KeysMapConcurrentAccessIsRaceFree(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	pkRepo := &stubPublickeyRepo{entries: map[string]*model.UserPublickey{}}
	for i := 0; i < 4; i++ {
		uid := fmt.Sprintf("u%d", i)
		_ = pkRepo.Upsert(&model.UserPublickey{UserID: uid, KeyID: "k" + uid, KeyPEM: "pem" + uid})
	}
	r.SetPublickeyRepo(pkRepo)

	// 8 並行で異なる userID の PublicKeyForActor を呼ぶ。in-memory cache
	// miss → DB fallback → r.keys 書き込み が並行で走る。
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		uid := fmt.Sprintf("u%d", i%4)
		wg.Add(1)
		go func(uid string) {
			defer wg.Done()
			_, _ = r.PublicKeyForActor(uid)
		}(uid)
	}
	wg.Wait()
	// 検証ポイントは race detector が黙ること。
}

// upstream Misskey #17167 (= 2026.5.0 fix / triage #1004): mentionLimit を
// 超える inbound Note は ErrContainsTooManyMentions を返して保存せず、caller
// (processor.handleCreate) が non-retry skip 化する。21 件の local user URI を
// tag 配列に詰めて ExtractLocalUserID 経由で resolveMentionedUserIDs が
// 全件 ID を返すよう仕込み、limit 超え (= corenote.DefaultMentionLimit) を発火させる。
func TestIngestNote_MentionLimitExceededReturnsSentinel(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	// 21 件 = limit (20) + 1 の local user URI を tag に並べる。Mock urls の
	// UserURI prefix は "https://example.com/users/" なので ExtractLocalUserID
	// が "u1".."u21" を返す。
	var tagJSON string
	for i := 1; i <= corenote.DefaultMentionLimit+1; i++ {
		if i > 1 {
			tagJSON += ","
		}
		tagJSON += `{"type": "Mention", "href": "https://example.com/users/u` + strconv.Itoa(i) + `"}`
	}
	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/over",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "x",
		"to": ["https://www.w3.org/ns/activitystreams#Public"],
		"tag": [` + tagJSON + `]
	}`)
	_, err := r.IngestNote(body)
	require.ErrorIs(t, err, corenote.ErrContainsTooManyMentions)
	// 未保存であることも確認 (= queue retry 経由で再試行されても結果は同じ)。
	_, lookupErr := noteRepo.FindByURI("https://remote.example/notes/over")
	require.Error(t, lookupErr, "limit exceed note should NOT be persisted")
}

// #2069 (upstream #17576): 解決できたユーザー数が少なくても、raw な AP tag mention
// 数 (href ユニーク数) が limit を超えれば reject する。21 件の未知 remote mention URI
// は resolveMentionedUserIDs で全て skip され note.Mentions=0 になるが、raw count=21 が
// limit を超えるので ErrContainsTooManyMentions を返す (修正前は 0 ≤ 20 で受理されていた)。
func TestIngestNote_MentionLimitRawCountExceeded(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	var tagJSON string
	for i := 1; i <= corenote.DefaultMentionLimit+1; i++ {
		if i > 1 {
			tagJSON += ","
		}
		// 未知の remote URI (repo に無い) なので resolveMentionedUserIDs は skip する。
		tagJSON += `{"type": "Mention", "href": "https://remote.example/users/m` + strconv.Itoa(i) + `"}`
	}
	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/notes/rawover",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "x",
		"to": ["https://www.w3.org/ns/activitystreams#Public"],
		"tag": [` + tagJSON + `]
	}`)
	_, err := r.IngestNote(body)
	require.ErrorIs(t, err, corenote.ErrContainsTooManyMentions)
	_, lookupErr := noteRepo.FindByURI("https://remote.example/notes/rawover")
	require.Error(t, lookupErr, "raw mention 超過 note は保存されない")
}

// 境界条件 (= limit ぴったりの 20 件) は受理される。off-by-one 検出。
func TestIngestNote_MentionLimitBoundaryAccepted(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	var tagJSON string
	for i := 1; i <= corenote.DefaultMentionLimit; i++ {
		if i > 1 {
			tagJSON += ","
		}
		tagJSON += `{"type": "Mention", "href": "https://example.com/users/u` + strconv.Itoa(i) + `"}`
	}
	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/boundary",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "x",
		"to": ["https://www.w3.org/ns/activitystreams#Public"],
		"tag": [` + tagJSON + `]
	}`)
	note, err := r.IngestNote(body)
	require.NoError(t, err)
	require.NotNil(t, note)
	assert.Len(t, note.Mentions, corenote.DefaultMentionLimit)
}

// TestResolveActor_ExistingUserRefreshesHashtags は actor 再取得 (refreshActor)
// で person.tag の変化が user.tags に追従することを確認する (#1360, Part 2)。
// 自己紹介の hashtag を編集した remote user が hashtags/users に反映されるための前提。
func TestResolveActor_ExistingUserRefreshesHashtags(t *testing.T) {
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"tag": [{"type": "Hashtag", "name": "#Refreshed"}],
		"publicKey": {"publicKeyPem": "FAKE"}
	}`
	r, repo := newResolver(t, body, nil)
	uri := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["existing"] = &model.User{
		ID:       "existing",
		Username: "alice",
		URI:      &uri,
		Host:     &host,
		Tags:     []string{"stale"},
	}
	_, err := r.ResolveActor(uri)
	require.NoError(t, err)
	// refresh で旧 tags ("stale") が person.tag 由来の正規化済み tag に置き換わる。
	assert.Equal(t, []string{"refreshed"}, []string(repo.Users["existing"].Tags))
}

// TestResolveActor_NewUserIngestsHashtags は remote actor の person.tag の
// Hashtag entry が正規化されて user.tags に取り込まれることを確認する
// (#1360, Part 2)。hashtags/users が remote user を引けるための前提。
func TestResolveActor_NewUserIngestsHashtags(t *testing.T) {
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/users/tagger",
		"type": "Person",
		"preferredUsername": "tagger",
		"inbox": "https://remote.example/users/tagger/inbox",
		"summary": "<p>bio</p>",
		"tag": [
			{"type": "Hashtag", "name": "#Golang", "href": "https://remote.example/tags/golang"},
			{"type": "Hashtag", "name": "#ActivityPub"},
			{"type": "Emoji", "name": ":party:"}
		],
		"publicKey": {"publicKeyPem": "FAKE"}
	}`
	r, _ := newResolver(t, body, nil)
	user, err := r.ResolveActor("https://remote.example/users/tagger")
	require.NoError(t, err)
	// Hashtag のみ正規化して取り込む (Emoji は除外)。
	assert.ElementsMatch(t, []string{"golang", "activitypub"}, []string(user.Tags))
}

// --- #1527: inbound quote (引用) renote resolution ---

// _misskey_quote が既存 note を指す inbound note は、その note を renote として
// 紐付ける (fetch 不要 = DB hit)。
func TestIngestNote_QuoteMisskeyQuote(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	quotedURI := "https://remote.example/notes/quoted"
	noteRepo.Notes["quoted1"] = &model.Note{ID: "quoted1", URI: &quotedURI, UserID: "qu", UserHost: strPtr("remote.example")}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	// attributedTo の actor 解決のみ fetch (quote は DB hit)。
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/q1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "見て",
		"_misskey_quote": "https://remote.example/notes/quoted",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	got, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	require.NotNil(t, got.RenoteID)
	assert.Equal(t, "quoted1", *got.RenoteID)
	require.NotNil(t, got.RenoteUserID)
	assert.Equal(t, "qu", *got.RenoteUserID)
	require.NotNil(t, got.Text)
	assert.Equal(t, "見て", *got.Text)
}

// quoteUrl は _misskey_quote が無いときの fallback。
func TestIngestNote_QuoteUrlFallback(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	quotedURI := "https://remote.example/notes/quoted"
	noteRepo.Notes["quoted1"] = &model.Note{ID: "quoted1", URI: &quotedURI, UserID: "qu"}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/q1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "quote via quoteUrl",
		"quoteUrl": "https://remote.example/notes/quoted",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	got, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	require.NotNil(t, got.RenoteID)
	assert.Equal(t, "quoted1", *got.RenoteID)
}

// quote URI が未知 (DB 未取り込み) なら fetch して取り込み、renote 紐付けする。
func TestIngestNote_QuoteFetchesUnknownTarget(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	// 1: outer note の actor 解決 / 2: quote 先 note / 3: quote 先 note の actor 解決。
	quotedNote := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/quoted",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "original",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	fetcher := &scriptedFetcher{bodies: [][]byte{[]byte(sampleActor), []byte(quotedNote), []byte(sampleActor)}}
	r := federation.NewResolver(repo, noteRepo, urls, fetcher, idGen)

	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/q1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "quote of unknown",
		"_misskey_quote": "https://remote.example/notes/quoted",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	got, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	require.NotNil(t, got.RenoteID, "unknown quote target should be fetched and linked")
}

// quote field が無いノートは RenoteID nil (通常の投稿)。
func TestIngestNote_NoQuote(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	got, err := r.IngestNote([]byte(sampleRemoteNote))
	require.NoError(t, err)
	assert.Nil(t, got.RenoteID)
}

// 解決不能な quote (fetch 失敗) は best-effort で quote 無し扱い (ingest は成功)。
func TestIngestNote_QuoteUnresolvableDegrades(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	// actor は解決できるが quote fetch は失敗する。
	fetcher := &scriptedFetcher{bodies: [][]byte{[]byte(sampleActor)}}
	r := federation.NewResolver(repo, noteRepo, urls, fetcher, idGen)
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/q1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "quote of gone",
		"_misskey_quote": "https://remote.example/notes/gone",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	got, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	assert.Nil(t, got.RenoteID, "unresolvable quote degrades to plain note")
	require.NotNil(t, got.Text)
}

// quote cycle (A が B を quote し B が A を quote) でも無限再帰せず両者を ingest する。
// 後から辿る側 (cycle 検出) は quote 未解決で degrade する (#1527 在庫 guard)。
func TestIngestNote_QuoteCycleNoInfiniteRecursion(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	// A.ingest: actor(alice) fetch → quote B fetch → B.ingest: actor は DB hit、
	// quote A は in-flight で skip。よって body は [actor, B-note] で足りる。
	bNote := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/B",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "B quotes A",
		"_misskey_quote": "https://remote.example/notes/A",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	fetcher := &scriptedFetcher{bodies: [][]byte{[]byte(sampleActor), []byte(bNote)}}
	r := federation.NewResolver(repo, noteRepo, urls, fetcher, idGen)
	aBody := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/A",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "A quotes B",
		"_misskey_quote": "https://remote.example/notes/B",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	done := make(chan struct{})
	var got *model.Note
	var err error
	go func() {
		got, err = r.IngestNote([]byte(aBody))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("IngestNote did not terminate (quote cycle caused infinite recursion)")
	}
	require.NoError(t, err)
	require.NotNil(t, got.RenoteID, "A should link its quote B")
	// 両 note が永続化されている。
	assert.GreaterOrEqual(t, len(noteRepo.Notes), 2)
	// cycle を辿った側 (B) は quote 未解決で degrade している (in-flight guard が
	// A への再 fetch を遮断したため)。
	var bNoteRow *model.Note
	for _, n := range noteRepo.Notes {
		if n.URI != nil && *n.URI == "https://remote.example/notes/B" {
			bNoteRow = n
		}
	}
	require.NotNil(t, bNoteRow, "B should be persisted")
	assert.Nil(t, bNoteRow.RenoteID, "B's quote (back to A) is broken by the cycle guard")
}

// remote note が LOCAL note を quote する場合、URI から local ID を抽出して
// fetch 無しで紐付ける (resolveQuoteTarget の extractLocalNoteID 経路)。
func TestIngestNote_QuoteLocalNote(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["localq1"] = &model.Note{ID: "localq1", UserID: "localuser"}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/q1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "quoting a local note",
		"_misskey_quote": "https://example.com/notes/localq1",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	got, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	require.NotNil(t, got.RenoteID)
	assert.Equal(t, "localq1", *got.RenoteID)
}

// local quote URI だが DB に存在しないなら quote 無し扱い (fetch しない)。
func TestIngestNote_QuoteLocalNoteMissing(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/q1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "quoting a missing local note",
		"_misskey_quote": "https://example.com/notes/ghost",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	got, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	assert.Nil(t, got.RenoteID)
}

// 引用は対象 note の renoteCount を increment する (本家 NoteCreateService:786 準拠)。
// あわせて RenoteUserHost が denormalize される。
func TestIngestNote_QuoteIncrementsRenoteCountAndSetsHost(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	quotedURI := "https://remote.example/notes/quoted"
	noteRepo.Notes["quoted1"] = &model.Note{ID: "quoted1", URI: &quotedURI, UserID: "qu", UserHost: strPtr("other.example")}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/q1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "quote",
		"_misskey_quote": "https://remote.example/notes/quoted",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	got, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	require.NotNil(t, got.RenoteUserHost)
	assert.Equal(t, "other.example", *got.RenoteUserHost)
	assert.Equal(t, int16(1), noteRepo.Notes["quoted1"].RenoteCount, "quote は対象の renoteCount を増分する")
}

// 自分の note を自己引用した場合は renoteCount を増やさない (本家 userId !== user.id)。
func TestIngestNote_SelfQuoteNoIncrement(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	quotedURI := "https://remote.example/notes/quoted"
	// quote 先の作者 = 引用する actor (alice) と同一。
	noteRepo.Notes["quoted1"] = &model.Note{ID: "quoted1", URI: &quotedURI, UserID: "ualice", UserHost: strPtr("remote.example")}
	repo.Users["ualice"] = &model.User{ID: "ualice", Username: "alice", Host: strPtr("remote.example"), URI: strPtr("https://remote.example/users/alice")}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/q1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "self quote",
		"_misskey_quote": "https://remote.example/notes/quoted",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	got, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	require.NotNil(t, got.RenoteID, "自己引用でも quote 紐付け自体はする")
	assert.Equal(t, int16(0), noteRepo.Notes["quoted1"].RenoteCount, "自己引用は renoteCount を増やさない")
}

// reply と quote を併せ持つ note は ReplyID / RenoteID の両方が立つ。
func TestIngestNote_ReplyAndQuote(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	replyURI := "https://remote.example/notes/reply-target"
	quotedURI := "https://remote.example/notes/quoted"
	noteRepo.Notes["rt"] = &model.Note{ID: "rt", URI: &replyURI, UserID: "ru"}
	noteRepo.Notes["quoted1"] = &model.Note{ID: "quoted1", URI: &quotedURI, UserID: "qu"}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/q1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "reply + quote",
		"inReplyTo": "https://remote.example/notes/reply-target",
		"_misskey_quote": "https://remote.example/notes/quoted",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	got, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	require.NotNil(t, got.ReplyID)
	assert.Equal(t, "rt", *got.ReplyID)
	require.NotNil(t, got.RenoteID)
	assert.Equal(t, "quoted1", *got.RenoteID)
}

// _misskey_quote が解決できなくても quoteUrl で解決できれば quote を採用する
// (本家は両 URI を試して最初に成功したものを使う)。
func TestIngestNote_QuotePrefersResolvableUrl(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	quotedURI := "https://example.com/notes/localq" // quoteUrl 側 (local, 解決可)
	_ = quotedURI
	noteRepo.Notes["localq"] = &model.Note{ID: "localq", UserID: "lu"}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	// _misskey_quote は未知 remote (fetch 失敗); quoteUrl は local 解決可。
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/q1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "two quote uris",
		"_misskey_quote": "https://remote.example/notes/gone",
		"quoteUrl": "https://example.com/notes/localq",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	got, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	require.NotNil(t, got.RenoteID, "_misskey_quote 失敗時は quoteUrl にフォールバックする")
	assert.Equal(t, "localq", *got.RenoteID)
}

// 引用先が followers-only のローカル note の場合は紐付けず degrade する
// (本家 NoteCreateService:346-352 が他人の followers note を renote 対象から
// reject するのと同 doctrine)。非可視 note が renote embed 経由で本来見られない
// viewer へ broadcast される IDOR を防ぐ (#1534 / #1532 regression)。
// renoteCount も増分しない。
func TestIngestNote_QuoteFollowersTargetDegrades(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	quotedURI := "https://remote.example/notes/quoted"
	noteRepo.Notes["quoted1"] = &model.Note{ID: "quoted1", URI: &quotedURI, UserID: "qu", Visibility: model.NoteVisibilityFollowers}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/q1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "quoting a followers-only note",
		"_misskey_quote": "https://remote.example/notes/quoted",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	got, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	assert.Nil(t, got.RenoteID, "followers note は inbound quote から紐付けない (IDOR 防御)")
	assert.Equal(t, int16(0), noteRepo.Notes["quoted1"].RenoteCount, "degrade 時は renoteCount を増やさない")
}

// 引用先が specified(DM) のローカル note の場合も紐付けず degrade する
// (本家は specified を常に renote 対象から reject)。最も機微な DM 本文が embed
// 経由で漏れるのを防ぐ (#1534)。
func TestIngestNote_QuoteSpecifiedTargetDegrades(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	quotedURI := "https://remote.example/notes/dm"
	noteRepo.Notes["dm1"] = &model.Note{ID: "dm1", URI: &quotedURI, UserID: "qu", Visibility: model.NoteVisibilitySpecified}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/q1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "quoting a DM",
		"_misskey_quote": "https://remote.example/notes/dm",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	got, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	assert.Nil(t, got.RenoteID, "specified(DM) note は inbound quote から紐付けない (IDOR 防御)")
	assert.Equal(t, int16(0), noteRepo.Notes["dm1"].RenoteCount)
}

// home 可視性の引用先は従来どおり紐付ける (denylist は followers / specified のみ弾く)。
func TestIngestNote_QuoteHomeTargetLinks(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	quotedURI := "https://remote.example/notes/quoted"
	noteRepo.Notes["quoted1"] = &model.Note{ID: "quoted1", URI: &quotedURI, UserID: "qu", Visibility: model.NoteVisibilityHome}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/q1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "quoting a home note",
		"_misskey_quote": "https://remote.example/notes/quoted",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	got, err := r.IngestNote([]byte(body))
	require.NoError(t, err)
	require.NotNil(t, got.RenoteID, "home note は引用可能なので紐付ける")
	assert.Equal(t, "quoted1", *got.RenoteID)
}

// raceConflictNoteRepo simulates the dedup race: the top-of-IngestNote FindByURI
// misses, Create then fails with a UNIQUE violation (another ingest won the
// race), and a subsequent FindByURI returns the winner's row.
type raceConflictNoteRepo struct {
	*testutil.MockNoteRepository
	existing        *model.Note
	createAttempted bool
}

func (r *raceConflictNoteRepo) FindByURI(uri string) (*model.Note, error) {
	if r.createAttempted && r.existing != nil && r.existing.URI != nil && *r.existing.URI == uri {
		return r.existing, nil
	}
	return nil, testutil.ErrNotFound
}

func (r *raceConflictNoteRepo) Create(_ *model.Note) error {
	r.createAttempted = true
	// resolver は err 文字列を解釈せず「Create 失敗 + 直後 FindByURI で行が引ける」を
	// dedup race と判定するため、この SQLSTATE 文言は cosmetic (どんな err でも同経路)。
	return errors.New(`ERROR: duplicate key value violates unique constraint "IDX_note_uri" (SQLSTATE 23505)`)
}

// note.uri UNIQUE 制約違反 (dedup race) は重複 INSERT エラーにせず、既存行を引いて
// dedup hit (created=false) として返す (#1527 review #2)。created=false により
// caller (chart hook 等) と renoteCount 増分が二重発火しない。
func TestIngestNote_DedupRaceOnUniqueViolation(t *testing.T) {
	base := testutil.NewMockNoteRepository()
	existingURI := "https://remote.example/notes/n1"
	winner := &model.Note{ID: "winner", URI: &existingURI, UserID: "ualice"}
	raceRepo := &raceConflictNoteRepo{MockNoteRepository: base, existing: winner}
	repo := testutil.NewMockUserRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, raceRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	got, created, err := r.IngestNoteWithCreated([]byte(sampleRemoteNote), "")
	require.NoError(t, err, "unique violation should be treated as dedup hit, not an error")
	assert.False(t, created, "race loser must be created=false so hooks/renoteCount don't double-fire")
	require.NotNil(t, got)
	assert.Equal(t, "winner", got.ID, "should return the row the winning ingest created")
}

// Create が UNIQUE 違反以外で失敗し、FindByURI でも引けない場合は元の err を返す。
func TestIngestNote_CreateErrorNonDedupPropagates(t *testing.T) {
	base := testutil.NewMockNoteRepository()
	// existing なし → 競合後の FindByURI も miss → 元の err を伝播。
	raceRepo := &raceConflictNoteRepo{MockNoteRepository: base, existing: nil}
	repo := testutil.NewMockUserRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, raceRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	_, _, err := r.IngestNoteWithCreated([]byte(sampleRemoteNote), "")
	assert.Error(t, err, "non-dedup create error must propagate")
}

// dedup race が quote note で起きても、引用元の renoteCount は二重増分されない
// (Create 失敗 → dedup hit で increment 経路に到達しない, #1527 review #2)。
func TestIngestNote_DedupRaceOnQuoteNoDoubleRenoteCount(t *testing.T) {
	base := testutil.NewMockNoteRepository()
	// 引用先 (local note)。renoteCount 初期値 0。
	base.Notes["q1"] = &model.Note{ID: "q1", UserID: "qu", RenoteCount: 0}
	existingURI := "https://remote.example/notes/n1"
	winner := &model.Note{ID: "winner", URI: &existingURI, UserID: "ualice"}
	raceRepo := &raceConflictNoteRepo{MockNoteRepository: base, existing: winner}
	repo := testutil.NewMockUserRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, raceRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := `{ "@context": "https://www.w3.org/ns/activitystreams", 
		"id": "https://remote.example/notes/n1",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "quote that loses the race",
		"_misskey_quote": "https://example.com/notes/q1",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`
	got, created, err := r.IngestNoteWithCreated([]byte(body), "")
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, "winner", got.ID)
	assert.Equal(t, int16(0), base.Notes["q1"].RenoteCount, "dedup race must NOT increment the quote target's renoteCount")
}

// movedAt は移行が起きた瞬間だけ打刻する (#2412)。upstream
// ApPersonService.updatePerson の `moving` フラグと同じ判定で、無→有 と
// 有→別の値 のときだけ更新する。
//
// 同じ移行先を宣言し続けている actor を再取得するたびに打ち直していると、
// movedAt を基準にする時間窓 (移行直後 2h の import 上限緩和 / 14 日の移行
// クールダウン) が永久に開いたままになる。
func TestRefreshActor_StampsMovedAtOnlyOnTransition(t *testing.T) {
	const uri = "https://remote.example/users/x"
	old := "https://old.example/users/x"
	older := time.Now().Add(-72 * time.Hour)

	cases := []struct {
		name string
		// 既存行が宣言している移行先 (nil = 未移行)
		existingMovedTo *string
		// actor JSON が宣言する移行先 ("" = movedTo 無し)
		actorMovedTo string
		wantStamped  bool
		wantMovedTo  string
	}{
		{
			name:            "無→有 で打刻する",
			existingMovedTo: nil,
			actorMovedTo:    "https://new.example/users/x",
			wantStamped:     true,
			wantMovedTo:     "https://new.example/users/x",
		},
		{
			name:            "有→別の値 で打ち直す",
			existingMovedTo: &old,
			actorMovedTo:    "https://new.example/users/x",
			wantStamped:     true,
			wantMovedTo:     "https://new.example/users/x",
		},
		{
			name:            "同じ移行先の再取得では触らない",
			existingMovedTo: &old,
			actorMovedTo:    old,
			wantStamped:     false,
			wantMovedTo:     old,
		},
		{
			// upstream は movedToUri を null に戻すが mk-go は温存する。
			// 一時的な欠落でクリアすると、次の取得が「無→有」に見えて
			// movedAt が打ち直され、この修正が骨抜きになるため。
			name:            "移行先が消えても温存し打刻しない",
			existingMovedTo: &old,
			actorMovedTo:    "",
			wantStamped:     false,
			wantMovedTo:     old,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			stale := time.Now().Add(-48 * time.Hour) // TTL 超過で refreshActor を発火
			repo.Users["existing"] = &model.User{
				ID:            "existing",
				Username:      "x",
				URI:           strPtr(uri),
				LastFetchedAt: &stale,
				MovedToURI:    tc.existingMovedTo,
				MovedAt:       &older,
			}
			movedToField := ""
			if tc.actorMovedTo != "" {
				movedToField = `, "movedTo": "` + tc.actorMovedTo + `"`
			}
			body := `{ "@context": "https://www.w3.org/ns/activitystreams",
				"id": "` + uri + `",
				"type": "Person",
				"preferredUsername": "x",
				"inbox": "` + uri + `/inbox",
				"publicKey": {"publicKeyPem": "FAKE"}` + movedToField + `
			}`
			noteRepo := testutil.NewMockNoteRepository()
			urls := activitypub.NewURLBuilder("https://example.com")
			idGen, _ := id.NewGenerator("aidx")
			r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)

			user, err := r.ResolveActor(uri)
			require.NoError(t, err)
			require.NotNil(t, user.MovedToURI)
			assert.Equal(t, tc.wantMovedTo, *user.MovedToURI)
			require.NotNil(t, user.MovedAt)

			if tc.wantStamped {
				assert.True(t, user.MovedAt.After(older),
					"movedAt must be re-stamped when the move target changes")
			} else {
				assert.True(t, user.MovedAt.Equal(older),
					"movedAt must not move when the actor keeps declaring the same target")
			}

			// DB 側にも同じ判定が反映されていること (fields map を経由するので
			// メモリ上の struct だけ正しくても意味がない)。
			persisted := repo.Users["existing"]
			require.NotNil(t, persisted.MovedAt)
			if tc.wantStamped {
				assert.True(t, persisted.MovedAt.After(older), "persisted movedAt must be re-stamped")
			} else {
				assert.True(t, persisted.MovedAt.Equal(older), "persisted movedAt must be untouched")
			}
		})
	}
}

// fakeMoveProcessor records PostMoveProcess invocations.
type fakeMoveProcessor struct {
	calls    int
	src, dst *model.User
}

func (f *fakeMoveProcessor) PostMoveProcess(src, dst *model.User) {
	f.calls++
	f.src, f.dst = src, dst
}

// リモートアカウントの移行を検知したら引き継ぎを起動する (#2414)。
//
// **alsoKnownAs の相互確認が security boundary。** 移行先が移行元を
// alsoKnownAs で名乗っていない限り引き継ぎに入らない。これを省くと、任意の
// actor が movedTo で他人を指すだけでそのフォロワーを奪える。
func TestRefreshActor_ProcessesRemoteMove(t *testing.T) {
	const srcURI = "https://remote.example/users/src"
	const dstURI = "https://elsewhere.example/users/dst"

	cases := []struct {
		name string
		// 移行先ユーザーの既存行 (nil なら DB に居ない)
		dst      *model.User
		wantCall bool
	}{
		{
			name: "alsoKnownAs が移行元を含めば引き継ぐ",
			dst: &model.User{
				ID: "dstUser", URI: strPtr(dstURI), AlsoKnownAs: strPtr(srcURI),
			},
			wantCall: true,
		},
		{
			name: "alsoKnownAs に他の URI しか無ければ弾く",
			dst: &model.User{
				ID: "dstUser", URI: strPtr(dstURI),
				AlsoKnownAs: strPtr("https://other.example/users/someone"),
			},
			wantCall: false,
		},
		{
			name: "alsoKnownAs が空なら弾く",
			dst:  &model.User{ID: "dstUser", URI: strPtr(dstURI)},
		},
		{
			name: "移行先がさらに移行元を指していたら弾く (循環)",
			dst: &model.User{
				ID: "dstUser", URI: strPtr(dstURI), AlsoKnownAs: strPtr(srcURI),
				MovedToURI: strPtr(srcURI),
			},
			wantCall: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			stale := time.Now().Add(-48 * time.Hour)
			repo.Users["existing"] = &model.User{
				ID: "existing", Username: "src", URI: strPtr(srcURI), LastFetchedAt: &stale,
			}
			if tc.dst != nil {
				repo.Users["dstUser"] = tc.dst
			}
			body := `{ "@context": "https://www.w3.org/ns/activitystreams",
				"id": "` + srcURI + `",
				"type": "Person",
				"preferredUsername": "src",
				"inbox": "` + srcURI + `/inbox",
				"movedTo": "` + dstURI + `",
				"publicKey": {"publicKeyPem": "FAKE"}
			}`
			urls := activitypub.NewURLBuilder("https://local.example")
			idGen, _ := id.NewGenerator("aidx")
			r := federation.NewResolver(repo, testutil.NewMockNoteRepository(), urls,
				&stubFetcher{body: []byte(body)}, idGen)
			mp := &fakeMoveProcessor{}
			r.SetMoveProcessor(mp)

			_, err := r.ResolveActor(srcURI)
			require.NoError(t, err)

			if tc.wantCall {
				require.Equal(t, 1, mp.calls, "the move must be carried over")
				assert.Equal(t, "existing", mp.src.ID)
				assert.Equal(t, "dstUser", mp.dst.ID)
			} else {
				assert.Zero(t, mp.calls, "the move must NOT be carried over")
			}
		})
	}
}

// 同じ移行先を宣言し続けている actor の再取得では起動しない。#2412 で movedAt を
// 遷移時だけ打刻するようにしたので、2 回目以降は movedThisRefresh が false になる。
func TestRefreshActor_RemoteMoveNotRetriggeredOnSameTarget(t *testing.T) {
	const srcURI = "https://remote.example/users/src2"
	const dstURI = "https://elsewhere.example/users/dst2"

	repo := testutil.NewMockUserRepository()
	stale := time.Now().Add(-48 * time.Hour)
	repo.Users["existing"] = &model.User{
		ID: "existing", Username: "src", URI: strPtr(srcURI), LastFetchedAt: &stale,
		// 既に同じ移行先を記録済み
		MovedToURI: strPtr(dstURI),
	}
	repo.Users["dstUser"] = &model.User{
		ID: "dstUser", URI: strPtr(dstURI), AlsoKnownAs: strPtr(srcURI),
	}
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "` + srcURI + `",
		"type": "Person",
		"preferredUsername": "src",
		"inbox": "` + srcURI + `/inbox",
		"movedTo": "` + dstURI + `",
		"publicKey": {"publicKeyPem": "FAKE"}
	}`
	urls := activitypub.NewURLBuilder("https://local.example")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, testutil.NewMockNoteRepository(), urls,
		&stubFetcher{body: []byte(body)}, idGen)
	mp := &fakeMoveProcessor{}
	r.SetMoveProcessor(mp)

	_, err := r.ResolveActor(srcURI)
	require.NoError(t, err)
	assert.Zero(t, mp.calls, "re-declaring the same destination is not a new move")
}

// 自分自身への移行は無視する。
func TestRefreshActor_RemoteMoveToSelfIsIgnored(t *testing.T) {
	const srcURI = "https://remote.example/users/self"
	repo := testutil.NewMockUserRepository()
	stale := time.Now().Add(-48 * time.Hour)
	repo.Users["existing"] = &model.User{
		ID: "existing", Username: "self", URI: strPtr(srcURI), LastFetchedAt: &stale,
	}
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "` + srcURI + `",
		"type": "Person",
		"preferredUsername": "self",
		"inbox": "` + srcURI + `/inbox",
		"movedTo": "` + srcURI + `",
		"publicKey": {"publicKeyPem": "FAKE"}
	}`
	urls := activitypub.NewURLBuilder("https://local.example")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, testutil.NewMockNoteRepository(), urls,
		&stubFetcher{body: []byte(body)}, idGen)
	mp := &fakeMoveProcessor{}
	r.SetMoveProcessor(mp)

	_, err := r.ResolveActor(srcURI)
	require.NoError(t, err)
	assert.Zero(t, mp.calls)
}

// ローカルを名乗る移行先が DB に居ない場合は取りに行かず打ち切る。
// upstream の 'failed: movedTo is local but not found' 相当。
func TestRefreshActor_RemoteMoveToUnknownLocalURIIsRejected(t *testing.T) {
	const srcURI = "https://remote.example/users/src3"
	const dstURI = "https://local.example/users/nobody"

	repo := testutil.NewMockUserRepository()
	stale := time.Now().Add(-48 * time.Hour)
	repo.Users["existing"] = &model.User{
		ID: "existing", Username: "src", URI: strPtr(srcURI), LastFetchedAt: &stale,
	}
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "` + srcURI + `",
		"type": "Person",
		"preferredUsername": "src",
		"inbox": "` + srcURI + `/inbox",
		"movedTo": "` + dstURI + `",
		"publicKey": {"publicKeyPem": "FAKE"}
	}`
	urls := activitypub.NewURLBuilder("https://local.example")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, testutil.NewMockNoteRepository(), urls,
		&stubFetcher{body: []byte(body)}, idGen)
	mp := &fakeMoveProcessor{}
	r.SetMoveProcessor(mp)

	_, err := r.ResolveActor(srcURI)
	require.NoError(t, err)
	assert.Zero(t, mp.calls, "a local-looking destination that does not exist is rejected")
}

// refreshActor 経由では届かないゲートを直接突く (#2414)。
func TestProcessRemoteMove_Gates(t *testing.T) {
	const srcURI = "https://remote.example/users/g"
	const dstURI = "https://elsewhere.example/users/g2"

	newResolver := func(repo *testutil.MockUserRepository) (*federation.Resolver, *fakeMoveProcessor) {
		urls := activitypub.NewURLBuilder("https://local.example")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(repo, testutil.NewMockNoteRepository(), urls,
			&stubFetcher{body: []byte(`{}`)}, idGen)
		mp := &fakeMoveProcessor{}
		r.SetMoveProcessor(mp)
		return r, mp
	}
	// 相互確認が取れる移行先を持つ標準構成。
	seed := func() *testutil.MockUserRepository {
		repo := testutil.NewMockUserRepository()
		repo.Users["dstUser"] = &model.User{
			ID: "dstUser", URI: strPtr(dstURI), AlsoKnownAs: strPtr(srcURI),
		}
		return repo
	}
	now := time.Now()

	t.Run("移行先が未設定なら何もしない", func(t *testing.T) {
		r, mp := newResolver(seed())
		r.ProcessRemoteMove(&model.User{ID: "s", URI: strPtr(srcURI)}, nil, nil)
		assert.Zero(t, mp.calls)
	})

	t.Run("クールダウン中は処理しない", func(t *testing.T) {
		r, mp := newResolver(seed())
		prev := now.Add(-24 * time.Hour) // 14 日未満
		src := &model.User{
			ID: "s", URI: strPtr(srcURI), MovedToURI: strPtr(dstURI), MovedAt: &now,
		}
		r.ProcessRemoteMove(src, &prev, nil)
		assert.Zero(t, mp.calls, "a second move within 14 days is ignored")
	})

	t.Run("クールダウンを過ぎていれば処理する", func(t *testing.T) {
		r, mp := newResolver(seed())
		prev := now.Add(-30 * 24 * time.Hour)
		src := &model.User{
			ID: "s", URI: strPtr(srcURI), MovedToURI: strPtr(dstURI), MovedAt: &now,
		}
		r.ProcessRemoteMove(src, &prev, nil)
		assert.Equal(t, 1, mp.calls)
	})

	t.Run("既に辿った移行先なら循環として打ち切る", func(t *testing.T) {
		r, mp := newResolver(seed())
		src := &model.User{ID: "s", URI: strPtr(srcURI), MovedToURI: strPtr(dstURI)}
		r.ProcessRemoteMove(src, nil, map[string]bool{dstURI: true})
		assert.Zero(t, mp.calls)
	})

	t.Run("連鎖が長すぎれば打ち切る", func(t *testing.T) {
		r, mp := newResolver(seed())
		visited := map[string]bool{}
		for i := range 11 {
			visited["https://hop.example/users/"+string(rune('a'+i))] = true
		}
		src := &model.User{ID: "s", URI: strPtr(srcURI), MovedToURI: strPtr(dstURI)}
		r.ProcessRemoteMove(src, nil, visited)
		assert.Zero(t, mp.calls)
	})

	t.Run("解決した移行先の URI が宣言と食い違えば弾く", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		// FindByURI は dstURI で引けるが、行の URI が別物になっている。
		repo.Users["dstUser"] = &model.User{
			ID: "dstUser", URI: strPtr("https://elsewhere.example/users/OTHER"),
			AlsoKnownAs: strPtr(srcURI),
		}
		r, mp := newResolver(repo)
		src := &model.User{ID: "s", URI: strPtr(srcURI), MovedToURI: strPtr(dstURI)}
		r.ProcessRemoteMove(src, nil, nil)
		assert.Zero(t, mp.calls, "declared destination must match the resolved actor URI")
	})

	t.Run("移行元の URI が無ければ弾く", func(t *testing.T) {
		r, mp := newResolver(seed())
		src := &model.User{ID: "s", MovedToURI: strPtr(dstURI)}
		r.ProcessRemoteMove(src, nil, nil)
		assert.Zero(t, mp.calls)
	})

	t.Run("移行先が自分自身を指していれば弾く", func(t *testing.T) {
		repo := testutil.NewMockUserRepository()
		repo.Users["dstUser"] = &model.User{
			ID: "dstUser", URI: strPtr(dstURI), AlsoKnownAs: strPtr(srcURI),
			MovedToURI: strPtr(dstURI),
		}
		r, mp := newResolver(repo)
		src := &model.User{ID: "s", URI: strPtr(srcURI), MovedToURI: strPtr(dstURI)}
		r.ProcessRemoteMove(src, nil, nil)
		assert.Zero(t, mp.calls)
	})

	t.Run("processor 未配線なら何もしない", func(t *testing.T) {
		urls := activitypub.NewURLBuilder("https://local.example")
		idGen, _ := id.NewGenerator("aidx")
		r := federation.NewResolver(seed(), testutil.NewMockNoteRepository(), urls,
			&stubFetcher{body: []byte(`{}`)}, idGen)
		src := &model.User{ID: "s", URI: strPtr(srcURI), MovedToURI: strPtr(dstURI)}
		assert.NotPanics(t, func() { r.ProcessRemoteMove(src, nil, nil) })
	})

	t.Run("移行先の取得に失敗したら打ち切る", func(t *testing.T) {
		// DB に居らず、fetch も失敗する remote URI。
		r, mp := newResolver(testutil.NewMockUserRepository())
		src := &model.User{ID: "s", URI: strPtr(srcURI), MovedToURI: strPtr(dstURI)}
		r.ProcessRemoteMove(src, nil, nil)
		assert.Zero(t, mp.calls)
	})
}

// ローカルユーザーが移行先の場合、user.uri が空でも正規 URI を組み立てて照合する。
func TestProcessRemoteMove_LocalDestination(t *testing.T) {
	const srcURI = "https://remote.example/users/incoming"
	repo := testutil.NewMockUserRepository()
	// ローカルユーザーは URI 列を持たない。
	repo.Users["localDst"] = &model.User{
		ID: "localDst", Username: "me", AlsoKnownAs: strPtr(srcURI),
	}
	urls := activitypub.NewURLBuilder("https://local.example")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, testutil.NewMockNoteRepository(), urls,
		&stubFetcher{body: []byte(`{}`)}, idGen)
	mp := &fakeMoveProcessor{}
	r.SetMoveProcessor(mp)

	src := &model.User{
		ID: "s", URI: strPtr(srcURI),
		MovedToURI: strPtr(urls.UserURI("localDst")),
	}
	r.ProcessRemoteMove(src, nil, nil)

	require.Equal(t, 1, mp.calls, "moving into a local account is carried over")
	assert.Equal(t, "localDst", mp.dst.ID)
}

// --- #3121: 「確かめられなかった」と「引けなかった」を ack に潰さない ---

// AP 投票の判定に使う poll の lookup が落ちたとき、**普通の返信ノートとして
// 確定させない**。確定させると dedup があるので retry でも直らない。
func TestIngestNote_PollLookupFailurePropagates(t *testing.T) {
	boom := errors.New("connection refused")
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["pollNote"] = &model.Note{ID: "pollNote", UserID: "author", HasPoll: true, Visibility: model.NoteVisibilityPublic}
	pollRepo := testutil.NewMockPollRepository()
	require.NoError(t, pollRepo.Create(&model.Poll{
		NoteID: "pollNote", Choices: []string{"Apple", "Banana"}, Votes: []int64{0, 0},
	}))
	pollRepo.FindErr = boom
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	r.SetPollRepo(pollRepo)
	voter := &stubPollVoter{}
	r.SetPollVoter(voter)

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/users/alice#votes/pollNote",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"name": "Banana",
		"inReplyTo": "https://example.com/notes/pollNote",
		"to": ["https://example.com/users/author"]
	}`)
	got, err := r.IngestNote(body)
	require.ErrorIs(t, err, boom, "poll の lookup 障害を素通りしている")
	assert.Nil(t, got)
	assert.Empty(t, voter.calls)
	// **返信ノートとして確定していないこと。** 確定すると uri が DB に載るので
	// retry が dedup にヒットし、投票は永久に反映されない。
	_, ferr := noteRepo.FindByURI("https://remote.example/users/alice#votes/pollNote")
	assert.True(t, repository.IsNotFound(ferr), "投票が返信ノートとして保存されている")
}

// 鍵を引けなかったときに「鍵が無い」に潰さない。潰すと呼び出し側が署名検証の
// 失敗として ack する。
func TestPublicKeyForActor_DBFailureIsVerifyUnavailable(t *testing.T) {
	boom := errors.New("connection refused")
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	pkRepo := &stubPublickeyRepo{findErr: boom}
	r.SetPublickeyRepo(pkRepo)

	_, err := r.PublicKeyForActor("alice")
	require.ErrorIs(t, err, federation.ErrLookupUnavailable, "DB 障害を「鍵が無い」に潰している")

	// not-found は従来どおり「鍵が無い」。ここを一緒に倒すと、鍵を持たない
	// actor からの署名が全部 retry に回って inbox が詰まる。
	pkRepo.findErr = nil
	_, err = r.PublicKeyForActor("alice")
	require.Error(t, err)
	assert.NotErrorIs(t, err, federation.ErrLookupUnavailable)
}

// keyId 付きの lookup も同じ。**fallback へ落とさない** — 落とすと RSA 鍵で
// verify を試みて失敗し、やはり ack される。
func TestPublicKeyForKeyID_DBFailureIsVerifyUnavailable(t *testing.T) {
	boom := errors.New("connection refused")
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	extra := &stubPublickeyExtraRepo{findErr: boom}
	r.SetPublickeyExtraRepo(extra)
	// fallback 先には鍵がある。**落とすと verify が「鍵はある」で進んでしまう**
	// ので、fallback が生きていることを確かめたうえで障害を注入する。
	pkRepo := &stubPublickeyRepo{entries: map[string]*model.UserPublickey{
		"alice": {UserID: "alice", KeyPEM: "RSA-PEM"},
	}}
	r.SetPublickeyRepo(pkRepo)

	_, err := r.PublicKeyForKeyID("alice", "https://remote.example/users/alice#ed25519-key")
	require.ErrorIs(t, err, federation.ErrLookupUnavailable, "DB 障害を fallback に落としている")

	// not-found (= その keyId の追加鍵は無い) は従来どおり RSA 鍵へ fallback。
	extra.findErr = nil
	pem, err := r.PublicKeyForKeyID("alice", "https://remote.example/users/alice#ed25519-key")
	require.NoError(t, err)
	assert.Equal(t, "RSA-PEM", pem)
}

// **逆向きも固定する。** not-found (= 返信先をこちらが取り込んでいない) まで
// 伝播させると、そのノートが永久に取り込めず job が retry を繰り返す。
func TestIngestNote_UnknownReplyTargetStillIngests(t *testing.T) {
	for _, tc := range []struct {
		name      string
		inReplyTo string
	}{
		// ローカル ID 経路 (FindByID) — 削除済みの投稿への返信。
		{"ローカルの返信先が無い", "https://example.com/notes/gone"},
		// URI 経路 (FindByURI) — 未取り込みのリモート投稿への返信。
		{"リモートの返信先が未取り込み", "https://remote.example/notes/unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			noteRepo := testutil.NewMockNoteRepository()
			urls := activitypub.NewURLBuilder("https://example.com")
			idGen, _ := id.NewGenerator("aidx")
			r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

			body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams",
				"id": "https://remote.example/notes/orphanreply",
				"type": "Note",
				"attributedTo": "https://remote.example/users/alice",
				"content": "re",
				"inReplyTo": "` + tc.inReplyTo + `",
				"to": ["https://www.w3.org/ns/activitystreams#Public"]
			}`)
			got, err := r.IngestNote(body)
			require.NoError(t, err, "返信先が無いだけで取り込みを止めている")
			require.NotNil(t, got)
			assert.Nil(t, got.ReplyID, "引けなかった返信先を繋いでいる")
		})
	}
}

// **逆向きも固定する。** poll 行が無いだけで伝播させると、hasPoll が立った
// ノートへの `name` 付き返信が永久に取り込めなくなる。
func TestIngestNote_MissingPollRowFallsThroughToReply(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["pollNote"] = &model.Note{ID: "pollNote", UserID: "author", HasPoll: true, Visibility: model.NoteVisibilityPublic}
	// poll 行は作らない (= FindByNoteID が not-found)。
	pollRepo := testutil.NewMockPollRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	r.SetPollRepo(pollRepo)
	voter := &stubPollVoter{}
	r.SetPollVoter(voter)

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/users/alice#votes/pollNote",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"name": "Banana",
		"inReplyTo": "https://example.com/notes/pollNote",
		"to": ["https://example.com/users/author"]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err, "poll 行が無いだけで取り込みを止めている")
	require.NotNil(t, got)
	assert.Empty(t, voter.calls)
	require.NotNil(t, got.ReplyID)
	assert.Equal(t, "pollNote", *got.ReplyID)
}

// actor の lookup が引けなかったときも「まだ知らない actor」に倒さない。
// 倒すとリモートへ fetch しに行き、その直後の Create が同じ障害で落ちるので、
// 呼び出し側からは「解決できなかった」としか見えない (= 署名検証が ack する)。
func TestResolveActor_DBFailureIsLookupUnavailable(t *testing.T) {
	boom := errors.New("connection refused")
	repo := testutil.NewMockUserRepository()
	repo.FindByURIErr = boom
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	fetcher := &countingFetcher{body: []byte(sampleActor)}
	r := federation.NewResolver(repo, noteRepo, urls, fetcher, idGen)

	_, err := r.ResolveActor("https://remote.example/users/alice")
	require.ErrorIs(t, err, federation.ErrLookupUnavailable, "DB 障害を「まだ知らない actor」に倒している")
	assert.Zero(t, fetcher.calls, "落ちている DB を相手に remote fetch まで走っている")

	// **逆向き。** not-found (= 本当にまだ知らない) は従来どおり fetch して作る。
	repo.FindByURIErr = nil
	got, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, 1, fetcher.calls)
}

// **宛先の解決が引けなかったら note を確定させない** (#3121)。確定させると
// `visibleUserIds` が空のまま `note.uri` が DB に載り、retry は dedup に
// ヒットするので **受信者本人が DM を読めない状態が恒久化する**。
func TestIngestNote_SpecifiedDMRecipientLookupFailurePropagates(t *testing.T) {
	boom := errors.New("connection refused")
	repo := testutil.NewMockUserRepository()
	// 著者の解決は通し、宛先 (リモート URI) の lookup だけ落とす。
	repo.FindByURIHook = func(uri string) error {
		if uri == "https://remote.example/users/bob" {
			return boom
		}
		return nil
	}
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/notes/dm2",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "secret hello",
		"to": ["https://remote.example/users/bob"],
		"cc": [],
		"tag": [
			{"type": "Mention", "href": "https://remote.example/users/bob", "name": "@bob@remote.example"}
		]
	}`)
	got, err := r.IngestNote(body)
	require.ErrorIs(t, err, boom, "宛先の解決を素通りしている")
	assert.Nil(t, got)
	_, ferr := noteRepo.FindByURI("https://remote.example/notes/dm2")
	assert.True(t, repository.IsNotFound(ferr), "宛先が空のまま note を確定させている")
}
