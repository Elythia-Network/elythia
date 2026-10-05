package federation

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/elythia-network/elythia/internal/activitypub"
	corechat "github.com/elythia-network/elythia/internal/core/chat"
	"github.com/elythia-network/elythia/internal/model"
)

// ChatRoomReceiver wires the chat service's room-federation operations into the
// inbox processor. Implemented by core/chat.Service. Mirrors the CherryPick
// group chat federation handshake (Invite / Accept / Reject of a Group object).
//
// **room の指定は URI で渡す (#2994)。** room id は相手が自由に決められる値で
// ID 空間はホストをまたいで共有されているので、id だけでは「どのホストの room か」
// が決まらない。core/chat 側が URI から自ホスト / 取り込み済み copy を引き分ける。
type ChatRoomReceiver interface {
	EnsureRoomViaAP(roomURI, name, summary, ownerUserID string) error
	CreateInvitationViaAP(roomURI, inviteeUserID string) error
	AddMemberViaAP(roomURI, userID string) error
	RemoveInvitationViaAP(roomURI, userID string) error
	RemoveMemberViaAP(roomURI, userID string) error
	// mfmSource は相手が併記した MFM の原文 (`source` / `_misskey_content`)。
	// 空なら text (HTML) から戻す。
	CreateRoomMessageViaAP(uri string, sender *model.User, roomURI, text, mfmSource string) error
}

// SetChatRoomReceiver wires the chat room federation receiver for inbound
// Invite / Accept / Reject of chat room Group objects (#1203).
func (p *Processor) SetChatRoomReceiver(r ChatRoomReceiver) {
	p.chatRoomReceiver = r
}

// nonRetryableChatRoomErr maps the chat service's permanent conditions onto
// ErrUnsupportedActivity so the inbox job is acked instead of retried.
//
// **room を URI で引くようになって「知らない room」が出るようになった (#2994)。**
// 以前の id 引きは room が無くても no-op で成功していたので、この経路は
// 無かった。生で返すと、取り込んでいない room への Accept / Reject が retry を
// 使い切って dead になる。
func nonRetryableChatRoomErr(err error) error {
	if errors.Is(err, corechat.ErrNotFound) || errors.Is(err, corechat.ErrInvalidTarget) {
		return ErrUnsupportedActivity
	}
	return err
}

// chatRoomURIMaxRunes は `chat_room.uri` の varchar(512)
// (migration/000090_chat_room_host.up.sql)。
//
// **room の身元は URI であって room id ではない (#2994)。** 行の `id` は取り込み時に
// こちらで採番するので相手の値は入らず、溢れうるのは URI のほう。収まらない URI は
// room として認識しない — 切ると別の room を指すので (#2726 と同じ判断)。
const chatRoomURIMaxRunes = 512

// chatRoomURI returns uri when it is a storable chat room URI, else "".
//
// 形の判定は URI を組み立てる側の隣 (`activitypub.ChatRoomIDFromURI`) にある。
//
// **`fitsColumn` は長さだけでなく NUL も見る。** 正規表現が縛るのは末尾の room id
// だけで、URI 全体には任意のバイトが混ざりうる (`https://e.example/<NUL>/chat/rooms/r1`
// は今の正規表現を通る)。長さだけの検査に「簡約」すると PostgreSQL へ NUL が渡り、
// 22021 で落ちた配送が retry を使い切る。
func chatRoomURI(uri string) string {
	if !activitypub.IsChatRoomURI(uri) || !fitsColumn(uri, chatRoomURIMaxRunes) {
		return ""
	}
	return uri
}

// apGroupObject is the AP `Group` object representing a chat room, carried as
// the object of Invite / Accept / Reject activities.
type apGroupObject struct {
	Type         string `json:"type"`
	ID           string `json:"id"`
	Name         string `json:"name"`
	Summary      string `json:"summary"`
	AttributedTo string `json:"attributedTo"`
}

// parseGroupObject decodes a raw AP object and reports whether it is a chat
// room Group object (type=Group with a recognizable /chat/rooms/{id} id).
func parseGroupObject(raw json.RawMessage) (apGroupObject, bool) {
	var g apGroupObject
	if len(raw) == 0 || json.Unmarshal(raw, &g) != nil {
		return apGroupObject{}, false
	}
	if !strings.EqualFold(g.Type, "Group") || chatRoomURI(g.ID) == "" {
		return apGroupObject{}, false
	}
	return g, true
}

// isChatRoomInvite reports whether an Invite activity carries a chat room
// Group object (as opposed to a reversi Game object).
func (p *Processor) isChatRoomInvite(act genericActivity) bool {
	_, ok := parseGroupObject(act.Object)
	return ok
}

// handleChatRoomInvite processes an inbound Invite whose object is a chat room
// Group: it creates a local copy of the remote room (owned by the inviter) and
// records a pending invitation for the targeted local user. The local user
// must explicitly accept via the chat API; no auto-join happens here.
func (p *Processor) handleChatRoomInvite(act genericActivity) error {
	if p.chatRoomReceiver == nil {
		return ErrUnsupportedActivity
	}
	group, ok := parseGroupObject(act.Object)
	if !ok {
		return ErrUnsupportedActivity
	}
	roomURI := group.ID

	// **room の host を actor の host に縛る。** これが無いと、署名が通る任意の
	// remote actor が **自分が管理していない host の room URI を名乗れる** —
	// 具体的には (a) `https://<このインスタンス>/chat/rooms/{id}` を名乗って
	// ローカルの room id 空間に行を作る、(b) 無関係な第三者インスタンスの URI を
	// 名乗って、その instance の room として行を作る、の 2 つ。
	//
	// **id の先取りは #2994 で塞いだ。** それまでは `chat_room` が room id だけで
	// keying していたので、攻撃者が **自分の host で** 好きな id の room を作ると、
	// 同じ id を持つ正規の remote room の Invite が EnsureRoomViaAP の owner
	// mismatch で恒久的に drop されていた。今は room の身元が URI で、取り込む行の
	// `id` はこちらで採番する。
	//
	// 比較は actor が申告する値の host 検証と同じ `sameDeliveryHost`
	// (punycode + 非既定 port。`www.` は同一視しない)。inbox 側も activity.id の
	// host を actor に縛っており (#1779、queue/processors/inbox.go の
	// authorizeActor)、判断をそこに揃える。恒久的な条件なので retry させない。
	if !sameDeliveryHost(group.ID, act.Actor) {
		slog.Warn("chat room invite: group id host does not match actor host",
			"actor", act.Actor, "group", group.ID)
		return ErrUnsupportedActivity
	}

	// owner (= inviter) は activity の actor。room copy の owner として保存する。
	owner, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		// **恒久的な失敗は ack する (レビュー L1)。** 自ホストの actor URI や
		// 不正な actor document は retry しても結果が変わらない。生で返すと
		// dispatch がここを `isPermanentSkipError` に通さないので、queue が
		// 無駄に回り続ける。
		if isPermanentSkipError(err) {
			slog.Warn("chat room invite: actor is not resolvable",
				"actor", act.Actor, "err", err)
			return ErrUnsupportedActivity
		}
		return err
	}
	// local actor 名義の Invite は loopback / なりすまし。room copy の owner が
	// local user になると「作った覚えのない room」がローカル利用者の名前で生える。
	// group message 経路 (handleChatRoomMessageCreate の sender.IsLocal) と判断を
	// 揃える。host 一致だけでは通ってしまう (local actor + local room URI)。
	if owner == nil || owner.IsLocal() {
		slog.Warn("chat room invite: actor is local (loopback?)", "actor", act.Actor)
		return ErrUnsupportedActivity
	}

	// target = 招待された local user。Invite の `target` フィールドから読む。
	var env struct {
		Target string `json:"target"`
	}
	_ = json.Unmarshal(act.raw, &env)
	// target 欠落 / invitee が local でないケースは恒久的に解決しないため、
	// retryable error ではなく ErrUnsupportedActivity を返して inbox worker の
	// 無駄な再試行 (retry storm) を防ぐ (#1204 review)。
	if env.Target == "" {
		slog.Warn("chat room invite: missing target", "actor", act.Actor)
		return ErrUnsupportedActivity
	}
	invitee, err := p.resolveTargetUser(env.Target)
	if err != nil || invitee == nil || !invitee.IsLocal() {
		slog.Warn("chat room invite: invitee is not a local user", "target", env.Target)
		return ErrUnsupportedActivity
	}

	if err := p.chatRoomReceiver.EnsureRoomViaAP(roomURI, group.Name, group.Summary, owner.ID); err != nil {
		// owner mismatch (= roomId が無関係なローカル room と衝突) は永久に
		// 解決しないので retry させない。それ以外 (DB 一過性エラー等) は
		// retryable のまま伝播させる。
		if errors.Is(err, corechat.ErrRoomOwnerMismatch) {
			slog.Warn("chat room invite: room uri is already owned by someone else", "roomURI", roomURI)
			return ErrUnsupportedActivity
		}
		// 列に収まらない room id も恒久的な条件なので retry させない (#2726)。
		// parseGroupObject が先に落とすので通常はここまで来ない。
		if errors.Is(err, corechat.ErrInvalidTarget) {
			slog.Warn("chat room invite: room uri cannot be stored", "actor", act.Actor)
			return ErrUnsupportedActivity
		}
		return fmt.Errorf("chat room invite: ensure room: %w", err)
	}
	if err := p.chatRoomReceiver.CreateInvitationViaAP(roomURI, invitee.ID); err != nil {
		// block されている / room が定員 / room が消えた、はいずれも retry しても
		// 解決しない条件なので drop する。それ以外 (DB 一過性エラー等) は
		// retryable のまま伝播させる。
		if errors.Is(err, corechat.ErrChatBlocked) ||
			errors.Is(err, corechat.ErrRoomFull) ||
			errors.Is(err, corechat.ErrNotFound) ||
			errors.Is(err, corechat.ErrInvalidTarget) {
			slog.Warn("chat room invite: invitation rejected", "roomURI", roomURI, "err", err)
			return ErrUnsupportedActivity
		}
		return fmt.Errorf("chat room invite: create invitation: %w", err)
	}
	return nil
}

// handleChatRoomAccept processes an inbound Accept whose inner object is a chat
// room Invite: a remote user has accepted our room invitation, so we record
// their membership. AddMemberViaAP requires a pending invitation, so a forged
// Accept for a room the actor was never invited to is ignored.
func (p *Processor) handleChatRoomAccept(act, inner genericActivity) error {
	if p.chatRoomReceiver == nil {
		return nil
	}
	group, ok := parseGroupObject(inner.Object)
	if !ok {
		return nil
	}
	accepter, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	// **知らない room への Accept は retry しない (#2994)。** room を URI で引く
	// ようになったので、取り込んでいない room / 収まらない URI は `ErrNotFound` /
	// `ErrInvalidTarget` で返る。どちらも待っても解決しないので ack する。
	return nonRetryableChatRoomErr(p.chatRoomReceiver.AddMemberViaAP(group.ID, accepter.ID))
}

// handleChatRoomReject processes an inbound Reject of a chat room Invite: the
// remote invitee declined, so the pending invitation is removed.
func (p *Processor) handleChatRoomReject(act, inner genericActivity) error {
	if p.chatRoomReceiver == nil {
		return nil
	}
	group, ok := parseGroupObject(inner.Object)
	if !ok {
		return nil
	}
	rejecter, err := p.resolver.ResolveActor(act.Actor)
	if err != nil {
		return err
	}
	return nonRetryableChatRoomErr(p.chatRoomReceiver.RemoveInvitationViaAP(group.ID, rejecter.ID))
}

// chatRoomURIFromContext extracts the room URI from a group chat message note's
// `@context`. CherryPick group messages set the note-level `@context` to the
// room URI (a JSON string); a normal note carries the standard JSON-LD context
// (an array), which is not a room.
//
// isRoom reports whether the `@context` is a chat room URI **at all**, and is
// true even when roomURI comes back empty because the URI does not fit
// `chat_room.uri`. 呼び出し側はこれで「room だが受け取れない」と「そもそも
// room ではない (= 1-on-1 DM)」を区別する。混ぜると、保存できない URI の
// group message が DM 経路へ落ちて別の理由で dead になる (#2726)。
func chatRoomURIFromContext(raw json.RawMessage) (roomURI string, isRoom bool) {
	if len(raw) == 0 {
		return "", false
	}
	var ctx string
	if json.Unmarshal(raw, &ctx) != nil {
		// 配列形式 (標準 JSON-LD context) は room ではない。
		return "", false
	}
	if !activitypub.IsChatRoomURI(ctx) {
		return "", false
	}
	return chatRoomURI(ctx), true
}

// handleChatRoomMessageCreate persists an inbound group chat message into a
// locally-known room. The room must exist locally and the remote sender must
// be a member (enforced by the chat service): unknown room or non-member is a
// permanent condition, so it is reported as ErrUnsupportedActivity (no retry).
func (p *Processor) handleChatRoomMessageCreate(sender *model.User, noteURI, content, mfmSource, roomURI string) error {
	// roomURI が空 = `@context` は room URI だが `chat_room.uri` に収まらない
	// (chatRoomURIFromContext)。retry では解決しないので drop する (#2726)。
	if roomURI == "" {
		slog.Warn("chat room message: room uri does not fit its column", "sender", sender.ID)
		return ErrUnsupportedActivity
	}
	// note id 欠落 / sender が local (loopback) は retry しても解決しない恒久的
	// 条件なので、同関数内の room 不在・非メンバーと同じく ErrUnsupportedActivity
	// (non-retry) に揃える。
	if noteURI == "" {
		slog.Warn("chat room message: missing note id", "sender", sender.ID)
		return ErrUnsupportedActivity
	}
	if sender.IsLocal() {
		slog.Warn("chat room message: sender is local (loopback?)", "sender", sender.ID)
		return ErrUnsupportedActivity
	}
	if p.chatRoomReceiver == nil {
		return ErrUnsupportedActivity
	}
	if err := p.chatRoomReceiver.CreateRoomMessageViaAP(noteURI, sender, roomURI, content, mfmSource); err != nil {
		// 未関与の room / 非メンバー送信、および列に収まらない uri は retry しても
		// 解決しないので drop (後者は ErrInvalidTarget、#2726)。
		if errors.Is(err, corechat.ErrNotFound) ||
			errors.Is(err, corechat.ErrForbidden) ||
			errors.Is(err, corechat.ErrInvalidTarget) {
			return ErrUnsupportedActivity
		}
		return err
	}
	return nil
}
