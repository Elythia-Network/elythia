package id

import (
	"time"

	"github.com/elythia-network/elythia/internal/misc/colfit"
)

// NormalizeCursor converts upstream Misskey pagination params (sinceId /
// untilId / sinceDate / untilDate) into the (sinceID, untilID) shape that
// mk-go repository layer consumes (#1166)。
//
// upstream `QueryService.makePaginationQuery` は 4 つの param を全て受けて
// SQL の `id > ?` / `id < ?` cursor に展開するが、mk-go の repository 層は
// 歴史的に `sinceID` / `untilID` 文字列だけを受ける shape で、`sinceDate` /
// `untilDate` (Unix ms) は handler 側で bind されても repository に渡らず
// silent ignore されていた。
//
// 本 helper は handler から呼ばれて: sinceID 空 + sinceDate 非 nil の場合に
// `AidxCutoffPrefix(time.UnixMilli(*sinceDate))` で aidx ID 形式 prefix に
// 変換し、repository 層には従来通り (sinceID, untilID) を渡す形を維持する。
// repository 側の signature 変更を全面的に避けるための adapter 層として
// 機能する。
//
// drop-in 仕様:
//   - sinceID が指定されていれば優先、sinceDate は無視 (= upstream 同様)
//   - sinceDate のみなら ms → aidx prefix に変換して sinceID とする
//   - untilID / untilDate も同パターン
//
// mk-go の id 生成の既定は aidx (CLAUDE.md Section 6 / `MK_ID=aidx`) なので、
// aidx 以外の generator (aid / meid / objectid / ulid) で動かしている operator
// では本 helper は意図通り機能しない。それらの generator は drop-in 主流路で
// 使われない既知の制限。
//
// **ok=false は列に入らないカーソルで、呼び出し側は 400 を返すこと (#3025)。**
// `pagination.ResolveLimit` と同じ形。NUL を含むカーソルは `id < ?` の bind
// parameter に載せた時点で PostgreSQL が落とすので (本番の pgx extended
// protocol で SQLSTATE 22021)、そのまま渡すと**認証済みの一般利用者が
// パラメータ 1 文字で 500 を起こせる**。
//
// **空に倒して 200 を返してはいけない。** 空文字は「カーソル無し = 先頭から」の
// 意味なので、利用者が指定した位置と無関係なページを正しい応答として返して
// しまう。ok=false のときに返す `""` は「無視しても DB に NUL を渡さない」ための
// 値で、**ページを返してよいという意味ではない**。
func NormalizeCursor(sinceID, untilID string, sinceDate, untilDate *int64) (string, string, bool) {
	if sinceID == "" && sinceDate != nil {
		sinceID = AidxCutoffPrefix(time.UnixMilli(*sinceDate))
	}
	if untilID == "" && untilDate != nil {
		untilID = AidxCutoffPrefix(time.UnixMilli(*untilDate))
	}
	if !colfit.Storable(sinceID) || !colfit.Storable(untilID) {
		return "", "", false
	}
	return sinceID, untilID, true
}
