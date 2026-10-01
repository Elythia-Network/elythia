package model

import (
	"time"

	"gorm.io/datatypes"
)

// BubbleGameVersusRecord represents the `bubble_game_versus_record` table: one
// finished (or still running) 1:1 bubble game match (#3232). User1 is the
// inviter. Per-player columns are NULL until that player's report arrives.
type BubbleGameVersusRecord struct {
	ID        string     `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	User1ID   string     `gorm:"column:user1Id;type:varchar(32);not null" json:"user1Id"`
	User2ID   string     `gorm:"column:user2Id;type:varchar(32);not null" json:"user2Id"`
	GameMode  string     `gorm:"column:gameMode;type:varchar(128);not null" json:"gameMode"`
	Seed      string     `gorm:"column:seed;type:varchar(64);not null" json:"seed"`
	StartedAt time.Time  `gorm:"column:startedAt;type:timestamp with time zone;not null" json:"startedAt"`
	EndedAt   *time.Time `gorm:"column:endedAt;type:timestamp with time zone" json:"endedAt"`
	WinnerID  *string    `gorm:"column:winnerId;type:varchar(32)" json:"winnerId"`
	Reason    *string    `gorm:"column:reason;type:varchar(32)" json:"reason"`

	User1Score       *int           `gorm:"column:user1Score" json:"user1Score"`
	User1Frame       *int           `gorm:"column:user1Frame" json:"user1Frame"`
	User1Reason      *string        `gorm:"column:user1Reason;type:varchar(32)" json:"user1Reason"`
	User1GameVersion *int           `gorm:"column:user1GameVersion" json:"user1GameVersion"`
	User1Logs        datatypes.JSON `gorm:"column:user1Logs;type:jsonb" json:"user1Logs"`
	User1Public      bool           `gorm:"column:user1Public;not null;default:false" json:"user1Public"`

	User2Score       *int           `gorm:"column:user2Score" json:"user2Score"`
	User2Frame       *int           `gorm:"column:user2Frame" json:"user2Frame"`
	User2Reason      *string        `gorm:"column:user2Reason;type:varchar(32)" json:"user2Reason"`
	User2GameVersion *int           `gorm:"column:user2GameVersion" json:"user2GameVersion"`
	User2Logs        datatypes.JSON `gorm:"column:user2Logs;type:jsonb" json:"user2Logs"`
	User2Public      bool           `gorm:"column:user2Public;not null;default:false" json:"user2Public"`

	User1 *User `gorm:"foreignKey:User1ID" json:"user1,omitempty"`
	User2 *User `gorm:"foreignKey:User2ID" json:"user2,omitempty"`
}

func (BubbleGameVersusRecord) TableName() string { return "bubble_game_versus_record" }
