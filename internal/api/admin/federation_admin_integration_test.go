package admin_test

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// integrationDB は admin パッケージで testcontainer / 環境変数経由の
// PostgreSQL に接続して使う共有 DB ハンドル。`internal/repository`
// パッケージと同じ pattern (TestMain で open + ApplyMigrations) を踏襲し、
// admin handler の SQL 経路を実 DB で観測するための基盤。
//
// 個別 test は `if integrationDB == nil { t.Skip(...) }` で skip するので
// DB が利用不可な環境 (Docker 無し / TEST_DB_HOST 未設定) でも CI を
// 落とさない。
//
// 将来 admin パッケージに別 integration test が追加された場合もこの
// ハンドルを共有すれば TestMain の重複を避けられる。
var integrationDB *gorm.DB

// TestMain は admin パッケージ全体で一度だけ走る。OpenTestDB が成功
// すれば integrationDB を populate、失敗 (Docker daemon 不在等) なら
// nil のまま個別 test に委ねる (= unit test だけ走る)。
func TestMain(m *testing.M) {
	if db, err := testutil.OpenTestDB(); err == nil {
		testutil.ApplyMigrations(db)
		integrationDB = db
	}
	os.Exit(m.Run())
}

// newIntegrationHandler は実 DB に紐付けた *apiadmin.Handler を返す。
// instanceRepo / modLogService が production 配線と同じ実装を使うので、
// SQL 経由の挙動 (空文字列を `Updates(map)` で書く / moderation_log JSONB
// の永続化) が壊れた場合に detect できる。
//
// admin user 行は moderation_log の FK 制約を満たすために事前に seed する。
// テスト末尾で削除する Cleanup を t.Cleanup で登録。
//
// 注意: t.Parallel() 非対応。共有 integrationDB と固定 host を使う test
// 間で race / unique 制約衝突するため、本 helper を使う test は serial に
// 走らせること。
func newIntegrationHandler(t *testing.T) (*apiadmin.Handler, *model.User) {
	t.Helper()
	if integrationDB == nil {
		t.Skip("integrationDB unavailable (set TEST_DB_HOST or run with Docker daemon for testcontainer)")
	}

	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)

	// Repositories backed by real DB.
	userRepo := repository.NewUserRepository(integrationDB)
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	roleRepo := testutil.NewMockRoleRepository()
	assignRepo := testutil.NewMockRoleAssignmentRepository(roleRepo)
	signupSvc := signup.NewService(userRepo, metaRepo, idGen)
	roleSvc := role.NewService(roleRepo, assignRepo, metaRepo, idGen)

	h := apiadmin.NewHandler(signupSvc, roleSvc, metaRepo, userRepo, idGen)
	h.SetInstanceRepo(repository.NewInstanceRepository(integrationDB))
	h.SetModLogService(moderationlog.New(repository.NewModerationLogRepository(integrationDB), idGen))

	// admin 行を seed (moderation_log.userId FK)。`adminUser` は package
	// 共有 (handler_test.go) の `&model.User{ID: "admin1"}` だが、ここでは
	// 実 user 行を別 ID で作って FK を満たす独自の admin を使う (他 test と
	// 干渉しないため)。
	admin := &model.User{
		ID:            "admin_int_" + idGen.Generate(time.Now()),
		Username:      "int_admin",
		UsernameLower: "int_admin",
	}
	require.NoError(t, integrationDB.Create(admin).Error)
	t.Cleanup(func() {
		// FK 順 (modlog → instance → user) で逐次削除。エラーは無視
		// (テストごとに作成行が異なるので残骸が残ると次回失敗する)。
		integrationDB.Exec(`DELETE FROM "moderation_log" WHERE "userId" = ?`, admin.ID)
		integrationDB.Exec(`DELETE FROM "user" WHERE id = ?`, admin.ID)
	})
	return h, admin
}

// seedTestInstance は host で identify される instance 行を 1 つ seed する。
// テスト末尾で host で削除する Cleanup を t.Cleanup で登録。
//
// 前回 test が異常終了して残骸が DB に残っているケースで Create が UNIQUE
// 制約 (host) で fail しないよう、事前に同 host を pre-delete してから
// 挿入する。これで CI の前回失敗 → 今回 setup 失敗の連鎖を防ぐ。
func seedTestInstance(t *testing.T, host, modNote string, state model.SuspensionState) string {
	t.Helper()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	instID := "inst_int_" + idGen.Generate(time.Now())
	inst := &model.Instance{
		ID:               instID,
		Host:             host,
		FirstRetrievedAt: time.Now(),
		ModerationNote:   modNote,
		SuspensionState:  state,
	}
	// 前回 cleanup の取りこぼしを preemptive に掃除して create を idempotent にする。
	integrationDB.Exec(`DELETE FROM "instance" WHERE host = ?`, host)
	require.NoError(t, integrationDB.Create(inst).Error)
	t.Cleanup(func() {
		integrationDB.Exec(`DELETE FROM "instance" WHERE host = ?`, host)
	})
	return instID
}

// TestFederationUpdateInstance_IntegrationClearsModerationNoteToEmpty は
// #675 で発覚した bug の SQL レベル regression guard (#696)。
//
// シナリオ: pre-state で moderationNote="old"、request で `""` を明示送信
// すると、handler の `req.updates()` が `out["moderationNote"] = ""` を
// 含む map を返し、`gorm.Updates(map)` が `UPDATE instance SET
// "moderationNote" = ” WHERE host = ?` を発行することを確認する。
//
// 旧コード (`*string` ではなく `string`) では `req.updates()` 内で
// `if req.ModerationNote != ""` ガードに弾かれて空文字列が map に入らず、
// SQL が走らないので "old" が残ってしまう regression があった。本 test は
// 実 DB の COL 値を読み返して `""` を直接 assert することでこの差を捕捉。
func TestFederationUpdateInstance_IntegrationClearsModerationNoteToEmpty(t *testing.T) {
	h, admin := newIntegrationHandler(t)
	host := "remote-int-clear.example"
	instID := seedTestInstance(t, host, "old note", model.SuspensionStateNone)

	rec := doPost(h.FederationUpdateInstance,
		`{"host":"`+host+`","moderationNote":""}`, admin)
	require.Equal(t, http.StatusNoContent, rec.Code)

	// SQL レベル: instance 行の moderationNote 列が空文字列に書き換わって
	// いること。GORM struct field 経由で読み返すので、column 名や type が
	// migration から zero value で読めない場合 (= 想定外) も検出できる。
	var got model.Instance
	require.NoError(t, integrationDB.First(&got, "host = ?", host).Error)
	assert.Equal(t, "", got.ModerationNote, "moderationNote should be persisted as empty string")
	assert.Equal(t, instID, got.ID, "row identity preserved across update")

	// moderation_log 行も実 INSERT されていること: `updateRemoteInstanceNote`
	// type で before/after JSON が正確に書かれているか jsonb から読み返す。
	require.Eventually(t, func() bool {
		var c int64
		integrationDB.Raw(
			`SELECT COUNT(*) FROM "moderation_log" WHERE "userId" = ? AND "type" = ?`,
			admin.ID, "updateRemoteInstanceNote",
		).Scan(&c)
		return c == 1
	}, 500*time.Millisecond, 5*time.Millisecond,
		"updateRemoteInstanceNote log row should be persisted")

	// jsonb 列は GORM Raw().Scan(&[]byte) では direct scan できないので、
	// model.ModerationLog 経由で読み返す (Info は datatypes.JSON 型で
	// []byte として扱える)。
	var logRow model.ModerationLog
	require.NoError(t, integrationDB.Where(
		`"userId" = ? AND "type" = ?`, admin.ID, "updateRemoteInstanceNote",
	).First(&logRow).Error)
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(logRow.Info, &parsed))
	assert.Equal(t, "old note", parsed["before"], "log captures pre-state value")
	assert.Equal(t, "", parsed["after"], "log captures cleared empty string")
	assert.Equal(t, host, parsed["host"])
	assert.Equal(t, instID, parsed["id"])
}

// TestFederationUpdateInstance_IntegrationSuspendsInstance は #715 / #724
// 修正の regression guard。handler が `isSuspended bool` を `suspensionState`
// enum に変換して GORM Updates に渡し、実 DB の suspensionState 列が更新
// されることを確認する。修正前は `out["isSuspended"] = bool` が存在しない
// 列への UPDATE になり silently NO-OP だった。
func TestFederationUpdateInstance_IntegrationSuspendsInstance(t *testing.T) {
	h, admin := newIntegrationHandler(t)
	host := "remote-int-suspend.example"
	instID := seedTestInstance(t, host, "", model.SuspensionStateNone)

	// suspend
	rec := doPost(h.FederationUpdateInstance,
		`{"host":"`+host+`","isSuspended":true}`, admin)
	require.Equal(t, http.StatusNoContent, rec.Code)

	var got model.Instance
	require.NoError(t, integrationDB.First(&got, "host = ?", host).Error)
	assert.Equal(t, model.SuspensionStateManuallySuspended, got.SuspensionState,
		"suspensionState column should be updated by handler conversion")
	assert.Equal(t, instID, got.ID)

	// moderation log
	require.Eventually(t, func() bool {
		var c int64
		integrationDB.Raw(
			`SELECT COUNT(*) FROM "moderation_log" WHERE "userId" = ? AND "type" = ?`,
			admin.ID, "suspendRemoteInstance",
		).Scan(&c)
		return c == 1
	}, 500*time.Millisecond, 5*time.Millisecond,
		"suspendRemoteInstance log should be persisted")

	// unsuspend
	rec = doPost(h.FederationUpdateInstance,
		`{"host":"`+host+`","isSuspended":false}`, admin)
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.NoError(t, integrationDB.First(&got, "host = ?", host).Error)
	assert.Equal(t, model.SuspensionStateNone, got.SuspensionState,
		"suspensionState should be reset to none on unsuspend")

	require.Eventually(t, func() bool {
		var c int64
		integrationDB.Raw(
			`SELECT COUNT(*) FROM "moderation_log" WHERE "userId" = ? AND "type" = ?`,
			admin.ID, "unsuspendRemoteInstance",
		).Scan(&c)
		return c == 1
	}, 500*time.Millisecond, 5*time.Millisecond,
		"unsuspendRemoteInstance log should be persisted")
}
