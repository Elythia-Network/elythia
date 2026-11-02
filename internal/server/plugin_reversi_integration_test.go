package server

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/plugin"
)

// プラグインが管理するアカウントがリバーシを遊べること (#3942)。招待と対局の進みが
// 通知で届き、REST の入口から手を打って終局まで進められる。組み立てたサーバーで、
// 配線 (router が Dispatcher を招待と対局の両方へ渡すこと) ごと確かめる。

// waitGameJob waits for a reversiGame delivery of event in gameID for account.
func (h *pluginNotificationsHarness) waitGameJob(pluginName, account, gameID, event string) notificationJob {
	h.t.Helper()
	var found notificationJob
	require.Eventually(h.t, func() bool {
		for _, j := range h.rec.notificationJobs(h.t, pluginName) {
			if j.ev.Type == "reversiGame" && j.ev.AccountID == account && j.ev.GameID == gameID && j.ev.GameEvent == event {
				found = j
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "%s の reversiGame %s (%s 宛て) が積まれない", pluginName, event, account)
	return found
}

func cleanupReversiRows(t *testing.T, h *pluginNotificationsHarness, usernames ...string) {
	t.Helper()
	clean := func() {
		var ids []string
		h.db.Table(`"user"`).Where(`"usernameLower" IN ? AND "host" IS NULL`, usernames).Pluck("id", &ids)
		if len(ids) > 0 {
			h.db.Exec(`DELETE FROM "reversi_game" WHERE "user1Id" IN ? OR "user2Id" IN ?`, ids, ids)
		}
	}
	clean()
	t.Cleanup(clean)
}

func TestPluginReversi_BotPlaysThroughNotificationsAndREST_RealDB(t *testing.T) {
	h := newPluginNotificationsHarness(t)
	users := []string{"pr_bot_a", "pr_alice", "pr_carol"}
	cleanupReversiRows(t, h, users...)
	cleanupPluginNotificationRows(t, h.db, users...)
	createLocalUser(t, h.db, "pralice", "pr_alice", "pralicetoken0016", "alice-pass", false)
	createLocalUser(t, h.db, "prcarol", "pr_carol", "prcaroltoken0016", "carol-pass", false)
	const alice, carol = "pralicetoken0016", "prcaroltoken0016"
	ctx := context.Background()

	bot, err := h.ctxA.Accounts().Create(ctx, "pr_bot_a")
	require.NoError(t, err)

	// 1. 招待が通知で届く (対局の ID 付き)
	h.post(alice, "reversi/match", map[string]any{"userId": bot.ID})
	invited := h.waitJob("bot-a", "reversiInvited", bot.ID)
	assert.Equal(t, "pralice", invited.ev.UserID)
	require.NotEmpty(t, invited.ev.GameID)
	gameID := invited.ev.GameID
	require.NoError(t, h.run(invited))
	got := h.receivedBy("bot-a")
	require.NotEmpty(t, got)
	last := got[len(got)-1]
	assert.Equal(t, plugin.NotificationReversiInvited, last.Type)
	assert.Equal(t, gameID, last.GameID)

	// 2. bot が受けて、両者が REST で準備すると始まる
	asBot(t, h.ctxA, bot.ID, "reversi/match", map[string]any{"userId": "pralice"})
	asBot(t, h.ctxA, bot.ID, "reversi/ready", map[string]any{"gameId": gameID, "ready": true})
	h.waitGameJob("bot-a", bot.ID, gameID, "changeReadyStates")
	h.post(alice, "reversi/ready", map[string]any{"gameId": gameID, "ready": true})
	started := h.waitGameJob("bot-a", bot.ID, gameID, "started")
	assert.Equal(t, "pralice", started.ev.UserID, "UserID は対局の相手")
	require.NoError(t, h.run(started))
	got = h.receivedBy("bot-a")
	last = got[len(got)-1]
	assert.Equal(t, plugin.NotificationReversiGame, last.Type)
	assert.Equal(t, gameID, last.GameID)
	assert.Equal(t, plugin.ReversiGameStarted, last.GameEvent)

	// 3. 手番の側が置く。bot の番なら bot が REST で置き、そうでなければ alice が置く。
	// どちらでも bot へ log が届く。2 手進めて、bot が必ず 1 手は置く
	for range 2 {
		var game model.ReversiGame
		require.NoError(t, h.db.Where("id = ?", gameID).First(&game).Error)
		engine, err := corereversi.EngineFromGame(&game)
		require.NoError(t, err)
		require.NotNil(t, engine.Turn)
		places := engine.GetPuttablePlaces(*engine.Turn)
		require.NotEmpty(t, places)
		before := len(h.rec.notificationJobs(t, "bot-a"))
		params := map[string]any{"gameId": gameID, "pos": places[0]}
		blackIsUser1 := game.Black != nil && *game.Black == 1
		botIsBlack := (game.User1ID == bot.ID) == blackIsUser1
		if *engine.Turn == botIsBlack {
			asBot(t, h.ctxA, bot.ID, "reversi/put-stone", params)
		} else {
			h.post(alice, "reversi/put-stone", params)
		}
		require.Eventually(t, func() bool {
			for _, j := range h.rec.notificationJobs(t, "bot-a")[before:] {
				if j.ev.GameEvent == "log" && j.ev.GameID == gameID {
					return true
				}
			}
			return false
		}, 5*time.Second, 10*time.Millisecond, "手を置いても log が届かない")
	}

	// 時間切れの申告も REST で受ける (時間が残っているので何も起きない)
	asBot(t, h.ctxA, bot.ID, "reversi/claim-time-is-up", map[string]any{"gameId": gameID})

	// 4. 終局 (alice の投了) も届く
	h.post(alice, "reversi/surrender", map[string]any{"gameId": gameID})
	h.waitGameJob("bot-a", bot.ID, gameID, "ended")

	// 5. 管理アカウントでない利用者どうしの対局では何も積まない
	beforeOthers := h.rec.allNotificationJobs()
	h.post(alice, "reversi/match", map[string]any{"userId": "prcarol"})
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, beforeOthers, h.rec.allNotificationJobs())

	// 6. bot がミュートした相手からの招待は届かない
	asBot(t, h.ctxA, bot.ID, "mute/create", map[string]any{"userId": "prcarol"})
	beforeMuted := len(h.rec.notificationJobs(t, "bot-a"))
	h.post(carol, "reversi/match", map[string]any{"userId": bot.ID})
	time.Sleep(100 * time.Millisecond)
	assert.Len(t, h.rec.notificationJobs(t, "bot-a"), beforeMuted, "ミュートした相手からの招待は届けない")
}

// 通知を受け取るプラグインが居る組み立てでは、招待 (API と連合の受信) と対局の観測者を
// 3 つとも配線する。連合の招待は上のテストの経路に無いので、組み立てた部品の欄で見る。
// 居ないときに配線しないことは TestBuiltHandlersHaveNoNilDependencies の許可リストが見る。
func TestPluginReversi_WiresObserversWhenAPluginTakesNotifications_RealDB(t *testing.T) {
	h := newPluginNotificationsHarness(t)
	parts := reflect.ValueOf(h.srv.builtRouteParts)
	handler := parts.FieldByName("reversi").Elem()
	for name, v := range map[string]reflect.Value{
		"reversi.inviteObs":                    handler.FieldByName("inviteObs"),
		"reversi.svc.observer":                 handler.FieldByName("svc").Elem().FieldByName("observer"),
		"federationProcessor.reversiInviteObs": parts.FieldByName("federationProcessor").Elem().FieldByName("reversiInviteObs"),
	} {
		require.True(t, v.IsValid(), "%s の欄が無い (rename した?)", name)
		assert.False(t, v.IsNil(), "%s が配線されていない", name)
	}
}
