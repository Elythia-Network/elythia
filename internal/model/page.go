package model

import (
	"time"

	"gorm.io/datatypes"
)

// PageVisibility represents the visibility level of a Page.
//
// 本家の enum は public / followers / specified だが、本家は常に public を書き、
// 公開範囲を選ぶ手段を持たない。Elythia は派生版の一つに揃えて public / private
// の 2 値にした (#3479、migration 000121)。private は作者だけが見られる。
type PageVisibility string

const (
	PageVisibilityPublic  PageVisibility = "public"
	PageVisibilityPrivate PageVisibility = "private"
)

// Page represents the `page` table. Misskey 互換のローカル限定機能で、
// content / variables / script はクライアント側 (AI Script + ブロックレンダラ)
// で解釈される。サーバは入力をそのまま保存・配信する。
type Page struct {
	ID                  string         `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	UpdatedAt           time.Time      `gorm:"column:updatedAt;type:timestamp with time zone;not null" json:"updatedAt"`
	Title               string         `gorm:"column:title;type:varchar(256);not null" json:"title"`
	Name                string         `gorm:"column:name;type:varchar(256);not null" json:"name"`
	Summary             *string        `gorm:"column:summary;type:varchar(256)" json:"summary"`
	AlignCenter         bool           `gorm:"column:alignCenter;default:false" json:"alignCenter"`
	HideTitleWhenPinned bool           `gorm:"column:hideTitleWhenPinned;default:false" json:"hideTitleWhenPinned"`
	Font                string         `gorm:"column:font;type:varchar(32);default:'sans-serif'" json:"font"`
	UserID              string         `gorm:"column:userId;type:varchar(32);not null" json:"userId"`
	EyeCatchingImageID  *string        `gorm:"column:eyeCatchingImageId;type:varchar(32)" json:"eyeCatchingImageId"`
	Content             datatypes.JSON `gorm:"column:content;type:jsonb;default:'[]'" json:"content"`
	Variables           datatypes.JSON `gorm:"column:variables;type:jsonb;default:'[]'" json:"variables"`
	Script              string         `gorm:"column:script;type:varchar(16384);default:''" json:"script"`
	Visibility          PageVisibility `gorm:"column:visibility;type:page_visibility_enum;default:'public'" json:"visibility"`
	VisibleUserIDs      StringArray    `gorm:"column:visibleUserIds;type:varchar(32)[];default:'{}'" json:"visibleUserIds"`
	LikedCount          int            `gorm:"column:likedCount;default:0" json:"likedCount"`
}

func (Page) TableName() string { return "page" }

// PageLike represents the `page_like` table.
type PageLike struct {
	ID     string `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	UserID string `gorm:"column:userId;type:varchar(32);not null" json:"userId"`
	PageID string `gorm:"column:pageId;type:varchar(32);not null" json:"pageId"`
}

func (PageLike) TableName() string { return "page_like" }
