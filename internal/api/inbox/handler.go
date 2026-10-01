// Package inbox provides the ActivityPub inbox endpoint.
package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/core/federation"
	"github.com/shiroha-a/mk/internal/misc/colfit"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
)

// HostBlockChecker reports whether a host is on the blocked list and whether
// the federation mode allows ingesting from it. Used by the inbox handler to
// reject activities from blocked / disallowed instances (#536).
type HostBlockChecker interface {
	IsBlocked(host string) bool
	// IsAllowed returns true when federation with host is permitted under
	// the current admin federation mode (none / specified / all) and the
	// host is not in blockedHosts.
	IsAllowed(host string) bool
	// FederationDisabled reports whether the instance federation mode is
	// "none". upstream ActivityPubServerService.inbox は federation==='none' の
	// とき body を読む前に 403 を即返す (#1959)。per-actor の IsAllowed は actor が
	// local 扱い (host nil) のとき素通る余地があるため、最前段の mode gate を別途置く。
	FederationDisabled() bool
}

// InstanceTracker is invoked after a successfully verified inbound request so
// that the instance row's latestRequestReceivedAt can be updated. パッケージ
// 間の循環依存を避けるため interface で受け取る。
type InstanceTracker interface {
	MarkRequestReceived(host string) error
}

// SignatureCapabilityRecorder is invoked after a successfully verified inbound
// request so the remote host's signature capability can be recorded (#2393).
// パッケージ間の循環依存を避けるため interface で受け取る (実装は
// core/instance.CapabilityBuffer)。
type SignatureCapabilityRecorder interface {
	ObserveInboundAlg(host, alg string)
}

// ChartHook is invoked after a successfully verified inbound request
// so the chart subsystem can record ApRequest.Inbox / FederationChart.
// Inbox / InstanceChart.RequestReceived. パッケージ間の循環依存を
// 避けるため interface で受け取る (実装は core/chart/charthook)。
type ChartHook interface {
	OnInboxReceived(host string)
}

// InboxEnqueuer is the narrow subset of queue.Enqueuer the inbox handler
// uses to dispatch verified activities to the worker pool (#534). Falling
// back to nil disables async dispatch — the handler then runs Process in
// the request goroutine (legacy synchronous behaviour, used in tests that
// don't wire a queue client).
type InboxEnqueuer interface {
	EnqueueInbox(ctx context.Context, payload queue.InboxPayload) error
}

// Handler accepts incoming activities and dispatches them to the federation
// processor after verifying their HTTP signature.
type Handler struct {
	resolver        *federation.Resolver
	processor       *federation.Processor
	hostBlocker     HostBlockChecker
	instanceTracker InstanceTracker
	chartHook       ChartHook
	capabilities    SignatureCapabilityRecorder
	enqueuer        InboxEnqueuer
	// keyCache は同期 fallback 経路 (SetEnqueuer 未配線時) の HTTP Signature
	// verify で公開鍵パースをメモ化する (#1426)。worker 側 InboxProcessor と
	// 同じ最適化を、enqueue せず inline verify する構成にも適用する。
	keyCache *activitypub.PublicKeyCache
	// expectedHost は config 由来の自ホスト (host:port)。inbox admission の
	// host 署名必須 + 一致チェックに使う。未設定時は host チェックを skip する
	// (#1949)。
	expectedHost string
}

// NewHandler constructs a Handler.
func NewHandler(resolver *federation.Resolver, processor *federation.Processor) *Handler {
	return &Handler{resolver: resolver, processor: processor, keyCache: activitypub.NewPublicKeyCache(0)}
}

// SetEnqueuer wires the queue.Enqueuer used to dispatch verified activities
// to the worker pool. When unset, the handler falls back to running
// processor.Process synchronously inside the request goroutine — which is
// the pre-#534 behaviour and what unit tests without a queue rely on.
func (h *Handler) SetEnqueuer(e InboxEnqueuer) {
	h.enqueuer = e
}

// SetHostBlockChecker attaches a HostBlockChecker. 設定されると、シグネチャ
// 検証成功後の actor が属するホストが blocked リストに含まれる場合は 403 を
// 返して以降の処理をスキップする。
func (h *Handler) SetHostBlockChecker(c HostBlockChecker) {
	h.hostBlocker = c
}

// SetInstanceTracker attaches an InstanceTracker. 設定されると、署名検証に
// 成功するたびに対応 instance row の latestRequestReceivedAt が更新される。
func (h *Handler) SetInstanceTracker(t InstanceTracker) {
	h.instanceTracker = t
}

// SetExpectedHost wires the configured host (host:port) used by inbox admission
// to require the request Host header to be signed and to match this server.
// Mirrors upstream `request.headers.host !== this.config.host` (#1949).
func (h *Handler) SetExpectedHost(host string) {
	h.expectedHost = host
}

// SetChartHook attaches a ChartHook invoked after each successfully
// verified inbound request.
func (h *Handler) SetChartHook(c ChartHook) {
	h.chartHook = c
}

// SetSignatureCapabilityRecorder attaches the recorder that tracks which
// signature scheme each remote host actually uses (#2393).
func (h *Handler) SetSignatureCapabilityRecorder(r SignatureCapabilityRecorder) {
	h.capabilities = r
}

// signatureRelevantHeaders is the small set of HTTP headers worker-side
// verification needs to reconstruct the signed request. Capturing the
// full Header map would needlessly bloat the queue payload.
var signatureRelevantHeaders = []string{
	"Signature",
	"Date",
	// **`X-Date` も運ぶ (#3037 レビュー)。** `x-date` を署名対象に入れて
	// くる peer のリクエストは handler の admission を通るが、worker 側の
	// `buildSigningString` がこのヘッダを持たないと
	// `missing required header "x-date"` で落ち、**署名が正しいのに drop
	// される**。同じ PR で「署名している peer は今までどおり」と書いたのに、
	// 本番の非同期経路ではそうなっていなかった。
	"X-Date",
	"Host",
	"Digest",
	"Content-Type",
	"Content-Length",
	"Accept",
}

// captureSignatureHeaders extracts the headers needed to re-verify the
// HTTP signature in the inbox worker. Only the small allowlist above is
// included so the queue payload stays compact.
//
// Host は net/http で req.Host に格納される (Header map には入らない) た
// め、明示的に補完してから capture する。これが無いと worker 側 verify が
// `host: ""` で署名再構築して RSA verify が失敗する。
func captureSignatureHeaders(req *http.Request) map[string]string {
	out := make(map[string]string, len(signatureRelevantHeaders))
	for _, h := range signatureRelevantHeaders {
		if v := req.Header.Get(h); v != "" {
			out[h] = v
		}
	}
	if out["Host"] == "" && req.Host != "" {
		out["Host"] = req.Host
	}
	return out
}

// Inbox handles POST /inbox and POST /users/:id/inbox.
//
// User ごとの inbox であっても処理は同じ (現状のシンプル実装では shared
// inbox と同様に activity を dispatch するだけ)。
//
// #565 で signature 検証 / host block / instance touch / chart hook を
// すべて inbox worker (queue/processors) 側に移し、handler は body+
// signature 関連 header を payload に詰めて 202 即返しする「fast write」
// 設計に変更した。これにより HTTP handler の同期 RSA-2048 verify (~1-2ms)
// が消え、faker.send rps が ~2x 改善する (#564 bench)。Misskey TS と同じ
// アーキテクチャ (read-and-enqueue + verify-in-worker)。
//
// SetEnqueuer 未配線時は legacy synchronous mode に fallback し、handler
// 内で従来通り verify + block + track + chart + process を実行する。これは
// 単体テスト / 旧来配線を維持するためのもので、production 配線では
// SetEnqueuer 経由の async path を踏む。
func (h *Handler) Inbox(c echo.Context) error {
	// upstream ActivityPubServerService.inbox と同じく、federation mode が "none" なら
	// body を読む前に 403 を即返す (#1959)。per-actor の IsAllowed gate は actor が
	// local 扱い (host nil) や async enqueue 後にしか効かない経路があるため、最前段で弾く。
	if h.hostBlocker != nil && h.hostBlocker.FederationDisabled() {
		return c.NoContent(http.StatusForbidden)
	}

	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		// BodyLimit (#1958) が wrap した limitedReader は 64KB 超過時に 413 HTTPError を
		// 返す。ContentLength での即時 reject は handler に到達しないが、chunked 超過は
		// ここで read error になるため echo.HTTPError を伝播して upstream fastify と同じ
		// 413 を返す (それ以外の read error は従来通り 400)。
		var he *echo.HTTPError
		if errors.As(err, &he) {
			return he
		}
		return c.NoContent(http.StatusBadRequest)
	}

	// Echo の Request からはホストヘッダが空のことがあるため、明示的に補完する。
	if c.Request().Header.Get("Host") == "" {
		c.Request().Header.Set("Host", c.Request().Host)
	}

	// upstream ActivityPubServerService.inbox と同じく、queue/処理に回す前に
	// host 署名+一致・digest 署名・digest 値 (sha256(body)) を検証する。RSA は
	// 走らない O(body) の安価なチェックで、fast write / legacy 両 path 共通。
	// Signature ヘッダ欠落/malformed もここで 401 になる (#1949 body integrity)。
	parsed, err := h.admitInbox(c.Request(), body)
	if err != nil {
		// **相手を出す。** activity を捨てる経路なので、誤って落としていた場合に
		// 気付く手段がこのログしか無い (#2725。worker 側の同じ盲点は #2716)。
		//
		// 出せるのは keyId 由来の値だけ。ここは署名検証を通る前で actor は
		// 解決していないし、**body は読まない** — この経路は無認証で誰でも
		// 到達できるので、parse 回数がそのまま攻撃者に押せる CPU になる。
		//
		// worker 側 (#2716) の `signer` は解決済み actor の URI なので、同じ名前は
		// 使わない。ここで出せるのは相手の申告した keyId でしかない。
		slog.Warn("inbox admission rejected",
			"host", signerHostOf(parsed), "keyId", signerKeyIDOf(parsed), "err", err)
		return c.NoContent(http.StatusUnauthorized)
	}

	// upstream ActivityPubServerService.inbox (#17558): actor を持たない activity は
	// authenticate 不能なので enqueue せず 400 で弾く。mk-go は actor 欠落で worker 側が
	// drop する (crash はしない) が、無駄な enqueue/retry を避けるため早期 reject する。
	if !bodyHasActor(body) {
		// admitInbox を通っているので parsed は非 nil。body の actor は読めない
		// から捨てているので出しようが無く、相手を示せるのは署名側だけ (#2725)。
		slog.Warn("inbox rejected: activity has no actor",
			"host", signerHostOf(parsed), "keyId", signerKeyIDOf(parsed))
		return c.NoContent(http.StatusBadRequest)
	}

	if h.enqueuer != nil {
		// Fast write path. signature の crypto 検証は worker 側で再現する。
		// admitInbox を上で通しているので handler 側の presence check は不要。
		// Path には query 込みの RequestURI を積む。送信側は `(request-target)` を
		// path + query で署名するので (本家 7c9c38c04a / 同期経路の
		// VerifyRequestCached も同じ)、URL.Path だと `/inbox?x=1` 宛ての署名が
		// worker 側でだけ合わなくなる。
		payload := queue.InboxPayload{
			Body:    body,
			Method:  c.Request().Method,
			Path:    c.Request().URL.RequestURI(),
			Headers: captureSignatureHeaders(c.Request()),
		}
		if err := h.enqueuer.EnqueueInbox(c.Request().Context(), payload); err != nil {
			// queue 障害時は 500 を返して上流に retry させる (best-effort
			// を装って 202 で握りつぶすと activity が黙って消える)。
			slog.Error("inbox enqueue failed", "err", err)
			return c.NoContent(http.StatusInternalServerError)
		}
		return c.NoContent(http.StatusAccepted)
	}

	// Legacy synchronous fallback (enqueuer 未配線時)。テストや旧来配線を
	// 維持するために残してある。production では router.go が必ず SetEnqueuer を
	// 呼ぶため到達しない。なお本経路は worker 側 InboxProcessor が持つ
	// actor-authorization gate (署名者 != activity.actor のなりすまし検出,
	// #parity review AUTH-1/AUTH-4) を通らない。production が async 一択である
	// 前提のため許容しているが、将来この fallback を残すなら同 gate の適用
	// (または fallback 自体の撤去) が必要。
	actor, keyType, err := h.verifySignature(c.Request())
	if err != nil {
		slog.Warn("inbox signature verification failed", "err", err)
		return c.NoContent(http.StatusUnauthorized)
	}

	if h.isHostBlocked(actor) {
		return c.NoContent(http.StatusForbidden)
	}

	h.touchInstance(actor)
	h.recordSignatureCapability(actor, keyType)
	h.commitChart(actor)

	return h.processSynchronously(c, body)
}

// processSynchronously runs federation.Processor.Process inline and maps
// the outcome to an HTTP status code. Kept as a fallback for tests and
// configurations that don't wire SetEnqueuer.
func (h *Handler) processSynchronously(c echo.Context, body []byte) error {
	if err := h.processor.Process(body); err != nil {
		if errors.Is(err, federation.ErrUnsupportedActivity) {
			// 未対応typeでも 202 Accepted を返す。
			return c.NoContent(http.StatusAccepted)
		}
		slog.Warn("inbox process failed", "err", err)
		return c.NoContent(http.StatusBadRequest)
	}
	return c.NoContent(http.StatusAccepted)
}

// admitInbox enforces the upstream inbox admission checks before the activity
// is queued or processed: the Host header must be signed and match the
// configured host, the Digest header must be signed, and its SHA-256 value must
// equal the body hash. Returns an error to reject with 401 (#1949). The Signature
// header must parse; a missing/malformed Signature also fails here.
//
// 破棄ログに相手を出すため、parse できた ParsedSignature は admission が失敗した
// ときも返す (#2725)。nil になるのは Signature ヘッダ自体が欠落 / malformed の
// ときだけ。**認可には使わない** — 署名検証を通る前の申告値でしかない。
func (h *Handler) admitInbox(req *http.Request, body []byte) (*activitypub.ParsedSignature, error) {
	parsed, err := activitypub.ParseSignatureHeader(req.Header.Get("Signature"))
	if err != nil {
		return nil, err
	}
	return parsed, activitypub.VerifyInboxAdmission(parsed, req.Host, h.expectedHost,
		activitypub.InboxDateHeader(req.Header, parsed.Headers), req.Header.Get("Digest"), body)
}

// maxLoggedSignerLen bounds the attacker-controlled strings the discard logs
// emit.
//
// keyId は署名検証の前に読む申告値で、ヘッダ上限 (internal/server の
// serverMaxHeaderBytes = 1MiB) まで長くできる。そのまま出すとリクエスト 1 本で
// ログを膨らませられる。実在の keyId は URL なのでこの長さに収まる。
//
// colfit からは**数え方 (rune 単位の切り詰め) だけ**を借りている。列に入れる値の
// 規則 (URL は切らずに値ごと捨てる) はここには当てはまらない — ログは表示先で、
// 切れた keyId を保存も参照もしない。
const maxLoggedSignerLen = 200

// signerHostOf returns the host the request's keyId claims to belong to.
//
// **認可には使わない。** 署名検証を通る前の値なので信用できず、破棄したときに
// 相手を示すためだけに使う (worker 側 hostFromSignatureKeyID と同じ位置づけ)。
// parsed が nil (Signature ヘッダ欠落 / malformed) なら空文字を返す。
func signerHostOf(parsed *activitypub.ParsedSignature) string {
	if parsed == nil {
		return ""
	}
	u, err := url.Parse(parsed.KeyID)
	if err != nil {
		return ""
	}
	return colfit.TruncateRunes(u.Host, maxLoggedSignerLen)
}

// signerKeyIDOf returns the keyId the request signed with, bounded for logging.
// parsed が nil なら空文字。
func signerKeyIDOf(parsed *activitypub.ParsedSignature) string {
	if parsed == nil {
		return ""
	}
	return colfit.TruncateRunes(parsed.KeyID, maxLoggedSignerLen)
}

// verifySignature parses the Signature header, resolves the actor, and
// validates the request signature against the actor's stored public key.
// 戻り値の actor は後続の host block チェックに再利用される。KeyType は検証に
// 成功した鍵の種別で、署名方式の観測記録に使う (#2393、err != nil のとき無意味)。
func (h *Handler) verifySignature(req *http.Request) (*model.User, activitypub.KeyType, error) {
	parsed, err := activitypub.ParseSignatureHeader(req.Header.Get("Signature"))
	if err != nil {
		return nil, 0, err
	}
	actorURI := activitypub.ResolveKeyURL(parsed.KeyID)
	actor, err := h.resolver.ResolveActor(actorURI)
	if err != nil {
		return nil, 0, err
	}
	// keyId fragment ベースで Ed25519 / RSA を dispatch する (#1067 / #1070)。
	pem, err := h.resolver.PublicKeyForKeyID(actor.ID, parsed.KeyID)
	if err != nil {
		return nil, 0, err
	}
	// keyCache 経由で verify し、同一 (keyId, PEM) の x509 パースをメモ化する
	// (#1426)。挙動は VerifyRequest と等価。
	kt, err := h.keyCache.VerifyRequestCached(req, parsed.KeyID, pem)
	if err != nil {
		return nil, 0, err
	}
	return actor, kt, nil
}

// isHostBlocked reports whether the actor's host is rejected by the local
// federation policy — either on the blocklist, or excluded by the
// federation mode (none / specified). hostBlocker が未設定 / actor が
// ローカル / Host nil なら false (#536)。
func (h *Handler) isHostBlocked(actor *model.User) bool {
	if h.hostBlocker == nil || actor == nil || actor.Host == nil {
		return false
	}
	host := *actor.Host
	if h.hostBlocker.IsBlocked(host) {
		return true
	}
	return !h.hostBlocker.IsAllowed(host)
}

// touchInstance is a best-effort hook into the InstanceTracker. tracker が
// 未設定 / actor がローカル / Host nil の場合は no-op。エラーは握りつぶす。
func (h *Handler) touchInstance(actor *model.User) {
	if h.instanceTracker == nil || actor == nil || actor.Host == nil {
		return
	}
	_ = h.instanceTracker.MarkRequestReceived(*actor.Host)
}

// recordSignatureCapability is a best-effort hook that records which signature
// scheme the remote host used (#2393). recorder 未設定 / actor がローカル /
// Host nil の場合は no-op。
//
// host block チェックの後に呼ぶ: 署名自体は valid でも、連合しない相手の行を
// 作っても表示先が無い (touchInstance と同じ位置づけ)。
func (h *Handler) recordSignatureCapability(actor *model.User, kt activitypub.KeyType) {
	if h.capabilities == nil || actor == nil || actor.Host == nil {
		return
	}
	h.capabilities.ObserveInboundAlg(*actor.Host, activitypub.KeyTypeName(kt))
}

// commitChart fires the chart hook for one inbound request. Chart hook
// が未設定 / actor がローカル / Host nil の場合は no-op。
func (h *Handler) commitChart(actor *model.User) {
	if h.chartHook == nil || actor == nil || actor.Host == nil {
		return
	}
	h.chartHook.OnInboxReceived(*actor.Host)
}

// bodyHasActor reports whether the inbox activity body is a JSON object with a
// non-null `actor` field. upstream #17558: actor 無し activity は authenticate
// 不能なので enqueue 前に弾く。actor は string ID / embedded object どちらも許容
// (presence + 非 null のみ判定)。
func bodyHasActor(body []byte) bool {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return false // object でない (= 不正) → reject
	}
	actor, ok := probe["actor"]
	return ok && len(actor) > 0 && string(actor) != "null"
}

// HasExpectedHost reports whether the expected Host value is wired.
//
// 空文字だと VerifyInboxAdmission の host 検査が丸ごと skip され、**host ヘッダが
// 署名対象に含まれているかの確認と、自ホストとの一致確認の 2 つが飛ぶ**。
// 本番では router が config.Host を必ず配線する (#2682)。
func (h *Handler) HasExpectedHost() bool { return h.expectedHost != "" }

// HasEnqueuer reports whether the async inbox enqueuer is wired.
//
// 未配線だと Handle が legacy な同期経路へ落ちる。その経路は worker 側の
// **actor-authorization gate (署名者 != activity.actor のなりすまし検出)・
// LD-Signature 検証・inbox replay guard を通らない**。同ファイルのコメントが
// 「production が async 一択である前提のため許容している」と書いているとおり、
// production では必ず配線される (#2682)。
func (h *Handler) HasEnqueuer() bool { return h.enqueuer != nil }

// HasHostBlockChecker reports whether the host block checker was wired.
//
// 未配線だと `federation: none` 設定でも inbox が受け付ける。起動時検査に
// 使う (#2683)。
func (h *Handler) HasHostBlockChecker() bool { return h.hostBlocker != nil }
