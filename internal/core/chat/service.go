// Package chat provides a thin service layer over ChatRepository that
// dispatches real-time delivery events to the matching WebSocket channel.
//
// Phase 9.8 — メッセージ送信 / 削除 / 編集 / 既読マークの各 API はこの service
// を経由することで、(a) DB への永続化と (b) Redis pubsub 経由の WebSocket
// 配信を同時に行う。本家 Misskey の ChatService.createMessageToUser /
// createMessageToRoom / deleteMessage の挙動を移植している。
package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/activitypub/mfm"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/colfit"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/misc/idnhost"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// AP 由来の値を書く chat 列の上限 (migration/000022_chat.up.sql)。溢れると
// INSERT ごと落ちて、その配送が retry を使い切って dead になる (#2726)。
//
// 判断は docs/divergence/security.md の「リモート由来の文字列を列に入れるときの規則」
// どおり: 本文 (name / description / text) は切る、身元 (id / uri) は
// document ごと拒否する。
const (
	chatRoomNameMaxRunes        = 256
	chatRoomDescriptionMaxRunes = 2048
	chatMessageTextMaxRunes     = 4096
	chatMessageURIMaxRunes      = 512
	// chatRoomURIMaxRunes is `chat_room.uri` (migration/000090_chat_room_host.up.sql).
	//
	// **room の身元は URI であって room id ではない (#2994)。** id は行を作るときに
	// こちらで採番するので相手の値は入らない。収まらない URI は room として
	// 認識しない (身元は切れない)。
	chatRoomURIMaxRunes = 512
	// chatRoomHostMaxRunes is `chat_room.host`.
	chatRoomHostMaxRunes = 128
)

// Errors returned by Service.
var (
	// ErrNotFound is returned when a referenced message or room does not exist.
	ErrNotFound = errors.New("chat resource not found")
	// ErrForbidden is returned when the acting user cannot modify the target
	// resource (e.g. deleting someone else's message, posting to a room they
	// are not a member of).
	ErrForbidden = errors.New("chat action forbidden")
	// ErrInvalidTarget is returned when neither toUserId nor toRoomId is
	// provided to CreateMessage helpers.
	ErrInvalidTarget = errors.New("chat target is required")
	// ErrRoomOwnerMismatch is returned by EnsureRoomViaAP when a room with the
	// requested ID already exists locally but is owned by a different user. It
	// signals a permanent (non-retryable) condition: the federated room ID
	// collides with an unrelated local room, so the inbound activity can never
	// be applied. Callers should not retry.
	ErrRoomOwnerMismatch = errors.New("chat room federation: room owner mismatch")
	// ErrChatBlocked is returned by CreateMessageToUser when the recipient has
	// blocked the sender. Mirrors upstream ChatService's 'blocked' error
	// (YOU_HAVE_BEEN_BLOCKED).
	ErrChatBlocked = errors.New("you have been blocked by the recipient")
	// ErrMessageAccess is returned by React and Unreact when the target message
	// does not exist, is the actor's own message, is a 1-on-1 message not
	// addressed to the actor, or belongs to a room the actor is not a member of.
	// Mirrors upstream ChatMessageAccessError (2026.10.0), which the API maps to
	// NO_SUCH_MESSAGE so that these cases are indistinguishable to the caller.
	ErrMessageAccess = errors.New("chat message not accessible")
	// ErrTooManyReactions is returned by React when the message already has the
	// maximum number of reactions (upstream MAX_REACTIONS_PER_MESSAGE).
	ErrTooManyReactions = errors.New("too many reactions")
	// ErrNoSuchEmoji is returned by React when a custom emoji reaction references
	// a local emoji that does not exist (upstream 'no such emoji').
	ErrNoSuchEmoji = errors.New("no such emoji")
	// ErrRoomFull is returned by CreateInvitationViaAP when the room's members
	// plus its pending invitations already fill maxRoomMembers. Mirrors upstream
	// createRoomInvitation's 'room is full' (upstream counts memberships only).
	// Permanent: callers must not retry.
	ErrRoomFull = errors.New("chat room is full")
)

// maxReactionsPerMessage caps the reactions on a single chat message
// (upstream MAX_REACTIONS_PER_MESSAGE = 100).
const maxReactionsPerMessage = 100

// maxRoomMembers mirrors upstream ChatService.MAX_ROOM_MEMBERS (= 50).
//
// owner は membership 行を持たないので、数えるのは owner を除いたメンバー数
// (upstream countBy({roomId}) と同じ)。api 層 (`internal/api/chat/handler.go`)
// が同じ値を自前で持っており、片側だけ動かすと AP 経路とローカル API で定員が
// 食い違う。`TestMaxRoomMembersMatchesAPILayer` が両者の一致を固定する。
const maxRoomMembers = 50

// chatCustomEmojiPattern matches a local custom-emoji reaction (`:name:` or the
// canonical `:name@.:` form). upstream isCustomEmojiRegexp は local emoji だけを
// 対象とするため host 付き (`:name@host:`) は unicode として扱う。
var chatCustomEmojiPattern = regexp.MustCompile(`^:([\w+\-]+)(?:@\.)?:$`)

// StreamingPublisher is the Redis pub/sub dispatch interface the service uses
// to broadcast chat events. Implementations live in internal/stream so the
// concrete pub/sub bus can be injected without creating a cycle.
//
// 本家 Misskey の GlobalEventService.publishChatUserStream /
// publishChatRoomStream に対応する。
type StreamingPublisher interface {
	PublishUserMessage(ctx context.Context, fromUserID, toUserID, eventType string, body any)
	PublishRoomMessage(ctx context.Context, roomID, eventType string, body any)
}

// APDeliverer delivers a pre-rendered activity body to a single remote user's
// inbox. Implemented by federation.DeliverService.DeliverToUser.
type APDeliverer interface {
	DeliverToUser(signerUserID string, recipient *model.User, body []byte) error
}

// MainStreamPublisher emits real-time events to a single target user's `main`
// WebSocket channel. Used here to deliver `newChatMessage` events to message
// recipients so the frontend can surface unread-chat indicators immediately.
// 循環依存を避けるため interface で受け取る (実装は internal/stream)。
type MainStreamPublisher interface {
	PublishMainEvent(userID, eventType string, body any)
}

// Event types mirrored by the WebSocket channel. 本家と同名。
const (
	EventMessage = "message"
	EventDeleted = "deleted"
	EventEdited  = "edited"
	EventRead    = "read"
	EventReact   = "react"
	EventUnreact = "unreact"
)

// ModeratorChecker reports whether a given user has moderator privileges.
// パッケージ間の循環依存を避けるため interface で受け取る (実装は core/role.Service)。
type ModeratorChecker interface {
	IsModerator(userID string) bool
}

// Service implements chat message CRUD + streaming fan-out.
type Service struct {
	repo                repository.ChatRepository
	idGen               id.Generator
	publisher           StreamingPublisher
	userRepo            repository.UserRepository
	followingRepo       repository.FollowingRepository
	renderer            *activitypub.Renderer
	urls                *activitypub.URLBuilder
	deliverer           APDeliverer
	mainStreamPublisher MainStreamPublisher
	chatPusher          ChatPusher
	// moderatorChecker: chat/rooms/show 等の権限 gate で「moderator は閲覧可」
	// を判定するために使う (upstream 2026.5.4 互換)。nil 安全。
	moderatorChecker ModeratorChecker
	// blockingRepo: 1-on-1 DM 送信時に recipient が sender を block して
	// いないか判定する (upstream ChatService の YOU_HAVE_BEEN_BLOCKED gate)。
	// nil なら block check は skip。
	blockingRepo repository.BlockingRepository
	// invitationNotifier: AP 経由で受け取った招待を local invitee へ通知する
	// (#1559)。未配線なら通知しない。
	invitationNotifier ChatInvitationNotifier
	// emojiRepo: React で custom emoji (`:name:`) reaction の存在検証に使う
	// (#1541)。nil の場合は custom emoji を検証せず `:name:` をそのまま採用する
	// (legacy/test path)。
	emojiRepo repository.EmojiRepository
	// approvalRepo: 1-on-1 DM の chatApproval bypass に使う (#1748)。相手が過去に
	// 自分へ送ってきている (otherApprovedMe) なら chatScope チェックを skip し、
	// 送信後に自分→相手の approval を挿入する。nil なら従来どおり scope のみで判定。
	approvalRepo repository.ChatApprovalRepository
}

// ChatInvitationNotifier records a 'chatRoomInvitationReceived' notification on
// the invitee's stream (#1559)。実装は core/notification.Hook。
type ChatInvitationNotifier interface {
	OnChatRoomInvitationReceived(inviteeID, inviterID, invitationID string)
}

// SetInvitationNotifier wires the notifier used by CreateInvitationViaAP to
// notify local invitees of inbound (federated) chat room invitations (#1559)。
func (s *Service) SetInvitationNotifier(n ChatInvitationNotifier) {
	s.invitationNotifier = n
}

// SetEmojiRepo wires the emoji repository so React can validate custom-emoji
// reactions against the local emoji table (#1541). nil disables the check.
func (s *Service) SetEmojiRepo(r repository.EmojiRepository) {
	s.emojiRepo = r
}

// SetApprovalRepo wires the chat-approval repository so CreateMessageToUser can
// apply the upstream chatApproval bypass: skip the chatScope check when the
// recipient previously messaged the sender, and record an approval after a
// successful send (#1748). nil disables the bypass (scope-only behaviour).
func (s *Service) SetApprovalRepo(r repository.ChatApprovalRepository) {
	s.approvalRepo = r
}

// NewService constructs a chat Service.
func NewService(repo repository.ChatRepository, idGen id.Generator) *Service {
	return &Service{repo: repo, idGen: idGen}
}

// SetStreamingPublisher wires a StreamingPublisher so state-changing methods
// can dispatch to the WebSocket channel. nil 渡しで配信は無効になる (既存の
// DB 処理は変わらない)。
func (s *Service) SetStreamingPublisher(p StreamingPublisher) {
	s.publisher = p
}

// ChatPusher delivers the Web Push half of a `newChatMessage` event.
//
// upstream ChatService.ts は main stream への publish と Web Push を**必ず同じ
// 場所で対にして**送る (1:1 チャットと room の 2 箇所)。mk-go は publish 側しか
// 持っておらず、`PushNewChatMessage` は実装も SW の受け口も揃っているのに
// **呼ぶ側が存在しなかった** (#2840)。エラーもログも出ないので、ブラウザを
// 閉じている利用者に通知が届かないだけだった。
type ChatPusher interface {
	PushNewChatMessage(userID string, body map[string]any)
}

// SetChatPusher attaches the Web Push sender used alongside `newChatMessage`.
func (s *Service) SetChatPusher(p ChatPusher) {
	s.chatPusher = p
}

// SetUserRepo attaches the user repository used to resolve the sender for the
// Web Push body.
//
// **SetAPDelivery とは別に持つ。** push は AP 配送とは無関係なので、AP が
// 未配線の構成でも sender を引けるようにする。
func (s *Service) SetUserRepo(r repository.UserRepository) {
	s.userRepo = r
}

// pushNewChatMessage sends the Web Push half. **main stream の publish と必ず
// 対で呼ぶこと** — 片方だけになると、タブを開いている利用者にしか届かない。
//
// **body は publish と同じでは足りない。** SW の newChatMessage ハンドラ
// (packages/sw/src/scripts/create-notification.ts) は `fromUser.name` /
// `fromUser.avatarUrl` を無条件で読み、room なら `toRoom.name` も読む。
// packMessageStream は `msg.FromUser` / `msg.ToRoom` が nil だと該当キーを
// 落とすが、create 経路は行を素で組むので**必ず nil** (repo は Preload しない)。
// そのまま渡すと SW が TypeError を投げ、**通知が 1 件も出ない** —
// publish 側は body を読まない (main-boot.ts は引数を捨てる) ので気付けない。
func (s *Service) pushNewChatMessage(userID string, packed map[string]any, sender *model.User, room *model.ChatRoom) {
	if s.chatPusher == nil {
		return
	}
	// **sender を引けなければ送らない。** fromUser の無い body を送ると SW が
	// TypeError で落ち、通知が出ないうえに push の枠だけ消費する。無通知に
	// 倒すほうが安全。
	if sender == nil {
		slog.Warn("chat: skipping newChatMessage push (sender unresolved)", "userId", userID)
		return
	}
	s.chatPusher.PushNewChatMessage(userID, s.packMessagePush(packed, sender, room))
}

// packMessagePush augments the stream payload with the fields the service
// worker dereferences unconditionally.
func (s *Service) packMessagePush(packed map[string]any, sender *model.User, room *model.ChatRoom) map[string]any {
	out := make(map[string]any, len(packed)+2)
	for k, v := range packed {
		out[k] = v
	}
	if _, ok := out["fromUser"]; !ok {
		out["fromUser"] = packUserStream(sender)
	}
	// **text キーを必ず持たせる。** packMessage は msg.Text が nil だとキーごと
	// 落とすが、SW は `${name}: ${body.text}` を無条件に組むので添付のみの
	// メッセージが「alice: undefined」になる。upstream は null を送る。
	if _, ok := out["text"]; !ok {
		out["text"] = nil
	}
	if room != nil {
		// **toRoom が無いと DM 分岐に落ちる。** tag が chat:room:<id> ではなく
		// chat:user:<senderId> になり、別 room の通知が同じ tag で潰れる。
		out["toRoom"] = map[string]any{"id": room.ID, "name": room.Name}
	}
	return out
}

// senderForPush resolves the message author for the push body. Returns nil when
// the repo is unwired or the lookup fails; pushNewChatMessage then drops the
// push entirely rather than sending a body the service worker cannot read.
func (s *Service) senderForPush(fromUserID string) *model.User {
	if s.userRepo == nil {
		return nil
	}
	u, err := s.userRepo.FindByID(fromUserID)
	if err != nil {
		slog.Warn("chat: sender lookup failed, skipping newChatMessage push", "fromUserId", fromUserID, "err", err)
		return nil
	}
	return u
}

// SetMainStreamPublisher attaches a publisher used to emit `newChatMessage`
// on the recipient(s)' main channel. Optional — nil disables main-stream emit
// but chat-stream `message` events remain unaffected.
func (s *Service) SetMainStreamPublisher(p MainStreamPublisher) {
	s.mainStreamPublisher = p
}

// SetAPDelivery wires the AP federation layer for chat message delivery to
// remote users. All parameters are required; pass nil to disable federation.
func (s *Service) SetAPDelivery(userRepo repository.UserRepository, renderer *activitypub.Renderer, urls *activitypub.URLBuilder, deliverer APDeliverer) {
	s.userRepo = userRepo
	s.renderer = renderer
	s.urls = urls
	s.deliverer = deliverer
}

// SetModeratorChecker wires a ModeratorChecker so chat/rooms/show 等の権限
// gate で moderator role を持つ user が閲覧できることを判定できる。
// nil なら moderator bypass は無効化 (= owner / member / 招待のみ true)。
func (s *Service) SetModeratorChecker(m ModeratorChecker) {
	s.moderatorChecker = m
}

// SetBlockingRepo wires a BlockingRepository so CreateMessageToUser can reject
// DMs to a recipient who has blocked the sender. nil disables the block check.
func (s *Service) SetBlockingRepo(r repository.BlockingRepository) {
	s.blockingRepo = r
}

// HasPermissionToViewRoomInfo は room のメタ情報を閲覧できる権限を持つか判定する
// (upstream Misskey TS 2026.5.4 `ChatService.hasPermissionToViewRoomInfo` 互換)。
// 返り値 true の条件:
//   - room の owner である
//   - room の member である
//   - room への招待を受け取っている (chat_room_invitation 行が存在する)
//   - 自身が moderator (ModeratorChecker 設定時のみ)
//
// 旧 mk-go の chat/rooms/show は権限 check 無しで誰でも room メタ情報を取得できる
// drop-in regression 状態だったため、本 helper を新設して /api/chat/rooms/show
// 等で gate する (#1164 Phase C)。
func (s *Service) HasPermissionToViewRoomInfo(meID string, room *model.ChatRoom) (bool, error) {
	if room == nil {
		return false, nil
	}
	if room.OwnerID == meID {
		return true, nil
	}
	if _, err := s.repo.FindMembership(meID, room.ID); err == nil {
		return true, nil
	}
	if _, err := s.repo.FindInvitation(meID, room.ID); err == nil {
		return true, nil
	}
	if s.moderatorChecker != nil && s.moderatorChecker.IsModerator(meID) {
		return true, nil
	}
	return false, nil
}

// SetFollowingRepo wires the following repository so chatScope=followers /
// following / mutual checks can be enforced (#692). 未配線のままで granular
// scope を持つ recipient へ送信しようとすると `canChat` が fail-closed で
// reject する (旧実装の silent allow は意図しない open relay を作るため
// PR #708 review で改めた)。production では必ず配線する。
func (s *Service) SetFollowingRepo(repo repository.FollowingRepository) {
	s.followingRepo = repo
}

// ErrChatScopeViolation is returned when a chat message is rejected because
// the recipient's chatScope does not allow the sender. CherryPick の
// `Error('recipient is cannot chat (...)')` 相当 (#692)。
var ErrChatScopeViolation = errors.New("chat scope violation")

// ErrChatScopeUnconfigured is returned when canChat() needs to consult the
// following graph (chatScope = followers / following / mutual) but no
// FollowingRepository was wired. Treated as fail-closed (= reject) instead
// of silently degrading to everyone, which would be an open relay (#708 review)。
var ErrChatScopeUnconfigured = errors.New("chat scope check unconfigured: followingRepo missing")

// canChat reports whether `sender` is allowed to send a 1-on-1 chat to
// `recipient` based on recipient.chatScope. Returns nil when allowed.
//
// scope semantics (CherryPick / Misskey TS と一致):
//   - "everyone": 誰からでも受信
//   - "followers": 受信者を follow している人だけ (sender が recipient の follower)
//   - "following": 受信者が follow している人だけ (recipient が sender を follow)
//   - "mutual": 双方向
//   - "none": 完全拒否
//
// remote recipient の場合は、AP に expose されるのは `_misskey_canChat`
// (boolean) だけなので chatScope は "everyone" / "none" の二値しか取り得ない
// (resolver で翻訳済み, #692)。granular な判定は受信側 instance に委ねる。
//
// followingRepo 未配線で granular scope を判定しないといけない局面では
// `ErrChatScopeUnconfigured` を返して fail-closed する (#708 review #2)。
// silent に everyone 降格すると production の wiring 忘れが open relay に
// なり、chatScope 設定が黙って無効化される。
func (s *Service) canChat(sender, recipient *model.User) error {
	if recipient == nil || sender == nil {
		return nil
	}
	if recipient.ChatScope == "none" {
		return ErrChatScopeViolation
	}
	if !recipient.IsLocal() {
		// 連合先のスコープは "everyone" / "none" の二値。connection grain の
		// followers/following/mutual は remote 側で評価されるので mk-go では
		// 'none' だけ事前 reject すれば十分。
		return nil
	}
	switch recipient.ChatScope {
	case "", "everyone":
		return nil
	}
	// granular scope (followers / following / mutual) は follow graph 必須。
	// 未配線なら fail-closed する。
	if s.followingRepo == nil {
		return ErrChatScopeUnconfigured
	}
	switch recipient.ChatScope {
	case "followers":
		ok, err := s.followingRepo.Exists(sender.ID, recipient.ID)
		if err == nil && ok {
			return nil
		}
		return ErrChatScopeViolation
	case "following":
		ok, err := s.followingRepo.Exists(recipient.ID, sender.ID)
		if err == nil && ok {
			return nil
		}
		return ErrChatScopeViolation
	case "mutual":
		ok1, e1 := s.followingRepo.Exists(sender.ID, recipient.ID)
		ok2, e2 := s.followingRepo.Exists(recipient.ID, sender.ID)
		if e1 == nil && e2 == nil && ok1 && ok2 {
			return nil
		}
		return ErrChatScopeViolation
	}
	return nil
}

// recordChatApproval records a (fromUserID -> toUserID) chat approval after a
// successful 1-on-1 send so the recipient can later reply regardless of the
// sender's chatScope (upstream ChatService.createMessageToUser の送信後 approval
// 挿入)。冪等: 既に存在すれば挿入しない。best-effort で error はログのみ (#1748)。
func (s *Service) recordChatApproval(fromUserID, toUserID string) {
	if s.approvalRepo == nil {
		return
	}
	// iApprovedOther: 自分が既に相手を許可済みなら重複挿入しない。
	if ok, err := s.approvalRepo.Exists(fromUserID, toUserID); err != nil || ok {
		return
	}
	if err := s.approvalRepo.Create(&model.ChatApproval{
		ID:      s.idGen.Generate(time.Now()),
		UserID:  fromUserID,
		OtherID: toUserID,
	}); err != nil {
		slog.Warn("chat: failed to record chat approval", "from", fromUserID, "to", toUserID, "err", err)
	}
}

// --- Message lifecycle ---

// CreateMessageToUser persists a direct-message row and fires the
// `message` event on both conversation directions so that either user's
// connected WebSocket subscribers see the message immediately.
//
// chatScope を強制する (#692)。recipient が local なら granular な
// followers/following/mutual まで判定し、remote なら `_misskey_canChat`
// 由来の "none" だけ事前 reject する (canChat() が分岐)。
func (s *Service) CreateMessageToUser(ctx context.Context, fromUserID, toUserID, text, fileID string) (*model.ChatMessage, error) {
	if fromUserID == "" || toUserID == "" {
		return nil, ErrInvalidTarget
	}
	// upstream は scope チェックの前に chatApprovals を見て、相手 (toUser) が過去に
	// 自分 (fromUser) へ送ってきている (otherApprovedMe) 場合は chatScope を bypass
	// する (#1748)。approvalRepo 未配線なら従来どおり scope のみで判定する。
	otherApprovedMe := false
	if s.approvalRepo != nil {
		if ok, err := s.approvalRepo.Exists(toUserID, fromUserID); err == nil {
			otherApprovedMe = ok
		}
	}
	if !otherApprovedMe && s.userRepo != nil {
		recipient, err := s.userRepo.FindByID(toUserID)
		if err == nil {
			sender, err := s.userRepo.FindByID(fromUserID)
			if err == nil {
				if err := s.canChat(sender, recipient); err != nil {
					return nil, err
				}
			}
		}
	}
	// recipient が sender を block している場合は DM を拒否する (upstream
	// ChatService.createMessageToUser の checkBlocked(toUser, fromUser) 相当)。
	// DB error は fail-closed で扱う (canChat の granular check と同方針)。
	if s.blockingRepo != nil {
		blocked, err := s.blockingRepo.Exists(toUserID, fromUserID)
		if err != nil {
			return nil, fmt.Errorf("chat block check: %w", err)
		}
		if blocked {
			return nil, ErrChatBlocked
		}
	}
	msg := &model.ChatMessage{
		ID:         s.idGen.Generate(time.Now()),
		FromUserID: fromUserID,
		ToUserID:   &toUserID,
	}
	// upstream は text.trim() を保存する (ChatService.createMessageToUser)。
	text = strings.TrimSpace(text)
	if text != "" {
		msg.Text = &text
	}
	if fileID != "" {
		msg.FileID = &fileID
	}
	if err := s.repo.CreateMessage(msg); err != nil {
		return nil, fmt.Errorf("create user message: %w", err)
	}
	// upstream は送信後、自分が相手をまだ許可していなければ (iApprovedOther でなければ)
	// 自分→相手の approval を挿入する (= 以降、相手は自分へ scope に依らず返信できる)。
	// best-effort: 失敗してもメッセージ送信は成功扱い (#1748)。
	s.recordChatApproval(fromUserID, toUserID)
	if s.publisher != nil {
		s.publisher.PublishUserMessage(ctx, fromUserID, toUserID, EventMessage, s.packMessageStream(msg))
	}
	// Misskey 本家の ChatService.createMessageToUser は、3 秒後に未読判定して
	// newChatMessage を publishMainStream する。本実装では未読判定を省略し、
	// 作成と同時に recipient (toUserID) の main に emit する (sender には
	// 送らない: TS と同じ fan-out 方針)。
	if s.mainStreamPublisher != nil {
		packed := s.packMessageStream(msg)
		s.mainStreamPublisher.PublishMainEvent(toUserID, "newChatMessage", packed)
		var sender *model.User
		if s.chatPusher != nil {
			sender = s.senderForPush(fromUserID)
		}
		s.pushNewChatMessage(toUserID, packed, sender, nil)
	}
	// リモートユーザー宛ならAP配送 (best-effort)
	s.tryDeliverToRemoteUser(msg, fromUserID, toUserID)
	return msg, nil
}

// tryDeliverToRemoteUser attempts AP delivery for a 1-on-1 chat message when
// the recipient is a remote user. Errors are logged and swallowed (DB commit
// is already done).
func (s *Service) tryDeliverToRemoteUser(msg *model.ChatMessage, fromUserID, toUserID string) {
	if s.deliverer == nil || s.userRepo == nil || s.renderer == nil || s.urls == nil {
		return
	}
	recipient, err := s.userRepo.FindByID(toUserID)
	if err != nil || recipient.IsLocal() {
		return
	}
	sender, err := s.userRepo.FindByID(fromUserID)
	if err != nil || !sender.IsLocal() {
		return
	}
	senderURI := s.urls.UserURI(sender.ID)
	recipientURI := ""
	if recipient.URI != nil {
		recipientURI = *recipient.URI
	}
	if recipientURI == "" {
		return
	}
	_ = s.repo.UpdateDeliveryStatus(msg.ID, true, false)
	// CherryPick 互換 wire format: Create + Note(_misskey_talk:true) (#692)。
	// published は ChatMessage.ID (ULID/aidx) のタイムスタンプから導出する。
	published := time.Now().UTC().Format(time.RFC3339)
	if t, err := s.idGen.ParseTime(msg.ID); err == nil {
		published = t.UTC().Format(time.RFC3339)
	}
	cm := s.renderer.RenderChatMessage(msg, senderURI, recipientURI, published)
	body, err := json.Marshal(cm)
	if err != nil {
		_ = s.repo.UpdateDeliveryStatus(msg.ID, false, true)
		slog.Warn("chat: failed to marshal ChatMessage activity", "msgID", msg.ID, "err", err)
		return
	}
	if err := s.deliverer.DeliverToUser(sender.ID, recipient, body); err != nil {
		_ = s.repo.UpdateDeliveryStatus(msg.ID, false, true)
		slog.Warn("chat: failed to deliver ChatMessage to remote user", "msgID", msg.ID, "err", err)
		return
	}
	_ = s.repo.UpdateDeliveryStatus(msg.ID, false, false)
}

// FederateInvitation delivers an Invite activity to a remote invitee so the
// remote instance can surface the chat room invitation (CherryPick group chat
// federation). Best-effort: no-op when AP delivery is unwired or the invitee
// is local; delivery failures are logged and swallowed (the local invitation
// row has already been committed, matching the 1-on-1 delivery policy). The
// activity is signed by the room owner (the inviter).
func (s *Service) FederateInvitation(roomID, inviteeID string) {
	if s.deliverer == nil || s.userRepo == nil || s.renderer == nil || s.urls == nil {
		return
	}
	invitee, err := s.userRepo.FindByID(inviteeID)
	if err != nil || invitee == nil || invitee.IsLocal() {
		return
	}
	inviteeURI := ""
	if invitee.URI != nil {
		inviteeURI = *invitee.URI
	}
	if inviteeURI == "" {
		return
	}
	room, err := s.repo.FindRoomByID(roomID)
	if err != nil || room == nil {
		slog.Warn("chat: invitation federation: room not found", "roomID", roomID, "err", err)
		return
	}
	// owner が remote の room は本インスタンスから Invite を署名配送できない
	// (秘密鍵が無い)。federation するのは local owner の room のみ。
	owner, err := s.userRepo.FindByID(room.OwnerID)
	if err != nil || owner == nil || !owner.IsLocal() {
		return
	}
	group := s.renderer.RenderChatRoom(room)
	invite := s.renderer.RenderInvite(room.OwnerID, group, inviteeURI)
	body, err := json.Marshal(invite)
	if err != nil {
		slog.Warn("chat: invitation federation: marshal failed", "roomID", roomID, "err", err)
		return
	}
	if err := s.deliverer.DeliverToUser(room.OwnerID, invitee, body); err != nil {
		slog.Warn("chat: invitation federation: deliver failed", "roomID", roomID, "inviteeID", inviteeID, "err", err)
	}
}

// EnsureRoomViaAP creates a local copy of a remote chat room, keyed by its
// canonical AP URI. Idempotent: an existing room with a matching owner is a
// no-op. An existing room with a different owner fails so federated
// invitations never attach to a room somebody else controls.
//
// **キーは URI で、room id ではない (#2994)。** room id は相手が自由に決められる
// 値なので ID 空間はホストをまたいで共有されている。id で keying していた頃は、
// あるホストが先に同じ id の room を作ると、別のホストの正規の room からの Invite が
// owner 不一致で**恒久的に drop されて**いた。取り込む行の `id` はこちらで採番する。
func (s *Service) EnsureRoomViaAP(roomURI, name, summary, ownerUserID string) error {
	if roomURI == "" || ownerUserID == "" {
		return ErrInvalidTarget
	}
	// 身元なので切れない。収まらなければ room として受け取らない (#2726)。
	if !colfit.Fits(roomURI, chatRoomURIMaxRunes) {
		return ErrInvalidTarget
	}
	// **自ホストの URI を名乗る Invite は取り込まない。** federation 側の
	// sameDeliveryHost が先に落とすが、判断を書き込みの隣にも置く — 取り込むと
	// 「ローカルの room なのに uri が付いた行」ができ、以後その room への Accept が
	// URI 経路と id 経路のどちらでも引けてしまう。
	if s.isLocalRoomURI(roomURI) {
		return ErrInvalidTarget
	}
	// **綴りを畳んでから身元にする。** 比較側 (`sameDeliveryHost`) は punycode と
	// 既定ポートを畳むので、`https://パイ.example/...` と
	// `https://xn--eckve.example/...`、`:443` の有無はどれも gate を通る。畳まずに
	// 保存すると**同じ room が別々の identity になり**、Invite で作った行に本文が
	// 届かなくなる (#1850 が同じ形を報告している)。
	canonical := idnhost.CanonicalURI(roomURI)
	if canonical == "" {
		return ErrInvalidTarget
	}
	if !colfit.Fits(canonical, chatRoomURIMaxRunes) {
		return ErrInvalidTarget
	}
	host := apURIHost(canonical)
	if host == "" || !colfit.Fits(host, chatRoomHostMaxRunes) {
		return ErrInvalidTarget
	}

	// **DB 障害を「その room は無い」に丸めない (#2792)。** 丸めると一過性の
	// エラーの間だけ二重取り込みへ進み、UNIQUE index に当たって job が dead になる。
	existing, err := s.repo.FindRoomByURI(canonical)
	if err != nil && !repository.IsNotFound(err) {
		return err
	}
	if err == nil && existing != nil {
		if existing.OwnerID != ownerUserID {
			return fmt.Errorf("%w: room %s", ErrRoomOwnerMismatch, canonical)
		}
		return nil
	}
	// name / description は表示用の本文なので切って NUL を落とす。
	return s.repo.CreateRoom(&model.ChatRoom{
		ID:          s.idGen.Generate(time.Now()),
		Name:        colfit.Text(name, chatRoomNameMaxRunes),
		Description: colfit.Text(summary, chatRoomDescriptionMaxRunes),
		OwnerID:     ownerUserID,
		Host:        &host,
		URI:         &canonical,
	})
}

// resolveRoomByAPURI resolves the room an inbound activity refers to.
//
// **URI が指す先は 2 通りある。** 取り込んだ copy (`uri` を持つ) か、こちらの room
// (`uri` は NULL)。片方しか見ないと、自分の room への Accept (相手がこちらの招待を
// 受けた形) か、相手の room からの本文のどちらかが丸ごと落ちる。
//
// **URI で引いてから id へ落ちる順序が要点。** 先に id で引くと、他ホストの URI に
// 含まれる id がたまたま取り込んだ copy の id と一致したときにその copy を掴む。
func (s *Service) resolveRoomByAPURI(roomURI string) (*model.ChatRoom, error) {
	if roomURI == "" || !colfit.Fits(roomURI, chatRoomURIMaxRunes) {
		return nil, ErrInvalidTarget
	}
	// 保存側と同じ正規形にしてから引く (綴り違いで取り違えない)。
	//
	// **host を持たない URI は room の身元にならない。** `@context` に
	// `/chat/rooms/<id>` のような相対形を書かれると、host の判定を通らないまま
	// id だけでローカル room に届いてしまう。
	canonical := idnhost.CanonicalURI(roomURI)
	if canonical == "" || !colfit.Fits(canonical, chatRoomURIMaxRunes) {
		return nil, ErrInvalidTarget
	}
	room, err := s.repo.FindRoomByURI(canonical)
	if err != nil && !repository.IsNotFound(err) {
		return nil, err
	}
	if err == nil && room != nil {
		return room, nil
	}

	// **ここから先はこちらの room。** 取り込んだ copy は上で引けている。
	//
	// **相手が返してくる URI の綴りには賭けない。** こちらが送った URI をそのまま
	// 返す実装 (mk-go 同士) なら自ホストの URI で来るが、自分の copy から
	// `${config.url}/chat/rooms/${room.id}` を組み立て直す実装は**相手の host +
	// こちらの room id** を送ってくる。id 一致で引いていた #2994 以前はそれで
	// 通っていたので、host の一致まで求めると interop の回帰になる。
	//
	// **安全性はここに依存していない。** Accept は保留中の招待、Reject / Remove は
	// 本人の行、group message はメンバー判定で、いずれも呼び出し先が独立に認可する。
	// 行を**作る** `EnsureRoomViaAP` だけが URI に厳格である必要があり、そこが
	// #2994 の穴だった。
	id := apRoomIDFromURI(roomURI)
	if id == "" {
		return nil, ErrNotFound
	}
	room, err = s.repo.FindRoomByID(id)
	if err != nil && !repository.IsNotFound(err) {
		return nil, err
	}
	// **取り込んだ copy を id で掴まない。** ローカルで採番した id が URI の id と
	// 一致する余地があるので、`uri` を持つ行はここでは返さない。
	if err != nil || room == nil || (room.URI != nil && *room.URI != "") {
		return nil, ErrNotFound
	}
	return room, nil
}

// isLocalRoomURI reports whether roomURI is this instance's own room URI.
func (s *Service) isLocalRoomURI(roomURI string) bool {
	return s.localRoomIDFromAPURI(roomURI) != ""
}

// localRoomIDFromAPURI returns the room id when roomURI is this instance's own
// canonical room URI, else "".
//
// **自前の URL builder で組み立て直して突き合わせる。** 両辺を正規形にしてから
// 比べるので、`:443` や大小文字の揺れでは外れない。
//
// **URL builder が未配線なら "" を返す = 「自ホストではない」と答える。**
// これは fail-open 側で、唯一の利用者である `EnsureRoomViaAP` の拒否が効かなく
// なる。本番では `router.go` が必ず配線するうえ、実際の防波堤は federation 側の
// `sameDeliveryHost` と local actor の除外なので、ここは多層防御の 1 枚目。
func (s *Service) localRoomIDFromAPURI(roomURI string) string {
	if s.urls == nil {
		return ""
	}
	id := apRoomIDFromURI(roomURI)
	if id == "" {
		return ""
	}
	mine := idnhost.CanonicalURI(s.urls.ChatRoomURI(id))
	theirs := idnhost.CanonicalURI(roomURI)
	if mine == "" || mine != theirs {
		return ""
	}
	return id
}

// CreateInvitationViaAP records a pending chat room invitation for a local
// user received from a remote room. Idempotent.
//
// AP 経路は署名が通る remote actor なら誰でも叩けるので、ローカル API
// (`chat/rooms/invitations/create`) が強制している条件をここでも通す:
// room の実在 / owner 自身への招待でない / 既存メンバーでない / 既存招待で
// ない / 定員 (upstream MAX_ROOM_MEMBERS)。
//
// 加えて **invitee が room owner (= 招待者) を block していれば招待ごと拒否する**。
// upstream の createRoomInvitation は `// TODO: cehck block` のまま block を見ないが、
// 1-on-1 DM の受信 (CreateMessageViaAP) は見ており、同じ chat 機能の中で AP 経路の
// 招待だけが無制限に通っていた (通知も発火する)。向きは DM と揃えて
// `Exists(invitee, owner)` = 受け手が送り手を block しているか。
//
// **chatScope (canChat) は見ない。** upstream で chatScope を評価するのは
// createMessageToUser だけで、room の招待は対象外 (createRoomInvitation にも
// ローカル API にも無い)。招待に広げると「chatScope=followers の利用者が
// 非 follower の room に呼ばれなくなる」形で AP 経路だけ挙動が変わり、ローカル
// API との非対称も増える。block は「その相手を既に拒否している」という invitee
// 自身の意思表示なので、こちらだけを適用する。
//
// ErrChatBlocked / ErrRoomFull / ErrNotFound はいずれも retry では解決しない条件なので、
// 呼び出し側 (federation の inbox handler) はこれらを non-retry に落とす。
func (s *Service) CreateInvitationViaAP(roomURI, inviteeUserID string) error {
	if roomURI == "" || inviteeUserID == "" {
		return ErrInvalidTarget
	}
	// room は block 判定の相手 (= inviter = owner) と定員判定に要る。
	// `chat_room_invitation.roomId` は `chat_room(id)` への FK (000022_chat.up.sql)
	// なので、room 不在で行だけ作ることはそもそもできない。
	room, err := s.resolveRoomByAPURI(roomURI)
	if err != nil {
		return err
	}
	roomID := room.ID
	// 既存招待は冪等 no-op (通知も再送しない)。**DB 障害を「招待が無い」に
	// 丸めない** (#2792) — 丸めると一過性のエラーの間だけ UNIQUE 違反まで進む。
	existing, ierr := s.repo.FindInvitation(inviteeUserID, roomID)
	if ierr != nil && !repository.IsNotFound(ierr) {
		return ierr
	}
	if ierr == nil && existing != nil {
		return nil
	}
	// owner 自身への招待は行を作らない (upstream 'yourself')。owner は暗黙の
	// メンバーなので、残しても AddMemberViaAP が消費するだけ (#2858)。
	if room.OwnerID == inviteeUserID {
		return nil
	}
	// 既にメンバーなら招待不要 (upstream 'already member')。
	// ここでも DB 障害を not-found に丸めない。
	if _, err := s.repo.FindMembership(inviteeUserID, roomID); err == nil {
		return nil
	} else if !repository.IsNotFound(err) {
		return err
	}
	// invitee が owner を block していれば招待を作らない (通知も出さない)。
	// CreateMessageViaAP の checkBlocked(to, from) gate と同じ形。
	if s.blockingRepo != nil {
		blocked, berr := s.blockingRepo.Exists(inviteeUserID, room.OwnerID)
		if berr != nil {
			// fail-closed: block 判定が引けない間は招待を作らない。
			return fmt.Errorf("chat block check: %w", berr)
		}
		if blocked {
			return ErrChatBlocked
		}
	}
	// 定員 (upstream MAX_ROOM_MEMBERS)。列挙の失敗は fail-closed にする
	// (数えられないまま作ると定員判定が素通りする)。
	members, err := s.repo.ListMembersByRoom(roomID)
	if err != nil {
		return fmt.Errorf("chat room member count: %w", err)
	}
	// **未消化の招待も定員に数える** (AP 経路のみの硬化)。upstream は membership
	// しか数えないので、誰も accept しない限り 1 つの room で invitation 行と
	// chatRoomInvitationReceived 通知を無制限に作れてしまう。room の収容人数は
	// maxRoomMembers なので、それを超える招待はどのみち全部は通らない。
	// membership だけで定員に達している upstream の 'room is full' も同じ式で落ちる。
	// limit は maxRoomMembers+1 で打ち切る (全件数える必要は無く、repository 側の
	// ページ上限 100 にも収まる)。
	pending, err := s.repo.ListInvitationsByRoom(roomID, "", "", maxRoomMembers+1)
	if err != nil {
		return fmt.Errorf("chat room invitation count: %w", err)
	}
	if len(members)+len(pending) >= maxRoomMembers {
		return ErrRoomFull
	}
	inv := &model.ChatRoomInvitation{
		ID: s.idGen.Generate(time.Now()), UserID: inviteeUserID, RoomID: roomID,
	}
	if err := s.repo.CreateInvitation(inv); err != nil {
		return err
	}
	// local invitee へ chatRoomInvitationReceived 通知を送る (#1559)。notifier
	// は招待者 (= room owner)。
	if s.invitationNotifier != nil {
		s.invitationNotifier.OnChatRoomInvitationReceived(inviteeUserID, room.OwnerID, inv.ID)
	}
	return nil
}

// AddMemberViaAP records a chat room membership for a (typically remote) user
// who accepted an invitation to our room. A pending invitation for the
// user+room is required: this prevents a remote actor from pushing itself into
// an arbitrary local room via a forged Accept. The invitation is consumed on
// success. Idempotent when membership already exists.
func (s *Service) AddMemberViaAP(roomURI, userID string) error {
	if roomURI == "" || userID == "" {
		return ErrInvalidTarget
	}
	room, rerr := s.resolveRoomByAPURI(roomURI)
	if rerr != nil {
		return rerr
	}
	roomID := room.ID
	inv, err := s.repo.FindInvitation(userID, roomID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を「招待が無い」に丸めない** (#2792)。丸めると inbox の job が
		// 成功扱いで retry されず、membership が永久に作られないまま相手だけが
		// 「参加した」と認識する。下の room lookup と判断を揃える。
		return err
	}
	if err != nil || inv == nil {
		// 招待が無い相手の Accept は無視する (なりすまし membership 防止)。
		return nil
	}
	// **owner の招待は行を作らずに消費する** (#2858)。owner は暗黙のメンバーで
	// membership 行を持たないので、譲渡で残った招待をここで実体化させない。
	if room.OwnerID == userID {
		_ = s.repo.DeleteInvitation(inv.ID)
		return nil
	}
	if _, err := s.repo.FindMembership(userID, roomID); err != nil {
		if cerr := s.repo.CreateMembership(&model.ChatRoomMembership{
			ID: s.idGen.Generate(time.Now()), UserID: userID, RoomID: roomID,
		}); cerr != nil {
			return cerr
		}
	}
	_ = s.repo.DeleteInvitation(inv.ID)
	return nil
}

// RemoveInvitationViaAP deletes a pending invitation when a remote invitee
// rejects our room invitation. No-op when none exists.
func (s *Service) RemoveInvitationViaAP(roomURI, userID string) error {
	if roomURI == "" || userID == "" {
		return ErrInvalidTarget
	}
	room, rerr := s.resolveRoomByAPURI(roomURI)
	if rerr != nil {
		return rerr
	}
	if inv, err := s.repo.FindInvitation(userID, room.ID); err == nil && inv != nil {
		return s.repo.DeleteInvitation(inv.ID)
	}
	return nil
}

// FederateInvitationResponse delivers an Accept / Reject activity to the owner
// of a remote chat room when a local user responds to that room's invitation
// (accept=true → Accept, accept=false → Reject). This lets the remote instance
// record (or drop) the local user's membership. Best-effort: no-op when AP
// delivery is unwired, the room is unknown, or the room is locally owned (own
// room → no federation needed). Delivery failures are logged and swallowed.
// The activity is signed by the responding local user.
func (s *Service) FederateInvitationResponse(roomID, localUserID string, accept bool) {
	if s.deliverer == nil || s.userRepo == nil || s.renderer == nil || s.urls == nil {
		return
	}
	room, err := s.repo.FindRoomByID(roomID)
	if err != nil || room == nil {
		return
	}
	owner, err := s.userRepo.FindByID(room.OwnerID)
	if err != nil || owner == nil || owner.IsLocal() {
		// 自前の room (local owner) は federation 不要。
		return
	}
	if owner.URI == nil || *owner.URI == "" {
		return
	}
	roomURI := s.roomAPURI(room, owner)
	if roomURI == "" {
		return
	}
	inviteeURI := s.urls.UserURI(localUserID)
	inviteRef := s.renderer.RenderChatRoomInviteRef(*owner.URI, roomURI, room.Name, inviteeURI)

	var activity any
	if accept {
		activity = s.renderer.RenderAccept(localUserID, inviteRef)
	} else {
		activity = s.renderer.RenderReject(localUserID, inviteRef)
	}
	body, err := json.Marshal(activity)
	if err != nil {
		slog.Warn("chat: invitation response federation: marshal failed", "roomID", roomID, "err", err)
		return
	}
	if err := s.deliverer.DeliverToUser(localUserID, owner, body); err != nil {
		slog.Warn("chat: invitation response federation: deliver failed", "roomID", roomID, "accept", accept, "err", err)
	}
}

// apRoomIDFromURI returns the room id embedded in a chat room URI, or "".
// 形式は URI を組み立てる側 (`activitypub.URLBuilder.ChatRoomURI`) の隣にある。
func apRoomIDFromURI(uri string) string { return activitypub.ChatRoomIDFromURI(uri) }

// apURIHost returns the authority of uri, or "" when it has none.
//
// **正規形の URI を渡すこと** (`idnhost.CanonicalURI`)。`chat_room.host` は
// `user.host` / `emoji.host` と突き合わせうる値なので、punycode と既定ポートが
// 畳まれていないと同じサーバーが別物として並ぶ。
func apURIHost(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// roomAPURI returns the AP URI to put on activities about room.
//
// **取り込んだ room は保存済みの URI をそのまま使う (#2994)。** 行の `id` は
// こちらで採番するので、owner の URI から `id` で組み立て直す旧来の導出は
// **別の room を指す**。その導出は削除した。
//
// **`uri` を持たない room はローカルのものだけ** — #2994 の migration が取り込み
// 済みの room をすべて埋め、`rooms/transfer-ownership` はローカル利用者にしか
// 譲れない。それでも owner がリモートな行を見たら身元が分からないので**名乗らない** —
// ローカルの正規 URI を返すと「その room はうちのもの」と主張することになる。
func (s *Service) roomAPURI(room *model.ChatRoom, owner *model.User) string {
	if room == nil {
		return ""
	}
	if room.URI != nil && *room.URI != "" {
		return *room.URI
	}
	if (room.Host != nil && *room.Host != "") || (owner != nil && !owner.IsLocal()) {
		return ""
	}
	if s.urls == nil {
		return ""
	}
	return s.urls.ChatRoomURI(room.ID)
}

// remoteChatText converts an inbound ActivityPub `content` payload into the MFM
// text mk-go stores, then clips it to `chat_message.text`.
//
// **連合で届く chat の本文は HTML。** upstream の note 取り込み
// (`ApNoteService`) も mk-go の `IngestNote` (`internal/core/federation/resolver.go`)
// も `content` を `mfm.FromHTML` で MFM に戻してから保存しており、chat だけが
// 生のまま保存していた。frontend は本文を MFM として描画するので、HTML のまま
// だと `<` や `&` が `&lt;` / `&amp;` とリテラル表示され、`<p>` などのタグは
// 本文として残る。mk-go 同士でも往復で壊れる。
//
// **変換してから切る。** 先に切ると tag の途中で切れた壊れた HTML を parser に
// 渡すことになる (`extractRemoteDescription` と同じ順序)。`colfit.Text` は
// NUL も落とす。
//
// **note 取り込みと同じ 3 段** (source → _misskey_content → content) で拾う。
// 前 2 つは `internal/core/federation/processor.go` の probe が読み取って
// `mfmSource` として渡してくるので、この層では「原文があるか」だけを見る。
func remoteChatText(content, mfmSource string) string {
	// **原文があれば HTML から戻さない。** `mfm.Parse` -> `ToHTML` -> `FromHTML`
	// は往復で情報が落ちる (装飾 `$[shake x]` / 色 / `<center>` / 引用の改行 /
	// 数式が失われ、絵文字の前後にゼロ幅文字が入る)。note 取り込みと同じ扱い。
	if mfmSource != "" {
		return colfit.Text(remoteNoNUL(mfmSource), chatMessageTextMaxRunes)
	}
	return colfit.Text(mfm.FromHTML(content), chatMessageTextMaxRunes)
}

// remoteNoNUL drops NUL bytes the way mfm.FromHTML does for the content path.
//
// `source` / `_misskey_content` は FromHTML を通らないので、ここで落とさないと
// PostgreSQL の text 列が 22021 で落ちる (note 側の `remoteText` と同じ判断)。
func remoteNoNUL(s string) string {
	return strings.ReplaceAll(s, "\x00", "")
}

// CreateMessageViaAP persists a chat message received via ActivityPub from a
// remote user. The uri parameter is the activity's canonical ID. AP retries
// are common so URI-based dedup is performed (IngestNote と同じパターン).
//
// recipient の chatScope を必ず強制する (#692)。連合受信時は AP retry でこの
// path が複数回踏まれるが、scope 違反は最初の試行で拒絶し、retry でも同じ
// 結果になるので peer 側は dead letter で処理する。
func (s *Service) CreateMessageViaAP(ctx context.Context, uri string, fromUser *model.User, toUserID, text, mfmSource string) (*model.ChatMessage, error) {
	if fromUser == nil || toUserID == "" {
		return nil, ErrInvalidTarget
	}
	// uri は dedup の鍵なので、収まらなければ message ごと拒否する
	// (CreateRoomMessageViaAP と同じ判断、#2726)。
	if !colfit.Fits(uri, chatMessageURIMaxRunes) {
		return nil, ErrInvalidTarget
	}
	// AP retry による重複メッセージ作成を防ぐ
	if uri != "" {
		if existing, err := s.repo.FindMessageByURI(uri); err == nil {
			return existing, nil
		}
	}
	// inbound でも chatApproval bypass を適用する: local recipient が過去に remote
	// sender へ送っていれば (otherApprovedMe) scope チェックを skip する (#1748)。
	otherApprovedMe := false
	if s.approvalRepo != nil {
		if ok, err := s.approvalRepo.Exists(toUserID, fromUser.ID); err == nil {
			otherApprovedMe = ok
		}
	}
	if !otherApprovedMe && s.userRepo != nil {
		if recipient, err := s.userRepo.FindByID(toUserID); err == nil && recipient.IsLocal() {
			if err := s.canChat(fromUser, recipient); err != nil {
				return nil, err
			}
		}
	}
	// inbound (連合) DM でも、recipient が sender を block していれば拒否する。
	// CreateMessageToUser と同じ checkBlocked(toUser, fromUser) gate を AP 経路
	// にも適用する (#parity review chat-block-1)。
	if s.blockingRepo != nil {
		blocked, err := s.blockingRepo.Exists(toUserID, fromUser.ID)
		if err != nil {
			return nil, fmt.Errorf("chat block check: %w", err)
		}
		if blocked {
			return nil, ErrChatBlocked
		}
	}
	msg := &model.ChatMessage{
		ID:         s.idGen.Generate(time.Now()),
		FromUserID: fromUser.ID,
		ToUserID:   &toUserID,
	}
	// text は HTML で届くので MFM に戻してから切る (CreateRoomMessageViaAP と同じ)。
	if text = remoteChatText(text, mfmSource); text != "" {
		msg.Text = &text
	}
	if uri != "" {
		msg.URI = &uri
	}
	if err := s.repo.CreateMessage(msg); err != nil {
		return nil, fmt.Errorf("create AP message: %w", err)
	}
	// inbound 受信後、remote sender -> local recipient の approval を記録する
	// (= 以降 local recipient は scope に依らず remote sender へ返信できる、#1748)。
	s.recordChatApproval(fromUser.ID, toUserID)
	if s.publisher != nil {
		s.publisher.PublishUserMessage(ctx, fromUser.ID, toUserID, EventMessage, packMessage(msg))
	}
	// Remote → local DM を受信した場合も、CreateMessageToUser と同じく
	// recipient の main チャネルに newChatMessage を emit してフロントの
	// 未読インジケータを更新させる。sender は remote なので emit 対象外。
	if s.mainStreamPublisher != nil {
		packed := s.packMessageStream(msg)
		s.mainStreamPublisher.PublishMainEvent(toUserID, "newChatMessage", packed)
		// **引数の fromUser をそのまま使う。** remote sender は user 表に
		// 居ないこともあるので、引き直すと sender 未解決で push を落とす。
		s.pushNewChatMessage(toUserID, packed, fromUser, nil)
	}
	return msg, nil
}

// CreateMessageToRoom persists a room-message row and fires the `message`
// event on the room topic so every subscribed member receives it.
func (s *Service) CreateMessageToRoom(ctx context.Context, fromUserID, roomID, text, fileID string) (*model.ChatMessage, error) {
	if fromUserID == "" || roomID == "" {
		return nil, ErrInvalidTarget
	}
	// ルームが存在し、送信者がメンバーであることを確認する。
	// 取得した room は後段の emitRoomNewChatMessage にも渡し、重複 fetch を避ける。
	room, err := s.repo.FindRoomByID(roomID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2792)。handler の mapChatErr は
		// ErrNotFound を 400 にするので、ここで潰すと接続断が
		// 「そんなメッセージは無い」として返る。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrNotFound
	}
	isMember, err := s.isRoomMemberWith(room, fromUserID, roomID)
	if err != nil {
		return nil, err
	}
	if !isMember {
		return nil, ErrForbidden
	}
	msg := &model.ChatMessage{
		ID:         s.idGen.Generate(time.Now()),
		FromUserID: fromUserID,
		ToRoomID:   &roomID,
	}
	// upstream は text.trim() を保存する (ChatService.createMessageToRoom)。
	text = strings.TrimSpace(text)
	if text != "" {
		msg.Text = &text
	}
	if fileID != "" {
		msg.FileID = &fileID
	}
	if err := s.repo.CreateMessage(msg); err != nil {
		return nil, fmt.Errorf("create room message: %w", err)
	}
	if s.publisher != nil {
		s.publisher.PublishRoomMessage(ctx, roomID, EventMessage, s.packMessageStream(msg))
	}
	// Room 宛も同様に、sender と **mute した member** を除く全 member
	// (owner 含む) の main に newChatMessage を emit する。Misskey 本家
	// ChatService.createMessageToRoom と同じ fan-out 方針。
	s.emitRoomNewChatMessage(room, fromUserID, msg, nil)
	// remote member が居れば group Create を AP 配送する (best-effort)。
	s.tryDeliverRoomMessage(msg, room, fromUserID)
	return msg, nil
}

// tryDeliverRoomMessage federates a room message as a CherryPick group chat
// Create to every remote member of the room. The note lists all members in
// `to` and carries the room URI as its `@context`. Best-effort: no-op when AP
// delivery is unwired, the sender is remote, or the room has no remote members.
// Per-recipient failures are logged and swallowed.
func (s *Service) tryDeliverRoomMessage(msg *model.ChatMessage, room *model.ChatRoom, fromUserID string) {
	if s.deliverer == nil || s.userRepo == nil || s.renderer == nil || s.urls == nil || room == nil {
		return
	}
	sender, err := s.userRepo.FindByID(fromUserID)
	if err != nil || sender == nil || !sender.IsLocal() {
		// remote sender が起点のメッセージは本インスタンスからは配送しない。
		return
	}
	// member 集合 = owner (暗黙メンバー) + membership 行。重複は set で除外する。
	memberIDs := map[string]struct{}{room.OwnerID: {}}
	if members, merr := s.repo.ListMembersByRoom(room.ID); merr == nil {
		for _, m := range members {
			memberIDs[m.UserID] = struct{}{}
		}
	}
	memberURIs := make([]string, 0, len(memberIDs))
	remoteRecipients := make([]*model.User, 0)
	var owner *model.User
	for uid := range memberIDs {
		u, uerr := s.userRepo.FindByID(uid)
		if uerr != nil || u == nil {
			continue
		}
		if uid == room.OwnerID {
			owner = u
		}
		if u.IsLocal() {
			memberURIs = append(memberURIs, s.urls.UserURI(u.ID))
		} else if u.URI != nil && *u.URI != "" {
			memberURIs = append(memberURIs, *u.URI)
			remoteRecipients = append(remoteRecipients, u)
		}
	}
	if len(remoteRecipients) == 0 {
		return
	}

	// room URI: local 所有なら local の正規 URI、取り込んだ copy なら保存済みの
	// URI。owner は member ループで既に解決済みなので使い回す。
	//
	// **空なら配送しない。** `@context` は `omitempty` なので、空のまま送ると
	// room の印が落ちて**受信側が 1-on-1 DM として取り込む** (`to` の先頭 1 人宛の
	// 私信になる)。届かないほうがまだ良い。
	roomURI := s.roomAPURI(room, owner)
	if roomURI == "" {
		slog.Warn("chat: room message federation: room uri is unknown", "roomID", room.ID)
		return
	}

	published := time.Now().UTC().Format(time.RFC3339)
	if t, terr := s.idGen.ParseTime(msg.ID); terr == nil {
		published = t.UTC().Format(time.RFC3339)
	}
	body, err := json.Marshal(s.renderer.RenderChatRoomMessage(msg, s.urls.UserURI(sender.ID), memberURIs, roomURI, published))
	if err != nil {
		slog.Warn("chat: room message federation: marshal failed", "msgID", msg.ID, "err", err)
		return
	}
	for _, recipient := range remoteRecipients {
		if derr := s.deliverer.DeliverToUser(sender.ID, recipient, body); derr != nil {
			slog.Warn("chat: room message federation: deliver failed", "msgID", msg.ID, "recipient", recipient.ID, "err", derr)
		}
	}
}

// LeaveRoom removes the user's membership and, when the leaving user is local,
// federates a Remove activity to the room's remote members (and remote owner)
// so their membership lists stay in sync. Mirrors cherrypick
// ChatService.leaveRoom. Membership deletion is best-effort-independent of
// federation: the row is always removed.
func (s *Service) LeaveRoom(_ context.Context, userID, roomID string) error {
	if userID == "" || roomID == "" {
		return ErrInvalidTarget
	}
	// room を先に引いて federation の recipient/roomURI 構築に使う。room が無くても
	// membership 削除は冪等に試みる (drift cleanup)。
	room, _ := s.repo.FindRoomByID(roomID)
	// **メンバーでないなら何もしない。** 検査が無いと、任意の利用者が room ID を
	// 指定するだけで、その room の remote メンバー全員へ自分の署名付き Remove を
	// 配送させられる (受信側は冪等 no-op なので実害は配送量だが、増幅になる)。
	// upstream の leaveRoom も membership を findOneByOrFail で要求する
	// (owner は membership 行を持たないので leave ではなく room 削除を使う)。
	if _, err := s.repo.FindMembership(userID, roomID); err != nil {
		if !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return err
		}
		return nil
	}
	if err := s.repo.DeleteMembership(userID, roomID); err != nil {
		return err
	}
	if room != nil {
		s.tryDeliverRoomLeave(room, userID)
	}
	return nil
}

// tryDeliverRoomLeave federates a chat room leave as a Remove activity to the
// room's remote members and remote owner. Best-effort: no-op when AP delivery
// is unwired, the leaving user is remote, or there are no remote recipients.
// Per-recipient failures are logged and swallowed.
func (s *Service) tryDeliverRoomLeave(room *model.ChatRoom, leavingUserID string) {
	if s.deliverer == nil || s.userRepo == nil || s.renderer == nil || s.urls == nil || room == nil {
		return
	}
	leaver, err := s.userRepo.FindByID(leavingUserID)
	if err != nil || leaver == nil || !leaver.IsLocal() {
		// remote user の leave は当人の instance から配送される (我々は受信側)。
		return
	}
	// recipient = owner + 残り membership のうち remote な者 (leaving user は除外)。
	memberIDs := map[string]struct{}{room.OwnerID: {}}
	if members, merr := s.repo.ListMembersByRoom(room.ID); merr == nil {
		for _, m := range members {
			memberIDs[m.UserID] = struct{}{}
		}
	}
	delete(memberIDs, leavingUserID)
	remoteRecipients := make([]*model.User, 0)
	var owner *model.User
	for uid := range memberIDs {
		u, uerr := s.userRepo.FindByID(uid)
		if uerr != nil || u == nil {
			continue
		}
		if uid == room.OwnerID {
			owner = u
		}
		if !u.IsLocal() && u.URI != nil && *u.URI != "" {
			remoteRecipients = append(remoteRecipients, u)
		}
	}
	if len(remoteRecipients) == 0 {
		return
	}
	// room URI: local 所有なら local 正規 URI、取り込んだ copy なら保存済みの URI。
	// **空なら配送しない** (どの room の Remove か伝わらない)。
	roomURI := s.roomAPURI(room, owner)
	if roomURI == "" {
		slog.Warn("chat: room leave federation: room uri is unknown", "roomID", room.ID)
		return
	}
	body, err := json.Marshal(s.renderer.RenderChatRoomRemove(s.urls.UserURI(leaver.ID), roomURI))
	if err != nil {
		slog.Warn("chat: room leave federation: marshal failed", "roomID", room.ID, "err", err)
		return
	}
	for _, recipient := range remoteRecipients {
		if derr := s.deliverer.DeliverToUser(leaver.ID, recipient, body); derr != nil {
			slog.Warn("chat: room leave federation: deliver failed", "roomID", room.ID, "recipient", recipient.ID, "err", derr)
		}
	}
}

// RemoveMemberViaAP removes a (typically remote) user's membership in response
// to an inbound Remove activity targeting the chat room. Idempotent: a no-op
// when the membership does not exist. Mirrors cherrypick ApInboxService.remove
// (deletes the membership of the activity actor).
func (s *Service) RemoveMemberViaAP(roomURI, userID string) error {
	if roomURI == "" || userID == "" {
		return ErrInvalidTarget
	}
	room, rerr := s.resolveRoomByAPURI(roomURI)
	if rerr != nil {
		return rerr
	}
	return s.repo.DeleteMembership(userID, room.ID)
}

// CreateRoomMessageViaAP persists a group chat (room) message received via
// ActivityPub and streams it to local members. The sender must be a member of
// the room (owner or membership) — this rejects messages injected by a remote
// actor that is not part of the room. URI-based dedup handles AP retries.
// Returns ErrNotFound when the room is unknown locally and ErrForbidden when
// the sender is not a member, both of which the caller treats as permanent.
func (s *Service) CreateRoomMessageViaAP(uri string, sender *model.User, roomURI, text, mfmSource string) error {
	if sender == nil || roomURI == "" {
		return ErrInvalidTarget
	}
	// uri は dedup の鍵。捨てて行だけ作ると **AP retry のたびに同じ message が
	// 増える**ので、収まらなければ message ごと拒否する (note.uri と同じ判断)。
	// ErrInvalidTarget は呼び出し側で non-retry に落ちる (#2726)。
	if !colfit.Fits(uri, chatMessageURIMaxRunes) {
		return ErrInvalidTarget
	}
	if uri != "" {
		if existing, err := s.repo.FindMessageByURI(uri); err == nil && existing != nil {
			return nil
		}
	}
	room, err := s.resolveRoomByAPURI(roomURI)
	if err != nil {
		return err
	}
	roomID := room.ID
	isMember, err := s.isRoomMemberWith(room, sender.ID, roomID)
	if err != nil {
		return err
	}
	if !isMember {
		return ErrForbidden
	}
	msg := &model.ChatMessage{
		ID:         s.idGen.Generate(time.Now()),
		FromUserID: sender.ID,
		ToRoomID:   &roomID,
	}
	// text は HTML で届くので MFM に戻してから切る (CreateMessageViaAP と同じ)。
	// 空になったら列を NULL のままにする (生値が空だったときと同じ形にする)。
	if text = remoteChatText(text, mfmSource); text != "" {
		msg.Text = &text
	}
	if uri != "" {
		msg.URI = &uri
	}
	if err := s.repo.CreateMessage(msg); err != nil {
		return fmt.Errorf("create AP room message: %w", err)
	}
	if s.publisher != nil {
		s.publisher.PublishRoomMessage(context.Background(), roomID, EventMessage, s.packMessageStream(msg))
	}
	// sender は remote なので emitRoomNewChatMessage で local member の main に
	// newChatMessage を流す (sender 自身は対象外、未読インジケータ更新)。
	s.emitRoomNewChatMessage(room, sender.ID, msg, sender)
	return nil
}

// isRoomMemberWith reports whether userID is a member of the given room,
// reusing an already-fetched room object to avoid a second FindRoomByID.
// The owner is treated as an implicit member even without a membership row.
func (s *Service) isRoomMemberWith(room *model.ChatRoom, userID, roomID string) (bool, error) {
	if room != nil && room.OwnerID == userID {
		return true, nil
	}
	if _, err := s.repo.FindMembership(userID, roomID); err == nil {
		return true, nil
	}
	return false, nil
}

// emitRoomNewChatMessage fans out `newChatMessage` to every room member
// except the sender and those who muted the room. Room membership 列挙失敗時は警告ログを残して emit を
// スキップする (message 自体の作成は成功として返される前に呼ばれる)。
// sender は分かっていれば渡す (nil なら fromUserID で引き直す)。**remote sender は
// user 表に居ないことがある**ので、AP 経路は必ず渡すこと。
func (s *Service) emitRoomNewChatMessage(room *model.ChatRoom, fromUserID string, msg *model.ChatMessage, sender *model.User) {
	if s.mainStreamPublisher == nil || room == nil {
		return
	}
	members, err := s.repo.ListMembersByRoom(room.ID)
	if err != nil {
		slog.Warn("chat: ListMembersByRoom failed, skipping newChatMessage emit", "roomID", room.ID, "err", err)
		return
	}
	packed := s.packMessageStream(msg)
	// push が無効な構成では引かない (1 メッセージあたり FindByID が 1 回増える)。
	if sender == nil && s.chatPusher != nil {
		sender = s.senderForPush(fromUserID)
	}
	// 重複 emit を防ぐため set 相当で ID を管理する (membership + owner)。
	emitted := make(map[string]bool, len(members)+1)
	// **owner は never-muted として扱う** (mute 判定より前に seed する)。
	// upstream ChatService.ts は owner を `concat({isMuted: false})` で足すし、
	// mk-go 自身も packRoomDetailed が owner に `isMuted: false` を固定で返し、
	// フロントは owner にミュートのスイッチを出さない
	// (`room.info.vue` の `v-if="!isOwner"`)。
	//
	// **owner は membership 行を持たない**ので、そもそも読む先が無い
	// (transfer-ownership も #2858 で行を入れ替えるようにした)。不整合データで
	// 行が残っていても、ここだけ mute を尊重すると **API もフロントも
	// 「ミュートしていない」と表示するのに通知だけ来ない**状態になり、
	// 解除する手段が UI に無いので利用者は原因に辿り着けない。
	if room.OwnerID != fromUserID {
		emitted[room.OwnerID] = true
	}
	for _, m := range members {
		if emitted[m.UserID] {
			continue
		}
		// **mute した member は除外する。** upstream ChatService.ts は
		// `if (membership.isMuted) continue;` で marker を張らず、publish も
		// push も行かない。mk-go は publish 側でこれを見ていなかったが、
		// Web Push を足すと**利用者が明示的に切った設定が OS 通知として
		// 破られる**ので、ここで揃える (#2840)。
		if m.UserID == fromUserID || m.IsMuted {
			continue
		}
		emitted[m.UserID] = true
	}
	for uid := range emitted {
		s.mainStreamPublisher.PublishMainEvent(uid, "newChatMessage", packed)
		s.pushNewChatMessage(uid, packed, sender, room)
	}
}

// DeleteMessage removes a message and fans out a `deleted` event on the
// appropriate topic (user DM or room). Only the author may delete.
func (s *Service) DeleteMessage(ctx context.Context, userID, messageID string) error {
	msg, err := s.repo.FindMessageByID(messageID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2792)。handler の mapChatErr は
		// ErrNotFound を 400 にするので、ここで潰すと接続断が
		// 「そんなメッセージは無い」として返る。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrNotFound
	}
	if msg.FromUserID != userID {
		return ErrForbidden
	}
	if err := s.repo.DeleteMessage(messageID); err != nil {
		return fmt.Errorf("delete message: %w", err)
	}
	if s.publisher != nil {
		body := map[string]any{"id": messageID}
		if msg.ToRoomID != nil {
			s.publisher.PublishRoomMessage(ctx, *msg.ToRoomID, EventDeleted, body)
		} else if msg.ToUserID != nil {
			s.publisher.PublishUserMessage(ctx, msg.FromUserID, *msg.ToUserID, EventDeleted, body)
		}
	}
	return nil
}

// UpdateMessage edits a message text and fires `edited`. Author-only.
func (s *Service) UpdateMessage(ctx context.Context, userID, messageID, text string) (*model.ChatMessage, error) {
	msg, err := s.repo.FindMessageByID(messageID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2792)。handler の mapChatErr は
		// ErrNotFound を 400 にするので、ここで潰すと接続断が
		// 「そんなメッセージは無い」として返る。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrNotFound
	}
	if msg.FromUserID != userID {
		return nil, ErrForbidden
	}
	msg.Text = &text
	if err := s.repo.UpdateMessage(msg); err != nil {
		return nil, fmt.Errorf("update message: %w", err)
	}
	if s.publisher != nil {
		body := s.packMessageStream(msg)
		if msg.ToRoomID != nil {
			s.publisher.PublishRoomMessage(ctx, *msg.ToRoomID, EventEdited, body)
		} else if msg.ToUserID != nil {
			s.publisher.PublishUserMessage(ctx, msg.FromUserID, *msg.ToUserID, EventEdited, body)
		}
	}
	return msg, nil
}

// MarkReadByMessageID records that userID has read messageID. Misskey 本家は
// 既読を redis cache のみで持ち配信しないが、マルチ端末/タブ同期のため軽微な
// 拡張として `read` イベントを発行する。
func (s *Service) MarkReadByMessageID(ctx context.Context, userID, messageID string) error {
	msg, err := s.repo.FindMessageByID(messageID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2792)。handler の mapChatErr は
		// ErrNotFound を 400 にするので、ここで潰すと接続断が
		// 「そんなメッセージは無い」として返る。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrNotFound
	}
	// **当事者でなければ既読にしない。** 検査が無いと、非参加者が他人の
	// `reads` を汚したうえ、下の publish で相手の topic に read イベントを
	// 注入できる。購読側 (ChatUserChannel / ChatRoomChannel) は閉じているのに、
	// publish 側だけが開いている状態だった。
	//
	// upstream にこの endpoint は無い (readUserChatMessage /
	// readRoomChatMessage / readAllChatMessages しか無く、per-message read は
	// mk-go 独自) ので、判定は他の chat 経路と同じ形に揃える。
	//
	// **応答は not-found に倒す** (ErrForbidden = ACCESS_DENIED ではなく)。
	// 区別すると messageId を総当たりして「その id が存在するか」を引き出せる。
	// messages/show も同じ判断にしてある。
	if msg.ToRoomID != nil {
		member, merr := s.IsRoomMember(userID, *msg.ToRoomID)
		if merr != nil {
			return merr
		}
		if !member {
			return ErrNotFound
		}
	} else if msg.FromUserID != userID && (msg.ToUserID == nil || *msg.ToUserID != userID) {
		return ErrNotFound
	}
	if err := s.repo.MarkRead(userID, messageID); err != nil {
		return fmt.Errorf("mark read: %w", err)
	}
	if s.publisher != nil {
		body := map[string]any{"id": messageID, "userId": userID}
		if msg.ToRoomID != nil {
			s.publisher.PublishRoomMessage(ctx, *msg.ToRoomID, EventRead, body)
		} else if msg.ToUserID != nil {
			// DM の場合、両端末に既読状態を通知するため双方向へ送る。
			s.publisher.PublishUserMessage(ctx, msg.FromUserID, *msg.ToUserID, EventRead, body)
		}
	}
	return nil
}

// React adds a reaction to a chat message and publishes a `react` stream event
// to the room (or both DM peers) so clients update reactions live (#1549,
// upstream ChatService.react)。reaction は reactor.ID+"/"+emoji で永続化し、
// stream event には raw emoji + reactor (UserLite) を載せる。
func (s *Service) React(ctx context.Context, messageID string, reactor *model.User, emoji string) error {
	// upstream ChatService.react は emoji の正規化/存在検証を最初に行い、未存在の
	// custom emoji は 'no such emoji' を投げる (message 検索より前)。
	reaction, err := s.normalizeChatReaction(emoji)
	if err != nil {
		return err
	}
	msg, err := s.findReactableMessage(messageID, reactor.ID, false)
	if err != nil {
		return err
	}
	// reaction 上限 (upstream MAX_REACTIONS_PER_MESSAGE)。
	// **アクセス検査より後に見る。** 先に見ると、参加していない利用者でも
	// 上限に達したメッセージだけ別のエラーになり、存在が分かってしまう。
	if len(msg.Reactions) >= maxReactionsPerMessage {
		return ErrTooManyReactions
	}
	if err := s.repo.AddReaction(messageID, reactor.ID+"/"+reaction); err != nil {
		return fmt.Errorf("add reaction: %w", err)
	}
	s.publishReaction(ctx, msg, EventReact, reactor, reaction)
	return nil
}

// findReactableMessage loads the message and checks that userID may react to
// (or unreact from) it. Every access failure, including a missing message, is
// reported as ErrMessageAccess; database failures are returned unchanged.
func (s *Service) findReactableMessage(messageID, userID string, unreact bool) (*model.ChatMessage, error) {
	msg, err := s.repo.FindMessageByID(messageID)
	if err != nil {
		// **DB 障害をアクセスエラーに丸めない** (#2792)。handler はアクセス
		// エラーを 400 にするので、ここで潰すと接続断が「そんなメッセージは
		// 無い」として返る。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrMessageAccess
	}
	// 自分のメッセージには react できない。unreact は本家どおり見ない (DM の
	// 参加者なら、送った側も自分のリアクションを外せる)。
	if !unreact && msg.FromUserID == userID {
		return nil, ErrMessageAccess
	}
	if msg.ToRoomID != nil && *msg.ToRoomID != "" {
		// room メッセージは member でなければ触れない。
		room, err := s.repo.FindRoomByID(*msg.ToRoomID)
		if err != nil {
			// **DB 障害を not-found に丸めない** (#2792)。
			if !repository.IsNotFound(err) {
				return nil, err
			}
			return nil, ErrMessageAccess
		}
		isMember, err := s.isRoomMemberWith(room, userID, *msg.ToRoomID)
		if err != nil {
			return nil, err
		}
		if !isMember {
			return nil, ErrMessageAccess
		}
		return msg, nil
	}
	// 1-on-1 の react は受信者だけ (upstream: toRoomId === null && toUserId !== userId)、
	// unreact は送った側と受け取った側の両方 (upstream: fromUserId !== userId &&
	// toUserId !== userId で拒否)。
	isRecipient := msg.ToUserID != nil && *msg.ToUserID == userID
	if unreact && (isRecipient || msg.FromUserID == userID) {
		return msg, nil
	}
	if !isRecipient {
		return nil, ErrMessageAccess
	}
	return msg, nil
}

// normalizeChatReaction validates and canonicalises a chat reaction string,
// mirroring upstream ChatService.react: a `:name:` custom-emoji reaction is
// looked up in the local emoji table (ErrNoSuchEmoji if absent) and stored as
// `:name:`, while anything else is treated as a unicode emoji with its
// variation selector (U+FE0F) stripped. emojiRepo 未配線時は custom emoji を
// 検証せず `:name:` をそのまま採用する (legacy/test path)。
func (s *Service) normalizeChatReaction(raw string) (string, error) {
	if m := chatCustomEmojiPattern.FindStringSubmatch(raw); m != nil {
		name := m[1]
		if s.emojiRepo != nil {
			if _, err := s.emojiRepo.FindByNameAndHost(name, nil); err != nil {
				// **DB 障害を not-found に丸めない** (#2799)。
				if !repository.IsNotFound(err) {
					return "", err
				}
				return "", ErrNoSuchEmoji
			}
		}
		return ":" + name + ":", nil
	}
	// variation selector (U+FE0F) を strip して upstream の canonical form に揃える。
	return strings.ReplaceAll(raw, "\ufe0f", ""), nil
}

// normalizeChatReactionForm canonicalises a chat reaction string WITHOUT validating
// custom emoji existence: a `:name:` custom-emoji reaction is kept as `:name:`,
// anything else is treated as a unicode emoji with its variation selector (U+FE0F)
// stripped. normalizeChatReaction (react、存在検証あり) と Unreact (#2106 N4、検証不要)
// で同じ canonical form を使い、array_append と array_remove を一致させる。
func normalizeChatReactionForm(raw string) string {
	if m := chatCustomEmojiPattern.FindStringSubmatch(raw); m != nil {
		return ":" + m[1] + ":"
	}
	return strings.ReplaceAll(raw, "️", "")
}

// Unreact removes a reaction and publishes an `unreact` stream event (#1549)。
func (s *Service) Unreact(ctx context.Context, messageID string, reactor *model.User, emoji string) error {
	// #2106 N4: React と同じく emoji を canonical form に正規化してから array_remove する。
	// React は VS strip / custom `:name:` 化して保存するため、raw emoji (VS 付き等) のままだと
	// 厳密一致 array_remove で取り消せず silent fail する。custom emoji の存在検証は unreact
	// では不要なので form だけ揃える (upstream ChatService.unreact も同様)。
	reaction := normalizeChatReactionForm(emoji)
	// **消す前に、upstream 2026.10.0 の unreact と同じアクセス検査を通す。**
	// 失敗はすべて ErrMessageAccess にまとめる (存在しない id と同じ応答にする)。
	msg, err := s.findReactableMessage(messageID, reactor.ID, true)
	if err != nil {
		return err
	}
	removed, err := s.repo.RemoveReaction(messageID, reactor.ID+"/"+reaction)
	if err != nil {
		return fmt.Errorf("remove reaction: %w", err)
	}
	// **実際に消えたときだけ publish する (#3037)。** 参加者でも、付けて
	// いないリアクションを指定されたときにイベントを流さない。upstream
	// (`ChatService.unreact`) にも同じ趣旨の TODO がある。
	if removed {
		// #2106 N4: stream event も正規化済 reaction で publish し React と対称にする。
		s.publishReaction(ctx, msg, EventUnreact, reactor, reaction)
	}
	return nil
}

func (s *Service) publishReaction(ctx context.Context, msg *model.ChatMessage, eventType string, reactor *model.User, emoji string) {
	if s.publisher == nil {
		return
	}
	body := map[string]any{
		"messageId": msg.ID,
		"reaction":  emoji,
		"user":      entity.PackUserLite(reactor),
	}
	switch {
	case msg.ToRoomID != nil && *msg.ToRoomID != "":
		s.publisher.PublishRoomMessage(ctx, *msg.ToRoomID, eventType, body)
	case msg.ToUserID != nil && *msg.ToUserID != "":
		s.publisher.PublishUserMessage(ctx, msg.FromUserID, *msg.ToUserID, eventType, body)
	}
}

// ReadUserChat marks the whole DM conversation with otherID as read for readerID
// (#1549, upstream readUserChatMessage)。stream channel の `read` (id 無し) が
// 会話全体既読にするために使う。
func (s *Service) ReadUserChat(_ context.Context, readerID, otherID string) error {
	return s.repo.MarkAllReadFromUser(readerID, otherID)
}

// ReadRoomChat marks the whole room conversation as read for readerID (#1549,
// upstream readRoomChatMessage)。
func (s *Service) ReadRoomChat(_ context.Context, readerID, roomID string) error {
	return s.repo.MarkAllReadInRoom(readerID, roomID)
}

// --- Queries used by the WebSocket channel for membership / auth checks ---

// IsRoomMember reports whether userID is a member of roomID. The room owner
// is implicitly a member even if no explicit chat_room_membership row exists.
func (s *Service) IsRoomMember(userID, roomID string) (bool, error) {
	room, err := s.repo.FindRoomByID(roomID)
	if err == nil && room.OwnerID == userID {
		return true, nil
	}
	if _, err := s.repo.FindMembership(userID, roomID); err == nil {
		return true, nil
	}
	return false, nil
}

// CanViewRoomTimeline reports whether userID may subscribe to the room's timeline
// stream: a room member (owner/member) or a moderator (#2106 L57, mirrors upstream
// hasPermissionToViewRoomTimeline = isRoomMember || isModerator)。
func (s *Service) CanViewRoomTimeline(userID, roomID string) (bool, error) {
	member, err := s.IsRoomMember(userID, roomID)
	if err != nil {
		return false, err
	}
	if member {
		return true, nil
	}
	if s.moderatorChecker != nil && s.moderatorChecker.IsModerator(userID) {
		return true, nil
	}
	return false, nil
}

// packMessage produces a JSON-serializable projection of a ChatMessage for
// outbound WebSocket events. フロント互換のため本家 ChatMessageLite と同じ
// 主要フィールドを含める。createdAt は idGen の初期化未完了時 (テスト等)
// に欠落することがあるが、上位の Service.packMessage 経由なら必ず付く (#692)。
func packMessage(msg *model.ChatMessage) map[string]any {
	out := map[string]any{
		"id":         msg.ID,
		"fromUserId": msg.FromUserID,
	}
	if msg.Text != nil {
		out["text"] = *msg.Text
	}
	if msg.ToUserID != nil {
		out["toUserId"] = *msg.ToUserID
	}
	if msg.ToRoomID != nil {
		out["toRoomId"] = *msg.ToRoomID
	}
	if msg.FileID != nil {
		out["fileId"] = *msg.FileID
	}
	if msg.URI != nil {
		out["uri"] = *msg.URI
	}
	if msg.Reads != nil {
		out["reads"] = []string(msg.Reads)
	}
	// misskey_dart の ChatMessage.fromJson は reactions を ChatMessageReaction
	// (Map) の非null配列として cast するため、"<userId>/<reaction>" 文字列配列を
	// {reaction} object 配列に変換し、空でも [] を出す (#1246 streaming 版)。
	out["reactions"] = packStreamReactions(msg.Reactions)
	return out
}

// packStreamReactions converts stored "<userId>/<reaction>" reaction strings
// into the misskey_dart ChatMessageReaction object shape (`[{reaction}]`).
// api/chat.packChatReactions の streaming 版 (layering 上 api を import できない
// ため重複)。
func packStreamReactions(reactions []string) []map[string]any {
	out := make([]map[string]any, 0, len(reactions))
	for _, r := range reactions {
		reaction := r
		if i := strings.Index(r, "/"); i >= 0 {
			reaction = r[i+1:]
		}
		out = append(out, map[string]any{"reaction": reaction})
	}
	return out
}

// packMessageStream is the service-level packer that adds createdAt + fromUser
// on top of packMessage. WebSocket streaming や mainStream の `newChatMessage`
// 配信に使う。createdAt 欠落だと FE 側の MkTime が NaN/NaN を表示する (#692)。
func (s *Service) packMessageStream(msg *model.ChatMessage) map[string]any {
	out := packMessage(msg)
	if s.idGen != nil {
		if t, err := s.idGen.ParseTime(msg.ID); err == nil {
			out["createdAt"] = t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}
	if msg.FromUser != nil {
		out["fromUser"] = packUserStream(msg.FromUser)
	}
	if msg.ToUser != nil {
		out["toUser"] = packUserStream(msg.ToUser)
	}
	return out
}

// packUserStream is a UserLite-shaped projection for stream payloads.
// FE の MkAvatar / chat 一覧が avatarUrl / avatarBlurhash を読むため、
// fromUser / toUser に含める。avatarUrl 未設定時は `entity.IdenticonURL`
// 経由で identicon URL に fallback する (#692)。
func packUserStream(u *model.User) map[string]any {
	return map[string]any{
		"id":             u.ID,
		"username":       u.Username,
		"name":           u.Name,
		"host":           u.Host,
		"avatarUrl":      entity.IdenticonURL(u),
		"avatarBlurhash": u.AvatarBlurhash,
		"isBot":          u.IsBot,
		"isCat":          u.IsCat,
	}
}

// HasBlockingRepo reports whether the blocking repository was wired.
//
// 未配線だと**自分をブロックしている**相手へ DM を送れる。gate は
// `Exists(toUserID, fromUserID)` (blocker が受信者)。起動時検査に使う (#2708)。
func (s *Service) HasBlockingRepo() bool { return s.blockingRepo != nil }
