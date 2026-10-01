package model

// Following represents the `following` table.
type Following struct {
	ID                   string  `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	FolloweeID           string  `gorm:"column:followeeId;type:varchar(32);not null" json:"followeeId"`
	FollowerID           string  `gorm:"column:followerId;type:varchar(32);not null" json:"followerId"`
	IsFollowerHibernated bool    `gorm:"column:isFollowerHibernated;default:false" json:"isFollowerHibernated"`
	WithReplies          bool    `gorm:"column:withReplies;default:false" json:"withReplies"`
	Notify               *string `gorm:"column:notify;type:varchar(32)" json:"notify"`
	// Denormalized fields
	FollowerHost        *string `gorm:"column:followerHost;type:varchar(128)" json:"followerHost"`
	FollowerInbox       *string `gorm:"column:followerInbox;type:varchar(512)" json:"followerInbox"`
	FollowerSharedInbox *string `gorm:"column:followerSharedInbox;type:varchar(512)" json:"followerSharedInbox"`
	FolloweeHost        *string `gorm:"column:followeeHost;type:varchar(128)" json:"followeeHost"`
	FolloweeInbox       *string `gorm:"column:followeeInbox;type:varchar(512)" json:"followeeInbox"`
	FolloweeSharedInbox *string `gorm:"column:followeeSharedInbox;type:varchar(512)" json:"followeeSharedInbox"`

	// Relations
	Followee *User `gorm:"foreignKey:FolloweeID" json:"followee,omitempty"`
	Follower *User `gorm:"foreignKey:FollowerID" json:"follower,omitempty"`
}

func (Following) TableName() string { return "following" }

// FollowListViewer identifies who is reading a per-host follow relation
// listing (federation/followers, federation/following), for the list owner's
// visibility filter.
type FollowListViewer struct {
	// UserID is the viewer's user ID, or "" for an anonymous viewer.
	UserID string
	// Moderator skips the owner visibility filter.
	Moderator bool
}
