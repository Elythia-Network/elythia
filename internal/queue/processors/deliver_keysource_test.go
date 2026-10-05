package processors

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue"
)

// newTestKeyPEM mints a throwaway RSA key. `deliver_test.go` の
// generateTestKey は package processors_test 側にあり、ここからは見えない。
func newTestKeyPEM(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 1024)
	require.NoError(t, err)
	der := x509.MarshalPKCS1PrivateKey(priv)
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}))
}

// stubKeySource records lookups so tests can assert the DB path is taken.
type stubKeySource struct {
	rsa      string
	ed       string
	err      error
	calls    int
	lastUID  string
	lastKind string
}

func (s *stubKeySource) SigningKeyPEM(userID, kind string) (string, error) {
	s.calls++
	s.lastUID, s.lastKind = userID, kind
	if s.err != nil {
		return "", s.err
	}
	if kind == keyKindEd25519 {
		return s.ed, nil
	}
	return s.rsa, nil
}

// **payload に鍵が無ければ SignerUserID から引く。**
func TestResolveSigningKey_FetchesWhenPayloadHasNoPEM(t *testing.T) {
	src := &stubKeySource{rsa: newTestKeyPEM(t)}
	p := NewDeliverProcessor(nil)
	p.SetSigningKeySource(src)

	key, err := p.resolveSigningKey(keyKindRSA, queue.DeliverPayload{
		KeyID:        "https://example.com/users/u1#main-key",
		SignerUserID: "u1",
	})
	require.NoError(t, err)
	require.NotNil(t, key)
	require.Equal(t, 1, src.calls, "DB から引くこと")
	require.Equal(t, "u1", src.lastUID)
	require.Equal(t, keyKindRSA, src.lastKind)
}

// **同じ署名者の 2 回目はキャッシュに乗る。** fan-out で inbox ごとに 1 job
// なので、ここを外すと配送のたびに DB を引く。
func TestResolveSigningKey_CachesLookup(t *testing.T) {
	src := &stubKeySource{rsa: newTestKeyPEM(t)}
	p := NewDeliverProcessor(nil)
	p.SetSigningKeySource(src)
	payload := queue.DeliverPayload{
		KeyID:        "https://example.com/users/u2#main-key",
		SignerUserID: "u2",
	}

	_, err := p.resolveSigningKey(keyKindRSA, payload)
	require.NoError(t, err)
	_, err = p.resolveSigningKey(keyKindRSA, payload)
	require.NoError(t, err)
	require.Equal(t, 1, src.calls, "2 回目はキャッシュから返すこと")
}

// **移行期間: 古い job は payload の PEM を使う。** この変更より前に積まれた
// job は SignerUserID を持たないので、DB を引きに行くと配送できない。
func TestResolveSigningKey_PrefersInlinePEM(t *testing.T) {
	src := &stubKeySource{err: errors.New("引いてはいけない")}
	p := NewDeliverProcessor(nil)
	p.SetSigningKeySource(src)

	key, err := p.resolveSigningKey(keyKindRSA, queue.DeliverPayload{
		KeyID:  "https://example.com/users/u3#main-key",
		KeyPEM: newTestKeyPEM(t),
	})
	require.NoError(t, err)
	require.NotNil(t, key)
	require.Equal(t, 0, src.calls, "payload に PEM があれば DB を引かないこと")
}

// **DB 障害を「鍵が無い」にしない** (#2792)。
func TestResolveSigningKey_PropagatesSourceError(t *testing.T) {
	src := &stubKeySource{err: errors.New("connection refused")}
	p := NewDeliverProcessor(nil)
	p.SetSigningKeySource(src)

	_, err := p.resolveSigningKey(keyKindRSA, queue.DeliverPayload{
		KeyID:        "https://example.com/users/u4#main-key",
		SignerUserID: "u4",
	})
	require.ErrorContains(t, err, "connection refused")
}

// 鍵が無いユーザーは、空文字ではなく明示のエラーにする。
func TestResolveSigningKey_EmptyPEMIsError(t *testing.T) {
	src := &stubKeySource{rsa: ""}
	p := NewDeliverProcessor(nil)
	p.SetSigningKeySource(src)

	_, err := p.resolveSigningKey(keyKindRSA, queue.DeliverPayload{
		KeyID:        "https://example.com/users/u5#main-key",
		SignerUserID: "u5",
	})
	// 文言ではなく sentinel を見る。呼び出し元はこれで retry / ErrSkipRetry を
	// 分けるので、ここが崩れると配送が恒久的に消える形に化ける。
	require.ErrorIs(t, err, ErrSigningKeyMissing)
}

// **source 未配線でも「鍵が無い」ではなく設定漏れとして落とす。**
func TestResolveSigningKey_NoSourceConfigured(t *testing.T) {
	p := NewDeliverProcessor(nil)
	_, err := p.resolveSigningKey(keyKindRSA, queue.DeliverPayload{
		KeyID:        "https://example.com/users/u6#main-key",
		SignerUserID: "u6",
	})
	require.ErrorContains(t, err, "no signing key source configured")
}

// **キャッシュキーに keyID も含まれること。** `PrivateKey` は keyID を保持する
// ので、`config.url` を変えた直後に古い keyID の鍵で署名しないため。同一
// ユーザーでも keyID が違えば引き直す。
func TestResolveSigningKey_CacheSeparatesByKeyID(t *testing.T) {
	src := &stubKeySource{rsa: newTestKeyPEM(t)}
	p := NewDeliverProcessor(nil)
	p.SetSigningKeySource(src)

	_, err := p.resolveSigningKey(keyKindRSA, queue.DeliverPayload{
		KeyID: "https://old.example/users/u1#main-key", SignerUserID: "u1",
	})
	require.NoError(t, err)
	_, err = p.resolveSigningKey(keyKindRSA, queue.DeliverPayload{
		KeyID: "https://new.example/users/u1#main-key", SignerUserID: "u1",
	})
	require.NoError(t, err)
	require.Equal(t, 2, src.calls,
		"keyID が変わったのにキャッシュを再利用している (古い keyID で署名する)")
}
