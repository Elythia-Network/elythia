package pluginsecret

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKey(b byte) []byte {
	return bytes.Repeat([]byte{b}, KeySize)
}

func mustSealer(t *testing.T, key []byte) *sealer {
	t.Helper()
	s, err := newSealer(key)
	require.NoError(t, err)
	return s
}

func TestSealer_RoundTrip(t *testing.T) {
	s := mustSealer(t, testKey(1))
	sealed, err := s.seal("bot", "apiKey", []byte("sk-0123456789"))
	require.NoError(t, err)

	plain, err := s.open("bot", "apiKey", sealed)
	require.NoError(t, err)
	assert.Equal(t, "sk-0123456789", string(plain))
}

// 保存する形が「版 || nonce || 暗号文 + タグ」で、平文をそのまま含まないこと。
func TestSealer_CiphertextIsNotPlaintext(t *testing.T) {
	s := mustSealer(t, testKey(1))
	value := []byte("plaintext-api-key-value")
	sealed, err := s.seal("bot", "apiKey", value)
	require.NoError(t, err)

	assert.False(t, bytes.Contains(sealed, value), "暗号文に平文が含まれている")
	assert.Equal(t, formatV1, sealed[0])
	assert.Len(t, sealed, 1+s.aead.NonceSize()+len(value)+s.aead.Overhead())
}

// nonce は書くたびに作り直す。同じ値を 2 回書いて同じ暗号文になると、
// GCM の nonce の使い回しになる。
func TestSealer_NonceIsFreshPerSeal(t *testing.T) {
	s := mustSealer(t, testKey(1))
	a, err := s.seal("bot", "apiKey", []byte("same"))
	require.NoError(t, err)
	b, err := s.seal("bot", "apiKey", []byte("same"))
	require.NoError(t, err)
	n := s.aead.NonceSize()
	assert.NotEqual(t, a[1:1+n], b[1:1+n], "nonce が使い回されている")
	assert.NotEqual(t, a, b)
}

func TestSealer_OtherKeyCannotOpen(t *testing.T) {
	sealed, err := mustSealer(t, testKey(1)).seal("bot", "apiKey", []byte("secret"))
	require.NoError(t, err)

	_, err = mustSealer(t, testKey(2)).open("bot", "apiKey", sealed)
	assert.ErrorIs(t, err, errOpen)
}

// **設定した鍵をそのまま AES の鍵に使っていないこと。** HKDF で用途を固定して
// いるので、生の鍵で GCM を組んでも開けない。
func TestSealer_UsesDerivedKey(t *testing.T) {
	key := testKey(7)
	s := mustSealer(t, key)
	sealed, err := s.seal("bot", "apiKey", []byte("secret"))
	require.NoError(t, err)

	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	raw, err := cipher.NewGCM(block)
	require.NoError(t, err)
	n := raw.NonceSize()
	_, err = raw.Open(nil, sealed[1:1+n], sealed[1+n:], aad("bot", "apiKey"))
	assert.Error(t, err, "生の鍵で開けてしまう (鍵を導出していない)")
}

// 暗号文はプラグイン名と値の名前に結び付いている (AAD)。DB の行の名前を
// 書き換えても、別のプラグイン・別の名前の値としては開けない。
func TestSealer_BindsPluginAndName(t *testing.T) {
	s := mustSealer(t, testKey(1))
	sealed, err := s.seal("bot", "apiKey", []byte("secret"))
	require.NoError(t, err)

	for _, tc := range []struct{ plugin, name string }{
		{"other", "apiKey"},
		{"bot", "token"},
		{"bo", "tapiKey"}, // 区切りをずらしても同じ AAD にならない
		{"botapiKey", ""},
	} {
		_, err := s.open(tc.plugin, tc.name, sealed)
		assert.ErrorIsf(t, err, errOpen, "(%q, %q) で開けてしまう", tc.plugin, tc.name)
	}
}

func TestSealer_RejectsTamperedOrMalformed(t *testing.T) {
	s := mustSealer(t, testKey(1))
	sealed, err := s.seal("bot", "apiKey", []byte("secret"))
	require.NoError(t, err)

	n := s.aead.NonceSize()
	for name, idx := range map[string]int{
		"version": 0,
		"nonce":   1,
		"body":    1 + n,
		"tag":     len(sealed) - 1,
	} {
		bad := append([]byte(nil), sealed...)
		bad[idx] ^= 0x01
		_, err := s.open("bot", "apiKey", bad)
		assert.ErrorIsf(t, err, errOpen, "%s を書き換えても開けてしまう", name)
	}

	_, err = s.open("bot", "apiKey", sealed[:1+n+s.aead.Overhead()-1])
	assert.ErrorIs(t, err, errOpen, "短すぎる値")
	_, err = s.open("bot", "apiKey", nil)
	assert.ErrorIs(t, err, errOpen)
}

func TestNewSealer_RejectsWrongKeySize(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		_, err := newSealer(make([]byte, n))
		assert.Errorf(t, err, "%d バイトの鍵を受け付けている", n)
	}
}

// goldenBlob was sealed once by the v1 code with key 0x00..0x1f for
// ("golden-bot", "apiKey"). **書き換えないこと。**
const goldenBlob = "013ce04efd0c45d9d990f7cd49d5c1f97d57b3ace9fde66d73804b1d53d4279f490b92e3a74a8ffff37d7a8a8785461632"

// **保存した形を固定する。** hkdfInfo / aadPrefix / AAD の区切り / 版を変えても、
// 往復するだけのテストは緑のままだが、更新したとたんに保存済みの値が全部
// 読めなくなる (プラグインが実行時に置いた値は入れ直せない)。形を変えるなら
// 版を上げ、v1 を読む経路を残すこと。
func TestSealer_OpensTheV1GoldenBlob(t *testing.T) {
	key := make([]byte, KeySize)
	for i := range key {
		key[i] = byte(i)
	}
	blob, err := hex.DecodeString(goldenBlob)
	require.NoError(t, err)
	plain, err := mustSealer(t, key).open("golden-bot", "apiKey", blob)
	require.NoError(t, err, "以前に保存した値が読めない (保存の形が変わった)")
	assert.Equal(t, "sk-golden-0123456789", string(plain))
}
