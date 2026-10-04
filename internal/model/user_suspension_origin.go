package model

import "time"

// SuspensionOrigin values recorded in user_suspension_origin.
const (
	// SuspensionOriginLocal marks a decision made by our own moderator.
	SuspensionOriginLocal = "local"
	// SuspensionOriginRemote marks a decision taken from the actor's
	// `toot:suspended` (#2951).
	SuspensionOriginRemote = "remote"
)

// UserSuspensionOrigin records who decided the current suspension state (#2973).
//
// **`user` に列を足さず別テーブルにしてある** (`relay_observed_user` /
// `signup_application` と同じ判断。理由は RelayObservedUser を参照)。
type UserSuspensionOrigin struct {
	UserID    string    `gorm:"column:userId;type:varchar(32);primaryKey" json:"userId"`
	Origin    string    `gorm:"column:origin;type:varchar(16);not null" json:"origin"`
	UpdatedAt time.Time `gorm:"column:updatedAt" json:"updatedAt"`
}

// TableName returns the table name.
func (UserSuspensionOrigin) TableName() string { return "user_suspension_origin" }
