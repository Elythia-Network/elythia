package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/labstack/echo/v4"
	redis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// metaRuntimeHarness is a full server whose meta row the test changes, either
// through the admin API of the same process or the way another process does it
// (write the row, then publish internal:metaUpdated) (#3663).
type metaRuntimeHarness struct {
	t     *testing.T
	db    *gorm.DB
	mr    *miniredis.Miniredis
	rdb   *redis.Client
	srv   *Server
	idGen id.Generator
	// rootToken は管理者 (root) の native token。
	rootToken string
	rootID    string
	// barrierSeq numbers the notifications updateMetaElsewhere publishes.
	barrierSeq int
}

// metaRuntimeColumns are the meta columns the #3663 tests change. 元の値を
// 戻さないと、同じパッケージの後続のテストが変えた値で起動する。
var metaRuntimeColumns = []string{"name", "deeplAuthKey", "deeplIsPro", "enableReactionsBuffering"}

func newMetaRuntimeHarness(t *testing.T, beforeSetup ...func(*Server)) *metaRuntimeHarness {
	t.Helper()
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	db := serverIntegrationDB
	t.Setenv(config.EnvOnlyServer, "1")
	t.Setenv(config.EnvOnlyQueue, "")
	// ドライブのファイルは作業ディレクトリ相対に置かれるので、リポジトリを汚さない。
	t.Chdir(t.TempDir())

	require.NoError(t, repository.NewMetaRepository(db).EnsureInitial("meta-runtime-meta"))
	var saved map[string]any
	require.NoError(t, db.Model(&model.Meta{}).Select(metaRuntimeColumns).Take(&saved).Error)
	t.Cleanup(func() {
		require.NoError(t, db.Model(&model.Meta{}).Where("1 = 1").Updates(saved).Error)
	})
	require.NoError(t, db.Model(&model.Meta{}).Where("1 = 1").Updates(map[string]any{
		"name": "before", "deeplAuthKey": nil, "deeplIsPro": false, "enableReactionsBuffering": false,
	}).Error)

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
	}

	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h := &metaRuntimeHarness{t: t, db: db, mr: mr, rdb: redisClient, idGen: idGen,
		rootID: "metart-root", rootToken: "metartroot000001"}
	h.cleanupUsers()
	t.Cleanup(h.cleanupUsers)
	h.createUser(h.rootID, "metart_root", h.rootToken, true)

	restoreProcessGlobals(t)
	srv, err := newServer(cfg, db, redisClients, nil, noopStorage, beforeSetup...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	h.srv = srv
	return h
}

func (h *metaRuntimeHarness) cleanupUsers() {
	h.t.Helper()
	var ids []string
	require.NoError(h.t, h.db.Model(&model.User{}).Where(`"usernameLower" LIKE ?`, "metart\\_%").Pluck("id", &ids).Error)
	if len(ids) == 0 {
		return
	}
	require.NoError(h.t, h.db.Where(`"userId" IN ?`, ids).Delete(&model.NoteReaction{}).Error)
	require.NoError(h.t, h.db.Where(`"userId" IN ?`, ids).Delete(&model.Note{}).Error)
	require.NoError(h.t, h.db.Where("id IN ?", ids).Delete(&model.User{}).Error)
}

func (h *metaRuntimeHarness) createUser(userID, username, token string, root bool) {
	h.t.Helper()
	require.NoError(h.t, repository.NewUserRepository(h.db).Create(&model.User{
		ID: userID, Username: username, UsernameLower: username, Token: &token, IsRoot: root,
	}))
}

func (h *metaRuntimeHarness) createNote(userID, text string) string {
	h.t.Helper()
	noteID := h.idGen.Generate(time.Now())
	require.NoError(h.t, repository.NewNoteRepository(h.db).Create(&model.Note{
		ID: noteID, UserID: userID, Text: &text, Visibility: model.NoteVisibilityPublic,
	}))
	return noteID
}

func (h *metaRuntimeHarness) post(path, token string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(h.t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if token != "" {
		req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (h *metaRuntimeHarness) get(path string) *httptest.ResponseRecorder {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// updateMetaHere changes meta through this process's admin API.
func (h *metaRuntimeHarness) updateMetaHere(fields map[string]any) {
	h.t.Helper()
	rec := h.post("/api/admin/update-meta", h.rootToken, fields)
	require.Equal(h.t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// updateMetaElsewhere changes meta the way another process does: it writes the
// row and publishes internal:metaUpdated once, then waits until this process
// has handled that message. このプロセスのキャッシュには直接触らない。
//
// **通知は 1 回だけ送る。** 目印を同じトピックへ続けて送って待つと、購読が
// 2 回走り、購読の中の順序の誤り (キャッシュを捨てる前に読み直す) を
// 2 回目が隠す。待つのは Server.metaUpdatedHandled で、payload の通し番号で
// 自分の送った通知だと見分ける (購読は payload を見ない)。
func (h *metaRuntimeHarness) updateMetaElsewhere(fields map[string]any) {
	h.t.Helper()
	require.NoError(h.t, h.db.Model(&model.Meta{}).Where("1 = 1").Updates(fields).Error)
	h.barrierSeq++
	marker := `"metart-update-` + strconv.Itoa(h.barrierSeq) + `"`
	seen := make(chan struct{})
	var once sync.Once
	hook := func(payload []byte) {
		if string(payload) == marker {
			once.Do(func() { close(seen) })
		}
	}
	h.srv.metaUpdatedHandled.Store(&hook)
	defer h.srv.metaUpdatedHandled.Store(nil)
	require.NoError(h.t, h.rdb.Publish(context.Background(), "internal:metaUpdated", marker).Err())
	select {
	case <-seen:
	case <-time.After(10 * time.Second):
		h.t.Fatal("this process did not handle internal:metaUpdated")
	}
}

// fakeDeepL records what the server sent to DeepL. ホスト名で Free と Pro を
// 見分ける (本物の DeepL へは繋がない)。
type fakeDeepL struct {
	mu    sync.Mutex
	calls []fakeDeepLCall
}

type fakeDeepLCall struct {
	host, auth string
}

func (f *fakeDeepL) last() (fakeDeepLCall, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return fakeDeepLCall{}, 0
	}
	return f.calls[len(f.calls)-1], len(f.calls)
}

// withFakeDeepL routes every outbound connection of the server to a local TLS
// server that answers like DeepL.
func withFakeDeepL(t *testing.T) (*fakeDeepL, func(*Server)) {
	t.Helper()
	fake := &fakeDeepL{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		fake.calls = append(fake.calls, fakeDeepLCall{host: r.Host, auth: r.Header.Get("Authorization")})
		fake.mu.Unlock()
		_, _ = w.Write([]byte(`{"translations":[{"detected_source_language":"JA","text":"translated"}]}`))
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
		// 宛先は手元の偽物なので、証明書のホスト名は合わない。
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // test-only fake DeepL server
	}
	t.Cleanup(tr.CloseIdleConnections)
	return fake, func(s *Server) {
		s.outboundTrOnce.Do(func() { s.outboundTr = tr })
	}
}

func TestMetaRuntime_DeepLSettingsApplyWithoutRebuild(t *testing.T) {
	fake, route := withFakeDeepL(t)
	h := newMetaRuntimeHarness(t, route)
	noteID := h.createNote(h.rootID, "こんにちは")
	translate := func() *httptest.ResponseRecorder {
		return h.post("/api/notes/translate", h.rootToken, map[string]any{"noteId": noteID, "targetLang": "en"})
	}

	rec := translate()
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "UNAVAILABLE")

	// 同じプロセスの管理画面でキーを入れる。
	h.updateMetaHere(map[string]any{"deeplAuthKey": "free-key", "deeplIsPro": false})
	rec = translate()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	call, n := fake.last()
	require.Equal(t, 1, n)
	require.Equal(t, "api-free.deepl.com", call.host)
	require.Equal(t, "DeepL-Auth-Key free-key", call.auth)

	// 別のプロセスがキーと Pro を変える。
	h.updateMetaElsewhere(map[string]any{"deeplAuthKey": "pro-key", "deeplIsPro": true})
	rec = translate()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	call, n = fake.last()
	require.Equal(t, 2, n)
	require.Equal(t, "api.deepl.com", call.host)
	require.Equal(t, "DeepL-Auth-Key pro-key", call.auth)

	// 別のプロセスがキーを外す。
	h.updateMetaElsewhere(map[string]any{"deeplAuthKey": nil})
	rec = translate()
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "UNAVAILABLE")
	_, n = fake.last()
	require.Equal(t, 2, n, "a removed key must not reach DeepL")
}
