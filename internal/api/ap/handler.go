// Package ap provides ActivityPub resource endpoints for users and notes.
package ap

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/api/notehide"
	"github.com/shiroha-a/mk/internal/api/userrelation"
	corenote "github.com/shiroha-a/mk/internal/core/note"
	coreuser "github.com/shiroha-a/mk/internal/core/user"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"
)

// RemoteFetcher fetches remote ActivityPub objects.
type RemoteFetcher interface {
	FetchObject(uri string) ([]byte, error)
}

// RemoteResolver resolves remote actors and notes.
type RemoteResolver interface {
	ResolveActor(uri string) (*model.User, error)
	ResolveNote(uri string) (*model.Note, error)
	// *AllowCrossHost variants relax the request-url ↔ id host binding for
	// user-initiated /api/ap/show lookups (upstream CrossOrigin softfail、#1828)。
	// federation-loop の解決は Strict な ResolveActor / ResolveNote を使う。
	ResolveActorAllowCrossHost(uri string) (*model.User, error)
	ResolveNoteAllowCrossHost(uri string) (*model.Note, error)
}

// HostBlockChecker reports whether a remote host is blocked / federation-allowed
// under the running instance's federation policy (none / specified / blockedHosts)。
// APIShow uses it to reject lookups of non-federated hosts (#1557、upstream
// UtilityService.isFederationAllowedUri)。
type HostBlockChecker interface {
	IsBlocked(host string) bool
	IsAllowed(host string) bool
	// FederationDisabled reports whether the instance federation mode is
	// "none" (fully isolated)。true のとき AP serve handler は 403 を返す
	// (upstream ActivityPubServerService の federation==='none' gate、#1879)。
	FederationDisabled() bool
}

// Handler handles ActivityPub resource endpoints.
type Handler struct {
	renderer         *activitypub.Renderer
	userService      *coreuser.Service
	queryService     *corenote.QueryService
	keypairRepo      repository.UserKeypairRepository
	keypairExtraRepo repository.UserKeypairExtraRepository
	idGen            id.Generator
	remoteFetcher    RemoteFetcher
	remoteResolver   RemoteResolver
	nonAPFallback    echo.HandlerFunc
	// hostBlocker / localHost は APIShow の federation-allow gate (#1557) に使う。
	// localHost は自インスタンスの hostname (gate を skip する判定用)。未配線
	// (nil) なら gate 無効 = 全 host 許可 (legacy 挙動)。
	hostBlocker HostBlockChecker
	localHost   string
	// backfillGroup collapses parallel lazy-backfill 経路の Generate +
	// InsertIfAbsent を同 user ID で 1 回に集約する。多 goroutine が
	// 同時に backfill しても entropy / CPU を浪費しない (#1081 review #1)。
	backfillGroup singleflight.Group
	// relation は ap/show が返す UserDetailedNotMe に viewer→target の relation
	// block (isFollowing 等) を埋めるための repo 束 (#1778)。未配線なら relation は
	// omit (= legacy 挙動)。
	relation userrelation.Repos
	// followingRepo は followers/following collection endpoint (#1877) の page
	// 取得用。未配線なら両 endpoint は 404 (= legacy 挙動)。
	followingRepo repository.FollowingRepository
	// noteRepo は outbox collection endpoint (#1878) の public note 取得 +
	// pure renote の target URI 解決用。未配線なら outbox は 404。
	noteRepo   repository.NoteRepository
	piningRepo repository.UserNotePiningRepository
	// quoteAuthorizations は引用の承認の実体を配る (FEP-044f、#3234)。
	quoteAuthorizations QuoteAuthorizationStore
}

// SetFollowingRepo wires the repository used by the followers/following AP
// collection endpoints (#1877)。
func (h *Handler) SetFollowingRepo(r repository.FollowingRepository) {
	h.followingRepo = r
}

// SetNoteRepo wires the repository used by the outbox AP collection endpoint
// (#1878)。
func (h *Handler) SetNoteRepo(r repository.NoteRepository) {
	h.noteRepo = r
}

// SetPiningRepo wires current profile-pin lookups for AP publication policy.
func (h *Handler) SetPiningRepo(r repository.UserNotePiningRepository) {
	h.piningRepo = r
}

// SetRelationRepos wires the repositories used to populate the viewer relation
// block on ap/show user responses (#1778)。
func (h *Handler) SetRelationRepos(r userrelation.Repos) {
	h.relation = r
}

// SetRemote attaches remote AP fetcher and resolver.
func (h *Handler) SetRemote(fetcher RemoteFetcher, resolver RemoteResolver) {
	h.remoteFetcher = fetcher
	h.remoteResolver = resolver
}

// SetFederationGate wires the federation-policy checker and the local hostname
// used by APIShow to reject lookups of blocked / non-federated hosts (#1557)。
func (h *Handler) SetFederationGate(c HostBlockChecker, localHost string) {
	h.hostBlocker = c
	h.localHost = localHost
}

// federationDisabled reports whether the instance is fully isolated
// (federation mode "none")。AP serve handler はこのとき 403 を返す
// (#1879)。hostBlocker 未配線時は false (= serve、legacy 挙動)。
func (h *Handler) federationDisabled() bool {
	return h.hostBlocker != nil && h.hostBlocker.FederationDisabled()
}

// SetNonAPFallback registers a handler to serve when the incoming Accept
// header does not request an ActivityPub document. Typically the SPA HTML
// frontend is registered here so that browser reloads on actor/note URLs
// render the frontend instead of returning a bare 404 JSON. Echo does not
// re-dispatch errored handlers to the catch-all route, so this delegation
// has to happen inside the AP handler itself.
func (h *Handler) SetNonAPFallback(fn echo.HandlerFunc) {
	h.nonAPFallback = fn
}

// serveNonAP is called from AP resource handlers when the client did not
// request application/activity+json. When a fallback is wired (runtime)
// it renders the frontend HTML; otherwise (tests) it returns ErrNotFound
// so the behavior degrades gracefully when the handler is used in
// isolation.
func (h *Handler) serveNonAP(c echo.Context) error {
	if h.nonAPFallback != nil {
		return h.nonAPFallback(c)
	}
	return echo.ErrNotFound
}

// NewHandler constructs a Handler.
func NewHandler(
	renderer *activitypub.Renderer,
	userService *coreuser.Service,
	queryService *corenote.QueryService,
	keypairRepo repository.UserKeypairRepository,
	idGen id.Generator,
) *Handler {
	return &Handler{
		renderer:     renderer,
		userService:  userService,
		queryService: queryService,
		keypairRepo:  keypairRepo,
		idGen:        idGen,
	}
}

// SetKeypairExtraRepo wires the Ed25519 keypair repository so that actor JSON
// responses can include `assertionMethod[]` Multikey entries for users that
// own an Ed25519 keypair (#1067 / #1069). Optional: 未配線でも RSA only で
// 動き、upstream Misskey TS と同一の actor JSON shape を維持する。
func (h *Handler) SetKeypairExtraRepo(r repository.UserKeypairExtraRepository) {
	h.keypairExtraRepo = r
}

// lookupEd25519PublicKey returns the Ed25519 public key for the local user
// when keypairExtraRepo is wired. P1 マイグレーション前から存在する旧 user
// には user_keypair_extra に行が無いため、ErrRecordNotFound のときに lazy
// backfill (鍵生成 + InsertIfAbsent + 再 lookup) を行って actor JSON に
// assertionMethod を expose する (#1072)。生成失敗 / PEM parse 失敗のいずれも
// nil を返して RSA only にフォールバックする。
//
// 並列 actor JSON 生成で同 user に対して複数 goroutine が backfill を
// 開始しても、InsertIfAbsent (ON CONFLICT DO NOTHING) で 最初に書かれた
// 行が残り、後続は再 lookup で既存行を取得する → race-safe。
//
// DB error (= ErrRecordNotFound 以外) と PEM parse error は診断のため
// slog.Warn を出す。
func (h *Handler) lookupEd25519PublicKey(userID string) ed25519.PublicKey {
	if h.keypairExtraRepo == nil {
		return nil
	}
	row, err := h.keypairExtraRepo.FindByUserID(userID)
	if err == nil {
		return h.parseEd25519PublicKeyOrLog(userID, row.Ed25519PublicKey)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		slog.Warn("ed25519 keypair lookup failed",
			"userID", userID, "error", err)
		return nil
	}
	// 行なし → P5 lazy backfill 経路
	return h.backfillEd25519PublicKey(userID)
}

// backfillEd25519PublicKey generates a fresh Ed25519 keypair for a local user
// that pre-dates the P1 migration, persists it via InsertIfAbsent (race-safe),
// and returns the public key for the renderer. Any failure path returns nil
// → RSA only fallback。
//
// 同 userID への並行 backfill は singleflight で 1 回に集約され、Generate +
// InsertIfAbsent + 再 lookup を多 goroutine が個別に走らせる無駄を抑える
// (#1081 review #1)。InsertIfAbsent の戻り値 inserted で「自分が書いた」
// 「他が先に書いた」を区別し、inserted=true なら自身が生成した PEM を
// そのまま返して再 FindByUserID を skip (= 1 query 削減 #1081 review #2)。
func (h *Handler) backfillEd25519PublicKey(userID string) ed25519.PublicKey {
	val, _, _ := h.backfillGroup.Do(userID, func() (any, error) {
		// 失敗時に typed-nil の ed25519.PublicKey を返すと caller 側で
		// `val == nil` が false になる (interface comparison の罠)。明示的に
		// untyped nil を return して check が正しく short-circuit するように
		// する。
		if pub := h.runEd25519Backfill(userID); pub != nil {
			return pub, nil
		}
		return nil, nil
	})
	if val == nil {
		return nil
	}
	return val.(ed25519.PublicKey)
}

func (h *Handler) runEd25519Backfill(userID string) ed25519.PublicKey {
	privPEM, pubPEM, err := activitypub.GenerateEd25519Keypair()
	if err != nil {
		slog.Warn("ed25519 backfill keypair generate failed",
			"userID", userID, "error", err)
		return nil
	}
	inserted, err := h.keypairExtraRepo.InsertIfAbsent(&model.UserKeypairExtra{
		UserID:            userID,
		Ed25519PublicKey:  pubPEM,
		Ed25519PrivateKey: privPEM,
	})
	if err != nil {
		slog.Warn("ed25519 backfill insert failed",
			"userID", userID, "error", err)
		return nil
	}
	if inserted {
		// 自身が書いた行 → 生成済 PEM をそのまま返す (1 query 削減)
		return h.parseEd25519PublicKeyOrLog(userID, pubPEM)
	}
	// 並列 race (= singleflight 期間外の重複 backfill) で別 goroutine が
	// 先に書いた → 再 lookup で永続化された別公開鍵を取得
	row, err := h.keypairExtraRepo.FindByUserID(userID)
	if err != nil {
		slog.Warn("ed25519 backfill re-lookup failed",
			"userID", userID, "error", err)
		return nil
	}
	return h.parseEd25519PublicKeyOrLog(userID, row.Ed25519PublicKey)
}

// parseEd25519PublicKeyOrLog parses an Ed25519 PEM and logs on failure.
// Shared by lookup / backfill paths.
func (h *Handler) parseEd25519PublicKeyOrLog(userID, pemStr string) ed25519.PublicKey {
	pub, err := activitypub.ParseEd25519PublicKeyPEM(pemStr)
	if err != nil {
		slog.Warn("ed25519 PEM parse failed",
			"userID", userID, "error", err)
		return nil
	}
	return pub
}

// User handles GET /users/:id with ActivityPub content negotiation.
// AP clients (Accept: application/activity+json) receive the Person
// document; browser callers are 302-redirected to the canonical
// `/@<username>` permalink so the SPA's userPage route can resolve.
//
// Misskey TS の ClientServerService.ts と同じ挙動: 共有 frontend は
// `/users/:id` の SPA ルートを持たず `/@:acct` のみなので、QR code (frontend
// が自分の id で組み立てる) や AP federation 由来のリンクから来た訪問者を
// 確実に SPA で開けるよう backend で redirect する (#691)。リモート user
// の場合は SPA 側でも canonical な remote URI を開かせるため Host 付きで
// `/@username@host` 形式に redirect する。
func (h *Handler) User(c echo.Context) error {
	// Vary: Accept は AP / browser の両分岐を持つすべての response に必要。
	// content-negotiation 実行前に header を立てておけば、redirect / Person /
	// 404 のいずれが返っても intermediate cache が誤配信しない (#691 review)。
	c.Response().Header().Set("Vary", "Accept")

	id := c.Param("id")
	bundle, err := h.userService.ShowByID(id)
	if err != nil {
		// 既存仕様: AP client は 404、browser も 404 (ID 不正)
		return c.NoContent(http.StatusNotFound)
	}

	if !wantsActivityJSON(c.Request().Header.Get("Accept")) {
		// suspended local user は upstream Misskey TS と同じく 404。SPA の
		// `/@<username>` route に流すと「アカウントが見つかりません」UI が
		// 出るが、upstream 仕様 (`isSuspended: false` filter) と合わせて
		// backend 側で明示的に止める (#691 review)。
		if (bundle.User.Host == nil || *bundle.User.Host == "") && bundle.User.IsSuspended {
			return c.NoContent(http.StatusNotFound)
		}
		// browser は username 付き permalink に redirect。
		acct := "/@" + bundle.User.Username
		if bundle.User.Host != nil && *bundle.User.Host != "" {
			acct += "@" + *bundle.User.Host
		}
		return c.Redirect(http.StatusFound, acct)
	}

	// federation 無効 (mode=none) なら actor を AP serve しない (#1879)。上の
	// browser (非AP Accept) 経路は redirect 済みなので、ローカル web UI は影響しない。
	if h.federationDisabled() {
		return c.NoContent(http.StatusForbidden)
	}

	return h.apUserInfo(c, bundle)
}

// apUserInfo serves the AP branch shared by User (/users/:id) and UserByAcct
// (/@:acct)。upstream ActivityPubServerService.userInfo に相当する。
func (h *Handler) apUserInfo(c echo.Context, bundle *coreuser.UserWithProfile) error {
	// suspended は upstream の route query (isSuspended: false) 相当で、
	// ローカル・リモートを問わず 404 (Person も redirect も返さない)。
	if bundle.User.IsSuspended {
		return c.NoContent(http.StatusNotFound)
	}
	// リモート actor は原本 URI へリダイレクトする。無いと他サーバーがこの
	// インスタンスの URL 経由で第三サーバーの actor を解決できない (#2506、
	// note の #2505 と同じ故障モード)。**note は 302 だが user は 301**
	// (upstream の使い分けに合わせる)。
	if bundle.User.Host != nil {
		// uri が無い・host が自ホストを指すのはデータ異常。upstream と同じく
		// 500 にする (自ホスト照合の割り切りは Note と同じ)。
		if bundle.User.URI == nil || *bundle.User.URI == "" ||
			(h.localHost != "" && strings.EqualFold(*bundle.User.Host, h.localHost)) {
			return c.NoContent(http.StatusInternalServerError)
		}
		return c.Redirect(http.StatusMovedPermanently, *bundle.User.URI)
	}
	keypair, err := h.keypairRepo.FindByUserID(bundle.User.ID)
	if err != nil {
		return c.NoContent(http.StatusInternalServerError)
	}
	// upstream と同じキャッシュ指示 (public, max-age=180)。
	c.Response().Header().Set("Cache-Control", "public, max-age=180")
	person := h.renderer.RenderPerson(bundle.User, bundle.Profile, keypair.PublicKey, h.lookupEd25519PublicKey(bundle.User.ID))
	return writeActivityJSON(c, person)
}

// UserByAcct handles GET /@:acct with ActivityPub content negotiation.
// When the Accept header prefers application/activity+json (which is how
// other AP implementations resolve actors that they discovered via
// WebFinger or a raw link), return the Person document. Otherwise
// delegate to the non-AP fallback so browser reloads render the SPA
// frontend instead of a bare 404.
func (h *Handler) UserByAcct(c echo.Context) error {
	// Vary: Accept は AP / browser の両分岐を持つすべての response に必要
	// (User と同じ規約。upstream も /@:acct の先頭で立てる)。
	c.Response().Header().Set("Vary", "Accept")
	if !wantsActivityJSON(c.Request().Header.Get("Accept")) {
		return h.serveNonAP(c)
	}
	// federation 無効 (mode=none) なら actor を AP serve しない (#1879)。/@acct も
	// /users/:id と同じく upstream で gate されるため、こちらにも入れる。browser 経路は
	// 上で serveNonAP に流れるので影響しない。
	if h.federationDisabled() {
		return c.NoContent(http.StatusForbidden)
	}
	// /@alice or /@alice@host 形式。**host 部を捨てない** (#2506) — upstream は
	// acct の host でリモート actor を照合し、userInfo が原本へ 301 する。
	// 捨てると /@alice@remote.example がローカルの alice の Person を返す
	// 取り違えになる (要求した URL と id の一致しない document を配る)。
	//
	// 解析は upstream の Acct.parse に合わせる: 先頭の @ を 1 枚剥がし、
	// 2 分割 (2 個目以降の @ は捨てる)。
	acct := strings.TrimPrefix(c.Param("acct"), "@")
	username := acct
	var host *string
	if idx := strings.Index(acct, "@"); idx >= 0 {
		username = acct[:idx]
		rest := acct[idx+1:]
		if j := strings.Index(rest, "@"); j >= 0 {
			rest = rest[:j]
		}
		// 自ホストはローカル扱いに正規化する (upstream の isSelfHost 相当)。
		if rest != "" && !(h.localHost != "" && strings.EqualFold(rest, h.localHost)) {
			host = &rest
		}
	}
	// **DB 照合のみ** (upstream と同じ)。ShowByUsername の remote fallback を
	// 使うと、認証不要の GET が WebFinger + actor fetch の outbound を外部から
	// 強制できる増幅面になる。
	bundle, err := h.userService.ShowByUsernameDB(username, host)
	if err != nil {
		return c.NoContent(http.StatusNotFound)
	}
	return h.apUserInfo(c, bundle)
}

// writeActivityJSON serializes v and writes it with the ActivityPub
// content type. Remote implementations (Misskey, Mastodon, ...) check
// Content-Type before treating a response as an AP document, so a plain
// application/json would cause them to reject it.
func writeActivityJSON(c echo.Context, v any) error {
	return c.Blob(http.StatusOK, `application/activity+json; charset=utf-8`, mustMarshal(v))
}

// wantsActivityJSON reports whether the caller prefers an AP document.
// Any occurrence of application/activity+json or application/ld+json in
// the Accept header is treated as a positive signal.
func wantsActivityJSON(accept string) bool {
	return strings.Contains(accept, "application/activity+json") ||
		strings.Contains(accept, "application/ld+json")
}

// APIGet handles POST /api/ap/get — Admin専用。URIからActivityPubオブジェクトを取得。
func (h *Handler) APIGet(c echo.Context) error {
	var req struct {
		URI string `json:"uri"`
	}
	if err := c.Bind(&req); err != nil || req.URI == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "uri is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}

	// ローカルURIからオブジェクトを解決
	obj, err := h.resolveLocal(req.URI)
	if err == nil {
		return c.JSON(http.StatusOK, obj)
	}

	// リモートフェッチ
	if h.remoteFetcher != nil {
		data, err := h.remoteFetcher.FetchObject(req.URI)
		if err == nil {
			var parsed map[string]any
			if json.Unmarshal(data, &parsed) == nil {
				return c.JSON(http.StatusOK, parsed)
			}
		}
	}

	return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_OBJECT", "No such object.", "dc94d745-1262-4e63-a17d-fecaa57efc82"))
}

// APIShow handles POST /api/ap/show — URIからUser/Noteを解決して返す。
func (h *Handler) APIShow(c echo.Context) error {
	var req struct {
		URI string `json:"uri"`
	}
	if err := c.Bind(&req); err != nil || req.URI == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "uri is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}

	// URI を解析して host を得る。http(s) でない / host 無しは URI_INVALID
	// (upstream uriInvalid 1a5eab56)。
	host, hostErr := apShowURIHost(req.URI)
	if hostErr != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("URI_INVALID", "URI is invalid.", "1a5eab56-e47b-48c2-8d5e-217b897d70db"))
	}
	// federation-allow gate (upstream isFederationAllowedUri、#1557)。自 host は
	// skip し、remote host が blocked / 非 federation なら FEDERATION_NOT_ALLOWED
	// (974b799e) を返す。gate 未配線なら全許可 (legacy)。
	if host != "" && host != h.localHost && h.hostBlocker != nil {
		if h.hostBlocker.IsBlocked(host) || !h.hostBlocker.IsAllowed(host) {
			return c.JSON(http.StatusBadRequest, apierr.Error("FEDERATION_NOT_ALLOWED", "Federation for this host is not allowed.", "974b799e-1a29-4889-b706-18d4dd93e266"))
		}
	}

	// ローカルのノートURIかチェック (/notes/ を含む)
	if noteID := extractLocalID(req.URI, "/notes/"); noteID != "" {
		n, err := h.queryService.Show(nil, noteID)
		if err == nil && n.UserHost == nil {
			return c.JSON(http.StatusOK, map[string]any{
				"type":   "Note",
				"object": h.packNoteForAPI(middleware.GetUser(c), n),
			})
		}
	}

	// ローカルのユーザーURIかチェック (/users/ を含む)
	if userID := extractLocalID(req.URI, "/users/"); userID != "" {
		bundle, err := h.userService.ShowByID(userID)
		if err == nil && bundle.User.Host == nil {
			return c.JSON(http.StatusOK, map[string]any{
				"type":   "User",
				"object": h.packUserForAPI(middleware.GetUser(c), bundle.User, bundle.Profile),
			})
		}
	}

	// リモートオブジェクトをフェッチしてType判定
	// Note の場合は ResolveNote でローカルDBに取り込み、local ID を返す。
	// これにより後続の notes/reactions/create などが local note を見つけられる。
	//
	// fetchRequestFailed: fetch が 404/410 (= 不在) 以外の理由 (5xx / network /
	// timeout) で失敗したときに立てる。webfinger fallback も失敗した場合に
	// NO_SUCH_OBJECT ではなく REQUEST_FAILED (81b539cf) を返すための記録。
	fetchRequestFailed := false
	if h.remoteFetcher != nil {
		data, fetchErr := h.remoteFetcher.FetchObject(req.URI)
		switch {
		case fetchErr != nil:
			// 404 / 410 は「不在」なので NO_SUCH_OBJECT 経路へ。それ以外は
			// REQUEST_FAILED 候補として記録 (webfinger fallback を先に試す)。
			var se *activitypub.StatusError
			if !(errors.As(fetchErr, &se) && (se.StatusCode == http.StatusNotFound || se.StatusCode == http.StatusGone)) {
				fetchRequestFailed = true
			}
		default:
			var parsed map[string]any
			if json.Unmarshal(data, &parsed) != nil {
				// fetch できたが JSON として不正 → RESPONSE_INVALID (70193c39)。
				return c.JSON(http.StatusBadGateway, apierr.Error("RESPONSE_INVALID", "Response from remote server is invalid.", "70193c39-54f3-4813-82f0-70a680f7495b"))
			}
			// AP object は id 必須。欠落は upstream の `object.id == null` →
			// responseInvalid に対応する。
			if objID, _ := parsed["id"].(string); objID == "" {
				return c.JSON(http.StatusBadGateway, apierr.Error("RESPONSE_INVALID", "Response from remote server is invalid.", "70193c39-54f3-4813-82f0-70a680f7495b"))
			}
			// `type` は配列で来ることがある (JSON-LD の `@type` は配列表現が
			// 正規)。string 決め打ちだと Note / Article / Question の分岐を
			// 外れ、fallback は actor 解決しか試さないので **400
			// NO_SUCH_OBJECT** になる。upstream は `getApType` 経由なので
			// 先頭要素を採って解決する (#2662)。
			t := activitypub.TypeOf(parsed["type"])
			switch t {
			// upstream の `validPost` (type.ts) と同じ集合。Note 取り込み側は
			// type を見ないので、ここで落とすと PeerTube の `Video` や
			// Mobilizon の `Event` の URL を貼ったときだけ 400 になる
			// (inbox 経由の `Create(Video)` は既に通るので経路で結果が違う、
			// #2662)。
			case "Note", "Question", "Article", "Audio", "Document", "Image", "Page", "Video", "Event":
				if h.remoteResolver != nil {
					if remoteNote, err := h.remoteResolver.ResolveNoteAllowCrossHost(req.URI); err == nil {
						return c.JSON(http.StatusOK, map[string]any{
							"type":   "Note",
							"object": h.packNoteForAPI(middleware.GetUser(c), remoteNote),
						})
					}
				}
				// Resolver 無し or ResolveNote 失敗時は raw AP JSON を返す
				return c.JSON(http.StatusOK, map[string]any{
					"type":   "Note",
					"object": parsed,
				})
			case "Person", "Service", "Application", "Organization", "Group":
				if h.remoteResolver != nil {
					if remoteUser, err := h.remoteResolver.ResolveActorAllowCrossHost(req.URI); err == nil {
						return c.JSON(http.StatusOK, map[string]any{
							"type":   "User",
							"object": h.packUserForAPI(middleware.GetUser(c), remoteUser, h.userService.GetProfile(remoteUser.ID)),
						})
					}
				}
			}
		}
	}

	// フェッチ失敗 or Type 不明の場合は ResolveActor を試す (webfinger 経由の
	// /@user URL に対する fetch が失敗するケースがあるため)。
	if h.remoteResolver != nil {
		if remoteUser, err := h.remoteResolver.ResolveActorAllowCrossHost(req.URI); err == nil {
			return c.JSON(http.StatusOK, map[string]any{
				"type":   "User",
				"object": h.packUserForAPI(middleware.GetUser(c), remoteUser, h.userService.GetProfile(remoteUser.ID)),
			})
		}
	}

	// fetch が transport 失敗 (5xx / network) だった場合は REQUEST_FAILED、
	// それ以外 (404/410 / 純粋に不在) は NO_SUCH_OBJECT。
	if fetchRequestFailed {
		return c.JSON(http.StatusBadGateway, apierr.Error("REQUEST_FAILED", "Request failed.", "81b539cf-4f57-4b29-bc98-032c33c0792e"))
	}
	return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_OBJECT", "No such object.", "dc94d745-1262-4e63-a17d-fecaa57efc82"))
}

// apShowURIHost parses an ap/show URI and returns its hostname. Returns an
// error for non-http(s) schemes or a missing host so the caller can emit
// URI_INVALID (#1557)。
func apShowURIHost(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("ap: invalid uri")
	}
	return u.Hostname(), nil
}

// resolveLocal attempts to resolve a local URI to an AP object.
func (h *Handler) resolveLocal(uri string) (any, error) {
	if noteID := extractLocalID(uri, "/notes/"); noteID != "" {
		n, err := h.queryService.Show(nil, noteID)
		if err != nil {
			return nil, err
		}
		// remote note は local-domain の id/uri で render すると別オブジェクトに
		// なってしまうので local 専用 (#1873)。error 返しで caller は remote fetch
		// → NO_SUCH_OBJECT へフォールバックする。
		if n.UserHost != nil && *n.UserHost != "" {
			return nil, http.ErrNotSupported
		}
		return h.renderer.RenderNote(n, h.idGen), nil
	}
	if userID := extractLocalID(uri, "/users/"); userID != "" {
		bundle, err := h.userService.ShowByID(userID)
		if err != nil {
			return nil, err
		}
		// remote user (local aidx id を持つ) を local-domain actor として render
		// すると id/url/featured が誤った自ドメイン URI になるので local 専用
		// (#1873。連合向けの User/Followers/Following/Outbox/Featured handler は
		// 既に Host guard 済みだが、この admin ap/get 経路だけ漏れていた)。
		if !bundle.User.IsLocal() {
			return nil, http.ErrNotSupported
		}
		keypair, err := h.keypairRepo.FindByUserID(bundle.User.ID)
		if err != nil {
			return nil, err
		}
		return h.renderer.RenderPerson(bundle.User, bundle.Profile, keypair.PublicKey, h.lookupEd25519PublicKey(bundle.User.ID)), nil
	}
	return nil, http.ErrNotSupported
}

func extractLocalID(uri, pathPrefix string) string {
	// URI末尾の /notes/{id} や /users/{id} からIDを抽出
	idx := len(uri) - 1
	for idx >= 0 && uri[idx] != '/' {
		idx--
	}
	if idx < 0 {
		return ""
	}
	// pathPrefixが含まれているか確認
	prefixIdx := -1
	for i := 0; i+len(pathPrefix) <= len(uri); i++ {
		if uri[i:i+len(pathPrefix)] == pathPrefix {
			prefixIdx = i
			break
		}
	}
	if prefixIdx < 0 {
		return ""
	}
	id := uri[prefixIdx+len(pathPrefix):]
	// フラグメント (#main-key 等) を除去
	if i := strings.IndexByte(id, '#'); i >= 0 {
		id = id[:i]
	}
	return id
}

// packNoteForAPI packs a note with the full Note entity schema, matching
// upstream ap/show which returns noteEntityService.pack(note, me, {detail:true})
// for the {type:'Note', object} branch (#1557). Previously this returned an
// ad-hoc minimal object (id/text/userId/visibility + 4-field user).
func (h *Handler) packNoteForAPI(viewer *model.User, n *model.Note) entity.NoteEntity {
	// embedded renote/reply を viewer 可視性で hide する (#1536 / #1557)。本家
	// noteEntityService.pack(note, me) は me 視点で embed を hideNote する。これを
	// 欠くと followers/specified note を引用/返信した note 経由で非可視本文が ap/show
	// から漏れる IDOR になる。ap/show は RequireAuth なので viewer は通常非 nil だが、
	// nil でも notehide が fail-closed (followers/specified embed を hide) する。
	arr := []entity.NoteEntity{entity.PackNote(n, h.idGen)}
	notehide.HideEmbeds(viewer, arr)
	return arr[0]
}

// packUserForAPI packs a user with the UserDetailedNotMe schema, matching
// upstream ap/show which returns userEntityService.pack(user, me,
// {schema:'UserDetailedNotMe'}) for the {type:'User', object} branch (#1557).
// Previously this returned an ad-hoc minimal object (id/username/name/host).
func (h *Handler) packUserForAPI(viewer *model.User, u *model.User, profile *model.UserProfile) any {
	if u == nil {
		return map[string]any{}
	}
	d := entity.PackUserDetailed(u, profile, h.idGen)
	// upstream は pack(user, me, {schema:'UserDetailedNotMe'}) で me!=null のとき
	// relation block (isFollowing/isBlocking 等) を埋める。authed viewer に同じ
	// relation を載せる (#1778)。anonymous / self は Apply 内で no-op。
	if viewer != nil {
		h.relation.Apply(&d, viewer.ID, u, profile)
	}
	return d
}

// Note handles GET /notes/:id with ActivityPub content negotiation.
// AP clients receive the AS Note object; browser reloads are handed
// off to the frontend fallback so the SPA renders the note permalink.
func (h *Handler) Note(c echo.Context) error {
	// Vary: Accept は AP / browser の両分岐を持つすべての response に必要
	// (upstream も /notes/:note の先頭で立てる)。302 が加わったことで、これが
	// 無いと中間キャッシュが AP 向け 302 をブラウザに配る混線が現実になる。
	c.Response().Header().Set("Vary", "Accept")
	if !wantsActivityJSON(c.Request().Header.Get("Accept")) {
		return h.serveNonAP(c)
	}
	// federation 無効 (mode=none) なら AP serve しない (#1879)。browser (非AP) 経路は
	// 上で serveNonAP に流れるので、ローカル web UI は影響しない。
	if h.federationDisabled() {
		return c.NoContent(http.StatusForbidden)
	}
	noteID := c.Param("id")
	// 公開ノートのみAPでフェッチ可能 (非ログインから取得されるため viewer=nil)。
	// CanSeeNote(nil) は public / home を通すので、upstream の
	// visibility ∈ {public, home} フィルタと一致する。
	n, err := h.queryService.ShowCurrent(nil, noteID)
	if err != nil {
		return c.NoContent(http.StatusNotFound)
	}
	// **localOnly は AP で serve しない** (upstream は query で localOnly: false
	// を強制)。CanSeeNote は可視性しか見ず、連合可否である localOnly を
	// 通してしまうため、ここで明示的に弾く。
	if n.LocalOnly {
		return c.NoContent(http.StatusNotFound)
	}
	// リモートノートは原本 URI へリダイレクトする (upstream
	// ActivityPubServerService の /notes/:note と同じ)。これが無いと、他サーバー
	// がこのインスタンスの URL 経由で第三サーバーの投稿を照会したとき、
	// リダイレクトを辿って権威サーバーから取得する経路が 404 で途切れる (#2505)。
	if n.UserHost != nil {
		// uri が無い・userHost が自ホストを指す、はデータ異常。upstream と
		// 同じく 500 にする (404 だと「無い」と区別できず調査の足掛かりを失う)。
		// 空文字列も弾くのは upstream との意図的な差 (upstream は null しか見ず
		// Location が空になる)。
		//
		// 自ホスト照合は素の比較 (EqualFold)。upstream の isSelfHost は puny
		// 正規化 + port 込みだが、この分岐はデータ異常の検出であって可視性の
		// 境界ではないので、既存の federation gate (h.localHost の比較) と
		// 同じ割り切りに揃える。
		if n.URI == nil || *n.URI == "" ||
			(h.localHost != "" && strings.EqualFold(*n.UserHost, h.localHost)) {
			return c.NoContent(http.StatusInternalServerError)
		}
		// fastify の reply.redirect 既定と同じ 302。
		return c.Redirect(http.StatusFound, *n.URI)
	}
	// upstream と同じキャッシュ指示 (public, max-age=180)。Vary: Accept を
	// 立てているので AP 変種としてキャッシュされる。
	if !h.anonymousNotePublicationAllowed(n, h.noteIsCurrentlyPinned(n)) {
		return c.NoContent(http.StatusNotFound)
	}
	c.Response().Header().Set("Cache-Control", "public, max-age=180")
	note := h.renderer.RenderNote(n, h.idGen)
	return writeActivityJSON(c, note)
}

// Featured handles GET /users/:id/collections/featured, serving the local
// user's pinned notes as an OrderedCollection (upstream
// ActivityPubServerService.featured, #1876)。actor が advertise する featured URI に
// 対応する。pinned note は inline 出力 (pagination 無し、upstream と同じ)。
//
// upstream と同じく !localOnly かつ visibility ∈ {public, home} の note のみ serve し、
// followers/specified/localOnly な pinned note を unauthenticated AP へ leak させない。
func (h *Handler) Featured(c echo.Context) error {
	c.Response().Header().Set("Vary", "Accept")
	if h.federationDisabled() {
		return c.NoContent(http.StatusForbidden)
	}
	id := c.Param("id")
	bundle, err := h.userService.ShowByID(id)
	if err != nil {
		return c.NoContent(http.StatusNotFound)
	}
	// local user 専用 (remote actor の featured collection は serve しない)。
	if !bundle.User.IsLocal() {
		return c.NoContent(http.StatusNotFound)
	}
	notes, err := h.userService.ListPinnedNotes(id)
	if err != nil {
		return c.NoContent(http.StatusInternalServerError)
	}
	items := make([]any, 0, len(notes))
	for _, n := range notes {
		// upstream featured の filter: !localOnly && visibility ∈ {public, home}。
		if n.LocalOnly || (n.Visibility != model.NoteVisibilityPublic && n.Visibility != model.NoteVisibilityHome) {
			continue
		}
		if !h.anonymousNotePublicationAllowed(n, true) {
			continue
		}
		rn := h.renderer.RenderNote(n, h.idGen)
		// collection 直下に embed するので per-note @context は外す
		// (upstream renderNote は bare object を返し、@context は collection 側のみ)。
		rn.Context = nil
		items = append(items, rn)
	}
	// totalItems は filter 後の件数 (upstream は renderedNotes.length)。
	col := h.renderer.RenderOrderedCollection(h.renderer.URLs().UserFeatured(id), len(items), "", "", items)
	activitypub.AddContext(col)
	return writeActivityJSON(c, col)
}

// Followers handles GET /users/:id/followers (#1877)。
func (h *Handler) noteIsCurrentlyPinned(n *model.Note) bool {
	if h.piningRepo == nil || n == nil {
		return false
	}
	p, err := h.piningRepo.FindByPair(n.UserID, n.ID)
	return err == nil && p != nil
}

func (h *Handler) currentPinnedNoteIDs(userID string) map[string]struct{} {
	ids := make(map[string]struct{})
	if h.piningRepo == nil {
		return ids
	}
	rows, err := h.piningRepo.ListByUser(userID)
	if err != nil {
		return ids
	}
	for _, row := range rows {
		ids[row.NoteID] = struct{}{}
	}
	return ids
}

func (h *Handler) anonymousNotePublicationAllowed(n *model.Note, pinned bool) bool {
	return corenote.AnonymousPublicationAllowed(corenote.FactsFromModel(n, h.noteCreatedAtMs(n)), pinned, time.Now().UnixMilli())
}

func (h *Handler) noteCreatedAtMs(n *model.Note) int64 {
	if n == nil || h.idGen == nil {
		return 0
	}
	t, err := h.idGen.ParseTime(n.ID)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

func (h *Handler) Followers(c echo.Context) error {
	return h.serveFollowCollection(c, true)
}

// Following handles GET /users/:id/following (#1877)。
func (h *Handler) Following(c echo.Context) error {
	return h.serveFollowCollection(c, false)
}

// serveFollowCollection serves the followers / following OrderedCollection
// for a local user (upstream ActivityPubServerService.followers/following, #1877)。
// followers=true なら followers、false なら following。
//
// privacy: profile の followers/followingVisibility が public 以外なら 403
// (unauthenticated AP request では公開リストのみ serve、upstream と同じ)。
// base (page!=true) は totalItems + first link、?page=true は cursor ページングで
// 相手側の actor URI を返す。
func (h *Handler) serveFollowCollection(c echo.Context, followers bool) error {
	c.Response().Header().Set("Vary", "Accept")
	if h.federationDisabled() {
		return c.NoContent(http.StatusForbidden)
	}
	if h.followingRepo == nil {
		return c.NoContent(http.StatusNotFound)
	}
	userID := c.Param("id")
	bundle, err := h.userService.ShowByID(userID)
	if err != nil {
		return c.NoContent(http.StatusNotFound)
	}
	if !bundle.User.IsLocal() {
		return c.NoContent(http.StatusNotFound)
	}

	// privacy gate: public 以外 (followers / private) は unauthenticated には出さない。
	// profile を引けない (transient DB error 等で ShowByID が Profile=nil を返す) 場合は
	// fail-closed で 403 にする。public default にすると private 設定の follower list を
	// 一時的に leak し、しかも max-age=180 で 3 分 cache されてしまう (#1877 review、
	// upstream は findOneByOrFail で 500 = fail-closed)。
	if bundle.Profile == nil {
		c.Response().Header().Set("Cache-Control", "public, max-age=30")
		return c.NoContent(http.StatusForbidden)
	}
	vis := bundle.Profile.FollowersVisibility
	if !followers {
		vis = bundle.Profile.FollowingVisibility
	}
	if vis != model.FollowingVisibilityPublic {
		c.Response().Header().Set("Cache-Control", "public, max-age=30")
		return c.NoContent(http.StatusForbidden)
	}

	urls := h.renderer.URLs()
	partOf := urls.UserFollowers(userID)
	totalItems := bundle.User.FollowersCount
	if !followers {
		partOf = urls.UserFollowing(userID)
		totalItems = bundle.User.FollowingCount
	}

	// index page: totalItems + first link (upstream renderOrderedCollection)。
	if c.QueryParam("page") != "true" {
		col := h.renderer.RenderOrderedCollection(partOf, totalItems, partOf+"?page=true", "", nil)
		activitypub.AddContext(col)
		c.Response().Header().Set("Cache-Control", "public, max-age=180")
		return writeActivityJSON(c, col)
	}

	// paginated page: cursor (= Following row id) で id DESC ページング。
	const limit = 10
	// **カーソルは列に入る値だけ通す (#3025 と同じ判断)。** この経路は未認証で
	// 叩けるので、`?cursor=%00` や `?cursor=%80` をそのまま `id < ?` へ載せると
	// PostgreSQL が SQLSTATE 22021 でクエリごと落とし、**誰でも 500 とエラー
	// ログを任意に生成できる**。JSON body と違いクエリパラメータは
	// percent-decode した生のバイト列がそのまま届く。
	//
	// cursor は untilId 相当 (`id < ?`) なので `NormalizeCursor` の untilID 側に
	// 渡す。**空に倒して 200 を返さない** — 空文字は「カーソル無し = 先頭から」の
	// 意味なので、利用者が指定した位置と無関係なページを正しい応答として返して
	// しまう。
	_, cursor, cursorOK := id.NormalizeCursor("", c.QueryParam("cursor"), nil, nil)
	if !cursorOK {
		return c.NoContent(http.StatusBadRequest)
	}
	var rows []*model.Following
	if followers {
		rows, err = h.followingRepo.ListFollowersBefore(userID, cursor, limit+1)
	} else {
		rows, err = h.followingRepo.ListFollowingBefore(userID, cursor, limit+1)
	}
	if err != nil {
		return c.NoContent(http.StatusInternalServerError)
	}
	inStock := len(rows) == limit+1
	if inStock {
		rows = rows[:limit]
	}

	counterpartIDs := make([]string, 0, len(rows))
	for _, f := range rows {
		if followers {
			counterpartIDs = append(counterpartIDs, f.FollowerID)
		} else {
			counterpartIDs = append(counterpartIDs, f.FolloweeID)
		}
	}
	items := h.resolveActorURIs(counterpartIDs)

	// cursor は client 由来なので URL-encode して reflect する (upstream encodeURIComponent
	// 相当)。aidx id は URL-safe だが、不正な cursor で id field が壊れないようにする。
	pageID := partOf + "?page=true"
	if cursor != "" {
		pageID += "&cursor=" + url.QueryEscape(cursor)
	}
	next := ""
	if inStock && len(rows) > 0 {
		next = partOf + "?page=true&cursor=" + url.QueryEscape(rows[len(rows)-1].ID)
	}
	page := h.renderer.RenderOrderedCollectionPage(pageID, totalItems, items, partOf, "", next)
	activitypub.AddContext(page)
	return writeActivityJSON(c, page)
}

// resolveActorURIs maps a list of user IDs to their actor URIs (local:
// /users/<id>, remote: user.uri), preserving order and dropping unresolved /
// URI-less users. upstream renderFollowUser → getUserUri 相当 (#1877)。
func (h *Handler) resolveActorURIs(ids []string) []any {
	if len(ids) == 0 {
		return []any{}
	}
	bundles, err := h.userService.ShowManyByIDs(ids)
	if err != nil {
		return []any{}
	}
	uriByID := make(map[string]string, len(bundles))
	for _, b := range bundles {
		uriByID[b.User.ID] = h.actorURI(b.User)
	}
	out := make([]any, 0, len(ids))
	for _, uid := range ids {
		if uri := uriByID[uid]; uri != "" {
			out = append(out, uri)
		}
	}
	return out
}

// actorURI returns the canonical actor URI for a user: local → /users/<id>,
// remote → stored uri. Empty for a remote user with no uri (skipped by caller)。
func (h *Handler) actorURI(u *model.User) string {
	if u.IsLocal() {
		return h.renderer.URLs().UserURI(u.ID)
	}
	if u.URI != nil {
		return *u.URI
	}
	return ""
}

// Outbox handles GET /users/:id/outbox, serving the local user's public/home
// (non-localOnly) notes as Create/Announce activities in an OrderedCollection
// (upstream ActivityPubServerService.outbox, #1878)。
func (h *Handler) Outbox(c echo.Context) error {
	c.Response().Header().Set("Vary", "Accept")
	if h.federationDisabled() {
		return c.NoContent(http.StatusForbidden)
	}
	if h.noteRepo == nil {
		return c.NoContent(http.StatusNotFound)
	}
	userID := c.Param("id")
	bundle, err := h.userService.ShowByID(userID)
	if err != nil {
		return c.NoContent(http.StatusNotFound)
	}
	if !bundle.User.IsLocal() {
		return c.NoContent(http.StatusNotFound)
	}

	partOf := h.renderer.URLs().UserOutbox(userID)
	totalItems := bundle.User.NotesCount

	// index page: totalItems + first link (upstream renderOrderedCollection)。
	// upstream は last link (since_id=<min>) も出すが ObjectId 前提の sentinel な
	// ので、aidx の mk-go では first + next 走査のみで完結させ last は省略する。
	if c.QueryParam("page") != "true" {
		col := h.renderer.RenderOrderedCollection(partOf, totalItems, partOf+"?page=true", "", nil)
		activitypub.AddContext(col)
		c.Response().Header().Set("Cache-Control", "public, max-age=180")
		return writeActivityJSON(c, col)
	}

	// **カーソルは列に入る値だけ通す (#3025 と同じ判断)。** outbox も未認証で
	// 叩けるので、`?until_id=%00` をそのまま `id < ?` へ載せると PostgreSQL が
	// クエリごと落として 500 になる。詳細は Followers 側のコメント。
	sinceID, untilID, cursorOK := id.NormalizeCursor(c.QueryParam("since_id"), c.QueryParam("until_id"), nil, nil)
	if !cursorOK {
		return c.NoContent(http.StatusBadRequest)
	}
	// upstream: since_id と until_id を同時指定したら 400。
	if sinceID != "" && untilID != "" {
		return c.NoContent(http.StatusBadRequest)
	}
	const limit = 20
	primary, ok := h.noteRepo.(repository.NotePrimaryReader)
	if !ok {
		return c.NoContent(http.StatusInternalServerError)
	}
	notes, err := primary.ListPublicByUserIDOnPrimary(userID, untilID, sinceID, limit)
	if err != nil {
		return c.NoContent(http.StatusInternalServerError)
	}
	// since_id 指定時は ASC で返るので、collection の新しい順 (DESC) に揃えるため反転。
	if sinceID != "" {
		for i, j := 0, len(notes)-1; i < j; i, j = i+1, j-1 {
			notes[i], notes[j] = notes[j], notes[i]
		}
	}

	items := make([]any, 0, len(notes))
	pinnedNoteIDs := h.currentPinnedNoteIDs(userID)
	for _, n := range notes {
		_, pinned := pinnedNoteIDs[n.ID]
		if !h.anonymousNotePublicationAllowed(n, pinned) {
			continue
		}
		items = append(items, h.packOutboxActivity(bundle.User, n))
	}

	pageID := partOf + "?page=true"
	if sinceID != "" {
		pageID += "&since_id=" + url.QueryEscape(sinceID)
	}
	if untilID != "" {
		pageID += "&until_id=" + url.QueryEscape(untilID)
	}
	prev, next := "", ""
	if len(notes) > 0 {
		prev = partOf + "?page=true&since_id=" + url.QueryEscape(notes[0].ID)
		next = partOf + "?page=true&until_id=" + url.QueryEscape(notes[len(notes)-1].ID)
	}
	page := h.renderer.RenderOrderedCollectionPage(pageID, totalItems, items, partOf, prev, next)
	activitypub.AddContext(page)
	return writeActivityJSON(c, page)
}

// NoteActivity handles GET /notes/:id/activity (upstream
// ActivityPubServerService の /notes/:note/activity、#2507)。
//
// renderer は Create / Announce の activity id として `<note URI>/activity` を
// **外向きに広告している** (RenderCreate / RenderAnnounce)。この route が無いと、
// 広告した id を dereference しに来た他サーバーが 404 を受け取る。
func (h *Handler) NoteActivity(c echo.Context) error {
	// upstream も vary(Accept) を立てる。この route は AP 専用で HTML 変種は
	// 無く、Accept で分岐しない点も upstream と同じ。
	c.Response().Header().Set("Vary", "Accept")
	if h.federationDisabled() {
		return c.NoContent(http.StatusForbidden)
	}
	n, err := h.queryService.ShowCurrent(nil, c.Param("id"))
	if err != nil {
		return c.NoContent(http.StatusNotFound)
	}
	// upstream は userHost: IsNull で**ローカル専用** (リモートノートの activity
	// はこのサーバーの発行物ではないので、redirect でなく 404)。visibility ∈
	// {public, home} は CanSeeNote(nil) が担保し、localOnly はここで弾く
	// (Note と同じ)。
	if n.UserHost != nil || n.LocalOnly {
		return c.NoContent(http.StatusNotFound)
	}
	if !h.anonymousNotePublicationAllowed(n, h.noteIsCurrentlyPinned(n)) {
		return c.NoContent(http.StatusNotFound)
	}
	author := n.User
	if author == nil {
		// preload が無い経路でも activity は author の ID しか使わない。
		author = &model.User{ID: n.UserID}
	}
	activity := h.packOutboxActivity(author, n)
	// 単体で返すので @context を付け直す (outbox 埋め込みでは外している)。
	activitypub.AddContext(activity)
	c.Response().Header().Set("Cache-Control", "public, max-age=180")
	return writeActivityJSON(c, activity)
}

// packOutboxActivity renders an outbox item: a pure renote becomes an Announce,
// everything else a Create (upstream packActivity, #1878)。embed するので
// per-activity @context は外す (collection 側が持つ)。
func (h *Handler) packOutboxActivity(author *model.User, n *model.Note) any {
	if corenote.IsPureRenote(n) && n.RenoteID != nil && *n.RenoteID != "" {
		if targetURI := h.renoteTargetURI(*n.RenoteID); targetURI != "" {
			ann := h.renderer.RenderAnnounce(author, n.ID, targetURI, n.Visibility, h.idGen)
			ann.Context = nil
			return ann
		}
		// target を解決できないときは Create にフォールバック (shape は valid のまま)。
	}
	create := h.renderer.RenderCreate(n, h.idGen)
	create.Context = nil
	return create
}

// renoteTargetURI resolves a renote target note id to its AP URI
// (local → /notes/<id>, remote → stored uri)。
func (h *Handler) renoteTargetURI(targetID string) string {
	// 未配線なら解決不能として扱う (呼び出し側が Create にフォールバックする)。
	// panic で 500 になるより shape の valid な Create を返す方が良い。
	if h.noteRepo == nil {
		return ""
	}
	t, err := h.noteRepo.FindByID(targetID)
	if err != nil || t == nil {
		return ""
	}
	if t.UserHost == nil || *t.UserHost == "" {
		return h.renderer.URLs().NoteURI(t.ID)
	}
	if t.URI != nil {
		return *t.URI
	}
	return ""
}

// HasHostBlockChecker reports whether the federation gate was wired.
//
// 未配線だと `FederationDisabled` が常に false になり `federation: none` でも
// Person を AP serve するうえ、`ap/show` の FEDERATION_NOT_ALLOWED gate ごと
// skip される。起動時検査に使う (#2708)。
func (h *Handler) HasHostBlockChecker() bool { return h.hostBlocker != nil }
