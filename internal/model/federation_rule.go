package model

import "time"

// Federation rule modes.
const (
	FederationRuleModeDisabled = "disabled"
	FederationRuleModeRecord   = "record"
	FederationRuleModeEnforce  = "enforce"
)

// Federation rule targets.
const (
	FederationRuleTargetNote     = "note"
	FederationRuleTargetActivity = "activity"
)

// FederationRule represents the mk-go-only `federation_rule` table (#3090):
// a set of conditions and actions applied to inbound activities and notes,
// layered on top of the host-level settings in meta.
//
// 空配列 / nil の条件は問わない。条件は AND、配列の中は OR。
type FederationRule struct {
	ID        string    `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	CreatedAt time.Time `gorm:"column:createdAt;type:timestamp with time zone;not null" json:"createdAt"`
	UpdatedAt time.Time `gorm:"column:updatedAt;type:timestamp with time zone;not null" json:"updatedAt"`
	Name      string    `gorm:"column:name;type:varchar(128);not null;default:''" json:"name"`
	Mode      string    `gorm:"column:mode;type:varchar(16);not null;default:'record'" json:"mode"`
	Target    string    `gorm:"column:target;type:varchar(16);not null;default:'note'" json:"target"`
	Position  int       `gorm:"column:position;not null;default:0" json:"position"`

	Hosts          StringArray `gorm:"column:hosts;type:varchar(128)[];not null;default:'{}'" json:"hosts"`
	ActivityTypes  StringArray `gorm:"column:activityTypes;type:varchar(64)[];not null;default:'{}'" json:"activityTypes"`
	IsBot          *bool       `gorm:"column:isBot" json:"isBot"`
	NewWithinHours *int        `gorm:"column:newWithinHours" json:"newWithinHours"`
	Patterns       StringArray `gorm:"column:patterns;type:varchar(1024)[];not null;default:'{}'" json:"patterns"`
	HasAttachment  *bool       `gorm:"column:hasAttachment" json:"hasAttachment"`
	Tags           StringArray `gorm:"column:tags;type:varchar(128)[];not null;default:'{}'" json:"tags"`

	Reject     bool    `gorm:"column:reject;not null;default:false" json:"reject"`
	StripMedia bool    `gorm:"column:stripMedia;not null;default:false" json:"stripMedia"`
	Sensitive  bool    `gorm:"column:sensitive;not null;default:false" json:"sensitive"`
	Unlist     bool    `gorm:"column:unlist;not null;default:false" json:"unlist"`
	CW         *string `gorm:"column:cw;type:varchar(512)" json:"cw"`
}

func (FederationRule) TableName() string {
	return "federation_rule"
}
