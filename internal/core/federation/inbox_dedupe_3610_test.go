package federation_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dedupeCreateBody = `{
	"id": "https://remote.example/notes/x/activity",
	"type": "Create",
	"actor": "https://remote.example/users/alice",
	"object": {"id":"https://remote.example/notes/x","type":"Note","attributedTo":"https://remote.example/users/alice","content":"hi","to":["https://www.w3.org/ns/activitystreams#Public"]}
}`

// seedLiveAlice stores alice as a live (not suspended, not deleted) row and
// returns a detached copy usable as the HTTP signer.
func seedLiveAlice(repo *testutil.MockUserRepository) *model.User {
	uri := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", UsernameLower: "alice", URI: &uri, Host: &host}
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	signer := *repo.Users["alice1"]
	return &signer
}

// #3610: 署名者がそのまま actor なら、凍結・削除の判定は署名者の行で行い、DB を
// 引き直さない。DB の行は生きているが署名者の行は凍結済み、という食い違いを
// 作り、署名者の行が使われたことを確かめる。
func TestProcessWithSigner_SignerIsActor_ModerationUsesSignerRow(t *testing.T) {
	for _, tc := range []struct {
		name string
		mark func(*model.User)
	}{
		{"suspended", func(u *model.User) { u.IsSuspended = true }},
		{"deleted", func(u *model.User) { u.IsDeleted = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, repo, noteRepo, _ := newProcForInbound1560(t, aliceActor)
			signer := seedLiveAlice(repo)
			tc.mark(signer)
			lookups := 0
			repo.FindByURIHook = func(string) error { lookups++; return nil }

			require.NoError(t, p.ProcessWithSigner([]byte(dedupeCreateBody), signer))
			assert.Empty(t, noteRepo.Notes, "the signer row says the actor is gone; the activity must be dropped")
			assert.Zero(t, lookups, "the actor must not be looked up again when the signer is the actor")
		})
	}
}

// 転送 (署名者 != actor) では、署名者の行で判定してはいけない。actor は DB から
// 引き直し、そちらが凍結済みなら捨てる。
func TestProcessWithSigner_ForwardedActorIsStillLookedUp(t *testing.T) {
	p, repo, noteRepo, _ := newProcForInbound1560(t, aliceActor)
	seedLiveAlice(repo)
	repo.Users["alice1"].IsSuspended = true
	relayURI := "https://relay.example/actor"
	relayHost := "relay.example"
	relay := &model.User{ID: "relay1", Username: "relay", URI: &relayURI, Host: &relayHost}

	require.NoError(t, p.ProcessWithSigner([]byte(dedupeCreateBody), relay))
	assert.Empty(t, noteRepo.Notes, "a suspended actor forwarded by a live signer must be dropped")
}

// 署名者の行が生きていれば、今どおり処理される (判定を緩めすぎていないことの対照)。
func TestProcessWithSigner_SignerIsLiveActor_Processes(t *testing.T) {
	p, repo, noteRepo, _ := newProcForInbound1560(t, aliceActor)
	signer := seedLiveAlice(repo)
	require.NoError(t, p.ProcessWithSigner([]byte(dedupeCreateBody), signer))
	assert.Len(t, noteRepo.Notes, 1)
}

// NormalizeActivity を通した入口は、Process と同じ結果を返す。
func TestProcessNormalized_MatchesProcess(t *testing.T) {
	t.Run("singleton array is unwrapped and Source keeps the raw bytes", func(t *testing.T) {
		body := []byte(`[` + dedupeCreateBody + `]`)
		n := federation.NormalizeActivity(body)
		assert.Equal(t, body, n.Source(), "Source must be the bytes given, not the normalized document")
		assert.Equal(t, federation.ActivityFields{
			Actor: "https://remote.example/users/alice",
			ID:    "https://remote.example/notes/x/activity",
			Type:  "Create",
		}, n.Fields())
		assert.Equal(t, federation.ExtractActivityFields(body), n.Fields())

		p, repo, noteRepo, _ := newProcForInbound1560(t, aliceActor)
		seedLiveAlice(repo)
		require.NoError(t, p.ProcessNormalized(n, nil))
		assert.Len(t, noteRepo.Notes, 1)
	})
	t.Run("conflicting keys are acked as unsupported", func(t *testing.T) {
		body := []byte(`{"type":"Delete","actor":"https://remote.example/users/alice","as:actor":"https://victim.example/users/a"}`)
		p, _, _, _ := newProcForInbound1560(t, aliceActor)
		n := federation.NormalizeActivity(body)
		assert.Equal(t, federation.ActivityFields{}, n.Fields())
		assert.ErrorIs(t, p.ProcessNormalized(n, nil), federation.ErrUnsupportedActivity)
		assert.ErrorIs(t, p.Process(body), federation.ErrUnsupportedActivity)
	})
	t.Run("broken json is an error to retry", func(t *testing.T) {
		body := []byte(`{"type":`)
		p, _, _, _ := newProcForInbound1560(t, aliceActor)
		err := p.ProcessNormalized(federation.NormalizeActivity(body), nil)
		require.Error(t, err)
		assert.NotErrorIs(t, err, federation.ErrUnsupportedActivity)
	})
	t.Run("zero value is dropped as an activity without actor", func(t *testing.T) {
		p, _, noteRepo, _ := newProcForInbound1560(t, aliceActor)
		require.NoError(t, p.ProcessNormalized(federation.NormalizedActivity{}, nil))
		assert.Empty(t, noteRepo.Notes)
	})
}

// countingExtraKeyRepo counts FindByUserAndKeyID round trips.
type countingExtraKeyRepo struct {
	*stubPublickeyExtraRepo
	finds int
	// afterFind runs after the lookup, before its result is returned.
	afterFind func()
	// beforeUpsert runs before a row is stored.
	beforeUpsert func()
}

func (c *countingExtraKeyRepo) FindByUserAndKeyID(userID, keyID string) (*model.UserPublickeyExtra, error) {
	c.finds++
	row, err := c.stubPublickeyExtraRepo.FindByUserAndKeyID(userID, keyID)
	if c.afterFind != nil {
		c.afterFind()
	}
	return row, err
}

func (c *countingExtraKeyRepo) Upsert(pk *model.UserPublickeyExtra) error {
	if c.beforeUpsert != nil {
		c.beforeUpsert()
	}
	return c.stubPublickeyExtraRepo.Upsert(pk)
}

const (
	dedupeMainKey    = "https://remote.example/users/alice#main-key"
	dedupeEd25519Key = "https://remote.example/users/alice#ed25519-key"
	dedupeRSAPEM     = "-----BEGIN PUBLIC KEY-----\nRSA-FAKE\n-----END PUBLIC KEY-----"
)

func newMissingExtraResolver(t *testing.T, f federation.HTTPFetcher) (*federation.Resolver, *testutil.MockUserRepository, *countingExtraKeyRepo, *time.Time) {
	t.Helper()
	r, repo := newResolverWithFetcher(t, f)
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	r.SetClock(func() time.Time { return now })
	pk := &stubPublickeyRepo{entries: map[string]*model.UserPublickey{}}
	require.NoError(t, pk.Upsert(&model.UserPublickey{UserID: "alice", KeyID: dedupeMainKey, KeyPEM: dedupeRSAPEM}))
	r.SetPublickeyRepo(pk)
	extra := &countingExtraKeyRepo{stubPublickeyExtraRepo: &stubPublickeyExtraRepo{}}
	r.SetPublickeyExtraRepo(extra)
	return r, repo, extra, &now
}

// #3610: `#main-key` は user_publickey_extra に無いのが普通の状態。見つからなかった
// ことを短い間覚え、受信のたびに問い合わせない。
func TestPublicKeyForKeyID_RemembersMissingExtraKey(t *testing.T) {
	r, _, extra, _ := newMissingExtraResolver(t, &stubFetcher{})
	for i := 0; i < 3; i++ {
		pem, err := r.PublicKeyForKeyID("alice", dedupeMainKey)
		require.NoError(t, err)
		assert.Equal(t, dedupeRSAPEM, pem)
	}
	assert.Equal(t, 1, extra.finds, "a missing extra key must be looked up once, not per activity")

	// 別の actor の同じ形の keyId は別の記録。
	_, _ = r.PublicKeyForKeyID("bob", "https://remote.example/users/bob#main-key")
	assert.Equal(t, 2, extra.finds)
}

// 記録には期限がある。過ぎたら DB をもう一度引く。
func TestPublicKeyForKeyID_MissingExtraKeyExpires(t *testing.T) {
	r, _, extra, now := newMissingExtraResolver(t, &stubFetcher{})
	_, err := r.PublicKeyForKeyID("alice", dedupeEd25519Key)
	require.NoError(t, err)
	*now = now.Add(59 * time.Second)
	_, err = r.PublicKeyForKeyID("alice", dedupeEd25519Key)
	require.NoError(t, err)
	require.Equal(t, 1, extra.finds, "within the TTL the miss is remembered")

	*now = now.Add(2 * time.Second)
	// 期限の間に別のプロセスが行を足していた。
	require.NoError(t, extra.Upsert(&model.UserPublickeyExtra{UserID: "alice", KeyID: dedupeEd25519Key, KeyPEM: "ED25519-PEM", Alg: model.AlgEd25519}))
	pem, err := r.PublicKeyForKeyID("alice", dedupeEd25519Key)
	require.NoError(t, err)
	assert.Equal(t, 2, extra.finds, "after the TTL the table must be read again")
	assert.Equal(t, "ED25519-PEM", pem)
}

// 障害は「無い」として覚えない (#3121 の「障害を鍵が無いに倒さない」)。
func TestPublicKeyForKeyID_LookupFailureIsNotRemembered(t *testing.T) {
	r, _, extra, _ := newMissingExtraResolver(t, &stubFetcher{})
	extra.findErr = errors.New("connection refused")
	_, err := r.PublicKeyForKeyID("alice", dedupeEd25519Key)
	require.ErrorIs(t, err, federation.ErrLookupUnavailable)

	extra.findErr = nil
	require.NoError(t, extra.Upsert(&model.UserPublickeyExtra{UserID: "alice", KeyID: dedupeEd25519Key, KeyPEM: "ED25519-PEM", Alg: model.AlgEd25519}))
	pem, err := r.PublicKeyForKeyID("alice", dedupeEd25519Key)
	require.NoError(t, err)
	assert.Equal(t, "ED25519-PEM", pem, "a lookup failure must not be cached as a missing key")
}

// 鍵の回転: 相手が Ed25519 の鍵を足したら、actor を読み直した時点で「無い」の
// 記録を捨て、期限を待たずに新しい鍵で検証する。
func TestPublicKeyForKeyID_ActorRefetchForgetsMissingExtraKey(t *testing.T) {
	f := &stubFetcher{body: []byte(sampleActor)}
	r, repo, extra, now := newMissingExtraResolver(t, f)
	uri := "https://remote.example/users/alice"
	host := "remote.example"
	fetched := *now
	repo.Users["alice"] = &model.User{ID: "alice", Username: "alice", UsernameLower: "alice", URI: &uri, Host: &host, LastFetchedAt: &fetched}

	pem, err := r.PublicKeyForKeyID("alice", dedupeEd25519Key)
	require.NoError(t, err)
	require.Equal(t, dedupeRSAPEM, pem, "before rotation the extra key is missing")

	// 相手が assertionMethod に Ed25519 の鍵を足した。
	f.body = []byte(actorWithEd25519Key(t, dedupeEd25519Key))
	repo.Users["alice"].LastFetchedAt = nil // 次の解決で読み直させる
	_, err = r.ResolveActor(uri)
	require.NoError(t, err)
	require.NotEmpty(t, extra.upserts, "the refetch must store the new key")

	stored, err := extra.stubPublickeyExtraRepo.FindByUserAndKeyID("alice", dedupeEd25519Key)
	require.NoError(t, err)

	// 読み直しは主鍵 (RSA) も書き換えるので、「RSA ではない」では足りない。
	// extra に入った Ed25519 の鍵そのものが返ることを見る。
	pem, err = r.PublicKeyForKeyID("alice", dedupeEd25519Key)
	require.NoError(t, err)
	assert.Equal(t, stored.KeyPEM, pem, "the remembered miss must be dropped when the actor is re-read")
}

// 1 つの actor が名乗れる keyId の記録には上限がある。署名を検証する前に呼ばれる
// ので、既存の actor の URI に任意の fragment を付けた要求で埋められるため。
func TestPublicKeyForKeyID_MissingExtraKeyPerActorBound(t *testing.T) {
	r, _, extra, _ := newMissingExtraResolver(t, &stubFetcher{})
	for i := 0; i < 4; i++ {
		_, _ = r.PublicKeyForKeyID("alice", dedupeMainKey+strings.Repeat("x", i))
	}
	require.Equal(t, 4, extra.finds)
	_, _ = r.PublicKeyForKeyID("alice", "https://remote.example/users/alice#fifth")
	_, _ = r.PublicKeyForKeyID("alice", "https://remote.example/users/alice#fifth")
	assert.Equal(t, 6, extra.finds, "past the per-actor bound the miss must not be remembered")
	// 先に覚えた 4 件は残っている。
	_, _ = r.PublicKeyForKeyID("alice", dedupeMainKey)
	assert.Equal(t, 6, extra.finds)
}

// 列には入るがバイト数の大きい keyId は覚えない (否定キャッシュのメモリを抑える)。
// 200 文字の「あ」で 235 文字・635 バイト: varchar(256) には入るので DB は引く。
func TestPublicKeyForKeyID_LongKeyIDIsNotRemembered(t *testing.T) {
	r, _, extra, _ := newMissingExtraResolver(t, &stubFetcher{})
	long := "https://remote.example/users/alice#" + strings.Repeat("あ", 200)
	require.Greater(t, len(long), 512)
	_, _ = r.PublicKeyForKeyID("alice", long)
	_, _ = r.PublicKeyForKeyID("alice", long)
	assert.Equal(t, 2, extra.finds)
}

// keyId の列 (varchar(256)) に入らない keyId は、DB を引かずに主鍵へ倒す。
// その行は存在しえないので結果は直す前と同じ (NUL / 不正な UTF-8 は、直す前も
// repository の `storable` が not found を返していた)。fake の findErr は、
// 問い合わせが走ったら error として表に出すための検出器で、本番の挙動ではない。
func TestPublicKeyForKeyID_KeyIDThatCannotBeStoredSkipsLookup(t *testing.T) {
	for _, tc := range []struct{ name, keyID string }{
		{"257 runes", "https://remote.example/users/alice#" + strings.Repeat("k", 257-35)},
		{"NUL", "https://remote.example/users/alice#main\x00key"},
		{"invalid UTF-8", "https://remote.example/users/alice#\xff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, extra, _ := newMissingExtraResolver(t, &stubFetcher{})
			extra.findErr = errors.New("lookup must not happen")
			pem, err := r.PublicKeyForKeyID("alice", tc.keyID)
			require.NoError(t, err)
			assert.Equal(t, dedupeRSAPEM, pem)
			assert.Zero(t, extra.finds, "a keyId that cannot be stored must not be looked up")
		})
	}
	// 境界: ちょうど 256 文字は引く。
	r, _, extra, _ := newMissingExtraResolver(t, &stubFetcher{})
	_, _ = r.PublicKeyForKeyID("alice", "https://remote.example/users/alice#"+strings.Repeat("k", 256-35))
	assert.Equal(t, 1, extra.finds)
}

// 問い合わせの間に actor が読み直されて鍵が足されたら、見つからなかった結果を
// 覚えない (L-1)。覚えると最大 1 分、正当な Ed25519 の署名が検証に失敗し、
// その activity は失われる。
func TestPublicKeyForKeyID_RefetchDuringLookupIsNotRemembered(t *testing.T) {
	f := &stubFetcher{body: []byte(actorWithEd25519Key(t, dedupeEd25519Key))}
	r, repo, extra, _ := newMissingExtraResolver(t, f)
	uri := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["alice"] = &model.User{ID: "alice", Username: "alice", UsernameLower: "alice", URI: &uri, Host: &host}
	// 1 回目の問い合わせが not found を読んだ直後に、別の経路で actor が読み直される。
	extra.afterFind = func() {
		extra.afterFind = nil
		_, err := r.ResolveActor(uri)
		require.NoError(t, err)
	}
	pem, err := r.PublicKeyForKeyID("alice", dedupeEd25519Key)
	require.NoError(t, err)
	require.NotContains(t, pem, "ED25519", "precondition: the first lookup raced and saw no row")
	require.NotEmpty(t, extra.upserts)

	stored, err := extra.stubPublickeyExtraRepo.FindByUserAndKeyID("alice", dedupeEd25519Key)
	require.NoError(t, err)
	pem, err = r.PublicKeyForKeyID("alice", dedupeEd25519Key)
	require.NoError(t, err)
	assert.Equal(t, stored.KeyPEM, pem, "a miss read before a concurrent refetch must not be remembered")
}

// 記録を捨てるのは鍵を足した後でなければならない。足す前に捨てると、その間に
// 読んだ「無い」が残る。
func TestPublicKeyForKeyID_ForgetRunsAfterUpsert(t *testing.T) {
	f := &stubFetcher{body: []byte(actorWithEd25519Key(t, dedupeEd25519Key))}
	r, repo, extra, _ := newMissingExtraResolver(t, f)
	uri := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["alice"] = &model.User{ID: "alice", Username: "alice", UsernameLower: "alice", URI: &uri, Host: &host}
	// 鍵を足す直前に、別の受信が「無い」を読んで覚える。
	extra.beforeUpsert = func() {
		extra.beforeUpsert = nil
		_, err := r.PublicKeyForKeyID("alice", dedupeEd25519Key)
		require.NoError(t, err)
	}
	_, err := r.ResolveActor(uri)
	require.NoError(t, err)
	require.Nil(t, extra.beforeUpsert, "precondition: the hook ran")

	stored, err := extra.stubPublickeyExtraRepo.FindByUserAndKeyID("alice", dedupeEd25519Key)
	require.NoError(t, err)
	pem, err := r.PublicKeyForKeyID("alice", dedupeEd25519Key)
	require.NoError(t, err)
	assert.Equal(t, stored.KeyPEM, pem, "the miss remembered before the upsert must be dropped after it")
}

// failingUpdateUserRepo returns detached copies (like a real DB) and fails every
// UpdateUser, so the in-memory row and the stored row can diverge.
type failingUpdateUserRepo struct {
	*testutil.MockUserRepository
}

func (f *failingUpdateUserRepo) FindByURI(uri string) (*model.User, error) {
	u, err := f.MockUserRepository.FindByURI(uri)
	if err != nil {
		return nil, err
	}
	c := *u
	return &c, nil
}

func (f *failingUpdateUserRepo) UpdateUser(string, map[string]any) error {
	return errors.New("connection reset")
}

// #3610: inbox は ResolveActor が返した行で凍結を判定する。発信元の解除を DB に
// 書けなかったなら、返す行も凍結のままでなければならない。
func TestResolveActor_UnsuspendWriteFailureKeepsReturnedRowSuspended(t *testing.T) {
	mock := testutil.NewMockUserRepository()
	repo := &failingUpdateUserRepo{MockUserRepository: mock}
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := federation.NewResolver(repo, testutil.NewMockNoteRepository(), activitypub.NewURLBuilder("https://example.com"),
		&stubFetcher{body: []byte(actorWithSuspended(t, "false"))}, idGen)
	origins := newStubOriginRepo()
	r.SetSuspensionOriginRepo(origins)
	host := "remote.example"
	uri := "https://remote.example/users/alice"
	mock.Users["existing"] = &model.User{ID: "existing", Username: "alice", UsernameLower: "alice", Host: &host, URI: &uri, IsSuspended: true}
	require.NoError(t, origins.Set("existing", model.SuspensionOriginRemote))

	user, err := r.ResolveActor(uri)
	require.NoError(t, err)
	assert.True(t, mock.Users["existing"].IsSuspended, "precondition: the stored row stays suspended")
	assert.True(t, user.IsSuspended, "the returned row must not claim an unsuspension the DB did not store")
}

// actorWithEd25519Key returns sampleActor publishing an Ed25519 Multikey under
// keyID in assertionMethod.
func actorWithEd25519Key(t *testing.T, keyID string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	mb, err := activitypub.EncodeEd25519Multikey(pub)
	require.NoError(t, err)
	out := strings.Replace(sampleActor, `"type": "Person",`,
		`"type": "Person",`+"\n\t\"assertionMethod\": [{\"id\": \""+keyID+`", "type": "Multikey", "controller": "https://remote.example/users/alice", "publicKeyMultibase": "`+mb+`"}],`, 1)
	require.Contains(t, out, "assertionMethod", "sampleActor の整形が変わって置換が空振りした")
	return out
}

// copyingUserRepo returns detached copies from FindByURI, like a real DB.
type copyingUserRepo struct {
	*testutil.MockUserRepository
}

func (c *copyingUserRepo) FindByURI(uri string) (*model.User, error) {
	u, err := c.MockUserRepository.FindByURI(uri)
	if err != nil {
		return nil, err
	}
	cp := *u
	return &cp, nil
}

// #3610 (L-2): inbox は署名者の行で凍結を判定するので、読み直しで発信元の凍結
// (`suspended: true`) を取り込んだら、返す行も凍結済みでなければならない。
// そうでないと DB は凍結済みなのに、その activity を運んだ要求が処理される。
func TestResolveActor_RefreshSuspensionIsVisibleToProcessWithSigner(t *testing.T) {
	mock := testutil.NewMockUserRepository()
	repo := &copyingUserRepo{MockUserRepository: mock}
	noteRepo := testutil.NewMockNoteRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := federation.NewResolver(repo, noteRepo, activitypub.NewURLBuilder("https://example.com"),
		&stubFetcher{body: []byte(actorWithSuspended(t, "true"))}, idGen)
	r.SetSuspensionOriginRepo(newStubOriginRepo())
	followingSvc := corefollowing.NewService(repo, testutil.NewMockFollowingRepository(), testutil.NewMockFollowRequestRepository(), idGen)
	p := federation.NewProcessor(r, followingSvc, nil, nil, repo, noteRepo)
	p.SetLocalBaseURL("https://example.com")

	signer := seedLiveAlice(mock) // lastFetchedAt が無いので次の解決で読み直す
	_ = signer
	got, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	require.True(t, mock.Users["alice1"].IsSuspended, "precondition: the refresh stored the suspension")
	assert.True(t, got.IsSuspended, "the returned row must carry the suspension the refresh stored")

	require.NoError(t, p.ProcessWithSigner([]byte(dedupeCreateBody), got))
	assert.Empty(t, noteRepo.Notes, "an activity from an actor the refresh just suspended must be dropped")
}
