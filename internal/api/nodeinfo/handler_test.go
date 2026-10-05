package nodeinfo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVersion2_1(t *testing.T) {
	h := NewHandler(&config.Config{Version: "0.0.0", Host: "example.com"})

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/nodeinfo/2.1", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Version2_1(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "2.1", resp["version"])
	sw := resp["software"].(map[string]any)
	assert.Equal(t, "mk-go", sw["name"])
	assert.Equal(t, config.MkGoVersion, sw["version"])
	// #1925: 2.1 は homepage=repository=softwareRepository。
	assert.Equal(t, softwareRepository, sw["homepage"])
	assert.Equal(t, softwareRepository, sw["repository"])
}

// #1948-22: upstream NodeinfoServerService は schema-profile つき Content-Type +
// Cache-Control: public, max-age=600 + CORS/Expose ヘッダを返す。
func TestVersion2_1_ResponseHeaders(t *testing.T) {
	h := NewHandler(&config.Config{Version: "0.0.0", Host: "example.com"})
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/nodeinfo/2.1", nil)
	rec := httptest.NewRecorder()
	require.NoError(t, h.Version2_1(e.NewContext(req, rec)))
	assert.Equal(t, `application/json; profile="http://nodeinfo.diaspora.software/ns/schema/2.1#"`, rec.Header().Get("Content-Type"))
	assert.Equal(t, "public, max-age=600", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "GET, OPTIONS", rec.Header().Get("Access-Control-Allow-Methods"))
	assert.Equal(t, "Accept", rec.Header().Get("Access-Control-Allow-Headers"))
	assert.Equal(t, "Vary", rec.Header().Get("Access-Control-Expose-Headers"))
}

// #1948-22: 2.0 は 2.0# profile を返す。
func TestVersion2_0_ResponseHeaders(t *testing.T) {
	h := NewHandler(&config.Config{Version: "0.0.0", Host: "example.com"})
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/nodeinfo/2.0", nil)
	rec := httptest.NewRecorder()
	require.NoError(t, h.Version2_0(e.NewContext(req, rec)))
	assert.Equal(t, `application/json; profile="http://nodeinfo.diaspora.software/ns/schema/2.0#"`, rec.Header().Get("Content-Type"))
	assert.Equal(t, "public, max-age=600", rec.Header().Get("Cache-Control"))
}

// admin で設定した Meta.Name / Description がnodeinfo.metadata に反映される
// (#348)。これが効かないとリモートが受け取る nodeName はconfig default
// (= host) のまま更新されない。
func TestVersion2_1_ReflectsMetaName(t *testing.T) {
	metaRepo := testutil.NewMockMetaRepository()
	name := "My Custom Instance"
	desc := "hello world"
	mn := "admin"
	me := "admin@example.com"
	metaRepo.Meta = &model.Meta{
		ID: "x", Name: &name, Description: &desc,
		MaintainerName: &mn, MaintainerEmail: &me,
	}
	h := NewHandler(&config.Config{Version: "0.0.0", Host: "fallback.example"})
	h.SetMetaRepo(metaRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/nodeinfo/2.1", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Version2_1(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	meta := resp["metadata"].(map[string]any)
	assert.Equal(t, "My Custom Instance", meta["nodeName"])
	assert.Equal(t, "hello world", meta["nodeDescription"])
	maint := meta["maintainer"].(map[string]any)
	assert.Equal(t, "admin", maint["name"])
	assert.Equal(t, "admin@example.com", maint["email"])
}

// #1777 [HIGH]: /.well-known/nodeinfo が advertise する /nodeinfo/2.0 を実際に配信し、
// schema 2.0 には無い software.repository を省略する (2.1 では出す)。
func TestVersion2_0(t *testing.T) {
	h := NewHandler(&config.Config{Version: "0.0.0", Host: "example.com"})

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/nodeinfo/2.0", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Version2_0(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "2.0", resp["version"])
	sw := resp["software"].(map[string]any)
	assert.Equal(t, "mk-go", sw["name"])
	_, hasRepo := sw["repository"]
	assert.False(t, hasRepo, "schema 2.0 は software.repository を含めない")
	// #1925: 2.0 は repository を delete するが homepage は残す。
	assert.Equal(t, softwareRepository, sw["homepage"], "schema 2.0 も software.homepage を含む")

	// 対照: 2.1 は repository を含む。
	rec21 := httptest.NewRecorder()
	require.NoError(t, h.Version2_1(e.NewContext(httptest.NewRequest(http.MethodGet, "/nodeinfo/2.1", nil), rec21)))
	var resp21 map[string]any
	require.NoError(t, json.Unmarshal(rec21.Body.Bytes(), &resp21))
	_, has21 := resp21["software"].(map[string]any)["repository"]
	assert.True(t, has21, "schema 2.1 は software.repository を含む")
}

// #1777 [MEDIUM]: metadata block が upstream の追加フィールド (themeColor /
// nodeAdmins / langs / disable*Timeline / captcha / maxNoteTextLength /
// proxyAccountName 等) を出す。
func TestVersion2_1_FullMetadata(t *testing.T) {
	metaRepo := testutil.NewMockMetaRepository()
	name := "Inst"
	tos := "https://example.com/tos"
	theme := "#abcdef"
	metaRepo.Meta = &model.Meta{
		ID: "x", Name: &name,
		TermsOfServiceURL:      &tos,
		ThemeColor:             &theme,
		Langs:                  []string{"ja-JP", "en-US"},
		EnableHcaptcha:         true,
		EmailRequiredForSignup: true,
		DisableRegistration:    true,
	}
	h := NewHandler(&config.Config{Version: "0.0.0", Host: "fallback.example"})
	h.SetMetaRepo(metaRepo)
	h.SetProxyAccountResolver(func() (string, bool) { return "proxy.bot", true })

	e := echo.New()
	rec := httptest.NewRecorder()
	require.NoError(t, h.Version2_1(e.NewContext(httptest.NewRequest(http.MethodGet, "/nodeinfo/2.1", nil), rec)))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	meta := resp["metadata"].(map[string]any)

	assert.Equal(t, "#abcdef", meta["themeColor"])
	assert.Equal(t, []any{"ja-JP", "en-US"}, meta["langs"])
	assert.Equal(t, "https://example.com/tos", meta["tosUrl"])
	assert.Equal(t, true, meta["enableHcaptcha"])
	assert.Equal(t, true, meta["emailRequiredForSignup"])
	assert.Equal(t, true, meta["disableRegistration"])
	assert.Equal(t, float64(maxNoteTextLength), meta["maxNoteTextLength"])
	assert.Equal(t, "proxy.bot", meta["proxyAccountName"])
	// nodeAdmins は [{name,email}] 配列。
	admins := meta["nodeAdmins"].([]any)
	require.Len(t, admins, 1)
	// disable*Timeline は policy 由来 (default policy では both available=true → false)。
	assert.Equal(t, false, meta["disableLocalTimeline"])
	assert.Equal(t, false, meta["disableGlobalTimeline"])
}

// themeColor 未設定時は upstream default '#86b300' を出す。
func TestVersion2_1_ThemeColorDefault(t *testing.T) {
	h := NewHandler(&config.Config{Version: "0.0.0", Host: "example.com"})
	e := echo.New()
	rec := httptest.NewRecorder()
	require.NoError(t, h.Version2_1(e.NewContext(httptest.NewRequest(http.MethodGet, "/nodeinfo/2.1", nil), rec)))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, defaultThemeColor, resp["metadata"].(map[string]any)["themeColor"])
}

// Meta未設定時は cfg.Host が nodeName に入る。
func TestVersion2_1_FallbackToConfigHost(t *testing.T) {
	h := NewHandler(&config.Config{Version: "0.0.0", Host: "example.com"})
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/nodeinfo/2.1", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Version2_1(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	meta := resp["metadata"].(map[string]any)
	assert.Equal(t, "example.com", meta["nodeName"])
}

// #403: nodeinfo usage 統計値が repo 経由で埋まる。
func TestVersion2_1_UsageStatsFromRepos(t *testing.T) {
	now := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()

	// Local users 計4人: active 月内2 / 半年内3 / それ以外1。
	within1Month := now.AddDate(0, 0, -10)
	within6Month := now.AddDate(0, -3, 0)
	beyond6Month := now.AddDate(0, -7, 0)
	userRepo.Users["u1"] = &model.User{ID: "u1", Host: nil, LastActiveDate: &within1Month}
	userRepo.Users["u2"] = &model.User{ID: "u2", Host: nil, LastActiveDate: &within1Month}
	userRepo.Users["u3"] = &model.User{ID: "u3", Host: nil, LastActiveDate: &within6Month}
	// active 外の local user (6ヶ月前)
	userRepo.Users["u4"] = &model.User{ID: "u4", Host: nil, LastActiveDate: &beyond6Month}
	// remote user (count 対象外)
	remoteHost := "remote.example"
	userRepo.Users["r1"] = &model.User{ID: "r1", Host: &remoteHost, LastActiveDate: &within1Month}

	// Local notes: 5本、内 2本が reply
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", UserHost: nil}
	noteRepo.Notes["n2"] = &model.Note{ID: "n2", UserID: "u1", UserHost: nil}
	replyA := "n1"
	noteRepo.Notes["n3"] = &model.Note{ID: "n3", UserID: "u2", UserHost: nil, ReplyID: &replyA}
	noteRepo.Notes["n4"] = &model.Note{ID: "n4", UserID: "u3", UserHost: nil}
	noteRepo.Notes["n5"] = &model.Note{ID: "n5", UserID: "u3", UserHost: nil, ReplyID: &replyA}
	// remote note (対象外)
	noteRepo.Notes["r1"] = &model.Note{ID: "r1", UserID: "ru", UserHost: &remoteHost}

	h := NewHandler(&config.Config{Version: "0.0.0", Host: "example.com"})
	h.SetUsageRepos(userRepo, noteRepo)
	h.SetClock(func() time.Time { return now })

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/nodeinfo/2.1", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Version2_1(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	usage := resp["usage"].(map[string]any)
	users := usage["users"].(map[string]any)
	assert.Equal(t, float64(4), users["total"]) // u1-u4 local
	// #1948-22: upstream は activeMonth/activeHalfyear=null、localComments=0 固定。
	assert.Nil(t, users["activeMonth"])
	assert.Nil(t, users["activeHalfyear"])
	assert.Equal(t, float64(5), usage["localPosts"])
	assert.Equal(t, float64(0), usage["localComments"])
}

// repo のerror path を通して slog.Warn 分岐を cover する。他の test
// file と同じく pointer embedding で mock を埋め込んで method promotion を
// 正しく働かせる。
type failingUserRepoForCount struct {
	*testutil.MockUserRepository
}

func (f *failingUserRepoForCount) CountLocalUsers() (int64, error) {
	return 0, errForTest
}

func (f *failingUserRepoForCount) CountLocalUsersActiveSince(time.Time) (int64, error) {
	return 0, errForTest
}

type failingNoteRepoForCount struct {
	*testutil.MockNoteRepository
}

func (f *failingNoteRepoForCount) CountLocalNotes() (int64, error)    { return 0, errForTest }
func (f *failingNoteRepoForCount) CountLocalComments() (int64, error) { return 0, errForTest }

var errForTest = errTest{}

type errTest struct{}

func (errTest) Error() string { return "test error" }

func TestVersion2_1_UsageStatsCountErrorsFallbackToZero(t *testing.T) {
	h := NewHandler(&config.Config{Version: "0.0.0", Host: "example.com"})
	h.SetUsageRepos(
		&failingUserRepoForCount{MockUserRepository: testutil.NewMockUserRepository()},
		&failingNoteRepoForCount{MockNoteRepository: testutil.NewMockNoteRepository()},
	)
	h.SetClock(func() time.Time { return time.Unix(0, 0) })

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/nodeinfo/2.1", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Version2_1(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	usage := resp["usage"].(map[string]any)
	users := usage["users"].(map[string]any)
	assert.Equal(t, float64(0), users["total"])
	assert.Nil(t, users["activeMonth"])
	assert.Nil(t, users["activeHalfyear"])
	assert.Equal(t, float64(0), usage["localPosts"])
	assert.Equal(t, float64(0), usage["localComments"])
}

// 未配線時は 0 埋め
func TestVersion2_1_UsageStatsZeroWhenReposMissing(t *testing.T) {
	h := NewHandler(&config.Config{Version: "0.0.0", Host: "example.com"})

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/nodeinfo/2.1", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Version2_1(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	usage := resp["usage"].(map[string]any)
	users := usage["users"].(map[string]any)
	assert.Equal(t, float64(0), users["total"])
	assert.Nil(t, users["activeMonth"])
	assert.Nil(t, users["activeHalfyear"])
	assert.Equal(t, float64(0), usage["localPosts"])
	assert.Equal(t, float64(0), usage["localComments"])
}

// 連合を宣言したプラグインだけが出ること (#2537)。
//
// **入れているプラグインを全部並べない。** 運営者がどんな拡張を使っているかは
// 攻撃面の情報になるので、宣言したものに限る。宣言が無ければキーごと出さない。
func TestVersion2_1_PeeredPlugins(t *testing.T) {
	h := NewHandler(&config.Config{Host: "example.test", URL: "https://example.test"})

	rec := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/nodeinfo/2.1", nil), rec)
	require.NoError(t, h.Version2_1(c))

	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	meta, _ := out["metadata"].(map[string]any)
	_, present := meta["mkGoPlugins"]
	assert.False(t, present, "宣言が無ければキーごと出さない")

	h.SetPeeredPlugins([]string{"demo"})
	rec = httptest.NewRecorder()
	c = echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/nodeinfo/2.1", nil), rec)
	require.NoError(t, h.Version2_1(c))

	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	meta, _ = out["metadata"].(map[string]any)
	assert.Equal(t, []any{"demo"}, meta["mkGoPlugins"])
}

// --- server-side cache (upstream MemorySingleCache 相当) ---

// countingUserRepo counts how many times the nodeinfo document asked for the
// local user total, so a test can tell a cache hit from a rebuild.
type countingUserRepo struct {
	*testutil.MockUserRepository
	calls atomic.Int64
	// before は COUNT に入った瞬間に呼ばれる hook。並行テストが「build 中」の
	// 窓を掴むのに使う。
	before func()
}

func (c *countingUserRepo) CountLocalUsers() (int64, error) {
	c.calls.Add(1)
	if c.before != nil {
		c.before()
	}
	return c.MockUserRepository.CountLocalUsers()
}

type countingNoteRepo struct {
	*testutil.MockNoteRepository
	calls atomic.Int64
}

func (c *countingNoteRepo) CountLocalNotes() (int64, error) {
	c.calls.Add(1)
	return c.MockNoteRepository.CountLocalNotes()
}

func newCountingHandler(t *testing.T, now time.Time) (*Handler, *countingUserRepo, *countingNoteRepo) {
	t.Helper()
	userRepo := &countingUserRepo{MockUserRepository: testutil.NewMockUserRepository()}
	noteRepo := &countingNoteRepo{MockNoteRepository: testutil.NewMockNoteRepository()}
	userRepo.Users["u1"] = &model.User{ID: "u1", Host: nil}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", UserHost: nil}
	h := NewHandler(&config.Config{Version: "0.0.0", Host: "example.com"})
	h.SetUsageRepos(userRepo, noteRepo)
	h.SetClock(func() time.Time { return now })
	return h, userRepo, noteRepo
}

func getNodeinfo(t *testing.T, fn func(echo.Context) error) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/nodeinfo", nil), rec)
	require.NoError(t, fn(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// 未認証の 1 リクエストごとに全表 COUNT を 2 本走らせない。TTL 内の 2 回目
// 以降は build せずに cache から返す (upstream の MemorySingleCache 10 分相当)。
func TestNodeinfo_CachesDocumentWithinTTL(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	h, userRepo, noteRepo := newCountingHandler(t, now)

	for i := 0; i < 5; i++ {
		out := getNodeinfo(t, h.Version2_1)
		usage := out["usage"].(map[string]any)
		assert.Equal(t, float64(1), usage["users"].(map[string]any)["total"])
	}
	assert.Equal(t, int64(1), userRepo.calls.Load(), "CountLocalUsers は TTL 内で 1 回だけ")
	assert.Equal(t, int64(1), noteRepo.calls.Load(), "CountLocalNotes は TTL 内で 1 回だけ")
}

// TTL が切れたら build し直す (= キャッシュが永久に固まらない)。
func TestNodeinfo_RebuildsAfterTTL(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	h, userRepo, _ := newCountingHandler(t, now)

	getNodeinfo(t, h.Version2_1)
	require.Equal(t, int64(1), userRepo.calls.Load())

	// TTL 直前はまだ hit。
	cur := now.Add(CacheTTL - time.Nanosecond)
	h.clock = func() time.Time { return cur }
	getNodeinfo(t, h.Version2_1)
	assert.Equal(t, int64(1), userRepo.calls.Load(), "TTL 内は cache hit のまま")

	// TTL ちょうどで失効する。
	cur = now.Add(CacheTTL)
	getNodeinfo(t, h.Version2_1)
	assert.Equal(t, int64(2), userRepo.calls.Load(), "TTL 経過後は build し直す")
}

// 2.0 と 2.1 で cache を共有しない。upstream は 1 つの cache を共有したうえで
// 2.0 側が software.repository を delete するので、2.0 を先に引くと 2.1 からも
// repository が消える。mk-go は version ごとに持つのでその穴が無い。
func TestNodeinfo_CacheIsPerVersion(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	h, _, _ := newCountingHandler(t, now)

	v20 := getNodeinfo(t, h.Version2_0)
	_, has20 := v20["software"].(map[string]any)["repository"]
	assert.False(t, has20, "schema 2.0 に software.repository は無い")

	v21 := getNodeinfo(t, h.Version2_1)
	assert.Equal(t, "2.1", v21["version"])
	assert.Equal(t, softwareRepository, v21["software"].(map[string]any)["repository"])

	// 逆順でも同じ (cache hit 経路を通しても混ざらない)。
	assert.Equal(t, "2.0", getNodeinfo(t, h.Version2_0)["version"])
	assert.Equal(t, "2.1", getNodeinfo(t, h.Version2_1)["version"])
}

// cache hit でも miss と同じヘッダを返す。ヘッダだけ落ちると中継キャッシュの
// 挙動が hit/miss で変わってしまう。
func TestNodeinfo_CachedResponseKeepsHeaders(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	h, _, _ := newCountingHandler(t, now)

	var bodies []string
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/nodeinfo/2.1", nil), rec)
		require.NoError(t, h.Version2_1(c))
		assert.Equal(t, `application/json; profile="http://nodeinfo.diaspora.software/ns/schema/2.1#"`, rec.Header().Get("Content-Type"))
		assert.Equal(t, "public, max-age=600", rec.Header().Get("Cache-Control"))
		assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"))
		assert.Equal(t, "Vary", rec.Header().Get("Access-Control-Expose-Headers"))
		bodies = append(bodies, rec.Body.String())
	}
	assert.Equal(t, bodies[0], bodies[1], "cache hit は miss と 1 byte 単位で同じ body")
}

// 並行 miss は singleflight で 1 build に集約する。集約しないと、cache が
// 空の瞬間を突いた並行リクエストがそのまま COUNT の並行実行になる。
//
// **build を全 caller が入場するまで止める。** sleep で「たぶん集まった」を
// 期待すると、集約していなくても 1 回しか数えない実行が出て空虚になる
// (`internal/api/fetchrss` の同種テストと同じ作り)。
func TestNodeinfo_ConcurrentMissesCollapse(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	h, userRepo, _ := newCountingHandler(t, now)

	const callers = 8
	var entered atomic.Int64
	userRepo.before = func() {
		// 全 caller が Version2_1 に入るまで待つ。集約されていれば待っている
		// のは leader 1 本だけで、他は singleflight の中で待っている。
		deadline := time.Now().Add(5 * time.Second)
		for entered.Load() < callers && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		// 入場直後 = まだ singleflight に登録される前かもしれないので一息置く。
		time.Sleep(20 * time.Millisecond)
	}

	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			entered.Add(1)
			rec := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/nodeinfo/2.1", nil), rec)
			assert.NoError(t, h.Version2_1(c))
			assert.Equal(t, http.StatusOK, rec.Code)
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(1), userRepo.calls.Load(),
		"singleflight must collapse concurrent nodeinfo builds; got %d COUNT round-trips for %d callers",
		userRepo.calls.Load(), callers)
}

// SetCacheTTL(0) で cache を切れる (毎回 build する)。
func TestNodeinfo_CacheDisabled(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	h, userRepo, _ := newCountingHandler(t, now)
	h.SetCacheTTL(0)

	getNodeinfo(t, h.Version2_1)
	getNodeinfo(t, h.Version2_1)
	assert.Equal(t, int64(2), userRepo.calls.Load())
}

// 配線 setter は cache を捨てる。捨てないと、起動時の配線順を変えただけで
// 古い document を TTL の間配り続ける。
func TestNodeinfo_SettersInvalidateCache(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	h, _, _ := newCountingHandler(t, now)

	out := getNodeinfo(t, h.Version2_1)
	meta := out["metadata"].(map[string]any)
	_, present := meta["mkGoPlugins"]
	require.False(t, present)

	h.SetPeeredPlugins([]string{"demo"})
	out = getNodeinfo(t, h.Version2_1)
	meta = out["metadata"].(map[string]any)
	assert.Equal(t, []any{"demo"}, meta["mkGoPlugins"], "setter 後は build し直す")
}

// 「受け付けない」(#3186) は、列だけが立っていても登録不可と名乗る。
// 正規化で disableRegistration も立つが、列を直接書き換えられた場合の備え。
func TestVersion2_1_RegistrationClosed(t *testing.T) {
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x", RegistrationClosed: true}
	h := NewHandler(&config.Config{Version: "0.0.0", Host: "example.com"})
	h.SetMetaRepo(metaRepo)

	e := echo.New()
	rec := httptest.NewRecorder()
	require.NoError(t, h.Version2_1(e.NewContext(httptest.NewRequest(http.MethodGet, "/nodeinfo/2.1", nil), rec)))
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["openRegistrations"])
	assert.Equal(t, true, resp["metadata"].(map[string]any)["disableRegistration"])
}
