package model

import "github.com/elythia-network/elythia/internal/pgarray"

// PostgreSQL の配列カラム (`text[]` / `varchar[]` / `int[]`) を扱う型。
//
// 実体は internal/pgarray (#2628 で lib/pq から取り込んだもの)。**型エイリアス
// にしてあるのは、model 側に実行可能コードを持ち込まないため。** ここに実装を
// 置くと `internal/model` に初のテストファイルが必要になり、CI のカバレッジ
// 閾値がパッケージ全体 (model 定義が大半で分母が大きい) に掛かってしまう
// (#462 で internal/server が踏んだのと同じ形)。
//
// エイリアスなので `model.StringArray` と `pgarray.StringArray` は同一型として
// 扱える。GORM は Scanner / Valuer を型に対して探すので、別名でも従来どおり動く。
type (
	StringArray = pgarray.StringArray
	Int64Array  = pgarray.Int64Array
)
