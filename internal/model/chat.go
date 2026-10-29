package model

// ChatRoom represents the `chat_room` table.
//
// **ID はローカルの行 ID で、リモート room の身元ではない (#2994)。** room id は
// 相手が自由に決められる値 (`https://<host>/chat/rooms/{id}` から取り出す) なので、
// ID 空間はホストをまたいで共有されている。ID だけで keying していた頃は、
// あるホストが先に同じ id の room を作ると、別のホストの正規の room からの Invite が
// owner 不一致で恒久的に drop されていた。リモート room の身元は URI で持つ。
type ChatRoom struct {
	ID          string `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	Name        string `gorm:"column:name;type:varchar(256);not null" json:"name"`
	OwnerID     string `gorm:"column:ownerId;type:varchar(32);not null" json:"ownerId"`
	Description string `gorm:"column:description;type:varchar(2048);default:''" json:"description"`
	IsArchived  bool   `gorm:"column:isArchived;default:false" json:"isArchived"`

	// Host is the origin host of a remote room, NULL for local ones
	// (`user.host` / `emoji.host` と同じ規約)。
	Host *string `gorm:"column:host;type:varchar(128)" json:"-"`
	// URI is the canonical AP URI of a remote room, NULL for local ones
	// (`note.uri` / `chat_message.uri` と同じ規約)。部分 UNIQUE index が張って
	// あり、同じ room の二重取り込みを DB で止める。
	URI *string `gorm:"column:uri;type:varchar(512)" json:"-"`

	Owner *User `gorm:"foreignKey:OwnerID" json:"owner,omitempty"`
}

func (ChatRoom) TableName() string { return "chat_room" }

// ChatMessage represents the `chat_message` table.
type ChatMessage struct {
	ID              string      `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	FromUserID      string      `gorm:"column:fromUserId;type:varchar(32);not null" json:"fromUserId"`
	ToUserID        *string     `gorm:"column:toUserId;type:varchar(32)" json:"toUserId"`
	ToRoomID        *string     `gorm:"column:toRoomId;type:varchar(32)" json:"toRoomId"`
	Text            *string     `gorm:"column:text;type:varchar(4096)" json:"text"`
	URI             *string     `gorm:"column:uri;type:varchar(512)" json:"uri"`
	Reads           StringArray `gorm:"column:reads;type:varchar(32)[];default:'{}'" json:"reads"`
	FileID          *string     `gorm:"column:fileId;type:varchar(32)" json:"fileId"`
	Reactions       StringArray `gorm:"column:reactions;type:varchar(1024)[];default:'{}'" json:"reactions"`
	Emojis          StringArray `gorm:"column:emojis;type:varchar(128)[];default:'{}'" json:"emojis"`
	IsDelivering    bool        `gorm:"column:isDelivering;default:false" json:"isDelivering"`
	IsDeliverFailed bool        `gorm:"column:isDeliverFailed;default:false" json:"isDeliverFailed"`

	FromUser *User      `gorm:"foreignKey:FromUserID" json:"fromUser,omitempty"`
	ToUser   *User      `gorm:"foreignKey:ToUserID" json:"toUser,omitempty"`
	ToRoom   *ChatRoom  `gorm:"foreignKey:ToRoomID" json:"toRoom,omitempty"`
	File     *DriveFile `gorm:"foreignKey:FileID" json:"file,omitempty"`
}

func (ChatMessage) TableName() string { return "chat_message" }

// ChatRoomMembership represents the `chat_room_membership` table.
type ChatRoomMembership struct {
	ID      string `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	UserID  string `gorm:"column:userId;type:varchar(32);not null" json:"userId"`
	RoomID  string `gorm:"column:roomId;type:varchar(32);not null" json:"roomId"`
	IsMuted bool   `gorm:"column:isMuted;default:false" json:"isMuted"`

	User *User     `gorm:"foreignKey:UserID" json:"user,omitempty"`
	Room *ChatRoom `gorm:"foreignKey:RoomID" json:"room,omitempty"`
}

func (ChatRoomMembership) TableName() string { return "chat_room_membership" }

// ChatRoomInvitation represents the `chat_room_invitation` table.
type ChatRoomInvitation struct {
	ID      string `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	UserID  string `gorm:"column:userId;type:varchar(32);not null" json:"userId"`
	RoomID  string `gorm:"column:roomId;type:varchar(32);not null" json:"roomId"`
	Ignored bool   `gorm:"column:ignored;default:false" json:"ignored"`

	User *User     `gorm:"foreignKey:UserID" json:"user,omitempty"`
	Room *ChatRoom `gorm:"foreignKey:RoomID" json:"room,omitempty"`
}

func (ChatRoomInvitation) TableName() string { return "chat_room_invitation" }

// ChatApproval represents the `chat_approval` table. It manages per-user
// overrides for 1-on-1 chat permissions (works with User.ChatScope).
type ChatApproval struct {
	ID      string `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	UserID  string `gorm:"column:userId;type:varchar(32);not null" json:"userId"`
	OtherID string `gorm:"column:otherId;type:varchar(32);not null" json:"otherId"`

	User  *User `gorm:"foreignKey:UserID" json:"user,omitempty"`
	Other *User `gorm:"foreignKey:OtherID" json:"other,omitempty"`
}

func (ChatApproval) TableName() string { return "chat_approval" }

// UserPending represents the `user_pending` table. It holds users awaiting
// email confirmation during invite-based registration.
type UserPending struct {
	ID string `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`
	// サインアップ確認コード。これを持っていればアカウントを有効化できる。
	Code             string           `gorm:"column:code;type:varchar(128);not null;uniqueIndex" json:"-"`
	Username         string           `gorm:"column:username;type:varchar(128);not null" json:"username"`
	Email            string           `gorm:"column:email;type:varchar(128);not null" json:"email"`
	Password         string           `gorm:"column:password;type:varchar(128);not null" json:"-"`
	FederationStatus FederationStatus `gorm:"column:federationStatus;type:varchar(16);not null;default:'on'" json:"-"`
	// InvitationTicketID は招待制 + email 確認制の併用時に消費した
	// registration_ticket の ID。PromotePending 完了時に MarkUsed する。
	// 通常 (招待制無効) signup では nil。
	InvitationTicketID *string `gorm:"column:invitationTicketId;type:varchar(32)" json:"invitationTicketId,omitempty"`
	// SignupApplicationID は承認制 (#2571) でメール確認を挟むときの申請 ID。
	// 確認完了時に申請を completed にするために持つ。**これが無いと申請が
	// approved のまま残り、1 つの承認から複数アカウントを作れる。**
	SignupApplicationID *string `gorm:"column:signupApplicationId;type:varchar(32)" json:"signupApplicationId,omitempty"`
}

func (UserPending) TableName() string { return "user_pending" }
