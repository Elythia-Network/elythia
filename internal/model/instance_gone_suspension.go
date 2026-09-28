package model

import "time"

// InstanceGoneSuspension represents the `instance_gone_suspension` table.
//
// mk-go 独自テーブルで、shared inbox が 410 を返して goneSuspended になった時刻を
// host 単位で記録する (#3067)。行があっても「今も消えている」とは限らない
// (管理者が戻した後も残る) ので、読む側は instance."suspensionState" で絞る。
type InstanceGoneSuspension struct {
	Host        string    `gorm:"column:host;type:varchar(128);primaryKey" json:"host"`
	SuspendedAt time.Time `gorm:"column:suspendedAt;type:timestamp with time zone;not null" json:"suspendedAt"`
}

func (InstanceGoneSuspension) TableName() string {
	return "instance_gone_suspension"
}
