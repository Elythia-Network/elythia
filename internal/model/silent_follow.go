package model

// SilentFollow represents the `silent_follow` table: a follow that succeeded
// without a notification because of the followee's followApprovalAction
// "silentFollow" (#3466, Elythia-only). The ID is regenerated each time the
// same follower follows again, so it tells when the latest one happened.
type SilentFollow struct {
	ID         string `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	FollowerID string `gorm:"column:followerId;type:varchar(32);not null" json:"followerId"`
	FolloweeID string `gorm:"column:followeeId;type:varchar(32);not null" json:"followeeId"`
}

// TableName returns the table name.
func (SilentFollow) TableName() string { return "silent_follow" }
