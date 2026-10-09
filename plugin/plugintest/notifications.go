package plugintest

import (
	"context"
	"fmt"
	"time"

	"github.com/elythia-network/elythia/plugin"
)

// fakeNotifications records the handler the plugin registers (#3469).
type fakeNotifications struct{ h *Harness }

func (n *fakeNotifications) Handle(fn plugin.NotificationHandler) {
	n.h.mu.Lock()
	defer n.h.mu.Unlock()
	n.h.notificationHandler = fn
}

// Notifications runs the definition's [plugin.Definition.Notifications]
// callback, so [Harness.Notify] can deliver to the handler it registers.
//
// 本番と同じく、宣言したのに Handle を呼ばなければテストを落とす (本番は
// 起動に失敗する)。
func (h *Harness) Notifications(def plugin.Definition) {
	h.t.Helper()
	h.applyMigrations(def)
	h.noteJobs(def)

	if def.Notifications == nil {
		return
	}
	if err := def.Notifications(h.Context(), &fakeNotifications{h: h}); err != nil {
		h.t.Fatalf("plugintest: Notifications に失敗しました: %v", err)
	}
	h.mu.Lock()
	registered := h.notificationHandler != nil
	h.mu.Unlock()
	if !registered {
		h.t.Fatalf("plugintest: Definition.Notifications を宣言していますが、Notifications.Handle が呼ばれていません")
	}
}

// Notify delivers n to the handler registered through
// [Harness.Notifications] and returns what the handler returned.
//
// n.AccountID には、このプラグインが管理するアカウント (ctx.Accounts().Create
// で作ったものか、SeedAccount(プラグイン名, ...) で置いたもの) を渡すこと。
// **本番が届けない宛先はテストを落とす** — 他のプラグインのアカウント・
// 削除したアカウント・凍結中のアカウントには、本番では届かない。
// [Harness.WithAccounts] で差し替えているときは宛先を確かめない。
//
// ID が空なら連番を、CreatedAt がゼロなら現在時刻を入れる。同じ ID で 2 回
// 呼べば、再試行で同じ通知がもう一度届いた場合を試せる。
func (h *Harness) Notify(n plugin.Notification) error {
	h.t.Helper()
	fn, n, err := h.prepareNotify(n)
	if err != nil {
		h.t.Fatal(err)
	}
	return fn(context.Background(), n)
}

// prepareNotify は Notify から切り出した部分 (Handlers.lookup と同じ理由)。
func (h *Harness) prepareNotify(n plugin.Notification) (plugin.NotificationHandler, plugin.Notification, error) {
	h.mu.Lock()
	fn := h.notificationHandler
	custom := h.accounts != nil
	h.notifySeq++
	seq := h.notifySeq
	h.mu.Unlock()
	if fn == nil {
		return nil, n, fmt.Errorf("plugintest: Notifications.Handle が登録されていません (先に Harness.Notifications を呼ぶこと)")
	}
	if n.Type == "" {
		return nil, n, fmt.Errorf("plugintest: Notification.Type が空です")
	}
	if !custom {
		if err := h.deliverable(n.AccountID); err != nil {
			return nil, n, err
		}
	}
	if n.ID == "" {
		n.ID = fmt.Sprintf("notification-%d", seq)
	}
	if n.CreatedAt.IsZero() {
		n.CreatedAt = time.Now()
	}
	return fn, n, nil
}

// deliverable reports why the host would not deliver to accountID, if so.
func (h *Harness) deliverable(accountID string) error {
	h.accountStore.mu.Lock()
	defer h.accountStore.mu.Unlock()
	acc := h.accountStore.owned(h.name, accountID)
	if acc == nil {
		return fmt.Errorf("plugintest: %q はこのプラグイン (%s) が管理するアカウントではありません (本番では届きません)", accountID, h.name)
	}
	if acc.Suspended {
		return fmt.Errorf("plugintest: %q は凍結中です (本番では届きません)", accountID)
	}
	return nil
}
