// Package pluginnotify hands notifications addressed to plugin-managed
// accounts to the plugin that manages them (#3469).
//
// 本体が通知を作った後 (notification.Hook) と、1:1 のチャットを保存した後
// (chat.Service) に呼ばれ、宛先がプラグインの管理するアカウントなら、その
// プラグインの専用キューへ積む。handler を呼ぶのはキューの worker
// (internal/server) で、ここは積むだけ。
package pluginnotify

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// TypeChatMessage is the event type for a direct chat message. チャットは
// 通知を作らないので、通知の type には無い独自の値。
const TypeChatMessage = "chatMessage"

// Event is the queue payload for one delivery.
//
// **キューに残っている間に版を跨ぐことがある**ので、JSON の名前は変えない。
// 足すのはよい (古い worker は知らないキーを捨てる)。
type Event struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	AccountID string `json:"accountId"`
	UserID    string `json:"userId,omitempty"`
	NoteID    string `json:"noteId,omitempty"`
	// NoteVisibility は通知を作った時点の投稿の公開範囲 (mention / reply /
	// quote だけ)。
	NoteVisibility string    `json:"noteVisibility,omitempty"`
	Reaction       string    `json:"reaction,omitempty"`
	ChatMessageID  string    `json:"chatMessageId,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
}

// Enqueuer puts one delivery onto a plugin's own queue. 実装は queue.Client。
type Enqueuer interface {
	EnqueuePluginNotification(ctx context.Context, plugin string, body []byte) error
}

// UserFinder looks users and profiles up. 実装は repository.UserRepository。
type UserFinder interface {
	FindByID(id string) (*model.User, error)
	FindProfileByUserID(userID string) (*model.UserProfile, error)
}

// MuteChecker reports whether muter has muted mutee. 実装は core/muting。
type MuteChecker interface {
	IsMuted(muterID, muteeID string) (bool, error)
}

// BlockChecker reports whether blocker has blocked blockee. 実装は
// repository.BlockingRepository。
type BlockChecker interface {
	Exists(blockerID, blockeeID string) (bool, error)
}

// Deps are what the dispatcher needs from the host.
type Deps struct {
	// Enqueuer and Users are required. どちらかが nil なら New は nil を返す
	// (相手や受信設定を確かめられないまま届けない)。
	Enqueuer Enqueuer
	Users    UserFinder
	// Mutes / Blocks gate deliveries by the managed account's own choices.
	// nil なら確かめない。
	Mutes  MuteChecker
	Blocks BlockChecker
}

// deliveredTypes are the notification types plugins receive.
//
// **増やすときは公開面 (plugin.NotificationType) と docs も一緒に。** ここに
// 無い型の通知 (note / renote / pollEnded など) は積まない。
var deliveredTypes = map[notification.Type]struct{}{
	notification.TypeMention:          {},
	notification.TypeReply:            {},
	notification.TypeQuote:            {},
	notification.TypeFollow:           {},
	notification.TypeReceiveFollowReq: {},
	notification.TypeReaction:         {},
}

// enqueueTimeout bounds one enqueue.
//
// 通知を作る処理の中で同期に積むので、Redis が詰まったときに投稿の処理を
// 長く止めないよう短く切る。積めなかった分は届かない (ログに残す)。
const enqueueTimeout = 5 * time.Second

// Dispatcher routes notifications to the plugins that manage the recipients.
type Dispatcher struct {
	plugins map[string]struct{}
	deps    Deps
	now     func() time.Time
}

// New returns a dispatcher for the named plugins (the enabled ones that
// declared a notification handler), or nil when there is nothing to deliver.
//
// **nil を返したら配線しないこと。** 通知を作るたびに宛先を調べる処理を、
// 受け取るプラグインが居ない構成で走らせない。
func New(plugins []string, deps Deps) *Dispatcher {
	if len(plugins) == 0 || deps.Enqueuer == nil || deps.Users == nil {
		return nil
	}
	set := make(map[string]struct{}, len(plugins))
	for _, p := range plugins {
		set[p] = struct{}{}
	}
	return &Dispatcher{plugins: set, deps: deps, now: time.Now}
}

// NotificationCreated implements notification.CreatedObserver.
//
// Hook が抑制の判定 (ミュート・受信設定・ロールの opt-out・スレッドミュート)
// を通して実際に作った通知だけがここに来る。
func (d *Dispatcher) NotificationCreated(ctx context.Context, recipient *model.User, n *notification.Notification) {
	if n == nil {
		return
	}
	if _, ok := deliveredTypes[n.Type]; !ok {
		return
	}
	plugin, ok := d.owner(recipient)
	if !ok {
		return
	}
	// **相手で絞る。** 本体はブロックした相手からのメンションでも通知を作る
	// (本家と同じ。ブロックでは通知は止まらない) し、インスタンスのミュートと
	// 凍結された相手は i/notifications が読むときに落とすだけ。ここで絞らないと、
	// 利用者には見えない通知に bot が返事をする。
	if n.NotifierID != "" && !d.allowed(plugin, recipient.ID, n.NotifierID) {
		return
	}
	d.enqueue(ctx, plugin, Event{
		ID:             n.ID,
		Type:           string(n.Type),
		AccountID:      recipient.ID,
		UserID:         n.NotifierID,
		NoteID:         n.NoteID,
		NoteVisibility: n.NoteVisibility,
		Reaction:       n.Reaction,
		CreatedAt:      n.CreatedAt,
	})
}

// DirectMessageCreated implements chat.MessageObserver.
//
// チャットは通知を作らないので、Hook の抑制の判定を通らない。ブロックは
// chat.Service がメッセージごと拒否しているが、ミュートは見ていない
// (本家もチャットをミュートで止めない) ので、ここで見る。bot が相手を
// ミュートしたら、そのメッセージに反応しないようにするため。
//
// recipient は chat.Service が既に引いていれば渡ってくる。そのときは引き直さない
// (プラグインが居る構成では全ての DM がここを通るので、管理するアカウント以外の
// 宛先に余計な問い合わせをしない)。ID が合わないものは信用せず引き直す。
func (d *Dispatcher) DirectMessageCreated(ctx context.Context, msg *model.ChatMessage, recipient *model.User) {
	if msg == nil || msg.ToUserID == nil || *msg.ToUserID == msg.FromUserID {
		return
	}
	if recipient == nil || recipient.ID != *msg.ToUserID {
		r, err := d.deps.Users.FindByID(*msg.ToUserID)
		if err != nil {
			return
		}
		recipient = r
	}
	plugin, ok := d.owner(recipient)
	if !ok {
		return
	}
	if !d.allowed(plugin, recipient.ID, msg.FromUserID) {
		return
	}
	d.enqueue(ctx, plugin, Event{
		ID:            msg.ID,
		Type:          TypeChatMessage,
		AccountID:     recipient.ID,
		UserID:        msg.FromUserID,
		ChatMessageID: msg.ID,
		CreatedAt:     d.now(),
	})
}

// allowed reports whether the managed account still wants to hear from
// other: other is not blocked or muted by it, not suspended, and not on an
// instance it muted.
//
// i/notifications が読むときに落とす条件 (ミュート・インスタンスのミュート・
// 凍結。api/notifications の filterValidNotifiers) と、ブロックを見る。
// ミュートは Hook も作る前に見ているが、あちらは判定できないと通す。
//
// **判定できないときは届けない。** ミュート・ブロックした相手に bot が返事を
// する方が、1 件取りこぼすより害が大きい。
func (d *Dispatcher) allowed(plugin, accountID, other string) bool {
	if err := d.checkRelation(accountID, other); err != nil {
		// 相手やプロフィールが無いのは障害ではない (連合の相手が user 表に居ない
		// ことがある)。届けないが、ログは出さない。
		if !errors.Is(err, errSuppressed) && !repository.IsNotFound(err) {
			slog.Warn("plugin notification: relation check failed",
				"plugin", plugin, "account", accountID, "other", other, "err", err)
		}
		return false
	}
	return true
}

// errSuppressed は「確かめた結果、届けない」。判定の失敗と分けてログに出す。
var errSuppressed = errors.New("suppressed")

func (d *Dispatcher) checkRelation(accountID, other string) error {
	if d.deps.Blocks != nil {
		blocked, err := d.deps.Blocks.Exists(accountID, other)
		if err != nil {
			return err
		}
		if blocked {
			return errSuppressed
		}
	}
	if d.deps.Mutes != nil {
		muted, err := d.deps.Mutes.IsMuted(accountID, other)
		if err != nil {
			return err
		}
		if muted {
			return errSuppressed
		}
	}
	notifier, err := d.deps.Users.FindByID(other)
	if err != nil {
		return err
	}
	if notifier == nil || notifier.IsSuspended {
		return errSuppressed
	}
	if notifier.Host == nil {
		return nil
	}
	profile, err := d.deps.Users.FindProfileByUserID(accountID)
	if err != nil {
		return err
	}
	if profile == nil || len(profile.MutedInstances) == 0 {
		return nil
	}
	var hosts []string
	if err := json.Unmarshal(profile.MutedInstances, &hosts); err != nil {
		return err
	}
	for _, h := range hosts {
		// i/notifications と同じく完全一致で比べる。
		if h == *notifier.Host {
			return errSuppressed
		}
	}
	return nil
}

// owner returns the plugin that manages u, if that plugin takes
// notifications.
//
// 削除済みと凍結中のアカウントには届けない。凍結中は AsUser の呼び出しが
// 403 になるので、届けても handler は失敗して再試行を繰り返すだけになる。
func (d *Dispatcher) owner(u *model.User) (string, bool) {
	if u == nil || !u.IsLocal() || !u.IsPluginManaged() || u.IsDeleted || u.IsSuspended {
		return "", false
	}
	name := *u.ManagedByPlugin
	if _, ok := d.plugins[name]; !ok {
		return "", false
	}
	return name, true
}

// enqueue puts ev onto the plugin's queue. 失敗しても呼び出し元へは返さない
// (通知や投稿の処理を失敗させない)。
func (d *Dispatcher) enqueue(ctx context.Context, plugin string, ev Event) {
	// 文字列と time.Time だけの struct なので失敗しない。
	body, _ := json.Marshal(ev)
	// 呼び出し元の ctx が先に終わっても積み終える (リクエストの終了で取りこぼ
	// さない)。上限は enqueueTimeout で別に切る。
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), enqueueTimeout)
	defer cancel()
	if err := d.deps.Enqueuer.EnqueuePluginNotification(c, plugin, body); err != nil {
		slog.Warn("plugin notification: enqueue failed",
			"plugin", plugin, "type", ev.Type, "account", ev.AccountID, "id", ev.ID, "err", err)
	}
}
