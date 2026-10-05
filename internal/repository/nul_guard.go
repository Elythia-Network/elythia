package repository

import "github.com/elythia-network/elythia/internal/misc/colfit"

// storable reports whether a caller-supplied value can match stored data.
//
// **NUL を含む値はどの列にも入らないので、どの行の値とも一致しない。** しかも
// 比較の右辺に置くだけで PostgreSQL が落ちる (手元の simple protocol で SQLSTATE
// 08P01、本番の pgx extended protocol では 22021) ので、そのまま渡すと「そんな
// 行は無い」ではなく**クエリそのものがエラー**になる。`IsNotFound` でもないので
// handler は `JSONInternalError` へ倒し、**認証済みの一般利用者がパラメータ
// 1 文字で 5xx を立てられる** (#3025)。
//
// **不正な UTF-8 も同じ** (SQLSTATE 22021 `invalid byte sequence for encoding
// "UTF8"`)。判定は `colfit.Storable` が両方まとめて持つ。JSON body から来る値は
// Go の decoder が U+FFFD へ矯正するので届かないが、**クエリパラメータと
// パス要素は percent-decode した生のバイト列がそのまま届く**。
//
// **#2792 の「DB 障害を not-found に丸めない」には反しない。** 丸めているのは
// 障害ではなく、**引く前に分かっている「一致しえない」**という事実で、クエリを
// 投げていない以上そこに隠れる障害が無い。逆に言うと、この判定を「引いた後」へ
// 動かすと #2792 違反になる。
//
// 使い分け:
//   - id を 1 件引く経路は `ErrNotFound` を返す (存在しえない id = 見つからない)
//   - 検索語は**空の結果**にする。利用者の入力が壊れているわけではなく、
//     「その語を含む行が無い」が事実として正しい答えなので、新しいエラーコードを
//     wire に足さない。詳細は docs/divergence.md
func storable(s string) bool {
	return colfit.Storable(s)
}

// storableIDs drops the ids that can never match a row.
//
// **要素ごとに落とす。** `IN (...)` や overlap (`&&`) は 1 つでも NUL を含むと
// クエリごと落ちるが、残りの id は普通に引けるので、全体を諦めるのは結果を
// 変えてしまう。全部落ちたときは空になるので、呼び出し側は既存の
// `len(ids) == 0` の早期 return に乗せること (空の `IN ()` を投げない)。
// **「指定はあったが全部落ちた」をフィルタ素通しにしないこと** — 絞ったつもりで
// 全件返る。
//
// **戻り値は入力と配列を共有しうる。** 落とす要素が無いときは入力をそのまま
// 返すので、**戻り値を並べ替えたり書き換えたりしないこと** (呼び出し元の slice を
// 壊す)。現在の呼び出しは `ids = storableIDs(ids)` の後に読むだけ。
func storableIDs(ids []string) []string {
	for i, id := range ids {
		if storable(id) {
			continue
		}
		// **落とす要素が出てから初めて確保する。** 通常のリクエストでは 1 つも
		// 落ちないので、その経路では割り当てを増やさない。
		out := make([]string, i, len(ids)-1)
		copy(out, ids[:i])
		for _, rest := range ids[i+1:] {
			if storable(rest) {
				out = append(out, rest)
			}
		}
		return out
	}
	return ids
}

// allStorable reports whether every value can match stored data.
//
// **AND で畳む集合に使う** (検索語など)。1 つでも一致しえない語があれば結果
// 全体が空になるので、呼び出し側はそのまま空を返してよい。**OR で畳む集合には
// 使わない** — そちらは `storableIDs` のように要素ごとに落とす (落ちた語の枝が
// 偽になるだけで、他の語の一致は残る)。
func allStorable(values []string) bool {
	for _, v := range values {
		if !storable(v) {
			return false
		}
	}
	return true
}
