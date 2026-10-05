package federation_test

import (
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oversizedKeyIDActor builds an actor whose publicKey.id / publicKeyPem exceed
// their columns. keyId host は actor と揃える (host mismatch で先に弾かれないため)。
func oversizedKeyActor(keyIDFragment, pem string) string {
	return `{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/users/alice",
		"type": "Person",
		"preferredUsername": "alice",
		"inbox": "https://remote.example/users/alice/inbox",
		"publicKey": {
			"id": "https://remote.example/users/alice#` + keyIDFragment + `",
			"owner": "https://remote.example/users/alice",
			"publicKeyPem": "` + pem + `"
		}
	}`
}

// `user_publickey.keyId` は varchar(256)。行の身元なので切らず、収まらなければ
// 行を作らない。**in-memory cache は残す** ので verify はそのまま通る (#2726)。
func TestCachePublicKey_OversizedKeyIDNotPersisted(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	body := oversizedKeyActor(strings.Repeat("k", 256),
		"-----BEGIN PUBLIC KEY-----\\nFAKE\\n-----END PUBLIC KEY-----")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	pkRepo := testutil.NewMockUserPublickeyRepository()
	r.SetPublickeyRepo(pkRepo)

	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.Empty(t, pkRepo.Keys, "列に収まらない keyId の行は作らない")

	pem, err := r.PublicKeyForActor(user.ID)
	require.NoError(t, err)
	assert.Contains(t, pem, "FAKE", "in-memory cache は残るので verify は続けられる")
}

// keyPem 側も同じ (varchar(4096))。
func TestCachePublicKey_OversizedPEMNotPersisted(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	body := oversizedKeyActor("main-key", strings.Repeat("A", 4097))
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	pkRepo := testutil.NewMockUserPublickeyRepository()
	r.SetPublickeyRepo(pkRepo)

	_, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.Empty(t, pkRepo.Keys)
}

// 上限ちょうどは通す (境界を締めすぎていないこと)。
func TestCachePublicKey_MaxLengthKeyIDPersisted(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	// "https://remote.example/users/alice#" は 35 文字。
	body := oversizedKeyActor(strings.Repeat("k", 256-35),
		"-----BEGIN PUBLIC KEY-----\\nFAKE\\n-----END PUBLIC KEY-----")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(body)}, idGen)
	pkRepo := testutil.NewMockUserPublickeyRepository()
	r.SetPublickeyRepo(pkRepo)

	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	require.Len(t, pkRepo.Keys, 1)
	assert.Equal(t, 256, len([]rune(pkRepo.Keys[user.ID].KeyID)))
}

// `user_publickey_extra.keyId` も varchar(256)。収まらない entry だけ skip し、
// actor 自体は取り込む (fail-soft)。
func TestCacheAssertionMethods_OversizedKeyIDSkipped(t *testing.T) {
	long := "https://remote.example/users/alice#" + strings.Repeat("k", 256)
	body := ed25519ActorBody(t, long, "https://remote.example/users/alice#ed25519-key")
	r, _ := newResolver(t, body, nil)
	extra := &stubPublickeyExtraRepo{}
	r.SetPublickeyExtraRepo(extra)

	_, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	require.Len(t, extra.upserts, 1, "収まる keyId だけ保存される")
	assert.Equal(t, "https://remote.example/users/alice#ed25519-key", extra.upserts[0].KeyID)
}
