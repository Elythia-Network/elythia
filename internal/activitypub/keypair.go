package activitypub

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"time"
)

// randReader is the entropy source used by GenerateRSAKeypair. テストで差し替え
// 可能なように package-level の var にしている。
var randReader io.Reader = rand.Reader

// nowFunc returns the current time. テストで差し替え可能。
var nowFunc = time.Now

// KeyType discriminates between supported HTTP Signature key algorithms.
// RSAはMastodon/Misskeyの事実上の標準、Ed25519は新世代Fediverse実装で
// 採用されつつある軽量・高速な署名方式。
type KeyType int

const (
	// KeyTypeRSA is used for RSA-SHA256 / RSA-SHA512 / hs2019(RSA) signatures.
	KeyTypeRSA KeyType = iota
	// KeyTypeEd25519 is used for ed25519 / hs2019(Ed25519) signatures.
	KeyTypeEd25519
)

// KeyTypeName returns the canonical short name of a key type ("rsa" /
// "ed25519").
//
// algorithmForKeyType が返すのは Signature header に載せる algorithm 文字列
// (rsa-sha256 等) で、こちらは鍵種別そのものの語彙。受信した署名がどの方式だった
// かを記録する用途で使う (#2393)。header の algorithm は "" や hs2019 がありうる
// ため、観測値としては鍵種別の方が実態に忠実。
//
// 値は model.SignatureAlg{RSA,Ed25519} と一致していなければならない (model は
// 最下層なので activitypub を import できず、定数を共有できない)。一致は
// core/instance の test で担保する。
func KeyTypeName(kt KeyType) string {
	if kt == KeyTypeEd25519 {
		return "ed25519"
	}
	return "rsa"
}

// GenerateRSAKeypair returns a fresh 2048-bit RSA keypair encoded as PEM
// strings (private + public). 失敗時はエラーを返す。MarshalPKIXPublicKey は
// RSAキーに対しては常に成功するため、その後のエラーチェックは省略している。
//
// 秘密鍵は **PKCS#8 (`BEGIN PRIVATE KEY`)** で出す。upstream の genRsaKeyPair が
// `privateKeyEncoding: { type: 'pkcs8' }` を使っており、署名時に読むのは Rust 製の
// `slacc` の `RsaKeyPair.fromPem()` で、**これは PKCS#8 しか受け付けない**。
// PKCS#1 (`BEGIN RSA PRIVATE KEY`) を渡すと `no items found` で落ちる。
//
// mk-go 自身は ParseRSAPrivateKey が両形式を読めるので PKCS#1 でも動いてしまい、
// **mk-go で作ったユーザーを TS に引き渡したときだけ送信側の連合が全滅する**という
// 形で表面化する。TS→mk-go→TS の swap test は鍵を作るのが TS なので踏まない
// (#2379 の mk-go→TS 経路で発見)。TS へ引き渡すこと (復路) は保証しなくなった
// (#3191) が、鍵の形式は本家と揃えておく。
func GenerateRSAKeypair() (privatePEM string, publicPEM string, err error) {
	priv, err := rsa.GenerateKey(randReader, 2048)
	if err != nil {
		return "", "", err
	}
	privBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", "", err
	}
	privBlock := &pem.Block{Type: "PRIVATE KEY", Bytes: privBytes}

	pubBytes, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	pubBlock := &pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes}

	return string(pem.EncodeToMemory(privBlock)), string(pem.EncodeToMemory(pubBlock)), nil
}

// GenerateEd25519Keypair returns a fresh Ed25519 keypair encoded as PEM
// strings (private + public). 鍵生成はioエラーの時だけ失敗するため、
// MarshalPKCS8PrivateKey / MarshalPKIXPublicKey側のエラーチェックは省略。
func GenerateEd25519Keypair() (privatePEM string, publicPEM string, err error) {
	pub, priv, err := ed25519.GenerateKey(randReader)
	if err != nil {
		return "", "", err
	}
	privBytes, _ := x509.MarshalPKCS8PrivateKey(priv)
	privBlock := &pem.Block{Type: "PRIVATE KEY", Bytes: privBytes}

	pubBytes, _ := x509.MarshalPKIXPublicKey(pub)
	pubBlock := &pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes}

	return string(pem.EncodeToMemory(privBlock)), string(pem.EncodeToMemory(pubBlock)), nil
}

// ParseRSAPrivateKey decodes a PEM-encoded RSA private key.
// PKCS1 ("BEGIN RSA PRIVATE KEY") と PKCS8 ("BEGIN PRIVATE KEY") の両形式を
// 受け付ける。Misskey TS と #2378 以降の mk-go は PKCS8 で RSA 鍵を保存する。
// PKCS1 は #2378 より前に mk-go が生成した鍵で、起動時に PKCS8 へ変換している。
func ParseRSAPrivateKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("invalid PEM block")
	}
	// 1. まず PKCS1 (#2378 より前に mk-go が生成した鍵はこの形式) を試す。
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	// 2. PKCS1 失敗時は PKCS8 を試す。TS Misskey は openssl 3.x デフォルトで
	//    `-----BEGIN PRIVATE KEY-----` の PKCS8 形式を生成し、#2378 以降の mk-go も同じ。
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse RSA private key (tried PKCS1 and PKCS8): %w", err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("PKCS8 key is not RSA: %T", parsed)
	}
	return rsaKey, nil
}

// ParseEd25519PrivateKey decodes a PEM-encoded Ed25519 private key (PKCS8).
func ParseEd25519PrivateKey(pemStr string) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("invalid PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("not an Ed25519 private key")
	}
	return priv, nil
}

// ParseRSAPublicKey decodes a PEM-encoded RSA public key.
// 既存呼び出し側の互換のため残す。新規コードはParsePublicKeyを使うこと。
func ParseRSAPublicKey(pemStr string) (*rsa.PublicKey, error) {
	pub, kt, err := ParsePublicKey(pemStr)
	if err != nil {
		return nil, err
	}
	if kt != KeyTypeRSA {
		return nil, errors.New("not an RSA public key")
	}
	return pub.(*rsa.PublicKey), nil
}

// MarshalEd25519PublicKeyPEM encodes an Ed25519 public key into the PKIX
// SubjectPublicKeyInfo PEM form so it can be stored in user_publickey_extra
// alongside RSA keys (= 同一テーブルで ParsePublicKey が type-dispatch 可能)。
// 入力 key の長さは ed25519.PublicKeySize (= 32 byte) でなければならない。
func MarshalEd25519PublicKeyPEM(pub ed25519.PublicKey) (string, error) {
	if len(pub) != ed25519.PublicKeySize {
		return "", fmt.Errorf("invalid ed25519 public key size %d", len(pub))
	}
	pubBytes, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	block := &pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes}
	return string(pem.EncodeToMemory(block)), nil
}

// ParseEd25519PublicKeyPEM decodes a PEM-encoded Ed25519 public key
// (PKIX SubjectPublicKeyInfo). Errors are returned when the PEM is missing,
// malformed, or contains a non-Ed25519 key (= caller can fall back to RSA).
func ParseEd25519PublicKeyPEM(pemStr string) (ed25519.PublicKey, error) {
	pub, kt, err := ParsePublicKey(pemStr)
	if err != nil {
		return nil, err
	}
	if kt != KeyTypeEd25519 {
		return nil, errors.New("not an Ed25519 public key")
	}
	return pub.(ed25519.PublicKey), nil
}

// ParsePublicKey decodes a PEM-encoded public key and reports its type.
// RSA / Ed25519を識別できる。それ以外は "unsupported public key type" で
// 拒否する (Misskeyフェデレーションでは ECDSA等は事実上使われていない)。
func ParsePublicKey(pemStr string) (crypto.PublicKey, KeyType, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, 0, errors.New("invalid PEM block")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, 0, err
	}
	switch k := pub.(type) {
	case *rsa.PublicKey:
		return k, KeyTypeRSA, nil
	case ed25519.PublicKey:
		return k, KeyTypeEd25519, nil
	default:
		return nil, 0, fmt.Errorf("unsupported public key type: %T", pub)
	}
}

// ConvertRSAPrivateKeyToPKCS8 re-encodes a PEM-encoded RSA private key as
// PKCS#8 (`BEGIN PRIVATE KEY`). 既に PKCS#8 ならそのまま返す (冪等)。
//
// 鍵そのものは変わらず、**PEM のエンコーディングだけ**が変わる。公開鍵も鍵 ID も
// 不変なので、連合相手から見て何も変わらない。
//
// mk-go は 000072 以前 PKCS#1 で発行しており、Misskey TS はそれを読めない
// (#2378)。既存インスタンスを救うための変換に使う。
func ConvertRSAPrivateKeyToPKCS8(privatePEM string) (string, error) {
	block, _ := pem.Decode([]byte(privatePEM))
	if block == nil {
		return "", errors.New("activitypub: PEM をデコードできない")
	}
	if block.Type == "PRIVATE KEY" {
		// 既に PKCS#8。冪等に扱う。
		return privatePEM, nil
	}
	key, err := ParseRSAPrivateKey(privatePEM)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}
