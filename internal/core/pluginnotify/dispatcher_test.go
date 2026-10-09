package pluginnotify

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

type enqueued struct {
	plugin string
	ev     Event
	ctxErr error
	hasDL  bool
}

type fakeEnqueuer struct {
	calls []enqueued
	err   error
}

func (f *fakeEnqueuer) EnqueuePluginNotification(ctx context.Context, plugin string, body []byte) error {
	var ev Event
	if err := json.Unmarshal(body, &ev); err != nil {
		return err
	}
	_, hasDL := ctx.Deadline()
	f.calls = append(f.calls, enqueued{plugin: plugin, ev: ev, ctxErr: ctx.Err(), hasDL: hasDL})
	return f.err
}

// fakeUsers resolves users and profiles. 名前に "remote" を含む ID は
// remote.example のリモート利用者として、それ以外は普通のローカル利用者として
// 自動で返す (notifier を一人ずつ登録しなくてよいように)。"ghost" は見つからない。
type fakeUsers struct {
	users      map[string]*model.User
	profiles   map[string]*model.UserProfile
	profileErr error
	lookups    *[]string
}

func (f fakeUsers) FindByID(id string) (*model.User, error) {
	if f.lookups != nil {
		*f.lookups = append(*f.lookups, id)
	}
	if u, ok := f.users[id]; ok {
		return u, nil
	}
	switch {
	case strings.Contains(id, "vanished"):
		return nil, nil
	case strings.Contains(id, "ghost"):
		// 本物の repository と同じ not-found を返す。
		return nil, repository.ErrNotFound
	case strings.Contains(id, "remote"):
		host := "remote.example"
		return &model.User{ID: id, Host: &host}, nil
	}
	return &model.User{ID: id}, nil
}

func (f fakeUsers) FindProfileByUserID(id string) (*model.UserProfile, error) {
	if f.profileErr != nil {
		return nil, f.profileErr
	}
	p, ok := f.profiles[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	return p, nil
}

func users(us ...*model.User) fakeUsers {
	m := map[string]*model.User{}
	for _, u := range us {
		m[u.ID] = u
	}
	return fakeUsers{users: m, profiles: map[string]*model.UserProfile{}}
}

// fakeRelations answers both mute and block checks from a set of
// "subject>object" pairs.
type fakeRelations struct {
	pairs map[string]bool
	err   error
}

func (f fakeRelations) IsMuted(muter, mutee string) (bool, error) {
	return f.pairs["mute:"+muter+">"+mutee], f.err
}

type fakeBlocks fakeRelations

func (f fakeBlocks) Exists(blocker, blockee string) (bool, error) {
	return f.pairs["block:"+blocker+">"+blockee], f.err
}

func managed(id, plugin string) *model.User {
	return &model.User{ID: id, ManagedByPlugin: &plugin}
}

func strp(s string) *string { return &s }

func TestNew_NilWhenNothingToDeliver(t *testing.T) {
	assert.Nil(t, New(nil, Deps{Enqueuer: &fakeEnqueuer{}, Users: users()}), "受け取るプラグインが居なければ nil")
	assert.Nil(t, New([]string{"bot"}, Deps{Users: users()}), "積む先が無ければ nil")
	assert.Nil(t, New([]string{"bot"}, Deps{Enqueuer: &fakeEnqueuer{}}), "相手を確かめられなければ nil")
	assert.NotNil(t, New([]string{"bot"}, Deps{Enqueuer: &fakeEnqueuer{}, Users: users()}))
}

func TestNotificationCreated_DeliversDeliveredTypesToOwner(t *testing.T) {
	enq := &fakeEnqueuer{}
	d := New([]string{"bot-a", "bot-b"}, Deps{Enqueuer: enq, Users: users()})
	at := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	recipient := managed("acc-a", "bot-a")

	for _, typ := range []notification.Type{
		notification.TypeMention, notification.TypeReply, notification.TypeQuote,
		notification.TypeFollow, notification.TypeReceiveFollowReq, notification.TypeReaction,
	} {
		d.NotificationCreated(context.Background(), recipient, &notification.Notification{
			ID: "n-" + string(typ), Type: typ, NotifierID: "alice", NoteID: "note1", NoteVisibility: "home", Reaction: ":x@.:", CreatedAt: at,
		})
	}
	require.Len(t, enq.calls, 6)
	for _, c := range enq.calls {
		assert.Equal(t, "bot-a", c.plugin)
		assert.Equal(t, "acc-a", c.ev.AccountID)
		assert.Equal(t, "alice", c.ev.UserID)
		assert.Equal(t, "note1", c.ev.NoteID)
		assert.Equal(t, ":x@.:", c.ev.Reaction)
		assert.Equal(t, "home", c.ev.NoteVisibility)
		assert.Equal(t, "n-"+c.ev.Type, c.ev.ID)
		assert.True(t, at.Equal(c.ev.CreatedAt))
		assert.True(t, c.hasDL, "enqueue に期限を付ける")
	}
}

// 届けない型の通知 (note / renote / 実績など) は積まない。
func TestNotificationCreated_SkipsOtherTypes(t *testing.T) {
	enq := &fakeEnqueuer{}
	d := New([]string{"bot-a"}, Deps{Enqueuer: enq, Users: users()})
	for _, typ := range []notification.Type{
		notification.TypeNote, notification.TypeRenote, notification.TypeAchievementEarned,
		notification.TypeFollowRequestAccept, notification.TypePollEnded, notification.TypeLogin,
	} {
		d.NotificationCreated(context.Background(), managed("acc-a", "bot-a"), &notification.Notification{ID: "n", Type: typ})
	}
	d.NotificationCreated(context.Background(), managed("acc-a", "bot-a"), nil)
	assert.Empty(t, enq.calls)
}

// 宛先がそのプラグインの管理するアカウントでなければ積まない。
func TestNotificationCreated_OnlyManagedRecipientsOfReceivingPlugins(t *testing.T) {
	remote := managed("remote", "bot-a")
	remote.Host = strp("remote.example")
	deleted := managed("deleted", "bot-a")
	deleted.IsDeleted = true
	suspended := managed("suspended", "bot-a")
	suspended.IsSuspended = true

	for name, u := range map[string]*model.User{
		"普通の利用者": {ID: "normal"},
		"受け取りを宣言していないプラグインのアカウント": managed("acc-c", "bot-c"),
		"リモート": remote,
		"削除済み": deleted,
		"凍結中":  suspended,
		"nil":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			enq := &fakeEnqueuer{}
			d := New([]string{"bot-a", "bot-b"}, Deps{Enqueuer: enq, Users: users()})
			d.NotificationCreated(context.Background(), u, &notification.Notification{ID: "n", Type: notification.TypeMention})
			assert.Empty(t, enq.calls)
		})
	}

	// 他のプラグインのアカウント宛ては、そちらのプラグインへ積む (取り違えない)。
	enq := &fakeEnqueuer{}
	d := New([]string{"bot-a", "bot-b"}, Deps{Enqueuer: enq, Users: users()})
	d.NotificationCreated(context.Background(), managed("acc-b", "bot-b"), &notification.Notification{ID: "n", Type: notification.TypeMention})
	require.Len(t, enq.calls, 1)
	assert.Equal(t, "bot-b", enq.calls[0].plugin)
}

// ブロック・ミュートした相手からの通知は届けない。ブロックでは本体は通知を
// 作るし、ミュートは Hook が判定に失敗すると通すため。
func TestNotificationCreated_SkipsBlockedNotifier(t *testing.T) {
	enq := &fakeEnqueuer{}
	pairs := map[string]bool{"block:acc-a>blocked": true, "mute:acc-a>muted": true}
	d := New([]string{"bot-a"}, Deps{Enqueuer: enq, Users: users(), Blocks: fakeBlocks{pairs: pairs}, Mutes: fakeRelations{pairs: pairs}})
	recipient := managed("acc-a", "bot-a")

	d.NotificationCreated(context.Background(), recipient, &notification.Notification{ID: "1", Type: notification.TypeMention, NotifierID: "blocked"})
	assert.Empty(t, enq.calls)
	d.NotificationCreated(context.Background(), recipient, &notification.Notification{ID: "2", Type: notification.TypeMention, NotifierID: "muted"})
	assert.Empty(t, enq.calls, "Hook がミュートを判定できずに通したものも落とす")
	d.NotificationCreated(context.Background(), recipient, &notification.Notification{ID: "3", Type: notification.TypeMention, NotifierID: "alice"})
	require.Len(t, enq.calls, 1)
	d.NotificationCreated(context.Background(), recipient, &notification.Notification{ID: "4", Type: notification.TypeMention})
	assert.Len(t, enq.calls, 2, "notifier の無い通知は相手を見ない")
}

// 判定できないときは届けない。
func TestNotificationCreated_BlockCheckErrorFailsClosed(t *testing.T) {
	enq := &fakeEnqueuer{}
	d := New([]string{"bot-a"}, Deps{Enqueuer: enq, Users: users(), Blocks: fakeBlocks{err: errors.New("db down")}})
	d.NotificationCreated(context.Background(), managed("acc-a", "bot-a"), &notification.Notification{ID: "1", Type: notification.TypeMention, NotifierID: "x"})
	assert.Empty(t, enq.calls)
}

// 積めなくても呼び出し元へは返さない (通知の作成を失敗させない)。呼び出し元の
// ctx が終わっていても積む。
func TestNotificationCreated_EnqueueIgnoresCallerCancellationAndErrors(t *testing.T) {
	enq := &fakeEnqueuer{err: errors.New("redis down")}
	d := New([]string{"bot-a"}, Deps{Enqueuer: enq, Users: users()})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.NotPanics(t, func() {
		d.NotificationCreated(ctx, managed("acc-a", "bot-a"), &notification.Notification{ID: "1", Type: notification.TypeFollow})
	})
	require.Len(t, enq.calls, 1)
	assert.NoError(t, enq.calls[0].ctxErr, "呼び出し元の cancel を引き継がない")
}

func TestDirectMessageCreated_DeliversToOwner(t *testing.T) {
	enq := &fakeEnqueuer{}
	d := New([]string{"bot-a"}, Deps{Enqueuer: enq, Users: users(managed("acc-a", "bot-a"))})
	at := time.Date(2026, 10, 9, 1, 0, 0, 0, time.UTC)
	d.now = func() time.Time { return at }

	d.DirectMessageCreated(context.Background(), &model.ChatMessage{ID: "m1", FromUserID: "alice", ToUserID: strp("acc-a")}, nil)
	require.Len(t, enq.calls, 1)
	assert.Equal(t, Event{
		ID: "m1", Type: TypeChatMessage, AccountID: "acc-a", UserID: "alice", ChatMessageID: "m1", CreatedAt: at,
	}, enq.calls[0].ev)
	assert.Equal(t, "bot-a", enq.calls[0].plugin)
}

func TestDirectMessageCreated_Skips(t *testing.T) {
	pairs := map[string]bool{"mute:acc-a>muted": true, "block:acc-a>blocked": true}
	us := users(managed("acc-a", "bot-a"), managed("acc-c", "bot-c"))
	cases := map[string]*model.ChatMessage{
		"nil":       nil,
		"ルームのメッセージ": {ID: "m", FromUserID: "alice", ToRoomID: strp("room")},
		"自分宛て":      {ID: "m", FromUserID: "acc-a", ToUserID: strp("acc-a")},
		"宛先が見つからない": {ID: "m", FromUserID: "alice", ToUserID: strp("ghost")},
		"普通の利用者宛て":  {ID: "m", FromUserID: "alice", ToUserID: strp("normal")},
		"宣言していないプラグイン宛て": {ID: "m", FromUserID: "alice", ToUserID: strp("acc-c")},
		"ミュートした相手から":     {ID: "m", FromUserID: "muted", ToUserID: strp("acc-a")},
		"ブロックした相手から":     {ID: "m", FromUserID: "blocked", ToUserID: strp("acc-a")},
	}
	for name, msg := range cases {
		t.Run(name, func(t *testing.T) {
			enq := &fakeEnqueuer{}
			d := New([]string{"bot-a"}, Deps{Enqueuer: enq, Users: us, Mutes: fakeRelations{pairs: pairs}, Blocks: fakeBlocks{pairs: pairs}})
			d.DirectMessageCreated(context.Background(), msg, nil)
			assert.Empty(t, enq.calls)
		})
	}

}

// ミュートを判定できないときは届けない。
func TestDirectMessageCreated_MuteCheckErrorFailsClosed(t *testing.T) {
	enq := &fakeEnqueuer{}
	d := New([]string{"bot-a"}, Deps{
		Enqueuer: enq, Users: users(managed("acc-a", "bot-a")),
		Mutes: fakeRelations{err: errors.New("db down")},
	})
	d.DirectMessageCreated(context.Background(), &model.ChatMessage{ID: "m", FromUserID: "alice", ToUserID: strp("acc-a")}, nil)
	assert.Empty(t, enq.calls)
}

// i/notifications が読むときに落とす相手 (凍結中・インスタンスをミュートした
// リモート) からは届けない。判定できないときも届けない。通知とチャットの両方。
func TestDelivery_SkipsSuspendedAndInstanceMutedNotifiers(t *testing.T) {
	recipient := managed("acc-a", "bot-a")
	frozen := &model.User{ID: "frozen", IsSuspended: true}
	newDispatcher := func(enq *fakeEnqueuer, mutedHosts string, profileErr error) *Dispatcher {
		us := users(recipient, frozen)
		us.profiles["acc-a"] = &model.UserProfile{UserID: "acc-a"}
		if mutedHosts != "" {
			us.profiles["acc-a"].MutedInstances = []byte(mutedHosts)
		}
		us.profileErr = profileErr
		return New([]string{"bot-a"}, Deps{Enqueuer: enq, Users: us})
	}
	deliver := func(d *Dispatcher, from string) {
		d.NotificationCreated(context.Background(), recipient, &notification.Notification{ID: "n", Type: notification.TypeMention, NotifierID: from})
		d.DirectMessageCreated(context.Background(), &model.ChatMessage{ID: "m", FromUserID: from, ToUserID: strp("acc-a")}, recipient)
	}

	for name, tc := range map[string]struct {
		from       string
		mutedHosts string
		profileErr error
	}{
		"凍結中の相手":          {from: "frozen"},
		"ミュートしたインスタンスの相手": {from: "remote-bob", mutedHosts: `["other.example","remote.example"]`},
		"相手が見つからない":       {from: "ghost"},
		"相手が nil で返る":     {from: "vanished"},
		"受信設定を読めない":       {from: "remote-bob", profileErr: errors.New("db down")},
		"受信設定が壊れている":      {from: "remote-bob", mutedHosts: `{not json`},
	} {
		t.Run(name, func(t *testing.T) {
			enq := &fakeEnqueuer{}
			deliver(newDispatcher(enq, tc.mutedHosts, tc.profileErr), tc.from)
			assert.Empty(t, enq.calls)
		})
	}

	// 届くもの: ミュートしていないインスタンスのリモート、ローカルの相手
	// (ローカルの相手には受信設定を読まない)。
	enq := &fakeEnqueuer{}
	deliver(newDispatcher(enq, `["other.example"]`, nil), "remote-bob")
	assert.Len(t, enq.calls, 2)
	enq = &fakeEnqueuer{}
	deliver(newDispatcher(enq, "", nil), "remote-bob")
	assert.Len(t, enq.calls, 2, "インスタンスのミュートが無ければリモートからも届く")
	enq = &fakeEnqueuer{}
	deliver(newDispatcher(enq, "", errors.New("db down")), "alice")
	assert.Len(t, enq.calls, 2, "ローカルの相手ならインスタンスのミュートは関係ない")
}

// 通知のミュートの判定に失敗したら届けない (Hook は通すので、ここで止める)。
func TestNotificationCreated_MuteCheckErrorFailsClosed(t *testing.T) {
	enq := &fakeEnqueuer{}
	d := New([]string{"bot-a"}, Deps{Enqueuer: enq, Users: users(), Mutes: fakeRelations{err: errors.New("db down")}})
	d.NotificationCreated(context.Background(), managed("acc-a", "bot-a"), &notification.Notification{ID: "1", Type: notification.TypeMention, NotifierID: "alice"})
	assert.Empty(t, enq.calls)
}

// chat.Service が引いた宛先を渡せば、引き直さない (#3469 review)。ID が合わない
// ものは信用せず引き直す。
func TestDirectMessageCreated_UsesGivenRecipient(t *testing.T) {
	var lookups []string
	us := users(managed("acc-a", "bot-a"))
	us.lookups = &lookups
	enq := &fakeEnqueuer{}
	d := New([]string{"bot-a"}, Deps{Enqueuer: enq, Users: us})

	normal := &model.User{ID: "normal"}
	d.DirectMessageCreated(context.Background(), &model.ChatMessage{ID: "m", FromUserID: "alice", ToUserID: strp("normal")}, normal)
	assert.Empty(t, lookups, "管理するアカウントでない宛先には問い合わせない")
	assert.Empty(t, enq.calls)

	d.DirectMessageCreated(context.Background(), &model.ChatMessage{ID: "m", FromUserID: "alice", ToUserID: strp("acc-a")}, managed("acc-a", "bot-a"))
	assert.Equal(t, []string{"alice"}, lookups, "宛先は引き直さず、相手だけを引く")
	assert.Len(t, enq.calls, 1)

	lookups = nil
	d.DirectMessageCreated(context.Background(), &model.ChatMessage{ID: "m", FromUserID: "alice", ToUserID: strp("acc-a")}, normal)
	assert.Equal(t, []string{"acc-a", "alice"}, lookups, "ID が合わない宛先は引き直す")
	assert.Len(t, enq.calls, 2)
}

// 相手やプロフィールが見つからないのは障害ではないので、届けないがログも
// 出さない。本物の DB エラーは Warn を出す (#3469 review)。
func TestDelivery_NotFoundIsQuietButDBErrorWarns(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	recipient := managed("acc-a", "bot-a")
	enq := &fakeEnqueuer{}
	d := New([]string{"bot-a"}, Deps{Enqueuer: enq, Users: users(recipient)})
	d.NotificationCreated(context.Background(), recipient, &notification.Notification{ID: "1", Type: notification.TypeMention, NotifierID: "ghost"})
	d.DirectMessageCreated(context.Background(), &model.ChatMessage{ID: "m", FromUserID: "remote-bob", ToUserID: strp("acc-a")}, recipient)
	assert.Empty(t, enq.calls, "相手やプロフィールが見つからなければ届けない")
	assert.Empty(t, buf.String(), "見つからないだけでは Warn を出さない")

	us := users(recipient)
	us.profileErr = errors.New("connection refused")
	d = New([]string{"bot-a"}, Deps{Enqueuer: enq, Users: us})
	d.DirectMessageCreated(context.Background(), &model.ChatMessage{ID: "m", FromUserID: "remote-bob", ToUserID: strp("acc-a")}, recipient)
	assert.Empty(t, enq.calls, "DB エラーでも届けない")
	assert.Contains(t, buf.String(), "relation check failed")
}
