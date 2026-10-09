package model

import (
	"gorm.io/datatypes"
)

// FollowingVisibility represents the visibility of following list.
type FollowingVisibility string

const (
	FollowingVisibilityPublic    FollowingVisibility = "public"
	FollowingVisibilityFollowers FollowingVisibility = "followers"
	FollowingVisibilityPrivate   FollowingVisibility = "private"
)

// Values of UserProfile.FollowApprovalAction (#3466).
const (
	// FollowApprovalActionRequest turns the follow into a follow request and
	// notifies the followee, like upstream. It is the default.
	FollowApprovalActionRequest = "request"
	// FollowApprovalActionSilentRequest turns the follow into a follow request
	// without the receiveFollowRequest notification.
	FollowApprovalActionSilentRequest = "silentRequest"
	// FollowApprovalActionSilentFollow lets the follow succeed without the
	// follow notification and records it in silent_follow.
	FollowApprovalActionSilentFollow = "silentFollow"
)

// IsFollowApprovalAction reports whether a is a valid followApprovalAction.
func IsFollowApprovalAction(a string) bool {
	switch a {
	case FollowApprovalActionRequest, FollowApprovalActionSilentRequest, FollowApprovalActionSilentFollow:
		return true
	}
	return false
}

// NormalizeFollowApprovalAction returns a when it is a valid action, and
// FollowApprovalActionRequest (the upstream behaviour) otherwise.
//
// 列は NOT NULL DEFAULT 'request' だが、列を通さずに組んだ profile (テストの
// モックなど) は空文字になる。知らない値も本家と同じ動きに倒す。
func NormalizeFollowApprovalAction(a string) string {
	if IsFollowApprovalAction(a) {
		return a
	}
	return FollowApprovalActionRequest
}

// UserProfile represents the `user_profile` table.
type UserProfile struct {
	UserID          string         `gorm:"column:userId;type:varchar(32);primaryKey" json:"userId"`
	Location        *string        `gorm:"column:location;type:varchar(128)" json:"location"`
	Birthday        *string        `gorm:"column:birthday;type:char(10)" json:"birthday"`
	Description     *string        `gorm:"column:description;type:varchar(2048)" json:"description"`
	FollowedMessage *string        `gorm:"column:followedMessage;type:varchar(256)" json:"followedMessage"`
	Fields          datatypes.JSON `gorm:"column:fields;type:jsonb;default:'[]'" json:"fields"`
	VerifiedLinks   StringArray    `gorm:"column:verifiedLinks;type:varchar[];default:'{}'" json:"verifiedLinks"`
	Lang            *string        `gorm:"column:lang;type:varchar(32)" json:"lang"`
	URL             *string        `gorm:"column:url;type:varchar(512)" json:"url"`
	Email           *string        `gorm:"column:email;type:varchar(128)" json:"email"`
	// メール確認用のワンタイムトークン。持っていれば本人確認を通せるので出さない。
	EmailVerifyCode        *string             `gorm:"column:emailVerifyCode;type:varchar(128)" json:"-"`
	EmailVerified          bool                `gorm:"column:emailVerified;default:false" json:"emailVerified"`
	EmailNotificationTypes datatypes.JSON      `gorm:"column:emailNotificationTypes;type:jsonb;default:'[\"follow\",\"receiveFollowRequest\"]'" json:"emailNotificationTypes"`
	PublicReactions        bool                `gorm:"column:publicReactions;default:true" json:"publicReactions"`
	FollowingVisibility    FollowingVisibility `gorm:"column:followingVisibility;type:following_visibility_enum;default:'public'" json:"followingVisibility"`
	FollowersVisibility    FollowingVisibility `gorm:"column:followersVisibility;type:followers_visibility_enum;default:'public'" json:"followersVisibility"`
	TwoFactorTempSecret    *string             `gorm:"column:twoFactorTempSecret;type:varchar(128)" json:"-"`
	TwoFactorSecret        *string             `gorm:"column:twoFactorSecret;type:varchar(128)" json:"-"`
	TwoFactorBackupSecret  StringArray         `gorm:"column:twoFactorBackupSecret;type:varchar[]" json:"-"`
	TwoFactorEnabled       bool                `gorm:"column:twoFactorEnabled;default:false" json:"twoFactorEnabled"`
	SecurityKeysAvailable  bool                `gorm:"column:securityKeysAvailable;default:false" json:"securityKeysAvailable"`
	UsePasswordLessLogin   bool                `gorm:"column:usePasswordLessLogin;default:false" json:"usePasswordLessLogin"`
	Password               *string             `gorm:"column:password;type:varchar(128)" json:"-"`
	ModerationNote         *string             `gorm:"column:moderationNote;type:varchar(8192);default:''" json:"moderationNote"`
	AutoAcceptFollowed     bool                `gorm:"column:autoAcceptFollowed;default:false" json:"autoAcceptFollowed"`
	// FollowApprovalLocalSeconds / FollowApprovalRemoteSeconds は、作られて
	// からこの秒数に満たないアカウントからのフォローを止める設定 (#3466、本家
	// PR 17998 と同じ列)。NULL と 0 は止めない。
	FollowApprovalLocalSeconds  *int `gorm:"column:followApprovalLocalSeconds" json:"followApprovalLocalSeconds"`
	FollowApprovalRemoteSeconds *int `gorm:"column:followApprovalRemoteSeconds" json:"followApprovalRemoteSeconds"`
	// FollowApprovalAction は止め方 (#3466、Elythia 独自)。値は
	// FollowApprovalAction* の定数。空文字は既定 (request) として読む。
	FollowApprovalAction      string         `gorm:"column:followApprovalAction;type:varchar(16);default:'request'" json:"followApprovalAction"`
	NoCrawle                  bool           `gorm:"column:noCrawle;default:false" json:"noCrawle"`
	PreventAiLearning         bool           `gorm:"column:preventAiLearning;default:true" json:"preventAiLearning"`
	AlwaysMarkNsfw            bool           `gorm:"column:alwaysMarkNsfw;default:false" json:"alwaysMarkNsfw"`
	AutoSensitive             bool           `gorm:"column:autoSensitive;default:false" json:"autoSensitive"`
	CarefulBot                bool           `gorm:"column:carefulBot;default:false" json:"carefulBot"`
	InjectFeaturedNote        bool           `gorm:"column:injectFeaturedNote;default:true" json:"injectFeaturedNote"`
	ReceiveAnnouncementEmail  bool           `gorm:"column:receiveAnnouncementEmail;default:true" json:"receiveAnnouncementEmail"`
	PinnedPageID              *string        `gorm:"column:pinnedPageId;type:varchar(32)" json:"pinnedPageId"`
	EnableWordMute            bool           `gorm:"column:enableWordMute;default:false" json:"enableWordMute"`
	MutedWords                datatypes.JSON `gorm:"column:mutedWords;type:jsonb;default:'[]'" json:"mutedWords"`
	HardMutedWords            datatypes.JSON `gorm:"column:hardMutedWords;type:jsonb;default:'[]'" json:"hardMutedWords"`
	MutedInstances            datatypes.JSON `gorm:"column:mutedInstances;type:jsonb;default:'[]'" json:"mutedInstances"`
	NotificationRecieveConfig datatypes.JSON `gorm:"column:notificationRecieveConfig;type:jsonb;default:'{}'" json:"notificationRecieveConfig"`
	LoggedInDates             StringArray    `gorm:"column:loggedInDates;type:varchar(32)[];default:'{}'" json:"loggedInDates"`
	Achievements              datatypes.JSON `gorm:"column:achievements;type:jsonb;default:'[]'" json:"achievements"`
	// ClientData / Room はクライアント固有の任意 JSON を pass-through 保存する
	// フィールド。Go バックエンドは値を解釈せず、i/update で受け取ったものをそのまま
	// 保存し、i で返却する。Go 側に解釈ロジックを追加する予定は現状なし。
	ClientData datatypes.JSON `gorm:"column:clientData;type:jsonb;default:'{}'" json:"clientData"`
	Room       datatypes.JSON `gorm:"column:room;type:jsonb;default:'{}'" json:"room"`
	UserHost   *string        `gorm:"column:userHost;type:varchar(128)" json:"userHost"`

	// Relations
	User *User `gorm:"foreignKey:UserID" json:"user,omitempty"`
}

func (UserProfile) TableName() string { return "user_profile" }

// FollowingBirthday is a (followeeID, birthday) pair returned by
// FollowingRepository.ListFollowingByBirthday. repository と testutil の両方
// から参照されるため model パッケージに置く (循環依存の回避)。
type FollowingBirthday struct {
	FolloweeID string `gorm:"column:followeeId"`
	// Birthday is the raw "YYYY-MM-DD" string as stored in user_profile.
	Birthday string `gorm:"column:birthday"`
}

// RemoteInbox is a remote follower's delivery inbox plus whether it is the
// instance's shared inbox (vs a personal inbox). Shared inboxes drive the
// deliver goneSuspended decision on a 410 Gone (#1811)。query-result DTO なので
// repository ではなく model に置く (testutil mock が repository を import すると
// repository の internal test 経由で import cycle になるため)。
type RemoteInbox struct {
	Inbox  string
	Shared bool
}
