package webhook

import (
	"context"
	"encoding/json"
	"time"

	corenote "github.com/shiroha-a/mk/internal/core/note"
	"github.com/shiroha-a/mk/internal/core/userpack"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// NoteCreateHook wraps Service to implement the WebhookHook interface
// expected by core/note.CreateService. It fans an `OnNoteCreated` callback
// into the appropriate user webhook events (note / reply / renote / mention).
//
// 発火ルール:
//   - 作者に対して常に `note` を発火
//   - reply の場合は親ノートの投稿者に `reply` を発火
//   - renote の場合は renote 対象の投稿者に `renote` を発火
//   - mention の場合は mention された各ユーザーに `mention` を発火
type NoteCreateHook struct {
	svc   *Service
	idGen id.Generator
	// threadMuteRepo は reply/mention webhook のスレッドミュート gate に使う
	// optional 依存 (#1965)。未配線なら gate は素通り。
	threadMuteRepo repository.NoteThreadMutingRepository
	// followingRepo は note payload の embed (renote/reply/引用先) を受信者の
	// 可視性で gate するために使う。未配線なら fail-closed (followers/specified
	// embed を隠す)。
	followingRepo repository.FollowingRepository
}

// NewNoteCreateHook constructs a NoteCreateHook.
func NewNoteCreateHook(svc *Service, idGen id.Generator) *NoteCreateHook {
	return &NoteCreateHook{svc: svc, idGen: idGen}
}

// SetThreadMutingRepo attaches a NoteThreadMutingRepository so reply/mention
// webhook events are suppressed for recipients who thread-muted the thread,
// matching upstream NoteCreateService (#1965). nil disables the gate.
func (h *NoteCreateHook) SetThreadMutingRepo(r repository.NoteThreadMutingRepository) {
	h.threadMuteRepo = r
}

// SetFollowingRepo attaches a FollowingRepository used to gate the embed
// (renote/reply/引用先) visibility of the note payload per recipient. nil keeps
// the gate fail-closed (followers/specified embeds hidden).
func (h *NoteCreateHook) SetFollowingRepo(r repository.FollowingRepository) {
	h.followingRepo = r
}

// isThreadMuted reports whether userID has muted the thread that threadNote
// belongs to (threadNote.ThreadID ?? threadNote.ID)。repo 未配線 / lookup error
// では false (fail-open: 配信)。#1954 / notification.Hook.isThreadMuted と同一規則。
func (h *NoteCreateHook) isThreadMuted(userID string, threadNote *model.Note) bool {
	if h.threadMuteRepo == nil || threadNote == nil {
		return false
	}
	threadID := threadNote.ID
	if threadNote.ThreadID != nil && *threadNote.ThreadID != "" {
		threadID = *threadNote.ThreadID
	}
	muted, err := h.threadMuteRepo.Exists(userID, threadID)
	return err == nil && muted
}

// OnNoteCreated fires note / reply / renote / mention webhook events. 失敗は
// すべて svc.DispatchUser 内でログに留まる。
func (h *NoteCreateHook) OnNoteCreated(note *model.Note, author *model.User, replyTarget, renoteTarget *model.Note) {
	if h == nil || h.svc == nil || note == nil || author == nil {
		return
	}
	// note payload は受信者ごとに pack + embed gate する。同一 body を共有すると、
	// ある受信者が見られない embed (第三者の followers 引用先など) が別受信者に
	// leak する。upstream の mention webhook と同じ per-recipient gate。
	emit := func(recipientID, event string) {
		body := map[string]any{"note": h.packNoteFor(note, author, recipientID)}
		h.svc.DispatchUser(recipientID, event, body)
	}

	// 作者本人のwebhook
	emit(author.ID, EventNote)

	// reply: 親ノート投稿者。upstream はスレッドミュート中の reply 先には reply
	// webhook を出さない (#1965)。thread は reply 先ノートの threadId。
	if replyTarget != nil && replyTarget.UserID != author.ID && !h.isThreadMuted(replyTarget.UserID, replyTarget) {
		emit(replyTarget.UserID, EventReply)
	}
	// renote: 対象ノート投稿者 (renote はスレッドミュートで gate しない、upstream 同様)。
	if renoteTarget != nil && renoteTarget.UserID != author.ID {
		emit(renoteTarget.UserID, EventRenote)
	}
	// mention: 本文から抽出されたユーザー ID ごとに配信。スレッドミュート中の
	// mention 先には mention webhook を出さない (#1965)。thread は新規ノートの threadId。
	for _, mentionID := range note.Mentions {
		if mentionID == "" || mentionID == author.ID {
			continue
		}
		if h.isThreadMuted(mentionID, note) {
			continue
		}
		emit(mentionID, EventMention)
	}
}

// packNoteFor packs note for a single webhook recipient and applies the
// per-recipient embed visibility gate (renote/reply/引用先) with recipientID as
// the viewer. entity.PackNote produces fresh embed pointers each call, so the
// per-recipient blank never mutates another recipient's payload.
func (h *NoteCreateHook) packNoteFor(note *model.Note, author *model.User, recipientID string) map[string]any {
	if note.User == nil && author != nil {
		clone := *note
		clone.User = author
		note = &clone
	}
	packed := entity.PackNote(note, h.idGen)
	recipient := &model.User{ID: recipientID}
	// **top-level の可視性ゲート。** `gateNoteEmbeds` が見るのは embed だけなので、
	// これが無いと note 本体が受信者の可視性を無視して届く。
	//
	// 実害は `specified` (DM) で出る。`note.Mentions` と `note.VisibleUserIDs` は
	// 乖離しうる (本文に `@x` があっても visible 指定は別) ことを
	// `note_create_service.go` 自身がコメントで書いており、通知経路
	// (`notifyVisibleToTarget`) と main stream (`canSeeNoteForStream`) は
	// その前提で弾いている。**webhook だけが弾いていなかった。**
	//
	// **隠すのであって配送を止めない。** upstream の mention webhook は
	// 受信者ごとに `pack(note, u, {detail:true})` して `hideNote()` を通す形で、
	// 「あなたが mention された」という信号は残す。ここも同じにする。
	//
	// **note / reply / renote / mention の 4 経路すべてに掛かる** —
	// mention だけ直すと隣に同じ穴が残る (`renote` は、自分の public note を
	// followers 限定で引用されたときに引用側の本文が届く形)。
	if !corenote.CanSeeNote(recipient, note, h.followingRepo) {
		entity.HideNoteEntity(&packed)
	}
	gateNoteEmbeds(recipient, &packed, h.followingRepo, time.Now().UnixMilli())
	return noteEntityToMap(packed)
}

// ReactionCreateHook implements the reaction WebhookHook interface.
type ReactionCreateHook struct {
	svc           *Service
	idGen         id.Generator
	followingRepo repository.FollowingRepository
}

// NewReactionCreateHook constructs a ReactionCreateHook.
func NewReactionCreateHook(svc *Service, idGen id.Generator) *ReactionCreateHook {
	return &ReactionCreateHook{svc: svc, idGen: idGen}
}

// SetFollowingRepo attaches a FollowingRepository used to gate the embed
// visibility of the reaction note payload for the recipient (= note author).
// nil keeps the gate fail-closed.
func (h *ReactionCreateHook) SetFollowingRepo(r repository.FollowingRepository) {
	h.followingRepo = r
}

// OnReactionCreated fires the `reaction` webhook event on the note author.
//
// #2106 L42 (intentional additive extension): upstream は webhookEventTypes に 'reaction' を
// 定義しているものの (Webhook.ts), enqueueUserWebhook では reaction を一切 fire しない
// (note/reply/renote/mention/follow 系のみ)。mk-go は valid な購読 enum である 'reaction' を
// 実際に配信する additive 拡張。'reaction' webhook が来ないことを前提にした client との観測差は
// あるが破壊ではない (購読していなければ届かない)。upstream が未配信であることを差分として記録。
func (h *ReactionCreateHook) OnReactionCreated(note *model.Note, reactor *model.User, reaction string) {
	if h == nil || h.svc == nil || note == nil || reactor == nil {
		return
	}
	// reaction webhook の受信者は note 著者 (note.UserID)。embed は受信者 (= 著者)
	// の可視性で gate する。著者本人が見られない第三者ネスト引用先が leak しないように。
	noteForPack := note
	if note.User == nil {
		clone := *note
		clone.User = &model.User{ID: note.UserID}
		noteForPack = &clone
	}
	packed := entity.PackNote(noteForPack, h.idGen)
	gateNoteEmbeds(&model.User{ID: note.UserID}, &packed, h.followingRepo, time.Now().UnixMilli())
	body := map[string]any{
		"note":     noteEntityToMap(packed),
		"userId":   reactor.ID,
		"reaction": reaction,
	}
	h.svc.DispatchUser(note.UserID, EventReaction, body)
}

// ProfileLookup / RelationApplier / ModeratorChecker / UserLookups are the
// lookups of the shared user packer (internal/core/userpack), kept here as
// aliases for existing wiring.
type (
	ProfileLookup    = userpack.ProfileLookup
	RelationApplier  = userpack.RelationApplier
	ModeratorChecker = userpack.ModeratorChecker
	UserLookups      = userpack.Lookups
)

// FollowingHook implements the following WebhookHook interface.
//
// 本家 UserFollowingService は follow / unfollow を
// `pack(followee, follower, {schema: 'UserDetailedNotMe'})` (閲覧者はフォローした側)、
// followed を `pack(follower, followee)` (既定の UserLite) で送る (#3269)。
// main stream の同じイベントも同じ packer で組む (#3330)。
type FollowingHook struct {
	svc    *Service
	packer *userpack.Packer
}

// NewFollowingHook constructs a FollowingHook.
func NewFollowingHook(svc *Service) *FollowingHook {
	return &FollowingHook{svc: svc}
}

// SetUserLookups wires the lookups used to pack the `user` of follow /
// followed / unfollow bodies, and idGen to derive createdAt.
func (h *FollowingHook) SetUserLookups(l UserLookups, idGen id.Generator) {
	h.packer = userpack.New(l, idGen)
}

// SetUserPacker wires a packer shared with the main stream publisher, so the
// webhook and the stream carry the same shape.
func (h *FollowingHook) SetUserPacker(p *userpack.Packer) {
	h.packer = p
}

// OnFollow fires the `follow` event on the follower's webhooks.
func (h *FollowingHook) OnFollow(follower, followee *model.User) {
	h.OnFollowPacked(follower, followee, h.ownPacker(follower, followee))
}

// OnUnfollow fires the `unfollow` event on the follower's webhooks.
func (h *FollowingHook) OnUnfollow(follower, followee *model.User) {
	h.OnUnfollowPacked(follower, followee, h.ownPacker(follower, followee))
}

// OnFollowPacked fires the `follow` event with followee built by packed, which
// the following service shares with the main stream (packed once).
func (h *FollowingHook) OnFollowPacked(follower, followee *model.User, packed func() (entity.UserDetailed, bool)) {
	h.dispatchDetailed(EventFollow, follower, followee, packed)
}

// OnUnfollowPacked fires the `unfollow` event with followee built by packed.
func (h *FollowingHook) OnUnfollowPacked(follower, followee *model.User, packed func() (entity.UserDetailed, bool)) {
	h.dispatchDetailed(EventUnfollow, follower, followee, packed)
}

func (h *FollowingHook) dispatchDetailed(event string, follower, followee *model.User, packed func() (entity.UserDetailed, bool)) {
	if h == nil || h.svc == nil || follower == nil || followee == nil || packed == nil {
		return
	}
	h.svc.DispatchUserLazy(follower.ID, event, func() (any, bool) {
		d, ok := packed()
		if !ok {
			return nil, false
		}
		return map[string]any{"user": toMap(d)}, true
	})
}

// ownPacker builds followee as UserDetailedNotMe seen by follower with the
// hook's own packer, for callers that do not share a packed value.
func (h *FollowingHook) ownPacker(follower, followee *model.User) func() (entity.UserDetailed, bool) {
	if h == nil || follower == nil || followee == nil {
		return nil
	}
	return func() (entity.UserDetailed, bool) {
		return h.userPacker().DetailedNotMe(context.Background(), followee, follower)
	}
}

// OnFollowed fires the `followed` event on the followee's webhooks.
func (h *FollowingHook) OnFollowed(follower, followee *model.User) {
	if h == nil || h.svc == nil || follower == nil || followee == nil {
		return
	}
	h.svc.DispatchUserLazy(followee.ID, EventFollowed, func() (any, bool) {
		return map[string]any{"user": toMap(h.userPacker().Lite(follower))}, true
	})
}

// HasUserPacker reports whether the user packer was wired.
//
// 未配線だと follow / unfollow の Webhook は profile を確かめられないので送られず、
// followed は instance と絵文字を解決しない形になる。起動時検査に使う。
func (h *FollowingHook) HasUserPacker() bool { return h.packer != nil }

// userPacker returns the wired packer, or one without lookups when unwired
// (which drops the detailed bodies, see userpack.Packer.DetailedNotMe).
func (h *FollowingHook) userPacker() *userpack.Packer {
	if h.packer == nil {
		return userpack.New(userpack.Lookups{}, nil)
	}
	return h.packer
}

// SignupHook implements the signup WebhookHook interface, firing the
// `userCreated` system webhook event on new user creation.
type SignupHook struct {
	svc *Service
}

// NewSignupHook constructs a SignupHook.
func NewSignupHook(svc *Service) *SignupHook {
	return &SignupHook{svc: svc}
}

// OnUserCreated fires the `userCreated` system webhook event.
func (h *SignupHook) OnUserCreated(user *model.User) {
	if h == nil || h.svc == nil || user == nil {
		return
	}
	h.svc.DispatchSystem(SystemEventUserCreated, packUser(user))
}

// noteEntityToMap converts a packed NoteEntity into the generic map[string]any
// shape the webhook envelope carries. json.Marshal/Unmarshal は正常値に対して
// 失敗しないため戻り値は常に非 nil。
func noteEntityToMap(packed entity.NoteEntity) map[string]any {
	raw, _ := json.Marshal(packed)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// packUser turns a model.User into a generic map matching entity.UserLite.
// 呼び出し元で u != nil は保証済み。
func packUser(u *model.User) map[string]any {
	return toMap(entity.PackUserLite(u))
}

// toMap converts a packed entity into the generic map the webhook envelope
// carries. json.Marshal/Unmarshal は正常値に対して失敗しない。
func toMap(v any) map[string]any {
	raw, _ := json.Marshal(v)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}
