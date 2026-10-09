package plugin

import (
	"context"
	"time"
)

/*
 * 管理するアカウントへの通知 (#3469)。
 *
 * bot がメンションに返事をするのに、定期的に `i/notifications` を読みに行かず
 * に済むようにする口。届けるのは**このプラグインが管理するアカウント
 * ([Accounts]) への通知だけ**で、他のプラグインのアカウントや普通の利用者への
 * 通知は届かない。
 *
 * **handler は通知を作る処理から切り離して呼ぶ。** 本体は通知を作った後に
 * プラグインの専用キュー (`plugin:<名前>`) へ積むだけで、handler はキューの
 * worker が呼ぶ。handler が遅くても失敗しても、投稿や通知の処理は待たないし
 * 失敗もしない。
 */

// Notifications registers the handler for notifications addressed to the
// accounts this plugin manages (#3469).
type Notifications interface {
	// Handle sets the handler. 呼ぶのは 1 回だけ (2 回目は前の handler を
	// 置き換える)。[Definition.Notifications] を宣言したのに呼ばないと、
	// 起動に失敗する。
	Handle(h NotificationHandler)
}

// NotificationHandler handles one notification. Returning an error retries
// the delivery later, unless the error wraps [ErrNoRetry] (see [NoRetry]).
//
// **at-least-once で届く。** 失敗すれば再試行し (初回を含めて 5 回まで、
// 10 秒起点の指数バックオフ)、成功していても worker の再起動などで同じ通知が
// もう一度届くことがある。[Notification.ID] を使って冪等に書くこと (返事を
// 二重に投稿しない、など)。
//
// **ctx を尊重すること。** ジョブと同じく 1 回の実行に上限がある
// ([JobHandler] を参照)。
type NotificationHandler func(ctx context.Context, n Notification) error

// NotificationType is the kind of a delivered notification.
//
// 値は本体の通知の type と同じ文字列 (チャットだけは通知を作らないので独自)。
// 今後種類が増えることがあるので、知らない値は無視すること。
type NotificationType string

// The notification types delivered to plugins.
const (
	// NotificationMention: an account mentioned the managed account.
	NotificationMention NotificationType = "mention"
	// NotificationReply: an account replied to the managed account's note.
	NotificationReply NotificationType = "reply"
	// NotificationQuote: an account quoted the managed account's note.
	NotificationQuote NotificationType = "quote"
	// NotificationFollow: an account followed the managed account.
	NotificationFollow NotificationType = "follow"
	// NotificationFollowRequest: an account requested to follow the managed
	// account (when it approves followers manually).
	NotificationFollowRequest NotificationType = "receiveFollowRequest"
	// NotificationReaction: an account reacted to the managed account's note.
	NotificationReaction NotificationType = "reaction"
	// NotificationChatMessage: an account sent a direct (1:1) chat message to
	// the managed account. ルームのメッセージは届けない。
	NotificationChatMessage NotificationType = "chatMessage"
)

// Notification is one notification addressed to a managed account.
//
// 本文などの中身は持たない。必要なら AccountID として本体の API を呼んで
// 取りに行く (`ctx.API().AsUser(n.AccountID).Call(c, "notes/show", ...)`)。
// その時点の可視性で判定されるので、削除された投稿は取れない。
type Notification struct {
	// ID identifies this delivery. Retries of the same notification carry the
	// same ID. 通知なら通知の ID、チャットならメッセージの ID。
	ID string
	// Type is the kind of notification.
	Type NotificationType
	// AccountID is the managed account that received the notification.
	AccountID string
	// UserID is the account that caused it (the author of the mention, the
	// follower, the sender of the message, ...). リモートの利用者のこともある。
	UserID string
	// NoteID is the related note: the mentioning / replying / quoting note
	// itself, or the managed account's note that got the reaction. Empty for
	// follow, follow request and chat.
	NoteID string
	// Reaction is the reaction (a Unicode emoji or ":name@.:"), only for
	// [NotificationReaction].
	Reaction string
	// NoteVisibility is the visibility of the note in NoteID ("public",
	// "home", "followers" or "specified") when the notification was created,
	// for mention, reply and quote. それ以外 (リアクションを含む) は空。
	//
	// **"specified" (ダイレクト) に公開の返事をしないこと。** 宛先の外へ
	// 本文が漏れる。返事を投稿するなら公開範囲を合わせるか、無視する。
	NoteVisibility string
	// ChatMessageID is the message, only for [NotificationChatMessage].
	ChatMessageID string
	// CreatedAt is when the host created the notification (or stored the
	// message).
	CreatedAt time.Time
}
