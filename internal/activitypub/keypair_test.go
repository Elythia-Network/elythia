package activitypub

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateAndParseKeypair(t *testing.T) {
	priv, pub, err := GenerateRSAKeypair()
	require.NoError(t, err)
	// 秘密鍵は PKCS#8 (`BEGIN PRIVATE KEY`) でなければならない。upstream の署名は
	// Rust 製 slacc の RsaKeyPair.fromPem() で読み、**PKCS#1 を受け付けない**。
	// PKCS#1 で出すと、mk-go が作ったユーザーを TS に引き渡したとき送信側の連合が
	// 全滅する (#2379 の経路検証で発見、#2378 で修正)。TS へ引き渡すこと (復路) は
	// 保証しない (#3191) が、本家と同じ形式で出しておく。mk-go 自身は両方を読める。
	assert.Contains(t, priv, "BEGIN PRIVATE KEY")
	assert.NotContains(t, priv, "RSA PRIVATE KEY", "PKCS#1 は TS が読めない")
	assert.Contains(t, pub, "PUBLIC KEY")

	rsaPriv, err := ParseRSAPrivateKey(priv)
	require.NoError(t, err)
	assert.NotNil(t, rsaPriv)

	rsaPub, err := ParseRSAPublicKey(pub)
	require.NoError(t, err)
	assert.NotNil(t, rsaPub)
}

func TestParseRSAPrivateKey_Invalid(t *testing.T) {
	_, err := ParseRSAPrivateKey("not pem")
	assert.Error(t, err)
}

func TestParseRSAPrivateKey_BadContents(t *testing.T) {
	pem := "-----BEGIN RSA PRIVATE KEY-----\nbm90YWtleQ==\n-----END RSA PRIVATE KEY-----\n"
	_, err := ParseRSAPrivateKey(pem)
	assert.Error(t, err)
}

// TestParseRSAPrivateKey_PKCS8 は Misskey TS が格納する PKCS8 形式の RSA
// 秘密鍵を mk-go が読めることを確認する回帰テスト (#369 drop-in)。
func TestParseRSAPrivateKey_PKCS8(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	// openssl 3.x デフォルトと同じ PKCS8 形式で encode する。
	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	pemStr := string(pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: pkcs8Bytes,
	}))

	parsed, err := ParseRSAPrivateKey(pemStr)
	require.NoError(t, err, "PKCS8 形式の RSA 鍵が parse できる")
	assert.Equal(t, priv.N, parsed.N)
}

// TestParseRSAPrivateKey_PKCS8NotRSA は PKCS8 に RSA 以外の鍵 (ed25519)
// が入っていた場合は明示的に reject することを確認する。
func TestParseRSAPrivateKey_PKCS8NotRSA(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	pemStr := string(pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: pkcs8Bytes,
	}))

	_, err = ParseRSAPrivateKey(pemStr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not RSA")
}

func TestParseRSAPublicKey_Invalid(t *testing.T) {
	_, err := ParseRSAPublicKey("not pem")
	assert.Error(t, err)
}

func TestParseRSAPublicKey_BadContents(t *testing.T) {
	pem := "-----BEGIN PUBLIC KEY-----\nbm90YWtleQ==\n-----END PUBLIC KEY-----\n"
	_, err := ParseRSAPublicKey(pem)
	assert.Error(t, err)
}

func TestParseRSAPublicKey_NotRSA(t *testing.T) {
	// Ed25519 鍵を生成して PEM にエンコードし、ParseRSAPublicKey の
	// 「not RSA」分岐を踏ませる。
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	derBytes, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: derBytes}))

	_, err = ParseRSAPublicKey(pemStr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not an RSA")
}

// errReader is an io.Reader that always errors. signature_test.go の
// TestSignRequest_RandError で randReader を差し替えるために共有する
// (Go 1.26 で rsa.GenerateKey 系の random 引数は無視されるが、
// rsa.SignPKCS1v15 / ed25519 経路では引き続き使われる)。
type errReader struct{}

func (errReader) Read(_ []byte) (int, error) { return 0, assertErr }

var assertErr = newSentinelErr("rand fail")

type sentinelErr struct{ msg string }

func (e *sentinelErr) Error() string { return e.msg }

func newSentinelErr(s string) error { return &sentinelErr{msg: s} }

// PKCS#1 で保存された既存の鍵を PKCS#8 に変換できる (#2378)。鍵そのものは
// 変わらず PEM のエンコーディングだけが変わることを、公開鍵の一致で確かめる。
func TestConvertRSAPrivateKeyToPKCS8(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	pkcs1 := string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv),
	}))

	got, err := ConvertRSAPrivateKeyToPKCS8(pkcs1)
	require.NoError(t, err)
	assert.Contains(t, got, "BEGIN PRIVATE KEY")
	assert.NotContains(t, got, "RSA PRIVATE KEY")

	// 鍵が同一であること (= 公開鍵が一致する)。
	reparsed, err := ParseRSAPrivateKey(got)
	require.NoError(t, err)
	assert.Equal(t, priv.PublicKey, reparsed.PublicKey, "変換で鍵が変わってはいけない")
}

// 冪等: 既に PKCS#8 ならそのまま返す。起動時に毎回走るので必須。
func TestConvertRSAPrivateKeyToPKCS8_Idempotent(t *testing.T) {
	priv, _, err := GenerateRSAKeypair()
	require.NoError(t, err)
	got, err := ConvertRSAPrivateKeyToPKCS8(priv)
	require.NoError(t, err)
	assert.Equal(t, priv, got)
}

func TestConvertRSAPrivateKeyToPKCS8_Invalid(t *testing.T) {
	_, err := ConvertRSAPrivateKeyToPKCS8("not a pem")
	assert.Error(t, err)
}
