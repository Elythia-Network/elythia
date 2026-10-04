package misc

import (
	"crypto/rand"
	"encoding/hex"
	"io"
)

// randReader is the source of cryptographic randomness.
// テスト時に差し替え可能にするためパッケージ変数として公開
var randReader io.Reader = rand.Reader

// SecureRandomHex returns a hex-encoded random string of n characters.
func SecureRandomHex(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := io.ReadFull(randReader, b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)[:n]
}

// AlphanumericChars mirrors upstream secureRndstr's default LU_CHARS
// (digits + lower + upper, 62 chars)。admin-issued temporary password 等で
// upstream と shape を揃えるために使う (#1825)。
const AlphanumericChars = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// SecureRandomString returns a cryptographically random string of length
// characters drawn from chars. Mirrors upstream secureRndstr (62-char
// alphanumeric set では modulo bias は無視できる程度で、upstream の
// floor(byte/0xFF*len) と同程度)。RNG 失敗時は panic する。
func SecureRandomString(length int, chars string) string {
	if length <= 0 || len(chars) == 0 {
		return ""
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(randReader, buf); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	// 乱数バイト列をそのまま charset の index に写像して in-place で書き戻す。
	for i := range buf {
		buf[i] = chars[int(buf[i])%len(chars)]
	}
	return string(buf)
}

// NativeTokenLength is how many characters a native session token has.
//
// **16 のまま揃えておく。** Misskey TS の `isNativeUserToken` は長さだけで native
// token とアプリのアクセストークンを判別する (`token.length === 16`) ので、
// 伸ばすと drop-in で TS に引き渡したときアプリのトークンとして扱われる。TS へ
// 引き渡すこと (復路) は保証しない (#3191) が、伸ばす理由も無いので本家と揃えておく。
const NativeTokenLength = 16

// NewNativeToken returns a fresh native session token.
//
// **英数字 62 文字から採る。** 16 進にすると見た目は同じ 16 文字でも 64 bit
// しかなく、upstream の `secureRndstr(16)` (約 95 bit) より 31 bit 弱くなる。
// 長さを動かせない以上、強度は文字集合でしか稼げない。
func NewNativeToken() string {
	return SecureRandomString(NativeTokenLength, AlphanumericChars)
}
