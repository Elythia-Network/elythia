package meta

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/config"
	"github.com/shiroha-a/mk/internal/entitycompat/shapetest"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestHandler returns a handler with a mock meta repository.
//
// **同じ handler で Meta を 2 回呼ぶと 2 回目は response cache に当たる**
// (#2649)。「状態を変えてもう一度呼び、field が変わったことを確認する」型の
// テストを書くときは、間に h.InvalidateResponseCache() を挟むか handler を
// 作り直すこと。挟まないと cache が返って偽陽性で緑になる。
func newTestHandler() (*Handler, *testutil.MockMetaRepository) {
	cfg := &config.Config{
		Version: config.MisskeyVersion,
		URL:     "https://misskey.example.com",
	}
	metaRepo := testutil.NewMockMetaRepository()
	h := NewHandler(cfg, metaRepo)
	return h, metaRepo
}

func TestMeta(t *testing.T) {
	h, metaRepo := newTestHandler()

	name := "Test Instance"
	desc := "A test instance"
	maintainerName := "admin"
	metaRepo.Meta = &model.Meta{
		ID:             "x",
		Name:           &name,
		Description:    &desc,
		MaintainerName: &maintainerName,
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := h.Meta(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, "Test Instance", resp["name"])
	assert.Equal(t, "A test instance", resp["description"])
	assert.Equal(t, "admin", resp["maintainerName"])
	assert.Equal(t, config.MisskeyVersion, resp["version"])
	assert.Equal(t, "https://misskey.example.com", resp["uri"])
	assert.Equal(t, float64(3000), resp["maxNoteTextLength"])

	features, ok := resp["features"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, features["miauth"])

	// detail 応答は MetaLite ∪ MetaDetailedOnly (= MetaDetailed) の superset。
	// 両 schema で検証して detail 契約全体を gate する (#1308)。
	shapetest.Assert(t, "MetaLite", resp)         // L3 (#1306)
	shapetest.Assert(t, "MetaDetailedOnly", resp) // L3 (#1308)
}

// TestMeta_LiteShape は detail=false の応答が golden MetaLite 準拠であることを
// 検証する。旧 omit は MetaLite 必須 field (policies/clientOptions/
// sentryForFrontend/noteSearchableScope/providesTarball) を誤って lite から
// 落とし、逆に MetaDetailedOnly (cacheRemoteFiles 等) を leak していた (#1306)。
func TestMeta_LiteShape(t *testing.T) {
	h, metaRepo := newTestHandler()
	name := "Test Instance"
	metaRepo.Meta = &model.Meta{ID: "x", Name: &name}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", strings.NewReader(`{"detail":false}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, h.Meta(e.NewContext(req, rec)))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// MetaLite 必須 field が lite に含まれること。
	for _, f := range []string{"policies", "clientOptions", "sentryForFrontend", "noteSearchableScope", "providesTarball"} {
		_, ok := resp[f]
		assert.Truef(t, ok, "lite must include MetaLite field %q", f)
	}
	// MetaDetailedOnly field が lite に leak しないこと。
	for _, f := range []string{"features", "cacheRemoteFiles", "cacheRemoteSensitiveFiles", "requireSetup", "proxyAccountName"} {
		_, ok := resp[f]
		assert.Falsef(t, ok, "lite must omit MetaDetailedOnly field %q", f)
	}
	shapetest.Assert(t, "MetaLite", resp) // L3 (#1306)
}

func TestMeta_NoMeta(t *testing.T) {
	h, _ := newTestHandler()
	// metaRepo.Metaはnil（未設定）

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := h.Meta(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestPing(t *testing.T) {
	h, _ := newTestHandler()

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/ping", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := h.Ping(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// #2027: pong は epoch-ms number (boolean ではない)。JSON round-trip で float64。
	pong, ok := resp["pong"].(float64)
	require.True(t, ok, "pong は number (#2027)")
	assert.Greater(t, pong, float64(0))
}

// requireSetup は rootUserId が nil のとき true を返す。
func TestMeta_RequireSetupWhenNoRootUser(t *testing.T) {
	h, repo := newTestHandler()
	repo.Meta = &model.Meta{ID: "x"} // RootUserID = nil

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["requireSetup"])
}

// requireSetup は rootUserId が設定済みのとき false を返す。
func TestMeta_RequireSetupFalseWhenRootExists(t *testing.T) {
	h, repo := newTestHandler()
	rootID := "root123"
	repo.Meta = &model.Meta{ID: "x", RootUserID: &rootID}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["requireSetup"])
}

// --- Branding / UI exposure (issue #50) ---

// /api/meta が meta テーブルの新フィールド (mascot / app192/512 / themes /
// inquiryUrl / clientOptions / ga) を返すことを確認する。
func TestMeta_BrandingFieldsExposed(t *testing.T) {
	h, repo := newTestHandler()
	mascot := "https://example.com/mascot.png"
	srvErr := "https://example.com/err.png"
	notFound := "https://example.com/404.png"
	info := "https://example.com/info.png"
	dlt := "{\"name\":\"light\"}"
	ddt := "{\"name\":\"dark\"}"
	inquiry := "https://example.com/inquiry"
	ga := "G-XXXXXX"
	repo.Meta = &model.Meta{
		ID:                           "x",
		MascotImageURL:               &mascot,
		ServerErrorImageURL:          &srvErr,
		NotFoundImageURL:             &notFound,
		InfoImageURL:                 &info,
		DefaultLightTheme:            &dlt,
		DefaultDarkTheme:             &ddt,
		InquiryURL:                   &inquiry,
		GoogleAnalyticsMeasurementID: &ga,
		ClientOptions:                []byte(`{"entrancePageStyle":"simple","showTimelineForVisitor":true}`),
	}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	require.NoError(t, h.Meta(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, mascot, resp["mascotImageUrl"])
	// app192IconUrl / app512IconUrl は upstream では admin/meta にしか無く、
	// 公開 /api/meta の res schema に含まれない。管理画面も admin/meta から
	// 取るので公開側には出さない (#2089 diff harness で検出)。
	assert.NotContains(t, resp, "app192IconUrl")
	assert.NotContains(t, resp, "app512IconUrl")
	assert.NotContains(t, resp, "singleUserMode")
	assert.Equal(t, srvErr, resp["serverErrorImageUrl"])
	assert.Equal(t, notFound, resp["notFoundImageUrl"])
	assert.Equal(t, info, resp["infoImageUrl"])
	assert.Equal(t, dlt, resp["defaultLightTheme"])
	assert.Equal(t, ddt, resp["defaultDarkTheme"])
	assert.Equal(t, inquiry, resp["inquiryUrl"])
	assert.Equal(t, ga, resp["googleAnalyticsMeasurementId"])

	co, _ := resp["clientOptions"].(map[string]any)
	assert.Equal(t, "simple", co["entrancePageStyle"])
	assert.Equal(t, true, co["showTimelineForVisitor"])
}

// mascotImageUrl 未設定時は legacy /assets/ai.png にフォールバック。
func TestMeta_MascotFallback(t *testing.T) {
	h, repo := newTestHandler()
	repo.Meta = &model.Meta{ID: "x"} // mascot 未設定

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "/assets/ai.png", resp["mascotImageUrl"])
}

// 空文字 mascot もフォールバックされる。
func TestMeta_MascotEmptyFallback(t *testing.T) {
	h, repo := newTestHandler()
	empty := ""
	repo.Meta = &model.Meta{ID: "x", MascotImageURL: &empty}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "/assets/ai.png", resp["mascotImageUrl"])
}

// clientOptions が空 jsonb のとき、空 map にフォールバックする。
func TestMeta_ClientOptionsEmpty(t *testing.T) {
	h, repo := newTestHandler()
	repo.Meta = &model.Meta{ID: "x"} // ClientOptions = nil

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	co, ok := resp["clientOptions"].(map[string]any)
	assert.True(t, ok)
	assert.Empty(t, co)
}

// clientOptions が壊れた jsonb のとき、空 map にフォールバックして 200 を返す。
func TestMeta_ClientOptionsMalformed(t *testing.T) {
	h, repo := newTestHandler()
	repo.Meta = &model.Meta{ID: "x", ClientOptions: []byte(`not json`)}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	co, ok := resp["clientOptions"].(map[string]any)
	assert.True(t, ok)
	assert.Empty(t, co)
}

// policies は { ...DEFAULT_POLICIES, ...instance.policies } でマージされ、
// admin が update-meta で設定した override が /api/meta に反映される。
func TestMeta_PoliciesMergedWithInstanceOverride(t *testing.T) {
	h, repo := newTestHandler()
	repo.Meta = &model.Meta{ID: "x", Policies: []byte(`{"gtlAvailable":false,"mentionLimit":3}`)}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	policies, ok := resp["policies"].(map[string]any)
	require.True(t, ok)
	// override が効く。
	assert.Equal(t, false, policies["gtlAvailable"])
	assert.Equal(t, float64(3), policies["mentionLimit"])
	// 未設定 key は default。
	assert.Equal(t, true, policies["ltlAvailable"])
	// features.globalTimeline も merged policies 由来 (override 反映)。
	// なお upstream MetaEntityService は features.localTimeline を raw な
	// instance.policies.ltlAvailable から読むため未 override 時は undefined を
	// 返すが、mk-go は frontend 安全のため merged (= default true) を返す意図的
	// divergence。autogen misskey-js の boolean 型にも整合する。
	features := resp["features"].(map[string]any)
	assert.Equal(t, false, features["globalTimeline"])
	assert.Equal(t, true, features["localTimeline"])
}

// --- Ads / notesPerOneAd (issue #52) ---

// stubAdRepo implements repository.AdRepository for tests without a real DB.
type stubAdRepo struct {
	ads []*model.Ad
	err error
}

func (s *stubAdRepo) ListActive(_ time.Time) ([]*model.Ad, error) {
	return s.ads, s.err
}
func (s *stubAdRepo) Create(_ *model.Ad) error             { return nil }
func (s *stubAdRepo) FindByID(_ string) (*model.Ad, error) { return nil, nil }
func (s *stubAdRepo) List(_, _ int) ([]*model.Ad, error)   { return nil, nil }
func (s *stubAdRepo) ListFiltered(_ *bool, _, _ string, _ int, _ time.Time) ([]*model.Ad, error) {
	return nil, nil
}
func (s *stubAdRepo) UpdateFields(_ string, _ map[string]any) error { return nil }
func (s *stubAdRepo) Delete(_ string) error                         { return nil }

// /api/meta returns the active ads list from AdRepository and notesPerOneAd
// from meta. Frontend reads these and handles injection.
func TestMeta_AdsExposed(t *testing.T) {
	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x", NotesPerOneAd: 5}
	h.SetAdRepo(&stubAdRepo{ads: []*model.Ad{
		{ID: "ad1", URL: "https://example.com", Place: "square", Ratio: 1, ImageURL: "https://example.com/i.png", DayOfWeek: 0},
		{ID: "ad2", URL: "https://e2.example.com", Place: "horizontal", Ratio: 2, ImageURL: "https://e2.example.com/i.png", DayOfWeek: 3},
	}})

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, float64(5), resp["notesPerOneAd"])

	ads, ok := resp["ads"].([]any)
	require.True(t, ok, "ads should be an array")
	require.Len(t, ads, 2)
	first := ads[0].(map[string]any)
	assert.Equal(t, "ad1", first["id"])
	assert.Equal(t, "square", first["place"])
	assert.Equal(t, float64(1), first["ratio"])
}

// #1777: ads[].isSensitive は sensitive のときだけ true を出し、それ以外は omit する
// (upstream MetaEntityService の `ad.isSensitive ? true : undefined`)。
func TestMeta_AdsIsSensitive(t *testing.T) {
	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x"}
	h.SetAdRepo(&stubAdRepo{ads: []*model.Ad{
		{ID: "ad1", URL: "https://e.example", Place: "square", IsSensitive: true},
		{ID: "ad2", URL: "https://e2.example", Place: "square", IsSensitive: false},
	}})

	e := echo.New()
	rec := httptest.NewRecorder()
	require.NoError(t, h.Meta(e.NewContext(httptest.NewRequest(http.MethodPost, "/api/meta", nil), rec)))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ads := resp["ads"].([]any)
	require.Len(t, ads, 2)
	assert.Equal(t, true, ads[0].(map[string]any)["isSensitive"], "sensitive ad は true")
	_, has := ads[1].(map[string]any)["isSensitive"]
	assert.False(t, has, "非 sensitive ad は isSensitive を omit する")
}

// AdRepository が未設定の場合 (既存テスト互換) は空配列が返る。
func TestMeta_AdsUnwiredReturnsEmpty(t *testing.T) {
	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x", NotesPerOneAd: 0}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ads, ok := resp["ads"].([]any)
	require.True(t, ok)
	assert.Empty(t, ads)
	assert.Equal(t, float64(0), resp["notesPerOneAd"])
}

// AdRepository がエラーを返したとき、meta エンドポイント全体は落とさずに
// ads を空として継続する (DB 障害で /api/meta 全体が 500 になると起動ページ
// がロードできない)。
func TestMeta_AdsRepoErrorFallsBackToEmpty(t *testing.T) {
	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x"}
	h.SetAdRepo(&stubAdRepo{err: assert.AnError})

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ads, ok := resp["ads"].([]any)
	require.True(t, ok)
	assert.Empty(t, ads)
}

func TestMeta_DetailFalse(t *testing.T) {
	h, metaRepo := newTestHandler()
	name := "Instance"
	metaRepo.Meta = &model.Meta{ID: "x", Name: &name, EnableHcaptcha: true}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", strings.NewReader(`{"detail":false}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// 基本フィールドが含まれる
	assert.Equal(t, "Instance", resp["name"])
	assert.Equal(t, true, resp["enableHcaptcha"])
	assert.Equal(t, config.MisskeyVersion, resp["version"])
	// MetaLite に属する field は lite でも含まれる (#1306 で修正: 旧 omit は
	// これらを誤って省いていた)。
	assert.NotNil(t, resp["policies"])
	assert.NotNil(t, resp["clientOptions"])
	assert.Equal(t, "global", resp["noteSearchableScope"])
	// MetaDetailedOnly は lite から省かれる。
	assert.Nil(t, resp["features"])
	assert.Nil(t, resp["cacheRemoteFiles"])
	assert.Nil(t, resp["requireSetup"])
}

// TestMeta_FeaturesTimelineFlags verifies that features.localTimeline and
// features.globalTimeline are exposed from the default role policies, so the
// frontend can decide whether to render LTL/GTL tabs.
func TestMeta_FeaturesTimelineFlags(t *testing.T) {
	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x"}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	features, ok := resp["features"].(map[string]any)
	require.True(t, ok, "features should be present when detail=true")
	assert.Equal(t, true, features["localTimeline"], "ltlAvailable default is true")
	assert.Equal(t, true, features["globalTimeline"], "gtlAvailable default is true")
}

// upstream Misskey #17341 (= 2026.5.0 fix / triage #1008): noteSearchableScope は
// "local" を返すのが fulltextSearch.provider="meilisearch" かつ meilisearch.scope=
// "local" のときだけ、それ以外は全て "global"。旧 mk-go は default "local" を
// 返していたが、upstream 修正に合わせて default "global" に揃える。
//
// TestMeta_NoteSearchableScope_DefaultGlobal covers the fallback path where
// no search config exists: scope defaults to "global".
func TestMeta_NoteSearchableScope_DefaultGlobal(t *testing.T) {
	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x"}
	// config.FulltextSearch / config.Meilisearch are nil in newTestHandler().

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "global", resp["noteSearchableScope"])
}

// fulltextSearch.provider が "sqlLike" (= meilisearch を使っていない) のとき、
// meilisearch.scope="local" が誤って反映されないこと (= upstream #17341 bug 本体)。
func TestMeta_NoteSearchableScope_SqlLikeIgnoresMeilisearchScope(t *testing.T) {
	cfg := &config.Config{
		Version:        "2026.5.4",
		URL:            "https://misskey.example.com",
		FulltextSearch: &config.FulltextSearchOptions{Provider: "sqlLike"},
		Meilisearch:    &config.MeilisearchOptions{Host: "ms", Port: "7700", Scope: "local"},
	}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	h := NewHandler(cfg, metaRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "global", resp["noteSearchableScope"],
		"sqlLike provider 時は meilisearch.scope を無視して global を返す (upstream #17341)")
}

// fulltextSearch.provider="meilisearch" + meilisearch.scope="local" → "local"。
// 唯一の "local" 返却ケース。
func TestMeta_NoteSearchableScope_MeilisearchLocal(t *testing.T) {
	cfg := &config.Config{
		Version:        "2026.5.4",
		URL:            "https://misskey.example.com",
		FulltextSearch: &config.FulltextSearchOptions{Provider: "meilisearch"},
		Meilisearch:    &config.MeilisearchOptions{Host: "ms", Port: "7700", Scope: "local"},
	}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	h := NewHandler(cfg, metaRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "local", resp["noteSearchableScope"])
}

// fulltextSearch.provider="meilisearch" + meilisearch.scope="global" → "global"。
// meilisearch でも明示的に global scope を選んでいるケース。
func TestMeta_NoteSearchableScope_MeilisearchGlobal(t *testing.T) {
	cfg := &config.Config{
		Version:        "2026.5.4",
		URL:            "https://misskey.example.com",
		FulltextSearch: &config.FulltextSearchOptions{Provider: "meilisearch"},
		Meilisearch:    &config.MeilisearchOptions{Host: "ms", Port: "7700", Scope: "global"},
	}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	h := NewHandler(cfg, metaRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "global", resp["noteSearchableScope"])
}

func TestPolicyBool(t *testing.T) {
	cases := []struct {
		name     string
		policies map[string]any
		key      string
		want     bool
	}{
		{"present true", map[string]any{"k": true}, "k", true},
		{"present false", map[string]any{"k": false}, "k", false},
		{"missing", map[string]any{}, "k", false},
		{"non-bool type", map[string]any{"k": "yes"}, "k", false},
		{"nil value", map[string]any{"k": nil}, "k", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, PolicyBool(tc.policies, tc.key))
		})
	}
}

// --- Phase 7-5c follow-up (#256): providesTarball / sentryForFrontend /
//     proxyAccountName のフィールド追加 ---

// providesTarball は設定を有効にしても false のまま返す。mk-go には
// /tarball/ を配信するルートが無く、true にすると frontend が 404 になる
// 「ソースコード (Tarball)」リンクを出すため (#2700)。
func TestMeta_ProvidesTarball_IgnoresConfigFlag(t *testing.T) {
	cfg := &config.Config{
		Version: config.MisskeyVersion,
		URL:     "https://misskey.example.com",
		PublishTarballInsteadOfProvideRepositoryURL: true,
	}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	h := NewHandler(cfg, metaRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["providesTarball"], "設定が true でも /tarball/ は無いので false")
}

func TestMeta_ProvidesTarball_DefaultFalse(t *testing.T) {
	h, repo := newTestHandler()
	repo.Meta = &model.Meta{ID: "x"}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["providesTarball"])
}

// sentryForFrontendはconfig.SentryForFrontend mapをそのまま返し、空なら null。
func TestMeta_SentryForFrontend_Populated(t *testing.T) {
	cfg := &config.Config{
		Version: config.MisskeyVersion,
		URL:     "https://misskey.example.com",
		SentryForFrontend: map[string]any{
			"options": map[string]any{"dsn": "https://sentry.example/1"},
		},
	}
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x"}
	h := NewHandler(cfg, metaRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	sentry, ok := resp["sentryForFrontend"].(map[string]any)
	require.True(t, ok)
	opts, _ := sentry["options"].(map[string]any)
	assert.Equal(t, "https://sentry.example/1", opts["dsn"])
}

func TestMeta_SentryForFrontend_EmptyReturnsNil(t *testing.T) {
	h, repo := newTestHandler()
	repo.Meta = &model.Meta{ID: "x"}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, resp["sentryForFrontend"])
}

// proxyAccountNameはresolverが値を返せばそれをexpose、解決不能ならnull。
func TestMeta_ProxyAccountName_Resolved(t *testing.T) {
	h, repo := newTestHandler()
	repo.Meta = &model.Meta{ID: "x"}
	h.SetProxyAccountResolver(func() (string, bool) {
		return "instance.proxy", true
	})

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "instance.proxy", resp["proxyAccountName"])
}

func TestMeta_ProxyAccountName_ResolverReturnsFalse(t *testing.T) {
	h, repo := newTestHandler()
	repo.Meta = &model.Meta{ID: "x"}
	h.SetProxyAccountResolver(func() (string, bool) { return "", false })

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, resp["proxyAccountName"])
}

func TestMeta_ProxyAccountName_NoResolver(t *testing.T) {
	h, repo := newTestHandler()
	repo.Meta = &model.Meta{ID: "x"}
	// resolver未設定時はnil返却 (tests / pre-setup でのデフォルト動作)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, resp["proxyAccountName"])
}

// /api/meta は `version` に **互換 Misskey バージョン** を返し、mk-go 自身の
// 実装バージョンは additive な `mkGoVersion` で出すこと (#2274)。
//
// version を mk-go の版に差し替えると
//   - 第三者クライアントの feature detection (Misskey の版で機能有無を判断)
//   - frontend `_error_.vue` の版ずれ検出 (build 定数 === meta.version)
//
// が壊れるので、両者は必ず別 field で出す。
func TestMeta_ExposesMkGoVersionSeparately(t *testing.T) {
	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x"}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, h.Meta(e.NewContext(req, rec)))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, config.MisskeyVersion, resp["version"], "version は互換 Misskey 版のまま")
	assert.Equal(t, config.MkGoVersion, resp["mkGoVersion"], "mkGoVersion は mk-go の実装版")
	assert.NotEqual(t, resp["version"], resp["mkGoVersion"], "両者は別物として出す")
}

// ビルドした revision を additive に出すこと (#2700)。/about-mkgo が
// 「mk-go 1.3.0 (abc1234)」として使う。同梱 frontend の版 (mkGoFrontendVersion) は
// frontend を本体へ取り込んで版が本体と同じになったので出さない (#3379)。
func TestMeta_ExposesBuildRevision(t *testing.T) {
	// ldflags で埋める package 変数なので、テストからは書き換えて戻す。
	// **プロセス共有なので必ず復元する** — `-shuffle` を掛けた CI で、
	// 後続のテストが偽の値を見ることになる (#2795)。
	prevCommit := config.MkGoCommit
	t.Cleanup(func() { config.MkGoCommit = prevCommit })
	config.MkGoCommit = "abc1234"

	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x"}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, h.Meta(e.NewContext(req, rec)))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "abc1234", resp["mkGoCommit"])
	assert.NotContains(t, resp, "mkGoFrontendVersion", "廃止した field (#3379)")
}

// 埋め込みの無いビルド (`go run` / build-arg を渡さない image) では空文字で
// 出す。**field ごと落とさない** — 値の有無で表示を決めるのは frontend 側の
// 責務で、キーが消えると「古い mk-go か、埋め忘れか」を区別できなくなる。
func TestMeta_BuildRevisionIsEmptyWhenNotEmbedded(t *testing.T) {
	prevCommit := config.MkGoCommit
	t.Cleanup(func() { config.MkGoCommit = prevCommit })
	config.MkGoCommit = ""

	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x"}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, h.Meta(e.NewContext(req, rec)))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Contains(t, resp, "mkGoCommit", "埋まっていなくてもキーは出す")
	assert.Equal(t, "", resp["mkGoCommit"])
}

// #2313: 分割アップロードの能力告知。未対応構成では field ごと出さないので、
// 純正 Misskey と同じく undefined になり、クライアントは単発アップロードに
// フォールバックできる。
func TestMeta_ChunkedUploadCapability(t *testing.T) {
	metaFor := func(t *testing.T, h *Handler) map[string]any {
		t.Helper()
		e := echo.New()
		req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		require.NoError(t, h.Meta(e.NewContext(req, rec)))
		require.Equal(t, http.StatusOK, rec.Code)
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return resp
	}

	// 未配線 → field なし。
	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x"}
	_, present := metaFor(t, h)["chunkedUpload"]
	assert.False(t, present, "unwired capability must not emit the field")

	// 配線済みだが ok=false (= オブジェクトストレージ未使用 / 無効) → field なし。
	h.SetChunkedUploadCapability(func() (int64, bool) { return 0, false })
	_, present = metaFor(t, h)["chunkedUpload"]
	assert.False(t, present, "unavailable capability must not emit the field")

	// 利用可能 → chunkSize を返す。
	h.SetChunkedUploadCapability(func() (int64, bool) { return 10 * 1024 * 1024, true })
	got := metaFor(t, h)["chunkedUpload"]
	require.NotNil(t, got)
	assert.Equal(t, float64(10*1024*1024), got.(map[string]any)["chunkSize"])
}

// lite (detail:false) にも出す。アップロード経路の判定はどちらの meta を
// 持っていても必要になる。
func TestMeta_ChunkedUploadCapabilityInLiteResponse(t *testing.T) {
	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x"}
	h.SetChunkedUploadCapability(func() (int64, bool) { return 5 * 1024 * 1024, true })

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", strings.NewReader(`{"detail":false}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, h.Meta(e.NewContext(req, rec)))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp["chunkedUpload"])
	assert.Equal(t, float64(5*1024*1024), resp["chunkedUpload"].(map[string]any)["chunkSize"])
}

// 承認制かどうかは登録ページが分岐に使うので、公開 meta に出す (#2554)。
// **features 側にも出す** — frontend は features を feature detection に使う。
func TestMeta_ApprovalRequiredForSignup(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
			h, metaRepo := newTestHandler()
			metaRepo.Meta = &model.Meta{ID: "x", ApprovalRequiredForSignup: enabled}

			e := echo.New()
			req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			require.NoError(t, h.Meta(c))
			require.Equal(t, http.StatusOK, rec.Code)

			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, enabled, body["approvalRequiredForSignup"])

			features, ok := body["features"].(map[string]any)
			require.True(t, ok, "features should be an object")
			assert.Equal(t, enabled, features["approvalRequiredForSignup"])
		})
	}
}

// **Misskey のテーマは JSON5 で配布される (#2583)。** 生のまま返すと frontend の
// `JSON.parse(instance.defaultDarkTheme)` (boot/common.ts) が落ちて起動しない。
// 本家 MetaEntityService.pack と同じく、JSON に直してから返す。
func TestMeta_PacksThemeAsJSON(t *testing.T) {
	h, metaRepo := newTestHandler()
	theme := `{id:'6c445bcb',base:'dark',name:'夜の世界',props:{bg:'rgb(32, 34, 37)',hashtag:'@accent'}}`
	light := `{id:'l1',base:'light',props:{bg:'#fff'}}`
	metaRepo.Meta = &model.Meta{ID: "x", DefaultDarkTheme: &theme, DefaultLightTheme: &light}

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	require.NoError(t, h.Meta(e.NewContext(req, rec)))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	dark, ok := resp["defaultDarkTheme"].(string)
	require.True(t, ok, "文字列で返ること (client は JSON.parse する)")
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(dark), &parsed), "JSON として読めること")
	assert.Equal(t, "夜の世界", parsed["name"])
	assert.Equal(t, "rgb(32, 34, 37)", parsed["props"].(map[string]any)["bg"])

	lightOut, ok := resp["defaultLightTheme"].(string)
	require.True(t, ok)
	require.NoError(t, json.Unmarshal([]byte(lightOut), &parsed))
	assert.Equal(t, "light", parsed["base"])
}

// **読めなければ null。** 壊れたテーマ 1 つでインスタンス全体を落とさない
// (本家も catch で握りつぶす)。
func TestMeta_BrokenThemeBecomesNull(t *testing.T) {
	for name, raw := range map[string]string{
		"garbage":    "not a theme",
		"unbalanced": "{a:1",
		"empty":      "",
	} {
		t.Run(name, func(t *testing.T) {
			h, metaRepo := newTestHandler()
			v := raw
			metaRepo.Meta = &model.Meta{ID: "x", DefaultDarkTheme: &v}

			e := echo.New()
			rec := httptest.NewRecorder()
			require.NoError(t, h.Meta(e.NewContext(
				httptest.NewRequest(http.MethodPost, "/api/meta", nil), rec)))
			require.Equal(t, http.StatusOK, rec.Code)

			var resp map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Nil(t, resp["defaultDarkTheme"])
		})
	}
}

func TestMeta_UnsetThemeStaysNull(t *testing.T) {
	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x"}

	e := echo.New()
	rec := httptest.NewRecorder()
	require.NoError(t, h.Meta(e.NewContext(
		httptest.NewRequest(http.MethodPost, "/api/meta", nil), rec)))

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, resp["defaultDarkTheme"])
	assert.Nil(t, resp["defaultLightTheme"])
}

// 新規登録の最小文字数を additive field で出すこと (#3015)。
//
// **frontend の登録フォームが事前チェックに使う。** 出さないと入力中は
// 「使えます」に見えて、送信して初めて弾かれる。
//
// **出すのは列の生値ではなく「実際に効く値」。** 列が範囲外の値を持っていても
// (TS が書いた行、手で UPDATE した行)、登録側は clamp 後の値で判定するので、
// 生値を出すと表示と挙動がずれる。
func TestMeta_ExposesEffectiveMinimumUsernameLength(t *testing.T) {
	for name, tc := range map[string]struct {
		column int
		want   float64
	}{
		"列が未設定 (0) なら 1": {0, 1},
		"設定値をそのまま":       {5, 5},
		"範囲外は 20 に丸める":   {999, 20},
	} {
		t.Run(name, func(t *testing.T) {
			h, metaRepo := newTestHandler()
			metaRepo.Meta = &model.Meta{ID: "x", MinimumUsernameLength: tc.column}

			e := echo.New()
			req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			require.NoError(t, h.Meta(e.NewContext(req, rec)))
			require.Equal(t, http.StatusOK, rec.Code)

			var resp map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, tc.want, resp["minimumUsernameLength"])
		})
	}
}

// 「受け付けない」(#3186) は入口と登録画面が表示の分岐に使うので公開する。外から
// 見える登録可否 (disableRegistration / features.registration) は、列だけが立って
// いても登録不可にする。
func TestMeta_RegistrationClosed(t *testing.T) {
	for _, closed := range []bool{true, false} {
		t.Run(map[bool]string{true: "closed", false: "open"}[closed], func(t *testing.T) {
			h, metaRepo := newTestHandler()
			metaRepo.Meta = &model.Meta{ID: "x", RegistrationClosed: closed}

			e := echo.New()
			req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			require.NoError(t, h.Meta(e.NewContext(req, rec)))
			require.Equal(t, http.StatusOK, rec.Code)

			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, closed, body["registrationClosed"])
			assert.Equal(t, closed, body["disableRegistration"])
			assert.Equal(t, !closed, body["features"].(map[string]any)["registration"])
		})
	}
}
