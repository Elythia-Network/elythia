package plugintest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/plugin"
	"github.com/elythia-network/elythia/plugin/plugintest"
)

// Notifications で登録させた handler へ、Notify で届けられる (#3469)。
func TestNotify_DeliversToRegisteredHandler(t *testing.T) {
	var got []plugin.Notification
	boom := errors.New("try again")
	def := plugin.Definition{
		Name: "bot", APIVersion: plugin.APIVersion,
		Notifications: func(ctx plugin.Context, n plugin.Notifications) error {
			assert.Equal(t, "bot", ctx.Name())
			n.Handle(func(_ context.Context, ev plugin.Notification) error {
				got = append(got, ev)
				if ev.Type == plugin.NotificationChatMessage {
					return boom
				}
				return nil
			})
			return nil
		},
	}
	h := plugintest.New(t).WithName("bot")
	h.Notifications(def)
	acc, err := h.Context().Accounts().Create(context.Background(), "mybot")
	require.NoError(t, err)

	at := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	n := plugin.Notification{ID: "n1", Type: plugin.NotificationMention, AccountID: acc.ID, UserID: "u1", NoteID: "note1", CreatedAt: at}
	require.NoError(t, h.Notify(n))
	require.NoError(t, h.Notify(n), "同じ ID でもう一度届けられる (再試行の再現)")
	assert.ErrorIs(t, h.Notify(plugin.Notification{Type: plugin.NotificationChatMessage, AccountID: acc.ID, ChatMessageID: "m1"}), boom,
		"handler のエラーをそのまま返す")

	require.Len(t, got, 3)
	assert.Equal(t, n, got[0], "渡したものがそのまま届く")
	assert.Equal(t, got[0].ID, got[1].ID)
	assert.NotEmpty(t, got[2].ID)
}

// 宣言していないプラグインでは何もしない (呼んでも落ちない)。
func TestNotifications_NoDeclarationIsNoop(t *testing.T) {
	h := plugintest.New(t)
	h.Notifications(plugin.Definition{Name: "x", APIVersion: plugin.APIVersion, Jobs: func(plugin.Context, plugin.Jobs) error { return nil }})
}
