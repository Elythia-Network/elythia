package role

import (
	"time"

	"github.com/elythia-network/elythia/internal/safemath"
)

// PolicyNumber normalizes a numeric policy value into a float.
//
// **policy の数値は int と float64 のどちらも取りうる。** [maxNumber] が
// 「整数値なら int、小数はそのまま float64」で返すと決めており、その doc は
// 「小数を取りうる policy の consumer は float も受けること」と明記している。
// role の policies は jsonb なので、admin が小数を入れれば float64 のまま
// consumer に届く。
//
// 素の `v.(int)` で読むと float64 で型アサーションに失敗する。consumer は
// ほぼ全て `if limit, ok := ...; ok && limit >= 0 { ...gate... }` の形なので、
// **上限違反で弾くのではなく上限そのものが消える** (#2611 / #2613)。
//
// 戻り値を float のまま使うこと。int に丸めると 0.5 が 0 になり、`limit > 0`
// のガードを抜けて結局ゲートが消える。
func PolicyNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// PolicyMinutes converts a minute-valued policy into a Duration.
//
// **`time.Duration(f) * time.Minute` と書かないこと。** Duration への変換で
// 小数が切り捨てられてから掛かるので、0.5 分が 0 になる。掛けてから変換する。
func PolicyMinutes(v any) (time.Duration, bool) {
	f, ok := PolicyNumber(v)
	if !ok {
		return 0, false
	}
	return time.Duration(safemath.MulFloat64(f, int64(time.Minute))), true
}
