// Package password centralises how mk-go hashes account passwords.
//
// **経路ごとに cost を書かない。** 以前は signup / パスワード変更が
// bcrypt.DefaultCost、パスワードリセットだけ 8 で、リセットするとハッシュの
// 強度が下がっていた。1 箇所に集約すれば、その手のずれが起こらない。
//
// upstream Misskey は全経路で cost 8 固定。mk-go は既定 10 で、運用者が
// 設定で上げられる (bcrypt のハッシュは `$2a$NN$` に cost が埋まっているので、
// **cost が混在しても同じ関数で検証できる**。TS から引き継いだハッシュ (cost 8) も
// そのまま検証できる)。
package password

import (
	"fmt"
	"sync/atomic"

	"golang.org/x/crypto/bcrypt"
)

const (
	// DefaultCost is used when the operator sets nothing.
	//
	// **10 は OWASP の下限。** 既定を上げないのは、既存のデプロイのログイン
	// 待ち時間が黙って伸びるため (cost を 1 上げると所要時間は倍になる)。
	// 上げたい運用者は設定で上げる。
	DefaultCost = 10
	// MinCost / MaxCost are what bcrypt itself accepts.
	MinCost = bcrypt.MinCost
	MaxCost = bcrypt.MaxCost
)

// cost は起動時に 1 度だけ設定される。**プロセス全体の設定値**なので、
// 5 つのパッケージに config を通すより package 変数のほうが読みやすい。
// atomic なのは、設定前後で読み書きが競合しても壊れないようにするため
// (テストが SetCost を呼ぶ)。
var cost atomic.Int32

func init() { cost.Store(DefaultCost) }

// SetCost replaces the work factor used for new hashes. 範囲外は拒否して
// 既定のままにする — **設定の書き間違いで cost 4 のハッシュを量産しない**。
func SetCost(c int) error {
	if c < MinCost || c > MaxCost {
		return fmt.Errorf("password: bcrypt cost %d は %d..%d の範囲外です", c, MinCost, MaxCost)
	}
	cost.Store(int32(c))
	return nil
}

// Cost returns the work factor currently used for new hashes.
func Cost() int { return int(cost.Load()) }

// Hash returns the bcrypt hash of plain.
//
// 73 byte 以上は bcrypt.ErrPasswordTooLong をそのまま返す。呼び出し側は既に
// errors.Is で拾って 400 に変換している (Node の bcrypt は黙って切り詰めるが、
// Go は error にするので、拾わないと空ハッシュが DB に入る、#1075)。
func Hash(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), Cost())
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// NeedsRehash reports whether hash was produced with a weaker work factor than
// the one configured now.
//
// **これが無いと設定を上げても意味がない。** 既存の利用者のハッシュは変更する
// まで古い cost のまま残る。ログインは平文を持っている唯一の機会なので、そこで
// 焼き直す。
//
// 読めないハッシュは false を返す。bcrypt 以外が入っている可能性は無いが、
// 読めないものを「弱い」と判断して上書きすると、パスワードを検証できないまま
// 別の形式に潰しかねない。
func NeedsRehash(hash string) bool {
	c, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		return false
	}
	return c < Cost()
}
