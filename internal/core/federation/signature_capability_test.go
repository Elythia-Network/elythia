package federation_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubCapabilityDeclarer captures RecordEd25519Declared calls.
type stubCapabilityDeclarer struct {
	hosts []string
	err   error
}

func (s *stubCapabilityDeclarer) RecordEd25519Declared(host string, _ time.Time) error {
	s.hosts = append(s.hosts, host)
	return s.err
}

func ed25519ActorBody(t *testing.T, keyIDs ...string) string {
	t.Helper()
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
		"assertionMethod": [`
	for i, keyID := range keyIDs {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		mb, err := activitypub.EncodeEd25519Multikey(pub)
		require.NoError(t, err)
		if i > 0 {
			body += ","
		}
		body += `{"id": "` + keyID + `", "type": "Multikey",
			"controller": "https://remote.example/users/alice",
			"publicKeyMultibase": "` + mb + `"}`
	}
	return body + `]}`
}

// assertionMethod に Ed25519 鍵があれば、その host を「Ed25519 対応を宣言して
// いる」として記録する。
func TestResolveActor_DeclaresEd25519Capability(t *testing.T) {
	r, _ := newResolver(t, ed25519ActorBody(t, "https://remote.example/users/alice#ed25519-key"), nil)
	r.SetPublickeyExtraRepo(&stubPublickeyExtraRepo{})
	declarer := &stubCapabilityDeclarer{}
	r.SetSignatureCapabilityDeclarer(declarer)

	_, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)

	// host は user.Host / instance.host と同じ導出でなければ一覧と突合できない。
	assert.Equal(t, []string{"remote.example"}, declarer.hosts)
}

// 鍵が複数あっても host 単位の事実は 1 つ。鍵の数だけ UPDATE を撃たない。
func TestResolveActor_DeclaresEd25519CapabilityOncePerActor(t *testing.T) {
	body := ed25519ActorBody(t,
		"https://remote.example/users/alice#ed25519-key",
		"https://remote.example/users/alice#ed25519-key-2")
	r, _ := newResolver(t, body, nil)
	r.SetPublickeyExtraRepo(&stubPublickeyExtraRepo{})
	declarer := &stubCapabilityDeclarer{}
	r.SetSignatureCapabilityDeclarer(declarer)

	_, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.Equal(t, []string{"remote.example"}, declarer.hosts, "鍵 2 本でも記録は 1 回")
}

// assertionMethod が無い actor では宣言を記録しない。ここが漏れると RSA のみの
// サーバー全部に Ed25519 ラベルが付く。
func TestResolveActor_NoDeclarationWithoutAssertionMethod(t *testing.T) {
	body := `{ "@context": "https://www.w3.org/ns/activitystreams",
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
	r, _ := newResolver(t, body, nil)
	r.SetPublickeyExtraRepo(&stubPublickeyExtraRepo{})
	declarer := &stubCapabilityDeclarer{}
	r.SetSignatureCapabilityDeclarer(declarer)

	_, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.Empty(t, declarer.hosts)
}

// entry が全て不正 (decode 失敗) なら宣言しない。upsert が 1 件も成功して
// いないのに「対応している」と記録するのは誤り。
func TestResolveActor_NoDeclarationWhenAllAssertionMethodsInvalid(t *testing.T) {
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
			{"id": "https://remote.example/users/alice#bad", "type": "Multikey",
			 "controller": "x", "publicKeyMultibase": "INVALID"}
		]
	}`
	r, _ := newResolver(t, body, nil)
	r.SetPublickeyExtraRepo(&stubPublickeyExtraRepo{})
	declarer := &stubCapabilityDeclarer{}
	r.SetSignatureCapabilityDeclarer(declarer)

	_, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.Empty(t, declarer.hosts)
}

// 宣言の記録に失敗しても actor の取り込みは続く (best-effort)。
func TestResolveActor_DeclarationFailureIsNonFatal(t *testing.T) {
	r, _ := newResolver(t, ed25519ActorBody(t, "https://remote.example/users/alice#ed25519-key"), nil)
	extra := &stubPublickeyExtraRepo{}
	r.SetPublickeyExtraRepo(extra)
	r.SetSignatureCapabilityDeclarer(&stubCapabilityDeclarer{err: errors.New("db down")})

	user, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	require.NotNil(t, user)
	_, err = extra.FindByUserAndKeyID(user.ID, "https://remote.example/users/alice#ed25519-key")
	assert.NoError(t, err, "鍵の永続化は宣言記録の失敗に巻き込まれない")
}

// declarer 未配線でも panic しない (既存デプロイ互換)。
func TestResolveActor_DeclarerUnwired(t *testing.T) {
	r, _ := newResolver(t, ed25519ActorBody(t, "https://remote.example/users/alice#ed25519-key"), nil)
	r.SetPublickeyExtraRepo(&stubPublickeyExtraRepo{})

	_, err := r.ResolveActor("https://remote.example/users/alice")
	assert.NoError(t, err)
}

// 参照形式 (bare IRI) だけの assertionMethod では宣言しない。ref は purge 保護の
// ために受領 keyId として扱うが、鍵素材が無いので Ed25519 対応の根拠にならない。
// ref だけで宣言すると鍵が 1 本も無い host が「対応」と表示される (#2662)。
func TestResolveActor_BareIRIAssertionMethodDoesNotDeclare(t *testing.T) {
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
		"assertionMethod": ["https://remote.example/users/alice#ed25519-key"]
	}`
	r, _ := newResolver(t, body, nil)
	r.SetPublickeyExtraRepo(&stubPublickeyExtraRepo{})
	declarer := &stubCapabilityDeclarer{}
	r.SetSignatureCapabilityDeclarer(declarer)

	_, err := r.ResolveActor("https://remote.example/users/alice")
	require.NoError(t, err)
	assert.Empty(t, declarer.hosts, "鍵素材の無い参照だけでは宣言しない")
}
