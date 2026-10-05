package model

import (
	"time"
)

// Emoji represents the `emoji` table.
type Emoji struct {
	ID                                      string      `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	UpdatedAt                               *time.Time  `gorm:"column:updatedAt;type:timestamp with time zone" json:"updatedAt"`
	Name                                    string      `gorm:"column:name;type:varchar(128);not null" json:"name"`
	Host                                    *string     `gorm:"column:host;type:varchar(128)" json:"host"`
	Category                                *string     `gorm:"column:category;type:varchar(128)" json:"category"`
	OriginalURL                             string      `gorm:"column:originalUrl;type:varchar(512);not null" json:"originalUrl"`
	PublicURL                               string      `gorm:"column:publicUrl;type:varchar(512);default:''" json:"publicUrl"`
	URI                                     *string     `gorm:"column:uri;type:varchar(512)" json:"uri"`
	Type                                    *string     `gorm:"column:type;type:varchar(64)" json:"type"`
	Aliases                                 StringArray `gorm:"column:aliases;type:varchar(128)[];default:'{}'" json:"aliases"`
	License                                 *string     `gorm:"column:license;type:varchar(1024)" json:"license"`
	LocalOnly                               bool        `gorm:"column:localOnly;default:false" json:"localOnly"`
	IsSensitive                             bool        `gorm:"column:isSensitive;default:false" json:"isSensitive"`
	RoleIDsThatCanBeUsedThisEmojiAsReaction StringArray `gorm:"column:roleIdsThatCanBeUsedThisEmojiAsReaction;type:varchar(128)[];default:'{}'" json:"roleIdsThatCanBeUsedThisEmojiAsReaction"`
}

func (Emoji) TableName() string { return "emoji" }

// EmojiKey identifies a custom emoji by its (name, host) pair. Host is ""
// for local emojis (host IS NULL in the emoji table).
type EmojiKey struct {
	Name string `json:"name"`
	Host string `json:"host"`
}

// EmojiKeyOf returns the EmojiKey of e.
func EmojiKeyOf(e *Emoji) EmojiKey {
	if e.Host == nil {
		return EmojiKey{Name: e.Name}
	}
	return EmojiKey{Name: e.Name, Host: *e.Host}
}

// EmojiV2Filter holds parameters for v2 admin emoji listing.
type EmojiV2Filter struct {
	Query    *EmojiV2Query
	SinceID  string
	UntilID  string
	Limit    int // default 10, max 100
	Page     int // 1-indexed; page > 0の場合はoffsetベース
	SortKeys []string
}

// EmojiV2Query holds query parameters for v2 admin emoji filtering.
type EmojiV2Query struct {
	Name          string
	Host          string
	HostType      string // "local" | "remote" | "all"
	Category      string
	Type          string
	Aliases       string
	License       string
	IsSensitive   *bool
	LocalOnly     *bool
	UpdatedAtFrom string
	UpdatedAtTo   string
	RoleIDs       []string
	// URI / PublicURL / OriginalURL は admin v2 emoji 検索の追加 filter
	// (#466)。frontend が remote emoji 一覧の検索ボックスから渡してくる
	// ので、空文字列でなければ ILIKE 部分一致で WHERE に展開する。
	URI         string
	PublicURL   string
	OriginalURL string
}
