package federation

import (
	"encoding/json"
	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// PollDeliveryHook handles two outbound AP paths for polls (#690):
//   - OnVote: local user voted on a REMOTE poll → AP Note (with name +
//     inReplyTo) to the author's inbox. Misskey TS notes/polls/vote.ts と
//     同じ wire format。
//   - OnLocalPollUpdated: local poll's vote count changed → AP
//     Update(Question) broadcast to remote followers so counts stay in
//     sync without polling. PollService.deliverQuestionUpdate と同等。
//
// 失敗は best-effort で skip + warn ログ。投票自体は既に成功しているので
// 連合配信失敗で DB 状態を巻き戻すことはしない。
type PollDeliveryHook struct {
	renderer *activitypub.Renderer
	deliver  *DeliverService
	userRepo repository.UserRepository
	urls     *activitypub.URLBuilder
	idGen    id.Generator
}

// NewPollDeliveryHook constructs a PollDeliveryHook.
func NewPollDeliveryHook(renderer *activitypub.Renderer, deliver *DeliverService, userRepo repository.UserRepository, urls *activitypub.URLBuilder, idGen id.Generator) *PollDeliveryHook {
	return &PollDeliveryHook{
		renderer: renderer,
		deliver:  deliver,
		userRepo: userRepo,
		urls:     urls,
		idGen:    idGen,
	}
}

// OnVote is invoked by core/poll.Service after a successful vote. Skip when
// the target poll is local (no remote delivery needed) or when essential AP
// metadata (target.URI, author.Inbox, author.URI) is missing.
func (h *PollDeliveryHook) OnVote(voter *model.User, target *model.Note, choice int, choiceName string) {
	if h == nil || voter == nil || target == nil {
		return
	}
	// local poll: 連合先に通知不要 (local user は同インスタンスの DB から
	// 直接 count を読む)。
	if target.UserHost == nil || *target.UserHost == "" {
		return
	}
	if target.URI == nil || *target.URI == "" {
		return
	}
	author, err := h.userRepo.FindByID(target.UserID)
	if err != nil || author == nil {
		return
	}
	if author.Inbox == nil || *author.Inbox == "" {
		return
	}
	authorURI := ""
	if author.URI != nil {
		authorURI = *author.URI
	}
	if authorURI == "" {
		return
	}

	activity := h.renderer.RenderVote(voter, target, *target.URI, authorURI, choiceName)
	body, err := json.Marshal(activity)
	if err != nil {
		slog.Warn("poll delivery: marshal vote activity failed",
			"voter", voter.ID, "target", target.ID, "err", err)
		return
	}
	if err := h.deliver.DeliverActivity(voter.ID, body, []string{*author.Inbox}); err != nil {
		slog.Warn("poll delivery: enqueue vote deliver failed",
			"voter", voter.ID, "target", target.ID, "inbox", *author.Inbox, "err", err)
	}
}

// OnLocalPollUpdated broadcasts an AP Update(Question) for a local poll to
// all remote followers of the poll author so they refresh their count
// display. local-only ノートや LocalOnly フラグ時は skip。target が
// remote (本来は OnVote の path) の場合も skip する。
func (h *PollDeliveryHook) OnLocalPollUpdated(target *model.Note) {
	if h == nil || target == nil {
		return
	}
	if target.UserHost != nil && *target.UserHost != "" {
		return
	}
	if target.LocalOnly {
		return
	}

	activity, err := h.renderer.RenderQuestionUpdate(target, h.idGen)
	if err != nil {
		// 引用の承認を引けなかった (#3234)。承認の抜けた Update を送ると、相手側で
		// 承認済みの引用が未承認に戻るので、この回の票数の更新は送らない
		// (次の投票で送り直される)。
		slog.Warn("poll delivery: render question update failed", "noteId", target.ID, "err", err)
		return
	}
	body, err := json.Marshal(activity)
	if err != nil {
		slog.Warn("poll delivery: marshal question update failed",
			"noteId", target.ID, "err", err)
		return
	}
	// **配送先は可視性から決める。** Update(Question) は RenderQuestionUpdate が
	// RenderNote をそのまま包むので content / cw / attachment が丸ごと入る。
	// specified (DM) なアンケートに票が入るたびに DeliverToFollowers へ流すと、
	// 宛先でもないリモートフォロワー全員に DM 本文が配送される
	// (upstream PollService.deliverQuestionUpdate も同型だが実害は残る)。
	// recipient 集合の作り方は reaction_delivery_hook.go に揃える:
	// specified は DirectRecipe のみ、それ以外は follower fanout。
	if target.Visibility == model.NoteVisibilitySpecified {
		// **メンション先も入れる (2 周目レビュー H1)。** mk-go は upstream と同じく
		// mention を `visibleUserIds` に入れない (一方向) ので、宛先だけに送ると
		// **メンションされたリモート利用者はノート本文は受け取るのに票数の
		// Update を一度も受け取らない** — 票が永久に初期値のまま見える。
		// `Create` (note_delivery_hook) も `Delete` (note_delete_delivery_hook)
		// も宛先 + メンション先へ送っているので、ここだけ狭いのは非対称。
		ids := make([]string, 0, len(target.VisibleUserIDs)+len(target.Mentions))
		ids = append(ids, target.VisibleUserIDs...)
		ids = append(ids, target.Mentions...)
		// specified なアンケートの Update はフォロワー配信と併用しないので、
		// 個別 inbox でよい。
		inboxes := remoteInboxesForUserIDs(h.userRepo, ids, target.UserID, preferredInbox)
		if len(inboxes) == 0 {
			return
		}
		if err := h.deliver.DeliverActivity(target.UserID, body, inboxes); err != nil {
			slog.Warn("poll delivery: enqueue question update (specified) failed",
				"noteId", target.ID, "err", err)
		}
		return
	}
	if err := h.deliver.DeliverToFollowers(target.UserID, body); err != nil {
		slog.Warn("poll delivery: enqueue question update failed",
			"noteId", target.ID, "err", err)
	}
}

// remoteInboxesForUserIDs resolves an inbox for every REMOTE user in
// ids, skipping local users, blank/duplicate ids, skipID and users without an
// inbox. `inboxOf` picks which one (`preferredInbox` for deliveries that stand
// alone, `fanoutInbox` for those that run alongside the follower fan-out —
// 理由はそれぞれの GoDoc)。 戻り値の inbox URL も重複排除する (sharedInbox を共有する相手が
// 複数居ても 1 回しか送らない)。
//
// note の宛先 (visibleUserIds / mentions) から DirectRecipe の inbox を組み立てる
// 用途で、PollDeliveryHook と NoteDeleteDeliveryHook の両方が使う。
//
// **ここは障害でも ack する** (#3116)。配送 hook は note の作成 / 更新後に
// `safeGo` で投げっぱなしに呼ばれ、戻り値も retry の仕組みも無い (詳しい理由は
// `note_delivery_hook.go` の `findNoteAuthor`)。error を返しても行き先が無いので、
// **DB 障害のあいだは宛先が組めず配送が落ちたまま戻らない** (warn log にだけ残る)。
func remoteInboxesForUserIDs(userRepo repository.UserRepository, ids []string, skipID string, inboxOf func(*model.User) string) []string {
	if userRepo == nil || len(ids) == 0 {
		return nil
	}
	unique := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, uid := range ids {
		if uid == "" || uid == skipID {
			continue
		}
		if _, dup := seen[uid]; dup {
			continue
		}
		seen[uid] = struct{}{}
		unique = append(unique, uid)
	}
	if len(unique) == 0 {
		return nil
	}
	users, err := userRepo.FindManyByIDs(unique)
	if err != nil {
		// 引けなければ配送先を組み立てられない。**フォロワーへの fallback は
		// しない** (宛先限定のノートを無関係な follower へ流すことになる)。
		slog.Warn("federation: direct recipient lookup failed",
			"requested", len(unique), "err", err)
		return nil
	}
	inboxes := make([]string, 0, len(users))
	seenInbox := make(map[string]struct{}, len(users))
	for _, u := range users {
		if u == nil || u.IsLocal() {
			continue
		}
		inbox := inboxOf(u)
		if inbox == "" {
			continue
		}
		if _, dup := seenInbox[inbox]; dup {
			continue
		}
		seenInbox[inbox] = struct{}{}
		inboxes = append(inboxes, inbox)
	}
	return inboxes
}
