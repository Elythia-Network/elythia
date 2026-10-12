package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/backup/restoreop"
	"github.com/elythia-network/elythia/internal/core/strongauth"
	"github.com/elythia-network/elythia/internal/maintmode"
	"github.com/elythia-network/elythia/internal/model"
)

// apiDB is a database side that only tracks names.
type apiDB struct {
	mu  sync.Mutex
	dbs map[string]bool
	// loadErr is returned by Load.
	loadErr error
	// migrationsErr, rollbackErr, kept and keptErr are what the rollback
	// checks and the listing return.
	migrationsErr, rollbackErr error
	kept                       []backup.KeptDatabase
	keptErr                    error
}

func (a *apiDB) Load(_ context.Context, id string) (*backup.RestorePlan, error) {
	if a.loadErr != nil {
		return nil, a.loadErr
	}
	return &backup.RestorePlan{ID: id, Verified: true}, nil
}
func (a *apiDB) CheckTarget(context.Context, backup.RestoreOptions) error { return nil }
func (a *apiDB) CheckCompatibility(context.Context, *backup.RestorePlan, backup.RestoreOptions) error {
	return nil
}
func (a *apiDB) RestoreInto(_ context.Context, _ *backup.RestorePlan, _ backup.RestoreOptions, name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dbs[name] = true
	return nil
}
func (a *apiDB) VerifyRestored(context.Context, string, backup.Meta) error { return nil }
func (a *apiDB) ApplyDatabaseSettings(context.Context, string, backup.Meta) ([]string, error) {
	return nil, nil
}
func (a *apiDB) Migrate(string) ([]backup.MigrationState, error) { return nil, nil }
func (a *apiDB) SwitchDatabase(_ context.Context, _, db, keepAs, repl string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dbs[keepAs] = true
	delete(a.dbs, repl)
	return nil
}
func (a *apiDB) DropDatabase(_ context.Context, _, name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.dbs, name)
	return nil
}
func (a *apiDB) ExistingDatabases(_ context.Context, _ string, names []string) (map[string]bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]bool{}
	for _, n := range names {
		out[n] = a.dbs[n]
	}
	return out, nil
}
func (a *apiDB) CheckDatabaseMigrations(context.Context, string) (*backup.DatabaseMigrations, error) {
	return &backup.DatabaseMigrations{}, a.migrationsErr
}
func (a *apiDB) CheckRollback(context.Context, string, string, string, string) error {
	return a.rollbackErr
}
func (a *apiDB) KeptDatabases(context.Context, string, string) ([]backup.KeptDatabase, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.kept, a.keptErr
}

// fakeReauth accepts password "pw" with code "123456" for the session token
// "session-token-01".
type fakeReauth struct {
	mu    sync.Mutex
	calls int
	ips   []string
	err   error
	// entered and block, when set, hold a check until block is closed.
	entered chan struct{}
	block   chan struct{}
}

func (f *fakeReauth) Verify(_ context.Context, token, ip string, r strongauth.Reauth) (*model.User, error) {
	f.mu.Lock()
	entered, block := f.entered, f.block
	f.entered = nil
	f.mu.Unlock()
	if block != nil {
		close(entered)
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.ips = append(f.ips, ip)
	if f.err != nil {
		return nil, f.err
	}
	if token != "session-token-01" {
		return nil, &strongauth.Error{Reason: strongauth.ReasonNotNativeToken}
	}
	if r.Password != "pw" || r.Token != "123456" {
		return nil, &strongauth.Error{Reason: strongauth.ReasonFailed}
	}
	return &model.User{ID: "admin1"}, nil
}

func (f *fakeReauth) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type apiRig struct {
	h      http.Handler
	verify *fakeReauth
	db     *apiDB
	maint  *maintmode.Store
	m      *restoreop.Manager
	d      *Daemon
	mr     *miniredis.Miniredis
	log    *syncLog
}

// syncLog collects log output across goroutines.
type syncLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *syncLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func newAPIRig(t *testing.T) *apiRig {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	db := &apiDB{dbs: map[string]bool{"app": true}}
	maint := maintmode.NewStore(rdb, rdb, "p:")
	ctx, cancel := context.WithCancel(context.Background())
	logs := &syncLog{}
	d := New(Options{Storage: newMemStorage(), Logger: slog.New(slog.NewTextHandler(logs, nil))})
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()
	require.Eventually(t, func() bool {
		release, err := d.Acquire(JobRestore)
		if err != nil {
			return false
		}
		release()
		return true
	}, 5*time.Second, time.Millisecond, "Run has started")
	m := restoreop.New(restoreop.Options{
		Store: restoreop.NewStore(rdb, "p:"), DB: db, Maint: maint,
		RunJudge: func(ctx context.Context) {
			(&maintmode.Judge{Store: maint, Counter: noSessions{}, ID: "j", DrainTimeout: time.Hour}).Run(ctx, 5*time.Millisecond)
		},
		Acquire:  func() (func(), error) { return d.Acquire(JobRestore) },
		SlotBusy: d.SlotBusy,
		Database: "app", MaintenanceDB: "postgres",
		Poll: 5 * time.Millisecond, HeldTimeout: time.Second, ResumeTimeout: time.Second, Logger: discardLogger(),
	})
	m.Start(ctx)
	t.Cleanup(func() {
		cancel()
		<-done
		m.Wait()
	})
	v := &fakeReauth{}
	h, err := Handler(d, testToken, &Restore{Manager: m, Verifier: v})
	require.NoError(t, err)
	return &apiRig{h: h, verify: v, db: db, maint: maint, m: m, d: d, mr: mr, log: logs}
}

type noSessions struct{}

func (noSessions) Sessions(context.Context) ([]maintmode.Blocker, error) { return nil, nil }
func (noSessions) Terminate(context.Context, maintmode.Blocker) (bool, error) {
	return false, nil
}

func (r *apiRig) post(t *testing.T, path string, body map[string]any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set(ClientIPHeader, "192.0.2.7")
	rec := httptest.NewRecorder()
	r.h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

func auth(extra map[string]any) map[string]any {
	m := map[string]any{"i": "session-token-01", "password": "pw", "token": "123456"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func (r *apiRig) prepare(t *testing.T) (opID, ticket string) {
	t.Helper()
	rec, out := r.post(t, "/restore/prepare", auth(map[string]any{"id": "20261010T120000Z"}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	opID, _ = out["opId"].(string)
	ticket, _ = out["ticket"].(string)
	require.NotEmpty(t, opID)
	require.NotEmpty(t, ticket)
	require.Eventually(t, func() bool {
		rec, out := r.post(t, "/restore/status", map[string]any{"i": "session-token-01", "opId": opID, "ticket": ticket})
		if rec.Code != http.StatusOK {
			return false
		}
		recd, _ := out["record"].(map[string]any)
		return recd["step"] == restoreop.StepReady
	}, 5*time.Second, 5*time.Millisecond)
	return opID, ticket
}

func TestRestoreAPI_PrepareConfirm(t *testing.T) {
	r := newAPIRig(t)
	opID, ticket := r.prepare(t)
	assert.Equal(t, 1, r.verify.count(), "prepare re-authenticates; status does not")
	assert.Equal(t, []string{"192.0.2.7"}, r.verify.ips, "the forwarded client address reaches the check")

	rec, _ := r.post(t, "/restore/confirm", auth(map[string]any{"opId": opID, "ticket": ticket}))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, 2, r.verify.count())
	require.Eventually(t, func() bool {
		_, out := r.post(t, "/restore/status", map[string]any{"i": "session-token-01", "opId": opID, "ticket": ticket})
		recd, _ := out["record"].(map[string]any)
		return recd["step"] == restoreop.StepDone
	}, 5*time.Second, 5*time.Millisecond)

	// 確定した後は、再認証の前に断る (名前を入れ替える間に照合の接続を張らない)。
	rec, out := r.post(t, "/restore/confirm", auth(map[string]any{"opId": opID, "ticket": ticket}))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "RESTORE_STEP", errCode(out))
	rec, out = r.post(t, "/restore/abort", auth(map[string]any{"opId": opID, "ticket": ticket}))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "RESTORE_STEP", errCode(out))
	assert.Equal(t, 2, r.verify.count(), "no check after the confirmation")
}

func TestRestoreAPI_Abort(t *testing.T) {
	r := newAPIRig(t)
	opID, ticket := r.prepare(t)
	rec, out := r.post(t, "/restore/abort", map[string]any{"i": "session-token-01", "opId": opID, "ticket": ticket, "password": "pw", "token": "000000"})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "REAUTHENTICATION_FAILED", errCode(out), "aborting needs the re-authentication too")
	rec, _ = r.post(t, "/restore/abort", auth(map[string]any{"opId": opID, "ticket": ticket}))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, out = r.post(t, "/restore/status", map[string]any{"i": "session-token-01", "opId": opID, "ticket": ticket})
	recd, _ := out["record"].(map[string]any)
	assert.Equal(t, restoreop.StepAborted, recd["step"])
}

func TestRestoreAPI_TicketAndSession(t *testing.T) {
	r := newAPIRig(t)
	opID, ticket := r.prepare(t)
	before := r.verify.count()
	for _, c := range []struct {
		name string
		body map[string]any
	}{
		{"another session", map[string]any{"i": "session-token-02", "opId": opID, "ticket": ticket, "password": "pw", "token": "123456"}},
		{"ticket only", map[string]any{"opId": opID, "ticket": ticket, "password": "pw", "token": "123456"}},
		{"wrong ticket", auth(map[string]any{"opId": opID, "ticket": ticket + "x"})},
	} {
		for _, path := range []string{"/restore/status", "/restore/confirm", "/restore/abort"} {
			rec, out := r.post(t, path, c.body)
			assert.Equal(t, http.StatusForbidden, rec.Code, c.name+" "+path)
			assert.Equal(t, "RESTORE_TICKET_INVALID", errCode(out), c.name+" "+path)
		}
	}
	assert.Equal(t, before, r.verify.count(), "a request without the ticket of the session is refused before the re-authentication")
	rec, out := r.post(t, "/restore/status", map[string]any{"i": "session-token-01", "opId": "restore-none", "ticket": ticket})
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "NO_SUCH_RESTORE", errCode(out))
}

func TestRestoreAPI_PrepareErrors(t *testing.T) {
	r := newAPIRig(t)
	rec, out := r.post(t, "/restore/prepare", map[string]any{"i": "session-token-01", "id": "20261010T120000Z", "password": "pw"})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "REAUTHENTICATION_FAILED", errCode(out))
	r.verify.err = &strongauth.Error{Reason: strongauth.ReasonRateLimited, RetryAfter: 3 * time.Second}
	rec, _ = r.post(t, "/restore/prepare", auth(map[string]any{"id": "20261010T120000Z"}))
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "3", rec.Header().Get("Retry-After"))
	r.verify.err = nil

	rec, out = r.post(t, "/restore/prepare", auth(map[string]any{"id": "latest"}))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "INVALID_PARAM", errCode(out))
	r.db.loadErr = fmt.Errorf("read meta: %w", backup.ErrNotFound)
	rec, out = r.post(t, "/restore/prepare", auth(map[string]any{"id": "20261010T120000Z"}))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "NO_SUCH_GENERATION", errCode(out))
	r.db.loadErr = errors.New("insufficient privilege")
	rec, out = r.post(t, "/restore/prepare", auth(map[string]any{"id": "20261010T120000Z"}))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "RESTORE_PREPARE_FAILED", errCode(out))
	r.db.loadErr = nil

	require.NoError(t, r.maint.Enter(context.Background(), maintmode.State{OpID: "m1", Kind: maintmode.KindManual}))
	rec, out = r.post(t, "/restore/prepare", auth(map[string]any{"id": "20261010T120000Z"}))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "MAINTENANCE_ACTIVE", errCode(out))

	req := httptest.NewRequest(http.MethodPost, "/restore/prepare", bytes.NewReader([]byte("{")))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	req = httptest.NewRequest(http.MethodPost, "/restore/prepare", bytes.NewReader(make([]byte, restoreBodyLimit+2)))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w = httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRestoreAPI_Unavailable(t *testing.T) {
	d := New(Options{Storage: newMemStorage(), Logger: discardLogger()})
	// 組み立てていない、まだ Set されていない、照合の口が無い、のどれも 503。
	for _, rs := range []*Restore{nil, {}, {Manager: restoreop.New(restoreop.Options{Logger: discardLogger()})}} {
		h, err := Handler(d, testToken, rs)
		require.NoError(t, err)
		for _, path := range []string{"/restore/prepare", "/restore/confirm", "/restore/abort", "/restore/status"} {
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte("{}")))
			req.Header.Set("Authorization", "Bearer "+testToken)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			assert.Equal(t, http.StatusServiceUnavailable, w.Code, path)
			assert.Contains(t, w.Body.String(), "RESTORE_UNAVAILABLE")
		}
	}
	h, err := Handler(d, testToken, nil)
	require.NoError(t, err)
	// token の無いリクエストは、リストアの口にも届かない。
	req := httptest.NewRequest(http.MethodPost, "/restore/status", bytes.NewReader([]byte("{}")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRestoreErrorMapping(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{restoreop.ErrNotVerified, http.StatusBadRequest, "GENERATION_NOT_VERIFIED"},
		{restoreop.ErrReplicas, http.StatusBadRequest, "RESTORE_NOT_SUPPORTED"},
		{restoreop.ErrBusy, http.StatusConflict, "BACKUP_SERVICE_BUSY"},
		{ErrBusy, http.StatusConflict, "BACKUP_SERVICE_BUSY"},
		{restoreop.ErrVersionMismatch, http.StatusConflict, "VERSION_MISMATCH"},
		{restoreop.ErrNotRunning, http.StatusServiceUnavailable, "RESTORE_UNAVAILABLE"},
		{ErrNotRunning, http.StatusServiceUnavailable, "RESTORE_UNAVAILABLE"},
	} {
		w := httptest.NewRecorder()
		restoreError(w, discardLogger(), c.err)
		assert.Equal(t, c.status, w.Code, c.code)
		assert.Contains(t, w.Body.String(), `"code":"`+c.code+`"`)
	}
}

// TestRestoreAPI_RefusesBeforeTheReauthentication checks that a request that
// cannot go on is refused before the re-authentication, which opens the
// database: during any maintenance, and while another operation is unfinished.
func TestRestoreAPI_RefusesBeforeTheReauthentication(t *testing.T) {
	idle := newAPIRig(t)
	require.NoError(t, idle.maint.Enter(context.Background(), maintmode.State{OpID: "m1", Kind: maintmode.KindManual}))
	rec, out := idle.post(t, "/restore/prepare", auth(map[string]any{"id": "20261010T120000Z"}))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "MAINTENANCE_ACTIVE", errCode(out))
	assert.Zero(t, idle.verify.count(), "prepare during a maintenance does not reach the database")

	r := newAPIRig(t)
	opID, ticket := r.prepare(t)
	before := r.verify.count()
	rec, out = r.post(t, "/restore/prepare", auth(map[string]any{"id": "20261010T120000Z"}))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "BACKUP_SERVICE_BUSY", errCode(out), "another operation is unfinished")

	require.NoError(t, r.maint.Enter(context.Background(), maintmode.State{OpID: "m1", Kind: maintmode.KindManual}))
	for _, path := range []string{"/restore/confirm", "/restore/abort"} {
		rec, out := r.post(t, path, auth(map[string]any{"opId": opID, "ticket": ticket}))
		assert.Equal(t, http.StatusConflict, rec.Code, path)
		assert.Equal(t, "MAINTENANCE_ACTIVE", errCode(out), path)
	}
	assert.Equal(t, before, r.verify.count(), "no check reaches the database")
	rec, _ = r.post(t, "/restore/status", map[string]any{"i": "session-token-01", "opId": opID, "ticket": ticket})
	assert.Equal(t, http.StatusOK, rec.Code, "the progress stays readable")
}

// TestRestoreAPI_OneChangeAtATime checks that while one request prepares,
// confirms or aborts, another is refused at once instead of starting a
// second check, and that the progress stays readable.
func TestRestoreAPI_OneChangeAtATime(t *testing.T) {
	r := newAPIRig(t)
	opID, ticket := r.prepare(t)
	entered, block := make(chan struct{}), make(chan struct{})
	r.verify.mu.Lock()
	r.verify.entered, r.verify.block = entered, block
	r.verify.mu.Unlock()
	done := make(chan int)
	go func() {
		rec, _ := r.post(t, "/restore/abort", auth(map[string]any{"opId": opID, "ticket": ticket}))
		done <- rec.Code
	}()
	<-entered
	for _, c := range []struct {
		path string
		body map[string]any
	}{
		{"/restore/prepare", auth(map[string]any{"id": "20261010T120000Z"})},
		{"/restore/confirm", auth(map[string]any{"opId": opID, "ticket": ticket})},
		{"/restore/abort", auth(map[string]any{"opId": opID, "ticket": ticket})},
	} {
		rec, out := r.post(t, c.path, c.body)
		assert.Equal(t, http.StatusConflict, rec.Code, c.path)
		assert.Equal(t, "BACKUP_SERVICE_BUSY", errCode(out), c.path)
	}
	rec, _ := r.post(t, "/restore/status", map[string]any{"i": "session-token-01", "opId": opID, "ticket": ticket})
	assert.Equal(t, http.StatusOK, rec.Code)
	close(block)
	assert.Equal(t, http.StatusNoContent, <-done)
	assert.Equal(t, 2, r.verify.count(), "only the first abort was checked")
}

// TestRestoreAPI_StatusBySession checks that status takes the session from
// the session field, which the admin page uses because the main server does
// not authenticate status (#3463 (6c) M-4).
func TestRestoreAPI_StatusBySession(t *testing.T) {
	r := newAPIRig(t)
	opID, ticket := r.prepare(t)
	rec, _ := r.post(t, "/restore/status", map[string]any{"session": "session-token-01", "opId": opID, "ticket": ticket})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec, _ = r.post(t, "/restore/status", map[string]any{"session": "session-token-01", "i": nil, "opId": opID, "ticket": ticket})
	assert.Equal(t, http.StatusOK, rec.Code, "i is null when the page sends no credential")
	rec, out := r.post(t, "/restore/status", map[string]any{"session": "session-token-02", "opId": opID, "ticket": ticket})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "RESTORE_TICKET_INVALID", errCode(out))
	// confirm と abort は session を見ない (再認証の token は i)。
	rec, out = r.post(t, "/restore/abort", map[string]any{"session": "session-token-01", "opId": opID, "ticket": ticket, "password": "pw", "token": "123456"})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "RESTORE_TICKET_INVALID", errCode(out))
}

// TestRestoreErrorKeepsTheReasonInTheLog checks that a failed check before a
// restore answers a generic message and leaves the reason (which names
// databases and files) to the log of the service.
func TestRestoreErrorKeepsTheReasonInTheLog(t *testing.T) {
	var buf bytes.Buffer
	w := httptest.NewRecorder()
	restoreError(w, slog.New(slog.NewTextHandler(&buf, nil)), errors.New(`permission denied for database "app_secret"`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "RESTORE_PREPARE_FAILED")
	assert.NotContains(t, w.Body.String(), "app_secret")
	assert.Contains(t, buf.String(), "app_secret")
	// ロガーが無くても応答は返す。
	w = httptest.NewRecorder()
	restoreError(w, nil, errors.New("x"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestRestoreAPI_RefusesWhileAJobRuns checks that prepare is refused before
// the re-authentication while a scheduled take or a verification holds the
// job slot, so that the TOTP code is not spent on a request that cannot
// start (#3463 (6c) L-3).
func TestRestoreAPI_RefusesWhileAJobRuns(t *testing.T) {
	r := newAPIRig(t)
	release, err := r.d.Acquire(JobVerify)
	require.NoError(t, err)
	rec, out := r.post(t, "/restore/prepare", auth(map[string]any{"id": "20261010T120000Z"}))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "BACKUP_SERVICE_BUSY", errCode(out))
	assert.Zero(t, r.verify.count(), "refused before the re-authentication")
	release()
	r.prepare(t)
}

// TestRestoreAPI_StatusFromAnyone covers status, which the server does not
// authenticate: a malformed operation ID is refused without reading Redis,
// an unknown or mismatched one is not logged, and a failure to read Redis is
// answered 503 and logged at most once a minute (#3463 (6c) L-2).
func TestRestoreAPI_StatusFromAnyone(t *testing.T) {
	r := newAPIRig(t)
	opID, ticket := r.prepare(t)
	before := r.mr.CommandCount()
	for _, bad := range []string{"", "restore-", "restore-XYZ", "restore-0123456789abcdef0", "x" + opID, "../" + opID} {
		rec, out := r.post(t, "/restore/status", map[string]any{"session": "session-token-01", "opId": bad, "ticket": ticket})
		assert.Equal(t, http.StatusNotFound, rec.Code, bad)
		assert.Equal(t, "NO_SUCH_RESTORE", errCode(out), bad)
	}
	assert.Equal(t, before, r.mr.CommandCount(), "a malformed ID does not reach Redis")

	rec, _ := r.post(t, "/restore/status", map[string]any{"session": "session-token-01", "opId": "restore-0123456789abcdef", "ticket": ticket})
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec, _ = r.post(t, "/restore/status", map[string]any{"session": "other-session-01", "opId": opID, "ticket": ticket})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.NotContains(t, r.log.String(), "cannot read a restore record")

	r.mr.SetError("LOADING Redis is loading the dataset in memory")
	for range 3 {
		rec, out := r.post(t, "/restore/status", map[string]any{"session": "session-token-01", "opId": opID, "ticket": ticket})
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Equal(t, "RESTORE_UNAVAILABLE", errCode(out))
	}
	r.mr.SetError("")
	assert.Equal(t, 1, strings.Count(r.log.String(), "cannot read a restore record"), "logged once a minute")
}

func TestLogThrottle(t *testing.T) {
	var lt logThrottle
	now := time.Unix(1000, 0)
	assert.True(t, lt.allow(now))
	assert.False(t, lt.allow(now.Add(storeErrLogInterval-time.Second)))
	assert.True(t, lt.allow(now.Add(storeErrLogInterval)))
}
