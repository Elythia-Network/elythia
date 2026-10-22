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
	"log/slog"

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

// Hasher hashes passwords with one bcrypt work factor.
//
// The zero value is ready to use and hashes with DefaultCost.
//
// **costはパッケージの変数ではなく値で持つ(#3780)。** 以前は起動時に
// SetCostでプロセス全体の変数を書き換えていたが、サーバーの世代を作るたびに
// 同じ設定の値で書き直すだけで、テストはそれを毎回戻す必要があった。使う部品へ
// 値で渡せば、戻すものが無くなる。
type Hasher struct {
	// costは0ならDefaultCost。0以外はNewHasherが範囲を確かめた値だけが入る。
	cost int
}

// NewHasher returns a Hasher that uses cost for new hashes, or an error when
// cost is outside MinCost..MaxCost.
//
// 範囲外は拒否する。**設定の書き間違いでcost 4のハッシュを量産しない。**
func NewHasher(cost int) (Hasher, error) {
	if cost < MinCost || cost > MaxCost {
		return Hasher{}, fmt.Errorf("password: bcrypt cost %d は %d..%d の範囲外です", cost, MinCost, MaxCost)
	}
	return Hasher{cost: cost}, nil
}

// HasherFromConfig returns the Hasher for the configured cost. 0 means unset
// and selects DefaultCost. A cost outside MinCost..MaxCost is logged and also
// falls back to DefaultCost.
//
// **ゼロ値は「未設定」として扱う。** config.resolve を通れば必ず範囲内の値が
// 入るが、Config を構造体リテラルで組む呼び出し元 (テスト / ツール) では 0 の
// まま来る。そこで起動を失敗させると、この項目を知らない呼び出し元が全部壊れる。
// 範囲外も resolve を通っていれば到達しない。通っていない呼び出し元のために
// 既定へ落として続行する。
func HasherFromConfig(cost int) Hasher {
	if cost == 0 {
		return Hasher{}
	}
	h, err := NewHasher(cost)
	if err != nil {
		slog.Warn("bcrypt cost が不正なので既定値を使います", "value", cost, "err", err)
		return Hasher{}
	}
	return h
}

// Cost returns the work factor h uses for new hashes.
func (h Hasher) Cost() int {
	if h.cost == 0 {
		return DefaultCost
	}
	return h.cost
}

// Hash returns the bcrypt hash of plain.
//
// 73 byte 以上は bcrypt.ErrPasswordTooLong をそのまま返す。呼び出し側は既に
// errors.Is で拾って 400 に変換している (Node の bcrypt は黙って切り詰めるが、
// Go は error にするので、拾わないと空ハッシュが DB に入る、#1075)。
func (h Hasher) Hash(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), h.Cost())
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// NeedsRehash reports whether hash was produced with a weaker work factor than
// the one h uses.
//
// **これが無いと設定を上げても意味がない。** 既存の利用者のハッシュは変更する
// まで古い cost のまま残る。ログインは平文を持っている唯一の機会なので、そこで
// 焼き直す。
//
// 読めないハッシュは false を返す。bcrypt 以外が入っている可能性は無いが、
// 読めないものを「弱い」と判断して上書きすると、パスワードを検証できないまま
// 別の形式に潰しかねない。
func (h Hasher) NeedsRehash(hash string) bool {
	c, err := bcrypt.Cost([]byte(hash))
	if err != nil {
		return false
	}
	return c < h.Cost()
}
