package signin

import (
	"github.com/elythia-network/elythia/internal/core/twofactor"
	"github.com/elythia-network/elythia/internal/repository"
)

// SwapReadRandomBytes lets external tests stub the random source for the
// passkey-context generator. Returns the previous value so callers can
// restore it.
//
// このシンボルは `_test.go` にしか存在しないので production binary には
// 含まれない (Go の test build 時のみ コンパイルされる)。test seam を
// production に export しないための慣用パターン。
func SwapReadRandomBytes(fn func([]byte) (int, error)) func([]byte) (int, error) {
	old := readRandom
	readRandom = fn
	return old
}

// NewPasskeyContextForTest exposes the context generator so external tests can
// assert that what it produces passes ValidPasskeyContextForTest (#3037)。
// **生成と検査を 1 対 1 に保つための seam。** 片方だけ長さを変えると検査が
// 実質的に消えるので、テストからその関係を固定する。
func NewPasskeyContextForTest() (string, error) { return newPasskeyContext() }

// ValidPasskeyContextForTest exposes the format check.
func ValidPasskeyContextForTest(s string) bool { return validPasskeyContext(s) }

// 下の …ForTest は、構築子の必須の引数にした依存 (#3823) を、補助関数が作った
// handler へ後から入れるための seam。同じパッケージのテストならフィールドへ直接
// 代入するところを、外部パッケージ (signin_test) のテストから行う。

// SetTOTPReplayGuardForTest replaces the TOTP replay guard.
func (h *Handler) SetTOTPReplayGuardForTest(g twofactor.ReplayGuard) { h.totpReplayGuard = g }

// SetMetaRepoForTest replaces the meta lookup for the mail locale fallback.
func (h *Handler) SetMetaRepoForTest(r repository.MetaRepository) { h.metaRepo = r }
