package i

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// postRegistryWithScope posts to a registry handler with both the authenticated
// user and an AuthScope (app token / native) set in the context, so #1717
// domain-isolation can be exercised.
func postRegistryWithScope(h func(echo.Context) error, body string, user *model.User, scope *middleware.AuthScope) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), user)
	if scope != nil {
		c.Set(string(middleware.AuthScopeContextKey), scope)
	}
	_ = h(c)
	return rec
}

// firstRegistryItem returns the single stored item (tests store exactly one).
func firstRegistryItem(reg *testutil.MockRegistryRepository) *model.RegistryItem {
	for _, it := range reg.Items {
		return it
	}
	return nil
}

// #1717: app access token は registry domain を自分の token id に強制する。
func TestRegistrySet_AppTokenForcesDomain(t *testing.T) {
	h, _ := newExtraHandler(t)
	reg := testutil.NewMockRegistryRepository()
	h.SetRegistryRepo(reg)
	// client が domain を送っても app token は token id に上書きされる。
	rec := postRegistryWithScope(h.RegistrySet, `{"key":"theme","value":"dark","scope":["client"],"domain":"ignored.example"}`,
		stubUser, &middleware.AuthScope{IsApp: true, TokenID: "tok-1"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	it := firstRegistryItem(reg)
	require.NotNil(t, it)
	require.NotNil(t, it.Domain)
	assert.Equal(t, "tok-1", *it.Domain, "app token は domain を token id に強制する")
}

// native token / cookie session は request の domain をそのまま使う。
func TestRegistrySet_NativeUsesRequestDomain(t *testing.T) {
	h, _ := newExtraHandler(t)
	reg := testutil.NewMockRegistryRepository()
	h.SetRegistryRepo(reg)
	// AuthScope 無し (native)。
	rec := postRegistryWithScope(h.RegistrySet, `{"key":"theme","value":"dark","scope":["client"],"domain":"client.example"}`,
		stubUser, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	it := firstRegistryItem(reg)
	require.NotNil(t, it)
	require.NotNil(t, it.Domain)
	assert.Equal(t, "client.example", *it.Domain)
}

// app token の set は domain が token id (非 nil) に強制されるため main へ
// publish されない。
func TestRegistrySet_AppTokenNoMainPublish(t *testing.T) {
	h, _ := newExtraHandler(t)
	reg := testutil.NewMockRegistryRepository()
	h.SetRegistryRepo(reg)
	pub := &stubIMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)
	rec := postRegistryWithScope(h.RegistrySet, `{"key":"theme","value":"dark","scope":["client"]}`,
		stubUser, &middleware.AuthScope{IsApp: true, TokenID: "tok-1"})
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, pub.calls, "app token (domain=token id) の set は main へ broadcast しない")
}

// native token (domain なし) の set は main へ publish される (従来挙動)。
func TestRegistrySet_NativeMainPublish(t *testing.T) {
	h, _ := newExtraHandler(t)
	reg := testutil.NewMockRegistryRepository()
	h.SetRegistryRepo(reg)
	pub := &stubIMainStreamPublisher{}
	h.SetMainStreamPublisher(pub)
	rec := postRegistryWithScope(h.RegistrySet, `{"key":"theme","value":"dark","scope":["client"]}`, stubUser, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Len(t, pub.calls, 1)
	assert.Equal(t, "registryUpdated", pub.calls[0].eventType)
}

func TestRegistryGetDetail(t *testing.T) {
	h, _ := newExtraHandler(t)
	assert.Equal(t, http.StatusOK, postExtra(h.RegistryGetDetail, `{}`, stubUser).Code)
}

func TestRegistryKeys(t *testing.T) {
	h, _ := newExtraHandler(t)
	assert.Equal(t, http.StatusOK, postExtra(h.RegistryKeys, `{}`, stubUser).Code)
}

func TestRegistryScopesWithDomain(t *testing.T) {
	h, _ := newExtraHandler(t)
	assert.Equal(t, http.StatusOK, postExtra(h.RegistryScopesWithDomain, `{}`, stubUser).Code)
}

// #1546: scopes-with-domain は domain ごとに集約し scopes を string[][] で返す。
func TestRegistryScopesWithDomain_GroupedByDomain(t *testing.T) {
	h, _ := newExtraHandler(t)
	reg := testutil.NewMockRegistryRepository()
	dom := "client.example"
	require.NoError(t, reg.Set(&model.RegistryItem{ID: "r1", UserID: stubUser.ID, Key: "a", Scope: model.StringArray{"client", "base"}, Domain: &dom}))
	require.NoError(t, reg.Set(&model.RegistryItem{ID: "r2", UserID: stubUser.ID, Key: "b", Scope: model.StringArray{"client", "theme"}, Domain: &dom}))
	require.NoError(t, reg.Set(&model.RegistryItem{ID: "r3", UserID: stubUser.ID, Key: "c", Scope: model.StringArray{"main"}}))
	h.SetRegistryRepo(reg)

	rec := postExtra(h.RegistryScopesWithDomain, `{}`, stubUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var out []struct {
		Domain *string    `json:"domain"`
		Scopes [][]string `json:"scopes"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	byDomain := map[string][][]string{}
	for _, e := range out {
		k := "nil"
		if e.Domain != nil {
			k = *e.Domain
		}
		byDomain[k] = e.Scopes
	}
	require.Contains(t, byDomain, "client.example")
	assert.Len(t, byDomain["client.example"], 2, "同一 domain の scope を scopes 配列に束ねる")
	require.Contains(t, byDomain, "nil")
	assert.Len(t, byDomain["nil"], 1)
}

// #1546: scope 要素が ^[a-zA-Z0-9_]+$ に反すると INVALID_PARAM。
func TestRegistryScopePatternRejected(t *testing.T) {
	h, _ := newExtraHandler(t)
	h.SetRegistryRepo(testutil.NewMockRegistryRepository())
	// RegistryGetDetail (registry.go) と RegistryGetAll (handler.go) の両系統を確認。
	assert.Equal(t, http.StatusBadRequest, postExtra(h.RegistryGetDetail, `{"key":"k","scope":["bad scope"]}`, stubUser).Code)
	assert.Equal(t, http.StatusBadRequest, postExtra(h.RegistryGetAll, `{"scope":["has.dot"]}`, stubUser).Code)
	assert.Equal(t, http.StatusBadRequest, postExtra(h.RegistryKeys, `{"scope":["nope!"]}`, stubUser).Code)
	// 正常な scope は通る。
	assert.Equal(t, http.StatusOK, postExtra(h.RegistryKeys, `{"scope":["client","base_1"]}`, stubUser).Code)
}

// --- P4-6 (#166): i/registry/* ---

func TestRegistryGetDetail_Found(t *testing.T) {
	h, _ := newExtraHandler(t)
	reg := testutil.NewMockRegistryRepository()
	require.NoError(t, reg.Set(&model.RegistryItem{
		ID: "r1", UserID: stubUser.ID, Key: "theme", Value: datatypes.JSON(`"dark"`),
	}))
	h.SetRegistryRepo(reg)
	rec := postExtra(h.RegistryGetDetail, `{"key":"theme"}`, stubUser)
	assert.Equal(t, http.StatusOK, rec.Code)
}

// #1948-10: get-detail の updatedAt は upstream toISOString() (.000Z)。
// raw time.Time の RFC3339Nano だと wire-byte が乖離する。
func TestRegistryGetDetail_UpdatedAtFormat(t *testing.T) {
	h, _ := newExtraHandler(t)
	reg := testutil.NewMockRegistryRepository()
	require.NoError(t, reg.Set(&model.RegistryItem{
		ID: "r1", UserID: stubUser.ID, Key: "theme", Value: datatypes.JSON(`"dark"`),
		UpdatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}))
	h.SetRegistryRepo(reg)
	rec := postExtra(h.RegistryGetDetail, `{"key":"theme"}`, stubUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "2026-01-02T03:04:05.000Z", got["updatedAt"], "updatedAt は .000Z 形式 (#1948-10)")
}

func TestRegistryGetDetail_NotFound(t *testing.T) {
	h, _ := newExtraHandler(t)
	h.SetRegistryRepo(testutil.NewMockRegistryRepository())
	rec := postExtra(h.RegistryGetDetail, `{"key":"ghost"}`, stubUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// 本家互換: JSON で scope を省略した場合でも、default の empty-scope アイテムに
// マッチすること (nil スライスが SQL NULL に化けると常に miss する bug の
// regression test — Devin review #175)。
func TestRegistryGetDetail_OmittedScopeFindsDefaultItem(t *testing.T) {
	h, _ := newExtraHandler(t)
	reg := testutil.NewMockRegistryRepository()
	// Scope を明示せず作成 (Set は item.Scope をそのまま使う)。デフォルト
	// の空スライス状態で保存することを mock で再現する。
	require.NoError(t, reg.Set(&model.RegistryItem{
		ID: "r1", UserID: stubUser.ID, Key: "theme", Value: datatypes.JSON(`"dark"`),
		Scope: []string{},
	}))
	h.SetRegistryRepo(reg)
	// scope 無しリクエスト
	rec := postExtra(h.RegistryGetDetail, `{"key":"theme"}`, stubUser)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRegistryKeys_OmittedScopeFindsDefaultItem(t *testing.T) {
	h, _ := newExtraHandler(t)
	reg := testutil.NewMockRegistryRepository()
	require.NoError(t, reg.Set(&model.RegistryItem{
		ID: "r1", UserID: stubUser.ID, Key: "alpha", Value: datatypes.JSON(`"x"`),
		Scope: []string{},
	}))
	h.SetRegistryRepo(reg)
	rec := postExtra(h.RegistryKeys, `{}`, stubUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var got []string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Contains(t, got, "alpha")
}

func TestRegistryKeys_ReturnsKeysArray(t *testing.T) {
	h, _ := newExtraHandler(t)
	reg := testutil.NewMockRegistryRepository()
	require.NoError(t, reg.Set(&model.RegistryItem{ID: "r1", UserID: stubUser.ID, Key: "a", Value: datatypes.JSON(`"x"`)}))
	require.NoError(t, reg.Set(&model.RegistryItem{ID: "r2", UserID: stubUser.ID, Key: "b", Value: datatypes.JSON(`"y"`)}))
	h.SetRegistryRepo(reg)
	rec := postExtra(h.RegistryKeys, `{}`, stubUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var got []string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Len(t, got, 2)
}

func TestRegistryScopesWithDomain_ReturnsDistinctPairs(t *testing.T) {
	h, _ := newExtraHandler(t)
	reg := testutil.NewMockRegistryRepository()
	require.NoError(t, reg.Set(&model.RegistryItem{ID: "r1", UserID: stubUser.ID, Key: "a", Value: datatypes.JSON(`"x"`), Scope: []string{"client"}}))
	require.NoError(t, reg.Set(&model.RegistryItem{ID: "r2", UserID: stubUser.ID, Key: "b", Value: datatypes.JSON(`"y"`), Scope: []string{"client"}}))
	require.NoError(t, reg.Set(&model.RegistryItem{ID: "r3", UserID: stubUser.ID, Key: "c", Value: datatypes.JSON(`"z"`), Scope: []string{"default"}}))
	h.SetRegistryRepo(reg)
	rec := postExtra(h.RegistryScopesWithDomain, `{}`, stubUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var got []struct {
		Domain *string    `json:"domain"`
		Scopes [][]string `json:"scopes"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	// #1546: domain (ここでは nil のみ) ごとに集約され、2 distinct scope が
	// scopes 配列に束ねられる。
	require.Len(t, got, 1)
	assert.Nil(t, got[0].Domain)
	assert.Len(t, got[0].Scopes, 2)
}

// failingRegistryRepo makes every Get look like a database failure.
type failingRegistryRepo struct {
	*testutil.MockRegistryRepository
	err error
}

func (r *failingRegistryRepo) Get(string, string, []string, *string) (*model.RegistryItem, error) {
	return nil, r.err
}

// **DB 障害を「そんなキーは無い」にしない** (#2792)。
//
// registry はクライアントの設定同期に使うので、障害を 400 で返すと「消えた」と
// 判断して既定値で上書きしうる。not-found だけが 400。
func TestRegistryGet_DBFailureIsNot4xx(t *testing.T) {
	dbErr := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")

	for _, tt := range []struct {
		name string
		call func(*Handler) func(echo.Context) error
	}{
		{"i/registry/get", func(h *Handler) func(echo.Context) error { return h.RegistryGet }},
		{"i/registry/get-detail", func(h *Handler) func(echo.Context) error { return h.RegistryGetDetail }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := newExtraHandler(t)
			h.SetRegistryRepo(&failingRegistryRepo{
				MockRegistryRepository: testutil.NewMockRegistryRepository(),
				err:                    dbErr,
			})

			rec := postRegistryWithScope(tt.call(h),
				`{"key":"theme","scope":["client"]}`, stubUser, nil)
			assert.Equal(t, http.StatusInternalServerError, rec.Code,
				"DB 障害が 4xx に化けている (#2792)")
		})
	}

	t.Run("not-found は 400 のまま", func(t *testing.T) {
		h, _ := newExtraHandler(t)
		h.SetRegistryRepo(testutil.NewMockRegistryRepository()) // 空 = ErrNotFound

		rec := postRegistryWithScope(h.RegistryGet,
			`{"key":"missing","scope":["client"]}`, stubUser, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "NO_SUCH_KEY")
	})
}

// **列に入らない registry の値を弾く (#3037)。**
//
// `i/registry/*` は**任意の認証ユーザー**が叩ける。`key` varchar(1024) /
// `domain` varchar(512) / `scope` varchar(1024)[] を超える値は SQLSTATE 22001 で
// クエリごと落ちるので、長い文字列 1 つで 500 を起こせていた。NUL (#3025) は
// 既に塞いであったが、幅は見ていなかった。
func TestRegistryGuards_RejectOverwideValues(t *testing.T) {
	long := strings.Repeat("a", 1025)

	assert.False(t, storableRegistryWrite(long, nil), "key の幅を見ていない")
	domain := strings.Repeat("d", 513)
	assert.False(t, storableRegistryWrite("k", &domain), "domain の幅を見ていない")
	assert.False(t, validRegistryScope([]string{long}), "scope の幅を見ていない")

	// NUL は従来どおり (回帰していないこと)。
	assert.False(t, storableRegistryValue("a\x00b", nil))
	nulDomain := "a\x00b"
	assert.False(t, storableRegistryValue("k", &nulDomain))

	// **読み取り側は幅を見ない。** 列幅を超えた値は比較の右辺では落ちない
	// (`varchar(10)` の列に 2000 文字を比べても 0 行が返るだけ、実測) ので、
	// 応答は従来どおり `NO_SUCH_KEY` でなければならない。幅で 400 にすると
	// wire のエラーコードが変わる。
	assert.True(t, storableRegistryValue(long, nil), "読み取り側で幅を弾いている")
	assert.True(t, storableRegistryValue("k", &domain), "読み取り側で幅を弾いている")
}

// **境界ちょうどと普通の値は通ったまま。** これが無いと「常に拒否する」実装でも
// 上のテストが通る。
func TestRegistryGuards_AcceptOrdinaryValues(t *testing.T) {
	assert.True(t, storableRegistryWrite(strings.Repeat("a", 1024), nil), "key の上限ちょうどを弾いている")
	domain := strings.Repeat("d", 512)
	assert.True(t, storableRegistryWrite("k", &domain), "domain の上限ちょうどを弾いている")
	assert.True(t, validRegistryScope([]string{strings.Repeat("a", 1024)}), "scope の上限ちょうどを弾いている")
	assert.True(t, storableRegistryValue("client", nil))
	assert.True(t, validRegistryScope([]string{"client", "base"}))
	assert.True(t, validRegistryScope(nil))
}

// **value は jsonb 列へそのまま入る (#3037)。** PostgreSQL は NUL エスケープを
// SQLSTATE 22P05 でクエリごと落とすので、引く前に弾かないと**任意の認証
// ユーザーが 500 を起こせる**。
func TestRegistrySet_UnstorableValueIs400(t *testing.T) {
	esc := `\u0000`
	for name, body := range map[string]string{
		"値に NUL":  `{"key":"theme","value":{"a":"x` + esc + `y"},"scope":["client"]}`,
		"キーに NUL": `{"key":"theme","value":{"x` + esc + `y":"a"},"scope":["client"]}`,
		"配列の中":    `{"key":"theme","value":["ok","x` + esc + `y"],"scope":["client"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			h, _ := newExtraHandler(t)
			reg := testutil.NewMockRegistryRepository()
			h.SetRegistryRepo(reg)
			rec := postRegistryWithScope(h.RegistrySet, body, stubUser, nil)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "列に入らない value を受け入れている")
			assert.Empty(t, reg.Items)
		})
	}
}

// **普通の値は通ったまま。** これが無いと「常に拒否する」実装でも上が通る。
// バックスラッシュ自体がエスケープされた形 (ただの 6 文字) も通ること。
func TestRegistrySet_OrdinaryValueStillPasses(t *testing.T) {
	h, _ := newExtraHandler(t)
	reg := testutil.NewMockRegistryRepository()
	h.SetRegistryRepo(reg)
	rec := postRegistryWithScope(h.RegistrySet,
		`{"key":"theme","value":{"あ":"絵文字","b":"x\\u0000y"},"scope":["client"]}`, stubUser, nil)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, reg.Items, 1)
}
