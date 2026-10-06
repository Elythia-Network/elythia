// Package colfit holds the primitives for fitting a remote-supplied string into
// a PostgreSQL varchar column.
//
// 規則そのもの (本文は切る / URL は値ごと捨てる / 身元は document ごと拒否) は
// docs/divergence/security.md の「リモート由来の文字列を列に入れるときの規則」にあり、
// ここはその**数え方**だけを持つ。以前は federation と instance に同型の
// ヘルパーが散っていて、rune / byte の数え方と NUL の扱いが分かれる土壌に
// なっていた (#2726。**内訳は docs/divergence/security.md に一本化してある** — 数を
// 2 箇所に書くと片方だけ古くなる)。
//
// 呼び出し側は「その列固有の判断とログ」を持つ薄い名前付きヘルパーを維持し、
// 中身だけここへ委譲する。
package colfit

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Fits reports whether s can be stored in a varchar(max) column.
//
// **rune で数える。** PostgreSQL の varchar はコードポイント数で数えるので、
// byte 長で見ると非 ASCII を含む値を必要以上に落とす。NUL と不正な UTF-8 は
// 長さに関わらず SQLSTATE 22021 で弾かれるので `Storable` で別に見る。
func Fits(s string, max int) bool {
	return Storable(s) && len([]rune(s)) <= max
}

// Storable reports whether s can be stored in a PostgreSQL text column **at
// all**, ignoring how wide the column is.
//
// 幅を知らない場所でも同じ判定を使えるようにするための分離 (#3025)。カーソルや
// id のように「列に入るかは分かっているが幅は関係ない」値は、`Fits` に適当な
// max を渡すと**比較できるだけの値まで落とす**ことになる (`id < ?` は長さに
// 関わらず成立する)。
//
// **NUL を含む値は SELECT の bind parameter にも載せられない。** 手元の simple
// protocol では SQLSTATE 08P01 (protocol_violation)、本番の pgx extended
// protocol では 22021 (character_not_in_repertoire) になる (#2726 / #3025)。
//
// **不正な UTF-8 も同じく落ちる。** UTF8 エンコーディングのデータベースは
// 不正なバイト列を受け付けず、比較の右辺に置くだけで SQLSTATE 22021
// (`invalid byte sequence for encoding "UTF8"`) でクエリごと落とす。実測した
// 3 形 — 孤立した継続バイト (`0x80`)、UTF-8 で符号化した孤立サロゲート
// (`0xED 0xA0 0x80`)、`0xFF` — がいずれも同じ SQLSTATE を返す。
//
// **NUL だけを見ていたので素通りしていた。** 両方を見ていたのは
// `internal/core/mediaproxy` の 1 箇所だけで、`id.NormalizeCursor` も
// `internal/repository` の guard 群もここを通るため、`%80` を 1 つ混ぜるだけで
// 同じ 500 を起こせた。**JSON body からは来ない** (Go の decoder が不正な
// UTF-8 も孤立サロゲートのエスケープも U+FFFD に置き換える) が、クエリ
// パラメータとパス要素は percent-decode した生のバイト列がそのまま届く。
func Storable(s string) bool {
	return !strings.ContainsRune(s, 0) && utf8.ValidString(s)
}

// ToStorable coerces s into a value a PostgreSQL text column accepts: NUL を
// 落とし、不正な UTF-8 を U+FFFD へ置き換える。
//
// JSON の NUL エスケープは正当な入力で、Go の decoder は実 NUL バイトを作る。
// PostgreSQL の text 系列はこれを受け付けず (SQLSTATE 22021)、jsonb も拒否する
// (22P05)。同じ書き込みに乗っている他の列まで巻き添えになるので落とす。
//
// **UTF-8 の矯正を先に行うのは、畳み方を揃えるため。** 順序を入れ替えても
// 「不正なバイト列が残る」ことはない — NUL (0x00) は多バイト列の内部に現れない
// ので除去で列を割れないし、`strings.Map` は range で decode するので不正バイトを
// 自分で U+FFFD に置き換える。**違うのは連続した不正バイトの畳み方**で、
// `ToValidUTF8` は連続を 1 個の U+FFFD にまとめ、`strings.Map` は 1 バイトずつ
// U+FFFD にする (孤立サロゲート 3 バイトが 1 個になるか 3 個になるか)。
// 実測で確かめたうえで前者を採っている。
//
// 置き換えであって削除ではないのは、**`Text` が rune で数えて切る**ため —
// 黙って詰めると「何文字目で切れたか」が入力と対応しなくなる。
//
// 値ごと捨てる側の判定は `Storable` / `Fits`。どちらを使うかは
// docs/divergence/security.md の「リモート由来の文字列を列に入れるときの規則」にある
// (本文は切る / URL は値ごと捨てる)。
func ToStorable(s string) string {
	if Storable(s) {
		return s
	}
	valid := strings.ToValidUTF8(s, "\uFFFD")
	return strings.Map(func(r rune) rune {
		if r == 0 {
			return -1
		}
		return r
	}, valid)
}

// TruncateRunes clips s to at most max runes.
//
// byte 単位で切ると壊れた UTF-8 を書くので rune で数える。max <= 0 なら切らない。
func TruncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	if runes := []rune(s); len(runes) > max {
		return string(runes[:max])
	}
	return s
}

// Text prepares a remote-supplied **body** string for a column: NUL を落として
// 不正な UTF-8 を矯正し、max rune で切る。max <= 0 なら切らない (無制限の
// text 列用)。
//
// **URL / ID には使わない。** 途中で切った URL は別物で、取りに行っても無駄な
// うえ壊れた参照を保存することになる。そちらは Fits で判定して値ごと捨てる。
func Text(raw string, max int) string {
	return TruncateRunes(ToStorable(raw), max)
}

// JSONStorable reports whether a JSON document can be stored in a `jsonb`
// column.
//
// **判定するのは decode 前の生バイト列。** ガードの対象はどれも
// `json.RawMessage` (あるいはそれを marshal した []byte) で、**列へ入るのは
// ほどく前のバイトそのもの**。Go の decoder は RawMessage の中身を一切正規化
// しないので、「ほどいてから文字列を歩く」実装だと素通りする形がある
// (1 稿目がそれで、敵対的レビューで 3 形を実測された)。
//
// PostgreSQL が jsonb を拒む形は 4 つあり、**全部ここで見る** (実 PostgreSQL で
// 境界まで実測):
//
//   - 不正な UTF-8 のバイト列 → SQLSTATE 22021
//   - `\u0000` エスケープ → 22P05
//   - 対になっていないサロゲートのエスケープ → 22P02
//   - numeric の範囲外 (整数部 131072 桁超 / 小数部 16383 桁超) → 22003
//
// **構文エラーは通す。** ここは「列に入るか」だけを見る述語で、パースの
// 可否は呼び出し側の既存の扱いに任せる (`json.RawMessage` は親の Unmarshal が
// 構文を検査済み)。
func JSONStorable(raw []byte) bool {
	if len(raw) == 0 {
		return true
	}
	// **生の NUL バイトは JSON として不正**なので親の Unmarshal が先に落とす
	// はずだが、`[]byte` を直に渡す呼び出しもあるので見ておく。
	if bytes.IndexByte(raw, 0) >= 0 {
		return false
	}
	if !utf8.Valid(raw) {
		return false
	}
	if !jsonEscapesStorable(raw) {
		return false
	}
	return jsonNumbersStorable(raw)
}

// jsonEscapesStorable scans the raw document for `\u` escapes jsonb rejects.
//
// **自前で走査するしかない。** Go の decoder は `\u0000` を実 NUL に、対に
// なっていないサロゲートを U+FFFD に**置き換えて**しまうので、ほどいた後の
// 値からは元の形が分からない。`json.Valid` も両方を通す。
//
// **壊れた JSON は通す** (`JSONStorable` の契約どおり)。読めない位置に来たら
// そこで打ち切って true を返す。
func jsonEscapesStorable(raw []byte) bool {
	inString := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			if i+1 >= len(raw) {
				return true
			}
			if raw[i+1] != 'u' {
				// `\"` `\\` `\/` `\b` `\f` `\n` `\r` `\t` はまとめて読み飛ばす。
				// **`\\` を飛ばすのが要点** — 飛ばさないと次の `u` を
				// エスケープの開始と誤読する (`"\\u0000"` はただの 6 文字)。
				i++
				continue
			}
			if i+6 > len(raw) {
				return true
			}
			v, ok := parseHex4(raw[i+2 : i+6])
			if !ok {
				return true
			}
			if v == 0 {
				return false // 22P05
			}
			if v >= 0xD800 && v <= 0xDBFF {
				// 上位サロゲートは直後に下位サロゲートのエスケープが要る。
				if i+12 > len(raw) || raw[i+6] != '\\' || raw[i+7] != 'u' {
					return false // 22P02
				}
				lo, ok2 := parseHex4(raw[i+8 : i+12])
				if !ok2 || lo < 0xDC00 || lo > 0xDFFF {
					return false // 22P02
				}
				i += 11
				continue
			}
			if v >= 0xDC00 && v <= 0xDFFF {
				return false // 単独の下位サロゲート。22P02
			}
			i += 5
		}
	}
	return true
}

// parseHex4 reads exactly four hex digits.
func parseHex4(b []byte) (int, bool) {
	if len(b) != 4 {
		return 0, false
	}
	v := 0
	for _, c := range b {
		switch {
		case c >= '0' && c <= '9':
			v = v<<4 | int(c-'0')
		case c >= 'a' && c <= 'f':
			v = v<<4 | int(c-'a'+10)
		case c >= 'A' && c <= 'F':
			v = v<<4 | int(c-'A'+10)
		default:
			return 0, false
		}
	}
	return v, true
}

// PostgreSQL numeric の桁数上限 (実 PostgreSQL で境界まで実測)。
//
//	1e131071 は通り 1e131072 は 22003 / 9 を 131072 個は通り 131073 個は 22003
//	1e-16383 は通り 1e-16384 は 22003 / 0.9...(16383 桁) は通り 16384 桁は 22003
const (
	pgNumericMaxWeight = 131072 // 整数部の桁数
	pgNumericMaxScale  = 16383  // 小数部の桁数
)

// jsonNumbersStorable reports whether every number in the document fits
// PostgreSQL's numeric.
//
// **数値はエスケープの影響を受けない**ので、こちらは decoder のトークンで歩く。
// `UseNumber` で元の字面を保つのが要点 — float64 に落とすと桁が失われて
// 判定できない。
func jsonNumbersStorable(raw []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if err != nil {
			// io.EOF も構文エラーもここへ来る。どちらも通す。
			return true
		}
		if n, ok := tok.(json.Number); ok && !numberFitsNumeric(n.String()) {
			return false
		}
	}
}

// numberFitsNumeric reports whether a JSON number literal fits numeric.
func numberFitsNumeric(s string) bool {
	s = strings.TrimPrefix(s, "-")
	mant, expPart, hasExp := strings.Cut(s, "e")
	if !hasExp {
		mant, expPart, hasExp = strings.Cut(s, "E")
	}
	intPart, fracPart, _ := strings.Cut(mant, ".")
	// 先頭 0 は桁数に数えない (`0.5` の整数部は 0 桁)。
	intPart = strings.TrimLeft(intPart, "0")
	exp := 0
	if hasExp {
		v, err := strconv.Atoi(strings.TrimPrefix(expPart, "+"))
		if err != nil {
			// int に入らない指数 = 確実に範囲外。
			return false
		}
		exp = v
	}
	if len(intPart)+exp > pgNumericMaxWeight {
		return false
	}
	return len(fracPart)-exp <= pgNumericMaxScale
}
