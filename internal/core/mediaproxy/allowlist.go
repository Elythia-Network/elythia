package mediaproxy

import (
	"context"
	"strings"

	"gorm.io/gorm"
)

// AllowlistChecker determines whether a URL is known in the database.
type AllowlistChecker interface {
	IsAllowedURL(ctx context.Context, url string) (bool, error)
}

// DBAllowlistChecker implements AllowlistChecker by checking URL columns across
// multiple tables. EXISTS + UNION ALL により最初のヒットで短絡する。
type DBAllowlistChecker struct {
	db *gorm.DB
}

// NewDBAllowlistChecker creates an AllowlistChecker backed by the given DB.
func NewDBAllowlistChecker(db *gorm.DB) *DBAllowlistChecker {
	return &DBAllowlistChecker{db: db}
}

// IsAllowedURL returns true if the given URL exists in any of the allowlisted
// columns: user avatar/banner, drive_file url/thumbnail/webpublic/src/uri,
// emoji original/public, instance icon/favicon.
func (c *DBAllowlistChecker) IsAllowedURL(ctx context.Context, url string) (bool, error) {
	var exists bool
	err := c.db.WithContext(ctx).Raw(allowlistSQL, allowlistArgs(url)...).Scan(&exists).Error
	return exists, err
}

// allowlistSQL checks every URL column the media proxy accepts.
//
// **OR でつないだ列は全部 index を持っていなければならない** (#3383)。1 列でも欠けると
// PostgreSQL はそのテーブルで BitmapOr を使えず全件走査に戻り、画像 1 枚ごとに
// 4 テーブル分を読むことになる (本番で 7,383 ページ、約 58MB)。列を足すときは
// index の migration も足す。TestAllowlistQueryUsesIndexes が実行計画で見ている。
const allowlistSQL = `
	SELECT EXISTS (
		SELECT 1 FROM "user" WHERE "avatarUrl" = ? OR "bannerUrl" = ?
		UNION ALL
		SELECT 1 FROM "drive_file" WHERE url = ? OR "thumbnailUrl" = ? OR "webpublicUrl" = ? OR src = ? OR uri = ?
		UNION ALL
		SELECT 1 FROM "emoji" WHERE "originalUrl" = ? OR "publicUrl" = ?
		UNION ALL
		SELECT 1 FROM "instance" WHERE "iconUrl" = ? OR "faviconUrl" = ?
	)`

// allowlistArgs binds url to every placeholder of allowlistSQL.
func allowlistArgs(url string) []any {
	args := make([]any, strings.Count(allowlistSQL, "?"))
	for i := range args {
		args[i] = url
	}
	return args
}
