package model

import "time"

// DriveObjectStorageMigrationFailure records a drive file the
// local-to-object-storage migration could not process (#4180). Elythia-only.
type DriveObjectStorageMigrationFailure struct {
	FileID    string    `gorm:"column:fileId;type:varchar(32);primaryKey"`
	Operation string    `gorm:"column:operation;type:varchar(16);primaryKey"`
	Error     string    `gorm:"column:error;type:text;not null"`
	FailedAt  time.Time `gorm:"column:failedAt;not null"`
}

// TableName returns the table of DriveObjectStorageMigrationFailure.
func (DriveObjectStorageMigrationFailure) TableName() string {
	return "drive_object_storage_migration_failure"
}
