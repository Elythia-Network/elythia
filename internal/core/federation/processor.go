package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/bgwork"
	coreblocking "github.com/elythia-network/elythia/internal/core/blocking"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	corereversi "github.com/elythia-network/elythia/internal/core/reversi"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/repository"
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
	// MarkAccepted / MarkRejected only change a relay that is still
	// "requesting"; answers for an already settled relay are ignored.
	MarkAccepted(ctx context.Context, id string) error
	MarkRejected(ctx context.Context, id string) error
	// FindByID returns the relay row identified by id, or an error when no
	// such row exists.
	//
	// status を書き換える前に「送信元 actor がその relay 自身か」を確かめる
	// ために要る。行が持っている identity は inbox URI だけなので、actor の
	// inbox / sharedInbox との完全一致 (relayActorInboxMatches) で突き合わせる。
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
	// background は受信後のベストエフォートフックを起動する箱 (#3661)。
	// 世代の停止で待つ。nil なら素の goroutine で起動する (テスト)。**本番で nil だと、
	// 世代を止めても受信後の hook の goroutine を待たず、閉じた DB へ書きに行く。**
	// 構築子の必須の引数 (#3833。以前は setter と起動時の検査)。
	background *bgwork.Group
	// rules は連合のルール (#3090)。nil なら評価しない。
	rules            RuleEvaluator
	resolver         *Resolver
	followingService *corefollowing.Service
	reactionService  *corereaction.Service
	noteDeleteSvc    *corenote.DeleteService
	userRepo         repository.UserRepository
	noteRepo         repository.NoteRepository

	// accountCreatedAt はフォローしてきたリモートの人のアカウントの作成日時を、
	// 分からなければ Misskey 系の相手の `/api/users/show` から埋める (#3465)。
	// 未配線なら埋めない (列は NULL のまま)。
	accountCreatedAt AccountCreatedAtFillerFunc

	// Block/Flag/Move/Add/Remove federation hooks.
	// 構築子 (blockingService) と SetAbuseReportRepo 等で注入。nilの場合は対応activityが
	// ErrUnsupportedActivityを返す。
	//
	// blockingService が nil だと、inbound の Block / Undo(Block) が
	// `ErrUnsupportedActivity` になるだけでなく、**reversi の Invite からブロック判定が
	// 丸ごと飛ぶ** (`reversi_inbox.go` の `p.blockingService != nil` guard)。ブロック済みの
	// リモート利用者が対象の `reversi:<userID>` ストリームへ `invited` を push できる。
	// 構築子の必須の引数 (#3833。以前は setter と起動時の検査)。
	blockingService *coreblocking.Service
	// quoteRequests は FEP-044f の QuoteRequest に答える (#3234)。未配線なら
	// ErrUnsupportedActivity。
	quoteRequests *QuoteRequestHandler
	// quoteAnswers は、こちらが送った QuoteRequest への Accept / Reject を受ける
	// (#3234 段階 3)。未配線なら従来どおり (Follow 以外の Accept は無視)。
	quoteAnswers    QuoteAnswerHandler
	abuseReportRepo repository.AbuseReportRepository
	abuseIDGen      id.Generator
	// abuseCreated はリモートからの通報 (AP Flag) をモデレーターに知らせる
	// (通知欄 / admin stream / abuseReport system webhook)。ローカルの
	// report-abuse と同じもの (core/abuse.CreatedNotifier) を呼ぶ (#3256)。
	// 以前は通知欄だけをここで出しており、admin stream と system webhook が
	// この経路だけ抜けていた。nil なら通知しない。
	abuseCreated AbuseReportCreatedNotifier
	pinningRepo  repository.UserNotePiningRepository
	pinningIDGen id.Generator

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

	// followerKeys は、instance actor に見せなかった Announce の対象を取り直す
	// ときに、actor をフォローしているローカルの利用者の鍵を選ぶ (#3916)。
	// nil なら取り直さない。
	followerKeys FollowerFetchKeys

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

	// Note chart hook for inbound Announce (#1156). inbound Create を含め resolver
	// が作る note は Resolver.fireNoteCreated が数える (#3330)。ローカル作成は
	// note_create_service.go から chartHook.OnNoteCreated を発火させているが、
	// handleAnnounce は renote 行を resolver を通さず直接作るため、本フィールド
	// 経由で同じ chart hook を発火させないと
	// PerUserNotesChart 等の inc 列が +1 されない (= プロフィールの
	// アクティビティタブの heatmap がリモートユーザーだけ空になる)。
	// noteChartHook は idempotent ではないので、dedup hit (= 既存ノートを返した
	// ケース) では発火させない。resolver の ChartHook (= OnRemoteUserCreated) とは責務が異なるので
	// 別 interface として分離する。
	noteChartHook NoteChartHook

	// localBaseURL はローカルユーザーのcanonical URI prefix
	// (例: "https://go.k7a.org") を保持する。inboxで受信したactivityの
	// object が "{localBaseURL}/users/{id}" 形式のとき、ローカルユーザーを
	// ID で lookup するために使用する (ローカルユーザーの user.uri は NULL
	// なので FindByURI では解決できない)。
	//
	// **空文字だと静かに壊れる。** relay の Accept / Reject は activity id が
	// 自ホストの URI であることを要求する (fail-closed) ので、ここが空だと
	// relay の状態遷移が全て落ちる。ローカル URI の判定も全部外れる。構築子の
	// 必須の引数 (#3833。以前は setter と起動時の検査)。
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

// SetQuoteRequestHandler wires FEP-044f QuoteRequest handling (#3234).
func (p *Processor) SetQuoteRequestHandler(h *QuoteRequestHandler) {
	p.quoteRequests = h
}

// QuoteAnswerHandler receives the answers to QuoteRequests sent by local notes.
type QuoteAnswerHandler interface {
	HandleAnswer(actorURI, requestURI string, accepted bool, result string) error
	HandleRevocation(actorURI, approvalURI string) (bool, error)
}

// SetQuoteAnswerHandler wires the handler of Accept / Reject for our
// QuoteRequests (FEP-044f、#3234)。
func (p *Processor) SetQuoteAnswerHandler(h QuoteAnswerHandler) {
	p.quoteAnswers = h
}

// handleQuoteRequest answers a FEP-044f QuoteRequest for a local note (#3234).
func (p *Processor) handleQuoteRequest(act genericActivity) error {
	if p.quoteRequests == nil {
		return ErrUnsupportedActivity
	}
	// activity の id は actor と同じホストのものだけ受ける (Mastodon と同じ)。
	// 違うと、Accept の object に他人の activity の id を返すことになる。
	if act.ID == "" || !sameHost(act.ID, act.Actor) {
		return nil
	}
	quoter, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		if isPermanentSkipError(err) {
			return nil
		}
		return err
	}
	return p.quoteRequests.Handle(quoter, act.raw)
}

// resolveTargetUser looks up a user referenced by URI in an inbound activity.
// ローカルユーザの user.uri は DB 上 NULL なので FindByURI では解決できない。
// 対策として localBaseURL 配下の URI ("{baseURL}/users/{id}") を検出し、
// ID パートを抜き出して FindByID で lookup する。リモートユーザーは従来通り
// FindByURI で解決する。
//
// 使うのは本家に対応の無い reversi の招待と chat room の招待の宛先だけ。
// Follow / Block とその Undo は本家が getUserFromApId で引くので
// userFromAPID を使う (#3330)。
func (p *Processor) resolveTargetUser(uri string, sourceHost ...string) (*model.User, error) {
	var user *model.User
	var err error
	if p.localBaseURL != "" {
		prefix := p.localBaseURL + "/users/"
		if id, ok := strings.CutPrefix(uri, prefix); ok {
			// id の中に "/" が残る (例: "/outbox") ケースがあるので切り落とす。
			if slash := strings.IndexByte(id, '/'); slash >= 0 {
				id = id[:slash]
			}
			if id != "" {
				user, err = p.userRepo.FindByID(id)
				goto checked
			}
		}
	}
	user, err = p.userRepo.FindByURI(uri)
checked:
	if err != nil {
		return nil, err
	}
	host := ""
	if len(sourceHost) > 0 {
		host = sourceHost[0]
	}
	if err := p.resolver.RequireInboundLocalUser(user, host); err != nil {
		return nil, err
	}
	return user, nil
}

// userFromAPID looks up the user an AP id names the way upstream
// ApDbResolverService.getUserFromApId does: a URI on this instance's host
// names the user whose ID is the segment after `/users/` (parseLocalURI, so
// `?` and `#` are not part of it), any other URI names the user with that
// uri. A deleted user is not found. It returns a repository not-found error
// when there is no such user.
//
// 本家は local / remote のどちらも `isDeleted: false` で引く。以前の Accept /
// Undo(Accept) / Reject は削除済みの利用者も引き、削除済みの利用者を follower
// としてフォローの承認・解除を進めていた (#3330)。
// ローカルの host で `/users/` 以外を指すものは、本家と同じく誰にも当てない
// (本家は `parsed.type !== 'users'` で null)。
func (p *Processor) userFromAPID(uri string, sourceHost ...string) (*model.User, error) {
	var (
		u   *model.User
		err error
	)
	if id, local := p.resolver.LocalUserIDFromURI(uri); local {
		if id == "" {
			return nil, repository.ErrNotFound
		}
		u, err = p.userRepo.FindByID(id)
	} else {
		u, err = p.userRepo.FindByURI(uri)
	}
	if err != nil {
		return nil, err
	}
	if u == nil || u.IsDeleted {
		return nil, repository.ErrNotFound
	}
	host := ""
	if len(sourceHost) > 0 {
		host = sourceHost[0]
	}
	if err := p.resolver.RequireInboundLocalUser(u, host); err != nil {
		return nil, err
	}
	return u, nil
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
	// OnNoteDeleted removes the renote notification a withdrawn boost left
	// (#3201)。Undo(Announce) は DeleteService を通らないので、ここから呼ぶ。
	OnNoteDeleted(note *model.Note)
}

// NoteChartHook is invoked after a freshly persisted note (the resolver's new
// notes and the processor's Announce renote rows) so that PerUserNotesChart 等の counters が +1 される (#1156)。ローカル作成では
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
//
// localBaseURL は inbound activity の object がローカルユーザーを指すかの判定に
// 使う、自インスタンスの base URL。background は受信後のベストエフォートフックを
// 起動する箱 (#3661)。localBaseURL / blockingService / background はテストでは
// 空文字か nil を渡せる (それぞれのフィールドの説明のとおりに劣化する)。
func NewProcessor(
	resolver *Resolver,
	followingService *corefollowing.Service,
	reactionService *corereaction.Service,
	noteDeleteSvc *corenote.DeleteService,
	userRepo repository.UserRepository,
	noteRepo repository.NoteRepository,
	localBaseURL string,
	blockingService *coreblocking.Service,
	background *bgwork.Group,
) *Processor {
	return &Processor{
		resolver:         resolver,
		followingService: followingService,
		reactionService:  reactionService,
		noteDeleteSvc:    noteDeleteSvc,
		userRepo:         userRepo,
		noteRepo:         noteRepo,
		localBaseURL:     localBaseURL,
		blockingService:  blockingService,
		background:       background,
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
//
// signer は呼び出し側がこの job の中で DB から引いた行 (inbox では
// Resolver.ResolveActor の戻り値) でなければならない。signer.URI が
// activity.actor と一致するとき、凍結・削除の判定はこの行で行い、DB を
// 引き直さない (#3610)。
func (p *Processor) ProcessWithSigner(body []byte, signer *model.User) error {
	return p.process(body, signer)
}

// ProcessNormalized is ProcessWithSigner for a body the caller has already
// passed through NormalizeActivity (#3610).
//
// inbox の gate は actor / id を読むために同じ前処理 (配列の剥がし + Normalize +
// decode) を済ませているので、その結果をそのまま使い、もう一度 Normalize しない。
// NormalizedActivity は NormalizeActivity でしか中身を作れないので、正規化して
// いない本文がこの入口を素通りすることは無い。
func (p *Processor) ProcessNormalized(n NormalizedActivity, signer *model.User) error {
	return p.processNormalized(n, signer)
}

// Process handles an inbound activity without signer information.
func (p *Processor) Process(body []byte) error {
	return p.process(body, nil)
}

func (p *Processor) process(body []byte, signer *model.User) error {
	return p.processNormalized(NormalizeActivity(body), signer)
}

// NormalizedActivity is an inbound activity after the singleton-array unwrap,
// activitypub.Normalize and decode that Process applies, done once (#3610).
//
// The zero value is an activity without an actor, which Process drops.
// Fields are unexported so that only NormalizeActivity can produce one: a
// caller cannot hand ProcessNormalized a body that skipped normalization.
type NormalizedActivity struct {
	// source は NormalizeActivity に渡された本文 (正規化の前の生のバイト)。
	// 署名の検証や LD-Signature の検査に使うのはこちらで、act.raw ではない。
	source []byte
	act    genericActivity
	err    error
	// unwrappedFrom / unwrappedSize は 1 要素の配列を剥がしたときの前後の長さ
	// (剥がしていなければ 0)。ログにだけ使う。
	unwrappedFrom, unwrappedSize int
}

// NormalizeActivity runs the unwrap+Normalize+decode pass once.
//
// Normalize の失敗は err に残し、ProcessNormalized が Process と同じ扱い
// (衝突キーは ErrUnsupportedActivity、それ以外は error) をする。
func NormalizeActivity(body []byte) NormalizedActivity {
	n := NormalizedActivity{source: body}
	// Foundkey 系 fork instance は valid な AS Activity を 1 要素 JSON
	// array で wrap して送信してくるケースがある (#1185)。AS 仕様上
	// inbox direct POST は object 前提なので、array が来た時は剥がして
	// 中身を処理する。2+ 要素は AS spec 外として no-op で素通し。
	//
	// Normalize の前で剥がす: Normalize は内部で json.Unmarshal → 再
	// encode するが top-level の array → object 変換はしないので、
	// object 化はここで済ませる必要がある。
	if unwrapped, ok := tryUnwrapSingletonArray(body); ok {
		n.unwrappedFrom, n.unwrappedSize = len(body), len(unwrapped)
		body = unwrapped
	}
	normalized, err := activitypub.Normalize(body)
	if err != nil {
		n.err = err
		return n
	}
	// Normalize は内部で json.Marshal して再エンコードするため、ここでの
	// Unmarshal は構文エラーで失敗しない。型エラー (actor が object など) は
	// 握り、読めたフィールドを使う。gate (Fields) と本処理は同じ値を見る。
	_ = json.Unmarshal(normalized, &n.act)
	n.act.raw = normalized
	// upstream Misskey #17340: activity.actor が string IRI でなく embedded
	// {"id": "..."} object のケースを救済する (triage #999)。
	n.act.normalizeActor(normalized)
	return n
}

// Source returns the body NormalizeActivity was given (before normalization).
func (n NormalizedActivity) Source() []byte { return n.source }

// Fields returns actor / id / type as Process will see them. All are "" when
// the body could not be normalized.
func (n NormalizedActivity) Fields() ActivityFields {
	return ActivityFields{Actor: n.act.Actor, ID: n.act.ID, Type: n.act.Type}
}

func (p *Processor) processNormalized(n NormalizedActivity, signer *model.User) error {
	if n.unwrappedFrom > 0 {
		// 観測性のため発火を記録 (log message は中立的に表現、Foundkey 由来は
		// helper / 本コメント側の doc に残す)。
		slog.Info("federation: unwrapping singleton JSON array activity",
			"originalSize", n.unwrappedFrom, "unwrappedSize", n.unwrappedSize)
	}
	if err := n.err; err != nil {
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
	act := n.act
	if act.Actor == "" {
		// 送り直しても actor は現れない。error で返すと inbox の job が再試行を
		// 使い切ってから failed に積まれる (既定 8 回) ので、受け取って捨てる。
		slog.Info("federation: dropping activity without an actor id")
		return nil
	}

	// upstream performOneActivity は actor.isSuspended なら dispatch 前に
	// 早期 return する (ApInboxService.ts:144、#1560)。mk-go は既知の
	// suspended remote actor を DB lookup で判定して drop する (新規 actor は
	// まだ DB に無く suspended にもなり得ないので素通し)。fetch はせず DB read
	// のみなので追加コストは小さい。
	//
	// 削除済み (isDeleted) の actor も同じく捨てる。本家は署名の持ち主を引く
	// `getAuthUserFromKeyId` / `getAuthUserFromApId` が削除済みなら null を返し、
	// InboxProcessorService が `skip: failed to resolve user` で捨てる。ここに
	// 届く act.Actor は、HTTP 署名の持ち主か、転送なら LD-Signature で認証された
	// actor なので、どちらの経路もこの判定を通る。
	if p.userRepo != nil {
		var actor *model.User
		var err error
		if signer != nil && signer.URI != nil && *signer.URI == act.Actor {
			// **署名者がそのまま actor なら、引き直さない (#3610)。** signer は
			// 同じ job の中で署名の検証のために DB から引いた行で、同じ URI を
			// 引き直しても数ミリ秒前と同じ行が返るだけ。凍結を古い値で判定する
			// 窓は、引き直しても残る「判定から処理までの間」と変わらない。
			// 転送 (署名者 != actor) と署名の無い経路は今どおり DB を引く。
			actor = signer
		} else {
			actor, err = p.userRepo.FindByURI(act.Actor)
		}
		switch {
		case err == nil && actor != nil && actor.IsSuspended:
			slog.Info("federation: dropping activity from suspended actor", "actor", act.Actor, "type", act.Type)
			return nil
		case err == nil && actor != nil && actor.IsDeleted:
			slog.Info("federation: dropping activity from deleted actor", "actor", act.Actor, "type", act.Type)
			return nil
		case err != nil && !repository.IsNotFound(err):
			// **判定できないまま素通しにしない** (#3116)。以前は `err == nil` を
			// 条件にしていたので、**DB 障害のあいだ凍結済み actor の activity が
			// そのまま処理されていた** (moderation の fail-open)。not-found は
			// 「まだ取り込んでいない actor」= 凍結されているはずがないので素通しが
			// 正しいが、障害は retry させる。
			return fmt.Errorf("actor moderation check: %w", err)
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
	// 連合のルール (#3090) で拒否するものは、ここで ack して捨てる (禁止語と
	// 同じく retry させない)。
	if p.activityRejectedByRules(act, signer) {
		return nil
	}
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
	case "quoterequest", "https://w3id.org/fep/044f#quoterequest":
		return p.handleQuoteRequest(act)
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
	return NormalizeActivity(body).Fields()
}

// SetAbuseReportRepo wires the abuse report repository for Flag activities.
func (p *Processor) SetAbuseReportRepo(repo repository.AbuseReportRepository, idGen id.Generator) {
	p.abuseReportRepo = repo
	p.abuseIDGen = idGen
}

// AbuseReportCreatedNotifier tells moderators about a newly created report.
// 実装は core/abuse.CreatedNotifier で、ローカルの report-abuse も同じものを
// 呼ぶ (#3256)。
type AbuseReportCreatedNotifier interface {
	NotifyCreated(ctx context.Context, report *model.AbuseUserReport, reporter, target *model.User)
}

// SetAbuseReportCreatedNotifier wires the notifications for reports that
// arrive over ActivityPub. nil なら通知しない。
func (p *Processor) SetAbuseReportCreatedNotifier(n AbuseReportCreatedNotifier) {
	p.abuseCreated = n
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

// AccountCreatedAtFillerFunc fills a remote user's unknown account creation
// time. (*AccountCreatedAtFiller).Fill satisfies it.
type AccountCreatedAtFillerFunc func(ctx context.Context, u *model.User)

// SetAccountCreatedAtFiller wires the best-effort lookup of a remote
// follower's account creation time on inbound Follow (#3465).
func (p *Processor) SetAccountCreatedAtFiller(fn AccountCreatedAtFillerFunc) {
	p.accountCreatedAt = fn
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

// SetNoteChartHook wires a NoteChartHook so that the renote rows an inbound
// Announce creates update PerUserNotesChart 等のリモートユーザー向け chart 集計
// (#1156)。Notes the resolver creates (inbound Create included) are counted
// through Resolver.SetNoteChartHook instead (#3330); wire both.Without it, リモートユーザーのプロフィールの「アクティビティ」
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
// host は hostFromURI で punycode + 小文字に正規化してから比較するため、
// `https://Relay.Example/actor` と `https://relay.example/inbox` は一致する。
// port は host の一部として扱う (別 authority を同一視しない安全側)。
//
// これは前段の安価な絞り込みで、本判定は relayActorInboxMatches (本家と同じ
// inbox / sharedInbox の完全一致)。host だけだと、relay と同じ host に別 actor
// を立てられる相手 (マルチテナントな relay サーバー) が status を動かせる。
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

// relayActorInboxMatches reports whether the actor's inbox or sharedInbox is
// exactly the inbox registered for the relay.
//
// 本家 2026.10.0 RelayService.updateRequestingRelayStatus の
// `actor.inbox === relay.inbox || actor.sharedInbox === relay.inbox` と同じ
// 完全一致にする。actor の inbox / sharedInbox は取り込み時に actor の host へ
// 縛られている (resolver の validateActor 相当) ので、他 host の actor が relay
// の inbox を名乗ることはできない。末尾スラッシュなどの表記揺れで一致しない
// relay は本家でも requesting のまま止まるので、互換の範囲では緩めない。
func relayActorInboxMatches(actor *model.User, rel *model.Relay) bool {
	if actor == nil || rel == nil || rel.Inbox == "" {
		return false
	}
	if actor.Inbox != nil && *actor.Inbox == rel.Inbox {
		return true
	}
	return actor.SharedInbox != nil && *actor.SharedInbox == rel.Inbox
}

// relayStatusChangeAllowed reports whether the actor that signed an inbound
// Accept / Reject may flip the status of the relay row referenced by relayID.
//
// kind はログ用の activity 種別 ("Accept" / "Reject")。検証に落ちたケースは
// 呼び出し側で状態を変えずに drop する (ack して retry しない)。actor を引けない
// ときだけは error を返し、呼び出し側はそれを返して再試行させる。
func (p *Processor) relayStatusChangeAllowed(actorURI, relayID, kind string) (bool, error) {
	rel, err := p.relayMarker.FindByID(context.Background(), relayID)
	if err != nil || rel == nil {
		slog.Info("federation: dropping relay "+kind+" for unknown relay",
			"actor", actorURI, "relayId", relayID, "err", err)
		return false, nil
	}
	if !relayActorOwnsRelay(actorURI, rel) {
		slog.Warn("federation: dropping forged relay "+kind+" (actor is not the relay)",
			"actor", actorURI, "relayId", relayID, "relayInbox", rel.Inbox)
		return false, nil
	}
	// 署名検証で actor は取り込み済みなので、通常は DB から引けて fetch しない。
	// 引けないときは捨てずにエラーを返して再試行させる。通常の Follow の
	// Accept / Reject と同じ扱いで、relay は Accept を送り直さないので、捨てると
	// requesting のまま止まる。
	actor, err := p.resolver.ResolveActor(actorURI)
	if err != nil {
		return false, fmt.Errorf("relay %s: resolve actor: %w", kind, err)
	}
	if !relayActorInboxMatches(actor, rel) {
		slog.Warn("federation: dropping relay "+kind+" (actor inbox does not match relay inbox)",
			"actor", actorURI, "relayId", relayID, "relayInbox", rel.Inbox)
		return false, nil
	}
	return true, nil
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

// mayBeQuoteAuthorization reports whether a Delete's object may be a FEP-044f
// approval: a bare id, or an object typed QuoteAuthorization.
func mayBeQuoteAuthorization(object json.RawMessage) bool {
	var id string
	if json.Unmarshal(object, &id) == nil {
		return true
	}
	var obj struct {
		Type json.RawMessage `json:"type"`
	}
	if json.Unmarshal(object, &obj) != nil {
		return false
	}
	return apTypeIs(obj.Type, "QuoteAuthorization")
}
