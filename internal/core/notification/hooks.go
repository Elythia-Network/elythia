package notification

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// MuteChecker reports whether muter has muted mutee. パッケージ間の循環依存を
// 避けるためinterfaceで受け取る (実装は core/muting)。
type MuteChecker interface {
	IsMuted(muterID, muteeID string) (bool, error)
}

// WebPushPublisher enqueues a Web Push notification job for a user. パッケージ
// 間の循環依存を避けるため interface で受け取る (実装は core/webpush.Service)。
type WebPushPublisher interface {
	PushNotification(userID string, body map[string]any)
}

// NotePacker packs a note ID into a JSON-serializable representation matching
// the Misskey-packed note shape. Used for composing Web Push payloads without
// creating a cycle on the entity package. viewerID is the push recipient
// (notifiee); the packer gates the note by their visibility and returns
// (nil, false) when the recipient cannot see it (#1572).
type NotePacker interface {
	PackNoteByID(noteID, viewerID string) (map[string]any, bool)
}

// UserPacker packs a user ID into a JSON-serializable representation matching
// the Misskey-packed user shape.
type UserPacker interface {
	PackUserByID(userID string) (map[string]any, bool)
}

// Hook implements the various NotificationHook interfaces exposed by other
// services. Single struct in order to share the underlying Service and
// userRepo dependencies.
type Hook struct {
	svc            *Service
	userRepo       repository.UserRepository
	muteChecker    MuteChecker
	webpush        WebPushPublisher
	notePacker     NotePacker
	userPacker     UserPacker
	noteUnreadRepo repository.NoteUnreadRepository
	// followingRepo / renoteMutingRepo は note 通知 (notify='normal' フォロワー
	// への投稿通知) の fan-out に使う optional 依存 (#1559)。未配線なら note 通知
	// を発火しない。
	followingRepo    repository.FollowingRepository
	renoteMutingRepo repository.RenoteMutingRepository
	// userListRepo は notificationRecieveConfig の type=='list' gate で notifier が
	// 指定リストの member かを確認するための optional 依存 (#1775)。未配線なら
	// list gate は素通り (best-effort)。
	userListRepo repository.UserListRepository
	// threadMuteRepo は reply/mention 通知のスレッドミュート gate に使う
	// optional 依存 (#1954)。未配線なら gate は素通り。
	threadMuteRepo repository.NoteThreadMutingRepository
}

// NewHook constructs a Hook bound to a NotificationService and userRepo.
// userRepo is used to look up host information when filtering remote users
// (リモートユーザーへの通知は不要なので除外する)。
func NewHook(svc *Service, userRepo repository.UserRepository) *Hook {
	return &Hook{svc: svc, userRepo: userRepo}
}

// SetMuteChecker attaches a MuteChecker. 通知前にmuteEEがmuterをmuteしている
// 場合は通知をスキップする (Misskey本家の挙動)。
func (h *Hook) SetMuteChecker(c MuteChecker) {
	h.muteChecker = c
}

// SetWebPushPublisher attaches a WebPushPublisher. 通知作成後にWeb Push
// 配信キューへenqueueするために使う。
func (h *Hook) SetWebPushPublisher(p WebPushPublisher) {
	h.webpush = p
}

// SetPackers attaches user/note packers used when composing Web Push payloads.
// Either or both may be nil; payload fields will be omitted accordingly.
func (h *Hook) SetPackers(u UserPacker, n NotePacker) {
	h.userPacker = u
	h.notePacker = n
}

// SetNoteUnreadRepo attaches a NoteUnreadRepository so specified-visibility
// and mention notes create note_unread rows for local recipients (#319).
// Optional — nil disables note_unread tracking; callers fall back to the
// notification-proxy implementation of HasUnreadSpecifiedNotes.
func (h *Hook) SetNoteUnreadRepo(r repository.NoteUnreadRepository) {
	h.noteUnreadRepo = r
}

// SetNoteNotifyRepos attaches the repositories required to fan out 'note'
// notifications to notify='normal' followers (#1559). renoteMutingRepo may be
// nil; pure renotes will then notify all opted-in followers. followingRepo nil
// disables note-notification fan-out entirely.
func (h *Hook) SetNoteNotifyRepos(following repository.FollowingRepository, renoteMuting repository.RenoteMutingRepository) {
	h.followingRepo = following
	h.renoteMutingRepo = renoteMuting
}

// SetThreadMutingRepo attaches a NoteThreadMutingRepository so reply/mention
// notifications are suppressed for recipients who thread-muted the thread,
// matching upstream NoteCreateService (#1954). nil disables the gate.
func (h *Hook) SetThreadMutingRepo(r repository.NoteThreadMutingRepository) {
	h.threadMuteRepo = r
}

// SetUserListRepo attaches the user-list repository used by the
// notificationRecieveConfig type=='list' gate (#1775)。
func (h *Hook) SetUserListRepo(r repository.UserListRepository) {
	h.userListRepo = r
}

// OnNoteCreated is called by note.CreateService after persisting a new note.
// Reply/Renote/Mention の通知を非同期に作成する。
//
// upstream Misskey #17350 (= 2026.5.0 review fix): NotificationManager.queue を
// array から Map<userID, entry> に変更し add() 時の重複検出を O(N) → O(1) に
// する内部 perf 最適化が入ったが、mk-go では同等の重複検出は既に
// resolveMentionIDs 側で seen map による dedup として実装済 (#103 line 153)。
// 本 OnNoteCreated は reply / renote / mention の 3 種別をそれぞれ独立した
// notifyLocalUser 呼び出しで処理する設計のため、upstream の Map 化が想定する
// 「同一 target に複数 reason が積まれる」シナリオは構造的に発生しない (=
// reply target と mention が重なる場合は 132 行の continue で skip 済)。
// よって upstream perf 最適化を mk-go に持ち込む必要はない。
func (h *Hook) OnNoteCreated(n *model.Note, author *model.User, replyTarget, renoteTarget *model.Note) {
	if n == nil || author == nil {
		return
	}
	ctx := context.Background()

	// note.MentionsはID/usernameが混在するため、1回DBで解決してから
	// 両path (通知 / note_unread) で共有する。userRepo未配線なら空スライス。
	mentionedIDs := h.resolveMentionIDs(n, author.ID)

	// specified noteのvisibleUserIds / mentions経由でnote_unreadを
	// 差し込む。通知の有無に関係なくDBに永続化するため、通知抑制 (mute等)
	// や通知未生成のケース (visibleUserIdsに含まれるがmentionされていない
	// ユーザー) もここで捕捉できる。
	h.recordNoteUnreads(n, author, mentionedIDs)

	// upstream Misskey #17335 (= 2026.5.0 fix / triage #1006) + #17363
	// (= 2026.5.1 fix / triage #1010): 通知を生成する前に note の visibility を
	// 見て target を絞る。specified 可視性 note では visibleUserIds 外の user に
	// 通知が飛ぶと情報漏洩になる (= mention 通知から本文の URL が辿れる)。
	// public / home / followers は filter 無し (= upstream の followers ケースが
	// #17363 で null = no filter に揃った)。default (= 不明 visibility) は
	// 通知 fan-out 自体を停止して安全側に倒す。

	// reply: 親ノートの投稿者がローカルユーザーなら通知。ただし reply 先が
	// スレッドをミュートしていれば通知しない (#1954)。thread は reply 先ノートの
	// threadId (無ければ自身の id)。
	if replyTarget != nil && replyTarget.UserID != author.ID &&
		h.notifyVisibleToTarget(n, replyTarget.UserID) &&
		!h.isThreadMuted(replyTarget.UserID, replyTarget) {
		h.notifyLocalUser(ctx, replyTarget.UserID, CreateInput{
			NotifieeID:     replyTarget.UserID,
			NotifierID:     author.ID,
			Type:           TypeReply,
			NoteID:         n.ID,
			NoteVisibility: string(n.Visibility),
		})
	}

	// renote / quote: 対象ノートの投稿者へ通知
	if renoteTarget != nil && renoteTarget.UserID != author.ID &&
		h.notifyVisibleToTarget(n, renoteTarget.UserID) {
		t := TypeRenote
		if isQuote(n) {
			t = TypeQuote
		}
		h.notifyLocalUser(ctx, renoteTarget.UserID, CreateInput{
			NotifieeID:     renoteTarget.UserID,
			NotifierID:     author.ID,
			Type:           t,
			NoteID:         n.ID,
			NoteVisibility: string(n.Visibility),
		})
	}

	// mention通知: resolveMentionIDsで解決済みのIDを使う
	for _, mentionedID := range mentionedIDs {
		// reply先と同じユーザーにはreplyとmentionの両方を出さない
		if replyTarget != nil && replyTarget.UserID == mentionedID {
			continue
		}
		if !h.notifyVisibleToTarget(n, mentionedID) {
			continue
		}
		// mention 先がこのスレッドをミュートしていれば通知しない (#1954)。
		// thread は新規ノート自身の threadId (無ければ自身の id)。
		if h.isThreadMuted(mentionedID, n) {
			continue
		}
		h.notifyLocalUser(ctx, mentionedID, CreateInput{
			NotifieeID:     mentionedID,
			NotifierID:     author.ID,
			Type:           TypeMention,
			NoteID:         n.ID,
			NoteVisibility: string(n.Visibility),
		})
	}

	// note 通知: notify='normal' フォロワーへ投稿を fan-out する (#1559)。
	h.notifyFollowersOfNote(ctx, n, author)
}

// notifyFollowersOfNote fans a 'note' notification out to followers who set
// notify='normal' on their follow edge (upstream NoteCreateService)。
//
// upstream の発火条件をそのまま踏襲する:
//   - reply には付かない (data.reply == null のときだけ)
//   - visibility が specified のノートは対象外
//   - pure renote は、その投稿者の renote を mute しているフォロワーには送らない
func (h *Hook) notifyFollowersOfNote(ctx context.Context, n *model.Note, author *model.User) {
	if h.followingRepo == nil {
		return
	}
	// reply / specified は対象外。
	if n.ReplyID != nil || n.Visibility == model.NoteVisibilitySpecified {
		return
	}
	followers, err := h.followingRepo.ListFollowersToNotify(author.ID)
	if err != nil {
		slog.Warn("note notification: list followers failed", "noteId", n.ID, "err", err)
		return
	}
	// quote でない renote (= pure renote) のみ renote-mute の対象。
	isPureRenote := n.RenoteID != nil && !isQuote(n)
	for _, f := range followers {
		if f.FollowerID == author.ID {
			continue
		}
		if isPureRenote && h.renoteMutingRepo != nil {
			if muted, err := h.renoteMutingRepo.Exists(f.FollowerID, author.ID); err == nil && muted {
				continue
			}
		}
		h.notifyLocalUser(ctx, f.FollowerID, CreateInput{
			NotifieeID:     f.FollowerID,
			NotifierID:     author.ID,
			Type:           TypeNote,
			NoteID:         n.ID,
			NoteVisibility: string(n.Visibility),
		})
	}
}

// OnLogin records a 'login' notification on a user's own stream after a
// successful signin (upstream SigninService)。notifier を持たない。
func (h *Hook) OnLogin(userID string) {
	h.notifyLocalUser(context.Background(), userID, CreateInput{
		NotifieeID: userID,
		Type:       TypeLogin,
	})
}

// OnCreateToken records a 'createToken' notification after a miauth access
// token is generated (upstream miauth/gen-token)。notifier を持たない。
func (h *Hook) OnCreateToken(userID string) {
	h.notifyLocalUser(context.Background(), userID, CreateInput{
		NotifieeID: userID,
		Type:       TypeCreateToken,
	})
}

// OnTest records a 'test' notification on the caller's own stream so that the
// notifications/test-notification endpoint can verify web push / streaming
// delivery (upstream notifications/test-notification)。notifier を持たない。
func (h *Hook) OnTest(userID string) {
	h.notifyLocalUser(context.Background(), userID, CreateInput{
		NotifieeID: userID,
		Type:       TypeTest,
	})
}

// OnRoleAssigned records a 'roleAssigned' notification on the assigned user's
// stream (upstream RoleService.assign)。notifier を持たず、role ID を Extra に
// 格納する (entity 側で read 時に packed role へ解決する)。リモートユーザー
// 宛ては notifyLocalUser の host guard で除外される。
func (h *Hook) OnRoleAssigned(userID, roleID string) {
	h.notifyLocalUser(context.Background(), userID, CreateInput{
		NotifieeID: userID,
		Type:       TypeRoleAssigned,
		Extra:      map[string]any{"roleId": roleID},
	})
}

// OnChatRoomInvitationReceived records a 'chatRoomInvitationReceived'
// notification on the invitee's stream (upstream ChatService の招待作成)。
// notifier は招待者で、invitation ID を Extra に格納する (entity 側で read 時に
// packed invitation へ解決する)。リモート invitee 宛ては notifyLocalUser の
// host guard で除外される。
func (h *Hook) OnChatRoomInvitationReceived(inviteeID, inviterID, invitationID string) {
	h.notifyLocalUser(context.Background(), inviteeID, CreateInput{
		NotifieeID: inviteeID,
		NotifierID: inviterID,
		Type:       TypeChatRoomInvitationReceived,
		Extra:      map[string]any{"invitationId": invitationID},
	})
}

// notifyVisibleToTarget reports whether `targetID` should receive a notification
// about note `n`, based on its visibility (upstream #17335 / triage #1006 +
// upstream #17363 / triage #1010)。
//
//   - public / home / followers / 空: 全 target に通知 (followers は upstream
//     #17363 で null = no filter に揃った)
//   - specified: visibleUserIds に含まれる target だけ
//   - 未知 visibility: 通知しない (= 安全側 fallback、broken state での誤通知防止)
//
// 注: VisibleUserIDs 比較は線形探索だが、specified note の visibleUserIds 配列
// は通常 10 件以下なので map 化のコスト (= 各通知呼び出しで毎回 map 構築) より
// 安い。
// isThreadMuted reports whether userID has muted the thread that threadNote
// belongs to. The thread root is threadNote.ThreadID (falling back to its own
// id), mirroring upstream `note.threadId ?? note.id`. Returns false when the
// repository is not wired (gate disabled) or on lookup error (fail-open: notify).
func (h *Hook) isThreadMuted(userID string, threadNote *model.Note) bool {
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

func (h *Hook) notifyVisibleToTarget(n *model.Note, targetID string) bool {
	switch n.Visibility {
	case "", model.NoteVisibilityPublic, model.NoteVisibilityHome, model.NoteVisibilityFollowers:
		return true
	case model.NoteVisibilitySpecified:
		for _, id := range n.VisibleUserIDs {
			if id == targetID {
				return true
			}
		}
		return false
	default:
		// 未知 visibility は安全側に倒す (= notify しない) が、silent drop だと
		// misconfig や将来の visibility 追加忘れの debug が困難になるため warn
		// で観測可能にする (= fail-closed + observable)。
		slog.Warn("notification: unknown note visibility, dropping notification",
			"visibility", string(n.Visibility),
			"noteId", n.ID,
			"targetId", targetID,
		)
		return false
	}
}

// resolveMentionIDs turns n.Mentions (a mix of user IDs and usernames) into a
// unique slice of resolved user IDs, skipping blanks, self-mentions, and
// unresolvable entries. 空sliceを返すケース: userRepo未配線 / n.Mentionsが空
// / 全entryが失敗。
func (h *Hook) resolveMentionIDs(n *model.Note, authorID string) []string {
	if h.userRepo == nil || len(n.Mentions) == 0 {
		return nil
	}
	out := make([]string, 0, len(n.Mentions))
	seen := map[string]struct{}{}
	for _, idOrName := range n.Mentions {
		if idOrName == "" {
			continue
		}
		mentionedID := idOrName
		if _, err := h.userRepo.FindByID(idOrName); err != nil {
			m, err := h.userRepo.FindByUsernameLower(idOrName, nil)
			if err != nil {
				continue
			}
			mentionedID = m.ID
		}
		if mentionedID == authorID {
			continue
		}
		if _, dup := seen[mentionedID]; dup {
			continue
		}
		seen[mentionedID] = struct{}{}
		out = append(out, mentionedID)
	}
	return out
}

// OnFollowed records a follow notification on the followee's stream.
func (h *Hook) OnFollowed(followerID, followeeID string) {
	h.notifyLocalUser(context.Background(), followeeID, CreateInput{
		NotifieeID: followeeID,
		NotifierID: followerID,
		Type:       TypeFollow,
	})
}

// OnFollowRequested records a "follow request received" notification.
func (h *Hook) OnFollowRequested(followerID, followeeID string) {
	h.notifyLocalUser(context.Background(), followeeID, CreateInput{
		NotifieeID: followeeID,
		NotifierID: followerID,
		Type:       TypeReceiveFollowReq,
	})
}

// OnFollowAccepted records a notification on the requester's side.
// 同時に followee 側に残っている `receiveFollowRequest` 通知を削除する。
// 残したままだと、後で同じユーザーから再度リクエストが来た時に過去分まで
// 一覧に復活する (#349 コメント)。
func (h *Hook) OnFollowAccepted(followerID, followeeID string) {
	h.notifyLocalUser(context.Background(), followerID, CreateInput{
		NotifieeID: followerID,
		NotifierID: followeeID,
		Type:       TypeFollowRequestAccept,
	})
	if h.svc != nil {
		_ = h.svc.DeleteByTypeAndNotifier(context.Background(), followeeID, TypeReceiveFollowReq, followerID)
	}
}

// OnFollowRejected removes the receiveFollowRequest notification on the
// rejecting user's side (follower 側へのrejected通知は本家に無いので作らない)。
func (h *Hook) OnFollowRejected(followerID, followeeID string) {
	if h.svc != nil {
		_ = h.svc.DeleteByTypeAndNotifier(context.Background(), followeeID, TypeReceiveFollowReq, followerID)
	}
}

// OnReactionCreated records a reaction notification on the note author's stream.
func (h *Hook) OnReactionCreated(notifieeID, notifierID, noteID, reaction string) {
	h.notifyLocalUser(context.Background(), notifieeID, CreateInput{
		NotifieeID: notifieeID,
		NotifierID: notifierID,
		Type:       TypeReaction,
		NoteID:     noteID,
		Reaction:   reaction,
	})
}

// OnPollVote records a poll vote notification on the note author's stream.
func (h *Hook) OnPollVote(notifieeID, notifierID, noteID string, choice int) {
	c := choice
	h.notifyLocalUser(context.Background(), notifieeID, CreateInput{
		NotifieeID: notifieeID,
		NotifierID: notifierID,
		Type:       TypePollVote,
		NoteID:     noteID,
		Choice:     &c,
	})
}

// recordNoteUnreads inserts note_unread rows for local recipients of a
// specified-visibility note or a note that mentions them. 本家TSの
// noteUnreadInsert相当で、isSpecified / isMentioned flagはORで集約する
// (upsert)。repo未配線 / noteUnreadRepo == nilのときはno-op。
// mentionedIDsはresolveMentionIDsで解決済みのuser IDリスト。
func (h *Hook) recordNoteUnreads(n *model.Note, author *model.User, mentionedIDs []string) {
	if h.noteUnreadRepo == nil {
		return
	}
	// targets: userID → (isSpecified, isMentioned)
	type flags struct{ specified, mentioned bool }
	targets := map[string]*flags{}
	isSpecifiedNote := n.Visibility == model.NoteVisibilitySpecified

	if isSpecifiedNote {
		for _, uid := range n.VisibleUserIDs {
			if uid == "" || uid == author.ID {
				continue
			}
			t := targets[uid]
			if t == nil {
				t = &flags{}
				targets[uid] = t
			}
			t.specified = true
		}
	}

	for _, mentionedID := range mentionedIDs {
		t := targets[mentionedID]
		if t == nil {
			t = &flags{}
			targets[mentionedID] = t
		}
		t.mentioned = true
	}

	if len(targets) == 0 {
		return
	}

	// notifieeがローカルユーザーでない場合はnote_unreadを作らない
	// (リモートユーザーには自インスタンスのDB未読表示が発生しないため)。
	now := time.Now()
	for uid, f := range targets {
		if h.userRepo != nil {
			u, err := h.userRepo.FindByID(uid)
			if err != nil || u == nil || u.Host != nil {
				continue
			}
		}
		// ephemeral note (リレー由来で DB に無い) では note_unread.noteId の
		// 外部キーを張れないので Upsert が失敗する。materialize しないのは
		// antenna と同じ理由 (#2332)。mention / specified は直接配送でも届く
		// ため DB に在ることがほとんどで、実害は小さい。
		row := &model.NoteUnread{
			ID:          h.svc.idGen.Generate(now),
			UserID:      uid,
			NoteID:      n.ID,
			NoteUserID:  author.ID,
			IsSpecified: f.specified,
			IsMentioned: f.mentioned,
		}
		if err := h.noteUnreadRepo.Upsert(row); err != nil {
			slog.Warn("note_unread upsert failed", "userID", uid, "noteID", n.ID, "err", err)
		}
	}
}

// notifyLocalUser dispatches a notification only when the notifiee is a local
// user (host == nil). リモートユーザーへの通知はAP連合経由で送られるので
// ローカルストリームには入れない。Muteしているnotifierからの通知も抑制する。
func (h *Hook) notifyLocalUser(ctx context.Context, notifieeID string, in CreateInput) {
	if h.userRepo != nil {
		u, err := h.userRepo.FindByID(notifieeID)
		if err != nil {
			return
		}
		if u.Host != nil {
			return
		}
	}
	// notifiee がnotifierをmuteしている場合は通知をスキップする
	if h.muteChecker != nil && in.NotifierID != "" {
		if muted, err := h.muteChecker.IsMuted(notifieeID, in.NotifierID); err == nil && muted {
			return
		}
	}
	// notifiee の notificationRecieveConfig による種別ごとの受信ゲート (#1775)。
	if !h.passesReceiveConfig(notifieeID, in) {
		return
	}
	// #2106 L35 / #2224: Web Push を notification.Service の scheduleUnreadPublish guard 内へ
	// 移す。upstream NotificationService 同様、作成から unreadPublishDelay (既定 2 秒) 以内に
	// 既読化 (MarkAllAsRead) されると unreadNotification stream event だけでなく Web Push も
	// 抑制される。pushFn は webpush 配線時のみ渡し、guard の latestRead チェックを越えた後に
	// 永続化済 Notification から push body を組んで配信する。
	// packer 未設定でも type/id/userId は最低限埋まるので sw.js 側の 24h 破棄チェックと
	// ユーザー判定は成立する。
	var pushFn func(*Notification)
	if h.webpush != nil {
		pushFn = func(n *Notification) {
			if n != nil {
				h.webpush.PushNotification(notifieeID, h.buildPushBody(n, notifieeID))
			}
		}
	}
	if _, err := h.svc.CreateWithPush(ctx, in, pushFn); err != nil {
		slog.Warn("notification create failed", "type", in.Type, "notifiee", notifieeID, "err", err)
		return
	}
}

// receiveConfigEntry mirrors one entry of profile.notificationRecieveConfig
// ({ type, userListId? })。
type receiveConfigEntry struct {
	Type       string `json:"type"`
	UserListID string `json:"userListId"`
}

// passesReceiveConfig reports whether the notifiee's notificationRecieveConfig
// permits delivering a notification of in.Type from in.NotifierID. Mirrors
// upstream NotificationService.createNotification の recieveConfig gate (#1775):
// type==='never' は常に抑制、following/follower/mutualFollow/followingOrFollower/
// list は関係性を満たさないと抑制する。設定未登録 (= 'all') / parse 不能 / 依存
// 未配線は許可側に倒す (best-effort、通知の取りこぼしを避ける)。
func (h *Hook) passesReceiveConfig(notifieeID string, in CreateInput) bool {
	if h.userRepo == nil {
		return true
	}
	profile, err := h.userRepo.FindProfileByUserID(notifieeID)
	if err != nil || profile == nil || len(profile.NotificationRecieveConfig) == 0 {
		return true
	}
	var cfg map[string]receiveConfigEntry
	if json.Unmarshal(profile.NotificationRecieveConfig, &cfg) != nil {
		return true
	}
	entry, ok := cfg[string(in.Type)]
	if !ok {
		return true
	}
	if entry.Type == "never" {
		return false
	}
	if in.NotifierID == "" {
		return true
	}
	switch entry.Type {
	case "following":
		return h.followingExists(notifieeID, in.NotifierID)
	case "follower":
		return h.followingExists(in.NotifierID, notifieeID)
	case "mutualFollow":
		return h.followingExists(notifieeID, in.NotifierID) && h.followingExists(in.NotifierID, notifieeID)
	case "followingOrFollower":
		return h.followingExists(notifieeID, in.NotifierID) || h.followingExists(in.NotifierID, notifieeID)
	case "list":
		return h.listContains(notifieeID, entry.UserListID, in.NotifierID)
	}
	return true
}

// followingExists reports whether followerID follows followeeID. dep 未配線 /
// query error は許可側 (true) に倒す。
func (h *Hook) followingExists(followerID, followeeID string) bool {
	if h.followingRepo == nil {
		return true
	}
	ex, err := h.followingRepo.Exists(followerID, followeeID)
	if err != nil {
		return true
	}
	return ex
}

// listContains reports whether userID is a member of listID, which must be
// owned by ownerID (the notifiee). dep 未配線 / 空 listID / query error は許可側
// (true) に倒す。list が無い、または ownerID のものでなければ false。
func (h *Hook) listContains(ownerID, listID, userID string) bool {
	if h.userListRepo == nil || listID == "" {
		return true
	}
	// userListId は i/update で保存するときに所有者を検証していない (upstream も
	// 同じ)。他人の list を指定されたまま照合すると、通知が届くかどうかでその
	// list のメンバー構成を 1 人ずつ確かめられるので、照合側で所有者を見る
	// (アンテナの照合と同じ多層防御)。
	list, err := h.userListRepo.FindByID(listID)
	if err != nil {
		return !repository.IsNotFound(err)
	}
	if list == nil || list.UserID != ownerID {
		return false
	}
	members, err := h.userListRepo.ListMembers(listID)
	if err != nil {
		return true
	}
	for _, m := range members {
		if m.UserID == userID {
			return true
		}
	}
	return false
}

// buildPushBody converts a persisted Notification into a map matching
// the Misskey `Packed<'Notification'>` shape. Missing fields are omitted so
// that sw.js gracefully falls back to defaults.
func (h *Hook) buildPushBody(n *Notification, notifieeID string) map[string]any {
	body := map[string]any{
		"id":        n.ID,
		"createdAt": n.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		"type":      string(n.Type),
	}
	if n.NotifierID != "" {
		body["userId"] = n.NotifierID
		if h.userPacker != nil {
			if packed, ok := h.userPacker.PackUserByID(n.NotifierID); ok {
				body["user"] = packed
			}
		}
	}
	if n.NoteID != "" {
		body["noteId"] = n.NoteID
		// note embed は受信者 (notifiee) 可視性で gate する。stream 通知 (#1471) と
		// 同じく、見えない note は detail を載せず noteId だけ残す (#1572)。
		// REST i/notifications は #1953 以降そういう行を**丸ごと**落とすので、
		// そちらに揃えないこと (push を落とすと通知自体が届かなくなる)。
		// packer 未配線時は note 省略。
		if h.notePacker != nil {
			if packed, ok := h.notePacker.PackNoteByID(n.NoteID, notifieeID); ok {
				body["note"] = packed
			}
		}
	}
	if n.Reaction != "" {
		body["reaction"] = n.Reaction
	}
	if n.Choice != nil {
		body["choice"] = *n.Choice
	}
	return body
}

// isQuote reports whether the note is a quote renote (renote with
// text/cw/files/poll/reply)。upstream isQuote は replyId != null も quote 扱い (#1886)。
func isQuote(n *model.Note) bool {
	if n.RenoteID == nil {
		return false
	}
	if n.Text != nil && *n.Text != "" {
		return true
	}
	if n.CW != nil && *n.CW != "" {
		return true
	}
	if len(n.FileIDs) > 0 {
		return true
	}
	if n.HasPoll {
		return true
	}
	if n.ReplyID != nil && *n.ReplyID != "" {
		return true
	}
	return false
}
