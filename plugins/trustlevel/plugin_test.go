package trustlevel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/elythia-network/elythia/plugin"
	"github.com/elythia-network/elythia/plugin/plugintest"
	_ "github.com/jackc/pgx/v5/stdlib"
)

/*
 * plugin/plugintest で mk-go を起動せずにジョブとルートを直接叩く。
 *
 * DB だけは本物を使う。SQL の挙動を模したフェイクは本物とずれ、通ったのに本番で
 * 落ちるテストになる。
 */

const testSchema = "plugin_trustlevel_test"

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	base := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		envOr("TEST_DB_HOST", "localhost"), envOr("TEST_DB_PORT", "5432"),
		envOr("TEST_DB_USER", "mk"), envOr("TEST_DB_PASS", "mk"),
		envOr("TEST_DB_NAME", "misskey_test"))

	admin, err := sql.Open("pgx", base)
	if err != nil {
		dbUnavailable(t, err)
	}
	defer admin.Close() //nolint:errcheck // 使い捨て
	if err := admin.Ping(); err != nil {
		dbUnavailable(t, err)
	}
	for _, q := range []string{
		`DROP SCHEMA IF EXISTS ` + testSchema + ` CASCADE`,
		`CREATE SCHEMA ` + testSchema,
	} {
		if _, err := admin.Exec(q); err != nil {
			t.Fatal(err)
		}
	}

	db, err := sql.Open("pgx", base+" search_path="+testSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		if a, err := sql.Open("pgx", base); err == nil {
			_, _ = a.Exec(`DROP SCHEMA IF EXISTS ` + testSchema + ` CASCADE`)
			_ = a.Close()
		}
	})
	return db
}

// stubAPI answers admin/show-users and admin/roles/assign without a running
// mk-go.
type stubAPI struct {
	// users は admin/show-users が offset 順に返す集合。
	users []map[string]any
	// assigned は admin/roles/assign を受けた userId。
	assigned []string
	// assignErr を返すと付与失敗になる。
	assignErr error
	// actor は AsUser に渡された ID。**主体が誰かを確かめる。**
	actor string
	// listErr は admin/show-users を失敗させる。
	listErr error
}

func (s *stubAPI) Anonymous() plugin.Caller { return &stubCaller{api: s} }

func (s *stubAPI) AsUser(userID string) plugin.Caller {
	s.actor = userID
	return &stubCaller{api: s}
}

type stubCaller struct{ api *stubAPI }

func (c *stubCaller) Call(_ context.Context, endpoint string, params any) (json.RawMessage, error) {
	p, _ := params.(map[string]any)
	switch endpoint {
	case "admin/show-users":
		if c.api.listErr != nil {
			return nil, c.api.listErr
		}
		offset, _ := p["offset"].(int)
		limit, _ := p["limit"].(int)
		if offset >= len(c.api.users) {
			return json.Marshal([]map[string]any{})
		}
		end := min(offset+limit, len(c.api.users))
		return json.Marshal(c.api.users[offset:end])
	case "admin/roles/assign":
		if c.api.assignErr != nil {
			return nil, c.api.assignErr
		}
		c.api.assigned = append(c.api.assigned, p["userId"].(string))
		return json.RawMessage(`null`), nil
	}
	return nil, fmt.Errorf("stub: 未対応の endpoint %q", endpoint)
}

func localUser(id string, ageDays, notes int, suspended bool) map[string]any {
	return map[string]any{
		"id":          id,
		"host":        nil,
		"createdAt":   time.Now().Add(-time.Duration(ageDays) * 24 * time.Hour).Format(time.RFC3339),
		"notesCount":  notes,
		"isSuspended": suspended,
	}
}

func newHarness(t *testing.T, api plugin.API, overrides map[string]any) *plugintest.Harness {
	t.Helper()
	cfg := map[string]any{
		"roleId":            "role-1",
		"actorId":           "admin-1",
		"minAccountAgeDays": 7,
		"minNotes":          10,
		"pageSize":          2,
	}
	for k, v := range overrides {
		cfg[k] = v
	}
	return plugintest.New(t).
		WithName("trustlevel").
		WithDB(testDB(t)).
		WithConfig(cfg).
		WithAPI(api)
}

// **条件を満たした利用者にだけロールが付く。**
func TestReconcile_GrantsQualifyingUsers(t *testing.T) {
	api := &stubAPI{users: []map[string]any{
		localUser("u-ok", 30, 50, false),
		localUser("u-young", 1, 50, false),
		localUser("u-quiet", 30, 3, false),
		localUser("u-suspended", 30, 50, true),
	}}
	h := newHarness(t, api, nil)
	jobs := h.Jobs(Plugin)

	require(t, jobs.Run(t, "reconcile", ""))

	assertSlice(t, []string{"u-ok"}, api.assigned)
	// 管理操作は設定された管理者の権限で行う (`AsSystem` は無い)。
	assertEqual(t, "admin-1", api.actor)
}

// **409 は目的の状態。** 冪等に回すので、既に付いていることを異常系にすると
// reconcile のたびに失敗が積み上がる。
func TestReconcile_AlreadyAssignedIsSuccess(t *testing.T) {
	api := &stubAPI{
		users:     []map[string]any{localUser("u-ok", 30, 50, false)},
		assignErr: &plugin.APIError{Endpoint: "admin/roles/assign", Status: http.StatusConflict},
	}
	h := newHarness(t, api, nil)
	jobs := h.Jobs(Plugin)

	require(t, jobs.Run(t, "reconcile", ""))

	db := h.Context().Storage().DB()
	var granted bool
	var lastErr string
	require(t, db.QueryRow(
		`SELECT granted, last_error FROM subjects WHERE user_id = 'u-ok'`).
		Scan(&granted, &lastErr))
	if !granted {
		t.Fatal("409 を成功として扱っていない")
	}
	if lastErr != "" {
		t.Fatalf("409 でエラーを記録している: %q", lastErr)
	}
}

// **主体の管理者が使えないときに原因が残る。** 残らないと「なぜか昇格が
// 止まっている」を外から追えない。
func TestReconcile_RecordsAssignFailure(t *testing.T) {
	api := &stubAPI{
		users:     []map[string]any{localUser("u-ok", 30, 50, false)},
		assignErr: &plugin.APIError{Endpoint: "admin/roles/assign", Status: http.StatusForbidden, Body: json.RawMessage(`{"error":{"code":"ACCESS_DENIED"}}`)},
	}
	h := newHarness(t, api, nil)
	jobs := h.Jobs(Plugin)

	// **個々の付与失敗ではジョブを失敗にしない。** エラーを返すと queue が 1 周
	// まるごと再試行するが、主体の管理者が使えないなら全員が失敗するので、
	// 同じところで止まってキューを埋めるだけ。原因は記録側で見せる。
	require(t, jobs.Run(t, "reconcile", ""))

	db := h.Context().Storage().DB()
	var granted bool
	var lastErr string
	require(t, db.QueryRow(
		`SELECT granted, last_error FROM subjects WHERE user_id = 'u-ok'`).
		Scan(&granted, &lastErr))
	if granted {
		t.Fatal("失敗したのに付与済みになっている")
	}
	if lastErr == "" {
		t.Fatal("失敗の内容が残っていない")
	}

	// 実行記録にも失敗件数が残り、管理画面から気づける。
	var failed int
	require(t, db.QueryRow(`SELECT failed FROM runs ORDER BY id DESC LIMIT 1`).Scan(&failed))
	if failed != 1 {
		t.Fatalf("失敗件数が記録されていない: %d", failed)
	}
}

// 保留は運営者の判断。**条件を満たしても覆さない。**
func TestReconcile_HeldUserIsNotGranted(t *testing.T) {
	api := &stubAPI{users: []map[string]any{localUser("u-held", 30, 50, false)}}
	h := newHarness(t, api, nil)
	handlers := h.Routes(Plugin)

	_, err := handlers.Call(t, "POST /admin/hold", plugintest.Request{
		Moderator: true, Body: `{"userId":"u-held","held":true}`,
	})
	require(t, err)

	require(t, h.Jobs(Plugin).Run(t, "reconcile", ""))
	if len(api.assigned) != 0 {
		t.Fatalf("保留中なのに付与された: %v", api.assigned)
	}
}

// 走査は 1 ページで終わらない。**ページ送りを間違えると途中で打ち切られる。**
func TestReconcile_PagesThroughAllUsers(t *testing.T) {
	var users []map[string]any
	for i := range 5 {
		users = append(users, localUser(fmt.Sprintf("u-%d", i), 30, 50, false))
	}
	api := &stubAPI{users: users}
	h := newHarness(t, api, nil) // pageSize=2 なので 3 ページ
	require(t, h.Jobs(Plugin).Run(t, "reconcile", ""))

	if len(api.assigned) != 5 {
		t.Fatalf("全員に付いていない: %v", api.assigned)
	}
}

// **1 周の実測を残す。** サブ issue を切るときの根拠になる。
func TestReconcile_RecordsRun(t *testing.T) {
	api := &stubAPI{users: []map[string]any{
		localUser("u-ok", 30, 50, false),
		localUser("u-young", 1, 50, false),
	}}
	h := newHarness(t, api, nil)
	require(t, h.Jobs(Plugin).Run(t, "reconcile", ""))

	handlers := h.Routes(Plugin)
	got, err := handlers.Call(t, "POST /admin/overview", plugintest.Request{Moderator: true})
	require(t, err)

	m := got.(map[string]any)
	runs := m["runs"].([]runEntry)
	if len(runs) != 1 {
		t.Fatalf("実行記録が残っていない: %v", runs)
	}
	if runs[0].Scanned != 2 || runs[0].Granted != 1 {
		t.Fatalf("集計が合わない: %+v", runs[0])
	}
}

// 一覧の取得に失敗しても、その回の記録は残す。
// **落ちた回こそ、あとで原因を追いたくなる。**
func TestReconcile_RecordsFailedRun(t *testing.T) {
	api := &stubAPI{listErr: fmt.Errorf("boom")}
	h := newHarness(t, api, nil)

	if err := h.Jobs(Plugin).Run(t, "reconcile", ""); err == nil {
		t.Fatal("一覧の失敗が握り潰されている")
	}

	db := h.Context().Storage().DB()
	var msg string
	require(t, db.QueryRow(`SELECT error FROM runs ORDER BY id DESC LIMIT 1`).Scan(&msg))
	if msg == "" {
		t.Fatal("失敗した回の記録が残っていない")
	}
}

// **管理用のルートは自分で守る。** Router は認証の有無しか見ない。
func TestAdminRoutes_RequireModerator(t *testing.T) {
	h := newHarness(t, &stubAPI{}, nil)
	handlers := h.Routes(Plugin)

	for _, route := range []string{"POST /admin/overview", "POST /admin/subjects", "POST /admin/hold"} {
		t.Run(route, func(t *testing.T) {
			_, err := handlers.Call(t, route, plugintest.Request{UserID: "u-1"})
			var se *plugin.StatusError
			if !asStatusError(err, &se) || se.Status != http.StatusForbidden {
				t.Fatalf("モデレーター以外を弾いていない: %v", err)
			}
		})
	}
}

// 設定漏れは起動で落とす。**黙って何もしないと運営者が原因を追えない。**
func TestConfig_RejectsMissingValues(t *testing.T) {
	tests := map[string]map[string]any{
		// **片方だけ空は書き途中か書き間違い。** 落として気づかせる。
		"roleId だけ空":  {"roleId": ""},
		"actorId だけ空": {"actorId": ""},
		"pageSize 過大": {"pageSize": 500},
		"pageSize 0":  {"pageSize": 0},
		"minNotes 負":  {"minNotes": -1},
		"minAge 負":    {"minAccountAgeDays": -1},
	}
	for name, override := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, &stubAPI{}, override)
			if _, err := loadConfig(h.Context()); err == nil {
				t.Fatal("不正な設定が通っている")
			}
		})
	}
}

// **未設定では何も登録せず、起動も止めない。** 同梱プラグインはビルドに含まれた
// だけで有効になる (runtime の enabled は既定 true) ので、config を書いていない
// インスタンスがルート登録のエラーで全断する。
func TestUnconfigured_RegistersNothingAndDoesNotFail(t *testing.T) {
	h := newHarness(t, &stubAPI{}, map[string]any{"roleId": "", "actorId": ""})

	jobs := h.Jobs(Plugin)
	if len(jobs.Schedules) != 0 || len(jobs.Handlers) != 0 {
		t.Fatalf("未設定なのにジョブが登録された: %+v", jobs.Schedules)
	}

	handlers := h.Routes(Plugin)
	if len(handlers) != 0 {
		t.Fatalf("未設定なのにルートが登録された: %v", handlers)
	}
}

// 定期実行が登録されていること。
func TestJobs_SchedulesReconcile(t *testing.T) {
	h := newHarness(t, &stubAPI{}, map[string]any{"cron": "*/5 * * * *"})
	jobs := h.Jobs(Plugin)

	if len(jobs.Schedules) != 1 {
		t.Fatalf("定期実行が登録されていない: %v", jobs.Schedules)
	}
	if jobs.Schedules[0].Cron != "*/5 * * * *" || jobs.Schedules[0].Name != "reconcile" {
		t.Fatalf("登録内容が違う: %+v", jobs.Schedules[0])
	}
}

func require(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func assertEqual[T comparable](t *testing.T, want T, got T) {
	t.Helper()
	if want != got {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func assertSlice[T comparable](t *testing.T, want, got []T) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("want %v, got %v", want, got)
		}
	}
}

// asStatusError unwraps a plugin.StatusError.
func asStatusError(err error, target **plugin.StatusError) bool {
	return errors.As(err, target)
}

// dbUnavailable decides what to do when the test database cannot be reached.
//
// **CI では skip を許さない。** skip は成功として扱われるので、接続に失敗した
// ことに気づかないまま緑になる = 無検証で通る (#2588)。手元では PostgreSQL を
// 用意していない開発者のために skip したいので、CI 側だけ環境変数で切り替える。
func dbUnavailable(t *testing.T, err error) {
	t.Helper()
	if os.Getenv("MK_PLUGIN_TESTS_REQUIRE_DB") != "" {
		t.Fatalf("PostgreSQL に接続できません (MK_PLUGIN_TESTS_REQUIRE_DB が設定されているので skip しません): %v", err)
	}
	t.Skipf("PostgreSQL に接続できません: %v", err)
}
