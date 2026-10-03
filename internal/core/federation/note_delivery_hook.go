package federation

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/shiroha-a/mk/internal/activitypub"
	corenote "github.com/shiroha-a/mk/internal/core/note"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// NoteDeliveryHook implements core/note.FederationHook by rendering a Create
// activity and dispatching it through DeliverService.
//
// 配信先は visibility に応じて変える:
//   - public/home/followers: フォロワーへ
//   - specified: visibleUserIds の中のリモートユーザーへ
//   - localOnly: 何もしない
//
// pure renote (text/cw/file/poll を伴わない renote) は Create ではなく
// Announce activity として配信する。
// RelayBroadcaster is the subset of core/relay.Service used by the note
// delivery hook to fan a public Create activity out to every accepted
// relay's inbox. nil を許容 (relay 未構築時は no-op)。
type RelayBroadcaster interface {
	DeliverToAccepted(ctx context.Context, signerUserID string, activity any) error
}

type NoteDeliveryHook struct {
	deliver  *DeliverService
	renderer *activitypub.Renderer
	urls     *activitypub.URLBuilder
	idGen    id.Generator
	userRepo repository.UserRepository
	noteRepo repository.NoteRepository

	relay RelayBroadcaster
	// quotes は引用の承認 (FEP-044f) を取りに行く (#3234)。nil なら何もしない。
	quotes QuoteApprovalRequester
}

// QuoteApprovalRequester obtains FEP-044f approvals for local quotes (#3234).
type QuoteApprovalRequester interface {
	Prepare(note *model.Note, author *model.User) error
	RequestApproval(note *model.Note, author *model.User) error
}

// SetQuoteOutbox wires the quote approval flow. nil で無効 (初期状態)。
func (h *NoteDeliveryHook) SetQuoteOutbox(q QuoteApprovalRequester) {
	h.quotes = q
}

// NewNoteDeliveryHook constructs a NoteDeliveryHook.
func NewNoteDeliveryHook(
	deliver *DeliverService,
	renderer *activitypub.Renderer,
	urls *activitypub.URLBuilder,
	idGen id.Generator,
	userRepo repository.UserRepository,
	noteRepo repository.NoteRepository,
) *NoteDeliveryHook {
	return &NoteDeliveryHook{
		deliver:  deliver,
		renderer: renderer,
		urls:     urls,
		idGen:    idGen,
		userRepo: userRepo,
		noteRepo: noteRepo,
	}
}

// SetRelayBroadcaster wires the relay fan-out target. nil を渡せば
// relay 配送を無効化できる (初期状態)。
func (h *NoteDeliveryHook) SetRelayBroadcaster(r RelayBroadcaster) {
	h.relay = r
}

// OnNoteCreated is invoked by NoteCreateService once a note has been
// persisted. リモートユーザーが投稿した note は (取り込み経由で生成された場合
// 含め) リモート発信なので連合配信しない。
func (h *NoteDeliveryHook) OnNoteCreated(note *model.Note, author *model.User) {
	if note == nil || author == nil {
		return
	}
	if !author.IsLocal() {
		return
	}
	if note.LocalOnly {
		return
	}

	// 純粋なリノートは Create ではなく Announce として配信する。
	if corenote.IsPureRenote(note) {
		h.deliverAnnounce(note, author)
		return
	}

	// ローカル同士の引用の承認は Create を描画する前に発行する (最初の Create から
	// quoteAuthorization が付く)。失敗しても配送は止めない (承認が無いだけ)。
	if h.quotes != nil {
		if err := h.quotes.Prepare(note, author); err != nil {
			slog.Warn("note delivery: prepare quote approval failed", "noteId", note.ID, "err", err)
		}
	}

	create := h.renderer.RenderCreate(note, h.idGen)
	// renderer 由来の Create は string/[]string/Note (string fields のみ) で
	// 構成されるため json.Marshal が失敗するケースは存在しない。
	body, _ := json.Marshal(create)

	switch note.Visibility {
	case model.NoteVisibilityPublic, model.NoteVisibilityHome, model.NoteVisibilityFollowers:
		if err := h.deliver.DeliverToFollowers(author.ID, body); err != nil {
			slog.Warn("note delivery: deliver to followers failed",
				"noteId", note.ID, "err", err)
		}
	case model.NoteVisibilitySpecified:
		h.deliverToSpecified(author, note, body)
	}

	// 以下の remote user にも直接配信する。フォロワー配信だけでは相手に
	// 届かず、リプライ / 引用リノート / メンション通知が発生しない。
	//   - メンションされたリモートユーザー (note.Mentions の解決済み ID。本文・CW・
	//     投票の選択肢から作成時に解決したもの。#3330)
	//   - リプライ対象 (ReplyID) の作者がリモートの場合
	//   - 引用リノート (Renote + text/cw/files/poll) の renote 元作者がリモートの場合
	// フォロワー配信先と重複したユーザーは DeliverService 側でも deduplicate
	// されているが、ここでも user ID ベースで seen map を共有して無駄な
	// enqueue を避ける。#369。
	h.deliverToDirectRecipients(author, note, body)

	// リモートの投稿の引用は、Create の後に作者へ承認を求める。
	if h.quotes != nil {
		if err := h.quotes.RequestApproval(note, author); err != nil {
			slog.Warn("note delivery: request quote approval failed", "noteId", note.ID, "err", err)
		}
	}

	// public な note のみ relay に fanout する。relay は AS Public addressed
	// activity しか受け付けないため、home/followers/specified はスキップ。
	if note.Visibility == model.NoteVisibilityPublic && h.relay != nil {
		if err := h.relay.DeliverToAccepted(context.Background(), author.ID, create); err != nil {
			slog.Warn("note delivery: relay fanout failed",
				"noteId", note.ID, "err", err)
		}
	}
}

// deliverToDirectRecipients は mention / reply target / quote renote target の
// 3 経路から remote user を集めて dedup した上で Create activity を直接
// 各 inbox へ送る。
//
// ローカル DB にキャッシュ済みの user だけを対象にする (未解決のリモート
// user は webfinger を叩かない)。local user は連合配信の対象外。
func (h *NoteDeliveryHook) deliverToDirectRecipients(author *model.User, note *model.Note, body []byte) {
	if note == nil {
		return
	}
	seen := make(map[string]struct{})
	// specified の宛先は deliverToSpecified が配り終えている。返信先の作者は
	// 作成時に宛先へ足されるので、ここで除かないと同じ相手へ 2 回配る (#3330)。
	// 本家は DeliverManager が宛先を 1 つの集合にまとめるので 1 回だけ届く。
	if note.Visibility == model.NoteVisibilitySpecified {
		for _, id := range note.VisibleUserIDs {
			seen[id] = struct{}{}
		}
	}
	recipients := make([]*model.User, 0, 3)

	add := func(u *model.User) {
		if u == nil || u.ID == author.ID || u.IsLocal() {
			return
		}
		if _, dup := seen[u.ID]; dup {
			return
		}
		seen[u.ID] = struct{}{}
		recipients = append(recipients, u)
	}

	// 1. メンションされた利用者 (note.Mentions)。本家は mentionedUsers のうち
	//    リモートの利用者へ直接配る。mentions は本文・CW・投票の選択肢から
	//    解決済みの ID なので、ここで本文を読み直さない (読み直すと CW と選択肢の
	//    メンション、`@user@<自ホスト>`、noExtractMentions の扱いが note.Mentions と
	//    食い違う。#3330)。specified の宛先は deliverToSpecified が配るので除く。
	if len(note.Mentions) > 0 {
		ids := make([]string, 0, len(note.Mentions))
		for _, id := range note.Mentions {
			if _, dup := seen[id]; dup {
				continue
			}
			ids = append(ids, id)
		}
		if len(ids) > 0 {
			users, err := h.userRepo.FindManyByIDs(ids)
			if err != nil {
				slog.Warn("direct delivery: mentioned users lookup failed",
					"noteId", note.ID, "err", err)
			}
			byID := make(map[string]*model.User, len(users))
			for _, u := range users {
				byID[u.ID] = u
			}
			// mention の順序で配る。
			for _, id := range ids {
				add(byID[id])
			}
		}
	}

	// 2. reply target の作者
	if note.ReplyID != nil && *note.ReplyID != "" {
		if u := h.findNoteAuthor(*note.ReplyID, note, "reply"); u != nil {
			add(u)
		}
	}

	// 3. quote renote target の作者 (pure renote は Announce で follower 配信
	//    済なので対象外、quote renote だけ対象)。
	if note.RenoteID != nil && *note.RenoteID != "" && !corenote.IsPureRenote(note) {
		if u := h.findNoteAuthor(*note.RenoteID, note, "renote"); u != nil {
			add(u)
		}
	}

	for _, u := range recipients {
		if err := h.deliver.DeliverToUser(author.ID, u, body); err != nil {
			slog.Warn("direct delivery: deliver to user failed",
				"noteId", note.ID, "userId", u.ID, "err", err)
		}
	}
}

// findNoteAuthor は noteID の note を引いて作者 user を返す。失敗は warn log
// して nil を返す。kind はログ用のラベル ("reply" / "renote" 等)。
//
// **ここは障害でも ack する** (#3116)。この hook は note 作成後に `safeGo` で
// 投げっぱなしに呼ばれ、戻り値も retry の仕組みも無い。error を返しても
// 行き先が無いので、**DB 障害のあいだに作られたノートの直接配送は落ちたまま
// 戻らない** (warn log にだけ残る)。直すには配送そのものを job にして queue へ
// 逃がす必要があり、それは hook の設計を変える話になる。
func (h *NoteDeliveryHook) findNoteAuthor(noteID string, origin *model.Note, kind string) *model.User {
	target, err := h.noteRepo.FindByID(noteID)
	if err != nil {
		slog.Warn("direct delivery: "+kind+" target note not found",
			"noteId", origin.ID, "targetId", noteID, "err", err)
		return nil
	}
	user, err := h.userRepo.FindByID(target.UserID)
	if err != nil {
		slog.Warn("direct delivery: "+kind+" target author not found",
			"noteId", origin.ID, "targetId", noteID, "authorId", target.UserID, "err", err)
		return nil
	}
	return user
}

// deliverAnnounce renders an Announce activity for a pure renote and ships it
// to the renoter's followers.
func (h *NoteDeliveryHook) deliverAnnounce(note *model.Note, author *model.User) {
	// upstream renderAnnounce は specified visibility で throw し、federation 自体が
	// 中止される (= specified pure renote は連合しない)。mk-go も specified renote を
	// public-addressed Announce で follower へ漏らさないよう、ここで連合を skip する
	// (#1886)。specified の宛先は visibleUsers 限定であり、renote の Announce を
	// 配送する経路は upstream に存在しない。
	if note.Visibility == model.NoteVisibilitySpecified {
		return
	}
	target, err := h.noteRepo.FindByID(*note.RenoteID)
	if err != nil {
		slog.Warn("note delivery: renote target not found",
			"noteId", note.ID, "renoteId", *note.RenoteID, "err", err)
		return
	}
	targetURI := h.urls.NoteURI(target.ID)
	if target.URI != nil && *target.URI != "" {
		targetURI = *target.URI
	}
	announce := h.renderer.RenderAnnounce(author, note.ID, targetURI, note.Visibility, h.idGen)
	body, _ := json.Marshal(announce)
	if err := h.deliver.DeliverToFollowers(author.ID, body); err != nil {
		slog.Warn("note delivery: announce failed",
			"noteId", note.ID, "err", err)
	}
}

// deliverToSpecified looks up each visibleUserId and routes to the recipient
// if remote.
func (h *NoteDeliveryHook) deliverToSpecified(author *model.User, note *model.Note, body []byte) {
	for _, uid := range note.VisibleUserIDs {
		recipient, err := h.userRepo.FindByID(uid)
		if err != nil {
			slog.Warn("note delivery: visible user not found",
				"noteId", note.ID, "userId", uid, "err", err)
			continue
		}
		if err := h.deliver.DeliverToUser(author.ID, recipient, body); err != nil {
			slog.Warn("note delivery: deliver to user failed",
				"noteId", note.ID, "userId", uid, "err", err)
		}
	}
}

// SendQuoteRequest implements QuoteOutboxDelivery (#3234).
//
// **配送の queue では再試行しない** (#3238)。Mastodon は QuoteRequest を受けると
// 引用の状態を見ずに承認し直すので、何時間も後に届いた再試行が、その間に作者が
// 取り消した引用を承認済みに戻す (範囲を狭めた後なら Reject が返って、こちらが
// 取り消しと誤認する)。送り直しは、保留中かを確かめてから送る定期処理
// (QuoteOutbox.ResendPending) だけが行う。
func (h *NoteDeliveryHook) SendQuoteRequest(note *model.Note, quotedURI string, quotedAuthor *model.User) error {
	body, err := json.Marshal(h.renderer.RenderQuoteRequest(note, quotedURI, h.idGen))
	if err != nil {
		return err
	}
	return h.deliver.DeliverToUserOnce(note.UserID, quotedAuthor, body)
}

// SendNoteUpdate implements QuoteOutboxDelivery (#3234): it re-delivers the
// current rendering of note as an Update to the recipients of its Create.
// 承認を付けたものを、Create を受け取った相手 (フォロワー・直接の宛先・公開なら
// relay) へ配り直す。
func (h *NoteDeliveryHook) SendNoteUpdate(note *model.Note, author *model.User) error {
	if note.LocalOnly {
		return nil
	}
	switch note.Visibility {
	case model.NoteVisibilityPublic, model.NoteVisibilityHome, model.NoteVisibilityFollowers:
	default:
		// 承認を取りに行くのは公開・未収載・フォロワー限定の引用だけ。
		return nil
	}
	// 承認を引けなければ作らずに error で返す (承認の抜けた Update は、相手側で
	// 承認済みの引用を未承認に戻す)。
	update, err := h.renderer.RenderNoteUpdate(note, h.idGen)
	if err != nil {
		return err
	}
	body, err := json.Marshal(update)
	if err != nil {
		return err
	}
	if err := h.deliver.DeliverToFollowers(author.ID, body); err != nil {
		return err
	}
	h.deliverToDirectRecipients(author, note, body)
	// Create を relay 経由で受け取ったサーバーにも届ける (OnNoteCreated と同じ条件)。
	if note.Visibility == model.NoteVisibilityPublic && h.relay != nil {
		if err := h.relay.DeliverToAccepted(context.Background(), author.ID, update); err != nil {
			slog.Warn("note delivery: relay fanout of update failed", "noteId", note.ID, "err", err)
		}
	}
	return nil
}
