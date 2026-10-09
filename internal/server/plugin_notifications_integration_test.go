package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/core/pluginnotify"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/plugin"
)

// pluginJobRecorder records what reaches the plugin queues and the handlers
// registered for them, while passing everything through to the real driver.
//
// **worker は動かさない。** 積まれたジョブは handler を直接呼んで処理する。
// 確かめたいのは「何を、どのキューへ、どのオプションで積んだか」と「handler
// が何を受け取るか」で、mkq の転送そのものは queue パッケージのテストの範囲。
type pluginJobRecorder struct {
	mu       sync.Mutex
	calls    []fakeQueueCall
	handlers map[string]driver.HandlerFunc
	// enqueueErr を入れると、プラグインのキューへの enqueue を失敗させる。
	enqueueErr error
}

type pluginRecordingDriver struct {
	driver.Driver
	rec *pluginJobRecorder
}

func (d *pluginRecordingDriver) Client() driver.Client {
	return &pluginRecordingClient{Client: d.Driver.Client(), rec: d.rec}
}

func (d *pluginRecordingDriver) Server() driver.Server {
	return &pluginRecordingServer{Server: d.Driver.Server(), rec: d.rec}
}

type pluginRecordingClient struct {
	driver.Client
	rec *pluginJobRecorder
}

func (c *pluginRecordingClient) Enqueue(ctx context.Context, taskType string, payload []byte, opts ...driver.EnqueueOption) error {
	if !strings.HasPrefix(taskType, queue.PluginQueuePrefix) {
		return c.Client.Enqueue(ctx, taskType, payload, opts...)
	}
	// プラグインのキューへは実際には積まない (miniredis は mkq の Lua が使う
	// cmsgpack を持たないので、積もうとすると必ず失敗する)。
	c.rec.mu.Lock()
	defer c.rec.mu.Unlock()
	if c.rec.enqueueErr != nil {
		return c.rec.enqueueErr
	}
	c.rec.calls = append(c.rec.calls, fakeQueueCall{taskType: taskType, payload: payload, opts: opts})
	return nil
}

type pluginRecordingServer struct {
	driver.Server
	rec *pluginJobRecorder
}

func (s *pluginRecordingServer) Handle(taskType string, h driver.HandlerFunc) {
	s.rec.mu.Lock()
	s.rec.handlers[taskType] = h
	s.rec.mu.Unlock()
	s.Server.Handle(taskType, h)
}

// notificationJob is one recorded notification delivery.
type notificationJob struct {
	call fakeQueueCall
	ev   pluginnotify.Event
}

// notificationJobs returns the deliveries recorded for pluginName.
func (r *pluginJobRecorder) notificationJobs(t *testing.T, pluginName string) []notificationJob {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []notificationJob
	for _, c := range r.calls {
		if c.taskType != queue.PluginNotificationTaskType(pluginName) {
			continue
		}
		var ev pluginnotify.Event
		require.NoError(t, json.Unmarshal(c.payload, &ev))
		out = append(out, notificationJob{call: c, ev: ev})
	}
	return out
}

func (r *pluginJobRecorder) allNotificationJobs() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if strings.HasSuffix(c.taskType, ":"+queue.PluginNotificationJobName) {
			n++
		}
	}
	return n
}

func (r *pluginJobRecorder) handler(taskType string) driver.HandlerFunc {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.handlers[taskType]
}

// pluginNotificationsHarness is a full server (web + queue role) with three
// plugins, backed by the real database (#3469).
//
//   - bot-a / bot-b: 通知の handler を宣言している
//   - bot-c: 宣言していない (アカウントだけ持つ)
type pluginNotificationsHarness struct {
	*pluginAccountsHarness
	ctxC     plugin.Context
	rec      *pluginJobRecorder
	mu       sync.Mutex
	received map[string][]plugin.Notification
	failA    error
}

func newPluginNotificationsHarness(t *testing.T) *pluginNotificationsHarness {
	t.Helper()
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	t.Setenv(config.EnvOnlyServer, "")
	t.Setenv(config.EnvOnlyQueue, "")
	t.Chdir(t.TempDir())

	mr := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, redisClient.Close()) })
	redisClients := &cache.RedisClients{
		Default: redisClient, Pubsub: redisClient, JobQueue: redisClient,
		Timelines: redisClient, Reactions: redisClient,
	}
	redisPort, err := strconv.Atoi(mr.Port())
	require.NoError(t, err)
	redisOptions := config.RedisOptions{Host: mr.Host(), Port: redisPort}
	cfg := &config.Config{
		URL: "http://example.test", Host: "example.test", Hostname: "example.test",
		Scheme: "http", WsScheme: "ws", ID: "aidx", TestMode: true,
		JobQueueDriver: "mkq", MediaProxySecret: []byte("test-secret"),
		Redis: redisOptions, RedisForPubsub: redisOptions, RedisForJobQueue: redisOptions,
		RedisForTimelines: redisOptions, RedisForReactions: redisOptions,
	}

	h := &pluginNotificationsHarness{
		pluginAccountsHarness: &pluginAccountsHarness{t: t, db: serverIntegrationDB},
		rec:                   &pluginJobRecorder{handlers: map[string]driver.HandlerFunc{}},
		received:              map[string][]plugin.Notification{},
	}
	handlerFor := func(name string) func(plugin.Context, plugin.Notifications) error {
		return func(_ plugin.Context, n plugin.Notifications) error {
			n.Handle(func(_ context.Context, ev plugin.Notification) error {
				h.mu.Lock()
				defer h.mu.Unlock()
				h.received[name] = append(h.received[name], ev)
				if name == "bot-a" {
					return h.failA
				}
				return nil
			})
			return nil
		}
	}
	capture := func(dst *plugin.Context) func(plugin.Context, plugin.Router) error {
		return func(ctx plugin.Context, _ plugin.Router) error {
			*dst = ctx
			return nil
		}
	}
	defs := []plugin.Definition{
		{Name: "bot-a", APIVersion: plugin.APIVersion, Routes: capture(&h.ctxA), Notifications: handlerFor("bot-a")},
		{Name: "bot-b", APIVersion: plugin.APIVersion, Routes: capture(&h.ctxB), Notifications: handlerFor("bot-b")},
		{Name: "bot-c", APIVersion: plugin.APIVersion, Routes: capture(&h.ctxC)},
	}
	restoreProcessGlobals(t)
	srv, err := newServer(cfg, h.db, redisClients, defs, noopStorage, func(s *Server) {
		rd := &pluginRecordingDriver{Driver: s.queueDriver, rec: h.rec}
		s.queueClient = queue.NewClient(rd)
		s.queueServer = queue.NewServer(rd)
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	h.srv = srv
	return h
}

// waitJob waits until a delivery of typ for account reaches pluginName's queue.
func (h *pluginNotificationsHarness) waitJob(pluginName, typ, account string) notificationJob {
	h.t.Helper()
	var found notificationJob
	require.Eventually(h.t, func() bool {
		for _, j := range h.rec.notificationJobs(h.t, pluginName) {
			if j.ev.Type == typ && j.ev.AccountID == account {
				found = j
				return true
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond, "%s の %s (%s 宛て) が積まれない", pluginName, typ, account)
	return found
}

// run hands a recorded job to the handler the queue role registered.
func (h *pluginNotificationsHarness) run(j notificationJob) error {
	h.t.Helper()
	fn := h.rec.handler(j.call.taskType)
	require.NotNil(h.t, fn, "%s の handler が登録されていない", j.call.taskType)
	return fn(context.Background(), driver.RawTask{TypeName: j.call.taskType, Body: j.call.payload})
}

func (h *pluginNotificationsHarness) receivedBy(name string) []plugin.Notification {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]plugin.Notification(nil), h.received[name]...)
}

// post calls an endpoint as an ordinary user and returns the decoded body.
func (h *pluginNotificationsHarness) post(token, endpoint string, body map[string]any) map[string]any {
	h.t.Helper()
	rec := h.do(http.MethodPost, "/api/"+endpoint, token, body)
	require.Less(h.t, rec.Code, 300, "%s: %s", endpoint, rec.Body.String())
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return out
}

// asBot calls an endpoint as a managed account through the plugin API.
func asBot(t *testing.T, ctx plugin.Context, botID, endpoint string, params map[string]any) map[string]any {
	t.Helper()
	raw, err := ctx.API().AsUser(botID).Call(context.Background(), endpoint, params)
	require.NoError(t, err, endpoint)
	out := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func createdNoteID(t *testing.T, res map[string]any) string {
	t.Helper()
	note, ok := res["createdNote"].(map[string]any)
	require.True(t, ok, "createdNote が無い: %v", res)
	id, _ := note["id"].(string)
	require.NotEmpty(t, id)
	return id
}

func cleanupPluginNotificationRows(t *testing.T, db *gorm.DB, usernames ...string) {
	t.Helper()
	clean := func() {
		var ids []string
		db.Table(`"user"`).Where(`"usernameLower" IN ? AND "host" IS NULL`, usernames).Pluck("id", &ids)
		if len(ids) == 0 {
			return
		}
		db.Exec(`DELETE FROM "chat_message" WHERE "fromUserId" IN ? OR "toUserId" IN ?`, ids, ids)
		db.Exec(`DELETE FROM "chat_approval" WHERE "userId" IN ? OR "otherId" IN ?`, ids, ids)
		db.Exec(`DELETE FROM "note_reaction" WHERE "userId" IN ?`, ids)
		db.Exec(`DELETE FROM "note" WHERE "userId" IN ?`, ids)
		db.Exec(`DELETE FROM "following" WHERE "followerId" IN ? OR "followeeId" IN ?`, ids, ids)
		db.Exec(`DELETE FROM "follow_request" WHERE "followerId" IN ? OR "followeeId" IN ?`, ids, ids)
		db.Exec(`DELETE FROM "muting" WHERE "muterId" IN ? OR "muteeId" IN ?`, ids, ids)
		db.Exec(`DELETE FROM "blocking" WHERE "blockerId" IN ? OR "blockeeId" IN ?`, ids, ids)
	}
	clean()
	t.Cleanup(clean)
	cleanupPluginAccountUsers(t, db, usernames...)
}

// 管理するアカウントへの通知が、そのプラグインの専用キューに積まれ、queue
// ロールが登録した handler に届くこと。種類ごとに、本体の API を実際に叩いて
// 通知を作らせて確かめる (#3469)。
func TestPluginNotifications_DeliversToManagingPlugin_RealDB(t *testing.T) {
	h := newPluginNotificationsHarness(t)
	users := []string{"pn_bot_a", "pn_bot_b", "pn_bot_c", "pn_alice", "pn_carol"}
	cleanupPluginNotificationRows(t, h.db, users...)
	createLocalUser(t, h.db, "pn-alice", "pn_alice", "pnalicetoken0016", "alice-pass", false)
	createLocalUser(t, h.db, "pn-carol", "pn_carol", "pncaroltoken0016", "carol-pass", false)
	const alice, carol = "pnalicetoken0016", "pncaroltoken0016"
	ctx := context.Background()

	botA, err := h.ctxA.Accounts().Create(ctx, "pn_bot_a")
	require.NoError(t, err)
	botB, err := h.ctxB.Accounts().Create(ctx, "pn_bot_b")
	require.NoError(t, err)
	_, err = h.ctxC.Accounts().Create(ctx, "pn_bot_c")
	require.NoError(t, err)

	// mention: 宛先のプラグインのキューにだけ積む。宣言の無い bot-c と、普通の
	// 利用者 (carol) へのメンションは積まない。
	mention := createdNoteID(t, h.post(alice, "notes/create", map[string]any{
		"text": "@pn_bot_a @pn_bot_b @pn_bot_c @pn_carol hello",
	}))
	jA := h.waitJob("bot-a", "mention", botA.ID)
	assert.Equal(t, mention, h.waitJob("bot-b", "mention", botB.ID).ev.NoteID)
	assert.Equal(t, "pn-alice", jA.ev.UserID)
	assert.Equal(t, mention, jA.ev.NoteID)
	assert.NotEmpty(t, jA.ev.ID)
	for _, j := range h.rec.notificationJobs(t, "bot-a") {
		assert.Equal(t, botA.ID, j.ev.AccountID, "bot-a のキューに他のアカウント宛てが混ざらない")
	}
	for _, j := range h.rec.notificationJobs(t, "bot-b") {
		assert.Equal(t, botB.ID, j.ev.AccountID, "bot-b のキューに他のアカウント宛てが混ざらない")
	}
	assert.Empty(t, h.rec.notificationJobs(t, "bot-c"), "Notifications を宣言していないプラグインには積まない")

	// 専用キューへ、再試行と backoff を付けて積む。
	o := driver.ApplyEnqueueOptions(jA.call.opts)
	assert.Equal(t, queue.PluginQueueName("bot-a"), o.Queue)
	assert.Equal(t, queue.PluginNotificationMaxAttempts-1, o.MaxRetry)
	assert.Equal(t, driver.BackoffExponential, o.BackoffType)

	// handler へ渡る形。
	require.NoError(t, h.run(jA))
	got := h.receivedBy("bot-a")
	require.Len(t, got, 1)
	assert.Equal(t, plugin.Notification{
		ID: jA.ev.ID, Type: plugin.NotificationMention, AccountID: botA.ID, UserID: "pn-alice",
		NoteID: mention, NoteVisibility: "public", CreatedAt: jA.ev.CreatedAt,
	}, got[0])
	assert.False(t, got[0].CreatedAt.IsZero())

	// reply / quote / reaction: bot-a の投稿に対して。
	own := createdNoteID(t, asBot(t, h.ctxA, botA.ID, "notes/create", map[string]any{"text": "bot post"}))
	reply := createdNoteID(t, h.post(alice, "notes/create", map[string]any{"text": "re", "replyId": own}))
	assert.Equal(t, reply, h.waitJob("bot-a", "reply", botA.ID).ev.NoteID)
	quote := createdNoteID(t, h.post(alice, "notes/create", map[string]any{"text": "quote", "renoteId": own}))
	assert.Equal(t, quote, h.waitJob("bot-a", "quote", botA.ID).ev.NoteID)
	h.post(alice, "notes/reactions/create", map[string]any{"noteId": own, "reaction": "⭐"})
	react := h.waitJob("bot-a", "reaction", botA.ID)
	assert.Equal(t, own, react.ev.NoteID)
	assert.NotEmpty(t, react.ev.Reaction)

	// 返信は reply だけ (同じ相手への mention は作らない)。
	replyJobs := 0
	for _, j := range h.rec.notificationJobs(t, "bot-a") {
		if j.ev.NoteID == reply {
			replyJobs++
		}
	}
	assert.Equal(t, 1, replyJobs, "1 つの投稿で同じアカウントに reply と mention の両方を積まない")

	// follow / followRequest。
	h.post(alice, "following/create", map[string]any{"userId": botA.ID})
	assert.Equal(t, "pn-alice", h.waitJob("bot-a", "follow", botA.ID).ev.UserID)
	asBot(t, h.ctxA, botA.ID, "i/update", map[string]any{"isLocked": true})
	h.post(carol, "following/create", map[string]any{"userId": botA.ID})
	assert.Equal(t, "pn-carol", h.waitJob("bot-a", "receiveFollowRequest", botA.ID).ev.UserID)

	// chat: 1:1 のメッセージ。
	asBot(t, h.ctxA, botA.ID, "i/update", map[string]any{"chatScope": "everyone"})
	msg := h.post(alice, "chat/messages/create-to-user", map[string]any{"toUserId": botA.ID, "text": "hi bot"})
	chat := h.waitJob("bot-a", "chatMessage", botA.ID)
	assert.Equal(t, msg["id"], chat.ev.ChatMessageID)
	assert.Equal(t, chat.ev.ChatMessageID, chat.ev.ID)
	assert.Equal(t, "pn-alice", chat.ev.UserID)
	require.NoError(t, h.run(chat))
	got = h.receivedBy("bot-a")
	assert.Equal(t, plugin.NotificationChatMessage, got[len(got)-1].Type)

	// 普通の利用者へのチャットは積まない。
	h.post(carol, "i/update", map[string]any{"chatScope": "everyone"})
	h.post(alice, "chat/messages/create-to-user", map[string]any{"toUserId": "pn-carol", "text": "hi carol"})
	for _, name := range []string{"bot-a", "bot-b", "bot-c"} {
		for _, j := range h.rec.notificationJobs(t, name) {
			assert.NotEqual(t, "pn-carol", j.ev.AccountID)
		}
	}
}

// handler が失敗しても、通知と投稿の処理は成功する。失敗はジョブのエラーと
// してキューへ返るので、キューが再試行する (#3469)。
func TestPluginNotifications_HandlerFailureDoesNotFailNoteCreation_RealDB(t *testing.T) {
	h := newPluginNotificationsHarness(t)
	cleanupPluginNotificationRows(t, h.db, "pf_bot_a", "pf_alice")
	createLocalUser(t, h.db, "pf-alice", "pf_alice", "pfalicetoken0016", "alice-pass", false)
	const alice = "pfalicetoken0016"
	boom := errors.New("handler down")
	h.mu.Lock()
	h.failA = boom
	h.mu.Unlock()

	bot, err := h.ctxA.Accounts().Create(context.Background(), "pf_bot_a")
	require.NoError(t, err)
	rec := h.do(http.MethodPost, "/api/notes/create", alice, map[string]any{"text": "@pf_bot_a are you there"})
	require.Equal(t, http.StatusOK, rec.Code, "handler が落ちていても投稿は成功する: %s", rec.Body.String())

	j := h.waitJob("bot-a", "mention", bot.ID)
	err = h.run(j)
	require.ErrorIs(t, err, boom, "handler のエラーをキューへ返す (再試行させる)")
	require.NotErrorIs(t, err, driver.ErrSkipRetry)
	// 再試行は同じ ID で届く。
	h.mu.Lock()
	h.failA = nil
	h.mu.Unlock()
	require.NoError(t, h.run(j))
	got := h.receivedBy("bot-a")
	require.Len(t, got, 2)
	assert.Equal(t, got[0].ID, got[1].ID)

	// 通知そのものも作られている (handler の結果と無関係)。
	raw, err := h.ctxA.API().AsUser(bot.ID).Call(context.Background(), "i/notifications", map[string]any{})
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"mention"`)
}

// ミュート・ブロックで通知が作られないときは積まない (#3469)。
func TestPluginNotifications_SuppressedByMuteAndBlock_RealDB(t *testing.T) {
	h := newPluginNotificationsHarness(t)
	cleanupPluginNotificationRows(t, h.db, "ps_bot_a", "ps_muted", "ps_blocked", "ps_ok")
	createLocalUser(t, h.db, "ps-muted", "ps_muted", "psmutedtoken0016", "muted-pass", false)
	createLocalUser(t, h.db, "ps-blocked", "ps_blocked", "psblocktoken0016", "block-pass", false)
	createLocalUser(t, h.db, "ps-ok", "ps_ok", "psokokoktoken016", "ok-pass", false)
	const muted, blocked, ok = "psmutedtoken0016", "psblocktoken0016", "psokokoktoken016"

	bot, err := h.ctxA.Accounts().Create(context.Background(), "ps_bot_a")
	require.NoError(t, err)
	asBot(t, h.ctxA, bot.ID, "i/update", map[string]any{"chatScope": "everyone"})
	asBot(t, h.ctxA, bot.ID, "mute/create", map[string]any{"userId": "ps-muted"})
	asBot(t, h.ctxA, bot.ID, "blocking/create", map[string]any{"userId": "ps-blocked"})
	own := createdNoteID(t, asBot(t, h.ctxA, bot.ID, "notes/create", map[string]any{"text": "bot post"}))

	// ミュートした相手: メンション・リアクション・チャットのどれも積まない。
	h.post(muted, "notes/create", map[string]any{"text": "@ps_bot_a muted mention"})
	h.post(muted, "notes/reactions/create", map[string]any{"noteId": own, "reaction": "⭐"})
	h.post(muted, "chat/messages/create-to-user", map[string]any{"toUserId": bot.ID, "text": "muted chat"})
	// ブロックした相手: メンションは通知が作られない。フォロー・チャットは
	// 本体が拒否する。
	h.post(blocked, "notes/create", map[string]any{"text": "@ps_bot_a blocked mention"})
	assert.GreaterOrEqual(t, h.do(http.MethodPost, "/api/following/create", blocked, map[string]any{"userId": bot.ID}).Code, 400)
	assert.GreaterOrEqual(t, h.do(http.MethodPost, "/api/chat/messages/create-to-user", blocked, map[string]any{"toUserId": bot.ID, "text": "x"}).Code, 400)

	// 通知の作成は投稿の後に非同期で走るので、後から来た通知が積まれるのを
	// 待ってから、抑制した分が積まれていないことを見る。
	h.post(ok, "notes/create", map[string]any{"text": "@ps_bot_a fine"})
	h.waitJob("bot-a", "mention", bot.ID)
	time.Sleep(300 * time.Millisecond)
	for _, j := range h.rec.notificationJobs(t, "bot-a") {
		assert.NotEqual(t, "ps-muted", j.ev.UserID, "ミュートした相手の %s を積んだ", j.ev.Type)
		assert.NotEqual(t, "ps-blocked", j.ev.UserID, "ブロックした相手の %s を積んだ", j.ev.Type)
	}
	assert.Equal(t, 1, h.rec.allNotificationJobs(), "積まれるのは ps_ok のメンションだけ")
}

// キューへ積めなくても (Redis の障害など)、投稿と通知は成功する (#3469)。
func TestPluginNotifications_EnqueueFailureDoesNotFailNoteCreation_RealDB(t *testing.T) {
	h := newPluginNotificationsHarness(t)
	cleanupPluginNotificationRows(t, h.db, "pe_bot_a", "pe_alice")
	createLocalUser(t, h.db, "pe-alice", "pe_alice", "pealicetoken0016", "alice-pass", false)
	h.rec.mu.Lock()
	h.rec.enqueueErr = errors.New("redis down")
	h.rec.mu.Unlock()

	bot, err := h.ctxA.Accounts().Create(context.Background(), "pe_bot_a")
	require.NoError(t, err)
	rec := h.do(http.MethodPost, "/api/notes/create", "pealicetoken0016", map[string]any{"text": "@pe_bot_a hi"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Eventually(t, func() bool {
		raw, err := h.ctxA.API().AsUser(bot.ID).Call(context.Background(), "i/notifications", map[string]any{})
		return err == nil && strings.Contains(string(raw), `"mention"`)
	}, 5*time.Second, 10*time.Millisecond, "通知は作られる")
	assert.Zero(t, h.rec.allNotificationJobs())
}
