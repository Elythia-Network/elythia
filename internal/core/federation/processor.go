package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/shiroha-a/mk/internal/activitypub"
	coreblocking "github.com/shiroha-a/mk/internal/core/blocking"
	corechat "github.com/shiroha-a/mk/internal/core/chat"
	corefollowing "github.com/shiroha-a/mk/internal/core/following"
	corenote "github.com/shiroha-a/mk/internal/core/note"
	"github.com/shiroha-a/mk/internal/core/notification"
	corereaction "github.com/shiroha-a/mk/internal/core/reaction"
	corereversi "github.com/shiroha-a/mk/internal/core/reversi"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/repository"
)

// nowFn is the time source used by Processor when generating new note IDs.
// テストでの差し替えを容易にするため変数として保持する。
var nowFn = time.Now

// Errors returned by Processor.
var (
	// ErrUnsupportedActivity is returned when an activity type cannot be handled.
	ErrUnsupportedActivity = errors.New("unsupported activity type")
)

// RelayStatusMarker is the minimal interface needed from core/relay.Service
// for the inbox processor to toggle relay status on Accept / Reject
// activities whose id matches `{baseURL}/activities/follow-relay/{id}`.
type RelayStatusMarker interface {
	MarkAccepted(ctx context.Context, id string) error
	MarkRejected(ctx context.Context, id string) error
	// FindByID returns the relay row identified by id, or an error when no
	// such row exists.
	//
	// status を書き換える前に「送信元 actor がその relay 自身か」を確かめる
	// ために要る。行が持っている identity は inbox URI だけなので、
	// 突き合わせはその host と act.actor の host で行う
	// (relayActorOwnsRelay)。
	FindByID(ctx context.Context, id string) (*model.Relay, error)
}

// RelayActorChecker reports whether a given remote user is one of the locally
// registered relays. Used by handleAnnounce to detect relay-delivered Announce
// activities (upstream Misskey #17308 / triage #1002) so they get published to
// timeline streams as direct notes rather than as renotes.
//
// 実装は core/relay.Service。同 service は RelayStatusMarker と
// RelayActorChecker 両方を満たすが、用途が独立しているので interface も分離。
type RelayActorChecker interface {
	IsRelayActor(actor *model.User) bool
}

// Processor dispatches inbound activities to the right handler.
type Processor struct {
	resolver         *Resolver
	followingService *corefollowing.Service
	reactionService  *corereaction.Service
	noteDeleteSvc    *corenote.DeleteService
	userRepo         repository.UserRepository
	noteRepo         repository.NoteRepository

	// Block/Flag/Move/Add/Remove federation hooks.
	// SetBlockingService等で注入。nilの場合は対応activityがErrUnsupportedActivityを返す。
	blockingService *coreblocking.Service
	abuseReportRepo repository.AbuseReportRepository
	abuseIDGen      id.Generator
	// abuseModeratorLister / abuseInAppNotifier はリモートからの通報 (AP Flag)
	// をモデレーターの通知欄に出す (#2868)。local の report-abuse と同じ扱いに
	// するためで、片方でも nil なら通知しない。
	//
	// **admin stream と system webhook はこの経路では飛んでいない** (local 側の
	// #1549 / #1542 が users handler にしかない既存の穴)。ここで in-app だけを
	// 足すのは、通報の出どころで通知の有無が変わる非対称を作らないため。
	abuseModeratorLister AbuseModeratorLister
	abuseInAppNotifier   AbuseInAppNotifier
	pinningRepo          repository.UserNotePiningRepository
	pinningIDGen         id.Generator

	// Reversi federation hooks (Phase 9.7). All four are set via
	// SetReversi; if nil, reversi inbox types are treated as unsupported.
	reversiSvc      *corereversi.Service
	reversiRepo     repository.ReversiRepository
	reversiIDGen    id.Generator
	reversiFedCache *corereversi.FederationIDCache
	// reversiStreamPub: 招待受信時に recipient の `reversi:<id>` stream
	// に `invited` イベントを publish する (#417 P2 リアルタイム招待)。
	reversiStreamPub ReversiStreamPublisher

	// Relay follow-accept / -reject hook. nil 時は relay 特化処理をスキップ。
	relayMarker RelayStatusMarker

	// Relay actor 判定 hook (#1002 / upstream #17308)。nil 時は全 Announce が
	// 従来通り renote として処理される。
	relayActorChecker RelayActorChecker

	// Chat federation (CherryPick互換)
	chatService ChatMessageReceiver
	// chatRoomReceiver は group chat (room) federation の inbound 操作
	// (room copy / invitation / membership) を担う (#1203)。
	chatRoomReceiver ChatRoomReceiver
	// accountDeleteEnqueuer は inbound actor self-delete 時に remote user の
	// notes / drive / following を cascade purge する job を enqueue する
	// (#1220)。nil の場合は tombstone (isDeleted=true) のみで purge は行わない。
	accountDeleteEnqueuer AccountDeleteEnqueuer

	// Timeline fanout hook for remote notes (#330). ローカルノートは
	// noteCreateService経由でfanoutされるが、リモートノートはIngestNote/
	// handleAnnounce経由でDB直挿入されるためここで明示的にfanoutする。
	fanoutHook TimelineFanoutHook
	// Antenna hook for inbound Create / Announce (#2743)。リレー経由では
	// 発火させない (AntennaHook の doc を参照)。
	antennaHook AntennaHook

	// Notification hook for inbound Create / Announce (#415)。fanoutHook と
	// 同じ位置で呼び出して reply / renote / quote / mention 通知を生成する。
	// 配線されていなければ no-op。
	notificationHook NotificationHook

	// Note chart hook for inbound Create / Announce (#1156). ローカル作成は
	// note_create_service.go から chartHook.OnNoteCreated を発火させているが、
	// federation 経由のリモートノートは handleCreate / handleAnnounce で直接
	// note を作るため、本フィールド経由で同じ chart hook を発火させないと
	// PerUserNotesChart 等の inc 列が +1 されない (= プロフィールの
	// アクティビティタブの heatmap がリモートユーザーだけ空になる)。
	// noteChartHook は idempotent ではないので、dedup hit (= 既存ノートを返した
	// ケース) では発火させない (IngestNoteWithCreated の created==false 時は
	// skip)。resolver の ChartHook (= OnRemoteUserCreated) とは責務が異なるので
	// 別 interface として分離する。
	noteChartHook NoteChartHook

	// localBaseURL はローカルユーザーのcanonical URI prefix
	// (例: "https://go.k7a.org") を保持する。inboxで受信したactivityの
	// object が "{localBaseURL}/users/{id}" 形式のとき、ローカルユーザーを
	// ID で lookup するために使用する (ローカルユーザーの user.uri は NULL
	// なので FindByURI では解決できない)。
	localBaseURL string

	// inboundFollowAcceptor は inbound Follow に対して Accept activity を
	// 送り返す。original Follow のID (リモート側のURL) を参照しないと相手が
	// Acceptをマッチングできないため、processorが受け取った raw activity を
	// そのまま Accept の object に包んで送る。nil なら何もしない (locked
	// follow 経路ではここを通らないのでOK)。
	inboundFollowAcceptor InboundFollowAcceptor
}

// InboundFollowAcceptor delivers an Accept or Reject activity in response to
// an inbound Follow. The raw Follow (as received on our inbox) is wrapped as
// the inner object so the remote server can match the response to the
// original Follow id.
type InboundFollowAcceptor interface {
	SendAcceptForInboundFollow(follower, followee *model.User, originalFollow json.RawMessage) error
	// SendRejectForInboundFollow is used when the local followee has blocked
	// the remote follower (#1631、upstream は error でなく Reject を返す)。
	SendRejectForInboundFollow(follower, followee *model.User, originalFollow json.RawMessage) error
}

// SetInboundFollowAcceptor wires the sender that delivers Accept activities
// for inbound Follow activities targeting local users.
func (p *Processor) SetInboundFollowAcceptor(a InboundFollowAcceptor) {
	p.inboundFollowAcceptor = a
}

// SetLocalBaseURL configures the local instance's base URL. This is used to
// detect whether an inbound activity's object refers to a local user.
func (p *Processor) SetLocalBaseURL(baseURL string) {
	p.localBaseURL = baseURL
}

// HasLocalBaseURL reports whether the local base URL was wired.
//
// **未配線だと静かに壊れる。** relay の Accept / Reject は activity id が
// 自ホストの URI であることを要求する (fail-closed) ので、ここが空だと
// relay の状態遷移が全て落ちる。ローカル URI の判定も全部外れる。
func (p *Processor) HasLocalBaseURL() bool { return p.localBaseURL != "" }

// resolveTargetUser looks up a user referenced by URI in an inbound activity.
// ローカルユーザの user.uri は DB 上 NULL なので FindByURI では解決できない。
// 対策として localBaseURL 配下の URI ("{baseURL}/users/{id}") を検出し、
// ID パートを抜き出して FindByID で lookup する。リモートユーザーは従来通り
// FindByURI で解決する。
func (p *Processor) resolveTargetUser(uri string) (*model.User, error) {
	if p.localBaseURL != "" {
		prefix := p.localBaseURL + "/users/"
		if id, ok := strings.CutPrefix(uri, prefix); ok {
			// id の中に "/" が残る (例: "/outbox") ケースがあるので切り落とす。
			if slash := strings.IndexByte(id, '/'); slash >= 0 {
				id = id[:slash]
			}
			if id != "" {
				return p.userRepo.FindByID(id)
			}
		}
	}
	return p.userRepo.FindByURI(uri)
}

// ChatMessageReceiver handles inbound Misskey:ChatMessage activities.
type ChatMessageReceiver interface {
	// mfmSource は相手が併記した MFM の原文 (`source` / `_misskey_content`)。
	// 空なら text (HTML) から戻す。
	CreateMessageViaAP(ctx context.Context, uri string, fromUser *model.User, toUserID, text, mfmSource string) (*model.ChatMessage, error)
}

// Hook mutation contract (TimelineFanoutHook / NotificationHook / NoteChartHook):
//
// 以下 3 interface の OnNoteCreated は handleCreate / handleAnnounce 経路で
// safeGoFedHook 経由の **並行 goroutine** から発火される (#569 / #1156 / #1158)。
// 3 hook は同じ `*model.Note` ポインタを共有して並行実行されるため、
// 実装側は渡された note (および各種 relation: Reply / Renote / User 等) を
// **read-only として扱うこと**。
//
//   - 渡された note や relation を mutate しない (例: `note.User = ...` 禁止)
//   - field 値の保持が必要なら呼び出し側でコピーを作る (`localUser := *note.User`
//     等)
//   - 集計値の Inc など hook 内 DB 書き込みは note 自体の値変更を伴わない限り OK
//
// 同期発火だった頃 (#569 / #1158 以前) は read-only 契約が暗黙だったが、
// 並行発火後は契約違反が data race として顕在化するため明示する。違反疑いがある
// ときは `-race` test で検出可能 (= queue-bench / processor_test のいずれかが
// CONCURRENT MAP READ AND WRITE 等で落ちる)。

// TimelineFanoutHook is invoked after a remote note has been persisted so
// that timeline/streaming subscribers receive the note. パッケージ間の循環
// 依存を避けるためinterfaceで受け取る (実装は core/timeline.FanoutHook)。
//
// note / author は read-only。詳細は本ファイル上部の "Hook mutation contract"。
type TimelineFanoutHook interface {
	OnNoteCreated(note *model.Note, author *model.User)
}

// AntennaHook is invoked after an inbound Create / Announce so that remote
// notes reach their subscribers' antennas (#2743). ローカル作成と同じ
// interface を使う (実装は core/antenna.NoteCreateHook)。
//
// **リレー経由の note では発火させない。**
//
// `enableEphemeralRelayNotes` (既定 false) が有効なとき、リレー投稿は Redis に
// TTL 付きで置かれ DB に行が無い。antenna_note_unread は FK を満たせず
// `Create` が失敗するが、antenna service は**あえて materialize しない**方針
// なので (#2332、antenna_service.go の OnNoteCreated 参照) DB が膨らむことは
// ない。実害は別で、`pushNote` は先に走るため **DB から引けない ID が
// antenna の ZSET (上限 200) を埋める**。読み取り側 (api/antennas) は DB しか
// 見ないので、幽霊 ID のぶんだけ本来載る note が押し出される。
//
// 既定構成 (ephemeral 無効) ではリレー投稿も DB に入るので上記は起きない。
// それでも外すのは量。#2752 で ListAllActive はキャッシュしたので DB クエリは
// 消えたが、`matchNote` はアクティブ antenna 全件に対して評価され、ヒットすれば
// Redis への push が走る。リレーの firehose 全量をこれに通すコストは読めない。
//
// note / author は read-only。詳細は本ファイル上部の "Hook mutation contract"。
type AntennaHook interface {
	OnNoteCreated(note *model.Note, author *model.User)
	// IsAntennaHook は TimelineFanoutHook との取り違えを型で防ぐためだけの
	// marker。両者はメソッドセットが同一で、Go の interface は structural
	// なので marker が無いと SetFanoutHook / SetAntennaHook を入れ替えても
	// コンパイルが通り、テストも全部緑になる (#2683 と同型の配線事故)。
	IsAntennaHook()
}

// NotificationHook is invoked after an inbound Create / Announce so that
// reply / renote / quote / mention 通知が local な被通知者に対して生成される。
// ローカル作成と同じ interface を使う (実装は core/notification.Hook)。
// notifyLocalUser 側で notifiee が local かどうかを判定するため、ここでは
// 常に fire-and-forget で呼び出せばよい。
//
// note / author / replyTarget / renoteTarget は read-only。詳細は本ファイル
// 上部の "Hook mutation contract"。
type NotificationHook interface {
	OnNoteCreated(note *model.Note, author *model.User, replyTarget, renoteTarget *model.Note)
}

// NoteChartHook is invoked after a freshly persisted inbound Create / Announce
// so that PerUserNotesChart 等の counters が +1 される (#1156)。ローカル作成では
// note_create_service.go の ChartHook が同じ役目を担う。
//
// dedup ヒット (同 URI の重複配送) では発火させないこと: chart hook は
// idempotent ではなく、二重呼び出しで日次集計が +2 されてしまう。
// 実装契約: 呼び出し側は呼び出しを goroutine で非同期化すること推奨 (= local
// 側と同じく safeGo パターン)。
//
// 名称が NoteChartHook なのは、resolver の ChartHook (OnRemoteUserCreated 用)
// と衝突するのを避けつつ責務 (note chart 系) を明示するため。
//
// note は read-only。詳細は本ファイル上部の "Hook mutation contract"。
type NoteChartHook interface {
	OnNoteCreated(note *model.Note)
}

// NewProcessor constructs a Processor. reactionService / noteDeleteSvc は省略
// 可能 (nil)。nil の場合、対応する activity 種別は ErrUnsupportedActivity を返す。
func NewProcessor(
	resolver *Resolver,
	followingService *corefollowing.Service,
	reactionService *corereaction.Service,
	noteDeleteSvc *corenote.DeleteService,
	userRepo repository.UserRepository,
	noteRepo repository.NoteRepository,
) *Processor {
	return &Processor{
		resolver:         resolver,
		followingService: followingService,
		reactionService:  reactionService,
		noteDeleteSvc:    noteDeleteSvc,
		userRepo:         userRepo,
		noteRepo:         noteRepo,
	}
}

// genericActivity is the minimum struct used by the dispatcher to read the
// activity type and actor.
type genericActivity struct {
	Type   string          `json:"type"`
	Actor  string          `json:"actor"`
	Object json.RawMessage `json:"object"`
	ID     string          `json:"id"`
	// Published は Announce などで activity 自体の発火時刻を表す (#940)。
	// 遅延配送された Renote の timeline 並びを origin と揃えるために使う。
	Published string `json:"published"`
	// To は reversi Invite の配送先を読むために保持する。string と []string
	// 両方を受け入れるため RawMessage で保持し、必要な時点で解釈する。
	To json.RawMessage `json:"to"`
	// CC は Create(Note) の audience union (#1560) で activity 側の cc を Note
	// object へ合成するために保持する。To 同様 string / []string 両形式を
	// 受けるため RawMessage で持つ。
	CC json.RawMessage `json:"cc"`
	// raw はアクテ���ビティ全体の生JSON。handleLike 等で content や
	// _misskey_reaction な�� genericActivity に含まれないフィールドを
	// 読むために保持する。
	raw json.RawMessage
}

// Process consumes a JSON activity body received in an inbox. Returns nil if
// the activity is accepted (whether or not it produced side effects). Returns
// ErrUnsupportedActivity for types that are accepted but currently not
// processed (callers should still acknowledge the request to the sender).
//
// 入力 JSON は最初に activitypub.Normalize を通してキー名を canonical な短形式
// に揃えてから dispatch する。これにより `as:type` / `https://www.w3.org/ns/
// activitystreams#type` / `@type` のいずれでも同じ struct フィールドにマップ
// される。
//
// **Idempotency invariant (#534)**: each activity handler MUST be idempotent so
// that the inbox queue worker can safely retry on transient failure and so
// that out-of-order delivery (e.g. Update before Create due to federation
// hop reordering) eventually converges. Verified at the time of #534:
//
//   - Follow / Accept: swallow ErrAlreadyFollowing / ErrAlreadyRequested /
//     ErrRequestNotFound (so re-deliveries are no-ops). Accept also swallows
//     ErrBlocking / ErrBlocked, returned by AcceptRequest's defense-in-depth
//     block check (a block condition never clears on retry; Follow itself
//     handles ErrBlocking / ErrBlocked separately via auto-unblock / Reject
//     before reaching this generic list)
//   - Undo Follow / Like / Block: swallow ErrNotFollowing / ErrReactionNotFound /
//     ErrNotBlocking
//   - Like / Block: swallow ErrAlreadyReacted / ErrAlreadyBlocking
//   - Create / Announce: dedup via note URI lookup before insert
//   - Delete: treat "target not found" as success
//   - Update Note / Person: overwrite semantics (last-write-wins). 後着
//     Update が先着 Update を上書きしても peer が次回 fetch した値で
//     収束するので一時的な不整合は許容
//
// New activity handlers added below MUST preserve this invariant; otherwise
// retried inbox jobs will produce duplicate side effects (notifications
// fired twice, counters double-incremented, etc).
// ProcessWithSigner is Process carrying the verified HTTP signer (#2338).
//
// Mastodon 系リレーは Announce ではなく Create を LD-Signature 付きで転送する。
// その判定には「誰が配送してきたか」= HTTP 署名者が要るが、Process(body) には
// body しか渡っていなかったため handleCreate がリレー転送を判別できず、著者が
// DB に積み上がっていた。
//
// signer が nil (署名なし経路 / 未配線) のときは従来どおり DB 経路に倒れる。
func (p *Processor) ProcessWithSigner(body []byte, signer *model.User) error {
	return p.process(body, signer)
}

// Process handles an inbound activity without signer information.
func (p *Processor) Process(body []byte) error {
	return p.process(body, nil)
}

func (p *Processor) process(body []byte, signer *model.User) error {
	// Foundkey 系 fork instance は valid な AS Activity を 1 要素 JSON
	// array で wrap して送信してくるケースがある (#1185)。AS 仕様上
	// inbox direct POST は object 前提なので、array が来た時は剥がして
	// 中身を処理する。2+ 要素は AS spec 外として no-op で素通し。
	//
	// Normalize の前で剥がす: Normalize は内部で json.Unmarshal → 再
	// encode するが top-level の array → object 変換はしないので、
	// object 化はここで済ませる必要がある。
	if unwrapped, ok := tryUnwrapSingletonArray(body); ok {
		// 観測性のため発火を記録 (log message は中立的に表現、Foundkey 由来は
		// helper / 本コメント側の doc に残す)。
		slog.Info("federation: unwrapping singleton JSON array activity",
			"originalSize", len(body), "unwrappedSize", len(unwrapped))
		body = unwrapped
	}

	normalized, err := activitypub.Normalize(body)
	if err != nil {
		// **retry しても結果が変わらないものは ack する (レビュー L1)。** 衝突
		// キーを持つ document も JSON として壊れた body も、同じ body を何度
		// 投げ直しても同じ error になる。生で返すと inbox job が 8 回 retry して
		// dead letter に積まれるだけ。この PR の他の判断 (禁止語 / chat の
		// 恒久エラー) と揃える。
		if errors.Is(err, activitypub.ErrConflictingKeys) {
			slog.Warn("activitypub: conflicting json-ld keys", "err", err)
			return ErrUnsupportedActivity
		}
		return fmt.Errorf("invalid activity json: %w", err)
	}
	body = normalized

	// Normalize は内部で json.Marshal して再エンコードするため、ここでの
	// Unmarshal は構文エラーで失敗しないことが保証される。
	var act genericActivity
	_ = json.Unmarshal(body, &act)
	act.raw = body
	// upstream Misskey #17340: activity.actor が string IRI でなく embedded
	// {"id": "..."} object のケースを救済する (triage #999)。
	act.normalizeActor(body)
	if act.Actor == "" {
		return errors.New("activity missing actor")
	}

	// upstream performOneActivity は actor.isSuspended なら dispatch 前に
	// 早期 return する (ApInboxService.ts:144、#1560)。mk-go は既知の
	// suspended remote actor を DB lookup で判定して drop する (新規 actor は
	// まだ DB に無く suspended にもなり得ないので素通し)。fetch はせず DB read
	// のみなので追加コストは小さい。
	if p.userRepo != nil {
		actor, err := p.userRepo.FindByURI(act.Actor)
		switch {
		case err == nil && actor != nil && actor.IsSuspended:
			slog.Info("federation: dropping activity from suspended actor", "actor", act.Actor, "type", act.Type)
			return nil
		case err != nil && !repository.IsNotFound(err):
			// **判定できないまま素通しにしない** (#3116)。以前は `err == nil` を
			// 条件にしていたので、**DB 障害のあいだ凍結済み actor の activity が
			// そのまま処理されていた** (moderation の fail-open)。not-found は
			// 「まだ取り込んでいない actor」= 凍結されているはずがないので素通しが
			// 正しいが、障害は retry させる。
			return fmt.Errorf("suspended actor check: %w", err)
		}
	}

	return p.dispatchActivity(act, 0, signer)
}

// maxCollectionDepth bounds nested Collection/OrderedCollection unrolling so a
// collection-of-collections cannot recurse without limit (#2023).
const maxCollectionDepth = 1

// dispatchActivity routes a parsed activity to its handler. depth tracks
// Collection/OrderedCollection unrolling so handleCollection can bound recursion.
func (p *Processor) dispatchActivity(act genericActivity, depth int, signer *model.User) error {
	switch strings.ToLower(act.Type) {
	case "follow":
		return p.handleFollow(act)
	case "undo":
		return p.handleUndo(act)
	case "accept":
		return p.handleAccept(act)
	case "create":
		return p.handleCreate(act, signer)
	case "like":
		return p.handleLike(act)
	case "announce":
		return p.handleAnnounce(act, signer)
	case "delete":
		return p.handleDelete(act)
	case "update":
		return p.handleUpdate(act)
	case "reject":
		return p.handleReject(act)
	case "block":
		return p.handleBlock(act)
	case "flag":
		return p.handleFlag(act)
	case "move":
		return p.handleMove(act)
	case "add":
		return p.handleAdd(act)
	case "remove":
		return p.handleRemove(act)
	case "emojireaction", "emojireact":
		return p.handleLike(act)
	case "invite":
		// object が chat room の Group なら group chat federation の招待。
		// reversi Game object とは object.type で区別する (#1203)。
		if p.isChatRoomInvite(act) {
			return p.handleChatRoomInvite(act)
		}
		// 非 reversi (Group Invite 等) は未対応扱いで 202 を返させる。
		// reversi Game object 以外の Invite をここで 400 にすると relay
		// 以外の peer との互換性が崩れる (#417 P4 Devin review)。
		if err := p.handleReversiInvite(act); err != nil {
			if errors.Is(err, ErrNotReversiGame) {
				return ErrUnsupportedActivity
			}
			return err
		}
		return nil
	case "join":
		return p.handleReversiJoin(act)
	case "leave":
		return p.handleReversiLeave(act)
	case "misskey:chatmessage":
		return p.handleChatMessage(act)
	case "collection", "orderedcollection":
		return p.handleCollection(act, depth, signer)
	}
	return ErrUnsupportedActivity
}

// handleCollection unrolls a Collection / OrderedCollection activity, dispatching
// each inline item whose id host matches the collection actor's host. Mirrors
// upstream ApInboxService.performActivity の collection branch: items 数が
// recursion limit を超えたら skip、各 item は extractDbHost(item.id) ==
// extractDbHost(actor.uri) を要求する (spoofing 防止)。URI 文字列の item は解決せず
// skip する (fetch 増幅を避ける; relay の inline 配送のみ対応、#2023)。
func (p *Processor) handleCollection(act genericActivity, depth int, signer *model.User) error {
	if depth >= maxCollectionDepth {
		return nil // skip: nested collection beyond depth limit
	}
	var col struct {
		// APRawList は単一 object も 1 件として拾う。upstream は
		// `toArray(...)` で展開する (#2662)。**ここは decode の error を
		// 握るので、片方がスカラーでももう片方は生き残る** (featured.go は
		// error を見て collection ごと捨てるので、そちらだけ巻き添えになる)。
		Items        activitypub.APRawList `json:"items"`
		OrderedItems activitypub.APRawList `json:"orderedItems"`
	}
	if len(act.raw) > 0 {
		_ = json.Unmarshal(act.raw, &col)
	}
	// upstream は type に応じて片方のみ読む (Collection→items / OrderedCollection→
	// orderedItems)。
	items := col.OrderedItems
	if strings.ToLower(act.Type) == "collection" {
		items = col.Items
	}
	// 過大な collection は upstream の getRecursionLimit と同様に弾く。
	if len(items) >= resolveRecursionLimit {
		return nil
	}
	actorHost, err := hostFromURI(act.Actor)
	if err != nil {
		return nil
	}
	for _, item := range items {
		var itemAct genericActivity
		// 型エラーを握る (#2662)。握らないと `{"actor": {"id": ...}}` や
		// 先行 field の型エラーがある item を丸ごと落とす。文字列 URI 等の
		// inline でない item は握っても `itemAct.ID` が空のまま下の gate で
		// skip される。
		if err := unmarshalIgnoringTypeErrors(item, &itemAct); err != nil {
			continue
		}
		itemAct.raw = item
		// per-item: id host == collection actor host を要求する (upstream の host 一致
		// check)。punyHost で IDN/大文字を正規化して比較する (#1850 と同方針)。
		itemHost, herr := hostFromURI(itemAct.ID)
		if itemAct.ID == "" || herr != nil || punyHost(itemHost) != punyHost(actorHost) {
			continue
		}
		// upstream performActivity は collection item を **collection の認証済み actor**
		// の権威で performOneActivity に渡す (item.actor は使わない)。item が別 actor を
		// 詐称しても collection actor として処理されるよう actor を上書きする (#2023
		// security)。これにより third-party actor を詐称した reaction/announce 等の
		// 注入を防ぐ。
		itemAct.Actor = act.Actor
		if derr := p.dispatchActivity(itemAct, depth+1, signer); derr != nil && !errors.Is(derr, ErrUnsupportedActivity) {
			slog.Error("federation: collection item failed", "id", itemAct.ID, "type", itemAct.Type, "err", derr)
		}
	}
	return nil
}

// ExtractActorIRI returns the authoritative actor IRI of an inbound activity,
// applying the SAME singleton-array unwrap + JSON-LD normalization that Process
// performs before dispatch. The InboxProcessor's actor-authorization gate must
// use this (not a raw-body parse) so that the actor it authorizes is exactly
// the actor Process will act on. Otherwise `as:actor`, `actor:{"@id":...}` or a
// singleton-array-wrapped activity would let an attacker present an empty/benign
// actor to the gate while Process resolves the real (spoofed) actor.
//
// Returns "" when no actor can be derived (Process will then reject the body).
func ExtractActorIRI(body []byte) string {
	if unwrapped, ok := tryUnwrapSingletonArray(body); ok {
		body = unwrapped
	}
	normalized, err := activitypub.Normalize(body)
	if err != nil {
		return ""
	}
	var act genericActivity
	// **型エラーで "" を返さない。** 呼び出し側 (inbox の authorizeActor) は
	// `actor == ""` を「actor 欠落 = Process 側が弾く」と解釈して**ゲートを
	// 素通しさせる**。`process()` は `_ = json.Unmarshal` + `normalizeActor` で
	// 救済するので、`{"actor":{"id":...}}` や `"published": 12345` のような
	// document は actor を読めてしまい、署名者 != actor の LD-Signature 検証・
	// `activity.id` host ゲート・replay guard が全部飛ぶ (#2662)。
	if err := unmarshalIgnoringTypeErrors(normalized, &act); err != nil {
		return ""
	}
	act.normalizeActor(normalized)
	return act.Actor
}

// unmarshalIgnoringTypeErrors decodes JSON, keeping whatever fields decoded and
// swallowing `*json.UnmarshalTypeError` (構文エラーは返す)。
//
// `process()` が `_ = json.Unmarshal` で実質同じことをしているので、そこから
// 値を読む helper も同じ寛容さでないと**ゲートと本処理で actor がずれる**。
func unmarshalIgnoringTypeErrors(data []byte, v any) error {
	err := json.Unmarshal(data, v)
	if err == nil {
		return nil
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return nil
	}
	return err
}

// ExtractActivityID returns the activity's top-level `id` after the same
// unwrap+Normalize that Process applies. Used by the inbox authorization gate to
// enforce that activity.id is a string whose host matches the actor's host
// (upstream InboxProcessorService の signerHost!==activity.id host gate、#1779)。
// 取得不能 / 欠落時は "" を返す。
func ExtractActivityID(body []byte) string {
	if unwrapped, ok := tryUnwrapSingletonArray(body); ok {
		body = unwrapped
	}
	normalized, err := activitypub.Normalize(body)
	if err != nil {
		return ""
	}
	var act genericActivity
	// ExtractActorIRI と同じ理由で型エラーを握る。
	if err := unmarshalIgnoringTypeErrors(normalized, &act); err != nil {
		return ""
	}
	return act.ID
}

// ActivityFields are the identity fields the inbox gate needs from a body.
type ActivityFields struct {
	Actor string // activity.actor (IRI)
	ID    string // activity.id
	Type  string // activity.type
}

// ExtractActivityFields reads actor / id / type in **one** unwrap+Normalize pass.
//
// **1 回で済ませる。** 個別に取ると同じ body を JSON-LD normalize し直すことに
// なる。破棄の判定と破棄ログはどちらもこの経路を通り、しかも「解決できる actor で
// HTTP 署名が通る」だけで到達できるので、パス数がそのまま攻撃者に押せる CPU に
// なる (#2724 review MEDIUM-4。60KB の body で 1 パス約 1ms を実測)。
//
// 前処理は ExtractActorIRI / ExtractActivityID と同一にする。生 body を直接見ると
// 配列 wrap や `as:type` で読めず、gate が見ているものとずれる。
// 取得不能 / 欠落の field は "" になる。
func ExtractActivityFields(body []byte) ActivityFields {
	if unwrapped, ok := tryUnwrapSingletonArray(body); ok {
		body = unwrapped
	}
	normalized, err := activitypub.Normalize(body)
	if err != nil {
		return ActivityFields{}
	}
	var act genericActivity
	// ExtractActorIRI と同じ理由で型エラーを握る。
	if err := unmarshalIgnoringTypeErrors(normalized, &act); err != nil {
		return ActivityFields{}
	}
	act.normalizeActor(normalized)
	return ActivityFields{Actor: act.Actor, ID: act.ID, Type: act.Type}
}

// SetBlockingService wires the blocking service for Block/Undo Block activities.
func (p *Processor) SetBlockingService(svc *coreblocking.Service) {
	p.blockingService = svc
}

// HasBlockingService reports whether the blocking service was wired.
//
// 未配線だと inbound の Block / Undo(Block) が `ErrUnsupportedActivity` に
// なるだけでなく、**reversi の Invite からブロック判定が丸ごと飛ぶ**
// (`reversi_inbox.go` の `p.blockingService != nil` guard)。ブロック済みの
// リモート利用者が対象の `reversi:<userID>` ストリームへ `invited` を push
// できる。起動時検査に使う。
func (p *Processor) HasBlockingService() bool { return p != nil && p.blockingService != nil }

// SetAbuseReportRepo wires the abuse report repository for Flag activities.
func (p *Processor) SetAbuseReportRepo(repo repository.AbuseReportRepository, idGen id.Generator) {
	p.abuseReportRepo = repo
	p.abuseIDGen = idGen
}

// AbuseModeratorLister lists moderator/administrator users (#2868)。
// 実装は core/role.Service。
type AbuseModeratorLister interface {
	GetModerators() ([]*model.User, error)
}

// AbuseInAppNotifier creates the in-app notification moderators see for a new
// report (#2868)。実装は core/notification.Service。
type AbuseInAppNotifier interface {
	Create(ctx context.Context, in notification.CreateInput) (*notification.Notification, error)
}

// SetAbuseReportNotification wires the in-app notification for reports that
// arrive over ActivityPub (#2868)。片方でも nil なら通知しない。
func (p *Processor) SetAbuseReportNotification(lister AbuseModeratorLister, notifier AbuseInAppNotifier) {
	p.abuseModeratorLister = lister
	p.abuseInAppNotifier = notifier
}

// notifyModeratorsOfRemoteAbuseReport mirrors the local report-abuse fanout for
// reports received over ActivityPub (#2868)。best-effort。
func (p *Processor) notifyModeratorsOfRemoteAbuseReport(report *model.AbuseUserReport) {
	if p.abuseModeratorLister == nil || p.abuseInAppNotifier == nil {
		return
	}
	mods, err := p.abuseModeratorLister.GetModerators()
	if err != nil {
		slog.Warn("remote abuse report: list moderators failed", "err", err)
		return
	}
	for _, m := range mods {
		// notifier は通報者 (リモートユーザー)。通報者自身がローカルの
		// モデレーターになることは無いので self-notification は起きないが、
		// 起きても正しい挙動なので同じく警告に出さない。
		_, nerr := p.abuseInAppNotifier.Create(context.Background(), notification.CreateInput{
			NotifieeID: m.ID,
			NotifierID: report.ReporterID,
			Type:       notification.TypeAbuseReport,
			// **comment は入れない (#2868)。** 通報コメントは定型フォームの全文が
			// 入るので通知欄に出しても読めず、出さない以上 Redis に通報本文の
			// 複製を残す理由が無い (権限を失った元モデレーターに読まれる面も減る)。
			Extra: map[string]any{
				"reportId":     report.ID,
				"targetUserId": report.TargetUserID,
			},
		})
		if nerr != nil && !errors.Is(nerr, notification.ErrSelfNotification) {
			slog.Warn("remote abuse report: in-app notification failed", "moderator", m.ID, "err", nerr)
		}
	}
}

// SetPinningRepo wires the note pinning repository for Add/Remove activities.
func (p *Processor) SetPinningRepo(repo repository.UserNotePiningRepository, idGen id.Generator) {
	p.pinningRepo = repo
	p.pinningIDGen = idGen
}

// SetRelayMarker wires a RelayStatusMarker so inbound Accept / Reject
// activities whose id is a follow-relay URI issued by this instance can flip
// the corresponding relay row to accepted / rejected. 反映するのは送信元
// actor がその relay 自身だったときだけ (relayStatusChangeAllowed)。
func (p *Processor) SetRelayMarker(m RelayStatusMarker) {
	p.relayMarker = m
}

// SetRelayActorChecker wires a RelayActorChecker so handleAnnounce can detect
// relay-delivered Announce activities and publish target notes to timeline
// streams directly instead of wrapping them in a renote (upstream #17308 /
// triage #1002).
func (p *Processor) SetRelayActorChecker(c RelayActorChecker) {
	p.relayActorChecker = c
}

// AccountDeleteEnqueuer schedules the background cascade deletion of a user's
// notes / drive files / following rows. Implemented by queue.Client. Used by
// inbound actor self-delete to purge a deleted remote user's mirror data.
type AccountDeleteEnqueuer interface {
	EnqueueDeleteAccount(payload queue.DeleteAccountPayload) error
}

// SetAccountDeleteEnqueuer wires the enqueuer used to cascade-purge a remote
// user's data when an inbound actor self-delete is received (#1220).
func (p *Processor) SetAccountDeleteEnqueuer(e AccountDeleteEnqueuer) {
	p.accountDeleteEnqueuer = e
}

// SetChatService wires a ChatMessageReceiver for inbound Misskey:ChatMessage
// activities (CherryPick v12 federation).
func (p *Processor) SetChatService(svc ChatMessageReceiver) {
	p.chatService = svc
}

// SetFanoutHook wires a TimelineFanoutHook so that remote notes ingested via
// handleCreate / handleAnnounce get pushed to timeline caches and streaming
// subscribers (#330).
func (p *Processor) SetFanoutHook(h TimelineFanoutHook) {
	p.fanoutHook = h
}

// SetAntennaHook wires an AntennaHook so that remote notes ingested via
// handleCreate / handleAnnounce are matched against antennas (#2743).
// 配線しないと**リモートの投稿が antenna に一切入らない** — upstream は
// ApNoteService が noteCreateService.create を通すので入る。
//
// publishRelayDeliveredNote (リレー経由) からは呼ばない。理由は AntennaHook
// の doc を参照。
func (p *Processor) SetAntennaHook(h AntennaHook) {
	p.antennaHook = h
}

// SetNotificationHook wires a NotificationHook so that inbound Create /
// Announce activities generate reply / renote / quote / mention 通知 for
// local notifiees (#415). Without it, remote users が local note を renote /
// reply / mention しても通知が飛ばない (ローカル作成だけが note_create_service
// 経由で通知を作る状態になる)。
func (p *Processor) SetNotificationHook(h NotificationHook) {
	p.notificationHook = h
}

// SetNoteChartHook wires a NoteChartHook so that inbound Create / Announce
// activities update PerUserNotesChart 等のリモートユーザー向け chart 集計
// (#1156)。Without it, リモートユーザーのプロフィールの「アクティビティ」
// タブが空になる (delete だけ note_delete_service 経由で -1 されるため
// heatmap がマイナスにしか動かない drop-in regression が再発する)。
//
// 名称が SetNoteChartHook なのは resolver.SetChartHook (= 新規 remote user
// 集計用) と衝突するのを避けるため。配線対象は同じ chart hooks 集約だが、
// 注入経路を別 method に分けて誤配線を防ぐ。
func (p *Processor) SetNoteChartHook(h NoteChartHook) {
	p.noteChartHook = h
}

// followRelayPath is the path prefix of the Follow(Relay) activity ids this
// instance issues. Must stay in lockstep with
// activitypub.URLBuilder.FollowRelayURI.
const followRelayPath = "/activities/follow-relay/"

// followRelayIDPattern matches the id part of a Follow(Relay) activity URI
// (the `{id}` in `{baseURL}/activities/follow-relay/{id}`). 字種は旧実装の
// 末尾正規表現と同じ集合に揃えてあり、path 区切りを含む値 (`rel/../other`
// のような細工) は relay 行の主キーとして受け付けない。
var followRelayIDPattern = regexp.MustCompile(`^[\w-]+$`)

// matchFollowRelayID returns the relay id when uri is a Follow(Relay) activity
// URI **issued by this instance**, otherwise the empty string.
//
// localBaseURL を必須にしているのが要点。relay への Follow はこちらが出した
// ものなので、その id は必ず自ホストの URI になる。旧実装はパス末尾だけを
// 正規表現で見ていたため `https://evil.test/activities/follow-relay/<id>` の
// ような他ホストの id でも relay 行を特定でき、任意のリモート actor が
// relay の status を書き換えられた (upstream ApInboxService の accept() /
// reject() も同じ形)。localBaseURL が未配線 (空) のときは fail-closed で
// "" を返す。
func matchFollowRelayID(uri, localBaseURL string) string {
	if localBaseURL == "" {
		return ""
	}
	rest, ok := strings.CutPrefix(uri, localBaseURL+followRelayPath)
	if !ok || !followRelayIDPattern.MatchString(rest) {
		return ""
	}
	return rest
}

// relayActorOwnsRelay reports whether actorURI belongs to the same host as the
// relay's registered inbox.
//
// relay 行は inbox URI しか identity を持たないので host で突き合わせる。
// host は hostFromURI で punycode + 小文字に正規化してから比較するため、
// `https://Relay.Example/actor` と `https://relay.example/inbox` は一致する。
// port は host の一部として扱う (別 authority を同一視しない安全側)。
//
// 既知の限界: relay と同じ host に別 actor を立てられる相手 (マルチテナントな
// relay サーバー) は依然としてその relay の status を動かせる。行が inbox URI
// しか持たない以上ここが上限で、actor の advertise する inbox と管理者が
// 登録した inbox が完全一致する保証は無い (末尾スラッシュ等で正当な relay を
// 落とすほうが害が大きい)。
func relayActorOwnsRelay(actorURI string, rel *model.Relay) bool {
	if rel == nil || rel.Inbox == "" || actorURI == "" {
		return false
	}
	actorHost, err := hostFromURI(actorURI)
	if err != nil || actorHost == "" {
		return false
	}
	inboxHost, err := hostFromURI(rel.Inbox)
	if err != nil || inboxHost == "" {
		return false
	}
	return actorHost == inboxHost
}

// relayStatusChangeAllowed reports whether the actor that signed an inbound
// Accept / Reject may flip the status of the relay row referenced by relayID.
//
// kind はログ用の activity 種別 ("Accept" / "Reject")。検証に落ちたケースは
// 呼び出し側で状態を変えずに drop する (ack して retry しない)。
func (p *Processor) relayStatusChangeAllowed(actorURI, relayID, kind string) bool {
	rel, err := p.relayMarker.FindByID(context.Background(), relayID)
	if err != nil || rel == nil {
		slog.Info("federation: dropping relay "+kind+" for unknown relay",
			"actor", actorURI, "relayId", relayID, "err", err)
		return false
	}
	if !relayActorOwnsRelay(actorURI, rel) {
		slog.Warn("federation: dropping forged relay "+kind+" (actor is not the relay)",
			"actor", actorURI, "relayId", relayID, "relayInbox", rel.Inbox)
		return false
	}
	return true
}

// SetReversi wires the reversi federation dependencies. When any of the
// arguments is nil, reversi inbox activities fall through as unsupported
// (useful for tests that do not care about reversi).
func (p *Processor) SetReversi(
	svc *corereversi.Service,
	repo repository.ReversiRepository,
	idGen id.Generator,
	fedCache *corereversi.FederationIDCache,
) {
	p.reversiSvc = svc
	p.reversiRepo = repo
	p.reversiIDGen = idGen
	p.reversiFedCache = fedCache
}

// SetReversiStreamPublisher wires a per-user reversi stream publisher used
// by handleReversiInvite to push `invited` events in real-time (#417 P2)。
func (p *Processor) SetReversiStreamPublisher(pub ReversiStreamPublisher) {
	p.reversiStreamPub = pub
}

// handleFollow processes an inbound Follow activity.
func (p *Processor) handleFollow(act genericActivity) error {
	follower, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	followeeURI, err := readObjectString(act.Object)
	if err != nil {
		return err
	}
	followee, err := p.resolveTargetUser(followeeURI)
	if err != nil {
		return errors.New("unknown followee")
	}
	// upstream follow() は followee が remote の場合 'skip: フォローしようと
	// しているユーザーはローカルユーザーではありません' で明示的に拒否する
	// (ApInboxService.ts)。これが無いと relay / 悪意ある remote actor が
	// Follow(actor=remoteA, object=既知 remoteB) を送るだけで local DB に
	// remoteA->remoteB の Following 行が作られ、フォロワー数や timeline fanout を
	// 汚染できる。skip は error にせず ack して終わる (inbox retry を防ぐ) (#1826)。
	if !followee.IsLocal() {
		slog.Info("federation: skipping inbound Follow targeting a remote followee",
			"follower", act.Actor, "followee", followeeURI)
		return nil
	}
	// inbound AP Follow は AP protocol で withReplies を運ばないので
	// FollowOptions{} (default false) で作成する (#1056)。remote follower の
	// per-followee withReplies preference は AP protocol 範囲外。
	result, err := p.followingService.Follow(follower.ID, followee.ID, corefollowing.FollowOptions{})
	// block 起因の失敗は upstream UserFollowingService.follow と同じ AP 流儀で
	// 処理する (#1631)。エラーのまま返すと inbox が retry → dead-letter になり
	// 相手サーバーの pending follow が永遠に解消されない。
	if errors.Is(err, corefollowing.ErrBlocking) && !follower.IsLocal() && followee.IsLocal() && p.blockingService != nil {
		// ErrBlocking = remote follower が local followee を block している。
		// upstream は相互 block なら blocked (Reject) を優先し、blocking 単独
		// なら自動で block 解除して follow を続行する。
		blocked, berr := p.blockingService.IsBlocked(followee.ID, follower.ID)
		if berr != nil {
			return berr
		}
		if blocked {
			err = corefollowing.ErrBlocked
		} else {
			if uerr := p.blockingService.Unblock(follower.ID, followee.ID); uerr != nil {
				return fmt.Errorf("inbound follow: auto-unblock %s->%s: %w", follower.ID, followee.ID, uerr)
			}
			slog.Info("inbound follow: auto-unblocked stale remote block",
				"follower", follower.ID, "followee", followee.ID)
			result, err = p.followingService.Follow(follower.ID, followee.ID, corefollowing.FollowOptions{})
		}
	}
	if errors.Is(err, corefollowing.ErrBlocked) && !follower.IsLocal() && followee.IsLocal() {
		// local followee が remote follower を block している場合は Reject を
		// 送り返して正常終了する (upstream「エラーにするのではなく Reject を
		// 送り返しておしまい」)。配送失敗は Accept 側と同じく warn のみ。
		if p.inboundFollowAcceptor != nil {
			if rerr := p.inboundFollowAcceptor.SendRejectForInboundFollow(follower, followee, act.raw); rerr != nil {
				slog.Warn("inbound follow reject delivery failed",
					"follower", follower.ID, "followee", followee.ID, "err", rerr)
			}
		}
		return nil
	}
	alreadyFollowing := errors.Is(err, corefollowing.ErrAlreadyFollowing)
	// ErrAlreadyRequested はlocked followeeへの再送。既に FollowRequest が
	// 存在するので何もしなくて良い (Accept は承認時に送られる)。エラーとして
	// 上に返すと inbox が 4xx を返して相手が retry を続けるので swallow する。
	alreadyRequested := errors.Is(err, corefollowing.ErrAlreadyRequested)
	if err != nil && !alreadyFollowing && !alreadyRequested {
		return err
	}
	// Accept を返送するのは Following が成立した (新規 or 既存) 場合のみ。
	// 既にフォローされている (ErrAlreadyFollowing) 場合も Accept を再送する:
	// 相手サーバーが Accept を見逃していた or retry で新規 Follow を
	// 送り直した状況で、我々が何も返さないと相手は永遠に pending のままに
	// なる (idempotent な Accept 再送で解消する)。original Follow の raw を
	// そのまま object に包むことでリモート側が送った Follow と matching
	// できる。FollowRequest 止まり (locked followee) の場合は acceptor を
	// 呼ばない (Accept は明示承認後)。
	shouldAccept := alreadyFollowing || (result != nil && result.Following != nil)
	if shouldAccept && p.inboundFollowAcceptor != nil {
		if err := p.inboundFollowAcceptor.SendAcceptForInboundFollow(follower, followee, act.raw); err != nil {
			slog.Warn("inbound follow accept delivery failed",
				"follower", follower.ID, "followee", followee.ID, "err", err)
		}
	}
	return nil
}

// handleUndo processes an Undo activity wrapping a Follow / Like / Announce.
func (p *Processor) handleUndo(act genericActivity) error {
	var inner genericActivity
	// 型エラーを握らないと、直後の normalizeActor (#999) が救うはずの
	// `inner.actor` が embedded object のケースに**到達できない** (#2662)。
	if err := unmarshalIgnoringTypeErrors(act.Object, &inner); err != nil {
		return fmt.Errorf("invalid undo object: %w", err)
	}
	// inner.actor が embedded object のケースも救済する (#999 / upstream #17340)。
	//
	// **現状の Undo 配下はどれも `inner.Actor` を読まない** (権威は外側の
	// `act.Actor`、対象は `inner.Object`)。Accept / Reject と形を揃えるための
	// 予防で、ここだけは挙動で観測できない = テストで固定できない。消しても
	// 今は壊れないが、inner.actor を読む sub-handler を足したときに object
	// 形式が黙って落ちるので残す (#2665)。
	inner.normalizeActor(act.Object)
	switch strings.ToLower(inner.Type) {
	case "follow":
		return p.handleUndoFollow(act, inner)
	case "like":
		return p.handleUndoLike(act, inner)
	case "announce":
		return p.handleUndoAnnounce(act, inner)
	case "block":
		return p.handleUndoBlock(act, inner)
	case "accept":
		return p.handleUndoAccept(act, inner)
	case "emojireaction", "emojireact":
		return p.handleUndoLike(act, inner)
	case "invite":
		// CherryPick が pre-start reversi 招待を取り消す際に Undo(Invite) を
		// 送ってくる (#417 P4)。inner.Object が reversi Game なら reversi
		// 側で処理、それ以外 (例: Group Invite) は未対応扱いにして inbox
		// が 202 を返すようにする (400 にすると peer 側の配送 retry を
		// 招いてしまう)。
		if err := p.handleReversiUndoInvite(act, inner); err != nil {
			if errors.Is(err, ErrNotReversiGame) {
				return ErrUnsupportedActivity
			}
			return err
		}
		return nil
	}
	return ErrUnsupportedActivity
}

// handleUndoAccept processes an inbound Undo(Accept): a remote followee revokes
// a previously-accepted follow, which means we should drop our local follower's
// following relationship to them (upstream ApInboxService.undoAccept、#1560)。
//
// 二段ネスト: act=Undo, inner=Accept, inner.Object=元の Follow。
//   - act.Actor       = remote followee (accept を撤回した側)
//   - Follow.actor     = local follower (フォローしていた側)
//
// follower→followee の Following があれば Unfollow する。
func (p *Processor) handleUndoAccept(act genericActivity, inner genericActivity) error {
	followee, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	// inner.Object は元の Follow。その actor が local follower。
	//
	// 型エラーを握らないと、直後の normalizeActor (#999) が救うはずの
	// `"actor": {"id": ...}` 形式で早期 return してしまい、Undo(Accept(Follow))
	// が unfollow されない (#2662)。string URI は握っても ID / Actor が空のまま
	// なので従来どおり skip に落ちる。
	var follow genericActivity
	if uerr := unmarshalIgnoringTypeErrors(inner.Object, &follow); uerr != nil {
		// object が Follow の URI 文字列だけのケースは follower を特定できない。
		// upstream は getUserFromApId(activity.object) で解決するが、mk-go では
		// Follow URI から follower を逆引きする経路が無いので skip (ack)。
		return nil
	}
	follow.normalizeActor(inner.Object)
	followerURI, err := readActorString(follow)
	if err != nil || followerURI == "" {
		return nil
	}
	var follower *model.User
	if localID := p.resolver.ExtractLocalUserID(followerURI); localID != "" {
		follower, err = p.userRepo.FindByID(localID)
	} else {
		follower, err = p.userRepo.FindByURI(followerURI)
	}
	if err != nil {
		// **#3115 が Accept / Reject で確立した原則の裏返し** (#3116)。ack すると
		// job が retry されず、**accept を撤回された相手をフォローし続ける**。
		if repository.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("undo accept: lookup follower: %w", err)
	}
	if follower == nil {
		return nil
	}
	if err := p.followingService.Unfollow(follower.ID, followee.ID); err != nil {
		// フォローしていなければ no-op (upstream の 'skip: フォローされていない')。
		if errors.Is(err, corefollowing.ErrNotFollowing) {
			return nil
		}
		return err
	}
	return nil
}

// handleUndoFollow undoes a previously created Follow.
func (p *Processor) handleUndoFollow(act genericActivity, inner genericActivity) error {
	follower, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	followeeURI, err := readObjectString(inner.Object)
	if err != nil {
		return err
	}
	followee, err := p.resolveTargetUser(followeeURI)
	if err != nil {
		return errors.New("unknown followee")
	}
	// #2106 L30: upstream undoFollow は followee が remote (host != null) なら skip する
	// (handleFollow の gate と対称)。remote→remote の Undo を DB 走査 / counter 経路に
	// 入れない防御線を Undo 側にも揃える。
	if followee.Host != nil {
		return nil
	}
	if err := p.followingService.Unfollow(follower.ID, followee.ID); err != nil &&
		!errors.Is(err, corefollowing.ErrNotFollowing) {
		return err
	}
	// locked local followee 宛の pending follow request を取り消す。upstream
	// undoFollow は requestExist を見て cancelFollowRequest を呼ぶ。これが無いと
	// remote が accept 前に Undo(Follow) を送った場合、phantom な follow request 行と
	// receiveFollowRequest 通知が残る (handleReject と同じ pattern、#1948-21)。
	if err := p.followingService.CancelRequest(follower.ID, followee.ID); err != nil &&
		!errors.Is(err, corefollowing.ErrRequestNotFound) {
		return err
	}
	return nil
}

// handleUndoLike removes a reaction previously added via Like.
//
// Note: reversi game session URI を対象とした Undo(EmojiReaction) は
// handleLike と対称に 202 ack で握り潰す (#417 P5)。dispatch 順も handleLike
// と揃えており、ResolveActor を先頭で実行することで actor cache の populate
// が両 path で同様に行われるようにする (#417 P5 Devin review)。
// reactionService nil の本番 config は無いので unconditional な ResolveActor
// で実害なし。
func (p *Processor) handleUndoLike(act genericActivity, inner genericActivity) error {
	reactor, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		// permanent な actor resolve 失敗は ack して skip (#1183)。
		if isPermanentSkipError(err) {
			slog.Info("federation: skipping Undo(Like) (actor unresolvable)",
				"actor", act.Actor, "err", err)
			return nil
		}
		return err
	}
	targetURI, err := readObjectString(inner.Object)
	if err != nil {
		// inner.object 欠如した malformed Undo(Like) は handleLike と同じく
		// 202 ack 扱い (旧実装は reactionService nil 経路で ErrUnsupportedActivity
		// を返していた)。
		return ErrUnsupportedActivity
	}
	if corereversi.IsReversiGameSessionURI(targetURI) {
		return nil
	}
	if p.reactionService == nil {
		return ErrUnsupportedActivity
	}
	target, err := p.resolver.ResolveNote(targetURI)
	if err != nil {
		// permanent な target resolve 失敗は ack。Undo の対象 reaction record
		// が存在しないだけでも idempotent (= 削除済みは ErrReactionNotFound で
		// nil 返却) なので、resolve できない先の reaction は元から無い扱いで
		// 整合する (#1183)。
		if isPermanentSkipError(err) {
			slog.Info("federation: skipping Undo(Like) (target unresolvable)",
				"object", targetURI, "actor", act.Actor, "err", err)
			return nil
		}
		return err
	}
	if err := p.reactionService.Delete(reactor, target.ID); err != nil {
		// 既に削除済みは許容
		if errors.Is(err, corereaction.ErrReactionNotFound) {
			return nil
		}
		return err
	}
	return nil
}

// handleUndoAnnounce removes a previously created Announce (renote).
// inner.Object には元ノートの URI を持つことを想定する。
func (p *Processor) handleUndoAnnounce(act genericActivity, inner genericActivity) error {
	announcer, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		// permanent な actor resolve 失敗は ack (#1183)。announcer が居な
		// ければ Announce record も作っていないはずなので削除対象も無く
		// idempotent と整合する。
		if isPermanentSkipError(err) {
			slog.Info("federation: skipping Undo(Announce) (actor unresolvable)",
				"actor", act.Actor, "err", err)
			return nil
		}
		return err
	}
	targetURI, err := readObjectString(inner.Object)
	if err != nil {
		return err
	}
	target, err := p.resolver.ResolveNote(targetURI)
	if err != nil {
		// permanent な target resolve 失敗は ack。target が解決できなければ
		// renote record も無いはずなので、削除対象が無いまま activity を ack
		// する形で整合する (#1183)。
		if isPermanentSkipError(err) {
			slog.Info("federation: skipping Undo(Announce) (target unresolvable)",
				"object", targetURI, "actor", act.Actor, "err", err)
			return nil
		}
		return err
	}
	// announcer が pure renote を 1 件でも持っていれば削除する。複数あった場合
	// は最新の 1 件のみで十分 (本家 misskey の挙動と同じ)。
	// viewerID には announcer.ID を渡す: push-down 後 (#1500) も self-branch で
	// announcer 自身の renote は visibility を問わず全件見えるため、下の
	// `n.UserID != announcer.ID` で残す対象行は従来どおり取得できる。
	renotes, err := p.noteRepo.ListRenotesOf(target.ID, announcer.ID, "", "", 50)
	if err != nil {
		return err
	}
	for _, n := range renotes {
		if n.UserID != announcer.ID {
			continue
		}
		if !corenote.IsPureRenote(n) {
			continue
		}
		if err := p.noteRepo.Delete(n); err != nil {
			return err
		}
		// handleAnnounce の increment と同条件でのみ減算する。条件がずれると
		// 加算しなかった boost の undo で count が負に振れる (#2283)。
		//
		// なお upstream は renoteCount を減算しない (incRenoteCount しか無く、
		// note 削除時も据え置き) ため、これは mk-go の意図的な divergence。
		// unrenote 後もカウントが残り続ける方が不自然なので維持する
		// (docs/divergence.md 参照)。
		if target.UserID != announcer.ID && !announcer.IsBot {
			_ = p.noteRepo.IncrementCount(target.ID, "renoteCount", -1)
		}
		return nil
	}
	return nil
}

// lookup の失敗を ack するか伝播させるかの規則 (#3115 / #3116 / #3121)。
//
//  1. **inbox job / queue job から到達する経路は分ける。** not-found は retry しても
//     結果が変わらないので ack、それ以外は伝播させて queue に retry させる。
//     `repository.IsNotFound` で判定する。ack すると job が成功扱いになるので、
//     **一時的な接続断だけで activity が恒久的に失われる** — 相手の再送は
//     当てにできない (inbox は enqueue した時点で 202 を返す)。
//  2. **fire-and-forget な hook は ack のまま。** 配送 hook は `safeGo` で
//     投げっぱなしに呼ばれ、戻り値も retry の仕組みも無いので、error を返しても
//     行き先が無い。**理由を書いて残す** (`note_delivery_hook.go` の
//     `findNoteAuthor` に書いてある。他はそこを参照する)。
//  3. **#534 の idempotency invariant と混同しない。** あれは「retry しても結果が
//     変わらないもの」の話で、`ErrRequestNotFound` / `ErrBlocking` / `ErrBlocked` が
//     それに該当する。**retry すれば成功しうる障害はそこに入らない。**
//  4. **「確かめられなかった」は「無かった」ではない (#3121)。** 署名検証の失敗は
//     既定で ack する — 署名が合わない body を retry しても結果は変わらないため。
//     しかし検証は DB も読むので、そのまま書くと **DB 障害のあいだに届いた
//     activity まで同じ ack に落ちる**。actor / 公開鍵の lookup は
//     `ErrLookupUnavailable` で種別を残し、`inbox.go` がそれだけ retry に倒す。
//     **落とす側は 1 箇所ではない** — HTTP 署名 (`verifyPayload`)、転送 activity の
//     認可 (`authorizeActor` → LD-Signature の creator 解決と鍵引き)、Headers 無しの
//     legacy 経路 (`ldVerifier.VerifyAndCompact`) の 3 つが同じ sentinel を見る。
//
// この規則で数えれば対象は導ける (件数を別に持たない)。
//
// **#3121 時点で ack のまま残っているもの** (どれも規則 1-4 で説明が付く):
//   - `resolver.go` の `fetchActor` / `fetchNote` の失敗 — リモートが取れないという
//     相手側の事情で、**こちらの retry で取り戻せるとは限らない**。分けるなら HTTP
//     status から一時的か恒久的かを決める別の設計が要るので、ここでは触っていない。
//   - 同 `userRepo.Create` / `noteRepo.Create` の失敗 — 読み側を先に分けたので、DB が
//     落ちていれば手前の lookup で止まる。ここに来るのは列に入らない値のように
//     retry しても変わらないものが主だが、**書き込みだけが落ちている状態は
//     取りこぼす**。
//   - `post_scheduled_note.go` の publish 失敗 — 二重 publish / 二重通知を避ける
//     ための意図的な設計 (#2106 L61)。予約投稿の job は attempts を積むように
//     なったが (#3121)、retry されるのは **publish に到達する前の失敗だけ**。
//   - 配送 hook 一式 — 規則 2。

// handleAccept processes an inbound Accept activity. リモートfolloweeがローカル
// followerからのフォローリクエストを承認した場合に、フォロー関係を確立する。
// inner.id が自ホストの follow-relay URI で、かつ送信元 actor がその relay
// 自身であれば relay の Accept とみなし、RelayStatusMarker.MarkAccepted を
// 呼び出す (所有権検証は upstream に無い mk-go 側の硬化)。
func (p *Processor) handleAccept(act genericActivity) error {
	var inner genericActivity
	// 型エラーを握らないと、直後の normalizeActor (#999) が救うはずの
	// `inner.actor` が embedded object のケースに**到達できない** (#2662)。
	if err := unmarshalIgnoringTypeErrors(act.Object, &inner); err != nil {
		// objectが文字列（Follow IDのURI）の場合もあるが、現状はnilで許容
		return nil
	}
	// inner.actor が embedded object のケースも救済する (#999 / upstream #17340)。
	inner.normalizeActor(act.Object)
	// chat room invitation の Accept (remote が我々の room 招待を承認) は
	// membership 化する (#1203)。
	if strings.EqualFold(inner.Type, "invite") {
		return p.handleChatRoomAccept(act, inner)
	}
	if !strings.EqualFold(inner.Type, "follow") {
		return nil
	}
	if relayID := matchFollowRelayID(inner.ID, p.localBaseURL); relayID != "" && p.relayMarker != nil {
		// 送信元がその relay 自身でなければ状態を変えずに drop する。
		// 通さないと、署名が通る任意のリモート actor が未承認 / 拒否済みの
		// relay を accepted に倒せ、全公開ノートがその inbox へ流れ出す。
		if !p.relayStatusChangeAllowed(act.Actor, relayID, "Accept") {
			return nil
		}
		return p.relayMarker.MarkAccepted(context.Background(), relayID)
	}
	// actorはリモートfollowee（承認した側）
	followee, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	// inner.actorはローカルfollower（フォローを申請した側）
	followerURI, err := readActorString(inner)
	if err != nil {
		return nil
	}
	// ローカルユーザーはURIカラムがNULLなのでFindByURIでは見つからない。
	// ローカルURI（/users/{id}）からIDを抽出してFindByIDで検索する。
	var follower *model.User
	if localID := p.resolver.ExtractLocalUserID(followerURI); localID != "" {
		follower, err = p.userRepo.FindByID(localID)
	} else {
		follower, err = p.userRepo.FindByURI(followerURI)
	}
	if err != nil {
		// **not-found だけ ack する** (#3115)。その利用者が居ないことは retry
		// しても変わらないので、7 回 retry (試行は計 8 回、
		// `defaultInboxJobMaxAttempts`) してから dead letter に積む意味が無い。
		//
		// **DB 障害は伝播させる。** ack すると**job が成功扱いになって queue が
		// retry しない**ので、一時的な接続断だけで Accept が恒久的に失われ、
		// ローカル利用者のフォローリクエストは「リクエスト中」のまま永久に残る。
		// **相手の再送は当てにできない** — inbox は enqueue した時点で 202 を
		// 返すので、worker が ack しようが error を返そうが相手が見る応答は
		// 変わらない。#534 の idempotency invariant は「retry しても結果が
		// 変わらないもの」の話で、**retry すれば成功しうる障害はそこに入らない**
		// (#2792 と同じ判断)。
		if repository.IsNotFound(err) {
			return nil
		}
		return err
	}
	// フォローリクエストを承認してフォロー関係を確立
	if err := p.followingService.AcceptRequest(followee.ID, follower.ID); err != nil {
		// フォローリクエストが存在しない場合は無視（既にフォロー済み等）
		if errors.Is(err, corefollowing.ErrRequestNotFound) {
			return nil
		}
		// block 関係による拒否 (AcceptRequest の多層防御、#3111 レビュー) も
		// retry しても結果は変わらないので ack する。生で返すと inbox job が
		// 8 回 retry してから dead letter に積まれ、その間 inbox health の
		// 統計にも失敗として乗る (#534 の idempotency invariant)。
		if errors.Is(err, corefollowing.ErrBlocking) || errors.Is(err, corefollowing.ErrBlocked) {
			return nil
		}
		return err
	}
	return nil
}

// mergeCreateAudience unions the Create activity's to/cc onto the carried Note
// object's to/cc and fills the object's attributedTo from the activity actor
// when absent, mirroring upstream ApInboxService.create
// (third_party/misskey/.../ApInboxService.ts:403-417). The merged JSON is what
// IngestNoteWithCreated parses, so visibility (public/home/followers/specified)
// and visibleUserIds are derived from the combined audience rather than the
// Note object's own to/cc only (#1560).
//
// 本家は activity / object 双方の to/cc を同一 union 値に揃えるが、mk-go は
// object 側しか後段で参照しないので object の to/cc のみ書き戻す。union は
// 順序保持 dedup で、activity 側を先・object 側を後に連結する (本家 concat 順)。
//
// object が JSON object でない (string IRI 等) / parse 不能なケースでは元の
// act.Object をそのまま返し、既存挙動へ degrade する (fail-safe)。union 自体は
// visibility を広げる方向ではなく、Create 側にしか無い followers/specified の
// audience を拾うので、誤って public 化する事故を防ぐ fail-closed 寄りの補正。
func mergeCreateAudience(act genericActivity) json.RawMessage {
	if len(act.Object) == 0 {
		return act.Object
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(act.Object, &obj); err != nil || obj == nil {
		// object が embedded JSON object でなければ何もできない (本家も
		// `typeof activity.object === 'object'` でガードしている)。
		return act.Object
	}

	actTo := decodeAudience(act.To)
	actCC := decodeAudience(act.CC)
	objTo := decodeAudience(obj["to"])
	objCC := decodeAudience(obj["cc"])

	mergedTo := unionStrings(actTo, objTo)
	mergedCC := unionStrings(actCC, objCC)

	mutated := false
	if encoded, ok := encodeAudienceIfChanged(obj["to"], mergedTo); ok {
		obj["to"] = encoded
		mutated = true
	}
	if encoded, ok := encodeAudienceIfChanged(obj["cc"], mergedCC); ok {
		obj["cc"] = encoded
		mutated = true
	}

	// object.attributedTo が無ければ activity.actor で補う。IngestNote は空の
	// attributedTo を ErrInvalidNote で弾くので、Create actor から埋めて本家の
	// `activity.object.attributedTo = activity.actor` に揃える。
	if _, present := obj["attributedTo"]; !present && act.Actor != "" {
		if encoded, err := json.Marshal(act.Actor); err == nil {
			obj["attributedTo"] = encoded
			mutated = true
		}
	}

	if !mutated {
		return act.Object
	}
	merged, err := json.Marshal(obj)
	if err != nil {
		return act.Object
	}
	return merged
}

// decodeAudience parses an AP audience field into a []string. The value may be
// a single id, an object carrying `id`, or an array of either. Unreadable
// elements are dropped. Empty / null input yields nil.
func decodeAudience(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	// APIDList は upstream getApIds と同じく `{"id": ...}` 形式の要素も拾う。
	// `[]string` 決め打ちだと object 要素で unmarshal ごと失敗し audience が丸ごと
	// 落ちる (#2662)。
	var list activitypub.APIDList
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	return list
}

// unionStrings concatenates a then b, preserving first-seen order and dropping
// duplicates, matching upstream `unique(concat([toArray(a), toArray(b)]))`.
func unionStrings(a, b []string) []string {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, v := range a {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	for _, v := range b {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// encodeAudienceIfChanged re-encodes merged as a JSON array and reports whether
// it differs from the original raw value. Returns ok=false (no write) when the
// merged audience is empty so a missing field is not materialised as `[]`, and
// when the canonical array form already equals the original (avoids needless
// string→array normalisation churn). 元と等価なら書き戻さないことで、純粋に
// audience 追加が無い一般 note では act.Object をそのまま素通しする。
func encodeAudienceIfChanged(raw json.RawMessage, merged []string) (json.RawMessage, bool) {
	if len(merged) == 0 {
		return nil, false
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, false
	}
	if bytes.Equal(bytes.TrimSpace(raw), encoded) {
		return nil, false
	}
	return encoded, true
}

// handleCreate persists an inbound Note or Question carried by a Create activity.
// Question (投票) はNote同様にIngestNoteで処理され、Pollレコードが自動作成される。
// ノート取込み成功後、fanoutHookが配線されていればタイムライン/ストリーミングに
// 配信する (#330)。
//
// `_misskey_talk: true` flag が立った Note は CherryPick / レガシー Misskey の
// 1-on-1 chat federation。notes テーブルではなく chat_messages として処理する
// ため IngestNote をスキップして chatService にルートする (#692)。
func (p *Processor) handleCreate(act genericActivity, signer *model.User) error {
	// bearcaps (bear:) object URI は未対応として skip する (#1560、upstream
	// ApInboxService.create の 'skip: bearcaps url not supported')。
	if isBearcapURI(act.Object) {
		slog.Info("federation: skipping Create with bearcaps object url", "actor", act.Actor)
		return nil
	}
	// Mastodon 系リレーは Announce ではなく Create を LD-Signature 付きで
	// 転送する (#2338)。配送してきたのが購読中のリレーなら、著者を DB に
	// 作らず ephemeral 経路で解決する。
	//
	// signer が nil (署名なし経路 / 未配線) のときは従来どおり DB 経路に
	// 倒れる (fail-safe)。
	viaRelay := p.isRelayDelivery(signer)
	actor, err := p.resolveCreateActor(act.Actor, viaRelay)
	if err != nil {
		if errors.Is(err, ErrHostNotAllowed) {
			// federation policy で許可されない host の actor からの Create は
			// retry で解消しないため ack して drop する (#1419 review)。署名検証
			// 経路では verifyPayload が先に弾くが、署名なし inbound 経路でも
			// retry ループにならないようここでも吸収する。
			slog.Info("federation: dropping Create from non-federated host actor",
				"actor", act.Actor)
			return nil
		}
		return err
	}
	// Note の `_misskey_talk` を覗き見て chat か通常 note かを分岐する。
	// IngestNote が走る前に短絡しないと chat message が notes テーブルに
	// 流れ込んでタイムラインを汚す。1-on-1 は chatService、group (room) は
	// chatRoomReceiver が処理するのでどちらか配線済みなら probe する。
	if p.chatService != nil || p.chatRoomReceiver != nil {
		var probe struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Content string `json:"content"`
			// **MFM の原文も拾う。** `content` は HTML なので、MFM に戻すと
			// 情報が落ちる (装飾・色・絵文字・引用の改行など)。note 取り込みは
			// 同じ 3 段 (source -> _misskey_content -> content) で拾っており、
			// chat だけ `content` しか見ていなかった。
			Source *struct {
				Content   string `json:"content"`
				MediaType string `json:"mediaType"`
			} `json:"source"`
			MisskeyContent string          `json:"_misskey_content"`
			MisskeyTalk    bool            `json:"_misskey_talk"`
			To             json.RawMessage `json:"to"`
			Context        json.RawMessage `json:"@context"`
		}
		if err := json.Unmarshal(act.Object, &probe); err == nil && probe.MisskeyTalk && probe.Type == "Note" {
			mfmSource := ""
			if probe.Source != nil && probe.Source.MediaType == "text/x.misskeymarkdown" {
				mfmSource = probe.Source.Content
			}
			if mfmSource == "" {
				mfmSource = probe.MisskeyContent
			}
			// **メッセージの id を配送してきた actor のホストに縛る。**
			//
			// この分岐は `ingestCreateNote` の手前で短絡するので、通常の note に
			// 掛かる `id host == attributedTo host` の検査 (`resolver.go` の
			// `validateNote` 相当) を一度も通らない。縛らないと、署名が通る
			// リモート actor が**任意ホストの URI を名乗って chat メッセージを
			// 1 通送れる**。以後その URI を使う正規の連合 chat メッセージは
			// `FindMessageByURI` が hit して黙って捨てられる (= 相手のメッセージを
			// 先回りして潰せる)。
			//
			// **`sameDeliveryHost` を使う。`assertRequestHostMatches` では緩い。**
			// あちらは `normalizeMatchHost` 経由で **`www.` を剥がして同一視する**
			// ので、`www.remote.example` の actor が `remote.example` の id を
			// 名乗れてしまう。`normalizeMatchHost` 自身のコメントが「actor が
			// 申告する値の host 検証には `sameDeliveryHost` を使うこと」と書いて
			// いて (#2662)、chat の id はまさに actor の申告値。note 側の
			// `validateNote` も `punyHost` 比較で `www.` を剥がさない。
			//
			// **1-on-1 と room の両方の手前に置く。** どちらも同じ `probe.ID` を
			// メッセージの uri として保存する。
			if !sameDeliveryHost(act.Actor, probe.ID) {
				slog.Warn("federation: chat message id host does not match delivering actor",
					"actor", act.Actor, "id", probe.ID)
				// retry では解決しないので ack して drop する。
				return ErrUnsupportedActivity
			}
			// note の @context が room URI なら group chat message (#1209)。
			// それ以外は従来の 1-on-1 DM。
			if roomURI, isRoom := chatRoomURIFromContext(probe.Context); isRoom {
				return p.handleChatRoomMessageCreate(actor, probe.ID, probe.Content, mfmSource, roomURI)
			}
			return p.handleChatCreate(actor, probe.ID, probe.Content, mfmSource, probe.To)
		}
	}
	// 本家 ApInboxService.create は resolve 前に activity.to/cc を Note object の
	// to/cc に union し、object.attributedTo が無ければ activity.actor で埋める
	// (#1560)。これをしないと audience を Create 側だけに載せる実装 (一部 Mastodon
	// 系) の note が followers/specified を public 等に誤判定し得る。visibility は
	// fail-closed に倒すため union 後の object を ingest に渡す。union に失敗した
	// 場合は元の object を使う (= 既存挙動へ degrade)。
	object := mergeCreateAudience(act)
	// act.Actor を配送 actor として渡し、note の著者 (attributedTo) が配送者本人で
	// あることを検証させる (なりすまし forge 防止、#1839)。
	note, created, err := p.ingestCreateNote(object, act.Actor, viaRelay)
	if errors.Is(err, corenote.ErrContainsTooManyMentions) {
		// upstream Misskey #17167 (= 2026.5.0 fix / triage #1004): role policy
		// 由来の "note contains too many mentions" は永続的に解決しない error な
		// ので queue retry に乗せず ack して drop する。upstream は同種 error を
		// IdentifiableError('9f466dab-...') で switch する patch だが、mk-go は
		// sentinel error の errors.Is で同等の non-retry 化を行う。
		slog.Info("federation: dropping inbound note exceeding mentionLimit",
			"actor", act.Actor, "limit", corenote.DefaultMentionLimit)
		return nil
	}
	if errors.Is(err, ErrHostNotAllowed) {
		// federation policy (none / specified / blockedHosts) で許可されない
		// host の Create(Note) は retry で解消しないため ack して drop する
		// (#1419 review)。handleAnnounce 等は isPermanentSkipError で吸収する
		// が、handleCreate は他の error を retry に乗せる方針なので明示分岐。
		slog.Info("federation: dropping inbound Create(Note) from non-federated host",
			"actor", act.Actor)
		return nil
	}
	if errors.Is(err, ErrNoteAttributionMismatch) {
		// id/attributedTo host 不一致・attribution≠配送actor (なりすまし forge) は
		// retry で解消しないため ack して drop する (#1839)。malformed note
		// (ErrInvalidNote) は従来どおり下の err 分岐で surface する。
		slog.Info("federation: dropping forged inbound Create(Note) (attribution mismatch)",
			"actor", act.Actor)
		return nil
	}
	if err != nil {
		return err
	}
	if note != nil {
		// IngestNoteが既存ノートを返した場合 (重複) でもfanout自体は
		// べき等なので呼んで問題ない (通知フックは冪等でないので下で created
		// ゲートを掛ける)。authorはnoteに紐付くUserを使うが、
		// IngestNoteはUser fieldを設定しないためactorを使う。
		// Renote/Reply relation は IngestNote 直後では未ロード (ID だけ) な
		// ので、ストリーミング payload で renote 先が null にならないように
		// preload 付きで再取得する (#416)。
		//
		// Reply / Renote が無い「素の Create(Note)」(連合の大半を占める一般
		// 投稿) では preload する relation が無いので reload を skip する
		// (#569)。User 関係だけ caller の actor から手で詰める。これにより
		// inbox worker drain time の支配的コストの 1 つだった redundant
		// SELECT が消える。
		hydrated := note
		if note.ReplyID != nil || note.RenoteID != nil {
			hydrated = hydrateNoteForFanout(p.noteRepo, note)
		} else if note.User == nil {
			note.User = actor
		}
		// fanoutHook / notificationHook はベストエフォートで、local note
		// create 側 (note_create_service) では既に safeGo で非同期発火して
		// いる。federation 側もこれに揃え、worker が Redis LPUSH / publish
		// 待ちで block されないようにする (#569)。順序保証は Misskey TS と
		// 同じく各 hook 側の冪等性で吸収する。
		if p.fanoutHook != nil {
			safeGoFedHook(func() { p.fanoutHook.OnNoteCreated(hydrated, actor) })
		}
		// **リレー経由では発火させない (#2743)。** Mastodon 系リレーは元の
		// Create をそのまま転送して署名だけがリレーのものになるので、
		// ingestCreateNote と同じ viaRelay で判定する。これを見落とすと
		// ephemeral 有効時に DB に行が無い note が antenna の ZSET に積まれ、
		// 上限 200 を幽霊 ID が埋める。
		// **`created` では gate しない。** notification / chart hook は dedup hit を
		// 除くが、antenna で同じことをすると「リレーや Announce の target 解決が
		// 先に DB へ入れた note は、その後の直接配送でも antenna に入らない」に
		// なる (ephemeral 無効時、リレー経由でも DB 行はできる)。リレーを購読
		// している構成では取りこぼしが恒常化する。再配送での WS 重複は
		// fanoutHook も同じく許している側で、こちらのほうが軽い。
		if p.antennaHook != nil && !viaRelay {
			safeGoFedHook(func() { p.antennaHook.OnNoteCreated(hydrated, actor) })
		}
		// 通知フックは dedup hit では発火させない。**通知は冪等ではない** —
		// 呼ぶたびに新しい id で 1 件積まれるので、同じ Create(Note) が再配送
		// されると受信者の通知一覧に同じメンション / DM が 2 件並ぶ。実運用の
		// インスタンスでは配送リトライや inbox / sharedInbox への二重投げが
		// 日常的に起きるため、fanout (note id で冪等) と同じ扱いにはできない。
		//
		// upstream も通知は NoteCreateService.create の中でしか作らないので、
		// 既存ノートを返した経路では通知が出ない (= ここと同じ挙動)。
		if p.notificationHook != nil && created {
			safeGoFedHook(func() {
				// reply / quote / mention 通知を local notifiee に対して
				// 生成する。hydrated.Reply / hydrated.Renote は preload
				// 済みなので、notification hook 側で UserID を取れる
				// (#415)。
				p.notificationHook.OnNoteCreated(hydrated, actor, hydrated.Reply, hydrated.Renote)
			})
		}
		// chart hook は dedup hit では発火させない (#1156)。同 URI の
		// Create activity が重複配送されたとき (= リトライ / S2S 二重投げ)
		// に PerUserNotesChart の inc 列が +2 されないようにする。
		if p.noteChartHook != nil && created {
			safeGoFedHook(func() { p.noteChartHook.OnNoteCreated(hydrated) })
		}
	}
	return nil
}

// safeGoFedHook runs fn in a new goroutine, recovering from panics. local
// note service の safeGo と同じ振る舞い (`note_create_service.go`)。
func safeGoFedHook(fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("recovered panic in federation best-effort hook", "panic", r)
			}
		}()
		fn()
	}()
}

// hydrateNoteForFanout reloads a freshly-created note via
// FindByIDWithRelations so that Renote/Reply relations are preloaded before
// handing it to the fanout hook. reload が失敗した場合は元の note をその
// まま返す (best-effort)。
func hydrateNoteForFanout(repo interface {
	FindByIDWithRelations(id string) (*model.Note, error)
}, note *model.Note) *model.Note {
	if repo == nil || note == nil {
		return note
	}
	if loaded, err := repo.FindByIDWithRelations(note.ID); err == nil && loaded != nil {
		return loaded
	}
	return note
}

// handleLike attaches a reaction to a local note based on a Like activity.
//
// Note: P5 (#417) で reversi 早期分岐を入れた都合上、reactionService nil
// チェックを reversi 分岐の後に移動している。副作用として:
//   - ResolveActor が無条件で呼ばれる (旧実装は reactionService nil 時に
//     早期 return していた)。reversi 分岐側でも actor 解決は必要なので
//     妥当な変更。reactionService が wire されていない config は production
//     には無いので実害なし (#417 P5 Devin review)。
//   - object が malformed (string でも nested object でもない) Like は
//     reactionService の有無に関わらず ErrUnsupportedActivity (= 202 ack)
//     を返す。旧実装は reactionService 配線時に readObjectString error を
//     400 として返していたが、404 / 400 で peer の retry を誘発するより
//     202 で握り潰すほうが連合衛生的に望ましい (#417 P5 Devin review)。
func (p *Processor) handleLike(act genericActivity) error {
	reactor, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		// permanent な actor resolve 失敗 (削除済 / visibility 制限 /
		// authorized fetch 拒否 / malformed JSON) は activity を ack して
		// failed bucket 行きを避ける (#1183)。reactor が居なければ reaction
		// 自体作れないので skip = noop。transient (5xx / network) は従来通り
		// retry サイクルに乗せる。
		if isPermanentSkipError(err) {
			slog.Info("federation: skipping Like (actor unresolvable)",
				"actor", act.Actor, "err", err)
			return nil
		}
		return err
	}
	// アクティビティ全体を Like として解釈し content/_misskey_reaction を取得する。
	// object は URI 文字列の場合とネストされた Like オブジェクトの場合がある。
	var like activitypub.Like
	_ = json.Unmarshal(act.raw, &like)
	if like.Object == "" {
		// object が文字列の場合: Activity.Object は json:"-" でスキップされるため
		// act.Object (RawMessage) から直接読む
		uri, rerr := readObjectString(act.Object)
		if rerr != nil {
			return ErrUnsupportedActivity
		}
		like.Object = uri
	}
	reaction := like.Content
	if like.MisskeyReaction != "" {
		reaction = like.MisskeyReaction
	}
	// upstream like() は reaction を `_misskey_reaction ?? content ?? name` で解決
	// する。一部の Mastodon-fork / Pleroma 系 EmojiReact は shortcode を `name` に
	// 乗せるため、content/_misskey_reaction が無ければ name を fallback に使う
	// (#1948-21)。like.Name は埋め込み Object の name (AS activity の name)。
	if reaction == "" {
		reaction = like.Name.String()
	}
	// reversi game session URI (`/games/{UUID}/{sessionID}`) は CherryPick
	// 拡張の reaction 連合。純正 Misskey フロントは `reacted` を表示する UI を
	// 持たないので mk-go では state 変化させず 202 ack だけ返す (#417 P5)。
	// resolver.ResolveNote が 404 で失敗するのを避けるため Note Like より先に
	// 弾く必要がある。
	if corereversi.IsReversiGameSessionURI(like.Object) {
		// reactor / reaction は note Like 経路で使われるので blank assign 不要。
		// actor 解決は handleLike 冒頭で済ませているのでキャッシュ取込の
		// 副作用はこの早期 return でも残る。
		return nil
	}
	if p.reactionService == nil {
		return ErrUnsupportedActivity
	}
	target, err := p.resolver.ResolveNote(like.Object)
	if err != nil {
		// permanent な target resolve 失敗 (followers-only / 削除済 等) は
		// reaction record を作らず ack。retry しても永久に解消しない (#1183)。
		if isPermanentSkipError(err) {
			slog.Info("federation: skipping Like (target unresolvable)",
				"object", like.Object, "actor", act.Actor, "err", err)
			return nil
		}
		return err
	}
	// Like activity に乗ってきたカスタム絵文字メタデータを emoji table に
	// upsert する。Misskey TS の apNoteService.extractEmojis(...) 相当
	// (#459)。reactionService.Create 自体は emoji 不在でも reaction 文字列
	// を保存できるので、upsert 失敗で Create を止める必要はなく、UI 表示
	// 側の reactionEmojis 解決が FallbackReaction (heart) に落ちる程度。
	if reactor.Host != nil && len(like.Tag) > 0 {
		p.resolver.upsertEmojis(extractEmojiTags(like.Tag), *reactor.Host)
	}
	if _, err := p.reactionService.Create(reactor, target.ID, reaction); err != nil {
		if errors.Is(err, corereaction.ErrAlreadyReacted) {
			return nil
		}
		// reaction service が visibility 違反 (`ErrNoteNotVisible`) 等を
		// 返すケースも permanent 扱いで skip (#1183)。
		if isPermanentSkipError(err) {
			slog.Info("federation: skipping Like (reaction policy violation)",
				"object", like.Object, "actor", act.Actor, "err", err)
			return nil
		}
		return err
	}
	return nil
}

// handleAnnounce creates a renote pointing at the announced note.
// signer は antenna hook の relay 判定にのみ使う。既存の viaRelay
// (announcer が relay actor 本人か) は renote 抑止の判定で、そちらの意味は
// 変えない — Mastodon 系リレーが転送した Announce は従来どおり renote を作る。
func (p *Processor) handleAnnounce(act genericActivity, signer *model.User) error {
	// bearcaps (bear:) object URI は未対応として skip (#1560、upstream
	// ApInboxService.announce の 'skip: bearcaps url not supported')。
	if isBearcapURI(act.Object) {
		slog.Info("federation: skipping Announce with bearcaps object url", "actor", act.Actor)
		return nil
	}
	announcer, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		// permanent な actor resolve 失敗は ack して skip (#1183、handleLike
		// と同じ理由)。
		if isPermanentSkipError(err) {
			slog.Info("federation: skipping Announce (actor unresolvable)",
				"actor", act.Actor, "err", err)
			return nil
		}
		return err
	}
	targetURI, err := readObjectString(act.Object)
	if err != nil {
		return err
	}
	// relay 由来かどうかは **note を解決する前に** 判定する。ResolveNote は
	// 解決と同時に DB へ永続化するので、後から判定したのでは「リレー投稿を
	// DB に入れない」(#2332) が成立しない。
	//
	// announcer.Inbox / SharedInbox が registered relay の Inbox と一致した
	// 場合のみ relay 由来と判定。
	viaRelay := p.relayActorChecker != nil && p.relayActorChecker.IsRelayActor(announcer)

	var target *model.Note
	targetCreated := false
	if viaRelay {
		// リレー経由でしか観測しない投稿は Redis に置く。ephemeral store が
		// 未配線 / 機能無効なら ResolveNoteEphemeral が通常経路に倒れる。
		target, err = p.resolver.ResolveNoteEphemeral(targetURI)
	} else {
		// **新規取り込みかどうかは resolver から受け取る** (#2751)。取得 URI で
		// 既存行を引く方式では、Announce の object が document id と別名の URL
		// (`https://h/@user/1` に対し id は `https://h/notes/1`) のときに何度
		// 取り込んでも「新規」に見え、既知の note を繰り返し antenna へ押し込める。
		target, targetCreated, err = p.resolver.ResolveNoteWithCreated(targetURI)
	}
	if err != nil {
		// permanent な target resolve 失敗 (削除済 note / followers-only) は
		// Announce 記録を skip して ack (#1183)。
		if isPermanentSkipError(err) {
			slog.Info("federation: skipping Announce (target unresolvable)",
				"object", targetURI, "actor", act.Actor, "err", err)
			return nil
		}
		return err
	}
	// upstream Misskey #17308 (= 2026.5.0 fix / triage #1002): relay 由来の
	// Announce は renote として保存せず、target note 自体を timeline stream に
	// publish する。renote 表示にしてしまうと「relay actor がリノートした」と
	// いう偽装表示になり、原投稿者からの直接配送と区別がつかなくなるため。
	if viaRelay {
		return p.publishRelayDeliveredNote(target, targetURI, act.Actor)
	}
	// upstream announceNote の 2 つの skip guard (#1560、ApInboxService.ts:350-361):
	//   1. !isVisibleForMe(renote, actor): boost は public/home の note にしか
	//      行えない。followers / specified の note を announce してくる activity は
	//      不正なので drop する (mk-go は remote-remote の follow graph を持たない
	//      ため、isVisibleForMe を「対象が public/home か」で近似する)。
	//   2. activity.published < 対象 note の createdAt: announce が対象 note より
	//      前に published されたと主張する malformed timestamp は drop する。
	if target.Visibility != model.NoteVisibilityPublic && target.Visibility != model.NoteVisibilityHome {
		slog.Info("federation: skipping Announce of non-public note", "object", targetURI, "actor", act.Actor, "visibility", target.Visibility)
		return nil
	}
	// **ブースト対象そのものを antenna に載せる** (#2751)。hook に渡る renote 行は
	// Text / CW を持たないので、キーワード指定または withFile の antenna は
	// inbound Announce では原理的に 1 件もマッチしない (upstream は
	// ApInboxService.ts:329 の resolveNote(target) 経由で target 自体を
	// NoteCreateService へ通すので載る)。
	//
	// **upstream より後ろ、可視性 gate の直後に置く。** upstream は resolveNote の
	// 時点で載せるが、mk-go の antenna 可視性判定 (corenote.CanSeeNote) は
	// upstream の checkHitAntenna より緩く、followers note でも「viewer が
	// mention されている / reply 先が viewer」なら通す (#2106 N27 の意図的緩和)。
	// resolveNote の位置に置くと、crafted Announce を投げるだけで**配送されて
	// いない followers-only note を対象ユーザーの antenna に載せられる**入口が
	// できる。public/home に絞った後なら、その入口は開かない。
	//
	// **新規に取り込んだときだけ発火させる。** 無条件だと再ブーストのたびに古い
	// note が antenna に湧く。upstream も既知の note では createNote を通らない
	// ので載らない。
	//
	// **ローカル note は対象外。** 作成時に既に antenna を通っている。
	// **canonical なローカル URI は `targetCreated` が既に弾く** —
	// `extractLocalNoteID` (urls.NoteURI の前方一致) → `FindByID` hit で
	// created=false になるため。その範囲では UserHost の条件は二重で、片方を
	// 外してもテストは落ちない (等価変異)。ただし前方一致から外れるローカル URL
	// は remote 扱いで fetch されうるので、条件としては残す。
	//
	// **リレー経由は #2743 の方針どおり外す。** 取りこぼしの範囲は
	// docs/divergence.md に記載。`!viaRelay` はここに到達する時点で常に真
	// (上の early return を通る) で、renote 側の hook と同じく将来 early return
	// を消したときの保険。実際に止めているのは signer 側の判定。
	if p.antennaHook != nil && targetCreated && !viaRelay && !p.isRelayDelivery(signer) &&
		target.UserHost != nil && *target.UserHost != "" {
		hydratedTarget, author := p.hydrateWithAuthor(target)
		if author == nil {
			// replica 遅延などで直前に入れた行の author を引けないことがある。
			// 黙って落とすと「antenna に載らない」だけが残るのでログに出す。
			slog.Warn("federation: announced target has no resolvable author, skipping antenna",
				"noteURI", targetURI, "userId", target.UserID)
		} else {
			safeGoFedHook(func() { p.antennaHook.OnNoteCreated(hydratedTarget, author) })
		}
	}
	if act.Published != "" {
		if published, perr := time.Parse(time.RFC3339, act.Published); perr == nil {
			if targetCreatedAt, terr := p.resolver.idGen.ParseTime(target.ID); terr == nil && published.Before(targetCreatedAt) {
				slog.Info("federation: skipping Announce with malformed createdAt (published before target)",
					"object", targetURI, "actor", act.Actor)
				return nil
			}
		}
	}
	// announce 自身に id があれば URI として保存し、重複検出にも使う。
	// upstream Misskey #17356 (= 2026.5.1 fix) は同 activity の重複処理 race を
	// Redis 分散 lock で防ぐが、mk-go は single-instance 前提で DB unique 制約 +
	// 本 FindByURI 経由の dedup によって race を防いでいる。multi-instance 構成
	// (mk-go pod 複数台 load balance) を採用する場合は別途 Redis lock 化を検討
	// (= triage #1009 / upstream #17356 close)。
	if act.ID != "" {
		// `note.uri` は varchar(512)。**Announce の id は renote の身元**で、
		// 下の dedup (`FindByURI`) の鍵でもあるので切らずに拒否する (#2723)。
		// 切ると別の行と衝突する。
		//
		// **DB を引く前に見る。** NUL 入りの値は `FindByURI` の SELECT 自体が
		// 22021 で落ちる。
		//
		// **この error は上位に surface する** — `dispatchActivity` がそのまま返すので
		// inbox job は retry を使い切って dead になる (既定 8 回)。そもそも保存
		// できない activity なので結末としては正しい。
		//
		// **ここでの利得は「早く落とす」ことではない。** gate は `ResolveNote` の後
		// なので、対象 note の fetch と永続化は既に済んでいる。得られるのは、原因が
		// 22001 ではなく明示的な拒否として残ること。
		//
		// **NUL も届く。** `url.Parse` は制御文字を弾くが、**fragment だけは例外**で
		// `https://h/a#<NUL>` は parse を通る (実測)。`authorizeActor` の host 検査も
		// `Hostname()` を見るだけなので抜ける。「NUL は来ないから長さだけ見ればいい」に
		// 縮めないこと。
		if !fitsColumn(act.ID, noteURIMaxRunes) {
			slog.Warn("federation: rejecting Announce whose id does not fit note.uri",
				"id", truncateRunes(act.ID, noteURIMaxRunes))
			return ErrInvalidNote
		}
		switch _, err := p.noteRepo.FindByURI(act.ID); {
		case err == nil:
			// 既に取り込み済み。
			return nil
		case !repository.IsNotFound(err):
			// **障害を「重複でない」に倒さない** (#3116)。倒すと DB 障害のあいだ
			// 同じ Announce が二重の renote になる。
			return fmt.Errorf("announce dedup: %w", err)
		}
	}
	// 遅延配送 Announce では activity.published を採用して timeline 並びを
	// origin と揃える (#940)。空 / parse 不可なら nowFn にフォールバック。
	now := parseAPPublishedTime(act.Published, nowFn())
	// upstream announceNote は activity の to/cc から renote の visibility を決める。
	// 旧実装は public 固定で、home renote 由来の Announce (Misskey renderAnnounce は
	// home を to=[followers], cc=[Public] で送る) が public 化され public TL / RSS /
	// 連合配送に乗っていた (#1826)。inbound Note と同じ deriveVisibility で揃える
	// (Misskey renderAnnounce の public=to[Public] / home=to[followers]+cc[Public] /
	// followers=to[followers] shape を正しく判定する)。実装が送らない exotic shape
	// (cc のみ Public 等) での upstream parseAudience との差、および specified renote の
	// visibleUsers / 通知の扱いは #1864 で別途対応する。
	renoteVisibility := deriveVisibility(decodeAudience(act.To), decodeAudience(act.CC))
	// upstream NoteCreateService は renote の visibility を対象 note 以下に clamp する
	// (home note は public で renote できず home に落ちる)。target は上の gate で
	// public/home に限定済みなので、home target に対する public boost を home に
	// 落とす。これが無いと crafted Announce(to=[Public], object=home note) で home
	// note を global timeline に leak させられる。
	if target.Visibility == model.NoteVisibilityHome && renoteVisibility == model.NoteVisibilityPublic {
		renoteVisibility = model.NoteVisibilityHome
	}
	renote := &model.Note{
		ID:         p.resolver.idGen.Generate(now),
		UserID:     announcer.ID,
		UserHost:   announcer.Host,
		RenoteID:   &target.ID,
		Visibility: renoteVisibility,
	}
	if act.ID != "" {
		// 上の gate で列に入ることを確かめてある。
		uri := act.ID
		renote.URI = &uri
	}
	renote.RenoteUserID = &target.UserID
	renote.RenoteUserHost = target.UserHost
	if err := p.noteRepo.Create(renote); err != nil {
		return err
	}
	// upstream NoteCreateService の incRenoteCount 条件
	// (`data.renote.userId !== user.id && !user.isBot`) を inbound Announce
	// にも適用する。自己 boost と bot の boost は加算しない (#2283)。
	// Undo(Announce) 側の減算も同条件にして対称性を保つこと。
	if target.UserID != announcer.ID && !announcer.IsBot {
		_ = p.noteRepo.IncrementCount(target.ID, "renoteCount", 1)
	}
	hydrated := hydrateNoteForFanout(p.noteRepo, renote)
	// hook はベストエフォートで safeGoFedHook 経由で非同期発火する (#1158)。
	// handleCreate (#569) と同じく Redis LPUSH / publish 待ちで inbox worker
	// drain を block しないことが目的。順序保証は handleCreate と同様に各 hook
	// 側の冪等性で吸収する。closure capture する `hydrated` / `announcer` /
	// `target` は handleAnnounce 内 local 変数で関数 return 後も生存するため
	// goroutine 上の参照は安全 (race なし)。
	if p.fanoutHook != nil {
		safeGoFedHook(func() { p.fanoutHook.OnNoteCreated(hydrated, announcer) })
	}
	// **リレー経由では発火させない (#2743)。** Misskey 系リレー (announcer が
	// relay actor 本人) は上の `if viaRelay` で publishRelayDeliveredNote へ
	// 抜けるので、ここの `!viaRelay` は実質常に true — early return を将来
	// 消したときの保険として残してある。実際にここで止めているのは Mastodon
	// 系リレーが他人の Announce を転送する形で、それは signer 側で判定する。
	//
	// **signer が nil のときは素通しする** (isRelayDelivery の fail-safe)。
	// production の inbox は必ず signer を積むので到達しない。
	//
	// **ここで渡すのは renote 行。** renote は text を持たないので、キーワード
	// 指定のある antenna はこの行では当たらない。ブースト対象そのものは上の
	// #2751 の block が別途渡している (新規取り込み時のみ)。
	if p.antennaHook != nil && !viaRelay && !p.isRelayDelivery(signer) {
		safeGoFedHook(func() { p.antennaHook.OnNoteCreated(hydrated, announcer) })
	}
	if p.notificationHook != nil {
		// remote user が local note を renote した時に元投稿者へ通知を出す
		// (#415)。target が renoteTarget。reply 通知は Announce では発生
		// しないので nil を渡す。
		safeGoFedHook(func() {
			p.notificationHook.OnNoteCreated(hydrated, announcer, nil, target)
		})
	}
	// chart hook 発火 (#1156)。dedup チェック (上部の `act.ID != ""` ゲート付き
	// FindByURI) を通り抜けて noteRepo.Create(renote) も成功した時点で「この
	// 経路では」新規作成と確定するので、created flag は持たない。
	//
	// 例外として `act.ID` が空 (= activity id を持たない Announce) の場合は
	// 上の dedup 自体が skip されるため、同じ Boost が複数回届くと renote 行も
	// 毎回新規に生成され chart も毎回 fire する。これは本 PR で導入した挙動
	// ではなく既存の handleAnnounce 設計に従ったもの。activity id 無しの
	// Announce を出す実装は実運用ではほぼ無いので影響軽微だが、`act.ID` の
	// dedup 強化は別 issue でフォロー (= renote 重複そのものの解消が主題)。
	if p.noteChartHook != nil {
		safeGoFedHook(func() { p.noteChartHook.OnNoteCreated(hydrated) })
	}
	return nil
}

// publishRelayDeliveredNote handles a relay-delivered Announce (= no renote
// creation, just publish the target note to timeline streams). 仕様詳細は
// handleAnnounce 内の relay-delivered コメント参照 (upstream #17308 / triage
// #1002)。
//
// target の author を最も信頼できる relation 経由で解決する: まず
// hydrateNoteForFanout で User relation を pre-load し、それが空なら
// userRepo.FindByID(target.UserID) に fallback する。author が解決できない
// 場合 (= target.UserID も無い等の broken state) は fanoutHook を呼ばずに
// 戻る (safe degrade)。
//
// renoteCount は increment しない (= target は新規 renote 対象ではない)、
// notificationHook も呼ばない (= 原投稿者には relay 経由配送の通知を出さない、
// 上流 / 他 instance から直接届く Create で通知済みの可能性が高いため二重通知
// 回避)。
func (p *Processor) publishRelayDeliveredNote(target *model.Note, targetURI, relayActor string) error {
	hydrated, author := p.hydrateWithAuthor(target)
	if author == nil {
		slog.Warn("federation: relay-delivered note has no resolvable author, skipping fanout",
			"relay", relayActor, "noteURI", targetURI, "userId", target.UserID)
		return nil
	}
	if p.fanoutHook != nil {
		safeGoFedHook(func() { p.fanoutHook.OnNoteCreated(hydrated, author) })
	}
	// **antennaHook はここでは発火させない (#2743)。** 理由は AntennaHook の
	// doc を参照。handleCreate / handleAnnounce との差はこの 1 点だけなので、
	// 「揃っていない」と見て足さないこと。
	slog.Debug("federation: relay-delivered note published",
		"relay", relayActor, "noteURI", targetURI, "authorId", author.ID)
	return nil
}

// hydrateWithAuthor pre-loads a note's relations for hook delivery and resolves
// its author, falling back to userRepo when the User relation is empty.
// author が解決できない (broken state) 場合は nil を返すので、呼び出し側は
// hook を呼ばずに戻ること。
func (p *Processor) hydrateWithAuthor(n *model.Note) (*model.Note, *model.User) {
	hydrated := hydrateNoteForFanout(p.noteRepo, n)
	author := hydrated.User
	if author == nil && p.userRepo != nil && n.UserID != "" {
		if u, err := p.userRepo.FindByID(n.UserID); err == nil {
			author = u
		}
	}
	return hydrated, author
}

// isRelayDelivery reports whether the activity was forwarded by a subscribed
// relay, based on the verified HTTP signer (#2338).
//
// signer が nil (署名なし経路 / verifier 未配線) なら false。判定できない
// ときはリレー扱いしない = 従来どおり DB に保存する fail-safe。
func (p *Processor) isRelayDelivery(signer *model.User) bool {
	return signer != nil && p.relayActorChecker != nil && p.relayActorChecker.IsRelayActor(signer)
}

// resolveCreateActor resolves the author of an inbound Create, keeping
// relay-forwarded authors out of the database (#2338).
func (p *Processor) resolveCreateActor(uri string, viaRelay bool) (*model.User, error) {
	if viaRelay {
		return p.resolver.ResolveActorEphemeral(uri)
	}
	return p.resolver.ResolveActor(uri)
}

// ingestCreateNote persists an inbound Create's note, diverting relay-forwarded
// ones to the ephemeral store (#2338).
func (p *Processor) ingestCreateNote(object []byte, deliveringActorURI string, viaRelay bool) (*model.Note, bool, error) {
	if viaRelay {
		return p.resolver.IngestNoteEphemeral(object, deliveringActorURI)
	}
	return p.resolver.IngestNoteWithCreated(object, deliveringActorURI)
}

// handleDelete removes a remote note (or actor) referenced by a Delete activity.
func (p *Processor) handleDelete(act genericActivity) error {
	targetURI, err := readObjectString(act.Object)
	if err != nil {
		return err
	}
	// upstream Misskey #17294 (= 2026.5.0 fix / triage #1001): object が Actor
	// (self-delete または object.type が Actor 系) で、その actor がローカルに
	// 存在しないなら無視する。これをやらないと ResolveActor が remote fetch を
	// 試みて 404 で失敗 → queue retry に乗り続ける挙動になる (= "存在しない
	// actor の delete を受け取り続けて queue が膨れる" 既存 bug)。
	if isActorDelete(act.Actor, targetURI, act.Object) {
		if _, ferr := p.userRepo.FindByURI(targetURI); ferr != nil {
			// **not-found だけ ack する** (#3116)。存在しない actor の delete は
			// retry しても結果が変わらないので、ここで止めないと queue が膨れる
			// (上のコメントの元の動機)。
			//
			// **DB 障害は伝播させる。** 以前は「他の DB error も ignore して
			// retry を避ける方がマシ」と書いて**種別を見ずに ack** していたが、
			// それだと一時的な接続断だけでリモートのアカウント削除を取りこぼす。
			// #3115 が Accept / Reject で確立した原則と正面から衝突していた。
			if repository.IsNotFound(ferr) {
				return nil
			}
			return fmt.Errorf("actor delete: lookup actor: %w", ferr)
		}
		// 存在する actor の delete は従来通り resolve に進む。
	}
	author, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	// Actor 自身の Delete (アカウント削除) は tombstone + cascade purge する
	// (#1220, upstream ApInboxService.deleteActor 相当)。
	if author.URI != nil && *author.URI == targetURI {
		return p.handleActorDelete(author)
	}
	note, err := p.noteRepo.FindByURI(targetURI)
	if err != nil {
		// 既に存在しないなら成功扱い。**障害は伝播させる** (#3116) —
		// ack すると job が成功扱いになって retry されず、**削除されたはずの
		// ノートがこちらに残り続ける**。
		if repository.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete note: lookup note: %w", err)
	}
	if note.UserID != author.ID {
		return errors.New("delete from non-author")
	}
	if p.noteDeleteSvc != nil {
		return p.noteDeleteSvc.Delete(author, note.ID)
	}
	return p.noteRepo.Delete(note)
}

// handleActorDelete processes a remote actor's self-delete (account deletion):
// the user is tombstoned (isDeleted=true) and a cascade purge of their notes /
// drive files / following is enqueued. Mirrors upstream
// ApInboxService.deleteActor.
//
// Local users are never deleted via inbound AP (loopback / spoof guard), and
// an already-deleted user is a no-op so AP retries stay idempotent. Marking
// runs before enqueue (upstream order); a purge-enqueue failure is logged but
// not retried (the tombstone is already committed, and re-running would skip on
// the idempotency guard anyway).
func (p *Processor) handleActorDelete(actor *model.User) error {
	if actor.IsLocal() {
		return nil
	}
	if actor.IsDeleted {
		return nil
	}
	if err := p.userRepo.UpdateUser(actor.ID, map[string]any{"isDeleted": true}); err != nil {
		return fmt.Errorf("actor delete: mark deleted: %w", err)
	}
	if p.accountDeleteEnqueuer != nil {
		// #2230: inbound Delete(actor) は remote user なので Soft=true で行を tombstone として
		// 残す。物理削除すると再連合 (resolve) でアカウントが復活し得る (upstream 同様)。
		if err := p.accountDeleteEnqueuer.EnqueueDeleteAccount(queue.DeleteAccountPayload{UserID: actor.ID, Soft: true}); err != nil {
			slog.Warn("actor delete: enqueue cascade purge failed", "userId", actor.ID, "err", err)
		}
	}
	return nil
}

// isActorDelete reports whether the given Delete activity targets an actor.
// Heuristics: (a) actor URI と object URI が一致するなら self-delete、
// (b) object が embedded 形式で type が Actor 系 (Person / Service / Application
// / Group / Organization) なら actor delete とみなす。
// upstream Misskey の isActor() / getApId() の組合せに相当する判定。
func isActorDelete(actorURI, targetURI string, object json.RawMessage) bool {
	if actorURI != "" && actorURI == targetURI {
		return true
	}
	switch strings.ToLower(peekObjectType(object)) {
	case "person", "service", "application", "group", "organization":
		return true
	}
	return false
}

// handleUpdate refreshes a remote actor's stored profile fields, or applies
// an inbound Update Note activity from a federated peer.
//
// Misskey 本家にはノート編集 API が無いが、Mastodon 系の Update Note は受信
// するだけ受信して反映する (`Resolver.UpdateRemoteNote` 経由)。それ以外の
// type (Question / Article 等) は no-op。
// Reversi Game 型の Update (CherryPick 拡張) はこの中で分岐する。
func (p *Processor) handleUpdate(act genericActivity) error {
	// 先に object の type を覗いて Note / Person / Game の判定を行う。
	objectType := peekObjectType(act.Object)
	if strings.EqualFold(objectType, "game") {
		// Game オブジェクトは reversi 拡張専用。未ワイヤなら skip。
		if !p.reversiReady() {
			return nil
		}
		if err := p.handleReversiUpdate(act); err != nil {
			if errors.Is(err, ErrNotReversiGame) {
				return nil
			}
			return err
		}
		return nil
	}
	if strings.EqualFold(objectType, "note") {
		// act.Actor を渡して note 著者との一致を検証させる (#1819、Question 経路と対称)。
		_, err := p.resolver.UpdateRemoteNote(act.Object, act.Actor)
		// ErrInvalidNote は受信側の不備として skip 扱い (200 を返す)。
		if errors.Is(err, ErrInvalidNote) {
			return nil
		}
		return err
	}
	if strings.EqualFold(objectType, "question") {
		// inbound Update(Question): リモート poll の vote 数を refresh する
		// (upstream ApInboxService.update -> apQuestionService.updateQuestion、#1779)。
		return p.resolver.UpdateRemoteQuestion(act.Object, act.Actor)
	}

	// object が単なる URI なら fetch、object 内に Person があればそれを使う
	var person activitypub.Person
	if err := json.Unmarshal(act.Object, &person); err != nil || person.ID == "" {
		// object が文字列 URI のケース
		uri, rerr := readObjectString(act.Object)
		if rerr != nil {
			return rerr
		}
		person.ID = uri
	}
	if person.Type != "" && !strings.EqualFold(person.Type.String(), "person") {
		// Note / Question / Game は上で dispatch 済。残り (Article 等) は未対応。
		return nil
	}
	// upstream ApInboxService.update は配送 actor 本人 (actor.uri) のみを再取得・
	// 更新する。object の id が配送 actor と異なる Update(Person) は、別 actor の
	// TTL-bypass 強制再取得 (amplification) を誘発するため拒否する (Note/Question
	// 経路と対称、#1848)。act.Actor は authorizeActor で署名者と一致を検証済み。
	if act.Actor != "" && person.ID != act.Actor {
		return nil
	}
	if _, err := p.userRepo.FindByURI(person.ID); err != nil {
		// 未取得のリモートユーザーなら無視 (次回 follow/inbox などで取り込まれる)。
		// **障害は伝播させる** (#3116) — ack するとプロフィールの更新が消える。
		if repository.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("update person: lookup user: %w", err)
	}
	// upstream ApInboxService.update -> ApPersonService.updatePerson は actor を
	// 強制再取得して name だけでなく avatar/banner/bio/fields/isBot/isCat/
	// isLocked/movedTo/alsoKnownAs/emojis 等の全プロフィールを更新する (#1560)。
	// mk-go も ForceResolveActor (TTL バイパスで refreshActor を回す既存経路、
	// handleMove と同じ) を呼んで full refresh する。旧実装は name のみ反映して
	// いた。fetch 失敗時は refreshActor が silent に既存値を維持する。
	if _, err := p.resolver.ForceResolveActor(person.ID); err != nil {
		return err
	}
	return nil
}

// peekObjectType reads only the "type" field from a JSON object body. パース
// 不能・object でない・type フィールド無しの場合は空文字を返す。
func peekObjectType(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	return obj.Type
}

// objectAPID extracts the AP id of an activity object whether it is encoded as
// a bare string URI or as an object with an "id" field (upstream getApId 相当)。
func objectAPID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// readObjectString と同じ理由で正規化する (#2662)。
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return trimWHATWGURL(s)
	}
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return trimWHATWGURL(obj.ID)
	}
	return ""
}

// isBearcapURI reports whether the object id is a bearcaps (`bear:`) URI.
// upstream create()/announce() は bearcaps を未対応として skip する
// (ApInboxService.ts:401,291、#1560)。mk-go の resolver も bear: を扱えないため
// fetch で error にする前に cleanly skip する。
func isBearcapURI(raw json.RawMessage) bool {
	return strings.HasPrefix(objectAPID(raw), "bear:")
}

// handleReject undoes a Follow that was rejected by the followee.
//
// 想定: 自分 (ローカル follower) が remote followee に対して Follow を送ったが
// 拒否された場合に呼ばれる。inner.Follow.actor がローカル follower、object が
// remote followee。inner.id が自ホストの follow-relay URI で、かつ送信元
// actor がその relay 自身なら relay 関連として RelayStatusMarker.MarkRejected
// を呼ぶ。
func (p *Processor) handleReject(act genericActivity) error {
	var inner genericActivity
	// 型エラーを握らないと、直後の normalizeActor (#999) が救うはずの
	// `inner.actor` が embedded object のケースに**到達できない** (#2662)。
	if err := unmarshalIgnoringTypeErrors(act.Object, &inner); err != nil {
		return fmt.Errorf("invalid reject object: %w", err)
	}
	// inner.actor が embedded object のケースも救済する (#999 / upstream #17340)。
	inner.normalizeActor(act.Object)
	// chat room invitation の Reject (remote が我々の room 招待を辞退) は
	// pending invitation を削除する (#1203)。
	if strings.EqualFold(inner.Type, "invite") {
		return p.handleChatRoomReject(act, inner)
	}
	if !strings.EqualFold(inner.Type, "follow") {
		return ErrUnsupportedActivity
	}
	if relayID := matchFollowRelayID(inner.ID, p.localBaseURL); relayID != "" && p.relayMarker != nil {
		// Accept 側と同じ理由で送信元を検証する。こちらを通すと稼働中の
		// relay 配送を任意の actor が無言で止められる。
		if !p.relayStatusChangeAllowed(act.Actor, relayID, "Reject") {
			return nil
		}
		return p.relayMarker.MarkRejected(context.Background(), relayID)
	}
	followee, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	followerURI, err := readActorString(inner)
	if err != nil {
		return err
	}
	// ローカルユーザーは user.uri が NULL なので resolveTargetUser で ID
	// 解決する。これをやらないと FindByURI が fail して reject が silent drop
	// されてしまい、ローカル側の FollowRequest が消えずに永遠に pending の
	// ままになる。
	follower, err := p.resolveTargetUser(followerURI)
	if err != nil {
		// **Accept と同じ扱い** (#3115)。not-found は retry しても変わらないので
		// ack するが、**DB 障害を ack すると job が成功扱いになって retry されず**、
		// 直上のコメントが書いている「FollowRequest が永遠に pending」がそのまま
		// 起きる。Accept と Reject は同じ決定の裏表なので、片方だけ直さない。
		if repository.IsNotFound(err) {
			return nil
		}
		return err
	}
	// 既存のフォローがあれば解除する。pending な follow request も同様。
	// #2106 N11: upstream remoteReject は AP 配送を伴わない内部削除なので、federating な
	// Unfollow ではなく UnfollowSilent を使い、rejecter へ余計な Undo(Follow) を逆配送
	// しない (Following 行が残るエッジケースでの連合ノイズ / 相手の重複処理を防ぐ)。
	if err := p.followingService.UnfollowSilent(follower.ID, followee.ID); err != nil &&
		!errors.Is(err, corefollowing.ErrNotFollowing) {
		return err
	}
	if err := p.followingService.CancelRequest(follower.ID, followee.ID); err != nil &&
		!errors.Is(err, corefollowing.ErrRequestNotFound) {
		return err
	}
	return nil
}

// handleBlock processes an inbound Block activity. リモートユーザーがローカル
// ユーザーをブロックした場合にブロック関係を作成する。
func (p *Processor) handleBlock(act genericActivity) error {
	if p.blockingService == nil {
		return ErrUnsupportedActivity
	}
	blocker, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	blockeeURI, err := readObjectString(act.Object)
	if err != nil {
		return err
	}
	// ローカルユーザーは user.uri が NULL なので FindByURI では解決できない。
	// resolveTargetUser が localBaseURL prefix パターンから ID を抽出する。
	blockee, err := p.resolveTargetUser(blockeeURI)
	if err != nil {
		return errors.New("unknown blockee")
	}
	// ローカルユーザーのみ対象
	if blockee.Host != nil {
		return nil
	}
	if _, err := p.blockingService.Block(blocker.ID, blockee.ID); err != nil {
		if errors.Is(err, coreblocking.ErrAlreadyBlocking) {
			return nil
		}
		return err
	}
	return nil
}

// handleUndoBlock removes a blocking relationship created by a previous Block.
func (p *Processor) handleUndoBlock(act genericActivity, inner genericActivity) error {
	if p.blockingService == nil {
		return ErrUnsupportedActivity
	}
	blocker, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	blockeeURI, err := readObjectString(inner.Object)
	if err != nil {
		return err
	}
	// Block と同様、ローカルユーザー解決は resolveTargetUser で行う。
	blockee, err := p.resolveTargetUser(blockeeURI)
	if err != nil {
		return errors.New("unknown blockee")
	}
	// #2106 L30: upstream undoBlock は blockee が remote (host != null) なら skip する
	// (handleBlock の gate と対称)。
	if blockee.Host != nil {
		return nil
	}
	if err := p.blockingService.Unblock(blocker.ID, blockee.ID); err != nil {
		if errors.Is(err, coreblocking.ErrNotBlocking) {
			return nil
		}
		return err
	}
	return nil
}

// abuseReportCommentMaxRunes は `abuse_user_report.comment` の varchar(2048)
// (migration/000013_abuse_report.up.sql)。
const abuseReportCommentMaxRunes = 2048

// handleFlag processes an inbound Flag (abuse report) activity. リモートインスタンスからの
// 通報を保存する。
func (p *Processor) handleFlag(act genericActivity) error {
	if p.abuseReportRepo == nil {
		return ErrUnsupportedActivity
	}
	reporter, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	// object から URI 配列を取得する (ユーザーや Note の URI)。upstream は
	// `getApIds(activity.object)` なので、単一値・配列に加えて
	// `{"id": "..."}` の object 形式も受ける (ApInboxService.ts:560)。旧実装は
	// string と []string しか受けず、object 形式の Flag を落としていた。
	var uris activitypub.APIDList
	if err := json.Unmarshal(act.Object, &uris); err != nil {
		return errors.New("flag: cannot parse object")
	}
	if len(uris) == 0 {
		return errors.New("flag: empty object")
	}
	// upstream flag() は object URI を config.url + '/users/' prefix の LOCAL
	// user URI に絞ってから user id に map し users[0] を報告対象にする
	// (#1560、ApInboxService.ts:560-577)。mk-go も ExtractLocalUserID で
	// `{baseURL}/users/{id}` 形式のローカル URI だけを対象にする。旧実装は
	// 任意 host の user/note URI を受け、note 作者 (リモート可) まで fallback
	// して別 instance のユーザーを誤って報告し得た。
	var targetUserID string
	for _, uri := range uris {
		localID := p.resolver.ExtractLocalUserID(uri)
		if localID == "" {
			continue
		}
		if u, err := p.userRepo.FindByID(localID); err == nil && u != nil {
			targetUserID = u.ID
			break
		}
	}
	if targetUserID == "" {
		// ローカル user を 1 件も解決できない Flag は ack して drop する
		// (リモート対象や不正 URI。retry しても解決しないため error にしない)。
		slog.Info("federation: skipping Flag with no resolvable local target", "actor", act.Actor)
		return nil
	}
	// contentフィールドを取得。upstream は comment に flagged URI 一覧を
	// 付与する (`${content}\n${JSON.stringify(uris, null, 2)}`、#1560)。
	// **mk-go は pretty print しない** (upstream は 2 space indent)。通報の
	// 本文だけの差で、既存の通報との一貫性を優先して compact のままにする
	// (docs/divergence.md 3-3)。
	var content struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(act.raw, &content)
	urisJSON, _ := json.Marshal(uris)
	// `abuse_user_report.comment` は varchar(2048) (upstream の MiAbuseUserReport
	// も同じ)。**通報本文も URI 一覧も相手が自由に決められる**ので、溢れると
	// Create が 22001 で落ちて error を返し、**その inbox job が retry を
	// 使い切って dead になる = 通報が届かない**。通報本文は読むための文字列
	// なので切って NUL を落とす (#2726)。
	//
	// **URI 一覧を先に置かない。** 切るのは末尾なので、本文が長いと URI が
	// 落ちる。upstream の並び (content → uris) を保ったまま切ると、少なくとも
	// 通報の趣旨は残る。
	comment := remoteText(content.Content+"\n"+string(urisJSON), abuseReportCommentMaxRunes)
	report := &model.AbuseUserReport{
		ID:             p.abuseIDGen.Generate(nowFn()),
		TargetUserID:   targetUserID,
		ReporterID:     reporter.ID,
		Comment:        comment,
		TargetUserHost: nil,
		ReporterHost:   reporter.Host,
	}
	// ターゲットユーザーのホスト情報を取得
	if target, err := p.userRepo.FindByID(targetUserID); err == nil {
		report.TargetUserHost = target.Host
	}
	if err := p.abuseReportRepo.Create(report); err != nil {
		slog.Warn("failed to create abuse report from flag activity", "err", err)
		return err
	}
	p.notifyModeratorsOfRemoteAbuseReport(report)
	return nil
}

// handleMove processes an inbound Move activity. アカウント移行通知を受けて
// リモートactorのプロフィールを強制再取得する。TTLキャッシュをバイパスして
// movedTo/alsoKnownAsの最新値を反映する。
func (p *Processor) handleMove(act genericActivity) error {
	// upstream move() は activity.target を getApHrefNullable で読み、無い/不正なら
	// 'skip: invalid activity target' で早期 return する。target が無い Move で
	// ForceResolveActor (TTL-bypass の強制 refetch) を発火させない (#1948-21)。
	// getApHrefNullable は string か object.href を読む (id ではない) ので、同じ
	// 受理集合になるよう readApHref を使う。
	var t struct {
		Target json.RawMessage `json:"target"`
	}
	if len(act.raw) > 0 {
		_ = json.Unmarshal(act.raw, &t)
	}
	if readApHref(t.Target) == "" {
		return nil // skip: invalid activity target
	}
	if _, err := p.resolver.ForceResolveActor(act.Actor); err != nil {
		return err
	}
	return nil
}

// handleAdd processes an inbound Add activity. actorのfeaturedコレクションへの
// ノートピン留めを処理する。
func (p *Processor) handleAdd(act genericActivity) error {
	if p.pinningRepo == nil {
		return ErrUnsupportedActivity
	}
	actor, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	// targetがactorのfeaturedコレクションか確認
	var target struct {
		Target string `json:"target"`
	}
	_ = json.Unmarshal(act.raw, &target)
	if actor.Featured == nil || target.Target != *actor.Featured {
		return nil
	}
	noteURI, err := readObjectString(act.Object)
	if err != nil {
		return err
	}
	// ローカルノートならDBから検索、リモートならResolveNoteでフェッチ+取り込み
	note, err := p.noteRepo.FindByURI(noteURI)
	if err != nil {
		note, err = p.resolver.ResolveNote(noteURI)
		if err != nil {
			return err
		}
	}
	// **ピン留めできるのは自分の投稿だけ。** これを見ないと、署名付きの
	// リモート actor が**他人のノートを自分のプロフィールに並べられる**。
	// ローカルの非公開ノートも対象になりうる (`ResolveNote` はローカル URI を
	// 可視性を見ずに引く) ので、`users/show` の `pinnedNoteIds` 経由でその ID が
	// 未認証の相手に出る。
	//
	// **同じ規則が `featured.go` に既にある** (pull 側の featured 取り込み)。
	// こちらの push 側 (`Add`) だけが取り残されていた。upstream も
	// `NotePiningService.addPinned` が `findOneBy({id, userId: user.id})` で
	// 弾いている。
	if note.UserID != actor.ID {
		return nil
	}
	// **件数にも上限を置く。** `Add` は署名さえ通れば何度でも送れるので、
	// 上限が無いと `user_note_pining` を無制限に増やせる。しかも
	// `internal/api/users/handler.go` の設計メモ (#1489) は `pinnedNoteIds` を
	// 絞らない理由として「pinning は per-user 上限が厳しい (= 数件) ので
	// mass enumeration リスクは低い」と書いており、**その前提がここで壊れる**。
	//
	// 値は pull 側の `featuredPinLimit` と同じ。片方だけ緩いと、同じ
	// リモート actor が経路によって違う上限を受けることになる。
	//
	// **上限に当たったら古い方を外して入れ替える。捨てない。** upstream は
	// `Remove(A)` と `Add(B)` を**別々の deliver job** で配る
	// (`NotePiningService.deliverPinnedChange`)。こちらの inbox は並列に
	// 処理するので `Add(B)` が先に着くことがあり、そこで捨てると
	// (`return nil` は inbox job を成功扱いにするので再試行されない) B は
	// **次の actor 更新まで現れない** (既定 TTL 24 時間)。
	//
	// **再試行にはしない。** upstream は throw して job を再送させるが、
	// mk-go でそれをやると、上限を超える `Add` を投げ続けるだけで job が
	// 8 倍に膨らむ — この上限が防いでいる負荷を別の形で呼び込む。入れ替えなら
	// 行数は上限に収まったまま、順序の入れ替わりも拾える。
	//
	// **数えられなかったら errで返す。** ここで握り潰すと上限が効かないまま
	// 進むし、`nil` で捨てると inbox job が「処理済み」として再試行しない。
	// error なら再試行されるので、DB 障害が上限の無効化にも取りこぼしにも
	// 化けない (#2792 と同じ判断)。
	// **既にピン留め済みなら何もしない。** 再送や順序の入れ替わりで同じ
	// `Add` が二度届く。上限判定より前に置く — ここを通すと「既にあるものを
	// 入れるために別のピンを外す」ことになる。
	switch existing, ferr := p.pinningRepo.FindByPair(actor.ID, note.ID); {
	case ferr == nil && existing != nil:
		return nil
	case ferr != nil && !repository.IsNotFound(ferr):
		// **障害を「まだ無い」に倒さない** (#3116)。倒すと上限判定の側へ進み、
		// 「既にあるものを入れるために別のピンを外す」ことになる。
		return fmt.Errorf("pin dedup: %w", ferr)
	}
	count, cerr := p.pinningRepo.CountByUser(actor.ID)
	if cerr != nil {
		return fmt.Errorf("count pinned notes: %w", cerr)
	}
	if count >= featuredPinLimit {
		rows, lerr := p.pinningRepo.ListByUser(actor.ID)
		if lerr != nil {
			return fmt.Errorf("list pinned notes: %w", lerr)
		}
		// `ListByUser` は id DESC なので末尾が最も古い。新しい方を
		// `featuredPinLimit - 1` 件残して、あふれる分を外す。
		for i := featuredPinLimit - 1; i < len(rows); i++ {
			if derr := p.pinningRepo.Delete(rows[i]); derr != nil {
				return fmt.Errorf("evict pinned note: %w", derr)
			}
		}
	}
	now := nowFn()
	pin := &model.UserNotePining{
		ID:     p.pinningIDGen.Generate(now),
		UserID: actor.ID,
		NoteID: note.ID,
	}
	if err := p.pinningRepo.Create(pin); err != nil {
		// 既にピン留め済み（重複キー）の場合のみ無視
		if existing, _ := p.pinningRepo.FindByPair(actor.ID, note.ID); existing != nil {
			return nil
		}
		return err
	}
	return nil
}

// handleRemove processes an inbound Remove activity. actorのfeaturedコレクションから
// ノートのピン留めを解除する。
func (p *Processor) handleRemove(act genericActivity) error {
	var target struct {
		Target string `json:"target"`
	}
	_ = json.Unmarshal(act.raw, &target)
	// chat room target の Remove は group chat の leave (#1364)。featured pin
	// より先に判定する (pinningRepo 未配線でも chat leave は処理する)。actor の
	// membership を削除する (cherrypick ApInboxService.remove と同じく actor 基準)。
	if roomURI := chatRoomURI(target.Target); roomURI != "" && p.chatRoomReceiver != nil {
		actor, err := p.resolver.ResolveActor(act.Actor)
		if err != nil {
			return err
		}
		if err := p.chatRoomReceiver.RemoveMemberViaAP(roomURI, actor.ID); err != nil {
			if errors.Is(err, corechat.ErrNotFound) || errors.Is(err, corechat.ErrInvalidTarget) {
				return ErrUnsupportedActivity
			}
			return err
		}
		return nil
	}
	if p.pinningRepo == nil {
		return ErrUnsupportedActivity
	}
	actor, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	if actor.Featured == nil || target.Target != *actor.Featured {
		return nil
	}
	noteURI, err := readObjectString(act.Object)
	if err != nil {
		return err
	}
	note, err := p.noteRepo.FindByURI(noteURI)
	if err != nil {
		// 理由は handleAccept と同じ (#3115)。ここで DB 障害を ack すると
		// job が成功扱いになって retry されず、pin が外れないまま残る。
		if repository.IsNotFound(err) {
			return nil
		}
		return err
	}
	pin, err := p.pinningRepo.FindByPair(actor.ID, note.ID)
	if err != nil {
		if repository.IsNotFound(err) {
			return nil
		}
		return err
	}
	// **書き込みの失敗も伝播させる** (#3115)。lookup だけ retry に倒して
	// ここを捨てると、「pin が外れないまま残る」という同じ結末が DELETE 側で
	// 残る。対になる `handleAdd` は Create / eviction の Delete を両方伝播する。
	if err := p.pinningRepo.Delete(pin); err != nil {
		return fmt.Errorf("unpin note: %w", err)
	}
	return nil
}

// normalizeActor populates act.Actor when JSON tag-driven Unmarshal could not
// pull a plain string (= upstream Misskey #17340 fix; activity.actor may be
// either a string IRI or an embedded {"id": "..."} object). Pass the raw bytes
// that were Unmarshaled into act. No-op when Actor is already populated.
//
// 同 normalization は inner activity (Undo / Accept / Reject の object) にも
// 適用する必要があるため、handler 側でも inner.normalizeActor(act.Object) を
// 呼ぶ運用にする。
func (act *genericActivity) normalizeActor(raw json.RawMessage) {
	if act.Actor != "" {
		return
	}
	var probe struct {
		Actor json.RawMessage `json:"actor"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || len(probe.Actor) == 0 {
		return
	}
	if id, err := readObjectString(probe.Actor); err == nil {
		act.Actor = id
	}
}

// readObjectString reads an activity Object field that is either a plain
// string IRI or a nested object with an "id" field.
//
// **戻り値は `trimWHATWGURL` を通す。** 書き込み側 (`ingestNoteWithCreated` /
// `fetchActor`) が正規化した値を `note.uri` / `user.uri` に入れているので、
// 読み出し側が生値のままだと `FindByURI` が miss する。id に改行を付ける実装の
// ノートが「取り込めるが Delete / Update / ピン留めが一切届かない」状態になる
// (#2662)。upstream は書き込みも読み出しも生値なので対称。
func readObjectString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("missing object")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		if s = trimWHATWGURL(s); s != "" {
			return s, nil
		}
		return "", errors.New("object missing id")
	}
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", err
	}
	obj.ID = trimWHATWGURL(obj.ID)
	if obj.ID == "" {
		return "", errors.New("object missing id")
	}
	return obj.ID, nil
}

// readApHref mirrors upstream getApHrefNullable: it returns the value when it is
// a bare URI string, or its `href` property when it is an AS Link/object, and ""
// otherwise (including {id}-only objects, which upstream also treats as absent).
// Used for fields like Move.target that upstream reads via getApHrefNullable
// rather than getApId (#1948-21).
func readApHref(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj struct {
		Href string `json:"href"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		return obj.Href
	}
	return ""
}

// readActorString reads an activity's actor field, supporting both string and
// object forms (mirrors readObjectString but on the actor field of an inner
// activity such as the Follow inside a Reject).
func readActorString(act genericActivity) (string, error) {
	if act.Actor != "" {
		return act.Actor, nil
	}
	return "", errors.New("inner activity missing actor")
}

// handleChatCreate processes a Note flagged with `_misskey_talk: true`
// inside a Create activity (CherryPick / レガシー Misskey の 1-on-1 chat
// federation, #692)。
//
// `to` は CherryPick の renderChatMessage が string[] で出すが、互換性のため
// 単一文字列 / 配列の両方を受け付ける。配列の場合は先頭エントリを recipient
// として扱う。**1-on-1 経路なのでそれで正しい。**
//
// group chat は別プロトコルで、note の `@context` に room URI が入る形で届く。
// Create の入口 (`chatRoomURIFromContext`) が先に振り分け、
// `handleChatRoomMessageCreate` → `CreateRoomMessageViaAP` が room のメンバー
// 全員に配る。ここに複数 recipient が来ることはない。
func (p *Processor) handleChatCreate(sender *model.User, noteURI, content, mfmSource string, toRaw json.RawMessage) error {
	if p.chatService == nil {
		return ErrUnsupportedActivity
	}
	if noteURI == "" {
		return fmt.Errorf("chat create: missing note id")
	}
	if sender.IsLocal() {
		return fmt.Errorf("chat create: sender %s is local (loopback?)", sender.ID)
	}
	to, err := readRecipientURI(toRaw)
	if err != nil {
		return fmt.Errorf("chat create: %w", err)
	}
	recipient, err := p.userRepo.FindByURI(to)
	if err != nil {
		if localID := p.resolver.ExtractLocalUserID(to); localID != "" {
			recipient, err = p.userRepo.FindByID(localID)
		}
		if err != nil {
			return fmt.Errorf("chat create: resolve recipient: %w", err)
		}
	}
	if !recipient.IsLocal() {
		return fmt.Errorf("chat create: recipient %s is not local", to)
	}
	_, err = p.chatService.CreateMessageViaAP(context.Background(), noteURI, sender, recipient.ID, content, mfmSource)
	// 列に収まらない uri は retry しても解決しないので drop する (#2726)。
	if errors.Is(err, corechat.ErrInvalidTarget) {
		slog.Warn("chat create: message cannot be stored", "actor", sender.ID)
		return ErrUnsupportedActivity
	}
	return err
}

// readRecipientURI extracts the first recipient URI from a `to` field that
// may be either a JSON string or a JSON array of strings. Empty / missing
// values produce an error so the caller can distinguish "malformed" from
// "valid but addressed to public".
func readRecipientURI(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("missing 'to' field")
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "" {
			return "", errors.New("'to' is empty")
		}
		return single, nil
	}
	var multi []string
	if err := json.Unmarshal(raw, &multi); err == nil {
		for _, s := range multi {
			if s != "" {
				return s, nil
			}
		}
		return "", errors.New("'to' has no usable recipient")
	}
	return "", errors.New("'to' is neither string nor array")
}

// handleChatMessage processes an inbound Misskey:ChatMessage activity
// (CherryPick v12 federation). The activity itself IS the message object
// (not wrapped in Create), so act.ID is the message URI.
func (p *Processor) handleChatMessage(act genericActivity) error {
	if p.chatService == nil {
		return ErrUnsupportedActivity
	}
	// attributedTo と actor の一致を検証
	var raw struct {
		AttributedTo string `json:"attributedTo"`
		To           string `json:"to"`
		Content      string `json:"content"`
		// Create 経由の probe と同じ 3 段で MFM の原文も拾う。
		Source *struct {
			Content   string `json:"content"`
			MediaType string `json:"mediaType"`
		} `json:"source"`
		MisskeyContent string `json:"_misskey_content"`
	}
	if err := json.Unmarshal(act.raw, &raw); err != nil {
		return fmt.Errorf("chat message: unmarshal: %w", err)
	}
	if raw.AttributedTo == "" || raw.AttributedTo != act.Actor {
		return fmt.Errorf("chat message: attributedTo (%s) != actor (%s)", raw.AttributedTo, act.Actor)
	}
	if raw.To == "" {
		return fmt.Errorf("chat message: missing 'to' field")
	}
	// actor (送信者) を解決
	sender, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return fmt.Errorf("chat message: resolve sender: %w", err)
	}
	// ローカルユーザーはDB上URI==nilのためFindByURIでは解決できない。
	// handleAcceptと同じパターンでExtractLocalUserID→FindByIDにフォールバック。
	recipient, err := p.userRepo.FindByURI(raw.To)
	if err != nil {
		if localID := p.resolver.ExtractLocalUserID(raw.To); localID != "" {
			recipient, err = p.userRepo.FindByID(localID)
		}
		if err != nil {
			return fmt.Errorf("chat message: resolve recipient: %w", err)
		}
	}
	if !recipient.IsLocal() {
		return fmt.Errorf("chat message: recipient %s is not local", raw.To)
	}
	// **id を配送元ホストに縛る。** `Create` + `_misskey_talk` 側と同じ理由で、
	// ここも `act.ID` をそのままメッセージの uri として保存する
	// (`CreateMessageViaAP` の呼び出しはこの 2 箇所だけ)。片方だけ塞いでも、
	// この兄弟の活動型で同じ squat が通る。
	if !sameDeliveryHost(act.Actor, act.ID) {
		slog.Warn("federation: chat message id host does not match delivering actor",
			"actor", act.Actor, "id", act.ID)
		return ErrUnsupportedActivity
	}
	mfmSource := ""
	if raw.Source != nil && raw.Source.MediaType == "text/x.misskeymarkdown" {
		mfmSource = raw.Source.Content
	}
	if mfmSource == "" {
		mfmSource = raw.MisskeyContent
	}
	_, err = p.chatService.CreateMessageViaAP(context.Background(), act.ID, sender, recipient.ID, raw.Content, mfmSource)
	// handleChatCreate と同じ理由で non-retry に落とす (#2726)。
	if errors.Is(err, corechat.ErrInvalidTarget) {
		slog.Warn("chat message: message cannot be stored", "actor", act.Actor)
		return ErrUnsupportedActivity
	}
	return err
}
