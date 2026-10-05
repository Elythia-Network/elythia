package admin

import (
	"errors"

	"github.com/elythia-network/elythia/internal/misc/id"
)

// ErrIDGenMissing is returned when aidxCreatedAtString is called with a nil
// generator. Distinguishing this from a parse error lets callers decide
// whether to log (parse failure on a non-aidx ID is debug-worthy, missing
// generator usually means the handler was wired without an idGen and the
// log noise would be redundant).
var ErrIDGenMissing = errors.New("admin: idGen not configured")

// aidxCreatedAtString derives a Misskey-format ISO timestamp string from an
// aidx ID. mk-go の幾つかの table (user, registration_ticket, moderation_log
// 等) は created_at カラムを持たず aidx ID 先頭 8 文字に ms timestamp を
// 埋め込んでいる。frontend の `<MkTime :time="..."/>` は Date.parse できる
// ISO 文字列を要求するため "2006-01-02T15:04:05.000Z" 形式 (Misskey 標準)
// に揃えて返す。
func aidxCreatedAtString(idGen id.Generator, aidx string) (string, error) {
	if idGen == nil {
		return "", ErrIDGenMissing
	}
	t, err := idGen.ParseTime(aidx)
	if err != nil {
		return "", err
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z"), nil
}
