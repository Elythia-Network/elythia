package meta

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/config"
	"github.com/shiroha-a/mk/internal/core/role"
	"github.com/shiroha-a/mk/internal/core/signup"
	"github.com/shiroha-a/mk/internal/misc/json5"
	"github.com/shiroha-a/mk/internal/repository"
	"gorm.io/datatypes"
)

// ProxyAccountResolver returns the username of the "proxy" system account.
// 呼び出し側 (router) が SystemAccountRepository + UserRepository を使って
// type='proxy' の system_account から userId を辿り、username を返す。
// 解決できないときは "" と false を返す。
type ProxyAccountResolver func() (string, bool)

// Handler handles meta-related API endpoints.
type Handler struct {
	config           *config.Config
	metaRepo         repository.MetaRepository
	adRepo           repository.AdRepository
	proxyAccountName ProxyAccountResolver
	// chunkedUpload は分割アップロード (#2313) の能力告知。nil / ok=false なら
	// field ごと出さない。
	chunkedUpload ChunkedUploadCapability
	// respCache は serialize 済みレスポンスのキャッシュ (#2649)。
	// 上の依存を差し替える SetXxx は、レスポンスの中身が変わるので
	// いずれも respCache を落とす。
	respCache metaResponseCache
}

// NewHandler creates a new meta Handler.
func NewHandler(cfg *config.Config, metaRepo repository.MetaRepository) *Handler {
	return &Handler{config: cfg, metaRepo: metaRepo}
}

// SetAdRepo wires an AdRepository for the `ads` field on the /api/meta
// response. When unset, ads is returned as an empty array so existing tests
// without ad wiring keep passing.
func (h *Handler) SetAdRepo(r repository.AdRepository) {
	h.adRepo = r
	h.respCache.invalidate()
}

// SetProxyAccountResolver wires the resolver used to populate the
// `proxyAccountName` field on MetaDetailed responses. When unset (tests or
// pre-setup instances), the field is reported as null.
func (h *Handler) SetProxyAccountResolver(r ProxyAccountResolver) {
	h.proxyAccountName = r
	h.respCache.invalidate()
}

// ChunkedUploadCapability describes the chunked upload support advertised on
// /api/meta (#2313). ok=false means the instance cannot serve chunked uploads,
// in which case the field is omitted entirely.
type ChunkedUploadCapability func() (chunkSize int64, ok bool)

// SetChunkedUploadCapability wires the capability probe for the additive
// `chunkedUpload` field. Unset (or reporting ok=false) omits the field, which
// is what clients use to fall back to the single-request upload path — pure
// Misskey never emits it either.
func (h *Handler) SetChunkedUploadCapability(f ChunkedUploadCapability) {
	h.chunkedUpload = f
	h.respCache.invalidate()
}

// chunkedUploadInfo returns the value of the additive `chunkedUpload` field,
// or nil when the capability is unavailable.
func (h *Handler) chunkedUploadInfo() map[string]any {
	if h.chunkedUpload == nil {
		return nil
	}
	chunkSize, ok := h.chunkedUpload()
	if !ok {
		return nil
	}
	return map[string]any{"chunkSize": chunkSize}
}

// Meta returns server metadata.
// POST /api/meta
// Meta handles POST /api/meta.
// TS互換: detail (boolean, default true)。falseの場合は簡易レスポンスを返す。
func (h *Handler) Meta(c echo.Context) error {
	var params struct {
		Detail *bool `json:"detail"`
	}
	if err := c.Bind(&params); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	detail := params.Detail == nil || *params.Detail

	body, gen := h.respCache.get(detail)
	if body == nil {
		resp, err := h.buildMeta(detail)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		marshaled, err := json.Marshal(resp)
		if err != nil {
			// ここに来る値は c.JSON でも書けないが、挙動を変えないため従来どおり
			// echo に委ねる。
			return c.JSON(http.StatusOK, resp)
		}
		// 本番の serializer (server.fastJSONSerializer) も echo 標準も
		// json.Encoder.Encode を使うので末尾に改行が付く。cache 経由でも bytes を
		// 揃えるためここで足す。Content-Type は c.JSON も c.JSONBlob も
		// MIMEApplicationJSON で同じ。
		body = append(marshaled, '\n')
		h.respCache.putIfCurrent(detail, body, gen)
	}

	// echo は ?pretty / Debug のとき indent 付きで書く (context.JSON)。cache は
	// compact な bytes しか持たないので、ここで同じ形に整える。**cache を
	// 素通しにはしない**: /api/meta は未認証で叩けて per-endpoint の rate limit も
	// 無いので、素通しにすると ?pretty を付けるだけで誰でもキャッシュを外せる。
	if _, pretty := c.QueryParams()["pretty"]; c.Echo().Debug || pretty {
		var buf bytes.Buffer
		if err := json.Indent(&buf, body, "", metaPrettyIndent); err != nil {
			return c.JSONBlob(http.StatusOK, body)
		}
		return c.JSONBlob(http.StatusOK, buf.Bytes())
	}
	return c.JSONBlob(http.StatusOK, body)
}

// buildMeta assembles the /api/meta response map. Split out of Meta so the
// response can be marshalled once and cached (#2649).
//
// レスポンスは meta 行 / 起動時 config / ads / proxy account /
// chunkedUpload の能力告知 / detail にしか依存せず、リクエストごとに変わる要素が
// 無いので丸ごとキャッシュできる。**chunkedUpload も含めて runtime に変わりうる
// ものは全て meta 行に帰着する** (能力告知の closure は metaRepo.Fetch() を読み、
// ストレージ backend も MetaStorage が meta から決める) ので、meta の
// invalidation で覆える。
func (h *Handler) buildMeta(detail bool) (map[string]any, error) {
	m, err := h.metaRepo.Fetch()
	if err != nil {
		return nil, err
	}

	// upstream MetaEntityService と同じく policies は { ...DEFAULT_POLICIES,
	// ...instance.policies } でマージする。admin が update-meta で driveCapacityMb
	// 等を変更しても以前は DefaultPolicies のみ返して反映されなかった。
	mergedPolicies := role.MergeMetaPolicies(m.Policies)

	resp := map[string]any{
		"maintainerName":  m.MaintainerName,
		"maintainerEmail": m.MaintainerEmail,
		"version":         h.config.Version,
		// mkGoVersion は mk-go 独自の additive field (#2274)。`version` は
		// drop-in 互換のため **互換 Misskey バージョン** を返す必要がある
		// (第三者クライアントの feature detection と、frontend の
		// `_error_.vue` が build 定数と比較する版ずれ検出がこれに依存する)
		// ので、実際に動いている実装の版は別 field で出す。
		"mkGoVersion": config.MkGoVersion,
		// minimumUsernameLength は mk-go 独自の additive field (#3015)。
		// **frontend の登録フォームが事前チェックに使う** — 出さないと、
		// 入力中は「使えます」と見えて送信時に初めて弾かれる。
		// upstream の `localUsernameSchema` は `^\w{1,20}$` 固定なので
		// 対応する field が無い。
		"minimumUsernameLength": signup.EffectiveMinimumUsernameLength(m),
		// ビルドした revision と同梱 frontend の版 (#2700)。/about-mkgo が
		// 「mk-go 1.3.0 (abc1234)」「Misskey 2026.9.0-mk.3」として出す。
		// **埋まっていないビルドでは空文字**になる (`go run` や build-arg を
		// 渡さない `docker compose build`)。読む側が表示を省くので、ここで
		// "unknown" のような代替値を作らない。
		"mkGoCommit":             config.MkGoCommit,
		"mkGoFrontendVersion":    config.MkGoFrontendVersion,
		"name":                   m.Name,
		"shortName":              m.ShortName,
		"uri":                    h.config.URL,
		"description":            m.Description,
		"langs":                  strArrayOrEmpty(m.Langs),
		"disableRegistration":    m.DisableRegistration || m.RegistrationClosed,
		"emailRequiredForSignup": m.EmailRequiredForSignup,
		// 承認制かどうかは登録ページが分岐に使うので公開する
		// (emailRequiredForSignup と同じ扱い、#2554)。
		"approvalRequiredForSignup": m.ApprovalRequiredForSignup,
		// 「受け付けない」(#3186)。入口と登録画面が表示の分岐に使う。
		"registrationClosed": m.RegistrationClosed,
		// 申請フォームの定義。申請ページが描画に使う (#2570)。
		"signupApplicationForm": jsonArrayOrEmpty(m.SignupApplicationForm),
		"enableHcaptcha":        m.EnableHcaptcha,
		"hcaptchaSiteKey":       m.HcaptchaSiteKey,
		"enableRecaptcha":       m.EnableRecaptcha,
		"recaptchaSiteKey":      m.RecaptchaSiteKey,
		"enableTurnstile":       m.EnableTurnstile,
		"turnstileSiteKey":      m.TurnstileSiteKey,
		"themeColor":            m.ThemeColor,
		"bannerUrl":             m.BannerURL,
		"backgroundImageUrl":    m.BackgroundImageURL,
		"logoImageUrl":          m.LogoImageURL,
		"iconUrl":               m.IconURL,
		"cacheRemoteFiles":      m.CacheRemoteFiles,
		"enableServiceWorker":   m.EnableServiceWorker,
		"swPublickey":           m.SwPublicKey,
		"serverRules":           strArrayOrEmpty(m.ServerRules),
		"maxNoteTextLength":     3000,

		// フロントエンド互換性フィールド (Phase 4.5c)
		"tosUrl":              m.TermsOfServiceURL,
		"repositoryUrl":       m.RepositoryURL,
		"feedbackUrl":         m.FeedbackURL,
		"impressumUrl":        m.ImpressumURL,
		"privacyPolicyUrl":    m.PrivacyPolicyURL,
		"inquiryUrl":          m.InquiryURL,
		"federation":          m.Federation,
		"defaultLightTheme":   packTheme(m.DefaultLightTheme),
		"defaultDarkTheme":    packTheme(m.DefaultDarkTheme),
		"serverErrorImageUrl": m.ServerErrorImageURL,
		"notFoundImageUrl":    m.NotFoundImageURL,
		"infoImageUrl":        m.InfoImageURL,
		// app192IconUrl / app512IconUrl / singleUserMode は upstream では
		// admin/meta にしか無く、公開 /api/meta の res schema に含まれない。
		// 管理画面 (admin/branding.vue / MkServerSetupWizard.vue) も admin/meta
		// から取るので公開側に載せる必要が無い (#2089 diff harness で検出)。
		// mascotImageUrl: meta 値があればそれを返す。空または nil なら従来の
		// /assets/ai.png にフォールバック (フロントエンドが no-image にならないため)。
		"mascotImageUrl":            mascotURL(m.MascotImageURL),
		"translatorAvailable":       m.DeeplAuthKey != nil && *m.DeeplAuthKey != "",
		"enableEmail":               m.EnableEmail,
		"enableUrlPreview":          m.URLPreviewEnabled,
		"ads":                       h.serializeActiveAds(),
		"notesPerOneAd":             m.NotesPerOneAd,
		"mediaProxy":                h.config.MediaProxy,
		"cacheRemoteSensitiveFiles": m.CacheRemoteSensitiveFiles,
		"requireSetup":              RequireSetup(m),
		// mk-go は /tarball/ を配信しないので常に false (#2700)。config の値を
		// そのまま返すと、SPA catchall が HTML を 200 で返すリンクを
		// 「ソースコード (Tarball)」として出すため config.ProvidesTarball() を通す。
		"providesTarball":              h.config.ProvidesTarball(),
		"maxFileSize":                  h.config.MaxFileSize,
		"proxyAccountName":             resolveProxyAccountName(h.proxyAccountName),
		"enableMcaptcha":               m.EnableMcaptcha,
		"mcaptchaSiteKey":              m.McaptchaSiteKey,
		"mcaptchaInstanceUrl":          m.McaptchaInstanceURL,
		"enableTestcaptcha":            m.EnableTestcaptcha,
		"sentryForFrontend":            sentryForFrontendJSON(h.config.SentryForFrontend),
		"googleAnalyticsMeasurementId": m.GoogleAnalyticsMeasurementID,
		// clientOptions は jsonb なのでそのまま返す。空 jsonb なら frontend が
		// デフォルト値を使う前提。
		"clientOptions": clientOptionsJSON(m.ClientOptions),

		"policies": mergedPolicies,

		"features": map[string]any{
			"registration":              !m.DisableRegistration && !m.RegistrationClosed,
			"emailRequiredForSignup":    m.EmailRequiredForSignup,
			"approvalRequiredForSignup": m.ApprovalRequiredForSignup,
			"localTimeline":             PolicyBool(mergedPolicies, "ltlAvailable"),
			"globalTimeline":            PolicyBool(mergedPolicies, "gtlAvailable"),
			"hcaptcha":                  m.EnableHcaptcha,
			"recaptcha":                 m.EnableRecaptcha,
			"turnstile":                 m.EnableTurnstile,
			"objectStorage":             m.UseObjectStorage,
			"serviceWorker":             m.EnableServiceWorker,
			"miauth":                    true,
		},
	}

	// noteSearchableScope: 検索 backend (fulltextSearch.provider) と meilisearch.scope
	// に基づき "local"/"global" を返す (upstream #17341 / 2026.5.0)。詳細は
	// NoteSearchableScope 関数の doc 参照。
	resp["noteSearchableScope"] = NoteSearchableScope(h.config.FulltextSearch, h.config.Meilisearch)

	// detail=false: TS MetaLite 互換。omit には golden MetaLite に**無い** field
	// (= MetaDetailedOnly / mk-go 内部 field) のみを入れる。golden MetaLite は
	// policies / clientOptions / sentryForFrontend / noteSearchableScope /
	// providesTarball を必須とし、Misskey の MetaEntityService.pack() (lite) も
	// 返すため、lite から落とすと client が非 null cast で落ちる (#1306 で
	// 旧 omit がこれらを誤って除外していたのを修正)。features / cacheRemoteFiles /
	// cacheRemoteSensitiveFiles / requireSetup / proxyAccountName は
	// MetaDetailedOnly なので lite から除外する。
	// 分割アップロード (#2313) の能力告知。mk-go 独自の additive field で、純正
	// Misskey では undefined になるのでフロントエンドの feature detection に使える。
	// 未対応構成 (オブジェクトストレージ未使用 / meta で無効) では field ごと出さず、
	// クライアントは従来の単発アップロードに倒れる。lite / detailed 両方に出す —
	// アップロード経路の判定はどちらの meta を持っていても必要になる。
	if info := h.chunkedUploadInfo(); info != nil {
		resp["chunkedUpload"] = info
	}

	if !detail {
		omit := map[string]struct{}{
			"features":                  {},
			"cacheRemoteFiles":          {},
			"cacheRemoteSensitiveFiles": {},
			"requireSetup":              {},
			"proxyAccountName":          {},
		}
		lite := make(map[string]any, len(resp))
		for k, v := range resp {
			if _, skip := omit[k]; !skip {
				lite[k] = v
			}
		}
		return lite, nil
	}

	return resp, nil
}

// Ping returns a simple pong response.
// POST /api/ping
func (h *Handler) Ping(c echo.Context) error {
	// upstream ping.ts は `{ pong: Date.now() }` (epoch-ms number) を返す。
	// misskey-js PingResponse.pong も number 型 (boolean ではない、#2027)。
	return c.JSON(http.StatusOK, map[string]any{
		"pong": time.Now().UnixMilli(),
	})
}

// strArrayOrEmpty coalesces a nil string slice to a non-nil empty slice so the
// JSON encoder emits `[]` instead of `null`. golden MetaLite の langs /
// serverRules は string[] 必須 (non-null) で、空でも null だと client が
// 非 null cast で落ちる (#1306。channels pinnedNoteIds #1283 と同種)。
func strArrayOrEmpty(a []string) []string {
	if a == nil {
		return []string{}
	}
	return a
}

// mascotURL applies the legacy fallback for the mascot. Misskey フロントエンドは
// このフィールドが空文字 / 未設定でも特定の no-image を出すだけだが、本家挙動と
// 互換にするため /assets/ai.png をフォールバックに使う。
func mascotURL(v *string) string {
	if v != nil && *v != "" {
		return *v
	}
	return "/assets/ai.png"
}

// clientOptionsJSON returns m.ClientOptions as a generic any. JSON encoder の
// raw bytes をそのまま返すと frontend が parse しないので、空の場合は空 map に
// 正規化する (本家フロントエンドは object を期待する)。
func clientOptionsJSON(raw []byte) any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

// resolveProxyAccountName looks up the `proxyAccountName` field via the
// optional resolver wired by the router. Returns nil when the resolver is
// absent (tests, pre-setup instances) or cannot find the system account.
// Returning nil keeps the JSON field as null, matching the TS behavior when
// no proxy account is configured.
func resolveProxyAccountName(r ProxyAccountResolver) any {
	if r == nil {
		return nil
	}
	name, ok := r()
	if !ok {
		return nil
	}
	return name
}

// sentryForFrontendJSON normalizes config.SentryForFrontend for JSON output.
// Empty (nil or zero-length) map is treated as absent and returns nil so
// the JSON response carries `null` (TS behavior: `?? null`).
func sentryForFrontendJSON(m map[string]any) any {
	if len(m) == 0 {
		return nil
	}
	return m
}

// PolicyBool reads a boolean policy value from role.DefaultPolicies() output
// with a conservative default. Unknown or non-bool values fall back to false
// so the client never sees an undefined flag in the features block.
func PolicyBool(policies map[string]any, key string) bool {
	v, ok := policies[key]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	if !ok {
		return false
	}
	return b
}

// NoteSearchableScope derives the "local"/"global" search scope flag from
// the search provider configuration.
//
// upstream Misskey #17341 (= 2026.5.0 fix / triage #1008): "local" を返すのは
// fulltextSearch.provider が "meilisearch" かつ meilisearch.scope が "local"
// のときだけで、それ以外は全て "global"。旧 upstream 実装は meilisearch block
// の有無だけ見ていたため、provider が sqlLike / none でも meilisearch.scope
// 設定が誤って反映される bug があった。本関数は新 upstream semantics に合わせる。
//
// mk-go 視点: 旧実装は default "local" を返していたが、これは sqlLike fallback
// でも remote note を検索できる挙動と矛盾していた (UI が local 検索 only と
// 解釈してしまう)。upstream 修正と同じく default "global" にすることで
// sqlLike / none の実挙動とも整合する。
func NoteSearchableScope(fts *config.FulltextSearchOptions, m *config.MeilisearchOptions) string {
	if fts == nil || fts.Provider != "meilisearch" {
		return "global"
	}
	if m == nil || m.Scope != "local" {
		return "global"
	}
	return "local"
}

// serializeActiveAds fetches currently-active ads via adRepo and projects
// them into the Misskey-compatible shape. Returns an empty slice when the
// repo is unwired (tests) or fetch fails, keeping the response stable.
// フロントエンドが notesPerOneAd と組み合わせて timeline に差し込む前提。
func (h *Handler) serializeActiveAds() []map[string]any {
	if h.adRepo == nil {
		return []map[string]any{}
	}
	ads, err := h.adRepo.ListActive(time.Now())
	if err != nil || len(ads) == 0 {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(ads))
	for _, a := range ads {
		entry := map[string]any{
			"id":        a.ID,
			"url":       a.URL,
			"place":     a.Place,
			"ratio":     a.Ratio,
			"imageUrl":  a.ImageURL,
			"dayOfWeek": a.DayOfWeek,
		}
		// upstream MetaEntityService は isSensitive: ad.isSensitive ? true : undefined。
		// sensitive のときだけ true を出し、false なら field を omit する (#1777)。
		if a.IsSensitive {
			entry["isSensitive"] = true
		}
		out = append(out, entry)
	}
	return out
}

// jsonArrayOrEmpty renders a JSONB column as an array, falling back to an empty
// one. **null を返さない** — frontend が配列前提で回すので、null だと描画側で
// 分岐が増える。
func jsonArrayOrEmpty(raw datatypes.JSON) any {
	if len(raw) == 0 {
		return []any{}
	}
	var out []any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return []any{}
	}
	return out
}

// PackTheme converts a stored JSON5 theme into JSON for the client (#2583).
//
// **Misskey のテーマは JSON5 で配布される。** キーが無引用符で文字列が
// シングルクォート。本家 MetaEntityService.pack は「クライアントの手間を減らす
// ためあらかじめJSONに変換しておく」として同じ変換をしており、生のまま返すと
// frontend の `JSON.parse(instance.defaultDarkTheme)` (boot/common.ts) が落ちて
// 起動しなくなる。
//
// **読めなければ null を返す。** 本家も catch で握りつぶしている。壊れたテーマ
// 1 つでインスタンス全体を落とさない。
func PackTheme(raw *string) any {
	if raw == nil || *raw == "" {
		return nil
	}
	out, err := json5.ToJSON(*raw)
	if err != nil {
		return nil
	}
	return out
}

// packTheme is the package-local alias used by the /api/meta response.
func packTheme(raw *string) any { return PackTheme(raw) }
