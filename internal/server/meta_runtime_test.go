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
	return newMetaRuntimeHarnessWith(t, metaRuntimeOptions{onlyServer: "1"}, beforeSetup...)
}

// metaRuntimeOptions varies the process the harness builds.
type metaRuntimeOptions struct {
	// onlyServer is MK_ONLY_SERVER ("" runs the queue role too).
	onlyServer string
	// pubsubDown points the pub/sub client at a stopped Redis, so the process
	// receives no internal:metaUpdated, not even its own.
	pubsubDown bool
	// startBackground calls StartBackground after the construction, for a
	// test that needs the parts of the generation running (the reaction
	// flush ticker). Construction alone starts none of them (#3703).
	startBackground bool
}

func newMetaRuntimeHarnessWith(t *testing.T, opts metaRuntimeOptions, beforeSetup ...func(*Server)) *metaRuntimeHarness {
	t.Helper()
	if serverIntegrationDB == nil {
		t.Skip("PostgreSQL unavailable")
	}
	db := serverIntegrationDB
	t.Setenv(config.EnvOnlyServer, opts.onlyServer)
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
	if opts.pubsubDown {
		down := miniredis.RunT(t)
		pubsubClient := redis.NewClient(&redis.Options{Addr: down.Addr(), MaxRetries: -1})
		down.Close()
		t.Cleanup(func() { _ = pubsubClient.Close() })
		redisClients.Pubsub = pubsubClient
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
	if opts.startBackground {
		require.NoError(t, srv.StartBackground())
	}
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

// noteReactionsInDB reads the reaction counts stored on the note row.
func (h *metaRuntimeHarness) noteReactionsInDB(noteID string) map[string]int {
	h.t.Helper()
	var n model.Note
	require.NoError(h.t, h.db.Select("reactions").Take(&n, "id = ?", noteID).Error)
	out := map[string]int{}
	if len(n.Reactions) > 0 {
		require.NoError(h.t, json.Unmarshal(n.Reactions, &out))
	}
	return out
}

// noteReactionsShown reads the reaction counts the API returns.
func (h *metaRuntimeHarness) noteReactionsShown(noteID string) map[string]int {
	h.t.Helper()
	rec := h.post("/api/notes/show", h.rootToken, map[string]any{"noteId": noteID})
	require.Equal(h.t, http.StatusOK, rec.Code, rec.Body.String())
	var body struct {
		Reactions map[string]int `json:"reactions"`
	}
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body.Reactions
}

func (h *metaRuntimeHarness) react(token, noteID string) {
	h.t.Helper()
	rec := h.post("/api/notes/reactions/create", token, map[string]any{"noteId": noteID, "reaction": "👍"})
	require.Equal(h.t, http.StatusNoContent, rec.Code, rec.Body.String())
}

func TestMetaRuntime_ReactionsBufferingSwitchesWithoutRebuild(t *testing.T) {
	h := newMetaRuntimeHarness(t)
	tokens := []string{"metartuser000001", "metartuser000002", "metartuser000003"}
	for i, tok := range tokens {
		h.createUser("metart-user-"+strconv.Itoa(i), "metart_user"+strconv.Itoa(i), tok, false)
	}
	noteID := h.createNote(h.rootID, "react here")
	bufferKey := "reaction-buffer:" + noteID

	// 無効のまま: DB へ直接書く。
	h.react(tokens[0], noteID)
	require.Equal(t, map[string]int{"👍": 1}, h.noteReactionsInDB(noteID))
	require.False(t, h.mr.Exists(bufferKey))

	// 同じプロセスの管理画面で有効にする: Redis へ溜める。
	h.updateMetaHere(map[string]any{"enableReactionsBuffering": true})
	h.react(tokens[1], noteID)
	require.True(t, h.mr.Exists(bufferKey), "buffering must apply right after the admin enables it")
	require.Equal(t, map[string]int{"👍": 1}, h.noteReactionsInDB(noteID))
	require.Equal(t, map[string]int{"👍": 2}, h.noteReactionsShown(noteID))

	// 別のプロセスが無効にする (通知は 1 回)。溜まった分は消さず読みに足す。
	h.updateMetaElsewhere(map[string]any{"enableReactionsBuffering": false})
	require.Equal(t, map[string]int{"👍": 2}, h.noteReactionsShown(noteID))

	// flush の前に、溜めた側の人が取り消す。DB へ直接書く前に溜まった分を移すので、
	// 数は 1 に戻り、Redis には何も残らない (#3663 のレビュー M-1)。
	h.unreact(tokens[1], noteID)
	require.Equal(t, map[string]int{"👍": 1}, h.noteReactionsInDB(noteID))
	require.False(t, h.mr.Exists(bufferKey), "the buffered count must be moved before the direct write")
	require.Equal(t, map[string]int{"👍": 1}, h.noteReactionsShown(noteID))

	h.react(tokens[2], noteID)
	require.Equal(t, map[string]int{"👍": 2}, h.noteReactionsInDB(noteID))
	require.Equal(t, map[string]int{"👍": 2}, h.noteReactionsShown(noteID))
}

func (h *metaRuntimeHarness) unreact(token, noteID string) {
	h.t.Helper()
	rec := h.post("/api/notes/reactions/delete", token, map[string]any{"noteId": noteID})
	require.Equal(h.t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// 無効で起動しても、queue の役割は flush の ticker を回す。起動時の値で
// ticker の有無を決めていたので、後から有効にしても誰も flush しなかった。
func TestMetaRuntime_ReactionFlushTickerRunsWhenStartedDisabled(t *testing.T) {
	started := make(chan func() error, 1)
	_ = newMetaRuntimeHarnessWith(t, metaRuntimeOptions{startBackground: true}, func(s *Server) {
		s.reactionFlushWorker = func(ctx context.Context, fn func() error) {
			started <- fn
			<-ctx.Done()
		}
	})
	var enqueue func() error
	select {
	case enqueue = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the reaction flush worker must start even when buffering is off at startup")
	}

	// 起動直後は前の世代の残りを移す期限の内なので、flush を積みに行く。
	// miniredis は mkq の Lua (cmsgpack) を実行できないので、積みに行ったことを
	// mkq のエラーで確かめる (積まずに返すと nil になる)。
	err := enqueue()
	require.Error(t, err)
	require.Contains(t, err.Error(), "mkq")
}

// 管理画面で変えたプロセスは、pub/sub が届かなくても (自分の通知も届かない)
// その場で切り替える。metaUpdated の購読だけに頼ると、Redis の pub/sub が
// 切れている間は変えたプロセスでさえ効かない。
func TestMetaRuntime_AppliesInThisProcessWithoutPubSub(t *testing.T) {
	h := newMetaRuntimeHarnessWith(t, metaRuntimeOptions{onlyServer: "1", pubsubDown: true})
	token := "metartuser000009"
	h.createUser("metart-user-9", "metart_user9", token, false)
	noteID := h.createNote(h.rootID, "react here")

	require.Equal(t, "before", h.nodeinfoName())

	h.updateMetaHere(map[string]any{"enableReactionsBuffering": true, "name": "changed here"})
	h.react(token, noteID)
	require.True(t, h.mr.Exists("reaction-buffer:"+noteID))
	require.Equal(t, "changed here", h.nodeinfoName())
}

// nodeinfoName reads metadata.nodeName from /nodeinfo/2.1.
func (h *metaRuntimeHarness) nodeinfoName() string {
	h.t.Helper()
	rec := h.get("/nodeinfo/2.1")
	require.Equal(h.t, http.StatusOK, rec.Code, rec.Body.String())
	var doc struct {
		Metadata struct {
			NodeName string `json:"nodeName"`
		} `json:"metadata"`
	}
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &doc))
	return doc.Metadata.NodeName
}

func TestMetaRuntime_NodeinfoFollowsMetaUpdated(t *testing.T) {
	h := newMetaRuntimeHarness(t)
	require.Equal(t, "before", h.nodeinfoName())

	h.updateMetaHere(map[string]any{"name": "changed here"})
	require.Equal(t, "changed here", h.nodeinfoName())

	h.updateMetaElsewhere(map[string]any{"name": "changed elsewhere"})
	require.Equal(t, "changed elsewhere", h.nodeinfoName())
}

// web だけのプロセスも ticker を回し (設定の読み直しのため)、flush は積まない。
func TestMetaRuntime_ReactionTickerRunsInServerOnlyRoleWithoutEnqueue(t *testing.T) {
	started := make(chan func() error, 1)
	_ = newMetaRuntimeHarnessWith(t, metaRuntimeOptions{onlyServer: "1", startBackground: true}, func(s *Server) {
		s.reactionFlushWorker = func(ctx context.Context, fn func() error) {
			started <- fn
			<-ctx.Done()
		}
	})
	var tick func() error
	select {
	case tick = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the reaction ticker must run in the server-only role to re-read the setting")
	}
	// 起動直後の窓の内でも、queue の役割でなければ積みに行かない (積みに行くと
	// miniredis が mkq の Lua を実行できずにエラーになる)。
	require.NoError(t, tick())
}

// web だけのプロセスも tick で meta から設定を読み直す。通知を取りこぼして
// 有効のまま溜め続けている状態を作り、tick で DB の値 (無効) に揃うことを見る。
func TestMetaRuntime_ReactionTickReloadsInServerOnlyRole(t *testing.T) {
	started := make(chan func() error, 1)
	h := newMetaRuntimeHarnessWith(t, metaRuntimeOptions{onlyServer: "1", startBackground: true}, func(s *Server) {
		s.reactionFlushWorker = func(ctx context.Context, fn func() error) {
			started <- fn
			<-ctx.Done()
		}
	})
	var tick func() error
	select {
	case tick = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the reaction ticker did not start")
	}
	w := h.srv.reactionCountWriter
	require.NotNil(t, w)
	w.SetBuffering(true)
	require.NoError(t, tick())
	require.False(t, w.Buffering(), "a web-only process must re-read enableReactionsBuffering on each tick")
}

// metaUpdatedHandled は購読の処理が全部終わった後に呼ぶ。前で呼ぶと、
// updateMetaElsewhere の待ちが処理の終わりを待たなくなり、テストが処理の
// 速さに頼って通る。呼ばれた時点で効いていることを、その場で確かめる。
func TestMetaRuntime_HandledHookRunsAfterProcessing(t *testing.T) {
	h := newMetaRuntimeHarness(t)
	require.Equal(t, "before", h.nodeinfoName())
	require.NoError(t, h.db.Model(&model.Meta{}).Where("1 = 1").Update("name", "after hook").Error)

	const marker = `"metart-hook-order"`
	seen := make(chan string, 1)
	hook := func(payload []byte) {
		if string(payload) != marker {
			return
		}
		// 別の goroutine なので require は使わない。
		req := httptest.NewRequest(http.MethodGet, "/nodeinfo/2.1", nil)
		rec := httptest.NewRecorder()
		h.srv.Handler().ServeHTTP(rec, req)
		var doc struct {
			Metadata struct {
				NodeName string `json:"nodeName"`
			} `json:"metadata"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &doc)
		select {
		case seen <- doc.Metadata.NodeName:
		default:
		}
	}
	h.srv.metaUpdatedHandled.Store(&hook)
	defer h.srv.metaUpdatedHandled.Store(nil)
	require.NoError(t, h.rdb.Publish(context.Background(), "internal:metaUpdated", marker).Err())
	select {
	case name := <-seen:
		require.Equal(t, "after hook", name, "the hook must run after the handler has dropped the caches")
	case <-time.After(10 * time.Second):
		t.Fatal("this process did not handle internal:metaUpdated")
	}
}
