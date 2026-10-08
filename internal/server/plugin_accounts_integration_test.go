package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/labstack/echo/v4"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	apimeta "github.com/elythia-network/elythia/internal/api/meta"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/misc/password"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/elythia-network/elythia/plugin"
)

// pluginAccountsHarness is a full server with two plugins whose contexts the
// test holds, backed by the real database (#3468).
type pluginAccountsHarness struct {
	t    *testing.T
	db   *gorm.DB
	srv  *Server
	ctxA plugin.Context
	ctxB plugin.Context
}

func newPluginAccountsHarness(t *testing.T, settings map[string]map[string]any) *pluginAccountsHarness {
	t.Helper()
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	return newPluginAccountsHarnessDB(t, serverIntegrationDB, settings)
}

func newPluginAccountsHarnessDB(t *testing.T, db *gorm.DB, settings map[string]map[string]any) *pluginAccountsHarness {
	t.Helper()
	t.Setenv(config.EnvOnlyServer, "1")
	t.Setenv(config.EnvOnlyQueue, "")
	// ドライブのファイルは作業ディレクトリ相対の ./drive-files に置かれるので、
	// リポジトリを汚さないよう一時ディレクトリへ移る。
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
		MediaProxySecret: []byte("test-secret"),
		Redis:            redisOptions, RedisForPubsub: redisOptions, RedisForJobQueue: redisOptions,
		RedisForTimelines: redisOptions, RedisForReactions: redisOptions,
		Plugins: settings,
	}

	h := &pluginAccountsHarness{t: t, db: db}
	def := func(name string, dst *plugin.Context) plugin.Definition {
		return plugin.Definition{
			Name: name, APIVersion: plugin.APIVersion,
			Routes: func(ctx plugin.Context, _ plugin.Router) error {
				*dst = ctx
				return nil
			},
		}
	}
	restoreProcessGlobals(t)
	srv, err := newServer(cfg, h.db, redisClients,
		[]plugin.Definition{def("bot-a", &h.ctxA), def("bot-b", &h.ctxB)}, noopStorage)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	h.srv = srv
	return h
}

// do sends an external HTTP request (no in-process mark).
func (h *pluginAccountsHarness) do(method, path, token string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	var r *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(h.t, err)
		r = bytes.NewReader(raw)
	} else {
		r = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, r)
	if body != nil {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	if token != "" {
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (h *pluginAccountsHarness) user(id string) *model.User {
	h.t.Helper()
	var u model.User
	require.NoError(h.t, h.db.First(&u, "id = ?", id).Error)
	return &u
}

func (h *pluginAccountsHarness) profile(id string) *model.UserProfile {
	h.t.Helper()
	var p model.UserProfile
	require.NoError(h.t, h.db.First(&p, `"userId" = ?`, id).Error)
	return &p
}

// cleanupUsers deletes the rows the test creates, by username.
func cleanupPluginAccountUsers(t *testing.T, db *gorm.DB, usernames ...string) {
	t.Helper()
	clean := func() {
		var ids []string
		db.Model(&model.User{}).Where(`"usernameLower" IN ? AND "host" IS NULL`, usernames).Pluck("id", &ids)
		if len(ids) > 0 {
			db.Exec(`DELETE FROM "password_reset_request" WHERE "userId" IN ?`, ids)
			db.Exec(`DELETE FROM "access_token" WHERE "userId" IN ?`, ids)
			db.Exec(`DELETE FROM "drive_file" WHERE "userId" IN ?`, ids)
			db.Exec(`DELETE FROM "user_keypair" WHERE "userId" IN ?`, ids)
			db.Exec(`DELETE FROM "user_profile" WHERE "userId" IN ?`, ids)
			db.Exec(`DELETE FROM "user" WHERE "id" IN ?`, ids)
		}
		db.Exec(`DELETE FROM "used_username" WHERE "username" IN ?`, usernames)
	}
	clean()
	t.Cleanup(clean)
}

// createLocalUser inserts an ordinary local user with a password.
func createLocalUser(t *testing.T, db *gorm.DB, id, username, token, plain string, root bool) {
	t.Helper()
	tok := token
	require.NoError(t, db.Create(&model.User{
		ID: id, Username: username, UsernameLower: strings.ToLower(username), Token: &tok, IsRoot: root,
		AvatarDecorations: []byte("[]"),
	}).Error)
	hash, err := password.Hash(plain)
	require.NoError(t, err)
	email := username + "@example.test"
	require.NoError(t, db.Create(&model.UserProfile{
		UserID: id, Password: &hash, Email: &email, EmailVerified: true,
	}).Error)
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := 0; x < 4; x++ {
		for y := 0; y < 4; y++ {
			img.Set(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

func apiErrorCode(t *testing.T, err error) (int, string) {
	t.Helper()
	var apiErr *plugin.APIError
	require.True(t, errors.As(err, &apiErr), "APIError であること: %v", err)
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(apiErr.Body, &body)
	return apiErr.Status, body.Error.Code
}

// プラグインが管理するアカウントを、公開 API (ctx.Accounts()) から作成・一覧・
// 更新・削除できること、他のプラグインのアカウントや普通の利用者は操作
// できないことを、本物の配線と DB で確かめる (#3468)。
func TestPluginAccounts_ScopedPerPlugin_RealDB(t *testing.T) {
	h := newPluginAccountsHarness(t, nil)
	cleanupPluginAccountUsers(t, h.db, "pa_bot_a1", "pa_bot_a2", "pa_bot_b1", "pa_normal")
	createLocalUser(t, h.db, "pa-normal", "pa_normal", "panormaltoken016", "normal-pass", false)
	ctx := context.Background()
	accA, accB := h.ctxA.Accounts(), h.ctxB.Accounts()

	a1, err := accA.Create(ctx, "pa_bot_a1")
	require.NoError(t, err)
	assert.Equal(t, "pa_bot_a1", a1.Username)
	a2, err := accA.Create(ctx, "pa_bot_a2")
	require.NoError(t, err)
	b1, err := accB.Create(ctx, "pa_bot_b1")
	require.NoError(t, err)

	_, err = accA.Create(ctx, "pa_bot_b1")
	assert.ErrorIs(t, err, plugin.ErrUsernameUnavailable, "他のプラグインが使った名前も使えない")
	_, err = accB.Create(ctx, "pa_normal")
	assert.ErrorIs(t, err, plugin.ErrUsernameUnavailable, "普通の利用者の名前は使えない")
	_, err = accA.Create(ctx, "bad name")
	assert.ErrorIs(t, err, plugin.ErrInvalidUsername)

	// 作ったアカウントの形: 管理するプラグインの名前、bot、パスワード無し、token あり。
	u := h.user(a1.ID)
	require.NotNil(t, u.ManagedByPlugin)
	assert.Equal(t, "bot-a", *u.ManagedByPlugin)
	assert.True(t, u.IsBot, "管理するアカウントは必ず bot")
	require.NotNil(t, u.Token, "AsUser に使う native token は持つ")
	assert.Nil(t, h.profile(a1.ID).Password, "パスワードを持たせない")
	assert.Equal(t, "bot-b", *h.user(b1.ID).ManagedByPlugin)

	ids := func(list []plugin.Account) []string {
		out := []string{}
		for _, a := range list {
			out = append(out, a.ID)
		}
		return out
	}
	listA, err := accA.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{a1.ID, a2.ID}, ids(listA), "自分のアカウントだけを作成順に返す")
	listB, err := accB.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{b1.ID}, ids(listB))

	name := "Bot A1"
	// 他のプラグインのアカウントと普通の利用者は、存在しないのと同じに扱う。
	for _, target := range []string{b1.ID, "pa-normal", "no-such-user"} {
		assert.ErrorIs(t, accA.UpdateProfile(ctx, target, plugin.ProfileUpdate{Name: &name}), plugin.ErrAccountNotFound, target)
		assert.ErrorIs(t, accA.Delete(ctx, target), plugin.ErrAccountNotFound, target)
	}
	assert.Nil(t, h.user(b1.ID).Name, "他のプラグインのアカウントは変わらない")
	assert.False(t, h.user("pa-normal").IsDeleted, "普通の利用者は消えない")

	// プロフィールの更新は i/update を通る。画像はそのアカウントのドライブに置く。
	desc := "I am a bot."
	require.NoError(t, accA.UpdateProfile(ctx, a1.ID, plugin.ProfileUpdate{
		Name: &name, Description: &desc,
		Avatar: &plugin.Image{Data: pngBytes(t), Filename: "avatar.png"},
		Banner: &plugin.Image{Data: pngBytes(t)},
	}))
	u = h.user(a1.ID)
	require.NotNil(t, u.Name)
	assert.Equal(t, name, *u.Name)
	require.NotNil(t, h.profile(a1.ID).Description)
	assert.Equal(t, desc, *h.profile(a1.ID).Description)
	require.NotNil(t, u.AvatarID)
	require.NotNil(t, u.BannerID)
	var avatar model.DriveFile
	require.NoError(t, h.db.First(&avatar, "id = ?", *u.AvatarID).Error)
	require.NotNil(t, avatar.UserID)
	assert.Equal(t, a1.ID, *avatar.UserID, "画像はそのアカウントのドライブに置く")
	assert.Equal(t, "avatar.png", avatar.Name)

	// 空文字は「消す」(name は null に、description は空になる。i/update と同じ)。
	empty := ""
	require.NoError(t, accA.UpdateProfile(ctx, a1.ID, plugin.ProfileUpdate{Name: &empty, Description: &empty}))
	assert.Nil(t, h.user(a1.ID).Name)
	if d := h.profile(a1.ID).Description; d != nil {
		assert.Empty(t, *d)
	}

	// 画像でないファイルは i/update が拒否する。
	err = accA.UpdateProfile(ctx, a1.ID, plugin.ProfileUpdate{Avatar: &plugin.Image{Data: []byte("not an image at all")}})
	status, code := apiErrorCode(t, err)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "AVATAR_NOT_AN_IMAGE", code)

	// 削除は通常の流れ (論理削除 → ジョブ)。一覧から消え、AsUser も通らなくなる。
	require.NoError(t, accA.Delete(ctx, a2.ID))
	assert.True(t, h.user(a2.ID).IsDeleted)
	listA, err = accA.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{a1.ID}, ids(listA))
	assert.ErrorIs(t, accA.Delete(ctx, a2.ID), plugin.ErrAccountNotFound, "消したものは見えない")
	_, err = h.ctxA.API().AsUser(a2.ID).Call(ctx, "i", nil)
	require.Error(t, err, "消したアカウントとしては呼べない")
	_, err = accA.Create(ctx, "pa_bot_a2")
	assert.ErrorIs(t, err, plugin.ErrUsernameUnavailable, "消したアカウントの名前は再利用できない")
}

// 管理するアカウントの native token は、プロセス内の AsUser でだけ通る。外から
// 届いた HTTP と streaming では、token が正しくても拒否する。token はどの応答にも
// 出さない (#3468)。
func TestPluginAccounts_TokenOnlyInProcess_RealDB(t *testing.T) {
	h := newPluginAccountsHarness(t, nil)
	cleanupPluginAccountUsers(t, h.db, "pt_bot", "pt_normal", "pt_root")
	createLocalUser(t, h.db, "pt-normal", "pt_normal", "ptnormaltoken016", "normal-pass", false)
	createLocalUser(t, h.db, "pt-root", "pt_root", "ptroottokenxx016", "root-pass", true)
	ctx := context.Background()

	bot, err := h.ctxA.Accounts().Create(ctx, "pt_bot")
	require.NoError(t, err)
	token := strings.TrimRight(*h.user(bot.ID).Token, " ")
	require.NotEmpty(t, token)

	// in-process: 通る。応答に token が出ない。
	raw, err := h.ctxA.API().AsUser(bot.ID).Call(ctx, "i", nil)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"pt_bot"`)
	assert.NotContains(t, string(raw), token, "i の応答に token を出さない")
	raw, err = h.ctxA.API().AsUser(bot.ID).Call(ctx, "i/update", map[string]any{"name": "x"})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), token, "i/update の応答に token を出さない")

	// 外からの HTTP: token の置き場所によらず、無効な token と同じ 401。
	rec := h.do(http.MethodPost, "/api/i", token, map[string]any{})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "AUTHENTICATION_FAILED")
	rec = h.do(http.MethodPost, "/api/i", "", map[string]any{"i": token})
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "body の i でも拒否する")
	rec = h.do(http.MethodPost, "/api/notes/create", token, map[string]any{"text": "hello"})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	// 普通の利用者は今までどおり。
	assert.Equal(t, http.StatusOK, h.do(http.MethodPost, "/api/i", "ptnormaltoken016", map[string]any{}).Code)

	// streaming: upgrade の前に 401 で拒否する。普通の利用者は 401 にならない。
	stream := func(tok string) int {
		req := httptest.NewRequest(http.MethodGet, "/streaming?i="+url.QueryEscape(tok), nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		req.Header.Set("Sec-WebSocket-Version", "13")
		req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
		rec := httptest.NewRecorder()
		h.srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	assert.Equal(t, http.StatusUnauthorized, stream(token), "streaming も拒否する")
	assert.NotEqual(t, http.StatusUnauthorized, stream("ptnormaltoken016"))

	// 管理画面の詳細: 管理しているプラグインを出し、token は出さない。
	rec = h.do(http.MethodPost, "/api/admin/show-user", "ptroottokenxx016", map[string]any{"userId": bot.ID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var shown map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &shown))
	assert.Equal(t, "bot-a", shown["managedByPlugin"])
	assert.NotContains(t, rec.Body.String(), token)
	rec = h.do(http.MethodPost, "/api/admin/show-user", "ptroottokenxx016", map[string]any{"userId": "pt-normal"})
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &shown))
	v, ok := shown["managedByPlugin"]
	assert.True(t, ok, "普通のアカウントでも項目は出す")
	assert.Nil(t, v)
}

// 管理するアカウントには、どの経路からもログインできない。同じ条件の普通の
// 利用者は今までどおりログインできる (#3468)。
//
// 拒否が「パスワードが無いから」で偶然に成り立っているのではないことを示すため、
// 管理するアカウントにもパスワードと確認済みのメールを入れてから試す。
func TestPluginAccounts_EveryLoginPathRefused_RealDB(t *testing.T) {
	h := newPluginAccountsHarness(t, nil)
	cleanupPluginAccountUsers(t, h.db, "pl_bot", "pl_normal", "pl_root")
	createLocalUser(t, h.db, "pl-normal", "pl_normal", "plnormaltoken016", "same-pass", false)
	createLocalUser(t, h.db, "pl-root", "pl_root", "plroottokenxx016", "root-pass", true)
	ctx := context.Background()

	bot, err := h.ctxA.Accounts().Create(ctx, "pl_bot")
	require.NoError(t, err)
	hash, err := password.Hash("same-pass")
	require.NoError(t, err)
	email := "pl_bot@example.test"
	require.NoError(t, h.db.Model(&model.UserProfile{}).Where(`"userId" = ?`, bot.ID).
		Updates(map[string]any{"password": hash, "email": email, "emailVerified": true}).Error)

	type target struct{ id, username string }
	managed := target{bot.ID, "pl_bot"}
	normal := target{"pl-normal", "pl_normal"}

	resetRequests := func(userID string) int64 {
		var n int64
		require.NoError(t, h.db.Table("password_reset_request").Where(`"userId" = ?`, userID).Count(&n).Error)
		return n
	}

	cases := []struct {
		name string
		// run returns whether the attempt succeeded in getting credentials.
		run func(t *testing.T, tg target) bool
	}{
		{"signin", func(t *testing.T, tg target) bool {
			rec := h.do(http.MethodPost, "/api/signin", "", map[string]any{"username": tg.username, "password": "same-pass"})
			return rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), `"i"`)
		}},
		{"signin-flow", func(t *testing.T, tg target) bool {
			rec := h.do(http.MethodPost, "/api/signin-flow", "", map[string]any{"username": tg.username, "password": "same-pass"})
			return rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), `"i"`)
		}},
		{"signin-flow first step", func(t *testing.T, tg target) bool {
			// パスワード無しの最初の段で「次へ」を返すと、ログインできる相手だと読める。
			rec := h.do(http.MethodPost, "/api/signin-flow", "", map[string]any{"username": tg.username})
			return rec.Code == http.StatusOK
		}},
		{"signin-flow with 2fa token", func(t *testing.T, tg target) bool {
			rec := h.do(http.MethodPost, "/api/signin-flow", "", map[string]any{"username": tg.username, "password": "same-pass", "token": "000000"})
			return rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), `"i"`)
		}},
		{"request-reset-password", func(t *testing.T, tg target) bool {
			before := resetRequests(tg.id)
			rec := h.do(http.MethodPost, "/api/request-reset-password", "", map[string]any{"username": tg.username, "email": tg.username + "@example.test"})
			require.Equal(t, http.StatusNoContent, rec.Code, "応答は居ない利用者と同じ")
			return resetRequests(tg.id) > before
		}},
		{"reset-password", func(t *testing.T, tg target) bool {
			tok := "resettoken-" + tg.username
			require.NoError(t, h.db.Exec(`INSERT INTO "password_reset_request" ("id", "token", "userId") VALUES (?, ?, ?)`,
				h.srvID(), tok, tg.id).Error)
			rec := h.do(http.MethodPost, "/api/reset-password", "", map[string]any{"token": tok, "password": "new-pass"})
			return rec.Code == http.StatusNoContent
		}},
		{"admin/reset-password", func(t *testing.T, tg target) bool {
			rec := h.do(http.MethodPost, "/api/admin/reset-password", "plroottokenxx016", map[string]any{"userId": tg.id})
			return rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), `"password"`)
		}},
		{"miauth/gen-token via AsUser", func(t *testing.T, tg target) bool {
			_, err := h.ctxA.API().AsUser(tg.id).Call(ctx, "miauth/gen-token", map[string]any{
				"session": nil, "name": "x", "permission": []string{"read:account"},
			})
			return err == nil
		}},
		{"miauth/gen-token from outside", func(t *testing.T, tg target) bool {
			tok := *h.user(tg.id).Token
			rec := h.do(http.MethodPost, "/api/miauth/gen-token", strings.TrimRight(tok, " "), map[string]any{
				"session": nil, "name": "x", "permission": []string{"read:account"},
			})
			return rec.Code == http.StatusOK
		}},
		{"auth/accept via AsUser", func(t *testing.T, tg target) bool {
			_, err := h.ctxA.API().AsUser(tg.id).Call(ctx, "auth/accept", map[string]any{"token": "no-such-session"})
			// 普通の利用者は session が無いので NO_SUCH_SESSION で止まる。管理する
			// アカウントはその前に拒否する。区別は error code で見る。
			if err == nil {
				return true
			}
			_, code := apiErrorCode(t, err)
			return code != "PLUGIN_MANAGED_ACCOUNT"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.False(t, tc.run(t, managed), "管理するアカウントは拒否する")
			assert.True(t, tc.run(t, normal), "普通の利用者は今までどおり")
		})
	}
	// パスワードの再設定を通していないので、管理するアカウントのパスワードは
	// 入れたものから変わっていない。
	assert.Equal(t, hash, *h.profile(bot.ID).Password)
}

// srvID returns a fresh aidx ID for rows the test inserts directly. 再設定の
// 要求は ID の時刻で期限を見るので、今の時刻で作る。
func (h *pluginAccountsHarness) srvID() string {
	h.t.Helper()
	gen, err := id.NewGenerator("aidx")
	require.NoError(h.t, err)
	return gen.Generate(time.Now())
}

// isBot は外せない。凍結は普通のアカウントと同じく効く。プラグインを無効に
// してもアカウントは残る (#3468)。
func TestPluginAccounts_BotFlagModerationAndDisable_RealDB(t *testing.T) {
	h := newPluginAccountsHarness(t, nil)
	cleanupPluginAccountUsers(t, h.db, "pm_bot", "pm_normal", "pm_root")
	createLocalUser(t, h.db, "pm-normal", "pm_normal", "pmnormaltoken016", "normal-pass", false)
	createLocalUser(t, h.db, "pm-root", "pm_root", "pmroottokenxx016", "root-pass", true)
	ctx := context.Background()

	bot, err := h.ctxA.Accounts().Create(ctx, "pm_bot")
	require.NoError(t, err)
	as := h.ctxA.API().AsUser(bot.ID)

	_, err = as.Call(ctx, "i/update", map[string]any{"isBot": false})
	status, code := apiErrorCode(t, err)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, "PLUGIN_MANAGED_ACCOUNT_MUST_BE_BOT", code)
	assert.True(t, h.user(bot.ID).IsBot)
	_, err = as.Call(ctx, "i/update", map[string]any{"isBot": true, "name": "still bot"})
	require.NoError(t, err, "true を送る更新は通す")
	assert.Equal(t, "still bot", *h.user(bot.ID).Name)
	// 普通の利用者は今までどおり外せる。
	_, err = h.ctxA.API().AsUser("pm-normal").Call(ctx, "i/update", map[string]any{"isBot": true})
	require.NoError(t, err)
	_, err = h.ctxA.API().AsUser("pm-normal").Call(ctx, "i/update", map[string]any{"isBot": false})
	require.NoError(t, err)
	assert.False(t, h.user("pm-normal").IsBot)

	// 凍結すると、プラグインもそのアカウントとして書き込めない。
	rec := h.do(http.MethodPost, "/api/admin/suspend-user", "pmroottokenxx016", map[string]any{"userId": bot.ID})
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err = as.Call(ctx, "notes/create", map[string]any{"text": "hello"})
	status, code = apiErrorCode(t, err)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, "YOUR_ACCOUNT_SUSPENDED", code)
	// 凍結中のプロフィール更新は、画像を置く前に断る (ドライブにファイルを残さない)。
	err = h.ctxA.Accounts().UpdateProfile(ctx, bot.ID, plugin.ProfileUpdate{Avatar: &plugin.Image{Data: pngBytes(t)}})
	assert.ErrorIs(t, err, plugin.ErrAccountSuspended)
	var files int64
	require.NoError(t, h.db.Table("drive_file").Where(`"userId" = ?`, bot.ID).Count(&files).Error)
	assert.Zero(t, files)
	rec = h.do(http.MethodPost, "/api/admin/unsuspend-user", "pmroottokenxx016", map[string]any{"userId": bot.ID})
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err = as.Call(ctx, "notes/create", map[string]any{"text": "hello again"})
	require.NoError(t, err, "解除すれば戻る")

	// プラグインを無効にして起動し直しても、アカウントと投稿は残り、ログインの
	// 拒否も外れない。
	require.NoError(t, h.srv.Shutdown(context.Background()))
	h2 := newPluginAccountsHarness(t, map[string]map[string]any{"bot-a": {"enabled": false}})
	assert.Nil(t, h2.ctxA, "無効にしたプラグインは登録されない")
	u := h2.user(bot.ID)
	assert.False(t, u.IsDeleted)
	assert.False(t, u.IsSuspended)
	assert.Equal(t, "bot-a", *u.ManagedByPlugin)
	var notes int64
	require.NoError(t, h2.db.Table("note").Where(`"userId" = ?`, bot.ID).Count(&notes).Error)
	assert.Equal(t, int64(1), notes)
	token := strings.TrimRight(*u.Token, " ")
	assert.Equal(t, http.StatusUnauthorized, h2.do(http.MethodPost, "/api/i", token, map[string]any{}).Code)
	assert.Equal(t, http.StatusNotFound, h2.do(http.MethodPost, "/api/signin-flow", "", map[string]any{"username": "pm_bot"}).Code)
	// 他のプラグインからは、無効にしたプラグインのアカウントを操作できない。
	assert.ErrorIs(t, h2.ctxB.Accounts().Delete(ctx, bot.ID), plugin.ErrAccountNotFound)
	h2.db.Exec(`DELETE FROM "note" WHERE "userId" = ?`, bot.ID)
}

// 新しいインスタンスでプラグインが起動時に bot を作っても、初回セットアップ
// (最初の管理者を作る) の窓は閉じない (#3468)。管理するアカウントは
// セットアップの判定で数えない。普通のローカル利用者が居れば今までどおり閉じる。
//
// 利用者 0 の状態を作るので、パッケージの共有 schema ではなく専用の schema を使う。
func TestPluginAccounts_InitialSetupIgnoresManagedAccounts_RealDB(t *testing.T) {
	db, err := testutil.OpenTestDBSchema("pluginsetup")
	if err != nil {
		t.Skipf("PostgreSQL unavailable: %v", err)
	}
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	testutil.ApplyMigrations(db)
	reset := func() {
		require.NoError(t, db.Exec(`TRUNCATE "user" CASCADE`).Error)
		require.NoError(t, db.Exec(`DELETE FROM "used_username"`).Error)
		db.Exec(`UPDATE "meta" SET "rootUserId" = NULL`)
	}
	reset()
	t.Cleanup(reset)

	h := newPluginAccountsHarnessDB(t, db, nil)
	apimeta.ResetSetupLatchForTest()
	ctx := context.Background()
	_, err = h.ctxA.Accounts().Create(ctx, "setup_bot")
	require.NoError(t, err)

	requireSetup := func() bool {
		t.Helper()
		rec := h.do(http.MethodPost, "/api/meta", "", map[string]any{})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var m map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m))
		v, _ := m["requireSetup"].(bool)
		return v
	}
	assert.True(t, requireSetup(), "管理するアカウントしか居なければセットアップ画面を出す")

	rec := h.do(http.MethodPost, "/api/admin/accounts/create", "", map[string]any{"username": "setup_admin", "password": "admin-pass"})
	require.Equal(t, http.StatusOK, rec.Code, "最初の管理者を作れる: %s", rec.Body.String())
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	var meta struct{ RootUserID *string }
	require.NoError(t, db.Raw(`SELECT "rootUserId" AS root_user_id FROM "meta" LIMIT 1`).Scan(&meta).Error)
	require.NotNil(t, meta.RootUserID)
	assert.Equal(t, created.ID, *meta.RootUserID)
	assert.False(t, requireSetup())

	// 普通のローカル利用者が居るときは、今までどおり窓を閉じる。
	reset()
	apimeta.ResetSetupLatchForTest()
	createLocalUser(t, db, "setup-plain", "setup_plain", "setupplaintok016", "plain-pass", false)
	assert.False(t, requireSetup())
	rec = h.do(http.MethodPost, "/api/admin/accounts/create", "", map[string]any{"username": "setup_admin2", "password": "admin-pass"})
	assert.NotEqual(t, http.StatusOK, rec.Code)
}
