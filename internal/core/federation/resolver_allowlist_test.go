package federation_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubHostBlocker implements federation.HostBlockChecker.
//
//	IsAllowed(host) → !disallowed[host]
//	IsBlocked(host) → blocked[host]
//
// 「ホワイトリスト連合 (federation: specified) で host が allowlist 外」の
// シナリオは disallowed に詰めて再現する。
type stubHostBlocker struct {
	blocked    map[string]bool
	disallowed map[string]bool
}

func (s *stubHostBlocker) IsBlocked(host string) bool { return s.blocked[host] }
func (s *stubHostBlocker) IsAllowed(host string) bool { return !s.disallowed[host] }

func newGatedResolver(t *testing.T, body string, blocker federation.HostBlockChecker) (*federation.Resolver, *testutil.MockUserRepository, *testutil.MockNoteRepository) {
	t.Helper()
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	if blocker != nil {
		r.SetHostBlockChecker(blocker)
	}
	return r, repo, noteRepo
}

// federation: specified モードで allowlist 外の host から actor を resolve しよう
// とすると ErrHostNotAllowed を返し、HTTP fetch も DB write も走らないこと。
func TestResolveActor_HostNotAllowedRejectsFetch(t *testing.T) {
	blocker := &stubHostBlocker{disallowed: map[string]bool{"remote.example": true}}
	r, repo, _ := newGatedResolver(t, sampleActor, blocker)

	_, err := r.ResolveActor("https://remote.example/users/alice")
	require.ErrorIs(t, err, federation.ErrHostNotAllowed)
	assert.Empty(t, repo.Users, "blocked host の actor は DB に作らない")
}

// blockedHosts に登録された host も ErrHostNotAllowed (allowlist 範囲外と
// 同じ扱い) になること。
func TestResolveActor_BlockedHostRejectsFetch(t *testing.T) {
	blocker := &stubHostBlocker{blocked: map[string]bool{"bad.example": true}}
	r, _, _ := newGatedResolver(t, sampleActor, blocker)

	_, err := r.ResolveActor("https://bad.example/users/alice")
	require.ErrorIs(t, err, federation.ErrHostNotAllowed)
}

// allowlist 内 host は従来どおり通過すること (legacy compat)。
func TestResolveActor_AllowedHostFetchesNormally(t *testing.T) {
	blocker := &stubHostBlocker{}
	r, repo, _ := newGatedResolver(t, sampleActor, blocker)

	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.Equal(t, "alice", user.Username)
	assert.Len(t, repo.Users, 1)
}

// hostBlocker 未配線の resolver は legacy 挙動 (= 全 host 許可) で動くこと。
func TestResolveActor_HostBlockerUnwiredAllowsAll(t *testing.T) {
	r, _ := newResolver(t, sampleActor, nil)
	_, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
}

// ResolveNote が allowlist 外 host の URI で fetch 起こさず ErrHostNotAllowed
// を返すこと。Announce 経由 (handleAnnounce → ResolveNote) で第三者 host の
// note を取り込む leak を guard する。
func TestResolveNote_HostNotAllowedRejectsFetch(t *testing.T) {
	blocker := &stubHostBlocker{disallowed: map[string]bool{"random.example": true}}
	r, _, _ := newGatedResolver(t, sampleRemoteNote, blocker)

	_, err := r.ResolveNote("https://random.example/notes/x")
	require.ErrorIs(t, err, federation.ErrHostNotAllowed)
}

// allowlist 外でも DB に既存 row があれば素通しすること (legacy data 互換)。
// gate を入れた後に既存 remote データの read が壊れないことの保証。
func TestResolveNote_ExistingRowPassesEvenIfHostBlocked(t *testing.T) {
	blocker := &stubHostBlocker{disallowed: map[string]bool{"random.example": true}}
	r, _, noteRepo := newGatedResolver(t, "", blocker)
	uri := "https://random.example/notes/cached"
	noteRepo.Notes["cached"] = &model.Note{ID: "cached", URI: &uri}

	got, err := r.ResolveNote(uri)
	require.NoError(t, err)
	assert.Equal(t, "cached", got.ID)
}

// IngestNoteWithCreated は body 内 attributedTo の host を判定して、
// allowlist 外 host の Create activity を直接受け取った場合に DB に
// 永続化させないこと (handleCreate → IngestNote 経路)。
func TestIngestNote_HostNotAllowedByAttributedTo(t *testing.T) {
	blocker := &stubHostBlocker{disallowed: map[string]bool{"remote.example": true}}
	r, _, noteRepo := newGatedResolver(t, sampleActor, blocker)

	_, _, err := r.IngestNoteWithCreated([]byte(sampleRemoteNote), "")
	require.ErrorIs(t, err, federation.ErrHostNotAllowed)
	assert.Empty(t, noteRepo.Notes, "blocked host の note は DB に作らない")
}

// IngestNote も既存行 dedup の経路 (= attributedTo に対する gate より前で
// FindByURI hit) では素通しすること。
func TestIngestNote_DedupHitPassesEvenIfHostBlocked(t *testing.T) {
	blocker := &stubHostBlocker{disallowed: map[string]bool{"remote.example": true}}
	r, _, noteRepo := newGatedResolver(t, sampleActor, blocker)
	uri := "https://remote.example/notes/n1"
	noteRepo.Notes["existing"] = &model.Note{ID: "existing", URI: &uri}

	got, created, err := r.IngestNoteWithCreated([]byte(sampleRemoteNote), "")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "existing", got.ID)
	assert.False(t, created)
}

// 非連合 host からの Create(Note) を inbox processor が受けたとき、gate の
// ErrHostNotAllowed は permanent skip として ack され (Process は nil を返す)、
// queue retry に乗らないこと。actor / note いずれも DB に作られないことも確認。
func TestProcessCreate_NonFederatedHostIsDroppedNotRetried(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)
	resolver.SetHostBlockChecker(&stubHostBlocker{disallowed: map[string]bool{"remote.example": true}})
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)

	body := []byte(`{"type":"Create","actor":"https://remote.example/users/alice","object":` + sampleRemoteNote + `}`)
	// permanent skip = ack。err を返すと queue が無駄に retry してしまう。
	require.NoError(t, p.Process(body))
	assert.Empty(t, noteRepo.Notes, "非連合 host の note は永続化しない")
	assert.Empty(t, repo.Users, "非連合 host の actor も作らない")
}

// 送信者 (act.Actor) の host は許可されているが、note の attributedTo が
// 非連合 host を指すケース (中継経由)。handleCreate 冒頭の actor 解決は通過し、
// IngestNoteWithCreated 段の gate で ErrHostNotAllowed → ack される (= ingest
// 段の drop 分岐を踏む)。note は永続化されないこと。
func TestProcessCreate_AllowedSenderNonFederatedAttributedToIsDropped(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	// 送信者 (relay.allowed) の actor を返す。id host が request host と一致する
	// (Strict request-host binding #1828 を通す)。
	relayActor := `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://relay.allowed/users/relay",
		"type": "Application",
		"preferredUsername": "relay",
		"inbox": "https://relay.allowed/users/relay/inbox",
		"publicKey": {
			"id": "https://relay.allowed/users/relay#main-key",
			"owner": "https://relay.allowed/users/relay",
			"publicKeyPem": "-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----"
		}
	}`
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(relayActor)}, idGen)
	// note の attributedTo (remote.example) のみ非連合。送信者 host は許可。
	resolver.SetHostBlockChecker(&stubHostBlocker{disallowed: map[string]bool{"remote.example": true}})
	followingSvc := corefollowing.NewService(repo, followingRepo, testutil.NewMockFollowRequestRepository(), idGen)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)

	body := []byte(`{"type":"Create","actor":"https://relay.allowed/users/relay","object":` + sampleRemoteNote + `}`)
	require.NoError(t, p.Process(body))
	assert.Empty(t, noteRepo.Notes, "非連合 host (attributedTo) の note は永続化しない")
}

// stubSilencedChecker implements federation.SilencedHostChecker.
type stubSilencedChecker struct{ silenced map[string]bool }

func (s *stubSilencedChecker) IsSilenced(host string) bool { return s.silenced[host] }

// #2106 N14: silenced instance (meta.silencedHosts) の remote public note は home に降格される。
func TestIngestNote_SilencedHostPublicDemotedToHome(t *testing.T) {
	r, _, _ := newGatedResolver(t, sampleActor, nil)
	r.SetSilencedHostChecker(&stubSilencedChecker{silenced: map[string]bool{"remote.example": true}})

	note, _, err := r.IngestNoteWithCreated([]byte(sampleRemoteNote), "")
	require.NoError(t, err)
	require.NotNil(t, note)
	assert.Equal(t, model.NoteVisibilityHome, note.Visibility, "silenced host の public note は home に降格")
}

// #2106 N14: silenced でない host の public note は public のまま (降格しない)。
func TestIngestNote_NonSilencedHostPublicStaysPublic(t *testing.T) {
	r, _, _ := newGatedResolver(t, sampleActor, nil)
	r.SetSilencedHostChecker(&stubSilencedChecker{silenced: map[string]bool{"other.example": true}})

	note, _, err := r.IngestNoteWithCreated([]byte(sampleRemoteNote), "")
	require.NoError(t, err)
	require.NotNil(t, note)
	assert.Equal(t, model.NoteVisibilityPublic, note.Visibility, "silenced でない host は public のまま")
}
