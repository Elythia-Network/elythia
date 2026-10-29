// Package pagination provides shared helpers for Misskey-compatible list
// pagination params (limit / sinceId / untilId).
package pagination

// ResolveLimit validates a requested page-size limit against an endpoint's
// bounds and returns the effective value.
//
// limit == nil はキー省略で、def (upstream paramDef の `default`) を返す。
// 値がある場合は 1..max の範囲を要求し、外れていれば ok=false を返す。
// 呼び出し側はその場合 INVALID_PARAM (400) を返すこと。
//
// def / max は upstream の paramDef (`default` / `maximum`) と一致させる。
// 各呼び出し箇所のリテラルは limit-spec drift gate
// (entitycompat.TestLimitSpecDrift) が upstream の paramDef の golden
// (internal/api/paramschema/upstream_paramdefs.json) と突き合わせて検証する。
//
// 以前は範囲外を黙って丸めていた (ClampLimit) が、upstream は ajv で 400 に
// するため、`limit: 0` や `limit: 101` を送ったクライアントが「丸められた件数」
// を正しい応答と誤認できた。
func ResolveLimit(limit *int, def, max int) (int, bool) {
	if limit == nil {
		return def, true
	}
	if *limit < 1 || *limit > max {
		return 0, false
	}
	return *limit, true
}
