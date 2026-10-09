package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/elythia-network/elythia/internal/core/pluginnotify"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/plugin"
)

// pluginNotifications implements plugin.Notifications for one plugin (#3469).
type pluginNotifications struct {
	handler plugin.NotificationHandler
}

func (n *pluginNotifications) Handle(h plugin.NotificationHandler) { n.handler = h }

var errPluginNotificationHandlerMissing = errors.New("通知: Definition.Notifications を宣言していますが、Notifications.Handle が呼ばれていません")

// registerPluginNotifications runs def.Notifications and wires the handler
// to the plugin's queue. queue ロールでだけ呼ぶ (Jobs と同じ)。
//
// **Handle を呼ばなかったら起動を止める。** 宣言があればキューには積むので、
// handler が無いまま動かすと、届いた通知が全部「処理者なし」で失敗する。
func (s *Server) registerPluginNotifications(def plugin.Definition, pctx plugin.Context) error {
	if s.queueServer == nil {
		return fmt.Errorf("通知の handler を登録できません (queue server が未配線です)")
	}
	n := &pluginNotifications{}
	if err := def.Notifications(pctx, n); err != nil {
		return err
	}
	if n.handler == nil {
		return errPluginNotificationHandlerMissing
	}
	s.queueServer.Handle(queue.PluginNotificationTaskType(def.Name), pluginNotificationJobHandler(n.handler))
	return nil
}

// pluginNotificationJobHandler adapts the plugin's handler to the queue.
//
// handler のエラーはそのまま返す。キューが再試行する
// (queue.EnqueuePluginNotification が回数と間隔を決めている)。plugin.ErrNoRetry
// を包んだものだけは再試行しない。
func pluginNotificationJobHandler(h plugin.NotificationHandler) driver.HandlerFunc {
	return func(ctx context.Context, t driver.Task) error {
		var ev pluginnotify.Event
		if err := json.Unmarshal(t.Payload(), &ev); err != nil {
			// 読めないものを積み直しても同じ。
			return fmt.Errorf("%w: 通知のジョブを読めません: %w", driver.ErrSkipRetry, err)
		}
		return pluginJobError(h(ctx, plugin.Notification{
			ID:             ev.ID,
			Type:           plugin.NotificationType(ev.Type),
			AccountID:      ev.AccountID,
			UserID:         ev.UserID,
			NoteID:         ev.NoteID,
			Reaction:       ev.Reaction,
			ChatMessageID:  ev.ChatMessageID,
			CreatedAt:      ev.CreatedAt,
			NoteVisibility: ev.NoteVisibility,
		}))
	}
}

// pluginNotificationPlugins returns the enabled plugins that declared a
// notification handler.
//
// **ロールを見ない。** 通知はどのプロセスでも作られるので、積む側
// (pluginnotify.Dispatcher) は web 専用のプロセスにも要る。handler を
// 呼ぶのは queue ロールだけ。
func pluginNotificationPlugins(plugins []plugin.Definition, settings map[string]map[string]any) []string {
	var names []string
	for _, def := range plugins {
		if def.Notifications == nil || !pluginEnabled(settings[def.Name]) {
			continue
		}
		names = append(names, def.Name)
	}
	return names
}

// newPluginNotificationDispatcher builds the dispatcher router wires into the
// notification hook and the chat service, or nil when no enabled plugin takes
// notifications.
//
// **queue client が無ければ nil。** *queue.Client の nil を interface に入れると
// nil 判定をすり抜け、最初の通知で nil 参照の panic になる。
func newPluginNotificationDispatcher(plugins []plugin.Definition, settings map[string]map[string]any, client *queue.Client, deps pluginnotify.Deps) *pluginnotify.Dispatcher {
	if client == nil {
		return nil
	}
	deps.Enqueuer = client
	return pluginnotify.New(pluginNotificationPlugins(plugins, settings), deps)
}
