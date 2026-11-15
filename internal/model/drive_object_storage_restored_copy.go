package model

import "time"

// DriveObjectStorageRestoredCopy records a copy left in object storage by
// moving a migrated drive file back to the local storage (#4197). It has no
// foreign key to drive_file: the copy stays in the bucket after the file is
// deleted. Elythia-only.
type DriveObjectStorageRestoredCopy struct {
	// Key is the access key of the copy (the file, its thumbnail or its
	// webpublic variant).
	Key    string `gorm:"column:key;type:varchar(256);primaryKey"`
	FileID string `gorm:"column:fileId;type:varchar(32);not null"`
	// Location is where the copy was when the file was moved back: the
	// bucket and the object key with the prefix.
	Location   string    `gorm:"column:location;type:text;not null"`
	RestoredAt time.Time `gorm:"column:restoredAt;not null"`
	// Error and FailedAt record the last failure to delete the copy.
	Error    *string    `gorm:"column:error;type:text"`
	FailedAt *time.Time `gorm:"column:failedAt"`
}

// TableName returns the table of DriveObjectStorageRestoredCopy.
func (DriveObjectStorageRestoredCopy) TableName() string {
	return "drive_object_storage_restored_copy"
}
