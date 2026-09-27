package processors

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/core/deliveryhealth"
	"github.com/shiroha-a/mk/internal/core/federation"
	"github.com/shiroha-a/mk/internal/misc/idnhost"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
)

// FederationProcessor is the narrow surface of federation.Processor that
// InboxProcessor consumes (#534). 切り出すことで unit test が
// federation.NewProcessor(...) の重い依存 chain (resolver / following /
// reaction / userRepo / noteRepo) を組まなくても済む。
type FederationProcessor interface {
	Process(body []byte) error
}

// SignerAwareProcessor is an optional extension of FederationProcessor that
// accepts the verified HTTP signer (#2338).
//
// Mastodon 系リレーは Announce ではなく Create を LD-Signature 付きで転送する
// ため、「誰が配送してきたか」が分からないとリレー転送を判別できない。既存の
// 実装 / テスト fake を壊さないよう optional interface にしてある。
type SignerAwareProcessor interface {
	ProcessWithSigner(body []byte, signer *model.User) error
}

// RelayAwareVerifier is an optional extension of SignatureVerifier that can
// mark relay-derived actors when it creates them (#2340).
//
// 転送活動の LD-Signature 検証は creator の公開鍵を DB に載せる必要があるため、
// この経路の著者は DB に残る。後追いの孤児掃除で回収できるよう印を付ける。
type RelayAwareVerifier interface {
	ResolveActorViaRelay(actorURI string) (*model.User, error)
}

// RelayActorChecker reports whether an actor is a subscribed relay.
// Implemented by core/relay.Service.
type RelayActorChecker interface {
	IsRelayActor(actor *model.User) bool
}

// SetRelayActorChecker wires the relay lookup used to mark relay-derived
// actors. Optional — nil disables the marking (掃除の対象にならないだけ)。
func (p *InboxProcessor) SetRelayActorChecker(c RelayActorChecker) {
	p.relayChecker = c
}

// SignatureVerifier resolves an actor and confirms an HTTP request was
// signed with that actor's published public key. Implemented by
// federation.Resolver in production. nil の場合 verify を skip するので、
// payload に headers が無い (= legacy 形式) ケースと同様に扱う。
//
// PublicKeyForKeyID は signature header の keyId fragment (`#main-key` /
// `#ed25519-key` 等) に対応する PEM を返す。user_publickey_extra (FEP-521a
// Multikey) を先に探し、miss なら user_publickey の primary key (RSA) を
// fallback で返す → drop-in 互換維持 (#1067 / #1070)。
type SignatureVerifier interface {
	ResolveActor(actorURI string) (*model.User, error)
	PublicKeyForActor(actorID string) (string, error)
	PublicKeyForKeyID(actorID, keyID string) (string, error)
}

// HostBlockChecker reports whether a host is on the blocked list and whether
// the federation mode allows ingesting from it (#536). nil なら check を skip。
type HostBlockChecker interface {
	IsBlocked(host string) bool
	IsAllowed(host string) bool
}

// InstanceTracker is invoked after a successfully verified inbound
// activity so that the instance row's latestRequestReceivedAt can be
// updated. nil なら no-op。
type InstanceTracker interface {
	MarkRequestReceived(host string) error
}

// InboxChartHook is invoked after a successfully verified inbound
// activity so the chart subsystem can record federation / instance /
// request charts. nil なら no-op。
//
// processors.ChartHook は deliver 側 (OnDelivered) で同名占有されている
// ので、inbox 用は別名に分けて両 driver が共存できるようにしている。
type InboxChartHook interface {
	OnInboxReceived(host string)
}

// InboxProcessor handles ap:inbox tasks. From #565 onwards, signature
// verification + host block + instance tracker + chart hook all run in
// this worker (previously they were synchronous in the HTTP handler).
// The handler now just builds the payload and 202-returns, so faker.send
// rps is bound by HTTP I/O instead of RSA-2048 verify (~1-2ms).
//
// 各 activity handler は冪等であることを前提とする (Misskey TS と同じ
// 戦略)。順序保証や per-actor lock は持たない。
// LDSignatureVerifier is the per-activity LD-Signature gate (#1164 Phase D)。
// signature field がある activity に対して RsaSignature2017 + 2026.5.4
// hardening (forbidden directives / cache cap / freeze) を実行する。nil なら
// LD-Sig 経路を skip (= HTTP Signature だけで認証完了とする後方互換)。
// 実装は core/federation.LDSignatureVerifier。
type LDSignatureVerifier interface {
	// CheckForbiddenDirectivesIfPresent runs only the forbidden-directive hardening
	// (no key resolution / RSA verify). Used by the signer==actor path (#2106 N26)
	// so an HTTP-signature-authenticated activity is not dropped just because its
	// LD-Signature fails to verify, while keeping the JSON-LD injection defense.
	CheckForbiddenDirectivesIfPresent(rawBody []byte) error
	// VerifyAndCompact additionally reports whether the activity carried an
	// LD-Signature and, on success, the verified signature.creator key URI and
	// the activity compacted to the signed properties. Used to authenticate
	// forwarded activities whose HTTP signer differs from the activity actor
	// (and on the header-less legacy path); the caller must process the
	// returned Body instead of the raw body.
	VerifyAndCompact(rawBody []byte) (verified federation.VerifiedLDActivity, present bool, err error)
}

// SignatureCapabilityRecorder records which signature scheme a remote host
// actually uses (#2393). パッケージ間の循環依存を避けるため interface で受け取る
// (実装は core/instance.CapabilityBuffer)。
type SignatureCapabilityRecorder interface {
	ObserveInboundAlg(host, alg string)
	ObserveLDSignature(host string)
}

type InboxProcessor struct {
	processor  FederationProcessor
	verifier   SignatureVerifier
	ldVerifier LDSignatureVerifier
	// relayChecker は LD-Signature 検証で解決する著者に「リレー由来」の印を
	// 付けるかどうかの判定に使う (#2340)。
	relayChecker    RelayActorChecker
	hostBlocker     HostBlockChecker
	instanceTracker InstanceTracker
	chartHook       InboxChartHook
	capabilities    SignatureCapabilityRecorder
	// telemetry は受信結果をホストごとに記録する (#2471)。未配線なら記録
	// しないだけで、inbox 処理そのものには影響しない。
	telemetry DeliveryTelemetry
	// replayGuard は処理済み activity を覚えて再投函を弾く。未配線なら
	// 保護しないだけ (従来どおり冪等性に委ねる)。
	replayGuard InboxReplayGuard
	// keyCache は受信 HTTP Signature verify の公開鍵パースをメモ化する (#1426)。
	// InboxProcessor は worker 間共有の単一インスタンス (router.go:616) なので、
	// 同一 remote actor からの連続 activity で x509 パースを 1 回に集約できる
	// (#1436 の DeliverProcessor.keyCache と対になる inbound 側最適化)。
	keyCache *activitypub.PublicKeyCache
}

// NewInboxProcessor constructs an InboxProcessor wrapping the supplied
// federation.Processor. Set* メソッドで verify / block / track / chart の
// 各 dep を別途配線する。未配線の dep は no-op として扱われる。
func NewInboxProcessor(p FederationProcessor) *InboxProcessor {
	return &InboxProcessor{processor: p, keyCache: activitypub.NewPublicKeyCache(0)}
}

// SetLDSignatureVerifier wires an optional LD-Signature verifier that runs
// after HTTP Signature verify but before activity dispatch. signature 無し
// activity は skip され、verify fail なら activity を drop (return nil で
// queue を ack するが Process は呼ばない)。
func (p *InboxProcessor) SetLDSignatureVerifier(v LDSignatureVerifier) {
	p.ldVerifier = v
}

// SetInboxReplayGuard wires the guard that drops re-posted activities.
//
// **署名検証を通った経路にしか効かせない。** activity.id は body の値なので、
// 署名で本文が縛られていて、かつ authorizeActor が id の host と actor の host の
// 一致を確かめた後でなければ信用できない。信用できない id を覚えると、他人の
// activity id を先に登録して**本物を落とす**ことができてしまう。
func (p *InboxProcessor) SetInboxReplayGuard(g InboxReplayGuard) {
	p.replayGuard = g
}

// SetSignatureVerifier wires a verifier used to re-verify the inbound
// HTTP signature in the worker.
//
// **未配線でも payload は落ちない。** Handle の
// `if len(payload.Headers) > 0 && p.verifier != nil` が偽になり、
// 署名検証・連合ブロック・actor 一致 (authorizeActor)・replay 判定を
// **まとめて飛ばしたまま dispatch へ進む**。以前ここには "payloads carrying
// Headers are dropped" と書いてあったが誤りで、fail-closed に読める分だけ
// 危険だった (#2682)。起動時の配線検査は HasSignatureVerifier を見る。
func (p *InboxProcessor) SetSignatureVerifier(v SignatureVerifier) {
	p.verifier = v
}

// SetHostBlockChecker wires a checker used to reject activities from
// federation-blocked hosts after signature verification.
func (p *InboxProcessor) SetHostBlockChecker(c HostBlockChecker) {
	p.hostBlocker = c
}

// SetInstanceTracker wires the InstanceTracker used to update
// latestRequestReceivedAt for a verified actor's host.
func (p *InboxProcessor) SetInstanceTracker(t InstanceTracker) {
	p.instanceTracker = t
}

// SetChartHook wires the InboxChartHook used to record federation /
// inbox / request charts for a verified actor's host.
func (p *InboxProcessor) SetChartHook(h InboxChartHook) {
	p.chartHook = h
}

// SetDeliveryTelemetry wires the per-host inbox outcome recorder (#2471)。
// 未配線なら記録しないだけで、受信処理には影響しない。
func (p *InboxProcessor) SetDeliveryTelemetry(t DeliveryTelemetry) {
	p.telemetry = t
}

// SetSignatureCapabilityRecorder wires the recorder that tracks which
// signature scheme each remote host uses (#2393).
func (p *InboxProcessor) SetSignatureCapabilityRecorder(r SignatureCapabilityRecorder) {
	p.capabilities = r
}

// Handle dispatches a single inbox task. driver runtime invokes this for
// every dequeued task.
//
// payload decode 失敗は再 retry しても無意味なので driver.ErrSkipRetry で
// 確定 fail にする。worker 側の verify 失敗 / host block も retry せず
// silently drop する (sender に retry 要求しても解決しないため)。
// federation.ErrUnsupportedActivity は handler 不在 (= 受け付けたが何も
// しない) 扱いで成功扱い。それ以外の処理エラーは driver の retry policy
// (inboxJobMaxAttempts) に任せる。
// hostFromSignatureKeyID extracts the sender host from a Signature header's
// keyId, for telemetry attribution only.
//
// **認可に使ってはいけない。** 署名検証を通る前の値なので、送信者が自由に
// 名乗れる。ここで使うのは「どの host からの受信を数えるか」の宛先だけで、
// 誤っていても集計が汚れるに留まる (#2471)。
func hostFromSignatureKeyID(sig string) string {
	if sig == "" {
		return ""
	}
	parsed, err := activitypub.ParseSignatureHeader(sig)
	if err != nil {
		return ""
	}
	u, err := url.Parse(parsed.KeyID)
	if err != nil {
		return ""
	}
	return u.Host
}

// recordInboxTelemetry forwards one inbox outcome to the health recorder
// (#2471).
//
// **判定はしない。** class は呼び出し元 (Handle の各分岐) が決める。ここで
// 再分類すると「受理とみなす範囲」が二重管理になる。
func (p *InboxProcessor) recordInboxTelemetry(host string, class deliveryhealth.OutcomeClass, started time.Time, errMsg string) {
	if p.telemetry == nil || host == "" {
		return
	}
	p.telemetry.RecordDelivery(host, deliveryhealth.Outcome{
		Class:   class,
		Latency: time.Since(started),
		Err:     errMsg,
	})
}

func (p *InboxProcessor) Handle(_ context.Context, t driver.Task) error {
	payload, err := queue.DecodeInboxPayload(t.Payload())
	if err != nil {
		return fmt.Errorf("decode inbox payload: %w: %w", err, driver.ErrSkipRetry)
	}

	// payload に Headers が含まれている = #565 の fast-write handler から
	// 来た payload。worker 側で signature を verify する。Headers が無い
	// (= legacy / direct enqueue) 経路では verify をスキップする (handler
	// 側で既に検証済みという旧 contract)。
	host := payload.Host
	if host == "" {
		// **本番の inbox handler は payload.Host を設定していない。**
		// リクエストの Host header は「こちらのホスト名」なので送信元を表さず、
		// 埋めようが無いため空のまま流れてくる (既存の
		// 以前は破棄ログもこの空文字を出していた (#2716 で導出済みの host に
		// 差し替えた)。
		//
		// テレメトリは送信元ごとに集計するので、署名の keyId から導く (#2471)。
		// **認可には使わない** — 署名検証を通る前の値なので信用できない。
		// 集計の宛先を決めるためだけに使う。
		//
		// **keyId 由来なので port 付き / 大文字混在になりうる。** 署名者が
		// 解決できたら下で `*actor.Host` (DB 正規化済み) に差し替わる。
		host = hostFromSignatureKeyID(payload.Headers["Signature"])
	}
	// 受信結果の記録起点 (#2471)。host は署名者が解決できたら実 host に
	// 差し替わるので、記録は各分岐で行う。
	started := time.Now()
	// 署名者は Process へ渡してリレー転送の判定に使う (#2338)。
	var signer *model.User
	// activityID は再投函の判定に使う。**署名検証を通った経路でだけ埋まる** —
	// 未署名の body から採った id は相手の申告でしかなく、覚えると他人の id を
	// 先に登録して本物を落とせる。
	activityID := ""
	// body は dispatch する本文。転送経路では authorizeActor が署名済みの内容へ
	// 差し替える。
	body := payload.Body
	if len(payload.Headers) > 0 && p.verifier != nil {
		actor, keyType, err := p.verifyPayload(payload)
		if errors.Is(err, federation.ErrLookupUnavailable) {
			// **「確かめられなかった」は ack しない** (#3121)。署名が合わない body を
			// retry しても結果は変わらないので既定は ack だが、検証は DB も読むので
			// **DB 障害のあいだに届いた activity まで同じ ack に落ちる**。相手の
			// 再送は当てにできない (inbox は enqueue した時点で 202 を返す)。
			slog.Error("inbox: cannot verify signature (verification unavailable)",
				"host", host, "err", err)
			p.recordInboxTelemetry(host, deliveryhealth.ClassProcessingError, started, err.Error())
			return err
		}
		if err != nil {
			slog.Warn("inbox: signature verification failed in worker",
				"host", host, "err", err)
			p.recordInboxTelemetry(host, deliveryhealth.ClassSignatureFailed, started, err.Error())
			return nil
		}
		if p.isBlocked(actor) {
			// upstream Misskey #17336 (= 2026.5.0 fix) は NoteCreateService 深部で
			// 出る "Instance is blocked" IdentifiableError を error handler で捕まえて
			// retry を防ぐ修正だが、mk-go は verify-in-worker 設計 (#565) でここに
			// signature verify 直後に block check を入れているため、blocked host の
			// activity は body parse / NoteCreate の段にすら届かず、retry にも乗らない。
			// よって upstream の追加 case 分岐は mk-go では不要 (= triage #1003 close)。
			//
			// **ここは元々ログすら出ない。** 記録しないと「ブロックしたホストから
			// どれだけ来ているか」が一切見えない (#2471)。
			if actor != nil && actor.Host != nil {
				host = *actor.Host
			}
			p.recordInboxTelemetry(host, deliveryhealth.ClassBlocked, started, "")
			return nil
		}
		// HTTP 署名者 (actor) が activity body の actor と一致することを保証する
		// (actor spoofing 対策)。一致しない場合は転送活動とみなし LD-Signature が
		// body actor を認証している場合のみ許可する。LD-Signature の hardening
		// (forbidden directive 等) も本 gate に集約する。
		fields, authorizedBody, err := p.authorizeActor(payload.Body, actor)
		if err != nil {
			if errors.Is(err, federation.ErrLookupUnavailable) {
				// **ここも ack しない** (#3121)。転送 activity の認可は
				// LD-Signature の creator を `ResolveActor` で解決し、その鍵を
				// DB から引くので、**署名検証と同じだけ DB を読む**。理由を
				// 見ずに drop すると、DB 障害のあいだリレー経由の配送が
				// まるごと消える。
				slog.Error("inbox: cannot authorize actor (lookup unavailable)",
					"host", host, "signer", signerURIOf(actor), "err", err)
				p.recordInboxTelemetry(host, deliveryhealth.ClassProcessingError, started, err.Error())
				return err
			}
			// **payload.Host を出さない。** これは常に空 (リクエストの Host
			// ヘッダはこちらのホスト名なので送信元を表さず、handler も設定
			// しない)。署名の keyId から導いた host は既に上で計算済みなので
			// そちらを出す (#2716)。
			//
			// signer (HTTP 署名の actor) と actor (body の actor) は転送活動で
			// 食い違う。捨てた理由を追うにはどちらも要る。
			slog.Warn("inbox: actor authorization failed, dropping activity",
				"host", host,
				"signer", signerURIOf(actor),
				"actor", fields.Actor,
				"activityType", fields.Type,
				"err", err)
			p.recordInboxTelemetry(host, deliveryhealth.ClassActorUnauthorized, started, err.Error())
			return nil
		}
		// **以降は authorizeActor が返した本文を使う。** 転送経路では LD-Signature が
		// 覆う内容だけに compact した文書で、生 body には署名外のキー
		// (`_misskey_content` 等) を転送者が足せる。
		body = authorizedBody
		if actor != nil && actor.Host != nil {
			host = *actor.Host
		}
		// **ここまで来て初めて activity.id を信用できる。** 署名が本文を縛って
		// いて、authorizeActor が id の host と actor の host の一致を見た後。
		activityID = federation.ExtractActivityID(body)
		if p.isReplay(activityID) {
			slog.Debug("inbox: activity already processed, dropping replay",
				"host", host, "activityId", activityID)
			p.recordInboxTelemetry(host, deliveryhealth.ClassDuplicate, started, "")
			return nil
		}
		p.touchInstance(actor)
		p.recordSignatureCapability(actor, keyType, payload.Body)
		p.commitChart(actor)
		signer = actor
	} else if p.ldVerifier != nil {
		// legacy / direct-enqueue 経路 (Headers 無し = HTTP 署名者を特定できない)。
		// 署名者照合はできないが、body に LD-Signature があれば従来どおり検証して
		// hardening を効かせる (#1164 Phase D)。fail なら drop。
		//
		// **本番では到達しない。** inbox handler は admitInbox で Signature ヘッダを
		// 必須にしたうえで Headers を詰めて enqueue し (Host は必ず入る)、router は
		// SignatureVerifier を必ず配線する。InboxPayload を作るのもその handler
		// だけ。それでも検証済みとして扱うなら、転送経路と同じく**署名が覆う
		// 内容 (compact 済みの文書) を渡す** — 生 body には署名外のキー
		// (`_misskey_content` 等) を誰でも足せる。
		verified, present, err := p.ldVerifier.VerifyAndCompact(payload.Body)
		if err != nil {
			if errors.Is(err, federation.ErrLookupUnavailable) {
				// 上と同じ (#3121)。creator の鍵は DB から引くので、理由を
				// 見ずに drop すると DB 障害のあいだ届いた activity が消える。
				slog.Error("inbox: cannot verify LD-Signature (lookup unavailable)",
					"host", host, "err", err)
				p.recordInboxTelemetry(host, deliveryhealth.ClassProcessingError, started, err.Error())
				return err
			}
			slog.Warn("inbox: LD-Signature verification failed, dropping activity",
				"host", host, "err", err)
			p.recordInboxTelemetry(host, deliveryhealth.ClassLDSignatureFailed, started, err.Error())
			return nil
		}
		if present {
			body = verified.Body
		}
	}
	if err := p.dispatch(body, signer); err != nil {
		if errors.Is(err, federation.ErrUnsupportedActivity) {
			slog.Debug("inbox: unsupported activity, dropped", "host", host)
			// 異常ではない (相手は正しく送っており、こちらが対応していない
			// だけ)。受理側に数える。
			p.recordInboxTelemetry(host, deliveryhealth.ClassUnsupported, started, "")
			return nil
		}
		p.recordInboxTelemetry(host, deliveryhealth.ClassProcessingError, started, err.Error())
		return fmt.Errorf("process inbox activity: %w", err)
	}
	// **成功してから覚える。** 先に覚えると、処理が落ちてキューが再試行した
	// ときに自分の再試行を再投函として捨ててしまう。
	p.rememberActivity(activityID)
	p.recordInboxTelemetry(host, deliveryhealth.ClassAccepted, started, "")
	return nil
}

// isReplay reports whether this activity was already processed.
//
// guard の障害では**落とさない** (fail-open)。Redis が不調なだけで連合の受信が
// 止まると、再投函を防ぐより害が大きい。
func (p *InboxProcessor) isReplay(activityID string) bool {
	if p.replayGuard == nil || activityID == "" {
		return false
	}
	seen, err := p.replayGuard.Seen(context.Background(), activityID)
	if err != nil {
		slog.Warn("inbox: replay guard degraded, accepting without the check", "err", err)
		return false
	}
	return seen
}

// rememberActivity records a successfully processed activity.
func (p *InboxProcessor) rememberActivity(activityID string) {
	if p.replayGuard == nil || activityID == "" {
		return
	}
	if err := p.replayGuard.Remember(context.Background(), activityID); err != nil {
		slog.Warn("inbox: failed to record processed activity", "activityId", activityID, "err", err)
	}
}

// verifyPayload reconstructs the signed HTTP request from the queued
// payload and validates it against the actor's stored public key.
//
// 復元する request は Method / Path / Headers / Body のみで、TLS / remote
// addr 等は含まれない。HTTP Signature の signing string は Method (大文字
// 小文字混在不可なので handler 側で normalize 済前提)、Path、`(request-
// target)` を含む header 集合だけが必要なので、これで足りる。
// 戻り値の KeyType は検証に成功した鍵の種別で、署名方式の観測記録に使う
// (#2393、err != nil のとき無意味)。
func (p *InboxProcessor) verifyPayload(payload queue.InboxPayload) (*model.User, activitypub.KeyType, error) {
	parsed, err := activitypub.ParseSignatureHeader(payload.Headers["Signature"])
	if err != nil {
		return nil, 0, err
	}
	actorURI := activitypub.ResolveKeyURL(parsed.KeyID)
	actor, err := p.verifier.ResolveActor(actorURI)
	if err != nil {
		return nil, 0, err
	}
	// keyId fragment ベースで Ed25519 / RSA を dispatch する (#1067 / #1070)。
	pem, err := p.verifier.PublicKeyForKeyID(actor.ID, parsed.KeyID)
	if err != nil {
		return nil, 0, err
	}
	req, err := buildSignedRequest(payload)
	if err != nil {
		return nil, 0, err
	}
	// keyCache 経由で verify することで、同一 (keyId, PEM) の x509 パースを
	// worker 横断で 1 回に集約する (#1426)。挙動は VerifyRequest と等価。
	kt, err := p.keyCache.VerifyRequestCached(req, parsed.KeyID, pem)
	if err != nil {
		return nil, 0, err
	}
	return actor, kt, nil
}

// authorizeActor enforces that the HTTP-signature signer is authorized to
// post the given activity, mirroring upstream InboxProcessorService's
// `authUser.user.uri !== getApId(activity.actor)` gate.
//
//   - signer URI == activity.actor  -> accept (the common case). LD-Signature,
//     if present, is still verified for hardening (forbidden directives etc.).
//   - signer URI != activity.actor  -> the activity was forwarded (e.g.
//     Mastodon-style relay). Accept ONLY when a valid LD-Signature
//     authenticates the activity actor: the LD-Signature must be present, must
//     verify, and its creator key must belong to a user whose URI equals
//     activity.actor. Otherwise drop (return error) to block actor spoofing.
//
// authorizeActor gates the activity and returns the identity fields it read,
// so the caller can log **who** was dropped without re-parsing the body
// (#2716 / #2724 review MEDIUM-4)。
//
// It also returns the body the caller must process from here on: the raw body
// when the HTTP signature covers it (signer == actor), or the LD-Signature's
// compacted document when the activity was forwarded, so that properties the
// LD-Signature does not cover never reach the handlers.
func (p *InboxProcessor) authorizeActor(body []byte, signer *model.User) (federation.ActivityFields, []byte, error) {
	// gate は Process と同じ unwrap+Normalize を経た actor を見る必要がある。
	// raw body を直接 parse すると `as:actor` / `{"@id":...}` / 配列 wrap で
	// gate を空 actor にすり抜けさせ、Process だけが本当の actor で動く
	// なりすまし経路が残る (#parity review AUTH-1)。
	//
	// actor / id / type を **1 パス**で読む。個別に取ると同じ body を normalize
	// し直すことになる。
	fields := federation.ExtractActivityFields(body)
	bodyActor := fields.Actor
	if bodyActor == "" {
		// actor 欠落は Process 側 ("activity missing actor") が弾く。ここでは
		// 判定不能なので素通しし、なりすまし対象が無い状態にする。
		return fields, body, nil
	}
	// activity.id host gate (upstream InboxProcessorService): activity.id は文字列
	// 必須で、その host は actor の host と一致しなければならない。これが無いと、
	// 署名検証を通った actor が他 host の id を持つ activity を stamp でき、Announce
	// dedup の FindByURI(act.ID) / renote.URI=act.ID 等で foreign-host id を注入できる
	// (#1779)。署名検証後 (= 認証済み actor) に評価する。
	activityID := fields.ID
	if activityID == "" {
		return fields, nil, fmt.Errorf("activity id is not a string")
	}
	if idHost, actorHost := uriHost(activityID), uriHost(bodyActor); idHost == "" || idHost != actorHost {
		return fields, nil, fmt.Errorf("signerHost != activity.id host: actor=%q id=%q", bodyActor, activityID)
	}
	signerURI := signerURIOf(signer)

	if signerURI != "" && signerURI == bodyActor {
		// #2106 N26: 署名者 == actor かつ HTTP 署名検証済みの通常経路では、upstream
		// InboxProcessorService は LD-Signature を一切検証しない。mk-go は
		// forbidden-directive hardening (JSON-LD term 再定義防御) のみ残し、署名 verify
		// 失敗 (creator 鍵未解決 / legacy LD-sig / normalize 差異) で HTTP 署名済の正規
		// activity を drop しないようにする。
		if p.ldVerifier != nil {
			if err := p.ldVerifier.CheckForbiddenDirectivesIfPresent(body); err != nil {
				return fields, nil, err
			}
			return fields, body, nil
		}
		return fields, body, nil
	}

	// 署名者 != actor: 転送活動の可能性。LD-Signature による actor 認証が必須。
	if p.ldVerifier == nil {
		return fields, nil, fmt.Errorf("actor mismatch and no LD verifier: signer=%q actor=%q", signerURI, bodyActor)
	}
	// LD-Signature の creator (鍵 URI) を body から読む。
	creatorKeyID := extractLDCreator(body)
	if creatorKeyID == "" {
		return fields, nil, fmt.Errorf("actor mismatch and no LD-signature: signer=%q actor=%q", signerURI, bodyActor)
	}
	// 検証より先に creator actor を解決して鍵を取得/永続化する。VerifyAndCompact
	// は FindByKeyID の純粋 DB read で、先に解決しておかないと未知 origin actor
	// からの最初の転送活動が常に drop される (#parity review AUTH-2、upstream
	// getAuthUserFromKeyId は未知 actor を fetch する挙動と整合)。
	ldUser, err := p.resolveLDCreator(activitypub.ResolveKeyURL(creatorKeyID), signer)
	if err != nil {
		return fields, nil, fmt.Errorf("resolve ld-signature creator %q: %w", creatorKeyID, err)
	}
	// 鍵が DB に載った状態で LD-Signature 本体を検証する。
	verified, present, err := p.ldVerifier.VerifyAndCompact(body)
	if err != nil {
		// **creator だけ載せる。** signer / actor は破棄ログの専用属性になったので
		// 重ねる必要が無い。重ねると `LastError.Message` の 200 rune 切り詰めで
		// **原因 (crypto/rsa: verification error) が落ちる** — 診断のための PR が
		// 別の診断面を下げることになる (#2724 review MEDIUM-1)。
		return fields, nil, fmt.Errorf("ld-signature verify failed (creator=%q): %w", creatorKeyID, err)
	} else if !present {
		return fields, nil, fmt.Errorf("actor mismatch and no LD-signature: signer=%q actor=%q", signerURI, bodyActor)
	}
	// LD-Signature の creator (鍵) の owner URI が activity.actor と一致するか
	// 確認する。一致しなければ「自分の鍵で署名したが他人を actor に詐称」した
	// 活動なので drop する。
	ldURI := ""
	if ldUser != nil && ldUser.URI != nil {
		ldURI = *ldUser.URI
	}
	if ldURI == "" || ldURI != bodyActor {
		return fields, nil, fmt.Errorf("ld-signature signer %q != activity.actor %q", ldURI, bodyActor)
	}
	// **以降は compact 済みの文書で判定し直す** (upstream の「もう一度actorチェック」)。
	// 処理に渡すのは verified.Body なので、ゲートが見た actor / id と handler が
	// 見る actor / id を一致させる。生 body の値は creator 解決と早期の足切りに
	// 使っただけで、署名が覆っているのは compact 後の値のほう。
	compactFields := federation.ExtractActivityFields(verified.Body)
	if compactFields.Actor != ldURI {
		return fields, nil, fmt.Errorf("ld-signature signer %q != compacted activity.actor %q", ldURI, compactFields.Actor)
	}
	if idHost := uriHost(compactFields.ID); idHost == "" || idHost != uriHost(compactFields.Actor) {
		return fields, nil, fmt.Errorf("signerHost != compacted activity.id host: actor=%q id=%q", compactFields.Actor, compactFields.ID)
	}
	// **転送された actor 側の連合可否も見る。**
	//
	// ブロック判定 (`isBlocked`) は HTTP 署名者にしか掛かっていなかった。
	// 第三者 (リレー等) が転送すると、`blockedHosts` に入れたホストの actor でも
	// LD-Signature が有効なら処理される。`Announce` / `Like` / `Follow` /
	// `Block` / `Flag` / `Move` / chat がそのまま通り、defederation が素通りする。
	// `resolveActorOnceWithID` は DB に行があれば連合ゲートの手前で返すので、
	// 「一度連合してから defederate した」相手には届く。
	//
	// upstream は署名者側 (`InboxProcessorService.ts:76-80`) と LD-Signature 側
	// (`:207-211`) の 2 箇所で `isFederationAllowedHost` を見る。
	if p.isBlocked(ldUser) {
		return fields, nil, fmt.Errorf("blocked ld-signature actor host: %q", ldURI)
	}
	return compactFields, verified.Body, nil
}

// uriHost returns the normalized hostname of a URI (idnhost.Puny, port
// ignored), or "" when the URI is unparseable or has no host. Used by the
// activity.id host gate (#1779)。
//
// 保存側・連合ゲートと同じ正規化を掛ける。小文字化だけだと、接続先が同じ
// `ｅｖｉｌ.example` と `evil.example` を別 host として弾く (安全側ではあるが、
// 同じ authority の別綴りを同一視する規則が場所ごとに違う状態になる)。
func uriHost(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return idnhost.Puny(u.Hostname())
}

// hasLDSignature reports whether the raw activity body carries a non-null
// `signature` field (= an LD-Signature). 観測記録用の軽量判定で検証はしない
// (#2393)。判定条件は LDSignatureVerifier 側の presence 判定
// (`!hasSig || sigRaw == nil`) と揃えてあるので、「verifier が LD-Sig 有りと
// みなす body」と一致する。
func hasLDSignature(body []byte) bool {
	// LD-Signature 付き activity は少数派なので、まず substring で足切りする。
	// json.Unmarshal は RawMessage 1 field でも document 全体を走査するため、
	// Note の Create のような大きい body では無視できないコストになる。
	// 本文に "signature" を含む活動は false positive になるが、その場合は下の
	// 厳密判定に落ちるだけで結果は変わらない。
	if !bytes.Contains(body, []byte(`"signature"`)) {
		return false
	}
	var probe struct {
		Signature json.RawMessage `json:"signature"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return false
	}
	return len(probe.Signature) > 0 && string(probe.Signature) != "null"
}

// extractLDCreator reads `signature.creator` (the LD-Signature key URI) from a
// raw activity body. Returns "" when there is no LD-Signature or no creator.
func extractLDCreator(body []byte) string {
	var probe struct {
		Signature struct {
			Creator string `json:"creator"`
		} `json:"signature"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Signature.Creator
}

// buildSignedRequest reconstructs an *http.Request that activitypub.
// VerifyRequest can read for HTTP Signature verification. Path は signing
// string の `(request-target)` 構築にだけ使われるため、scheme/host は
// dummy で良い。Body は Digest header の再計算 (verify 側) に使われる。
func buildSignedRequest(payload queue.InboxPayload) (*http.Request, error) {
	method := payload.Method
	if method == "" {
		method = http.MethodPost
	}
	path := payload.Path
	if path == "" {
		path = "/inbox"
	}
	rawURL := "http://placeholder" + path
	if _, err := url.Parse(rawURL); err != nil {
		return nil, fmt.Errorf("invalid path: %w", err)
	}
	req, err := http.NewRequest(method, rawURL, bytes.NewReader(payload.Body))
	if err != nil {
		return nil, err
	}
	for k, v := range payload.Headers {
		req.Header.Set(k, v)
	}
	if host := payload.Headers["Host"]; host != "" {
		req.Host = host
	}
	return req, nil
}

func (p *InboxProcessor) isBlocked(actor *model.User) bool {
	if p.hostBlocker == nil || actor == nil || actor.Host == nil {
		return false
	}
	host := *actor.Host
	if p.hostBlocker.IsBlocked(host) {
		return true
	}
	return !p.hostBlocker.IsAllowed(host)
}

func (p *InboxProcessor) touchInstance(actor *model.User) {
	if p.instanceTracker == nil || actor == nil || actor.Host == nil {
		return
	}
	_ = p.instanceTracker.MarkRequestReceived(*actor.Host)
}

// recordSignatureCapability is a best-effort hook that records which signature
// scheme the remote host used (#2393). recorder 未設定 / actor がローカル /
// Host nil の場合は no-op。
//
// 署名検証と host block と actor 認可をすべて通った後にだけ呼ぶ。未検証の経路
// (Headers 無しの direct-enqueue) から記録すると、送信元を確かめずに任意 host の
// 観測を書けてしまう。
func (p *InboxProcessor) recordSignatureCapability(actor *model.User, kt activitypub.KeyType, body []byte) {
	if p.capabilities == nil || actor == nil || actor.Host == nil {
		return
	}
	host := *actor.Host
	p.capabilities.ObserveInboundAlg(host, activitypub.KeyTypeName(kt))
	if hasLDSignature(body) {
		p.capabilities.ObserveLDSignature(host)
	}
}

func (p *InboxProcessor) commitChart(actor *model.User) {
	if p.chartHook == nil || actor == nil || actor.Host == nil {
		return
	}
	p.chartHook.OnInboxReceived(*actor.Host)
}

// dispatch hands the activity to the federation processor, passing the
// verified HTTP signer when the processor accepts it (#2338).
func (p *InboxProcessor) dispatch(body []byte, signer *model.User) error {
	if sp, ok := p.processor.(SignerAwareProcessor); ok {
		return sp.ProcessWithSigner(body, signer)
	}
	return p.processor.Process(body)
}

// resolveLDCreator resolves the LD-Signature creator, marking it as
// relay-derived when a subscribed relay delivered the activity (#2340).
//
// 転送活動の署名検証には creator の公開鍵が要り、`VerifyAndCompact` は DB read
// なので先に解決して載せる必要がある。この経路は ephemeral 化できないため、
// 印を付けて後追いの孤児掃除で回収する。
//
// 既に DB に在る行には印を付けない (ResolveActorViaRelay は新規作成時のみ立てる)。
func (p *InboxProcessor) resolveLDCreator(uri string, signer *model.User) (*model.User, error) {
	if p.relayChecker != nil && signer != nil && p.relayChecker.IsRelayActor(signer) {
		if rv, ok := p.verifier.(RelayAwareVerifier); ok {
			return rv.ResolveActorViaRelay(uri)
		}
	}
	return p.verifier.ResolveActor(uri)
}

// HasSignatureVerifier reports whether the inbound signature verifier is wired.
//
// 未配線だと Handle の `len(payload.Headers) > 0 && p.verifier != nil` が偽になり、
// **署名検証・連合ブロック・actor 一致 (authorizeActor)・replay 判定が
// まとめて飛ぶ**。本番では必ず配線されるので、起動時にここを検査する (#2682)。
//
// **この検査が見るのは「4 つがまとめて飛ぶ」場合だけ。** 連合ブロック・replay・
// LD-Signature は `SetHostBlockChecker` / `SetInboxReplayGuard` /
// `SetLDSignatureVerifier` という独立の setter も持っており、そちらだけを
// 消すと本検査は通ったまま個別に無効化できる。replay 側は
// [InboxProcessor.HasInboxReplayGuard] で個別に検査する。連合ブロック側は
// moderation の tier なので #2683。LD-Signature は大半が fail-closed
// (signer != actor で error) だが、JSON-LD の term 再定義検査
// (CheckForbiddenDirectivesIfPresent) と Headers 無し経路の検証が黙って
// 飛ぶ (#2682 review L-E)。
func (p *InboxProcessor) HasSignatureVerifier() bool { return p.verifier != nil }

// HasInboxReplayGuard reports whether the inbound activity replay guard is
// wired.
//
// 未配線だと isReplay が常に false を返す (fail-open)。**一度処理した
// activity を何度でも再投函できる**。各 activity handler は冪等な前提で
// 書かれているので単純な二重適用にはならないが、冪等性は handler ごとの
// 性質であって保証ではなく、Date skew の窓の内側で好きなだけ再送できる
// 状態は一回性の tier そのもの。signin / i の TOTP guard を検査しながら
// inbox の replay guard だけ外すのは筋が通らないので載せる
// (#2682 review M-4)。
func (p *InboxProcessor) HasInboxReplayGuard() bool { return p.replayGuard != nil }

// HasHostBlockChecker reports whether the host block checker was wired.
//
// 未配線だと `isBlocked` が常に false になり、ブロック済み host / 許可外 host
// からの activity を受け入れる。起動時検査に使う (#2683)。
func (p *InboxProcessor) HasHostBlockChecker() bool { return p.hostBlocker != nil }

// signerURIOf returns the signer's actor URI, or "" when unresolved.
//
// 破棄ログ用。nil / URI 未設定でも空文字にするだけで、判定には使わない (#2716)。
func signerURIOf(signer *model.User) string {
	if signer == nil || signer.URI == nil {
		return ""
	}
	return *signer.URI
}
