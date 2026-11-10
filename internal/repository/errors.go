package repository

import (
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
)

// ErrNotFound is returned when a lookup finds no row.
//
// GORM の `ErrRecordNotFound` の別名。**呼び出し側が gorm を import せずに
// 種別を判定できる**ようにするための公開面で、値そのものは同じなので
// `errors.Is(err, gorm.ErrRecordNotFound)` と互換。
var ErrNotFound = gorm.ErrRecordNotFound

// IsNotFound reports whether err means "the row does not exist".
//
// **これで判定しないと DB 障害が 4xx に化ける。** repository は GORM の error を
// そのまま返すので、`err != nil` をまとめて not-found 扱いにすると接続断や
// syntax error まで「そんなノートは無い」になる。クライアントからは区別できず、
// 監視でも 5xx が立たない (#2792)。
//
// upstream は `.findOneBy` の結果が `null` かどうかで判定するので、DB 障害は
// 例外として 500 に化ける。こちらもそれに合わせる。
func IsNotFound(err error) bool {
	// **`sql.ErrNoRows` も見る。** 現状 production code の repository はすべて
	// GORM 経由だが、どこかが `database/sql` / pgx 直叩きに変わったとき
	// `IsNotFound` が黙って false を返すと、**本物の not-found が 500 になる**。
	// 4xx → 500 の向きに倒したぶん、この誤りは表に出やすい。
	return errors.Is(err, ErrNotFound) || errors.Is(err, sql.ErrNoRows)
}

// IsRejectedValue reports whether PostgreSQL refused a value the client sent:
// too long for its column (SQLSTATE 22001), a number out of its column's
// range (22003), a date out of range (22008), a NUL byte (22021), or a NUL
// escape in a jsonb value (22P05). Handlers answer these with 400 instead of
// 500 (#4120).
//
// 本家は引数の宣言に長さや範囲の上限を持たない口が多く、同じ入力で DB のエラーになって
// 500 を返す。値そのものが列に入らない、利用者の入力の誤りなので、Elythia は 400
// INVALID_PARAM にする (docs/divergence/approximations.md)。
// **外部キーの違反 (23503) は入れない。** 指した行が無いのは値の形の誤りではない。
// 本家の ajv が通す入力に INVALID_PARAM を返すことになるので、corpus
// (internal/server の TestEnforcedEndpointsMatchTheCorpus) にそうした入力が入ったら、
// handlerStricterThanUpstream に足す。
// 利用者の値だけを書く文の直後で使う。サーバーが計算した数や日時を書く文 (カウンタの
// 加算など) の失敗にも 22003 や 22008 は出るので、そこで使うと本物の 500 を隠す
func IsRejectedValue(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "22001", "22003", "22008", "22021", "22P05":
		return true
	}
	return false
}

// IsUniqueViolation reports whether err is a PostgreSQL unique constraint
// violation (SQLSTATE 23505), so callers can tell "someone else inserted the
// same row first" from other failures without importing pgconn.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
