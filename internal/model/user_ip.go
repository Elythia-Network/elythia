package model

import "time"

// UserIP represents the `user_ip` table.
//
// 認証済みリクエストで観測した IP を記録する。`meta.enableIpLogging` が真のときだけ
// 書き込む。
type UserIP struct {
	ID int64 `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	// CreatedAt is the **first** observation of this (user, ip) pair.
	//
	// **衝突時に更新しない。** upstream の `ApiCallService.logIp` が
	// `INSERT ... orIgnore` なので、この列は初回観測を意味する。mk-go は #3103 より
	// 前は衝突時に上書きしており最終観測を意味していた。純正から引き継いだ DB では
	// 両方の意味の行が混ざるため、mk-go 側を upstream に合わせた。
	CreatedAt time.Time `gorm:"column:createdAt;autoCreateTime" json:"createdAt"`
	UserID    string    `gorm:"column:userId;type:varchar(32);not null" json:"userId"`
	IP        string    `gorm:"column:ip;type:varchar(128);not null" json:"ip"`
	// LastSeenAt / ObservationCount are mk-go 独自列 (#3103)。`UNIQUE (userId, ip)`
	// で 1 ペア 1 行しか持てないので、最終観測と観測回数を出すには列が要る。
	// **純正はこの 2 列を知らない**ので DEFAULT を持たせてある (migration 000094)。
	// TS へ戻したときに TS の INSERT が通るための DEFAULT で、復路は保証しない (#3191)。
	LastSeenAt       time.Time `gorm:"column:lastSeenAt" json:"lastSeenAt"`
	ObservationCount int       `gorm:"column:observationCount" json:"observationCount"`
}

func (UserIP) TableName() string { return "user_ip" }
