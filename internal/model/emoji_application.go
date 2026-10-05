package model

import (
	"time"

	"github.com/elythia-network/elythia/internal/pgarray"
)

// EmojiApplication status values.
//
// 状態遷移は core/emojiapplication.Service が持つ。
//
//	無し    -> pending               (申請)
//	pending -> approved / rejected   (審査)
//	pending -> canceled              (申請者が取り下げ)
//
// approved / rejected / canceled は終端。出し直しは別レコードになる。
const (
	EmojiApplicationPending  = "pending"
	EmojiApplicationApproved = "approved"
	EmojiApplicationRejected = "rejected"
	EmojiApplicationCanceled = "canceled"
)

// EmojiApplication kind values.
//
// **#2935 (リモート絵文字のインポート申請) と 1 テーブルを共有するための区別。**
// 審査する側が見る場所を 2 つに増やさないため、種別を問わず同じ箱に入れる。
const (
	// EmojiApplicationKindOwn is a user-supplied image held in drive.
	EmojiApplicationKindOwn = "own"
	// EmojiApplicationKindRemote is a copy of an emoji on another instance (#2935).
	EmojiApplicationKindRemote = "remote"
)

// EmojiApplication represents the `emoji_application` table (#2934).
//
// One row is one request to register a custom emoji. 承認までは `emoji` 行を
// 作らない — 承認待ちを `emoji` に隠し列で持たせると、`emoji` を読む全ての経路
// (本家と共有する packer・連合・キャッシュ) に「未承認なら除く」条件を足すことになる。
// 当初は TS に切り替えた瞬間に未承認の絵文字が全部有効になることも理由だった
// (復路は保証しなくなった。#3191)。
// `signup_application` (#2555) が `user` に対して採ったのと同じ形。
type EmojiApplication struct {
	ID string `gorm:"column:id;type:varchar(32);primaryKey" json:"id"`

	UserID string `gorm:"column:userId;type:varchar(32);not null" json:"userId"`

	Kind   string `gorm:"column:kind;type:varchar(16);not null;default:own" json:"kind"`
	Status string `gorm:"column:status;type:varchar(16);not null;default:pending" json:"status"`

	// Name は申請時点の希望。**ここでは一意にしない** — 審査中に同じ名前の
	// 絵文字が別途登録されうるので、一意制約を張ると保存時ではなく承認時に
	// 落ちる。重複は承認の直前に見てモデレーターへ知らせる。
	Name     string  `gorm:"column:name;type:varchar(128);not null" json:"name"`
	Category *string `gorm:"column:category;type:varchar(128)" json:"category"`

	Aliases pgarray.StringArray `gorm:"column:aliases;type:varchar(128)[];not null;default:'{}'" json:"aliases"`

	// License は申請では必須。通常の追加では任意だが、出典が後から辿れないと
	// 権利関係で問題が出たときに消すしか手が無くなる。
	License string `gorm:"column:license;type:varchar(1024);not null" json:"license"`

	IsSensitive bool `gorm:"column:isSensitive;not null;default:false" json:"isSensitive"`

	// FileID is set when Kind == own. 承認するまで drive に置いたままにする。
	FileID *string `gorm:"column:fileId;type:varchar(32)" json:"fileId"`

	// FileHash is the drive file's MD5 at the time of application (#2960).
	//
	// **スナップショットで持つ。** 申請者は審査を待つ間に drive のファイルを
	// 消せるので、照合のたびに引き直すと「消した申請は履歴から消える」ことに
	// なり、過去の判断を追えなくなる。**審査の材料にのみ使う** — 自動拒否や
	// 重複判定には使わない。
	//
	// **API には出さない。** 照合の結果は `matchedBy` で伝わるので出す理由が
	// 無く、MD5 は画像の指紋なので「このインスタンスが既知の画像を持っているか」
	// を外から確かめる手がかりになる。
	FileHash *string `gorm:"column:fileHash;type:varchar(32)" json:"-"`

	// RemoteHost / RemoteName are set when Kind == remote (#2935).
	RemoteHost *string `gorm:"column:remoteHost;type:varchar(128)" json:"remoteHost"`
	RemoteName *string `gorm:"column:remoteName;type:varchar(128)" json:"remoteName"`

	Comment *string `gorm:"column:comment;type:varchar(2048)" json:"comment"`

	CreatedAt time.Time `gorm:"column:createdAt;not null;default:now()" json:"createdAt"`
	UpdatedAt time.Time `gorm:"column:updatedAt;not null;default:now()" json:"updatedAt"`

	ProcessedByID *string    `gorm:"column:processedById;type:varchar(32)" json:"processedById"`
	ProcessedAt   *time.Time `gorm:"column:processedAt" json:"processedAt"`

	// RejectReason は申請者にそのまま見せる。
	RejectReason *string `gorm:"column:rejectReason;type:varchar(2048)" json:"rejectReason"`

	// EmojiID は承認して作られた絵文字。
	//
	// **絵文字を消しても NULL には落ちない。** migration は FK を張っていない
	// (`000086_emoji_application.up.sql`、`signup_application` と同じ方針)
	// ので、行は id を持ったまま残る。`emoji-application/list-mine` の
	// プレビュー (#2989) はこれに依存していて、**id はあるのに絵文字が
	// 引けない = 承認後に削除された**と判定する。ここを「NULL に落ちる」と
	// 読むと、その分岐を逆に直してしまう。
	EmojiID *string `gorm:"column:emojiId;type:varchar(32)" json:"emojiId"`
}

// TableName returns the table name for GORM.
func (EmojiApplication) TableName() string { return "emoji_application" }

// IsPending reports whether the application is still awaiting review.
func (a *EmojiApplication) IsPending() bool {
	return a != nil && a.Status == EmojiApplicationPending
}
