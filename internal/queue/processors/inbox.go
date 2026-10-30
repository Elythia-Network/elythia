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
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/deliveryhealth"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/idnhost"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
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

// NormalizedActivityProcessor is an optional extension that accepts the
// activity the inbox gate already normalized (#3610).
//
// gate は actor / id を読むために本文を Normalize しているので、同じ結果を
// 渡して処理側でもう一度 Normalize しない。未実装の processor (テストの fake)
// には今どおり本文を渡す。
type NormalizedActivityProcessor interface {
	ProcessNormalized(activity federation.NormalizedActivity, signer *model.User) error
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

// SoftwareSuspensionChecker reports whether activities from a host must be
// refused because the instance runs software listed in
// meta.inboxSuspendedSoftware (Elythia 独自、#3362)。実装は
// core/federation.NewInboxSuspendedChecker。
type SoftwareSuspensionChecker interface {
	IsSuspended(host string) bool
}

// InstanceTracker is invoked after a successfully verified inbound
// activity so that the instance row's latestRequestReceivedAt can be
// updated. nil なら no-op。
type InstanceTracker interface {
	MarkRequestReceived(host string) error
}

// InstanceMetadataRefresher refetches a remote instance's nodeinfo in the
// background when it is older than 24 hours (#3564). 実装は
// core/instance.MetadataRefresher。
type InstanceMetadataRefresher interface {
	MaybeRefresh(host string)
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
	processor FederationProcessor
	// verifier は受信の HTTP 署名を worker で検証し直す。**無くても payload は落ちない。**
	// Handle の `len(payload.Headers) > 0 && p.verifier != nil` が偽になり、署名検証・
	// 連合ブロック・actor 一致 (authorizeActor)・replay 判定を**まとめて飛ばしたまま
	// dispatch へ進む** (#2682)。連合ブロック・replay・LD-Signature はそれぞれ別の依存も
	// 持つので、どれか 1 つだけを外しても個別に無効化される。LD-Signature の検証器
	// (SetLDSignatureVerifier) は setter のまま残っていて、外すと大半は fail-closed
	// (signer != actor で error) だが、JSON-LD の term 再定義検査
	// (CheckForbiddenDirectivesIfPresent) と Headers 無しの経路の検証が黙って飛ぶ
	// (#2682 review L-E)。
	verifier   SignatureVerifier
	ldVerifier LDSignatureVerifier
	// relayChecker は LD-Signature 検証で解決する著者に「リレー由来」の印を
	// 付けるかどうかの判定に使う (#2340)。
	relayChecker RelayActorChecker
	// hostBlocker は署名検証の後で、連合をブロックした host / 許可外の host からの
	// activity を弾く。無いと isBlocked が常に false になる (#2683)。
	hostBlocker HostBlockChecker
	// softwareBlocker は meta.inboxSuspendedSoftware の判定。設定は判定のたびに読む
	// ので、設定が空でも常に渡す (#3362)。
	softwareBlocker SoftwareSuspensionChecker
	instanceTracker InstanceTracker
	// metadataRefresher は受信のたびに相手の nodeinfo を 24 時間より古ければ
	// 取り直す (#3564)。未配線なら取り直さない。
	metadataRefresher InstanceMetadataRefresher
	chartHook         InboxChartHook
	capabilities      SignatureCapabilityRecorder
	// telemetry は受信結果をホストごとに記録する (#2471)。未配線なら記録
	// しないだけで、inbox 処理そのものには影響しない。
	telemetry DeliveryTelemetry
	// replayGuard は処理済み activity を覚えて再投函を弾く。未配線だと isReplay が
	// 常に false を返し (fail-open)、**一度処理した activity を Date skew の窓の内側で
	// 何度でも再投函できる**。各 activity handler は冪等な前提で書かれているが、
	// 冪等性は handler ごとの性質であって保証ではない。signin / i の TOTP guard と
	// 同じ一回性の tier なので、構築子の必須の引数にした (#2682 review M-4、#3829)。
	//
	// **署名検証を通った経路にしか効かせない。** activity.id は body の値なので、
	// 署名で本文が縛られていて、かつ authorizeActor が id の host と actor の host の
	// 一致を確かめた後でなければ信用できない。信用できない id を覚えると、他人の
	// activity id を先に登録して**本物を落とす**ことができてしまう。
	replayGuard InboxReplayGuard
	// keyCache は受信 HTTP Signature verify の公開鍵パースをメモ化する (#1426)。
	// InboxProcessor は worker 間共有の単一インスタンス (router.go:616) なので、
	// 同一 remote actor からの連続 activity で x509 パースを 1 回に集約できる
	// (#1436 の DeliverProcessor.keyCache と対になる inbound 側最適化)。
	keyCache *activitypub.PublicKeyCache
}

// NewInboxProcessor constructs an InboxProcessor wrapping the supplied
// federation.Processor.
//
// verifier re-verifies the inbound HTTP signature in the worker. hostBlocker
// rejects activities from federation-blocked hosts after signature
// verification. softwareBlocker is the checker for meta.inboxSuspendedSoftware
// (#3362). metadataRefresher refetches the sender's nodeinfo in the background
// (#3564). replayGuard drops re-posted activities. The remaining dependencies
// (LD-Signature verifier, relay checker, instance tracker, chart hook,
// telemetry, capability recorder) are wired with setters; an unwired one is a
// no-op.
func NewInboxProcessor(p FederationProcessor, verifier SignatureVerifier, hostBlocker HostBlockChecker, softwareBlocker SoftwareSuspensionChecker, metadataRefresher InstanceMetadataRefresher, replayGuard InboxReplayGuard) *InboxProcessor {
	return &InboxProcessor{
		processor:         p,
		verifier:          verifier,
		hostBlocker:       hostBlocker,
		softwareBlocker:   softwareBlocker,
		metadataRefresher: metadataRefresher,
		replayGuard:       replayGuard,
		keyCache:          activitypub.NewPublicKeyCache(0),
	}
}

// SetLDSignatureVerifier wires an optional LD-Signature verifier that runs
// after HTTP Signature verify but before activity dispatch. signature 無し
// activity は skip され、verify fail なら activity を drop (return nil で
// queue を ack するが Process は呼ばない)。
func (p *InboxProcessor) SetLDSignatureVerifier(v LDSignatureVerifier) {
	p.ldVerifier = v
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
	// replayKey は再投函ガードの鍵 (署名が覆った本文のダイジェスト)。activityID と
	// 同じく、署名検証と actor の認可を通った経路でだけ埋まる。
	replayKey := ""
	// body は dispatch する本文。転送経路では authorizeActor が署名済みの内容へ
	// 差し替える。
	body := payload.Body
	// normalized は body を NormalizeActivity にかけた結果で、authorizeActor を
	// 通った経路でだけ埋まる (#3610)。埋まっていれば dispatch はこちらを渡し、
	// 処理側で同じ本文を正規化し直さない。body と必ず同時に差し替える。
	var normalized *federation.NormalizedActivity
	// **署名を検証する前に、keyId のホストで受信の拒否を見る (#3362)。** 検証は
	// 署名者の actor を解決して保存し、プロフィールや nodeinfo も取りに行くので、
	// 拒否する相手の分まで行と通信が増える。ドメインブロックが resolver の手前で
	// 止まるのと同じく、先に止める。keyId は検証前で信用できない値だが、拒否の
	// 向きにしか使わないので、偽れば送った本人の要求が落ちるだけ。当たらなければ
	// 検証の後に DB で正規化したホストでもう一度見る (isBlocked)。
	if keyHost := strings.ToLower(host); len(payload.Headers) > 0 && p.softwareBlocker != nil && keyHost != "" && p.softwareBlocker.IsSuspended(keyHost) {
		// 記録も小文字にしたホストで残す (大文字混じりの keyId で別のホストとして
		// 数えないため)。
		p.recordInboxTelemetry(keyHost, deliveryhealth.ClassBlocked, started, "")
		// **拒否した相手の nodeinfo も取り直す (#3564)。** 拒否の判定は保存済みの
		// softwareVersion で行うので、ここで取り直さないと、相手が版を上げても
		// 拒否が続く (受信で状態を戻す MarkRequestReceived はこの後にあり、届かない)。
		// keyId は未検証なので、偽れば第三者が、登録済みで受信を拒否している
		// ホストへの nodeinfo の取得を起こせる。取りに行くのは登録済みの相手だけで、
		// 1 ホスト 1 日 1 回までに間引く。
		p.refreshMetadata(keyHost)
		return nil
	}
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
				// 受信を拒否するソフトウェアで止めた相手は取り直す (#3564)。
				// ドメインブロックの相手は refreshMetadata が除く。
				p.refreshMetadata(host)
			}
			p.recordInboxTelemetry(host, deliveryhealth.ClassBlocked, started, "")
			return nil
		}
		// HTTP 署名者 (actor) が activity body の actor と一致することを保証する
		// (actor spoofing 対策)。一致しない場合は転送活動とみなし LD-Signature が
		// body actor を認証している場合のみ許可する。LD-Signature の hardening
		// (forbidden directive 等) も本 gate に集約する。
		authorized, err := p.authorizeActor(payload.Body, actor)
		fields := authorized.Fields()
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
		body = authorized.Source()
		normalized = &authorized
		if actor != nil && actor.Host != nil {
			host = *actor.Host
		}
		// **ここまで来て初めて activity.id を信用できる。** 署名が本文を縛って
		// いて、authorizeActor が actor を読め、id の host と actor の host の一致を
		// 見た後。
		//
		// id は authorizeActor が読んだものをそのまま使う (#3610)。authorized は
		// 処理に渡す本文 (転送経路では compact 済みの文書) から読んだ値なので、
		// 以前の ExtractActivityID(body) と同じ値で、正規化をもう 1 回しない。
		activityID = fields.ID
		// **再投函の鍵は署名が覆った Date と本文。** id だけを鍵にすると、同じ id で
		// 中身の違う活動を 2 つ目から捨てる。本家 Misskey の Undo(Follow) は
		// `<Follow の id>/undo` の決定的な id で `published` だけが変わるので、
		// 15 分以内の 2 回目のフォロー解除が届かず、フォロワーのまま残っていた
		// (2026-10-31 の監査)。本文だけでも、Elythia 自身の Follow (決定的な id で
		// `published` 無し) の再フォローを捨てる。再投函は捕まえた署名付きリクエストを
		// そのまま投げ直す形なので、署名された Date を足せば過不足なく捕まえられる。
		replayKey = activityReplayKey(payload.Headers["Date"], payload.Body)
		if p.isReplay(replayKey) {
			slog.Debug("inbox: activity already processed, dropping replay",
				"host", host, "activityId", activityID)
			p.recordInboxTelemetry(host, deliveryhealth.ClassDuplicate, started, "")
			return nil
		}
		p.touchInstance(actor)
		if actor != nil && actor.Host != nil {
			p.refreshMetadata(*actor.Host)
		}
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
	if err := p.dispatch(body, normalized, signer); err != nil {
		if errors.Is(err, federation.ErrInboundLocalReferenceUnavailable) {
			slog.Debug("inbox: unavailable local reference, dropped", "host", host)
			p.recordInboxTelemetry(host, deliveryhealth.ClassUnsupported, started, "")
			return nil
		}
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
	p.rememberActivity(replayKey, activityID)
	p.recordInboxTelemetry(host, deliveryhealth.ClassAccepted, started, "")
	return nil
}

// isReplay reports whether a request with this replay key was already processed.
//
// guard の障害では**落とさない** (fail-open)。Redis が不調なだけで連合の受信が
// 止まると、再投函を防ぐより害が大きい。
func (p *InboxProcessor) isReplay(replayKey string) bool {
	if p.replayGuard == nil || replayKey == "" {
		return false
	}
	seen, err := p.replayGuard.Seen(context.Background(), replayKey)
	if err != nil {
		slog.Warn("inbox: replay guard degraded, accepting without the check", "err", err)
		return false
	}
	return seen
}

// rememberActivity records a successfully processed request by its replay key.
// activityID is only for the log.
func (p *InboxProcessor) rememberActivity(replayKey, activityID string) {
	if p.replayGuard == nil || replayKey == "" {
		return
	}
	if err := p.replayGuard.Remember(context.Background(), replayKey); err != nil {
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
// On success the returned activity is the one the caller must process from
// here on (its Source is the body): the raw body when the HTTP signature
// covers it (signer == actor), or the LD-Signature's compacted document when
// the activity was forwarded, so that properties the LD-Signature does not
// cover never reach the handlers. On error it is the raw body's, for logging.
func (p *InboxProcessor) authorizeActor(body []byte, signer *model.User) (federation.NormalizedActivity, error) {
	// gate は Process と同じ unwrap+Normalize を経た actor を見る必要がある。
	// raw body を直接 parse すると `as:actor` / `{"@id":...}` / 配列 wrap で
	// gate を空 actor にすり抜けさせ、Process だけが本当の actor で動く
	// なりすまし経路が残る (#parity review AUTH-1)。
	//
	// actor / id / type を **1 パス**で読む。個別に取ると同じ body を normalize
	// し直すことになる。その結果は処理にもそのまま渡す (#3610)。
	//
	// **署名に関わる検査 (CheckForbiddenDirectivesIfPresent / extractLDCreator /
	// VerifyAndCompact) には生の body を渡す。** raw.Source() は body と同じ
	// バイト列だが、正規化した結果 (act.raw) を渡してはいけない。正規化は
	// @context を落とし、キーを書き換える。
	raw := federation.NormalizeActivity(body)
	fields := raw.Fields()
	bodyActor := fields.Actor
	if bodyActor == "" {
		// **actor の読めない本文はここで落とす (upstream InboxProcessorService と
		// 同じ)。** 以前は「Process 側が弾く」として素通ししていたが、素通しした
		// 本文は下の id の host の検査も受けずに呼び出し元へ戻り、Process が actor
		// 無しを成功として捨てるので、**他ホストの任意の id が再投函ガードに
		// 覚えられた**。有効な鍵を持つ任意のリモートが `"actor": ""` の本文に
		// 被害者の将来の id を付けて 1 本送るだけで、本物の活動が重複として
		// 捨てられる (2026-10-31 の監査)。
		return raw, fmt.Errorf("activity has no actor")
	}
	// activity.id host gate (upstream InboxProcessorService): activity.id は文字列
	// 必須で、その host は actor の host と一致しなければならない。これが無いと、
	// 署名検証を通った actor が他 host の id を持つ activity を stamp でき、Announce
	// dedup の FindByURI(act.ID) / renote.URI=act.ID 等で foreign-host id を注入できる
	// (#1779)。署名検証後 (= 認証済み actor) に評価する。
	activityID := fields.ID
	if activityID == "" {
		return raw, fmt.Errorf("activity id is not a string")
	}
	if idHost, actorHost := uriHost(activityID), uriHost(bodyActor); idHost == "" || idHost != actorHost {
		return raw, fmt.Errorf("signerHost != activity.id host: actor=%q id=%q", bodyActor, activityID)
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
				return raw, err
			}
			return raw, nil
		}
		return raw, nil
	}

	// 署名者 != actor: 転送活動の可能性。LD-Signature による actor 認証が必須。
	if p.ldVerifier == nil {
		return raw, fmt.Errorf("actor mismatch and no LD verifier: signer=%q actor=%q", signerURI, bodyActor)
	}
	// LD-Signature の creator (鍵 URI) を body から読む。
	creatorKeyID := extractLDCreator(body)
	if creatorKeyID == "" {
		return raw, fmt.Errorf("actor mismatch and no LD-signature: signer=%q actor=%q", signerURI, bodyActor)
	}
	// 検証より先に creator actor を解決して鍵を取得/永続化する。VerifyAndCompact
	// は FindByKeyID の純粋 DB read で、先に解決しておかないと未知 origin actor
	// からの最初の転送活動が常に drop される (#parity review AUTH-2、upstream
	// getAuthUserFromKeyId は未知 actor を fetch する挙動と整合)。
	ldUser, err := p.resolveLDCreator(activitypub.ResolveKeyURL(creatorKeyID), signer)
	if err != nil {
		return raw, fmt.Errorf("resolve ld-signature creator %q: %w", creatorKeyID, err)
	}
	// 鍵が DB に載った状態で LD-Signature 本体を検証する。
	verified, present, err := p.ldVerifier.VerifyAndCompact(body)
	if err != nil {
		// **creator だけ載せる。** signer / actor は破棄ログの専用属性になったので
		// 重ねる必要が無い。重ねると `LastError.Message` の 200 rune 切り詰めで
		// **原因 (crypto/rsa: verification error) が落ちる** — 診断のための PR が
		// 別の診断面を下げることになる (#2724 review MEDIUM-1)。
		return raw, fmt.Errorf("ld-signature verify failed (creator=%q): %w", creatorKeyID, err)
	} else if !present {
		return raw, fmt.Errorf("actor mismatch and no LD-signature: signer=%q actor=%q", signerURI, bodyActor)
	}
	// LD-Signature の creator (鍵) の owner URI が activity.actor と一致するか
	// 確認する。一致しなければ「自分の鍵で署名したが他人を actor に詐称」した
	// 活動なので drop する。
	ldURI := ""
	if ldUser != nil && ldUser.URI != nil {
		ldURI = *ldUser.URI
	}
	if ldURI == "" || ldURI != bodyActor {
		return raw, fmt.Errorf("ld-signature signer %q != activity.actor %q", ldURI, bodyActor)
	}
	// **以降は compact 済みの文書で判定し直す** (upstream の「もう一度actorチェック」)。
	// 処理に渡すのは verified.Body なので、ゲートが見た actor / id と handler が
	// 見る actor / id を一致させる。生 body の値は creator 解決と早期の足切りに
	// 使っただけで、署名が覆っているのは compact 後の値のほう。
	compact := federation.NormalizeActivity(verified.Body)
	compactFields := compact.Fields()
	if compactFields.Actor != ldURI {
		return raw, fmt.Errorf("ld-signature signer %q != compacted activity.actor %q", ldURI, compactFields.Actor)
	}
	if idHost := uriHost(compactFields.ID); idHost == "" || idHost != uriHost(compactFields.Actor) {
		return raw, fmt.Errorf("signerHost != compacted activity.id host: actor=%q id=%q", compactFields.Actor, compactFields.ID)
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
		return raw, fmt.Errorf("blocked ld-signature actor host: %q", ldURI)
	}
	return compact, nil
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
// Path は handler が積む RequestURI (query 込み、空の `?` も残る) で、
// url.Parse がそのまま RawQuery / ForceQuery に戻すので RequestURI() で同じ
// 文字列が再現される。古い payload は query を持たない path だけなので互換。
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
	if !p.hostBlocker.IsAllowed(host) {
		return true
	}
	// **受信を拒否するソフトウェア (#3362)。** ドメインブロックと同じ場所で見る
	// ので、HTTP 署名の署名者と、LD 署名で転送された actor の両方に効き、
	// Delete / Undo も含めて拒否する。ソフトウェアが分からない相手は拒否しない
	// (配送停止と同じ)。
	return p.softwareBlocker != nil && p.softwareBlocker.IsSuspended(host)
}

// refreshMetadata schedules a background nodeinfo refetch for host unless the
// host is domain-blocked or outside the federation allowlist (#3564).
//
// 本家はドメインブロックした相手には受信でも配送でも取りに行かない
// (InboxProcessorService はブロックの判定で return した後に
// fetchInstanceMetadata を呼ぶ)。受信を拒否するソフトウェアは Elythia 独自で、
// 判定を更新するために取り直す。
func (p *InboxProcessor) refreshMetadata(host string) {
	if p.metadataRefresher == nil || host == "" {
		return
	}
	if p.hostBlocker != nil && (p.hostBlocker.IsBlocked(host) || !p.hostBlocker.IsAllowed(host)) {
		return
	}
	p.metadataRefresher.MaybeRefresh(host)
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
//
// normalized, when non-nil, must be NormalizeActivity(body); the processor
// then reuses it instead of normalizing body again (#3610).
func (p *InboxProcessor) dispatch(body []byte, normalized *federation.NormalizedActivity, signer *model.User) error {
	if normalized != nil {
		if np, ok := p.processor.(NormalizedActivityProcessor); ok {
			return np.ProcessNormalized(*normalized, signer)
		}
	}
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

// signerURIOf returns the signer's actor URI, or "" when unresolved.
//
// 破棄ログ用。nil / URI 未設定でも空文字にするだけで、判定には使わない (#2716)。
func signerURIOf(signer *model.User) string {
	if signer == nil || signer.URI == nil {
		return ""
	}
	return *signer.URI
}
