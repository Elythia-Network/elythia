package plugintest_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/plugin"
	"github.com/elythia-network/elythia/plugin/plugintest"
)

// samplePlugin exercises every part of the harness.
var samplePlugin = plugin.Definition{
	Name:       "sample",
	APIVersion: plugin.APIVersion,
	Migrations: []plugin.Migration{
		{Version: 1, SQL: `CREATE TABLE items (id serial PRIMARY KEY, label text)`},
	},
	Routes: func(ctx plugin.Context, r plugin.Router) error {
		var cfg struct {
			Label string `json:"label"`
		}
		cfg.Label = "既定"
		if err := ctx.Config().Unmarshal(&cfg); err != nil {
			return err
		}

		r.GET("/name", func(plugin.Request) (any, error) {
			return map[string]string{"name": ctx.Name(), "label": cfg.Label}, nil
		})
		r.POST("/echo", func(req plugin.Request) (any, error) {
			var body struct {
				Msg string `json:"msg"`
			}
			if err := req.Bind(&body); err != nil {
				return nil, err
			}
			return map[string]any{
				"msg": body.Msg, "user": req.UserID(),
				"id": req.Param("id"), "q": req.Query("q"),
			}, nil
		})
		r.POST("/boom", func(plugin.Request) (any, error) {
			return nil, errors.New("boom")
		})
		return nil
	},
	Jobs: func(_ plugin.Context, j plugin.Jobs) error {
		j.Handle("tick", func(_ context.Context, payload json.RawMessage) error {
			if string(payload) == `{"fail":true}` {
				return errors.New("failed")
			}
			return nil
		})
		j.Schedule("*/5 * * * *", "tick", map[string]any{"a": 1})
		return nil
	},
}

func TestRoutes_RegistersByMethodAndPath(t *testing.T) {
	h := plugintest.New(t).Routes(samplePlugin)

	assert.Contains(t, h, "GET /name")
	assert.Contains(t, h, "POST /echo")
}

func TestCall_PassesRequestFields(t *testing.T) {
	h := plugintest.New(t).Routes(samplePlugin)

	res, err := h.Call(t, "POST /echo", plugintest.Request{
		UserID: "u1",
		Body:   `{"msg":"hi"}`,
		Params: map[string]string{"id": "abc"},
		Query:  map[string]string{"q": "zzz"},
	})
	require.NoError(t, err)

	m := res.(map[string]any)
	assert.Equal(t, "hi", m["msg"])
	assert.Equal(t, "u1", m["user"])
	assert.Equal(t, "abc", m["id"])
	assert.Equal(t, "zzz", m["q"])
}

// Body 未指定でも Bind が壊れないこと (空 body を明示しないテストが書ける)。
func TestCall_EmptyBodyIsUsable(t *testing.T) {
	h := plugintest.New(t).Routes(samplePlugin)

	res, err := h.Call(t, "POST /echo", plugintest.Request{UserID: "u1"})
	require.NoError(t, err)
	assert.Empty(t, res.(map[string]any)["msg"])
}

func TestCall_PropagatesHandlerError(t *testing.T) {
	h := plugintest.New(t).Routes(samplePlugin)

	_, err := h.Call(t, "POST /boom", plugintest.Request{})
	require.Error(t, err)
}

// 設定は Unmarshal に渡る。未設定なら呼び出し側の既定値が残る。
func TestWithConfig(t *testing.T) {
	withCfg := plugintest.New(t).WithConfig(map[string]any{"label": "設定値"}).Routes(samplePlugin)
	res, err := withCfg.Call(t, "GET /name", plugintest.Request{})
	require.NoError(t, err)
	assert.Equal(t, "設定値", res.(map[string]string)["label"])

	noCfg := plugintest.New(t).Routes(samplePlugin)
	res, err = noCfg.Call(t, "GET /name", plugintest.Request{})
	require.NoError(t, err)
	assert.Equal(t, "既定", res.(map[string]string)["label"])
}

func TestWithName(t *testing.T) {
	h := plugintest.New(t).WithName("mine").Routes(samplePlugin)

	res, err := h.Call(t, "GET /name", plugintest.Request{})
	require.NoError(t, err)
	assert.Equal(t, "mine", res.(map[string]string)["name"])

	// 既定は "test"。
	h2 := plugintest.New(t).Routes(samplePlugin)
	res, err = h2.Call(t, "GET /name", plugintest.Request{})
	require.NoError(t, err)
	assert.Equal(t, "test", res.(map[string]string)["name"])
}

func TestJobs_CapturesHandlersAndSchedules(t *testing.T) {
	j := plugintest.New(t).Jobs(samplePlugin)

	require.Contains(t, j.Handlers, "tick")
	require.Len(t, j.Schedules, 1)
	assert.Equal(t, "*/5 * * * *", j.Schedules[0].Cron)
	assert.Equal(t, "tick", j.Schedules[0].Name)
	assert.Equal(t, map[string]any{"a": 1}, j.Schedules[0].Payload)
}

func TestJobs_Run(t *testing.T) {
	j := plugintest.New(t).Jobs(samplePlugin)

	require.NoError(t, j.Run(t, "tick", ""))
	require.Error(t, j.Run(t, "tick", `{"fail":true}`))
}

// Routes / Jobs を持たない Definition でも落ちない。
func TestEmptyDefinition(t *testing.T) {
	def := plugin.Definition{Name: "x", APIVersion: plugin.APIVersion}

	assert.Empty(t, plugintest.New(t).Routes(def))
	assert.Empty(t, plugintest.New(t).Jobs(def).Handlers)
}

func TestContext_GoRunsSynchronously(t *testing.T) {
	ran := false
	plugintest.New(t).Context().Go(func() { ran = true })
	// 非同期だと検証の前に終わっていない競合が入る。
	assert.True(t, ran)
}

func TestContext_APIDefaultsToNil(t *testing.T) {
	assert.Nil(t, plugintest.New(t).Context().API())

	api := &stubAPI{}
	assert.Same(t, api, plugintest.New(t).WithAPI(api).Context().API())
}

type stubAPI struct{}

func (s *stubAPI) Anonymous() plugin.Caller    { return nil }
func (s *stubAPI) AsUser(string) plugin.Caller { return nil }

func TestContext_LoggerIsUsable(t *testing.T) {
	assert.NotNil(t, plugintest.New(t).Context().Logger())
}

// DB 未設定で Migrate を呼ぶと、理由が分かるエラーになる。
func TestStorage_WithoutDB(t *testing.T) {
	err := plugintest.New(t).Context().Storage().Migrate(context.Background(),
		[]plugin.Migration{{Version: 1, SQL: "SELECT 1"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "WithDB")
}

// 壊れた JSON を渡したら Bind がエラーになる (握り潰さない)。
func TestCall_MalformedBodyIsError(t *testing.T) {
	h := plugintest.New(t).Routes(samplePlugin)

	_, err := h.Call(t, "POST /echo", plugintest.Request{Body: `{"msg":`})
	require.Error(t, err)
}

// Ctx を渡すとハンドラ側の Context() に届くこと。
func TestCall_PassesContext(t *testing.T) {
	type ctxKey struct{}
	def := plugin.Definition{
		Name: "c", APIVersion: plugin.APIVersion,
		Routes: func(_ plugin.Context, r plugin.Router) error {
			r.POST("/ctx", func(req plugin.Request) (any, error) {
				return req.Context().Value(ctxKey{}), nil
			})
			return nil
		},
	}
	h := plugintest.New(t).Routes(def)

	res, err := h.Call(t, "POST /ctx", plugintest.Request{
		Ctx: context.WithValue(context.Background(), ctxKey{}, "v"),
	})
	require.NoError(t, err)
	assert.Equal(t, "v", res)

	// 未指定なら Background になる (nil にしない)。
	res, err = h.Call(t, "POST /ctx", plugintest.Request{})
	require.NoError(t, err)
	assert.Nil(t, res)
}

// 変換できない設定はエラーとして返る (黙って空にしない)。
func TestConfig_UnconvertibleValueIsError(t *testing.T) {
	var v struct{}
	err := plugintest.New(t).
		WithConfig(map[string]any{"ch": make(chan int)}).
		Context().Config().Unmarshal(&v)
	require.Error(t, err)
}

// --- DB を使う経路 ---

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

const testSchema = "plugin_plugintest"

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	base := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		envOr("TEST_DB_HOST", "localhost"), envOr("TEST_DB_PORT", "5432"),
		envOr("TEST_DB_USER", "mk"), envOr("TEST_DB_PASS", "mk"),
		envOr("TEST_DB_NAME", "misskey_test"))

	admin, err := sql.Open("pgx", base)
	require.NoError(t, err)
	defer admin.Close() //nolint:errcheck // 使い捨て
	_, err = admin.Exec(`DROP SCHEMA IF EXISTS ` + testSchema + ` CASCADE`)
	require.NoError(t, err)
	_, err = admin.Exec(`CREATE SCHEMA ` + testSchema)
	require.NoError(t, err)

	db, err := sql.Open("pgx", base+" search_path="+testSchema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = db.Close()
		if a, err := sql.Open("pgx", base); err == nil {
			_, _ = a.Exec(`DROP SCHEMA IF EXISTS ` + testSchema + ` CASCADE`)
			_ = a.Close()
		}
	})
	return db
}

func TestWithDB_AppliesMigrations(t *testing.T) {
	db := testDB(t)
	plugintest.New(t).WithDB(db).Routes(samplePlugin)

	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name = 'items'`,
		testSchema).Scan(&n))
	assert.Equal(t, 1, n)
}

// **本番と同じ適用管理をすること。** 素朴に実行するフェイクだと 2 回目で
// "already exists" になり、Routes と Jobs を続けて呼べない。
func TestWithDB_MigrationsAreIdempotent(t *testing.T) {
	db := testDB(t)

	plugintest.New(t).WithDB(db).Routes(samplePlugin)
	plugintest.New(t).WithDB(db).Jobs(samplePlugin) // 2 回目
	plugintest.New(t).WithDB(db).Routes(samplePlugin)
}

// プラグインは渡した接続をそのまま受け取る (差し替えたフェイクではない)。
func TestWithDB_HandsThroughTheConnection(t *testing.T) {
	db := testDB(t)
	assert.Same(t, db, plugintest.New(t).WithDB(db).Context().Storage().DB())
}

// peerPlugin exercises the mk-go-only plugin channel (#2537).
var peerPlugin = plugin.Definition{
	Name:       "peered",
	APIVersion: plugin.APIVersion,
	Peered:     true,
	Routes: func(ctx plugin.Context, r plugin.Router) error {
		peer := ctx.Peer()

		peer.Handle(func(_ context.Context, from string, payload json.RawMessage) (any, error) {
			var req struct {
				User string `json:"user"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				return nil, err
			}
			if req.User == "" {
				return nil, errors.New("user が空です")
			}
			return map[string]any{"from": from, "user": req.User}, nil
		})

		peer.OnReply(func(_ context.Context, from, id string, reply json.RawMessage) error {
			var body struct {
				Score int `json:"score"`
			}
			if err := json.Unmarshal(reply, &body); err != nil {
				return err
			}
			if body.Score < 0 {
				return fmt.Errorf("score が負です (%s / %s)", from, id)
			}
			return nil
		})

		r.POST("/ask", func(req plugin.Request) (any, error) {
			id, err := peer.Send(req.Context(), req.Query("host"), map[string]any{"user": "alice"})
			if err != nil {
				return nil, err
			}
			return map[string]any{"id": id}, nil
		})
		r.POST("/has", func(req plugin.Request) (any, error) {
			ok, err := peer.Has(req.Context(), req.Query("host"))
			if err != nil {
				return nil, err
			}
			return map[string]any{"has": ok}, nil
		})
		return nil
	},
}

// Send は宛先と中身を記録するだけで、**実際には送らない**。
func TestPeer_SendIsRecorded(t *testing.T) {
	h := plugintest.New(t).WithName("peered").WithPeers("other.example")
	routes := h.Routes(peerPlugin)

	res, err := routes.Call(t, "POST /ask", plugintest.Request{Query: map[string]string{"host": "other.example"}})
	require.NoError(t, err)

	sends := h.PeerSends()
	require.Len(t, sends, 1)
	assert.Equal(t, "other.example", sends[0].Host)
	assert.JSONEq(t, `{"user":"alice"}`, string(sends[0].Payload))

	body, ok := res.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, sends[0].ID, body["id"], "Send の戻り値が OnReply の相関に使える")
}

// WithPeers に無い相手への Send は、本番と同じく落とす。
func TestPeer_SendRequiresDeclaredHost(t *testing.T) {
	h := plugintest.New(t).WithName("peered")
	routes := h.Routes(peerPlugin)

	_, err := routes.Call(t, "POST /ask", plugintest.Request{Query: map[string]string{"host": "unknown.example"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "持っていません")
	assert.Empty(t, h.PeerSends())
}

func TestPeer_Has(t *testing.T) {
	h := plugintest.New(t).WithName("peered").WithPeers("a.example", "b.example")
	routes := h.Routes(peerPlugin)

	res, err := routes.Call(t, "POST /has", plugintest.Request{Query: map[string]string{"host": "a.example"}})
	require.NoError(t, err)
	assert.Equal(t, true, res.(map[string]any)["has"])

	res, err = routes.Call(t, "POST /has", plugintest.Request{Query: map[string]string{"host": "c.example"}})
	require.NoError(t, err)
	assert.Equal(t, false, res.(map[string]any)["has"])
}

// 相手から届いたことにしてハンドラを叩く。from は「検証済みの送信元」。
func TestPeer_DeliverPeer(t *testing.T) {
	h := plugintest.New(t).WithName("peered")
	h.Routes(peerPlugin)

	res, err := h.DeliverPeer("sender.example", map[string]any{"user": "bob"})
	require.NoError(t, err)
	body, ok := res.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "sender.example", body["from"])
	assert.Equal(t, "bob", body["user"])

	// ハンドラのエラーはそのまま返る (テストから中身を検査できる)。
	_, err = h.DeliverPeer("sender.example", map[string]any{})
	assert.Error(t, err)
}

// 応答が返ってきたことにして OnReply を叩く。
func TestPeer_DeliverPeerReply(t *testing.T) {
	h := plugintest.New(t).WithName("peered").WithPeers("other.example")
	routes := h.Routes(peerPlugin)

	_, err := routes.Call(t, "POST /ask", plugintest.Request{Query: map[string]string{"host": "other.example"}})
	require.NoError(t, err)
	id := h.PeerSends()[0].ID

	require.NoError(t, h.DeliverPeerReply("other.example", id, map[string]any{"score": 3}))
	assert.Error(t, h.DeliverPeerReply("other.example", id, map[string]any{"score": -1}))
}

// 管理用ルートの守りをテストできること。**画面を隠すだけでは守れない**ので、
// プラグインは自分で IsModerator を見る必要がある (sample はそれを例示する)。
func TestRequest_PrivilegeFlags(t *testing.T) {
	def := plugin.Definition{
		Name:       "priv",
		APIVersion: plugin.APIVersion,
		Routes: func(_ plugin.Context, r plugin.Router) error {
			r.POST("/who", func(req plugin.Request) (any, error) {
				return map[string]any{
					"mod":   req.IsModerator(),
					"admin": req.IsAdministrator(),
				}, nil
			})
			return nil
		},
	}
	routes := plugintest.New(t).WithName("priv").Routes(def)

	res, err := routes.Call(t, "POST /who", plugintest.Request{})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"mod": false, "admin": false}, res)

	res, err = routes.Call(t, "POST /who", plugintest.Request{Moderator: true})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"mod": true, "admin": false}, res)

	// Administrator は IsModerator も真にする (管理者は上位の権限を持つ)。
	res, err = routes.Call(t, "POST /who", plugintest.Request{Administrator: true})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"mod": true, "admin": true}, res)
}

// JSON にできない payload は Send の時点で落とす (送ってから気付かない)。
func TestPeer_SendRejectsUnmarshalablePayload(t *testing.T) {
	def := plugin.Definition{
		Name:       "badsend",
		APIVersion: plugin.APIVersion,
		Peered:     true,
		Routes: func(ctx plugin.Context, r plugin.Router) error {
			r.POST("/send", func(req plugin.Request) (any, error) {
				// chan は JSON にできない。
				return ctx.Peer().Send(req.Context(), "other.example", make(chan int))
			})
			return nil
		},
	}
	h := plugintest.New(t).WithName("badsend").WithPeers("other.example")
	routes := h.Routes(def)

	_, err := routes.Call(t, "POST /send", plugintest.Request{})
	assert.Error(t, err)
	assert.Empty(t, h.PeerSends(), "失敗した Send は記録しない")
}

// Enqueue は **本番と同じ理由で** Jobs 未宣言を拒否する。ここで通すと、
// 本番では通らない経路をテストが緑で通してしまう。
func TestHarness_EnqueueRequiresJobs(t *testing.T) {
	h := plugintest.New(t)

	routesOnly := plugin.Definition{
		Name: "demo", APIVersion: plugin.APIVersion,
		Routes: func(plugin.Context, plugin.Router) error { return nil },
	}
	h.Routes(routesOnly)
	err := h.Context().Queue().Enqueue(context.Background(), "prune", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Definition.Jobs")
	assert.Empty(t, h.Enqueues())

	withJobs := routesOnly
	withJobs.Jobs = func(plugin.Context, plugin.Jobs) error { return nil }
	h.Routes(withJobs)
	require.NoError(t, h.Context().Queue().Enqueue(context.Background(), "prune",
		map[string]int{"a": 1}, plugin.WithDelay(time.Minute)))

	got := h.Enqueues()
	require.Len(t, got, 1)
	assert.Equal(t, "prune", got[0].Name)
	assert.JSONEq(t, `{"a":1}`, string(got[0].Payload))
	assert.Equal(t, time.Minute, got[0].Options.Delay)
}

// **Definition.Peer で登録したものが DeliverPeer から叩けること (#2819)。**
// doc は登録を Peer へ移すよう指示しているので、ハーネスが同じ形を実行できないと
// 「移したらテストが落ちる」ことになる。
func TestHarness_PeerCallback(t *testing.T) {
	var gotFrom string
	var gotReply string
	def := plugin.Definition{
		Name: "demo", APIVersion: plugin.APIVersion, Peered: true,
		Peer: func(_ plugin.Context, p plugin.Peer) error {
			p.Handle(func(_ context.Context, from string, payload json.RawMessage) (any, error) {
				gotFrom = from
				return map[string]any{"echo": json.RawMessage(payload)}, nil
			})
			p.OnReply(func(_ context.Context, _, _ string, reply json.RawMessage) error {
				gotReply = string(reply)
				return nil
			})
			return nil
		},
	}

	h := plugintest.New(t)
	h.Peer(def)

	res, err := h.DeliverPeer("other.example", map[string]int{"a": 1})
	require.NoError(t, err)
	assert.Equal(t, "other.example", gotFrom)
	assert.NotNil(t, res)

	require.NoError(t, h.DeliverPeerReply("other.example", "id1", map[string]int{"b": 2}))
	assert.JSONEq(t, `{"b":2}`, gotReply)
}

// Peer が nil でも落ちない (宣言していないプラグインもハーネスに通せる)。
func TestHarness_PeerCallbackNil(t *testing.T) {
	h := plugintest.New(t)
	h.Peer(plugin.Definition{Name: "demo", APIVersion: plugin.APIVersion})
	assert.Empty(t, h.PeerSends())
}

// **`ctx.HTTP()` の既定は必ず失敗する client (#3037)。**
//
// 素の `&http.Client{}` を既定にすると、テストが気付かないうちに**本物の
// 外部サービスへ出ていく**。本番の `ctx.HTTP()` は SSRF ガードと運営者の
// proxy 設定を持つので、テストでも明示的に差し替えさせる。
func TestHarness_HTTPDefaultsToFailing(t *testing.T) {
	h := plugintest.New(t)
	client := h.Context().HTTP()
	require.NotNil(t, client, "nil を返すとプラグイン側が nil 参照 panic になる")

	_, err := client.Get("https://example.com/")
	require.Error(t, err, "未設定の client が本当に外へ出ている")
	assert.Contains(t, err.Error(), "WithHTTPClient")
}

// 差し替えたものがそのまま返ること。
func TestHarness_WithHTTPClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	h := plugintest.New(t).WithHTTPClient(srv.Client())
	res, err := h.Context().HTTP().Get(srv.URL)
	require.NoError(t, err)
	defer res.Body.Close() //nolint:errcheck // テスト
	assert.Equal(t, http.StatusOK, res.StatusCode)
}
