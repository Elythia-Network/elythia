package model

import (
	"slices"
	"time"
)

// SuspensionState represents the suspension state of a remote instance.
type SuspensionState string

const (
	SuspensionStateNone                          SuspensionState = "none"
	SuspensionStateManuallySuspended             SuspensionState = "manuallySuspended"
	SuspensionStateGoneSuspended                 SuspensionState = "goneSuspended"
	SuspensionStateAutoSuspendedForNotResponding SuspensionState = "autoSuspendedForNotResponding"
)

// Instance represents the `instance` table.
type Instance struct {
	ID                      string          `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	FirstRetrievedAt        time.Time       `gorm:"column:firstRetrievedAt;type:timestamp with time zone;not null" json:"firstRetrievedAt"`
	Host                    string          `gorm:"column:host;type:varchar(128);uniqueIndex;not null" json:"host"`
	UsersCount              int             `gorm:"column:usersCount;default:0" json:"usersCount"`
	NotesCount              int             `gorm:"column:notesCount;default:0" json:"notesCount"`
	FollowingCount          int             `gorm:"column:followingCount;default:0" json:"followingCount"`
	FollowersCount          int             `gorm:"column:followersCount;default:0" json:"followersCount"`
	LatestRequestReceivedAt *time.Time      `gorm:"column:latestRequestReceivedAt;type:timestamp with time zone" json:"latestRequestReceivedAt"`
	IsNotResponding         bool            `gorm:"column:isNotResponding;default:false" json:"isNotResponding"`
	NotRespondingSince      *time.Time      `gorm:"column:notRespondingSince;type:timestamp with time zone" json:"notRespondingSince"`
	SuspensionState         SuspensionState `gorm:"column:suspensionState;type:varchar(64);default:'none'" json:"suspensionState"`
	SoftwareName            *string         `gorm:"column:softwareName;type:varchar(64)" json:"softwareName"`
	SoftwareVersion         *string         `gorm:"column:softwareVersion;type:varchar(64)" json:"softwareVersion"`
	OpenRegistrations       *bool           `gorm:"column:openRegistrations" json:"openRegistrations"`
	Name                    *string         `gorm:"column:name;type:varchar(256)" json:"name"`
	Description             *string         `gorm:"column:description;type:varchar(4096)" json:"description"`
	MaintainerName          *string         `gorm:"column:maintainerName;type:varchar(128)" json:"maintainerName"`
	MaintainerEmail         *string         `gorm:"column:maintainerEmail;type:varchar(256)" json:"maintainerEmail"`
	IconURL                 *string         `gorm:"column:iconUrl;type:varchar(256)" json:"iconUrl"`
	FaviconURL              *string         `gorm:"column:faviconUrl;type:varchar(256)" json:"faviconUrl"`
	ThemeColor              *string         `gorm:"column:themeColor;type:varchar(64)" json:"themeColor"`
	InfoUpdatedAt           *time.Time      `gorm:"column:infoUpdatedAt;type:timestamp with time zone" json:"infoUpdatedAt"`
	ModerationNote          string          `gorm:"column:moderationNote;type:varchar(16384);default:''" json:"moderationNote"`
}

func (Instance) TableName() string { return "instance" }

// InstanceListFilter narrows down the list of instances returned by repository
// queries. Zero values mean "no filtering on this field". Repository と service
// の双方から参照されるため model パッケージに置く (循環依存の回避)。
type InstanceListFilter struct {
	Host          string // 部分一致 (case-insensitive ILIKE)
	Suspended     *bool
	NotResponding *bool
	Federating    *bool // followingCount > 0 OR followersCount > 0
	Subscribing   *bool // followersCount > 0 (向こうがフォローしてくる)
	Publishing    *bool // followingCount > 0 (こちらからフォローしている)
	// Blocked / Silenced は instance 列ではなく meta.blockedHosts /
	// silencedHosts との突合で判定する。突合対象の host 一覧は service 層が
	// meta から解決して BlockedHosts / SilencedHosts に詰める。本家 TS の
	// federation/instances と同じく exact host IN/NOT IN matching を使う
	// (suffix match の IsBlocked / IsSilenced とは別物)。BlockedHosts /
	// SilencedHosts は対応する *bool が non-nil のときだけ参照される。
	Blocked       *bool
	Silenced      *bool
	BlockedHosts  []string
	SilencedHosts []string
	SortBy        string
	Limit         int
	Offset        int
	// Cursor pagination (frontend Paginator). 指定時は SortBy を id 順に
	// 上書きして cursor 一貫性を保つ。Offset は無視する。
	SinceID string
	UntilID string
}

// InstanceUpdateCondition is the state an instance row must still be in for
// InstanceRepository.UpdateFieldsIf to write it (#3731). The zero value
// matches every row.
type InstanceUpdateCondition struct {
	// SuspensionIn lists the suspension states the row may be in. Empty
	// means any state.
	SuspensionIn []SuspensionState
	// SuspensionNotIn lists the suspension states the row must not be in.
	SuspensionNotIn []SuspensionState
	// NotRespondingSinceAtOrBefore, when set, requires isNotResponding = true
	// and a notRespondingSince at or before it.
	NotRespondingSinceAtOrBefore *time.Time
}

// Matches reports whether inst satisfies c. The mock repository uses it, so
// the fake and the SQL of UpdateFieldsIf agree on what a condition means.
//
// repository ではなく model に置くのは、testutil の mock から使うため (repository の
// テストが testutil を import するので、testutil は repository を import できない)。
func (c InstanceUpdateCondition) Matches(inst *Instance) bool {
	if len(c.SuspensionIn) > 0 && !slices.Contains(c.SuspensionIn, inst.SuspensionState) {
		return false
	}
	if slices.Contains(c.SuspensionNotIn, inst.SuspensionState) {
		return false
	}
	if c.NotRespondingSinceAtOrBefore != nil &&
		(!inst.IsNotResponding || inst.NotRespondingSince == nil || inst.NotRespondingSince.After(*c.NotRespondingSinceAtOrBefore)) {
		return false
	}
	return true
}
