// Package nodeinfo provides /nodeinfo/* endpoints.
package nodeinfo

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/config"
	corereversi "github.com/shiroha-a/mk/internal/core/reversi"
	"github.com/shiroha-a/mk/internal/core/role"
	"github.com/shiroha-a/mk/internal/repository"
	"golang.org/x/sync/singleflight"
)

// maxNoteTextLength mirrors upstream MAX_NOTE_TEXT_LENGTH advertised in nodeinfo
// metadata。実際の投稿長 limit は別経路だが nodeinfo は upstream と同じ定数を出す。
const maxNoteTextLength = 3000

// CacheTTL mirrors upstream NodeinfoServerService の
// `new MemorySingleCache<...>(1000 * 60 * 10)` (10 分)。
//
// **サーバー側キャッシュが無いと未認証の 1 リクエストごとに全表 COUNT が
// 2 本走る** (`CountLocalUsers` + `CountLocalNotes`)。`/nodeinfo/*` は
// `s.echo.GET` 直付けで rate limiter (`/api` グループ専用) も掛からないので、
// 外部から無制限に回せる。Cache-Control: max-age=600 は返していたが、
// それは中継キャッシュ任せで origin には効かない。
const CacheTTL = 10 * time.Minute

// defaultThemeColor mirrors upstream NodeinfoServerService の
// `meta.themeColor ?? '#86b300'` fallback。
const defaultThemeColor = "#86b300"

// softwareRepository is the mk-go source repository URL advertised in the
// nodeinfo software block. Upstream advertises homepage/repository in the same
// object (2.0: homepage only; 2.1: homepage=repository=repositoryUrl); mk-go has
// no separate hub, so both point at the repository (#1925)。
//
// meta.repositoryUrl の既定値と同じ値なので config 側の定数を参照する (#2700)。
const softwareRepository = config.MkGoRepositoryURL

// Handler handles nodeinfo endpoints.
type Handler struct {
	cfg *config.Config
	// peeredPlugins は mk-go 専用のプラグイン通信 (#2537) を宣言した
	// プラグイン名。空なら metadata にキーごと出さない。
	peeredPlugins []string
	metaRepo      repository.MetaRepository
	userRepo      repository.UserRepository
	noteRepo      repository.NoteRepository
	proxyAccount  func() (string, bool)
	clock         func() time.Time

	// Server-side cache: version ("2.0" / "2.1") ごとに marshal 済み body を
	// TTL の間だけ保持する。cache miss が同時に来ても singleflight で 1 回の
	// build に集約するので、COUNT が並行に積み上がることもない。
	cacheTTL time.Duration
	cacheMu  sync.Mutex
	cache    map[string]cacheEntry
	sf       singleflight.Group
}

// cacheEntry は marshal 済みの nodeinfo document と、その有効期限。
type cacheEntry struct {
	body    []byte
	expires time.Time
}

// NewHandler constructs a Handler.
func NewHandler(cfg *config.Config) *Handler {
	return &Handler{
		cfg:      cfg,
		clock:    time.Now,
		cacheTTL: CacheTTL,
		cache:    make(map[string]cacheEntry),
	}
}

// SetCacheTTL overrides the server-side cache TTL. Intended for tests; in
// production the default (CacheTTL) matches the Cache-Control header so
// clients and the origin see the same freshness window.
//
// 0 以下を渡すとキャッシュを無効化する (毎回 build し直す)。
func (h *Handler) SetCacheTTL(d time.Duration) {
	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	h.cacheTTL = d
	h.cache = make(map[string]cacheEntry)
}

// now reads the injected clock, falling back to time.Now for a zero-value
// Handler (NewHandler always wires it).
func (h *Handler) now() time.Time {
	if h.clock == nil {
		return time.Now()
	}
	return h.clock()
}

// cacheGet returns the cached body for version when it is still fresh.
func (h *Handler) cacheGet(version string) ([]byte, bool) {
	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	if h.cacheTTL <= 0 {
		return nil, false
	}
	e, ok := h.cache[version]
	if !ok || !h.now().Before(e.expires) {
		return nil, false
	}
	return e.body, true
}

// cachePut stores the marshalled body for version.
func (h *Handler) cachePut(version string, body []byte) {
	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	if h.cacheTTL <= 0 {
		return
	}
	if h.cache == nil {
		h.cache = make(map[string]cacheEntry)
	}
	h.cache[version] = cacheEntry{body: body, expires: h.now().Add(h.cacheTTL)}
}

// invalidateCache drops every cached version. 配線 setter (SetMetaRepo /
// SetUsageRepos / SetPeeredPlugins / SetProxyAccountResolver / SetClock) が
// 呼ぶ。**呼ばないと、起動時に順序を変えただけで古い document を TTL の間
// 配り続ける**。運用では起動時に 1 度呼ばれるだけなので実質無コスト。
func (h *Handler) invalidateCache() {
	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	h.cache = make(map[string]cacheEntry)
}

// documentBody returns the marshalled nodeinfo document for version, building
// it at most once per TTL per version.
//
// **cache は version ごとに持つ。** upstream は 1 つの cache を共有して 2.0 で
// `delete base.software.repository` するため、2.0 を先に引くと 2.1 からも
// repository が消える。mk-go は version ごとに build して同じ穴を作らない。
func (h *Handler) documentBody(version string) ([]byte, error) {
	if body, ok := h.cacheGet(version); ok {
		return body, nil
	}
	v, err, _ := h.sf.Do(version, func() (any, error) {
		// singleflight 入場時に再確認する。直前に coalesce された呼び出しが
		// 積んでいれば build せずに済む。
		if body, ok := h.cacheGet(version); ok {
			return body, nil
		}
		body, err := json.Marshal(h.buildDocument(version))
		if err != nil {
			return nil, err
		}
		h.cachePut(version, body)
		return body, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]byte), nil
}

// SetPeeredPlugins declares the plugins that accept the mk-go-only plugin
// channel (#2537), so other instances can tell whether sending is worthwhile.
//
// **宣言したものだけを渡すこと。** 入れているプラグインを全部並べると、
// 運営者がどんな拡張を使っているかが攻撃面の情報になる。
func (h *Handler) SetPeeredPlugins(names []string) {
	h.peeredPlugins = names
	h.invalidateCache()
}

// SetMetaRepo injects a MetaRepository so that the nodeName / nodeDescription
// fields reflect the live admin settings instead of the config default.
// 未配線のまま呼ばれると cfg.Host fallback になる (#348)。
func (h *Handler) SetMetaRepo(r repository.MetaRepository) {
	h.metaRepo = r
	h.invalidateCache()
}

// SetUsageRepos injects repositories used to populate the usage statistics
// (users.total / activeMonth / activeHalfyear / localPosts / localComments).
// 未配線のまま呼ばれると対応 field は 0 のままになる (#403)。
func (h *Handler) SetUsageRepos(userRepo repository.UserRepository, noteRepo repository.NoteRepository) {
	h.userRepo = userRepo
	h.noteRepo = noteRepo
	h.invalidateCache()
}

// SetClock overrides the clock source. Intended for tests.
func (h *Handler) SetClock(now func() time.Time) {
	if now != nil {
		h.clock = now
		h.invalidateCache()
	}
}

// SetProxyAccountResolver wires the resolver used to populate the
// metadata.proxyAccountName field (#1777)。未配線なら proxyAccountName は null。
func (h *Handler) SetProxyAccountResolver(r func() (string, bool)) {
	h.proxyAccount = r
	h.invalidateCache()
}

// Version2_1 handles GET /nodeinfo/2.1.
func (h *Handler) Version2_1(c echo.Context) error {
	return h.serveDocument(c, "2.1")
}

// Version2_0 handles GET /nodeinfo/2.0. /.well-known/nodeinfo が 2.0 リンクも
// advertise するため、upstream NodeinfoServerService と同じく 2.0 も配信する
// (#1777)。schema 2.0 には software.repository が無いので省略する (upstream は
// version 2.0 で software.repository を delete する)。
func (h *Handler) Version2_0(c echo.Context) error {
	return h.serveDocument(c, "2.0")
}

// serveDocument serializes and writes the nodeinfo document with the upstream
// NodeinfoServerService response headers: a schema-profiled Content-Type
// (`application/json; profile="...schema/<ver>#"`), `Cache-Control: public,
// max-age=600`, and the CORS/Expose headers. c.JSON forces application/json so
// the document is written via c.Blob with an explicit Content-Type (#1948-22).
//
// 本体は documentBody が TTL 付きで cache する (upstream の MemorySingleCache
// 相当)。ヘッダは cache hit / miss で同じものを返す。
func (h *Handler) serveDocument(c echo.Context, version string) error {
	body, err := h.documentBody(version)
	if err != nil {
		return c.NoContent(http.StatusInternalServerError)
	}
	hdr := c.Response().Header()
	hdr.Set("Cache-Control", "public, max-age=600")
	hdr.Set("Access-Control-Allow-Headers", "Accept")
	hdr.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	hdr.Set("Access-Control-Allow-Origin", "*")
	hdr.Set("Access-Control-Expose-Headers", "Vary")
	ct := `application/json; profile="http://nodeinfo.diaspora.software/ns/schema/` + version + `#"`
	return c.Blob(http.StatusOK, ct, body)
}

// buildDocument assembles the nodeinfo document for the given schema version
// ("2.0" / "2.1"). software.repository は 2.1 のみ (schema 2.0 には無い)。
func (h *Handler) buildDocument(version string) map[string]any {
	nodeName := h.cfg.Host
	var (
		nodeDescription              string
		maintainerName               string
		maintainerEmail              string
		openRegistrations            bool
		disableRegistration          bool
		emailRequiredForSignup       bool
		enableHcaptcha               bool
		enableRecaptcha              bool
		enableMcaptcha               bool
		enableTurnstile              bool
		enableEmail                  bool
		enableServiceWorker          bool
		langs                        = []string{}
		themeColor                   = defaultThemeColor
		mergedPolicies               = role.MergeMetaPolicies(nil)
		tosURL, privacyURL, inqURL   any
		impressumURL, repoURL, fbURL any
	)
	if h.metaRepo != nil {
		if m, err := h.metaRepo.Fetch(); err == nil && m != nil {
			if m.Name != nil && *m.Name != "" {
				nodeName = *m.Name
			}
			if m.Description != nil {
				nodeDescription = *m.Description
			}
			if m.MaintainerName != nil {
				maintainerName = *m.MaintainerName
			}
			if m.MaintainerEmail != nil {
				maintainerEmail = *m.MaintainerEmail
			}
			// disableRegistration は「受け付けない」を有効にする更新で同時に立つが、列を
			// 直接書き換えられた場合にも登録可と名乗らないよう、ここでも見る (#3186)。
			openRegistrations = !m.DisableRegistration && !m.RegistrationClosed
			disableRegistration = m.DisableRegistration || m.RegistrationClosed
			emailRequiredForSignup = m.EmailRequiredForSignup
			enableHcaptcha = m.EnableHcaptcha
			enableRecaptcha = m.EnableRecaptcha
			enableMcaptcha = m.EnableMcaptcha
			enableTurnstile = m.EnableTurnstile
			enableEmail = m.EnableEmail
			enableServiceWorker = m.EnableServiceWorker
			if ls := []string(m.Langs); ls != nil {
				langs = ls
			}
			if m.ThemeColor != nil && *m.ThemeColor != "" {
				themeColor = *m.ThemeColor
			}
			mergedPolicies = role.MergeMetaPolicies([]byte(m.Policies))
			// *string をそのまま入れると nil は JSON null、値ありはその文字列になる。
			tosURL, privacyURL, inqURL = m.TermsOfServiceURL, m.PrivacyPolicyURL, m.InquiryURL
			impressumURL, repoURL, fbURL = m.ImpressumURL, m.RepositoryURL, m.FeedbackURL
		}
	}

	var proxyAccountName any
	if h.proxyAccount != nil {
		if name, ok := h.proxyAccount(); ok {
			proxyAccountName = name
		}
	}

	maintainer := map[string]any{"name": maintainerName, "email": maintainerEmail}
	metadata := map[string]any{
		"nodeName":        nodeName,
		"nodeDescription": nodeDescription,
		"nodeAdmins":      []map[string]any{maintainer},
		// deprecated だが upstream が残しているので維持する。
		"maintainer":             maintainer,
		"langs":                  langs,
		"tosUrl":                 tosURL,
		"privacyPolicyUrl":       privacyURL,
		"inquiryUrl":             inqURL,
		"impressumUrl":           impressumURL,
		"repositoryUrl":          repoURL,
		"feedbackUrl":            fbURL,
		"disableRegistration":    disableRegistration,
		"disableLocalTimeline":   !policyBool(mergedPolicies, "ltlAvailable"),
		"disableGlobalTimeline":  !policyBool(mergedPolicies, "gtlAvailable"),
		"emailRequiredForSignup": emailRequiredForSignup,
		"enableHcaptcha":         enableHcaptcha,
		"enableRecaptcha":        enableRecaptcha,
		"enableMcaptcha":         enableMcaptcha,
		"enableTurnstile":        enableTurnstile,
		"maxNoteTextLength":      maxNoteTextLength,
		"enableEmail":            enableEmail,
		"enableServiceWorker":    enableServiceWorker,
		"proxyAccountName":       proxyAccountName,
		"themeColor":             themeColor,
		// CherryPick 本家の reversi 連合拡張と互換性を示すバージョン。
		// 相手側 (CherryPick) はこの値のメジャーバージョン一致で連合可否を
		// 判定するので、破壊的変更が無い限り 1.1.x を維持する (#417 P3)。
		// corereversi.ReversiVersion と drift しないよう定数参照する。
		"reversiVersion": corereversi.ReversiVersion,
	}

	// 統計値は repo 経由で集計。未配線なら 0 (#403)。DB error は nodeinfo を
	// 丸ごと failさせるより partial 値で返す方が federation crawler に優しい
	// ので slog.Warn でログだけ残して 0 fallback する。
	// upstream NodeinfoServerService は activeHalfyear/activeMonth を `null` 固定
	// (コメント `// 重い`)、localComments を `0` 固定にしている。total と localPosts
	// だけ実集計する。strict parity のため mk-go も同じ wire 値に揃え、month/halfyear
	// の active-user 集計と localComments 集計の DB aggregation も省く (#1948-22)。
	var usersTotal, localPosts int64
	if h.userRepo != nil {
		if v, err := h.userRepo.CountLocalUsers(); err != nil {
			slog.Warn("nodeinfo: CountLocalUsers failed", "err", err)
		} else {
			usersTotal = v
		}
	}
	if h.noteRepo != nil {
		if v, err := h.noteRepo.CountLocalNotes(); err != nil {
			slog.Warn("nodeinfo: CountLocalNotes failed", "err", err)
		} else {
			localPosts = v
		}
	}

	// upstream NodeinfoServerService: software は name/version/homepage/repository
	// を持ち、2.0 は repository を delete (homepage は残す)、2.1 は homepage=repository。
	// mk-go は homepage=repository=softwareRepository で両 version に homepage を出す (#1925)。
	software := map[string]any{
		"name":     "mk-go",
		"version":  config.MkGoVersion,
		"homepage": softwareRepository,
	}
	// schema 2.0 には software.repository が無い (upstream は 2.0 で delete する)。
	if version == "2.1" {
		software["repository"] = softwareRepository
	}

	// mk-go 独自。相手が「同じプラグインを持っているか」を判断するのに使う
	// (#2537)。宣言が無ければキーごと出さない — 使っていないインスタンスが
	// 余計な情報を晒さないようにする。
	if len(h.peeredPlugins) > 0 {
		metadata["mkGoPlugins"] = h.peeredPlugins
	}

	return map[string]any{
		"version":   version,
		"software":  software,
		"protocols": []string{"activitypub"},
		"services": map[string]any{
			"inbound":  []string{},
			"outbound": []string{"atom1.0", "rss2.0"},
		},
		"openRegistrations": openRegistrations,
		"usage": map[string]any{
			"users": map[string]any{
				"total":          usersTotal,
				"activeMonth":    nil, // upstream は null 固定 (`// 重い`)
				"activeHalfyear": nil,
			},
			"localPosts":    localPosts,
			"localComments": 0, // upstream は 0 固定
		},
		"metadata": metadata,
	}
}

// policyBool reads a boolean policy value with a conservative false default
// (mirrors meta.PolicyBool; duplicated here to avoid importing the meta API
// package from nodeinfo)。
func policyBool(policies map[string]any, key string) bool {
	v, ok := policies[key]
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}
