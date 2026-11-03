package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	apimeta "github.com/elythia-network/elythia/internal/api/meta"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

// buildSSRMetaKeys renders the embedded meta and returns its top-level keys.
func buildSSRMetaKeys(t *testing.T, cfg *config.Config, m *model.Meta) map[string]any {
	t.Helper()
	raw := buildMetaJSON(cfg, m, nil, nil, nil, nil)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &out))
	return out
}

// buildAPIMetaKeys calls /api/meta and returns its top-level keys. detail は
// 既定で true なので body を送らなくても detail 相当が返る。
func buildAPIMetaKeys(t *testing.T, cfg *config.Config, m *model.Meta) map[string]any {
	t.Helper()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = m
	h := apimeta.NewHandler(cfg, metaRepo)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, h.Meta(c))
	require.Equal(t, http.StatusOK, rec.Code)

	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// TestSSRMetaCoversAPIMeta guards the SSR embedded meta against key drift.
//
// **#2570 でこの漏れが実際に機能を殺した。** 申請フォームの定義を `/api/meta` には
// 足したが SSR 埋め込みに足し忘れ、フォームを 1 つでも定義すると申請が完了不能に
// なった (client は定義を空と見て回答を 0 件送り、サーバーは定義どおりの件数を
// 期待して弾き続ける)。同じ罠は #2313 の分割アップロードでも踏みかけている。
//
// 埋め込みが効くのは、frontend の instance.ts が `data-generated-at` を見て
// localStorage cache を SSR 値で上書きし、**以後 1 時間 `/api/meta` を再取得
// しない**ため。載せ忘れたキーは「反映が最大 1 時間遅れる」のではなく、その間
// ずっと未設定として振る舞う。
//
// 現状 SSR 埋め込みは `/api/meta` の superset なので**除外リストを持たない**。
// 除外が要るキーが出たら、まず初期描画に要らないかを確かめること。要らないと
// 判断したなら、リストに 1 行足して済ませるのではなく、**なぜ 1 時間 未設定でも
// 平気なのか**を書いてから足す。
func TestSSRMetaCoversAPIMeta(t *testing.T) {
	cfg := &config.Config{Version: config.MisskeyVersion, URL: "https://misskey.example.com"}
	m := &model.Meta{ID: "x"}

	ssr := buildSSRMetaKeys(t, cfg, m)
	api := buildAPIMetaKeys(t, cfg, m)
	require.NotEmpty(t, api, "比較対象が空だと素通りする")

	var missing []string
	for k := range api {
		if _, ok := ssr[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)

	require.Emptyf(t, missing, "SSR 埋め込み meta に無いキー: %v", missing)
}

// 承認制の登録が SSR 埋め込みだけで成立することを名指しで確かめる。上の網羅
// テストはキーの有無しか見ないので、値の形は個別に固定する。
func TestSSRMetaCarriesSignupApplicationForm(t *testing.T) {
	cfg := &config.Config{Version: config.MisskeyVersion, URL: "https://misskey.example.com"}

	t.Run("carries the definition", func(t *testing.T) {
		m := &model.Meta{
			ID:                        "x",
			ApprovalRequiredForSignup: true,
			SignupApplicationForm:     []byte(`[{"label":"動機","type":"textarea","required":true}]`),
		}
		ssr := buildSSRMetaKeys(t, cfg, m)

		fields, ok := ssr["signupApplicationForm"].([]any)
		require.True(t, ok, "signupApplicationForm が配列で出ること")
		require.Len(t, fields, 1)
		first, ok := fields[0].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "動機", first["label"])
	})

	// 未設定・壊れた JSON は空配列。**null を出すと Array.isArray が false になり、
	// 申請ページが定義を読めない。**
	for name, raw := range map[string]string{
		"unset":  ``,
		"broken": `{"not":"an array"}`,
		"null":   `null`,
	} {
		t.Run("empty array for "+name, func(t *testing.T) {
			m := &model.Meta{ID: "x", SignupApplicationForm: []byte(raw)}
			ssr := buildSSRMetaKeys(t, cfg, m)
			require.Equal(t, []any{}, ssr["signupApplicationForm"])
		})
	}
}

// 登録 username の最小文字数が SSR 埋め込みだけで正しく効くこと (#3015)。
//
// 網羅テストはキーの有無しか見ないので、**clamp 後の「実際に効く値」**を
// 出していることはここで固定する。列の生値をそのまま出すと、範囲外の行で
// 登録フォームの事前チェックとサーバーの判定がずれる。
func TestSSRMetaCarriesMinimumUsernameLength(t *testing.T) {
	cfg := &config.Config{Version: config.MisskeyVersion, URL: "https://misskey.example.com"}

	for name, tc := range map[string]struct {
		column int
		want   float64
	}{
		"列が未設定 (0) なら 1": {0, 1},
		"設定値をそのまま":       {5, 5},
		"範囲外は 20 に丸める":   {999, 20},
	} {
		t.Run(name, func(t *testing.T) {
			ssr := buildSSRMetaKeys(t, cfg, &model.Meta{ID: "x", MinimumUsernameLength: tc.column})
			require.Equal(t, tc.want, ssr["minimumUsernameLength"])
		})
	}
}

// ページに埋め込む meta も、「受け付けない」(#3186) の間は登録不可と名乗る。
// 入口の表示は最初にこちらを読むので、/api/meta だけ直すと再取得までの間ずれる。
func TestSSRMeta_RegistrationClosed(t *testing.T) {
	cfg := &config.Config{URL: "https://example.com"}
	out := buildSSRMetaKeys(t, cfg, &model.Meta{ID: "x", RegistrationClosed: true})
	require.Equal(t, true, out["registrationClosed"])
	require.Equal(t, true, out["disableRegistration"])
	require.Equal(t, false, out["features"].(map[string]any)["registration"])
}

// ページに埋め込む meta の広告は、/api/meta と同じものを同じ形で出す (#3967)。
//
// 入口の画面は埋め込みを採ったとき /api/meta を取り直さないので、埋め込みの
// ads が空だと入口のタイムラインに広告が出ない。期間の判定は ListActive の
// 責務なので、ここでは射影 (isSensitive を真のときだけ出す、など) と、
// 渡した repository が使われることを見る。
func TestSSRMetaCarriesTheSameAdsAsAPIMeta(t *testing.T) {
	cfg := &config.Config{Version: config.MisskeyVersion, URL: "https://misskey.example.com"}
	m := &model.Meta{ID: "x", NotesPerOneAd: 5}
	now := time.Now()
	ads := testutil.NewMockAdRepository()
	ads.Ads["ad1"] = &model.Ad{ID: "ad1", URL: "https://ad.example/1", Place: "square", Ratio: 1, ImageURL: "https://ad.example/1.png", DayOfWeek: 0,
		StartsAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}
	ads.Ads["ad2"] = &model.Ad{ID: "ad2", URL: "https://ad.example/2", Place: "horizontal", Ratio: 2, ImageURL: "https://ad.example/2.png", DayOfWeek: 1 << int(now.Weekday()), IsSensitive: true,
		StartsAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}

	var ssr map[string]any
	require.NoError(t, json.Unmarshal([]byte(buildMetaJSON(cfg, m, nil, nil, nil, ads)), &ssr))

	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = m
	h := apimeta.NewHandler(cfg, metaRepo)
	h.SetAdRepo(ads)
	rec := httptest.NewRecorder()
	require.NoError(t, h.Meta(echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/api/meta", nil), rec)))
	var api map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &api))

	got, ok := ssr["ads"].([]any)
	require.True(t, ok, "ads must be an array: %v", ssr["ads"])
	require.Len(t, got, 2, "有効な広告が埋め込みに入っていない")
	require.Equal(t, api["ads"], ssr["ads"], "埋め込みと /api/meta で広告の形が違う")

	sensitive := got[0].(map[string]any)
	require.Equal(t, "ad2", sensitive["id"])
	require.Equal(t, true, sensitive["isSensitive"])
	require.NotContains(t, got[1].(map[string]any), "isSensitive", "isSensitive は真のときだけ出す")
}

// 広告を引けないときは空の配列にし、HTML の描画は止めない (/api/meta と同じ扱い)。
func TestSSRMetaAdsEmptyWithoutRepoOrOnError(t *testing.T) {
	cfg := &config.Config{Version: config.MisskeyVersion, URL: "https://misskey.example.com"}
	failing := testutil.NewMockAdRepository()
	failing.ListErr = errors.New("db down")
	now := time.Now()
	failing.Ads["ad1"] = &model.Ad{ID: "ad1", StartsAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}

	for name, repo := range map[string]repository.AdRepository{
		"no repository": nil,
		"fetch error":   failing,
	} {
		t.Run(name, func(t *testing.T) {
			metaRepo := testutil.NewMockMetaRepository()
			metaRepo.Meta = &model.Meta{ID: "x"}
			rec := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
			require.NoError(t, frontendHTML(cfg, metaRepo, nil, nil, nil, repo)(c))
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, []any{}, extractEmbeddedMeta(t, rec.Body.String())["ads"])
		})
	}
}
