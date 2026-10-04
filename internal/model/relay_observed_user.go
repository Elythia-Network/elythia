package model

import "time"

// RelayObservedUser records that a remote user was first observed through a
// subscribed relay (#2340).
//
// `user` に列を足さず別テーブルにしてある。`user` は連合・認証・API のあらゆる経路が
// 触るホットテーブルで、本家と共有する列の形を崩さずに済む。当初は TS 側から一切
// 見えなくなることも理由だった (復路は保証しなくなった。#3191)。
//
// 用途は孤児掃除の対象をリレー由来に限定すること。印が無いと、リレー購読前から
// 居る行やプロフィール閲覧・スレッド遡りで解決された行まで巻き込む。
type RelayObservedUser struct {
	UserID     string    `gorm:"column:userId;type:varchar(32);primaryKey" json:"userId"`
	ObservedAt time.Time `gorm:"column:observedAt;not null" json:"observedAt"`
}

func (RelayObservedUser) TableName() string { return "relay_observed_user" }
