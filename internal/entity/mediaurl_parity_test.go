package entity_test

// This test lives in the external entity_test package on purpose: it needs both
// entity (the URL builder) and core/mediaproxy (the proxy-side verifier), but
// core/mediaproxy -> core/drive -> entity, so an internal entity test importing
// mediaproxy would form an import cycle. Driving the builder through its
// exported API (ProxyAvatarURL) keeps the assertion end-to-end while staying
// outside that cycle.

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/mediaproxy"
	"github.com/elythia-network/elythia/internal/entity"
)

// TestProxySigAcceptedByMediaproxy guarantees the HMAC the builder appends to an
// internal-proxy URL is byte-identical to what mediaproxy.SignURL produces and
// is accepted by the same VerifyHMAC the proxy handler uses. If entity.signURL
// and mediaproxy.SignURL ever drift, every proxied remote URL would 403 — this
// test fails first.
func TestProxySigAcceptedByMediaproxy(t *testing.T) {
	secret := []byte("parity-secret-9f3a")
	entity.SetMediaURLContext(entity.NewMediaURLContext(
		"https://mk.example", "https://mk.example/proxy", secret, false, true))
	defer entity.SetMediaURLContext(nil)

	// A URL with spaces + query chars stresses the encode/decode round-trip the
	// sig depends on.
	raw := "https://remote.example/files/a b.png?w=1&h=2"
	built := entity.ProxyMediaURL(raw)

	u, err := url.Parse(built)
	if err != nil {
		t.Fatalf("builder produced unparseable URL %q: %v", built, err)
	}
	q := u.Query()
	if got := q.Get("url"); got != raw {
		t.Fatalf("url param did not round-trip: %q != %q", got, raw)
	}
	sig := q.Get("sig")
	if sig == "" {
		t.Fatal("internal-proxy URL is missing its sig")
	}
	if sig != mediaproxy.SignURL(secret, raw) {
		t.Errorf("entity sig != mediaproxy.SignURL — HMAC desync")
	}
	if !mediaproxy.VerifyHMAC(secret, raw, sig) {
		t.Error("mediaproxy.VerifyHMAC rejected the builder's sig")
	}
}

// **期限付き署名も proxy 側の検証と byte 単位で揃っていること (#3037)。**
// 素の署名と同じ理由 — ずれると URL プレビューの画像が全部 403 になる。
func TestExpiringProxySigAcceptedByMediaproxy(t *testing.T) {
	secret := []byte("parity-secret-9f3a")
	entity.SetMediaURLContext(entity.NewMediaURLContext(
		"https://mk.example", "https://mk.example/proxy", secret, false, true))
	defer entity.SetMediaURLContext(nil)

	raw := "https://remote.example/og/a b.png?w=1&h=2"
	built := entity.ProxyUserSuppliedMediaURLPtr(&raw)
	require.NotNil(t, built)

	u, err := url.Parse(*built)
	require.NoError(t, err)
	q := u.Query()
	require.Equal(t, raw, q.Get("url"), "url param did not round-trip")
	sig := q.Get("sig")
	require.NotEmpty(t, sig, "expiring proxy URL is missing its sig")

	// **期限が入っていること。** 素の署名 (hex 64 文字) のままだと無期限に
	// 戻っているので、形そのものを見る。
	exp, _, ok := strings.Cut(sig, ".")
	require.True(t, ok, "sig に期限が入っていない: %q", sig)
	sec, err := strconv.ParseInt(exp, 10, 64)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(entity.UserSuppliedProxyTTL), time.Unix(sec, 0), time.Minute)

	assert.Equal(t, mediaproxy.SignURLUntil(secret, raw, time.Unix(sec, 0)), sig,
		"entity 側と mediaproxy 側の署名がずれている")
	assert.True(t, mediaproxy.VerifyExpiringHMAC(secret, raw, sig, time.Now()),
		"mediaproxy.VerifyExpiringHMAC が builder の署名を拒否した")

	// 期限を過ぎたら通らない。
	assert.False(t, mediaproxy.VerifyExpiringHMAC(secret, raw, sig,
		time.Unix(sec, 0).Add(time.Second)))
}

// **管理者が入れる画像には期限を付けない。** ロールのアイコンやお知らせの
// 画像は長期間そのまま配る前提なので、期限を付けると「いつの間にか画像が
// 消える」事故になる。
func TestAdminSuppliedProxySigHasNoExpiry(t *testing.T) {
	entity.SetMediaURLContext(entity.NewMediaURLContext(
		"https://mk.example", "https://mk.example/proxy", []byte("k"), false, true))
	defer entity.SetMediaURLContext(nil)

	raw := "https://remote.example/role-icon.png"
	built := entity.ProxyMediaURLPtr(&raw)
	require.NotNil(t, built)

	u, err := url.Parse(*built)
	require.NoError(t, err)
	assert.NotContains(t, u.Query().Get("sig"), ".", "管理者設定の画像に期限が付いている")
}
