package model

import "time"

// Announcement represents the `announcement` table.
type Announcement struct {
	ID                     string     `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	UpdatedAt              *time.Time `gorm:"column:updatedAt" json:"updatedAt"`
	Title                  string     `gorm:"column:title;type:varchar(256);not null" json:"title"`
	Text                   string     `gorm:"column:text;type:varchar(8192);not null" json:"text"`
	ImageURL               *string    `gorm:"column:imageUrl;type:varchar(1024)" json:"imageUrl"`
	Icon                   string     `gorm:"column:icon;type:varchar(256);not null;default:'info'" json:"icon"`
	Display                string     `gorm:"column:display;type:varchar(256);not null;default:'normal'" json:"display"`
	NeedConfirmationToRead bool       `gorm:"column:needConfirmationToRead;default:false" json:"needConfirmationToRead"`
	IsActive               bool       `gorm:"column:isActive;default:true" json:"isActive"`
	ForExistingUsers       bool       `gorm:"column:forExistingUsers;default:false" json:"forExistingUsers"`
	Silence                bool       `gorm:"column:silence;default:false" json:"silence"`
	UserID                 *string    `gorm:"column:userId;type:varchar(32)" json:"userId"`
	// AutoArchiveAt is the time after which the announcement is treated as
	// archived (upstream 2026.10.1, #3809). nil means it is never archived
	// automatically.
	AutoArchiveAt *time.Time `gorm:"column:autoArchiveAt" json:"autoArchiveAt"`
}

func (Announcement) TableName() string { return "announcement" }

// AnnouncementRead represents the `announcement_read` table.
type AnnouncementRead struct {
	ID             string `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	UserID         string `gorm:"column:userId;type:varchar(32);not null" json:"userId"`
	AnnouncementID string `gorm:"column:announcementId;type:varchar(32);not null" json:"announcementId"`
}

func (AnnouncementRead) TableName() string { return "announcement_read" }
