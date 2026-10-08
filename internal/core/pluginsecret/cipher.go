// Package pluginsecret stores server plugins' secret values encrypted at rest
// (#3470).
//
// # 暗号の形
//
//   - 鍵は設定ファイルの `pluginSecretKey` (32 バイトを base64 で書いたもの)。
//     そのまま使わず、HKDF-SHA256 で用途を固定した鍵を導出する。同じ鍵を
//     運営者が別の用途に流用しても、暗号文が別の用途の鍵で読める形にしない
//   - AES-256-GCM。nonce は値を書くたびに crypto/rand で 12 バイト作る
//   - AAD に「プラグイン名と値の名前」を入れる。DB の行の名前を書き換えて、
//     別のプラグインや別の名前の値として復号させることはできない
//   - 保存する形は `版 (1 バイト) || nonce || 暗号文 + 認証タグ`。版は形を
//     変えるときのためのもの
//
// # 鍵が無いとき
//
// 鍵が設定されていなければ、書き込みも読み出しも
// [plugin.ErrSecretsUnavailable] で断る。**平文で置く経路を作らない。**
// 「鍵を設定し忘れたら平文で入る」形にすると、守っているつもりで守れていない
// 状態に気付けないため。
package pluginsecret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
)

// KeySize is the length of pluginSecretKey after base64 decoding.
const KeySize = 32

// formatV1 tags the stored layout. 1 バイト目で判別する。
const formatV1 byte = 1

// hkdfInfo pins the derived key to this use. 変えると既存の値が全部読めなくなる。
const hkdfInfo = "elythia plugin secret v1"

// aadPrefix separates this AAD from any other use of the same key material.
const aadPrefix = "elythia/plugin-secret/v1"

// errOpen is returned when the ciphertext cannot be authenticated.
//
// 原因 (鍵違い / 改ざん / AAD 違い) を区別しない。GCM はそもそも区別できず、
// 区別できるように見せると誤った案内になる。
var errOpen = errors.New("pluginsecret: 復号できません")

// sealer encrypts and decrypts with one key.
type sealer struct {
	aead cipher.AEAD
}

// newSealer derives the AES-256-GCM key from the configured key.
func newSealer(key []byte) (*sealer, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("pluginsecret: 鍵は %d バイトである必要があります (%d バイトでした)", KeySize, len(key))
	}
	derived, err := hkdf.Key(sha256.New, key, nil, hkdfInfo, KeySize)
	if err != nil {
		return nil, fmt.Errorf("pluginsecret: 鍵を導出できません: %w", err)
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, fmt.Errorf("pluginsecret: 鍵を作れません: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("pluginsecret: GCM を作れません: %w", err)
	}
	return &sealer{aead: aead}, nil
}

// aad binds a ciphertext to the plugin and the secret name.
//
// 区切りに NUL を使う。プラグイン名も値の名前も NUL を含めないので、
// ("a", "bc") と ("ab", "c") が同じ AAD になることは無い。
func aad(pluginName, name string) []byte {
	b := make([]byte, 0, len(aadPrefix)+len(pluginName)+len(name)+2)
	b = append(b, aadPrefix...)
	b = append(b, 0)
	b = append(b, pluginName...)
	b = append(b, 0)
	b = append(b, name...)
	return b
}

// seal encrypts value for (pluginName, name).
func (s *sealer) seal(pluginName, name string, value []byte) ([]byte, error) {
	nonceSize := s.aead.NonceSize()
	out := make([]byte, 1+nonceSize, 1+nonceSize+len(value)+s.aead.Overhead())
	out[0] = formatV1
	if _, err := rand.Read(out[1 : 1+nonceSize]); err != nil {
		return nil, fmt.Errorf("pluginsecret: nonce を作れません: %w", err)
	}
	return s.aead.Seal(out, out[1:1+nonceSize], value, aad(pluginName, name)), nil
}

// open decrypts a value sealed for (pluginName, name).
func (s *sealer) open(pluginName, name string, sealed []byte) ([]byte, error) {
	nonceSize := s.aead.NonceSize()
	if len(sealed) < 1+nonceSize+s.aead.Overhead() || sealed[0] != formatV1 {
		return nil, errOpen
	}
	plain, err := s.aead.Open(nil, sealed[1:1+nonceSize], sealed[1+nonceSize:], aad(pluginName, name))
	if err != nil {
		return nil, errOpen
	}
	return plain, nil
}
