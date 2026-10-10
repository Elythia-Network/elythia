package adminbackup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/core/backupadmin"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/core/passwordguard"
	"github.com/elythia-network/elythia/internal/core/strongauth"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/labstack/echo/v4"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

const (
	adminID   = "admin1"
	modID     = "mod1"
	no2faID   = "admin2"
	password  = "correct horse"
	gen1      = "20261001T040000Z"
	appToken  = "app-access-token"
	userToken = "native-"
)

// --- fakes ---

type roles struct{}

func (roles) IsAdministrator(id string) bool { return id == adminID || id == no2faID }
func (roles) IsModerator(id string) bool     { return id == adminID || id == no2faID || id == modID }

type profiles struct {
	m map[string]*model.UserProfile
}

func (p *profiles) GetProfileErr(id string) (*model.UserProfile, error) { return p.m[id], nil }
func (p *profiles) RemoveBackupCode(string, string) error               { return nil }

type keys struct{ list []*model.UserSecurityKey }

func (k *keys) ListByUser(string) ([]*model.UserSecurityKey, error) { return k.list, nil }
func (k *keys) UpdateCounter(string, int64) error                   { return nil }

type passkeys struct{}

func (passkeys) BeginReauth(context.Context, *model.User, []*model.UserSecurityKey) (*protocol.CredentialAssertion, error) {
	return &protocol.CredentialAssertion{Response: protocol.PublicKeyCredentialRequestOptions{Challenge: []byte("chal"), RelyingPartyID: "example.com"}}, nil
}

func (passkeys) FinishReauth(_ context.Context, _ *model.User, _ []*model.UserSecurityKey, req *http.Request) (*webauthn.Credential, error) {
	b, _ := io.ReadAll(req.Body)
	if !bytes.Contains(b, []byte("good-assertion")) {
		return nil, errors.New("bad assertion")
	}
	return &webauthn.Credential{ID: []byte{1}}, nil
}

// memStorage records every access so that a refused request can be shown to
// have touched nothing.
type memStorage struct {
	mu       sync.Mutex
	objs     map[string][]byte
	accesses int
	noSeek   bool
	getErr   error
}

func (m *memStorage) touch() { m.accesses++ }

func (m *memStorage) Put(_ context.Context, key string, r io.Reader) error {
	b, _ := io.ReadAll(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = b
	return nil
}

type seekCloser struct{ *bytes.Reader }

func (seekCloser) Close() error { return nil }

func (m *memStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touch()
	if m.getErr != nil {
		return nil, m.getErr
	}
	b, ok := m.objs[key]
	if !ok {
		return nil, backup.ErrNotFound
	}
	if m.noSeek {
		return io.NopCloser(bytes.NewReader(b)), nil
	}
	return seekCloser{bytes.NewReader(b)}, nil
}

func (m *memStorage) Stat(_ context.Context, key string) (backup.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touch()
	b, ok := m.objs[key]
	if !ok {
		return backup.ObjectInfo{}, backup.ErrNotFound
	}
	return backup.ObjectInfo{Key: key, Size: int64(len(b)), ModTime: time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)}, nil
}

func (m *memStorage) List(_ context.Context, prefix string) ([]backup.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touch()
	var out []backup.ObjectInfo
	for k, b := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, backup.ObjectInfo{Key: k, Size: int64(len(b))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *memStorage) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touch()
	delete(m.objs, key)
	return nil
}

type control struct {
	calls int
	ips   []string
	err   error
}

func (c *control) Take(_ context.Context, ip string) (*backupadmin.Job, error) {
	c.calls++
	c.ips = append(c.ips, ip)
	return &backupadmin.Job{Kind: "take", Trigger: "api"}, c.err
}

func (c *control) Verify(_ context.Context, id, ip string) (*backupadmin.Job, error) {
	c.calls++
	c.ips = append(c.ips, ip)
	return &backupadmin.Job{Kind: "verify", Trigger: "api", GenerationID: id}, c.err
}
func (c *control) Status(context.Context) (*backupadmin.Status, error) {
	return &backupadmin.Status{}, nil
}

type modlog struct {
	entries []moderationlog.LogType
	infos   []map[string]any
}

func (l *modlog) Log(_ context.Context, _ string, t moderationlog.LogType, info map[string]any) {
	l.entries = append(l.entries, t)
	l.infos = append(l.infos, info)
}

// --- fixture ---

type fixture struct {
	e       *echo.Echo
	storage *memStorage
	control *control
	log     *modlog
	secret  string
	keys    *keys
	tokens  *backupadmin.RedisTokens
}

func dumpBytes() []byte { return []byte("0123456789abcdef") }

// newFixture builds an echo app with the admin/backup routes. guardOnly
// leaves out RequireAdmin / RequireSecure to show that the guard alone keeps
// every endpoint closed.
func newFixture(t *testing.T, guardOnly bool) *fixture {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	require.NoError(t, err)
	h := string(hash)
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "t", AccountName: "a"})
	require.NoError(t, err)
	secret := key.Secret()
	pr := &profiles{m: map[string]*model.UserProfile{
		adminID: {UserID: adminID, Password: &h, TwoFactorEnabled: true, TwoFactorSecret: &secret},
		modID:   {UserID: modID, Password: &h, TwoFactorEnabled: true, TwoFactorSecret: &secret},
		no2faID: {UserID: no2faID, Password: &h},
	}}
	k := &keys{list: []*model.UserSecurityKey{{ID: "AQ", UserID: adminID}}}
	v, err := strongauth.New(strongauth.Deps{
		Roles:    roles{},
		Profiles: pr,
		Keys:     k,
		Passkeys: passkeys{},
		Guard:    passwordguard.NewRedisGuard(rdb),
		// TOTP の使い回しの拒否は strongauth のテストで見る。ここでは同じコードで
		// 何度も通したいので外す。
	})
	require.NoError(t, err)

	st := &memStorage{objs: map[string][]byte{}}
	putGeneration(t, st)
	ctl := &control{}
	tokens := backupadmin.NewRedisTokens(rdb)
	svc := backupadmin.NewService(backupadmin.Options{
		StorageType: "dir", Storage: st, Control: ctl, Tokens: tokens,
		DownloadURLBase: "https://example.com/backup-download?token=",
	})
	lg := &modlog{}
	hd := NewHandler(svc, v, lg)

	e := echo.New()
	setUser := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if id := c.Request().Header.Get("X-Test-User"); id != "" {
				tok := userToken + id
				c.Set(string(middleware.UserContextKey), &model.User{ID: id, Token: &tok})
				c.Set(string(middleware.TokenContextKey), c.Request().Header.Get("X-Test-Token"))
			}
			return next(c)
		}
	}
	chain := []echo.MiddlewareFunc{setUser, middleware.RequireAdmin(roles{}), middleware.RequireSecure(), hd.Guard()}
	pre := []echo.MiddlewareFunc{setUser, middleware.RequireAdmin(roles{}), middleware.RequireSecure()}
	if guardOnly {
		chain = []echo.MiddlewareFunc{setUser, hd.Guard()}
		pre = []echo.MiddlewareFunc{setUser}
	}
	e.POST("/api/admin/backup/list", hd.List, chain...)
	e.POST("/api/admin/backup/take", hd.Take, chain...)
	e.POST("/api/admin/backup/verify", hd.Verify, chain...)
	e.POST("/api/admin/backup/delete", hd.Delete, chain...)
	e.POST("/api/admin/backup/download", hd.Download, chain...)
	e.POST("/api/admin/backup/reauth-challenge", hd.ReauthChallenge, pre...)
	e.GET("/backup-download", hd.ServeDownload)
	return &fixture{e: e, storage: st, control: ctl, log: lg, secret: secret, keys: k, tokens: tokens}
}

func putGeneration(t *testing.T, st *memStorage) {
	t.Helper()
	st.objs[backup.Key(gen1, backup.DumpFile)] = dumpBytes()
	m, err := json.Marshal(backup.Meta{FormatVersion: backup.MetaFormatVersion, ID: gen1, DumpFile: backup.DumpFile, DumpSize: 16})
	require.NoError(t, err)
	st.objs[backup.Key(gen1, backup.MetaFile)] = m
}

func (f *fixture) code(t *testing.T) string {
	t.Helper()
	c, err := totp.GenerateCode(f.secret, time.Now())
	require.NoError(t, err)
	return c
}

type caller struct {
	user  string
	token string
}

func native(id string) caller { return caller{user: id, token: userToken + id} }

func (f *fixture) post(t *testing.T, path string, who caller, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if who.user != "" {
		req.Header.Set("X-Test-User", who.user)
		req.Header.Set("X-Test-Token", who.token)
	}
	rec := httptest.NewRecorder()
	f.e.ServeHTTP(rec, req)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
			ID   string `json:"id"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error.Code
}

// operations are every guarded endpoint, with the body each needs besides
// the re-authentication.
var operations = []struct {
	name string
	path string
	args map[string]any
}{
	{"list", "/api/admin/backup/list", nil},
	{"take", "/api/admin/backup/take", nil},
	{"verify", "/api/admin/backup/verify", map[string]any{"id": gen1}},
	{"delete", "/api/admin/backup/delete", map[string]any{"id": gen1}},
	{"download", "/api/admin/backup/download", map[string]any{"id": gen1}},
}

func withAuth(args map[string]any, auth map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		out[k] = v
	}
	for k, v := range auth {
		out[k] = v
	}
	return out
}

// TestEveryOperationRefusesEveryMissingCondition is the table of issue #3462
// 4: each condition of the strong authentication, missing alone, must refuse
// every operation without touching the storage, the backup service or the
// moderation log.
func TestEveryOperationRefusesEveryMissingCondition(t *testing.T) {
	type condition struct {
		name   string
		who    caller
		auth   func(f *fixture) map[string]any
		status int
		code   string
		// fail is how many wrong re-authentications to send first.
		fail int
	}
	good := func(f *fixture) map[string]any { return map[string]any{"password": password, "token": f.code(t)} }
	conditions := []condition{
		{name: "not signed in", who: caller{}, auth: good, status: http.StatusUnauthorized, code: "CREDENTIAL_REQUIRED"},
		{name: "moderator", who: native(modID), auth: good, status: http.StatusForbidden, code: "ROLE_PERMISSION_DENIED"},
		{name: "app token", who: caller{user: adminID, token: appToken}, auth: good, status: http.StatusForbidden, code: "ACCESS_DENIED"},
		{name: "2fa not registered", who: native(no2faID), auth: func(*fixture) map[string]any { return map[string]any{"password": password, "token": "123456"} }, status: http.StatusForbidden, code: "TWO_FACTOR_REQUIRED"},
		{name: "reauth omitted", who: native(adminID), auth: func(*fixture) map[string]any { return nil }, status: http.StatusForbidden, code: "REAUTHENTICATION_REQUIRED"},
		{name: "password omitted", who: native(adminID), auth: func(f *fixture) map[string]any { return map[string]any{"token": f.code(t)} }, status: http.StatusForbidden, code: "REAUTHENTICATION_REQUIRED"},
		{name: "2fa code omitted", who: native(adminID), auth: func(*fixture) map[string]any { return map[string]any{"password": password} }, status: http.StatusForbidden, code: "REAUTHENTICATION_REQUIRED"},
		{name: "wrong password", who: native(adminID), auth: func(f *fixture) map[string]any { return map[string]any{"password": "wrong", "token": f.code(t)} }, status: http.StatusForbidden, code: "REAUTHENTICATION_FAILED"},
		{name: "wrong 2fa code", who: native(adminID), auth: func(*fixture) map[string]any { return map[string]any{"password": password, "token": "000000"} }, status: http.StatusForbidden, code: "REAUTHENTICATION_FAILED"},
		{name: "wrong passkey", who: native(adminID), auth: func(*fixture) map[string]any {
			return map[string]any{"password": password, "credential": map[string]any{"id": "AQ", "response": "forged"}}
		}, status: http.StatusForbidden, code: "REAUTHENTICATION_FAILED"},
		{name: "rate limited", who: native(adminID), auth: good, fail: passwordguard.DefaultMaxFailures, status: http.StatusTooManyRequests, code: "RATE_LIMIT_EXCEEDED"},
	}
	for _, guardOnly := range []bool{false, true} {
		mode := "router chain"
		if guardOnly {
			mode = "guard only"
		}
		t.Run(mode, func(t *testing.T) {
			for _, op := range operations {
				for _, cond := range conditions {
					t.Run(op.name+"/"+cond.name, func(t *testing.T) {
						f := newFixture(t, guardOnly)
						for i := 0; i < cond.fail; i++ {
							rec := f.post(t, op.path, native(adminID), withAuth(op.args, map[string]any{"password": "wrong", "token": "000000"}))
							require.Equal(t, http.StatusForbidden, rec.Code)
						}
						before := f.storage.accesses
						rec := f.post(t, op.path, cond.who, withAuth(op.args, cond.auth(f)))
						assert.Equal(t, cond.status, rec.Code, rec.Body.String())
						assert.Equal(t, cond.code, errCode(t, rec), rec.Body.String())
						assert.Equal(t, before, f.storage.accesses, "the storage must not be touched")
						assert.Zero(t, f.control.calls, "the backup service must not be asked")
						assert.Empty(t, f.log.entries, "nothing happened, so nothing is logged")
						assert.Contains(t, f.storage.objs, backup.Key(gen1, backup.MetaFile), "the generation must survive")
					})
				}
			}
		})
	}
}

func TestEveryOperationPassesWithFullAuth(t *testing.T) {
	want := map[string]moderationlog.LogType{
		"list": moderationlog.LogListBackups, "take": moderationlog.LogTakeBackup, "verify": moderationlog.LogVerifyBackup,
		"delete": moderationlog.LogDeleteBackup, "download": moderationlog.LogDownloadBackup,
	}
	for _, op := range operations {
		for _, second := range []string{"totp", "passkey"} {
			t.Run(op.name+"/"+second, func(t *testing.T) {
				f := newFixture(t, false)
				auth := map[string]any{"password": password, "token": f.code(t)}
				if second == "passkey" {
					auth = map[string]any{"password": password, "credential": map[string]any{"id": "AQ", "response": "good-assertion"}}
				}
				rec := f.post(t, op.path, native(adminID), withAuth(op.args, auth))
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Equal(t, []moderationlog.LogType{want[op.name]}, f.log.entries)
			})
		}
	}
}

func TestOperationResponses(t *testing.T) {
	f := newFixture(t, false)
	auth := func(args map[string]any) map[string]any {
		return withAuth(args, map[string]any{"password": password, "token": f.code(t)})
	}

	rec := f.post(t, "/api/admin/backup/list", native(adminID), auth(nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var ov backupadmin.Overview
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ov))
	require.Len(t, ov.Generations, 1)
	assert.Equal(t, gen1, ov.Generations[0].ID)
	assert.Equal(t, map[string]any{"generationCount": 1, "totalBytes": ov.Usage.TotalBytes}, f.log.infos[0])

	rec = f.post(t, "/api/admin/backup/download", native(adminID), auth(map[string]any{"id": gen1}))
	require.Equal(t, http.StatusOK, rec.Code)
	var d backupadmin.Download
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &d))
	assert.Equal(t, "server", d.Via)
	require.True(t, strings.HasPrefix(d.URL, "https://example.com/backup-download?token="))
	token := strings.TrimPrefix(d.URL, "https://example.com/backup-download?token=")

	// 本体を通して渡す。Range で途中から取れる。
	req := httptest.NewRequest(http.MethodGet, "/backup-download?token="+token, nil)
	req.Header.Set("Range", "bytes=4-7")
	res := httptest.NewRecorder()
	f.e.ServeHTTP(res, req)
	require.Equal(t, http.StatusPartialContent, res.Code)
	assert.Equal(t, "4567", res.Body.String())
	assert.Equal(t, "no-store", res.Header().Get("Cache-Control"))
	assert.Contains(t, res.Header().Get("Content-Disposition"), "attachment")
	assert.Equal(t, `attachment; filename=`+gen1+`-dump.pgc`, res.Header().Get("Content-Disposition"))

	rec = f.post(t, "/api/admin/backup/verify", native(adminID), auth(map[string]any{"id": gen1}))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"accepted":true,"job":{"kind":"verify","trigger":"api","generationId":"`+gen1+`","startedAt":"0001-01-01T00:00:00Z"}}`, rec.Body.String())
	rec = f.post(t, "/api/admin/backup/take", native(adminID), auth(nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 2, f.control.calls)
	// 操作した人の接続元をサービスへ渡す (#3463 の passwordguard の枠)。
	assert.Equal(t, []string{"192.0.2.1", "192.0.2.1"}, f.control.ips)

	rec = f.post(t, "/api/admin/backup/delete", native(adminID), auth(map[string]any{"id": gen1}))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"freedBytes":`+jsonInt(int64(len(dumpBytes()))+int64(len(f.storageMetaLen())))+`}`, rec.Body.String())
	assert.Empty(t, f.storage.objs)
	assert.Equal(t, gen1, f.log.infos[len(f.log.infos)-1]["generationId"])

	rec = f.post(t, "/api/admin/backup/delete", native(adminID), auth(map[string]any{"id": gen1}))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "NO_SUCH_GENERATION", errCode(t, rec))
}

// storageMetaLen returns a slice as long as the meta.json written by
// putGeneration.
func (f *fixture) storageMetaLen() []byte {
	m, _ := json.Marshal(backup.Meta{FormatVersion: backup.MetaFormatVersion, ID: gen1, DumpFile: backup.DumpFile, DumpSize: 16})
	return m
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestServeDownload(t *testing.T) {
	f := newFixture(t, false)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		f.e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	assert.Equal(t, http.StatusNotFound, get("/backup-download?token=nope").Code)

	tok, err := f.tokens.Issue(context.Background(), backupadmin.DownloadGrant{Key: backup.Key(gen1, backup.DumpFile), FileName: "x.pgc"}, time.Minute)
	require.NoError(t, err)
	rec := get("/backup-download?token=" + tok)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, string(dumpBytes()), rec.Body.String())
	assert.Equal(t, "application/octet-stream", rec.Header().Get("Content-Type"))

	f.storage.noSeek = true
	rec = get("/backup-download?token=" + tok)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, string(dumpBytes()), rec.Body.String())

	f.storage.getErr = errors.New("io")
	assert.Equal(t, http.StatusInternalServerError, get("/backup-download?token="+tok).Code)

	f.storage.getErr = nil
	delete(f.storage.objs, backup.Key(gen1, backup.DumpFile))
	assert.Equal(t, http.StatusNotFound, get("/backup-download?token="+tok).Code)
}

func TestReauthChallenge(t *testing.T) {
	cases := []struct {
		name   string
		who    caller
		status int
		code   string
	}{
		{"admin", native(adminID), http.StatusOK, ""},
		{"not signed in", caller{}, http.StatusUnauthorized, "CREDENTIAL_REQUIRED"},
		{"moderator", native(modID), http.StatusForbidden, "ROLE_PERMISSION_DENIED"},
		{"app token", caller{user: adminID, token: appToken}, http.StatusForbidden, "ACCESS_DENIED"},
		{"2fa not registered", native(no2faID), http.StatusForbidden, "TWO_FACTOR_REQUIRED"},
	}
	for _, guardOnly := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				f := newFixture(t, guardOnly)
				rec := f.post(t, "/api/admin/backup/reauth-challenge", tc.who, nil)
				assert.Equal(t, tc.status, rec.Code, rec.Body.String())
				if tc.code != "" {
					assert.Equal(t, tc.code, errCode(t, rec))
					return
				}
				var body map[string]any
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				assert.Equal(t, "Y2hhbA", body["challenge"])
			})
		}
	}
	t.Run("no passkey", func(t *testing.T) {
		f := newFixture(t, false)
		f.keys.list = nil
		rec := f.post(t, "/api/admin/backup/reauth-challenge", native(adminID), nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Equal(t, "PASSKEY_UNAVAILABLE", errCode(t, rec))
	})
}
