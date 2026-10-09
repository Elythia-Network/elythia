package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/pluginnotify"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/elythia-network/elythia/plugin"
)

func notificationsDef(name string, fn plugin.NotificationHandler) plugin.Definition {
	return plugin.Definition{
		Name: name, APIVersion: plugin.APIVersion,
		Notifications: func(_ plugin.Context, n plugin.Notifications) error {
			if fn != nil {
				n.Handle(fn)
			}
			return nil
		},
	}
}

// 通知の handler は **queue ロールでだけ**登録する (Jobs と同じ)。
func TestSetupPlugins_NotificationHandlerIsRoleGated(t *testing.T) {
	for _, tt := range []struct {
		role config.ProcessRole
		want bool
	}{
		{config.RoleBoth, true},
		{config.RoleQueue, true},
		{config.RoleServer, false},
	} {
		t.Run(string(tt.role), func(t *testing.T) {
			s, api := newPluginTestServer(tt.role)
			d := newFakeQueueDriver()
			s.queueServer = queue.NewServer(d)
			def := notificationsDef("demo", func(context.Context, plugin.Notification) error { return nil })
			require.NoError(t, s.setupPlugins(api, []plugin.Definition{def}, noopStorage))
			_, ok := d.server.handlers["plugin:demo:_notification"]
			assert.Equal(t, tt.want, ok)
		})
	}
}

// 宣言したのに Handle を呼ばなければ起動を止める。積まれた通知が全部
// 「処理者なし」で失敗するのを、起動してから知ることになるため。
func TestSetupPlugins_NotificationsWithoutHandleFails(t *testing.T) {
	s, api := newPluginTestServer(config.RoleQueue)
	s.queueServer = queue.NewServer(newFakeQueueDriver())
	err := s.setupPlugins(api, []plugin.Definition{notificationsDef("demo", nil)}, noopStorage)
	require.Error(t, err)
	assert.ErrorIs(t, err, errPluginNotificationHandlerMissing)
	assert.Contains(t, err.Error(), `plugin "demo"`)
}

func TestSetupPlugins_PropagatesNotificationsError(t *testing.T) {
	s, api := newPluginTestServer(config.RoleQueue)
	s.queueServer = queue.NewServer(newFakeQueueDriver())
	def := plugin.Definition{
		Name: "demo", APIVersion: plugin.APIVersion,
		Notifications: func(plugin.Context, plugin.Notifications) error { return errors.New("だめ") },
	}
	err := s.setupPlugins(api, []plugin.Definition{def}, noopStorage)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "通知の handler の登録に失敗しました")
}

func TestSetupPlugins_NotificationsNeedQueueServer(t *testing.T) {
	s, api := newPluginTestServer(config.RoleQueue)
	def := notificationsDef("demo", func(context.Context, plugin.Notification) error { return nil })
	err := s.setupPlugins(api, []plugin.Definition{def}, noopStorage)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "queue server が未配線")
}

// queue の job から plugin.Notification への写し方。handler のエラーはそのまま
// 返して再試行させる。読めない payload は再試行しない。
func TestPluginNotificationJobHandler(t *testing.T) {
	var got plugin.Notification
	boom := errors.New("down")
	var ret error
	h := pluginNotificationJobHandler(func(_ context.Context, n plugin.Notification) error {
		got = n
		return ret
	})
	at := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	body, err := json.Marshal(pluginnotify.Event{
		ID: "n1", Type: "reaction", AccountID: "acc", UserID: "alice", NoteID: "note1",
		Reaction: ":x@.:", ChatMessageID: "m1", CreatedAt: at, NoteVisibility: "followers",
	})
	require.NoError(t, err)
	task := driver.RawTask{TypeName: "plugin:demo:_notification", Body: body}

	require.NoError(t, h(context.Background(), task))
	assert.Equal(t, plugin.Notification{
		ID: "n1", Type: plugin.NotificationReaction, AccountID: "acc", UserID: "alice", NoteID: "note1",
		Reaction: ":x@.:", ChatMessageID: "m1", CreatedAt: at, NoteVisibility: "followers",
	}, got)

	ret = boom
	err = h(context.Background(), task)
	assert.ErrorIs(t, err, boom)
	assert.NotErrorIs(t, err, driver.ErrSkipRetry, "handler の失敗は再試行させる")

	ret = plugin.NoRetry(boom)
	err = h(context.Background(), task)
	assert.ErrorIs(t, err, driver.ErrSkipRetry, "plugin.NoRetry で包んだものは再試行しない")
	assert.ErrorIs(t, err, boom)

	err = h(context.Background(), driver.RawTask{Body: []byte("not json")})
	assert.ErrorIs(t, err, driver.ErrSkipRetry, "読めないものは再試行しない")
}

// handler を持つプラグインにはキューを作る (worker がそこを見る)。
func TestPluginJobQueueNames_IncludesNotifications(t *testing.T) {
	got := pluginJobQueueNames([]plugin.Definition{
		notificationsDef("notifonly", nil),
		{Name: "plain", Routes: func(plugin.Context, plugin.Router) error { return nil }},
		notificationsDef("disabled", nil),
	}, map[string]map[string]any{"disabled": {enabledKey: false}})
	assert.Equal(t, []string{"plugin:notifonly"}, got)
}

func TestPluginNotificationPlugins(t *testing.T) {
	got := pluginNotificationPlugins([]plugin.Definition{
		notificationsDef("a", nil),
		{Name: "jobs", Jobs: func(plugin.Context, plugin.Jobs) error { return nil }},
		notificationsDef("off", nil),
		notificationsDef("b", nil),
	}, map[string]map[string]any{"off": {enabledKey: false}})
	assert.Equal(t, []string{"a", "b"}, got)
}

// queue client が無ければ配線しない。*queue.Client の nil を interface に
// 入れると nil 判定をすり抜け、最初の通知で panic になる。
func TestNewPluginNotificationDispatcher(t *testing.T) {
	defs := []plugin.Definition{notificationsDef("a", nil)}
	assert.Nil(t, newPluginNotificationDispatcher(defs, nil, nil, pluginnotify.Deps{}))
	assert.Nil(t, newPluginNotificationDispatcher(nil, nil, queue.NewClient(newFakeQueueDriver()), pluginnotify.Deps{}),
		"受け取るプラグインが居なければ nil")
	assert.NotNil(t, newPluginNotificationDispatcher(defs, nil, queue.NewClient(newFakeQueueDriver()),
		pluginnotify.Deps{Users: testutil.NewMockUserRepository()}))
}

// プラグインのジョブも plugin.NoRetry で再試行を止められる。包んでいない
// エラーと成功はそのまま。
func TestPluginJobs_NoRetryMapsToSkipRetry(t *testing.T) {
	d := newFakeQueueDriver()
	j := &pluginJobs{name: "demo", server: queue.NewServer(d)}
	boom := errors.New("gone")
	var ret error
	j.Handle("sync", func(context.Context, json.RawMessage) error { return ret })
	require.NoError(t, j.err)
	h := d.server.handlers["plugin:demo:sync"]
	require.NotNil(t, h)
	task := driver.RawTask{TypeName: "plugin:demo:sync"}

	require.NoError(t, h(context.Background(), task))
	ret = boom
	err := h(context.Background(), task)
	assert.ErrorIs(t, err, boom)
	assert.NotErrorIs(t, err, driver.ErrSkipRetry)
	ret = fmt.Errorf("wrapped: %w", plugin.NoRetry(boom))
	err = h(context.Background(), task)
	assert.ErrorIs(t, err, driver.ErrSkipRetry)
	assert.ErrorIs(t, err, boom)
	assert.Nil(t, pluginJobError(nil))
	already := fmt.Errorf("%w: %w", driver.ErrSkipRetry, plugin.ErrNoRetry)
	assert.Same(t, already, pluginJobError(already), "包み済みのものは二重に包まない")
}
