package model

import (
	"time"

	"gorm.io/datatypes"
)

// DriveFile represents the `drive_file` table.
type DriveFile struct {
	ID                 string         `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	UserID             *string        `gorm:"column:userId;type:varchar(32)" json:"userId"`
	UserHost           *string        `gorm:"column:userHost;type:varchar(128)" json:"userHost"`
	MD5                string         `gorm:"column:md5;type:varchar(32);not null" json:"md5"`
	Name               string         `gorm:"column:name;type:varchar(256);not null" json:"name"`
	Type               string         `gorm:"column:type;type:varchar(128);not null" json:"type"`
	Size               int            `gorm:"column:size;not null" json:"size"`
	Comment            *string        `gorm:"column:comment;type:varchar(512)" json:"comment"`
	Blurhash           *string        `gorm:"column:blurhash;type:varchar(128)" json:"blurhash"`
	Properties         datatypes.JSON `gorm:"column:properties;type:jsonb;default:'{}'" json:"properties"`
	StoredInternal     bool           `gorm:"column:storedInternal;not null" json:"storedInternal"`
	URL                string         `gorm:"column:url;type:varchar(1024);not null" json:"url"`
	ThumbnailURL       *string        `gorm:"column:thumbnailUrl;type:varchar(512)" json:"thumbnailUrl"`
	WebpublicURL       *string        `gorm:"column:webpublicUrl;type:varchar(512)" json:"webpublicUrl"`
	WebpublicType      *string        `gorm:"column:webpublicType;type:varchar(128)" json:"webpublicType"`
	AccessKey          *string        `gorm:"column:accessKey;type:varchar(256);uniqueIndex" json:"accessKey"`
	ThumbnailAccessKey *string        `gorm:"column:thumbnailAccessKey;type:varchar(256);uniqueIndex" json:"thumbnailAccessKey"`
	WebpublicAccessKey *string        `gorm:"column:webpublicAccessKey;type:varchar(256);uniqueIndex" json:"webpublicAccessKey"`
	URI                *string        `gorm:"column:uri;type:varchar(1024)" json:"uri"`
	Src                *string        `gorm:"column:src;type:varchar(1024)" json:"src"`
	FolderID           *string        `gorm:"column:folderId;type:varchar(32)" json:"folderId"`
	IsSensitive        bool           `gorm:"column:isSensitive;default:false" json:"isSensitive"`
	MaybeSensitive     bool           `gorm:"column:maybeSensitive;default:false" json:"maybeSensitive"`
	MaybePorn          bool           `gorm:"column:maybePorn;default:false" json:"maybePorn"`
	IsLink             bool           `gorm:"column:isLink;default:false" json:"isLink"`
	RequestHeaders     datatypes.JSON `gorm:"column:requestHeaders;type:jsonb;default:'{}'" json:"requestHeaders"`
	RequestIP          *string        `gorm:"column:requestIp;type:varchar(128)" json:"requestIp"`
	// ObjectStorageMigratedAt is when the local-to-object-storage migration
	// copied the file (#4180). Nil for files that were never migrated,
	// including those uploaded to object storage directly. Elythia-only.
	ObjectStorageMigratedAt *time.Time `gorm:"column:objectStorageMigratedAt" json:"-"`
	// ObjectStorageVerifiedAt is when the verification found the migrated
	// file readable in object storage with the recorded size and md5 (#4181).
	// Nil until verified, and again after a failed verification or a new
	// migration. Elythia-only.
	ObjectStorageVerifiedAt *time.Time `gorm:"column:objectStorageVerifiedAt" json:"-"`
	// ObjectStorageLocalDeletedAt is when the purge deleted the local copy of
	// the migrated file after verifying it once more (#4182). It is set
	// before the deletion, so a set value means the local copy is gone or was
	// left behind; nil does not mean the local copy is there. Elythia-only.
	ObjectStorageLocalDeletedAt *time.Time `gorm:"column:objectStorageLocalDeletedAt" json:"-"`

	// Relations
	User   *User        `gorm:"foreignKey:UserID" json:"user,omitempty"`
	Folder *DriveFolder `gorm:"foreignKey:FolderID" json:"folder,omitempty"`
}

func (DriveFile) TableName() string { return "drive_file" }

// DriveFolder represents the `drive_folder` table.
type DriveFolder struct {
	ID       string  `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	Name     string  `gorm:"column:name;type:varchar(128);not null" json:"name"`
	UserID   *string `gorm:"column:userId;type:varchar(32)" json:"userId"`
	ParentID *string `gorm:"column:parentId;type:varchar(32)" json:"parentId"`

	// Relations
	User   *User        `gorm:"foreignKey:UserID" json:"user,omitempty"`
	Parent *DriveFolder `gorm:"foreignKey:ParentID" json:"parent,omitempty"`
}

func (DriveFolder) TableName() string { return "drive_folder" }
