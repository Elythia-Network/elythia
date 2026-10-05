package i

import (
	"net/http"
	"regexp"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/colfit"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// registryScopeElemRe mirrors upstream registry paramDef scope items pattern
// ^[a-zA-Z0-9_]+$ (#1546)。各 registry endpoint で scope 要素を検証する。
var registryScopeElemRe = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// registry の列幅 (`internal/model/registry_item.go`)。**幅を超えた値は
// SQLSTATE 22001 でクエリごと落ちる**ので、列に届く前に弾く (#3037)。
// `i/registry/*` は**任意の認証ユーザー**が叩けるので、長い文字列 1 つで
// 500 を起こせていた。
const (
	registryKeyMaxRunes    = 1024 // key varchar(1024)
	registryDomainMaxRunes = 512  // domain varchar(512)
	registryScopeMaxRunes  = 1024 // scope varchar(1024)[]
)

// validRegistryScope reports whether every scope element matches the upstream
// pattern and fits its column. Empty scope ([]) is valid.
//
// **正規表現だけでは足りない。** `^[a-zA-Z0-9_]+$` は文字種しか見ないので、
// 同じ文字を 1025 個並べるだけで列に入らない値が通っていた。
//
// **scope は読み取り側でも弾いてよい。** 元から `INVALID_PARAM` を返す述語
// (upstream の paramDef 相当) で、長すぎる要素も同じ「形が違う」の一種。
// `key` / `domain` のように「無い」を返す述語ではない。
func validRegistryScope(scope []string) bool {
	for _, s := range scope {
		if !registryScopeElemRe.MatchString(s) || !colfit.Fits(s, registryScopeMaxRunes) {
			return false
		}
	}
	return true
}

// storableRegistryValue reports whether a registry key / domain can be stored.
//
// **scope だけ検証していた (#3025)。** `key` と `domain` は無検証のまま
// `key = ?` / `domain = ?` の bind parameter に載るので、NUL を 1 文字入れると
// クエリごと落ちて 500 になる (`i/registry/get-all` などは**任意の認証
// ユーザー**が叩ける)。scope と同じ場所で弾く。
//
// **幅を見るのは書き込み側だけ (#3037)。** NUL は比較の右辺に置いただけで
// クエリごと落ちるのでどちらにも要るが、**幅は落ちない** — `varchar(10)` の列に
// 対する `WHERE v = repeat('a', 2000)` は 0 行を返すだけ (実測)。読み取り側にも
// 掛けると、1025 文字の key に対する応答が従来の `NO_SUCH_KEY` から
// `INVALID_PARAM` に変わってしまう。
//
// 書き込み側は 22001 で 500 になるので、#3022 と同じく**既存の述語に畳んで
// 既存の 400 に落とす** — 利用者にできることは変わらないので新しいエラー
// コードを足さない。
func storableRegistryValue(key string, domain *string) bool {
	return colfit.Storable(key) && (domain == nil || colfit.Storable(*domain))
}

// storableRegistryWrite is storableRegistryValue plus the column widths.
//
// `i/registry/set` だけが使う。読み取り側の応答コードを変えないための分離。
func storableRegistryWrite(key string, domain *string) bool {
	return colfit.Fits(key, registryKeyMaxRunes) &&
		(domain == nil || colfit.Fits(*domain, registryDomainMaxRunes))
}

// registryEffectiveDomain returns the domain a registry request operates on.
// An app access token forces its own id as the domain (= per-app isolated
// registry space; it cannot read/write the main domain=null space). A native
// login token / cookie session uses the request's domain unchanged (#1717,
// upstream `accessToken != null ? accessToken.id : ps.domain ?? null`)。
func registryEffectiveDomain(c echo.Context, reqDomain *string) *string {
	if scope := middleware.GetAuthScope(c); scope != nil && scope.IsApp {
		id := scope.TokenID
		return &id
	}
	return reqDomain
}

// registryScopeDomainRequest is the canonical input shape for registry
// read endpoints. scope は []string、domain は *string (省略可)。
type registryScopeDomainRequest struct {
	Scope  []string `json:"scope"`
	Domain *string  `json:"domain"`
}

// normalizeRegistryScope treats a nil Scope as an empty slice. JSON で
// `"scope"` が省略されると req.Scope は nil になるが、repository 側で
// model.StringArray(nil) は SQL NULL にシリアライズされ `scope = NULL` で
// どのレコードにも一致しなくなる (registry_item のデフォルト値は
// `'{}'`)。他の registry* ハンドラ (RegistryGet / RegistryGetAll /
// RegistrySet 等) と挙動を揃えるため nil → 空配列に寄せる。
func normalizeRegistryScope(scope []string) []string {
	if scope == nil {
		return []string{}
	}
	return scope
}

// RegistryGetDetail handles POST /api/i/registry/get-detail.
// 指定の (key, scope, domain) に該当する RegistryItem を返す。
func (h *Handler) RegistryGetDetail(c echo.Context) error {
	if h.registryRepo == nil {
		return c.JSON(http.StatusOK, map[string]any{})
	}
	u := middleware.GetUser(c)
	var req struct {
		Key string `json:"key"`
		registryScopeDomainRequest
	}
	if err := c.Bind(&req); err != nil || req.Key == "" {
		return apierr.JSONInvalidParam(c)
	}
	req.Scope = normalizeRegistryScope(req.Scope)
	if !validRegistryScope(req.Scope) || !storableRegistryValue(req.Key, req.Domain) {
		return apierr.JSONInvalidParam(c)
	}
	item, err := h.registryRepo.Get(u.ID, req.Key, req.Scope, registryEffectiveDomain(c, req.Domain))
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を「そんなキーは無い」にしない** (#2792)。registry は
		// クライアントの設定同期に使うので、障害を 400 で返すと「消えた」と
		// 判断して既定値で上書きしうる。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_KEY", "No such key.", "97a1e8e7-c0f7-47d2-957a-92e61256e01a"))
	}
	return c.JSON(http.StatusOK, map[string]any{
		// upstream i/registry/get-detail.ts:62 は updatedAt.toISOString()。
		"updatedAt": entity.ISOMillis(item.UpdatedAt),
		"value":     item.Value,
		"scope":     []string(item.Scope),
		"domain":    item.Domain,
	})
}

// RegistryKeys handles POST /api/i/registry/keys.
// 指定 scope+domain 下の key 一覧を返す (本家互換で配列)。
func (h *Handler) RegistryKeys(c echo.Context) error {
	if h.registryRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	u := middleware.GetUser(c)
	var req registryScopeDomainRequest
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	req.Scope = normalizeRegistryScope(req.Scope)
	if !validRegistryScope(req.Scope) || !storableRegistryValue("", req.Domain) {
		return apierr.JSONInvalidParam(c)
	}
	keysMap, err := h.registryRepo.KeysWithType(u.ID, req.Scope, registryEffectiveDomain(c, req.Domain))
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	keys := make([]string, 0, len(keysMap))
	for k := range keysMap {
		keys = append(keys, k)
	}
	return c.JSON(http.StatusOK, keys)
}

// RegistryScopesWithDomain handles POST /api/i/registry/scopes-with-domain.
// ユーザーが保存している (scope, domain) の distinct 一覧を返す。
func (h *Handler) RegistryScopesWithDomain(c echo.Context) error {
	if h.registryRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	u := middleware.GetUser(c)
	pairs, err := h.registryRepo.ScopesWithDomain(u.ID)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	// upstream getAllScopeAndDomains: domain ごとに集約し scopes は string[][]
	// (1 domain につき複数 scope を束ねる、#1546)。repo は DISTINCT (scope,domain)
	// を返すので domain 内の scope は既に一意。domain は null (main 空間) を保つ。
	type scopeEntry struct {
		Domain *string    `json:"domain"`
		Scopes [][]string `json:"scopes"`
	}
	out := make([]*scopeEntry, 0, len(pairs))
	byDomain := make(map[string]*scopeEntry, len(pairs))
	for _, p := range pairs {
		// nil domain と空文字 domain を区別するため sentinel を付与する。
		key := "\x00nil"
		if p.Domain != nil {
			key = "d:" + *p.Domain
		}
		sc := p.Scope
		if sc == nil {
			sc = []string{}
		}
		if e, ok := byDomain[key]; ok {
			e.Scopes = append(e.Scopes, sc)
		} else {
			e := &scopeEntry{Domain: p.Domain, Scopes: [][]string{sc}}
			byDomain[key] = e
			out = append(out, e)
		}
	}
	return c.JSON(http.StatusOK, out)
}
