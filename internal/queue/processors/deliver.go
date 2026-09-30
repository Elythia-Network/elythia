// Package processors contains task handlers used by the queue worker.
// Handlers are driver-neutral: they accept a driver.Task and return
// driver.ErrSkipRetry to suppress retries.
package processors

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/redis/go-redis/v9"
	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/core/deliveryhealth"
	"github.com/shiroha-a/mk/internal/core/federation"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
)

// deliverKeyCacheSize bounds the parsed-signing-key LRU. 署名者は実質ローカル
// ユーザー数なので 1024 で大半の instance を賄える。超過分は LRU が evict する。
const deliverKeyCacheSize = 1024

// signingKey の cache key 先頭に付ける鍵種別の判別子。RSA / Ed25519 の entry を
// 区別する。将来 P-256 等を増やす際の typo 耐性のため定数化 (#1425 review)。
const (
	keyKindRSA     = "rsa"
	keyKindEd25519 = "ed"
)

// HTTPSigner abstracts the signed POST capability of activitypub.Client so
// that DeliverProcessor can be unit-tested without an actual HTTP client.
type HTTPSigner interface {
	PostSigned(url string, body []byte, key *activitypub.PrivateKey) (*http.Response, error)
}

// ResponseHook is invoked after each HTTP attempt so the instance metadata
// (isNotResponding / notRespondingSince) can be kept up to date. パッケージ間
// の循環依存を避けるため interface で受け取る。実装は core/instance.Service。
type ResponseHook interface {
	RecordResponseSuccess(host string) error
	RecordResponseError(host string) error
	// MarkGoneSuspended suspends an instance whose shared inbox returned 410
	// (goneSuspended、#1811)。
	MarkGoneSuspended(host string) error
}

// ChartHook is invoked after each HTTP attempt so the chart subsystem
// can record ApRequest / Federation / Instance metrics. パッケージ間の
// 循環依存を避けるため interface で受け取る。実装は core/chart/charthook。
type ChartHook interface {
	OnDelivered(host string, succeeded bool)
}

// DeliveryTelemetry receives the detailed outcome of each attempt (#2461).
//
// ResponseHook が `isNotResponding` の真偽値に潰してしまう情報 (status /
// レイテンシ / 失敗の種別) をそのまま渡す先。**分類は本 file の応答 switch を
// そのまま反映する**ので、実装側で「成功とみなす範囲」を再判定しないこと。
//
// 循環依存を避けるため interface で受け取る。実装は core/deliveryhealth。
type DeliveryTelemetry interface {
	RecordDelivery(host string, o deliveryhealth.Outcome)
}

// DeliveryBreaker holds deliveries to hosts that are down or asked us to
// slow down (#3048). 実装は core/deliveryhealth.Breaker。
type DeliveryBreaker interface {
	Check(ctx context.Context, host, job string) deliveryhealth.CheckResult
	RecordFailure(ctx context.Context, host, probeToken string) (open bool, untilProbe time.Duration)
	ReleaseProbe(ctx context.Context, host, probeToken string)
	HoldFor(untilProbe time.Duration) time.Duration
	RecordSuccess(ctx context.Context, host string)
	Throttle(ctx context.Context, host string, retryAfter time.Duration)
}

// SuspendedChecker reports whether delivery to a host should be skipped
// based on meta.deliverSuspendedSoftware.
type SuspendedChecker interface {
	IsSuspended(host string) bool
}

// DeliveryGate reports whether delivery to a host must be skipped because the
// remote instance is administratively blocked or suspended. dispatch 時に
// 呼ばれるため、suspend / block する前にキューへ積まれたジョブや retry-backoff
// 中のジョブも止められる (= enqueue 時フィルタだけでは取りこぼす経路の
// safety net、#1404)。実装は core/instance.Service。
type DeliveryGate interface {
	ShouldSkipDelivery(host string) bool
}

// Ed25519AcceptanceRecorder records that a remote host accepted an
// Ed25519-signed delivery (#2393). 実装は core/instance.CapabilityBuffer。
type Ed25519AcceptanceRecorder interface {
	ObserveEd25519Accepted(host string)
}

// DeliverProcessor handles ap:deliver tasks by posting the activity body to
// the recipient inbox with an HTTP signature.
type DeliverProcessor struct {
	signer           HTTPSigner
	responseHook     ResponseHook
	chartHook        ChartHook
	telemetry        DeliveryTelemetry
	breaker          DeliveryBreaker
	suspendedChecker SuspendedChecker
	deliveryGate     DeliveryGate
	capabilities     Ed25519AcceptanceRecorder
	// redis is used for the per-host Ed25519 degrade flag (#1067 / #1071).
	// 未配線時は capability gate なしの楽観動作 (= payload に Ed25519 鍵が
	// あれば必ず Ed25519 sign を試す) になるが、production では必須。
	redis *redis.Client
	// keyCache は (kind, keyID, PEM) -> パース済み *PrivateKey の LRU。
	// ap:deliver は inbox ごとに 1 job なので、同一署名者へ fan-out する際に
	// 毎 job で PEM->x509 を再パースしていた。worker 間共有の単一 processor
	// にここを持たせて job 横断でメモ化する (#1425)。nil の場合は都度パース。
	keyCache *lru.Cache[string, *activitypub.PrivateKey]
	// keySource は payload に鍵が無いとき (= 新しい job) に引く先。
	keySource SigningKeySource
}

// NewDeliverProcessor constructs a DeliverProcessor.
func NewDeliverProcessor(signer HTTPSigner) *DeliverProcessor {
	p := &DeliverProcessor{signer: signer}
	// lru.New は size>0 で error を返さない実装だが、防御的に握って nil
	// fallback (= 都度パース) に倒す。
	if c, err := lru.New[string, *activitypub.PrivateKey](deliverKeyCacheSize); err == nil {
		p.keyCache = c
	}
	return p
}

// signingKey returns a parsed PrivateKey for (keyID, pem), memoizing the parse
// across deliver jobs. kind は "rsa" / "ed" で、RSA と Ed25519 の cache entry を
// 区別する。PEM が変わる (= 鍵 rotation) と cache key も変わるため stale な鍵を
// 返さない。keyCache 未配線時は parse をそのまま呼ぶ。
//
// cache key は PEM 本体ではなく sha256(keyID+PEM) の hex を使う。map key を
// ~1.5KB から 64B 程度に縮めて memory footprint を抑え、cache key 経由で PEM
// 本体がログ等に混入するリスクも下げる。null-byte separator で keyID/PEM 境界
// の曖昧さを避ける。
func (p *DeliverProcessor) signingKey(kind, keyID, pem string, parse func(string, string) (*activitypub.PrivateKey, error)) (*activitypub.PrivateKey, error) {
	if p.keyCache == nil {
		return parse(keyID, pem)
	}
	sum := sha256.Sum256([]byte(keyID + "\x00" + pem))
	ck := kind + "\x00" + hex.EncodeToString(sum[:])
	if k, ok := p.keyCache.Get(ck); ok {
		return k, nil
	}
	k, err := parse(keyID, pem)
	if err != nil {
		return nil, err
	}
	p.keyCache.Add(ck, k)
	return k, nil
}

// SigningKeySource resolves a local user's signing keys at delivery time.
// **payload に鍵を載せないため**に要る (`admin/queue/jobs` が job を
// moderator へ返すので、載せると鍵がそこから読める)。
type SigningKeySource interface {
	// SigningKeyPEM returns the PEM for the given kind. kind は
	// `keyKindRSA` / `keyKindEd25519` のいずれかで、**値は "rsa" / "ed"**
	// (定数を見ずに "ed25519" と比較すると RSA 鍵が返り、Ed25519 署名が
	// 壊れたうえ 4xx → degrade に化けて気付きにくい)。
	//
	// 鍵が無いときは `ErrSigningKeyMissing` を返す。**DB 障害など一時的な
	// 失敗はそのまま error として返すこと** — 呼び出し元は前者だけ
	// `ErrSkipRetry` に包み、後者は retry させる。取り違えると、障害の瞬間に
	// 配送中だった job が恒久的に消える。
	SigningKeyPEM(userID, kind string) (string, error)
}

// SetSigningKeySource attaches the resolver used when a payload carries no key.
func (p *DeliverProcessor) SetSigningKeySource(s SigningKeySource) {
	p.keySource = s
}

// HasSigningKeySource reports whether the resolver is wired.
// **未配線だと全ての AP 配送が失敗する**ので criticalWiring 表で見る。
func (p *DeliverProcessor) HasSigningKeySource() bool { return p.keySource != nil }

// resolveSigningKey returns the parsed key for kind, preferring the payload's
// inline PEM when present.
//
// **payload の PEM を先に見るのは移行のため。** この変更より前に積まれた job は
// 鍵を持っている。新しい job は `SignerUserID` だけを持つので `keySource` から
// 引く。どちらも `keyCache` でメモ化する — ap:deliver は inbox ごとに 1 job
// なので、同一署名者への fan-out で毎回 DB と x509 parse を通さない (#1425)。
func (p *DeliverProcessor) resolveSigningKey(kind string, payload queue.DeliverPayload) (*activitypub.PrivateKey, error) {
	keyID, inline, parse := payload.KeyID, payload.KeyPEM, activitypub.NewPrivateKey
	if kind == keyKindEd25519 {
		keyID, inline, parse = payload.Ed25519KeyID, payload.Ed25519PrivPEM, activitypub.NewEd25519PrivateKey
	}
	if inline != "" {
		k, err := p.signingKey(kind, keyID, inline, parse)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errSigningKeyInvalid, err)
		}
		return k, nil
	}
	if p.keySource == nil {
		// **恒久扱いにしない。** 配線漏れは再起動で直るので、job を捨てるより
		// retry させて運用者が気付ける形にする (criticalWiring 表でも見る)。
		return nil, fmt.Errorf("no signing key source configured for %s", kind)
	}
	// **fetch に使う値 (SignerUserID) でメモ化する。** keyID を使うと、
	// keyID が空の payload が 2 人分来たときに 2 人目が 1 人目の鍵で署名する。
	// 現在の producer は必ず keyID を詰めるので到達しないが、秘密鍵の
	// キャッシュで「引く値と別の値をキーにする」形は残さない。
	//
	// **鍵は通常運用で変わらない**
	// (UserKeypairRepository は Create と FindByUserID しか持たず、更新は
	// `NormalizePrivateKeysToPKCS8` の一回限りのバッチだけ。あれは同じ鍵の
	// フォーマット変換なので、古い PEM が残っていても署名は正しい)。
	// keyID も含める。`PrivateKey` は keyID を保持するので、`config.url` を
	// 変えた直後に古い keyID を持つキャッシュ済みの鍵で署名しないため。
	ck := kind + "\x00uid\x00" + payload.SignerUserID + "\x00" + keyID
	if p.keyCache != nil {
		if k, ok := p.keyCache.Get(ck); ok {
			return k, nil
		}
	}
	pem, err := p.keySource.SigningKeyPEM(payload.SignerUserID, kind)
	if err != nil {
		return nil, fmt.Errorf("load %s signing key: %w", kind, err)
	}
	if pem == "" {
		return nil, fmt.Errorf("%w: kind=%s user=%s", ErrSigningKeyMissing, kind, payload.SignerUserID)
	}
	k, err := parse(keyID, pem)
	if err != nil {
		// PEM が壊れている。retry しても直らない。
		return nil, fmt.Errorf("%w: %w", errSigningKeyInvalid, err)
	}
	if p.keyCache != nil {
		p.keyCache.Add(ck, k)
	}
	return k, nil
}

// errSigningKeyInvalid marks a key that will never parse (retry は無意味)。
var errSigningKeyInvalid = errors.New("signing key is not parsable")

// isPermanentSigningKeyError reports whether retrying the job cannot help.
//
// **既定は「一時的」に倒す。** 判定を誤ったときの損失が非対称で、一時的な
// ものを恒久と誤ると配送が消える (取り返せない) 一方、恒久的なものを一時と
// 誤っても retry が 12 回空振りして failed になるだけだから。
//
// **既知の副作用**: アカウント削除は最後に `user_keypair` を FK CASCADE で
// 消すので、`Delete(actor)` の配送が backoff 中だとその後の retry で鍵を引け
// ず捨てられる。鍵を payload に載せていた頃は行が消えた後も再送できていた
// (= best-effort の失敗モードが「遅れる」から「消える」に変わった)。同一
// worker が既に署名していればキャッシュが救うが、再起動後や配送ノードが
// 複数ある構成では救われない。
func isPermanentSigningKeyError(err error) bool {
	return errors.Is(err, ErrSigningKeyMissing) || errors.Is(err, errSigningKeyInvalid)
}

// SetRedis attaches a Redis client used to persist per-host Ed25519 degrade
// flags. Ed25519 sign + POST が 4xx で失敗した host は `ed25519:degrade:<host>`
// に EX=300 で記録され、以後 5 分間は同 host への配送が即 RSA fallback される
// (= "assertionMethod を expose しているが Ed25519 verify が壊れている" 実装
// に対する safety net #1067 / #1071)。
func (p *DeliverProcessor) SetRedis(c *redis.Client) {
	p.redis = c
}

// SetSignatureCapabilityRecorder wires the recorder that tracks which remote
// hosts successfully accept Ed25519-signed deliveries (#2393).
func (p *DeliverProcessor) SetSignatureCapabilityRecorder(r Ed25519AcceptanceRecorder) {
	p.capabilities = r
}

// recordEd25519Accepted is a best-effort hook fired when an Ed25519-signed
// delivery returned 2xx. recorder 未配線 / host 不明なら no-op。
func (p *DeliverProcessor) recordEd25519Accepted(host string) {
	if p.capabilities == nil || host == "" {
		return
	}
	p.capabilities.ObserveEd25519Accepted(host)
}

// ed25519DegradeTTL is how long a host stays degraded after the failure
// counter reaches threshold. 5 分は production observation で「broken impl の
// 修正 deploy」に必要な時間より長い (= 改善が deploy されれば自動回復する)
// ことを想定。
const ed25519DegradeTTL = 5 * time.Minute

// ed25519FailWindow defines the sliding window the failure counter is held in.
// 1 つの transient 4xx で degrade が立つのを防ぐ目的で、60s の window 内に
// threshold 件以上の失敗があったときだけ degrade を立てる。
const ed25519FailWindow = 60 * time.Second

// ed25519FailThreshold is the number of failures within ed25519FailWindow
// required to trip the degrade flag. 1 件の false positive (= 受信側 transient
// error) で host 全体を縮退するのを避けるため 3 を採用。
const ed25519FailThreshold = 3

func ed25519DegradeKey(host string) string {
	return "ed25519:degrade:" + host
}

func ed25519FailKey(host string) string {
	return "ed25519:fail:" + host
}

// Ed25519Degraded reports whether deliveries to host currently fall back to
// RSA, for diagnosing federation with it (#3055). isEd25519Degraded と違い
// Redis の障害を隠さない (診断で「落としていない」と断定しないため)。
func (p *DeliverProcessor) Ed25519Degraded(ctx context.Context, host string) (bool, error) {
	if p.redis == nil || host == "" {
		return false, nil
	}
	n, err := p.redis.Exists(ctx, ed25519DegradeKey(host)).Result()
	if err != nil {
		return false, fmt.Errorf("read ed25519 degrade flag: %w", err)
	}
	return n > 0, nil
}

// isEd25519Degraded reports whether the host has the Ed25519 degrade flag
// set in Redis. Redis 未配線 or empty host or Redis 障害は false (= 安全側
// で Ed25519 試行を継続)。
func (p *DeliverProcessor) isEd25519Degraded(host string) bool {
	if p.redis == nil || host == "" {
		return false
	}
	n, err := p.redis.Exists(context.Background(), ed25519DegradeKey(host)).Result()
	if err != nil {
		// Redis 障害時は flag 判定不能 → 安全側に倒して "未 degrade" 扱い
		// (= 引き続き Ed25519 試行)。flag セット側も同じく fail-soft なので
		// 一時的 Redis 障害でも deliver は止まらない。
		return false
	}
	return n > 0
}

// markEd25519Degraded sets the Ed25519 degrade flag for the host with the
// default TTL. Redis 未配線 or empty host or Redis error は best-effort skip。
func (p *DeliverProcessor) markEd25519Degraded(host string) {
	if p.redis == nil || host == "" {
		return
	}
	if err := p.redis.Set(context.Background(), ed25519DegradeKey(host), "1", ed25519DegradeTTL).Err(); err != nil {
		slog.Warn("ed25519 degrade flag set failed", "host", host, "error", err)
	}
}

// recordEd25519Failure increments the per-host failure counter and trips the
// degrade flag once it reaches ed25519FailThreshold within ed25519FailWindow.
// 1 件の transient 4xx で host 全体縮退するのを避け、連続失敗のときだけ縮退
// させる設計 (#1080 review #4)。Redis 未配線 or Redis error は best-effort
// skip して 楽観的 Ed25519 試行を継続。
func (p *DeliverProcessor) recordEd25519Failure(host string) {
	if p.redis == nil || host == "" {
		return
	}
	ctx := context.Background()
	n, err := p.redis.Incr(ctx, ed25519FailKey(host)).Result()
	if err != nil {
		slog.Warn("ed25519 fail counter incr failed", "host", host, "error", err)
		return
	}
	if n == 1 {
		// 初回 INCR で TTL を設定 (window がスライドする)。Expire 失敗時は
		// key が TTL なしで残り counter が永続蓄積するリスクがあるので診断の
		// ために log を出す (#1080 review #2 follow-up)。
		if err := p.redis.Expire(ctx, ed25519FailKey(host), ed25519FailWindow).Err(); err != nil {
			slog.Warn("ed25519 fail counter TTL set failed", "host", host, "error", err)
		}
	}
	if n >= ed25519FailThreshold {
		slog.Warn("ed25519 fail threshold reached, degrading host",
			"host", host, "count", n)
		p.markEd25519Degraded(host)
	}
}

// SetSuspendedChecker attaches a checker for deliverSuspendedSoftware.
func (p *DeliverProcessor) SetSuspendedChecker(c SuspendedChecker) {
	p.suspendedChecker = c
}

// SetDeliveryGate attaches a gate consulted at dispatch time to skip delivery
// to blocked / suspended instances. 未配線なら従来どおりゲート無しで配送する
// (#1404)。
func (p *DeliverProcessor) SetDeliveryGate(g DeliveryGate) {
	p.deliveryGate = g
}

// SetResponseHook attaches a ResponseHook used to update instance health flags.
func (p *DeliverProcessor) SetResponseHook(h ResponseHook) {
	p.responseHook = h
}

// SetChartHook attaches a ChartHook invoked after each delivery attempt.
func (p *DeliverProcessor) SetChartHook(h ChartHook) {
	p.chartHook = h
}

// SetDeliveryTelemetry wires the per-host outcome recorder (#2461)。
// 未配線なら記録しないだけで、配送そのものには影響しない。
func (p *DeliverProcessor) SetDeliveryTelemetry(t DeliveryTelemetry) {
	p.telemetry = t
}

// SetDeliveryBreaker wires the per-host breaker / 429 throttle (#3048)。
// 未配線なら今までどおり、落ちた相手にも retry の回数まで撃ち続ける。
func (p *DeliverProcessor) SetDeliveryBreaker(b DeliveryBreaker) {
	p.breaker = b
}

// hostFromInbox returns the host portion of an inbox URL, or "" if the URL is
// not parseable. ResponseHook 通知用に共通化する。
//
// **正規化は federation 側の規則をそのまま使う。** この値は
// `ShouldSkipDelivery` (blockedHosts / federationHosts / suspensionState) と
// `instance.host` の lookup に渡るので、取り込み側 (`federation.hostFromURI`)
// と同じ形でないと gate が空振りする。生の `u.Host` を返していた頃は
// `https://blocked.example:443/inbox` が `blocked.example:443` になり、
// dispatch 時の block 判定を素通りしていた。**規則をここへ写さないこと** —
// 写した瞬間に片側だけ既定ポートを剥がす状態へ戻る。
func hostFromInbox(inbox string) string {
	return federation.NormalizeGateHost(inbox)
}

// recordSuccess is a best-effort wrapper that fires both the response
// hook and the chart hook for a successful inbox POST.
func (p *DeliverProcessor) recordSuccess(inbox string) {
	host := hostFromInbox(inbox)
	if p.responseHook != nil && host != "" {
		_ = p.responseHook.RecordResponseSuccess(host)
	}
	if p.chartHook != nil {
		p.chartHook.OnDelivered(host, true)
	}
}

// recordGoneSuspended is a best-effort wrapper that suspends the instance whose
// shared inbox returned 410 Gone (#1811)。
func (p *DeliverProcessor) recordGoneSuspended(inbox string) {
	host := hostFromInbox(inbox)
	if p.responseHook != nil && host != "" {
		_ = p.responseHook.MarkGoneSuspended(host)
	}
}

// recordError is a best-effort wrapper that fires both the response
// hook and the chart hook for a failed inbox POST.
func (p *DeliverProcessor) recordError(inbox string) {
	host := hostFromInbox(inbox)
	if p.responseHook != nil && host != "" {
		_ = p.responseHook.RecordResponseError(host)
	}
	if p.chartHook != nil {
		p.chartHook.OnDelivered(host, false)
	}
}

// attempt carries what recordAttempt needs about one delivery attempt.
type attempt struct {
	ctx     context.Context
	host    string
	started time.Time
	// breakerState は Check の時点でこのホストにブレーカーの状態があったか。
	// 無ければ成功のたびに Redis を叩かずに済む。
	breakerState bool
	// probeToken はこの配送が半開の試行のときの札 (試行でなければ "")。
	// 試行の失敗だけが間隔を倍にする。
	probeToken string
	// local は HTTP の送信まで至らなかった失敗 (署名鍵の読み込みなど、こちら側の
	// 事情)。相手の不調ではないのでブレーカーに数えない。
	local bool
}

// errLocalSend marks a delivery failure that happened before any request
// reached the remote host (our side: signing key, etc.).
var errLocalSend = errors.New("ap deliver: failed before sending")

// recordAttempt forwards one attempt's outcome to the health recorder and
// the breaker.
//
// **判定はしない。** class は呼び出し元 (応答 switch) が決める。ここで
// status から再分類すると「成功とみなす範囲」が二重管理になる。観測と
// ブレーカーも同じ class から決める (#3048)。
//
// 戻り値はブレーカーが開いていてこのジョブを待たせるべき時間 (held が true の
// とき)。**開いている間の失敗は試行回数を消費させない。** 消費させると、溜まった
// ジョブが試行のたびに持ち回りで回数を失い、7 日の自動停止より前に捨てられる。
func (p *DeliverProcessor) recordAttempt(a attempt, class deliveryhealth.OutcomeClass, status int, errMsg string) (hold time.Duration, held bool) {
	if a.host == "" {
		return 0, false
	}
	if p.telemetry != nil {
		p.telemetry.RecordDelivery(a.host, deliveryhealth.Outcome{
			Class:   class,
			Status:  status,
			Latency: time.Since(a.started),
			Err:     errMsg,
		})
	}
	if p.breaker == nil {
		return 0, false
	}
	if a.local {
		// こちら側の失敗は相手の不調として数えない。試行だったなら枠だけ返す
		// (返さないと 5 分間次の試行ができない)。
		p.breaker.ReleaseProbe(a.ctx, a.host, a.probeToken)
		return 0, false
	}
	// 相手が落ちているのは接続失敗と 5xx だけ。4xx / 410 / 429 は応答が返って
	// きている (相手は健在) ので閉じる側に数える。4xx は送ったものが悪く、止めても
	// 直らない。
	switch class {
	case deliveryhealth.ClassTransport, deliveryhealth.ClassServerError:
		open, untilProbe := p.breaker.RecordFailure(a.ctx, a.host, a.probeToken)
		if open {
			return p.breaker.HoldFor(untilProbe), true
		}
	default:
		if a.breakerState {
			p.breaker.RecordSuccess(a.ctx, a.host)
		}
	}
	return 0, false
}

// Handle dispatches a single deliver task. The driver runtime invokes
// this for every dequeued task.
func (p *DeliverProcessor) Handle(ctx context.Context, t driver.Task) error {
	payload, err := queue.DecodeDeliverPayload(t.Payload())
	if err != nil {
		// payload が壊れているジョブは何度リトライしても無意味なのでスキップ。
		return fmt.Errorf("decode deliver payload: %w: %w", err, driver.ErrSkipRetry)
	}
	// 期限付きの job (#3238) は、期限を過ぎていれば送らない。
	if expired(payload, time.Now()) {
		slog.Info("ap deliver: dropped (past its deadline)", "inbox", payload.Inbox)
		return fmt.Errorf("ap deliver: past its deadline: %w", driver.ErrSkipRetry)
	}

	// deliverSuspendedSoftware: 対象インスタンスの software がリストに該当すればスキップ
	host := hostFromInbox(payload.Inbox)
	// suspend / block されたインスタンスへは配送しない。enqueue 時にもフィルタ
	// しているが、ブロック前に積まれたジョブや retry-backoff 中のジョブは enqueue
	// 時チェックを通り抜けるため、dispatch 時にも弾く (#1404)。
	if p.deliveryGate != nil && host != "" && p.deliveryGate.ShouldSkipDelivery(host) {
		slog.Info("ap deliver: skip (instance suspended or blocked)", "inbox", payload.Inbox)
		return nil
	}
	if p.suspendedChecker != nil && host != "" && p.suspendedChecker.IsSuspended(host) {
		slog.Info("ap deliver: skip (software suspended)", "inbox", payload.Inbox)
		return nil
	}

	// Ed25519 sign 経路: payload に Ed25519 鍵情報があり (= DeliverService が
	// recipient capable と判定) かつ host が degrade flag に立っていない
	// (= 過去 5min 以内に Ed25519 4xx 失敗がない) ときに試す (#1067 / #1071)。
	// **判定は KeyID で行う。** 鍵 PEM は payload に載らなくなったので
	// (admin/queue/jobs から読めてしまうため)、PrivPEM の有無では判定できない。
	// KeyID は DeliverService が「相手が FEP-521a で Ed25519 を expose して
	// いる」と判断したときだけ詰める、という契約は変わっていない。
	useEd25519 := (payload.Ed25519KeyID != "" || payload.Ed25519PrivPEM != "") && !p.isEd25519Degraded(host)

	// 落ちている相手 / 間隔を空けるよう言われた相手へは送らず、試行回数を消費せずに
	// 後へ回す (#3048)。**生成側 (fanout) では止めない** — ブレーカーの開閉は可変な
	// 状態なので、生成側で止めると閉じたときに配送が復活しない。ジョブは作り、
	// 取り出したときに判断する。Delete も同じ扱いにする (相手が落ちている間は、
	// 迂回させても届かない)。
	a := attempt{ctx: ctx, host: host}
	if p.breaker != nil && host != "" {
		r := p.breaker.Check(ctx, host, deliveryJobKey(t.Payload()))
		if r.Decision == deliveryhealth.BreakerDelay {
			slog.Debug("ap deliver: held back (breaker open or rate limited)", "host", host, "wait", r.Delay)
			return holdOrDrop(payload, fmt.Errorf("ap deliver: %s held back", host), r.Delay)
		}
		a.probeToken = r.ProbeToken
		a.breakerState = r.HasState || r.Decision == deliveryhealth.BreakerProbe
	}

	// 配送レイテンシの起点 (#2461)。sendOnce の中で Ed25519->RSA の再送が起きた
	// 場合も含めて「この job が相手にかけた時間」を測る。
	a.started = time.Now()

	resp, signedWithEd25519, err := p.sendOnce(payload, useEd25519)
	if err != nil {
		// network error / parse error は再投函。詳細は sendOnce 内で log 済。
		p.recordError(payload.Inbox)
		a.local = errors.Is(err, errLocalSend)
		if hold, held := p.recordAttempt(a, deliveryhealth.ClassTransport, 0, err.Error()); held {
			return holdOrDrop(payload, err, hold)
		}
		return err
	}
	// closure 内の resp は defer 実行時の現在値を見る (= retry 後の resp も
	// 同じ defer 1 つで cleanup される)。retry 経路では古い resp を手動 Close
	// したあと `resp = nil` してから新 resp を代入することで double Close を
	// 回避する設計 (#1080 review #1)。
	defer func() {
		if resp == nil {
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	// Ed25519 sign で 4xx が返った場合は host を degrade して RSA で 1 回 retry
	// する safety net。"assertionMethod を expose しているが Ed25519 verify が
	// 壊れている" broken impl 対策。retry も失敗したら通常 4xx 経路で扱う。
	// #2106 N30: 429 (rate limited) は Ed25519 verify の問題ではなく単なる送信過多なので、
	// Ed25519→RSA degrade 経路には乗せず通常の retryable 扱い (下の switch) に落とす。
	if useEd25519 && resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusGone && resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusTooManyRequests {
		slog.Warn("ap deliver: ed25519 4xx, recording failure + retry with RSA",
			"host", host, "status", resp.StatusCode)
		p.recordEd25519Failure(host)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		resp = nil // defer の double Close を避ける
		retryResp, retrySignedWithEd25519, retryErr := p.sendOnce(payload, false)
		if retryErr != nil {
			p.recordError(payload.Inbox)
			a.local = errors.Is(retryErr, errLocalSend)
			if hold, held := p.recordAttempt(a, deliveryhealth.ClassTransport, 0, retryErr.Error()); held {
				return holdOrDrop(payload, retryErr, hold)
			}
			return retryErr
		}
		resp = retryResp // defer は新 resp を Close する
		// RSA で送り直したので、この後の 2xx を Ed25519 の成功として数えない。
		signedWithEd25519 = retrySignedWithEd25519
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		p.recordSuccess(payload.Inbox)
		_, _ = p.recordAttempt(a, deliveryhealth.ClassSuccess, resp.StatusCode, "")
		if signedWithEd25519 {
			// Ed25519 で送って同期的に拒否されなかったことを記録する (#2393)。
			// 「相手が検証できた」までは言えない (verify-in-worker な実装は検証前に
			// 202 を返す)。同期拒否は上の 4xx degrade 経路が拾う。
			p.recordEd25519Accepted(host)
		}
		return nil
	case resp.StatusCode == http.StatusGone,
		resp.StatusCode == http.StatusNotFound:
		// 410 / 404: 受信側がもう存在しない。リトライしても無駄なのでスキップ。
		// 「応答した」事自体は事実なので isNotResponding は解除する。
		slog.Info("ap deliver: target gone",
			"inbox", payload.Inbox, "status", resp.StatusCode)
		p.recordSuccess(payload.Inbox)
		_, _ = p.recordAttempt(a, deliveryhealth.ClassGone, resp.StatusCode, "")
		// shared inbox が 410 Gone を返したらインスタンス全体が消滅したとみなし
		// goneSuspended に切り替えて以後の配送を止める (upstream
		// DeliverProcessorService の isSharedInbox && 410、#1811)。404 は個別 actor
		// 不在の可能性があるので suspend しない。
		if payload.IsSharedInbox && resp.StatusCode == http.StatusGone {
			p.recordGoneSuspended(payload.Inbox)
		}
		return fmt.Errorf("target gone (%d): %w", resp.StatusCode, driver.ErrSkipRetry)
	case resp.StatusCode == http.StatusTooManyRequests:
		// #2106 N30: 429 は 4xx だが upstream status-error.ts では retryable
		// (isRetryable = !isClientError || statusCode === 429)。rate-limited な配送先には
		// backoff 付きで再試行する。「応答した」事実があるので isNotResponding は解除する
		// (instance は健在で rate-limit しているだけ)。ErrSkipRetry を付けず queue に retry させる。
		slog.Warn("ap deliver: rate limited (429), will retry",
			"inbox", payload.Inbox)
		p.recordSuccess(payload.Inbox)
		_, _ = p.recordAttempt(a, deliveryhealth.ClassRateLimited, resp.StatusCode, "")
		// ほかのジョブも同じホストへは間隔を空ける (#3048)。このジョブ自体は
		// 今までどおり retry の回数を消費する (上限があるように)。戻ってきたときは
		// 間隔の判定で待たされるので、Retry-After より早くは撃たない。
		if p.breaker != nil && host != "" {
			p.breaker.Throttle(ctx, host, parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()))
		}
		return fmt.Errorf("rate limited (429): %s", resp.Status)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// その他の4xxは受信側の不正リクエスト扱い。HTTP として応答が返って
		// きているので isNotResponding 状態は解除する。
		slog.Warn("ap deliver: client error",
			"inbox", payload.Inbox, "status", resp.StatusCode)
		p.recordSuccess(payload.Inbox)
		_, _ = p.recordAttempt(a, deliveryhealth.ClassClientError, resp.StatusCode, resp.Status)
		return fmt.Errorf("client error (%d): %w", resp.StatusCode, driver.ErrSkipRetry)
	default:
		// 5xx は受信側の一時的な障害。リトライさせる + 不調状態としてマーク。
		slog.Warn("ap deliver: server error",
			"inbox", payload.Inbox, "status", resp.StatusCode)
		p.recordError(payload.Inbox)
		if hold, held := p.recordAttempt(a, deliveryhealth.ClassServerError, resp.StatusCode, resp.Status); held {
			return holdOrDrop(payload, fmt.Errorf("server error: %s", resp.Status), hold)
		}
		return errors.New("server error: " + resp.Status)
	}
}

// sendOnce performs one signed POST attempt. useEd25519=true なら
// payload.Ed25519KeyID + Ed25519PrivPEM で sign、それ以外は RSA。鍵 parse 失敗
// 時は (useEd25519=true ならば) RSA に fallback する。RSA 鍵自体が壊れている
// 場合は driver.ErrSkipRetry で投函を中止。
//
// 2 つ目の戻り値は「実際に Ed25519 で署名したか」。呼び出し元の useEd25519 とは
// 一致しないことがある (鍵 parse 失敗で RSA に落ちた場合)。相手が Ed25519 を
// 観測として記録してよいのは実際に署名した方式だけなので、引数ではなくこの
// 戻り値を見ること (#2393)。
func (p *DeliverProcessor) sendOnce(payload queue.DeliverPayload, useEd25519 bool) (*http.Response, bool, error) {
	if useEd25519 {
		key, err := p.resolveSigningKey(keyKindEd25519, payload)
		if err == nil {
			resp, perr := p.signer.PostSigned(payload.Inbox, payload.Body, key)
			if perr != nil {
				slog.Warn("ap deliver: ed25519 post failed",
					"inbox", payload.Inbox, "err", perr)
				return nil, true, perr
			}
			return resp, true, nil
		}
		slog.Warn("ap deliver: ed25519 key parse failed, falling back to RSA",
			"inbox", payload.Inbox, "err", err)
		// fallthrough: RSA で sign
	}
	key, err := p.resolveSigningKey(keyKindRSA, payload)
	if err != nil {
		// **一時的な失敗は retry させる。** 鍵を payload から外したことで、
		// この分岐には DB 接続断・failover・pool 枯渇も合流するようになった。
		// それを ErrSkipRetry に包むと、**障害の瞬間に配送中だった job が恒久的に
		// 消える** (deliver は既定 12 回・約 32 時間かけて再送する設計)。
		// 消えたことは相手サーバー側でしか分からない。
		//
		// 恒久的な失敗 (鍵が無い / PEM が壊れている) だけを ErrSkipRetry にする。
		if isPermanentSigningKeyError(err) {
			return nil, false, fmt.Errorf("parse private key: %w: %w: %w", err, errLocalSend, driver.ErrSkipRetry)
		}
		return nil, false, fmt.Errorf("load private key: %w: %w", err, errLocalSend)
	}
	resp, perr := p.signer.PostSigned(payload.Inbox, payload.Body, key)
	if perr != nil {
		slog.Warn("ap deliver: post failed",
			"inbox", payload.Inbox, "err", perr)
		return nil, false, perr
	}
	return resp, false, nil
}

// HasDeliveryGate reports whether the dispatch-time delivery gate was wired.
//
// **enqueue 時のチェックを通り抜けたジョブを弾く第 2 の関門**なので、
// `deliver.hostBlockChecker` では代替できない。ブロック前に積まれたジョブや
// retry-backoff 中のジョブが該当する (#1404)。起動時検査に使う (#2708)。
//
// `SetSuspendedChecker` は operator が `deliverSuspendedSoftware` を設定した
// ときだけ配線されるので対象外。
func (p *DeliverProcessor) HasDeliveryGate() bool { return p.deliveryGate != nil }

// parseRetryAfter reads an HTTP Retry-After value (delay-seconds or an
// HTTP-date). It returns 0 when the header is absent or unusable; the
// breaker then applies its default.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
		// int64 に収まらない秒数は ParseInt が上限値と ErrRange を返す。数字としては
		// 読めているので、捨てずに上限で受ける。
		if secs <= 0 {
			return 0
		}
		// 上限はブレーカー側 (ThrottleMax) で掛ける。ここで秒数を Duration に
		// 直すときに溢れないよう、先に 1 日で頭打ちにする。
		return time.Duration(min(secs, 24*60*60)) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// deliveryJobKey identifies a delivery across wake-ups so that a job held by
// a 429 keeps its reserved send time.
//
// ジョブの ID は driver.Task に載っていないので payload から作る。同じ payload の
// ジョブが 2 つあると予約を共有するが、先に起きた方が予約を消し、後の方は
// 止めていなければそのまま送る (止めていれば並び直す) だけで、取りこぼしは無い。
func deliveryJobKey(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:16])
}

// expired reports whether a delivery with a deadline is past it (#3238).
func expired(p queue.DeliverPayload, now time.Time) bool {
	return p.NotAfter != 0 && now.UnixMilli() > p.NotAfter
}

// holdOrDrop defers the job by d without using up an attempt — unless the
// delivery has a deadline, in which case it is dropped (#3238)。期限付きの job を
// 後へ回すと、試行を消費しないまま何時間も後に届きうる (ブレーカーが開いている
// 間はずっと)。
func holdOrDrop(p queue.DeliverPayload, err error, d time.Duration) error {
	if p.NotAfter != 0 {
		return fmt.Errorf("%w (dropped instead of held: has a deadline): %w", err, driver.ErrSkipRetry)
	}
	return fmt.Errorf("%w: %w", err, driver.Delay(d))
}
