package timeline

import (
	"context"
	"log/slog"
	"slices"

	"github.com/elythia-network/elythia/internal/misc/searchnorm"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// CacheLimits captures the four per-user timeline cache caps that
// `meta` exposes (Phase DB-compat issue #51 / parent #33). The values
// correspond directly to:
//
//   - LocalUserUserTimeline  → meta.perLocalUserUserTimelineCacheMax
//   - RemoteUserUserTimeline → meta.perRemoteUserUserTimelineCacheMax
//   - UserHomeTimeline       → meta.perUserHomeTimelineCacheMax
//   - UserListTimeline       → meta.perUserListTimelineCacheMax
//
// 0 や負の値はデフォルト値 (300/100/300/300) にフォールバックする。
type CacheLimits struct {
	LocalUserUserTimeline  int
	RemoteUserUserTimeline int
	UserHomeTimeline       int
	UserListTimeline       int
}

// MetaCacheLimitsProvider abstracts how FanoutHook reads the dynamic timeline
// cache caps from the meta table. 1 引数 1 戻り値の小さい interface にして
// テストで stub しやすくする。実装は通常 metaRepo を呼ぶだけ。
type MetaCacheLimitsProvider interface {
	CacheLimits() CacheLimits
}

// StreamingPublisher publishes a freshly created note to the WebSocket
// streaming pub/sub topics. パッケージ間の循環依存を避けるため interface で
// 受け取る (実装は internal/stream)。topic は Misskey 互換の論理名:
//   - "localTimeline"
//   - "globalTimeline"
//   - "homeTimeline:<userID>"
type StreamingPublisher interface {
	PublishNote(topic string, n *model.Note, author *model.User)
}

// UserListMemberLookup abstracts the query "which user lists contain this
// user?" for user list timeline fanout. 循環依存を避けるため interface で
// 受け取る (実装は repository.UserListRepository)。
//
// ListIDsAndOwnersByMember は followers visibility note の per-list owner
// follow gate (#1465) で使う。owner ごとに author を follow しているかを
// 確認するため list owner 情報が必要なので、専用の 1-query lookup を分けている。
type UserListMemberLookup interface {
	ListIDsByMember(userID string) ([]string, error)
	ListIDsAndOwnersByMember(memberID string) (map[string]string, error)
}

// UserRolesLookup abstracts "which roles does this user hold?" for role timeline
// fanout (#1549). 循環依存回避のため interface (実装は core/role.Service)。
// isExplorable / visibility の gate は consumer (role_timeline.go) 側で行う。
type UserRolesLookup interface {
	GetUserRoles(userID string) ([]*model.Role, error)
}

// ChannelFollowerLister abstracts cursor-paged "who follows this channel?" for
// channel note home fanout (#1686). 循環依存回避のため narrow interface で受け
// 取る (実装は repository.ChannelFollowingRepository)。channel note は author の
// follower ではなく channel の follower の home へ push する (upstream pushToTl
// の channelId 分岐)。
type ChannelFollowerLister interface {
	ListFollowerIDsPage(channelID, afterRowID string, limit int) (ids []string, nextCursor string, err error)
}

// FanoutToggleProvider reports whether meta.enableFanoutTimeline is currently
// on. upstream は同フラグで FTT (Fanout Timeline Technique) 全体を切り、
// NoteCreateService.pushToTl は冒頭で return、timeline endpoint は Redis を
// 見ずに DB へ直行する。mk-go も push / read の両側を同じフラグで揃える。
type FanoutToggleProvider interface {
	FanoutTimelineEnabled() bool
}

// fanoutEnabledCtxKey carries the per-note-creation FTT toggle down to
// pushWithLimit. push は全て pushWithLimit を通るので、ここ 1 箇所で弾けば
// Redis へ一切書かなくなる。publishNote (WebSocket 配信) は FTT とは独立の
// 機能なので gate しない。
type fanoutEnabledCtxKey struct{}

func withFanoutEnabled(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, fanoutEnabledCtxKey{}, enabled)
}

// fanoutEnabledFrom defaults to true so that call paths which never set the
// value (tests, 旧来の呼び出し) keep the previous behaviour.
func fanoutEnabledFrom(ctx context.Context) bool {
	enabled, ok := ctx.Value(fanoutEnabledCtxKey{}).(bool)
	return !ok || enabled
}

// FanoutHook implements note.TimelineFanoutHook by pushing newly-created notes
// onto the appropriate Redis timelines (home/local/global/user).
type FanoutHook struct {
	fanout              *FanoutTimelineService
	followingRepo       repository.FollowingRepository
	publisher           StreamingPublisher
	limits              MetaCacheLimitsProvider
	fanoutToggle        FanoutToggleProvider
	userListRepo        UserListMemberLookup
	userRoles           UserRolesLookup
	channelFollowerRepo ChannelFollowerLister
}

// NewFanoutHook constructs a FanoutHook.
func NewFanoutHook(fanout *FanoutTimelineService, followingRepo repository.FollowingRepository) *FanoutHook {
	return &FanoutHook{fanout: fanout, followingRepo: followingRepo}
}

// SetStreamingPublisher attaches a StreamingPublisher invoked alongside the
// Redis list fan-out so that WebSocket subscribers receive a push.
func (h *FanoutHook) SetStreamingPublisher(p StreamingPublisher) {
	h.publisher = p
}

// SetCacheLimitsProvider attaches a MetaCacheLimitsProvider so that the
// per-timeline-kind cache caps come from the meta table at runtime. Without
// this setter the hook falls back to the package-level MaxTimelineLength
// constant for every timeline (legacy behaviour).
func (h *FanoutHook) SetCacheLimitsProvider(p MetaCacheLimitsProvider) {
	h.limits = p
}

// SetFanoutToggle attaches a FanoutToggleProvider so that note creation
// honours meta.enableFanoutTimeline. Without this setter every note is pushed
// to Redis (legacy behaviour).
func (h *FanoutHook) SetFanoutToggle(p FanoutToggleProvider) {
	h.fanoutToggle = p
}

// SetUserListRepo attaches a UserListMemberLookup so that note creation
// triggers push to userListTimeline:<listId> topics (#330).
func (h *FanoutHook) SetUserListRepo(r UserListMemberLookup) {
	h.userListRepo = r
}

// SetChannelFollowerRepo attaches a ChannelFollowerLister so that channel note
// creation fans the note out to each channel-follower's home timeline (#1686).
// Without it channel notes only reach the channel timeline / channel WS.
func (h *FanoutHook) SetChannelFollowerRepo(r ChannelFollowerLister) {
	h.channelFollowerRepo = r
}

// SetUserRolesLookup attaches a UserRolesLookup so that public note creation
// publishes to roleTimeline:<roleId> topics for the author's roles (#1549).
func (h *FanoutHook) SetUserRolesLookup(r UserRolesLookup) {
	h.userRoles = r
}

// OnNoteCreated delivers the given note to user/home/local/global timelines.
// 配信失敗はログに記録するだけで上位に伝搬しない (ベストエフォート)。
func (h *FanoutHook) OnNoteCreated(n *model.Note, author *model.User) {
	if n == nil || author == nil {
		return
	}
	// FTT が無効なら Redis へは一切 push しない (upstream pushToTl の冒頭 return
	// 相当)。WebSocket 配信は下の publishNote が担うので、ここでは ctx に印を
	// 付けて pushWithLimit 側だけを黙らせる。
	ctx := withFanoutEnabled(context.Background(), h.fanoutEnabled())

	// meta から動的な cache cap を 1 回だけ取得する。timeline 種別 4 つに
	// 渡すので per-push fetch ではなく per-create fetch にする (DB 呼び出し回数
	// を最小化)。limits が nil なら resolveCap は legacy デフォルト
	// (MaxTimelineLength) を返す。
	limits := h.fetchLimits()

	// 1. ユーザータイムライン (本人の投稿一覧)
	//    local user / remote user で別カラムを使う。
	userTimelineKind := UserTimelineKindLocal
	if author.Host != nil && *author.Host != "" {
		userTimelineKind = UserTimelineKindRemote
	}
	// upstream は他人宛ての返信を userTimelineWithReplies に分け、userTimeline
	// には「非返信 + 自己スレッド」だけを積む。withReplies=false でも自己
	// スレッドが出るのはこのため。
	if isReplyToOther(n) {
		h.pushWithLimit(ctx, UserTimelineWithRepliesName(author.ID), n.ID, resolveCap(limits, userTimelineKind))
	} else {
		h.pushWithLimit(ctx, UserTimelineName(author.ID), n.ID, resolveCap(limits, userTimelineKind))
	}

	// 2-5. channel note か否かで home/local/global/userList の配信先を分ける
	//      (#1686, upstream NoteCreateService.pushToTl の channelId 分岐)。
	homeCap := resolveCap(limits, HomeTimelineKind)
	if n.ChannelID != nil && *n.ChannelID != "" {
		// channel note は author の follower ではなく channel の follower の home
		// へ fanout する。LTL/GTL/userList へは出さない (channel note はこれらの
		// timeline に乗らない = upstream 互換、LTL/GTL の DB query も channelId IS
		// NULL で除外している)。channel WS への live publish は下記 step 6 が担う。
		if h.channelFollowerRepo != nil {
			h.fanoutToChannelFollowers(ctx, *n.ChannelID, n.ID, homeCap)
		}
	} else {
		// 2. ホームタイムライン: 投稿者本人 + フォロワー全員
		//    follower一覧の取得は1ページずつ繰り返し読みだす
		//
		// 投稿者本人の HTL は upstream NoteCreateService の「自分自身のHTL」
		// ブロックと同じ 2 条件で絞る。
		//   - リモートユーザーの HTL は作らない (note.userHost == null)。
		//     こちらのインスタンスがリモートユーザーのタイムラインを持つ意味が
		//     無く、Redis を無駄に太らせるだけ。
		//   - visibility=specified で自分が宛先に入っている場合は push しない
		//     (宛先としての配送が別途あるので二重になる)。
		if author.Host == nil && !selfIsSpecifiedTarget(n, author.ID) {
			h.pushWithLimit(ctx, HomeTimelineName(author.ID), n.ID, homeCap)
		}
		h.publishNote("homeTimeline:"+author.ID, n, author)
		if h.followingRepo != nil && shouldFanoutToFollowers(n) {
			h.fanoutToFollowersAndStream(ctx, author.ID, n, author, homeCap)
		}

		// 3. ローカルタイムライン: ローカル投稿でvisibility=publicのみ。
		// home visibilityはフォロワー向けなのでLTLには出さない (本家と同じ挙動)。
		//
		// upstream は返信の有無で LTL を 3 本に分ける。素の localTimeline には
		// 返信を入れず、「自分以外への返信」は localTimelineWithReplies と
		// localTimelineWithReplyTo:<replyUserId> に積む。取得時にどれを合流
		// させるかで「他人の他人への返信は出ない」「自分宛ての返信は出る」を
		// 両立させている (NoteCreateService の isReply 分岐)。
		if author.Host == nil && n.Visibility == model.NoteVisibilityPublic {
			if isReplyToOther(n) {
				h.pushWithLimit(ctx, LocalTimelineWithReplies, n.ID, MaxTimelineLength)
				// 返信先がローカルユーザーのときだけ宛先別キーに積む
				// (リモート宛ての返信を持っても引く人が居ない)。
				if n.ReplyUserID != nil && (n.ReplyUserHost == nil || *n.ReplyUserHost == "") {
					h.pushWithLimit(ctx, LocalTimelineWithReplyToName(*n.ReplyUserID), n.ID, MaxTimelineLength/10)
				}
			} else {
				h.pushWithLimit(ctx, LocalTimeline, n.ID, MaxTimelineLength)
			}
			h.publishNote("localTimeline", n, author)
		}

		// 4. グローバルタイムライン: visibility=publicのみ
		if n.Visibility == model.NoteVisibilityPublic {
			h.pushWithLimit(ctx, GlobalTimeline, n.ID, MaxTimelineLength)
			h.publishNote("globalTimeline", n, author)
		}

		// 5. ユーザーリストタイムライン: 投稿者が属するリストへ配信。
		// specified (DM) も upstream は push 対象にし、リスト所有者が宛先か
		// どうかで per-list に判定する (fanoutToUserLists 側)。
		if h.userListRepo != nil && (shouldFanoutToFollowers(n) || n.Visibility == model.NoteVisibilitySpecified) {
			listCap := resolveCap(limits, UserListTimelineKind)
			h.fanoutToUserLists(ctx, n, author, listCap)
		}
	}

	// 6. チャンネルタイムライン (Misskey channel): channel:<channelId> へ live
	//    publish する (#1549)。旧実装は core/channel.OnNotePosted で counter のみ
	//    更新し pubsub publish が無かったため channel WS が live note を1件も
	//    受信していなかった。可視性は consumer 側 (channel_timeline.go) で gate。
	if n.ChannelID != nil && *n.ChannelID != "" {
		h.publishNote("channel:"+*n.ChannelID, n, author)
	}

	// 7. ロールタイムライン (#1549): 著者の各ロールの roleTimeline:<roleId> へ
	//    publish。consumer (role_timeline.go) が isExplorable role かつ
	//    visibility==public のみ emit するので、publish 自体も public note に
	//    絞って無駄な fanout を避ける (isExplorable は runtime 可変なので gate は
	//    consumer 側に置く、本家 RoleService.addNoteToRoleTimeline と同じ)。
	if h.userRoles != nil && n.Visibility == model.NoteVisibilityPublic {
		if roles, err := h.userRoles.GetUserRoles(author.ID); err == nil {
			for _, r := range roles {
				h.publishNote("roleTimeline:"+r.ID, n, author)
			}
		}
	}

	// 8. ハッシュタグ stream (#1549): note.Tags の各タグを正規化 (NFKC+lower) して
	//    hashtag:<normalized> へ publish。consumer (hashtag.go) が OR-of-ANDs を
	//    payload tags に対して再評価 + dedupe + 可視性 gate する。正規化は publish/
	//    subscribe 両側で searchnorm.Normalize に揃える (揃えないと topic 文字列が
	//    一致せず配信されない)。可視性は consumer 側 gate (TS notesStream と同じ)。
	if len(n.Tags) > 0 {
		seenTags := make(map[string]struct{}, len(n.Tags))
		for _, tag := range n.Tags {
			key := searchnorm.Normalize(tag)
			if key == "" {
				continue
			}
			if _, ok := seenTags[key]; ok {
				continue
			}
			seenTags[key] = struct{}{}
			h.publishNote("hashtag:"+key, n, author)
		}
	}
}

// fetchLimits returns the meta-derived cache caps, or zero values if no
// provider is wired (resolveCap then falls back to legacy defaults).
func (h *FanoutHook) fetchLimits() CacheLimits {
	if h.limits == nil {
		return CacheLimits{}
	}
	return h.limits.CacheLimits()
}

// TimelineKind enumerates the four per-user timeline categories that
// have a dedicated meta cache-cap column.
type TimelineKind int

// Sentinels for resolveCap. Local/global timelines do not have a dedicated
// meta column and continue to use the legacy MaxTimelineLength constant.
const (
	UserTimelineKindLocal TimelineKind = iota
	UserTimelineKindRemote
	HomeTimelineKind
	UserListTimelineKind
)

// resolveCap picks the right cap from limits and falls back to the per-kind
// legacy default when meta value is zero/negative. This keeps fresh installs
// (where the column hasn't been touched yet) on the documented Misskey
// defaults of 300/100/300/300.
func resolveCap(limits CacheLimits, kind TimelineKind) int {
	switch kind {
	case UserTimelineKindLocal:
		if limits.LocalUserUserTimeline > 0 {
			return limits.LocalUserUserTimeline
		}
		return 300
	case UserTimelineKindRemote:
		if limits.RemoteUserUserTimeline > 0 {
			return limits.RemoteUserUserTimeline
		}
		return 100
	case HomeTimelineKind:
		if limits.UserHomeTimeline > 0 {
			return limits.UserHomeTimeline
		}
		return 300
	case UserListTimelineKind:
		if limits.UserListTimeline > 0 {
			return limits.UserListTimeline
		}
		return 300
	}
	return MaxTimelineLength
}

// publishNote forwards the note to the StreamingPublisher when set. Best-
// effort wrapper used by OnNoteCreated for non-follower topics; the per-
// follower streaming publish is inlined in fanoutToFollowersAndStream.
func (h *FanoutHook) publishNote(topic string, n *model.Note, author *model.User) {
	if h.publisher == nil {
		return
	}
	h.publisher.PublishNote(topic, n, author)
}

// shouldFanoutToFollowers reports whether followers' home timelines should
// receive this note.
//
// specified も対象に含める。upstream の pushToTl は follower ループの中で
// `note.visibility === 'specified' && !note.visibleUserIds.includes(followerId)`
// を見て個別に skip するので、宛先になっている follower の home には届く。
// ループごと外すと「フォローしている相手からの DM が home に出ない」になる。
func shouldFanoutToFollowers(n *model.Note) bool {
	switch n.Visibility {
	case model.NoteVisibilityPublic, model.NoteVisibilityHome,
		model.NoteVisibilityFollowers, model.NoteVisibilitySpecified:
		return true
	}
	return false
}

// fanoutToFollowersAndStream walks the author's followers once and, for each
// follower, both pushes the note id onto the home timeline list and publishes
// the note to the per-follower streaming topic.
//
// 旧実装は同じ followers ページネーションを Redis push 用と streaming publish
// 用で 2 回繰り返しており、フォロワー数 N に対して `ListFollowers` の DB
// クエリが 2N/pageSize 回走っていた (#300 2-4)。両者の per-row 操作はどちら
// もベストエフォートで失敗が他方に波及しないため、1 ループに畳む方が安全か
// つ DB 負荷半減になる。publisher 不在の場合だけ streaming step を skip する。
// homeCap は OnNoteCreated 側で meta から取得済みのものを再利用する。
func (h *FanoutHook) fanoutToFollowersAndStream(ctx context.Context, authorID string, n *model.Note, author *model.User, homeCap int) {
	const pageSize = 200
	// reply note は follower の `following.withReplies` 設定で push 制御
	// する (#1047 / upstream 互換)。
	//   - 通常 note (= replyId nil) → 全 follower に push
	//   - self-thread (= replyUserId = userId) → 全 follower に push (= TL
	//     filter で `replyUserId = note.userId` 経路で残るので fanout でも
	//     全 push する semantics)
	//   - reply-to-follower (= replyUserId = follower.id、自分への reply) →
	//     全 push (upstream `replyToMe` escape hatch、#1150)。stream filter
	//     `replyShouldEmit` も同 escape hatch を持つので fanout / stream の
	//     挙動を symmetric に保つ。
	//   - その他 reply (= 他人宛 reply) → withReplies=true の follower のみ push
	// これにより「他人 A → 他人 B reply」が default で他 follower の TL に
	// 流れず、かつ「他人 A → follower 本人」reply は default で push される
	// (= Misskey TS の `following.withReplies` setting + replyToMe 仕様と
	// 完全互換)。
	//
	// 加えて #1152: reply 対象 note の `visibility=followers` の場合、reply
	// target を follow していない follower は drop する (= stream filter
	// `replyShouldEmit` の followers-visibility gate と symmetric)。「context
	// が見えない reply 本文だけが流れる」privacy 漏洩を防ぐ。`n.Reply` が
	// nil (= FindByIDWithRelations の preload 失敗) の case は visibility 不明
	// なので保守的に gate を skip (= 既存挙動 = push)。stream filter は drop
	// に倒すが fanout は全 follower への影響が大きいので over-restrict を避ける。
	isReply := n.ReplyID != nil
	isSelfThread := isReply && n.ReplyUserID != nil && *n.ReplyUserID == n.UserID
	isFollowersOnlyReply := isReply && n.Reply != nil && n.Reply.Visibility == model.NoteVisibilityFollowers
	offset := 0
	for {
		rows, err := h.followingRepo.ListFollowers(authorID, pageSize, offset)
		if err != nil {
			slog.Warn("fanoutToFollowersAndStream: list followers failed", "err", err, "author", authorID)
			return
		}
		if len(rows) == 0 {
			return
		}
		// followers-only reply の per-page follow check (batch lookup)。
		// `FilterFollowingsToAnchor(replyUserID, followerIDs)` で「reply target
		// を follow している follower の subset」を 1 query で取得する。
		// query 失敗時は保守的に gate を skip (= 既存挙動 = push)。
		var followsReplyTarget map[string]bool
		if isFollowersOnlyReply && n.ReplyUserID != nil {
			followerIDs := make([]string, 0, len(rows))
			for _, f := range rows {
				followerIDs = append(followerIDs, f.FollowerID)
			}
			if ids, err := h.followingRepo.FilterFollowingsToAnchor(*n.ReplyUserID, followerIDs); err == nil {
				followsReplyTarget = make(map[string]bool, len(ids))
				for _, id := range ids {
					followsReplyTarget[id] = true
				}
			}
		}
		for _, f := range rows {
			// #2106 L34: upstream pushToTl は local かつ非 hibernated follower のみ home TL へ
			// push する。remote follower はこのインスタンスの home TL を読まず、hibernated user は
			// inactive なので、不要な Redis 書き込み・stream publish を避ける。
			if f.FollowerHost != nil || f.IsFollowerHibernated {
				continue
			}
			// specified は宛先の follower にだけ届ける (upstream pushToTl と同じ)。
			if n.Visibility == model.NoteVisibilitySpecified &&
				!slices.Contains([]string(n.VisibleUserIDs), f.FollowerID) {
				continue
			}
			isReplyToFollower := isReply && n.ReplyUserID != nil && *n.ReplyUserID == f.FollowerID
			// follower が note.mentions に含まれているなら withReplies=false の
			// reply filter を escape する (mk-go 独自仕様 / 上流 TS は持たない
			// escape hatch、#1195)。「他人 A → 他人 B reply」で本文に viewer
			// (= follower) への mention が含まれているとき、author の意図とし
			// て viewer に届けたいはずなので follower の withReplies 設定で隠
			// さない。
			isMentioned := isReply && slices.Contains(n.Mentions, f.FollowerID)
			if isReply && !isSelfThread && !isReplyToFollower && !isMentioned && !f.WithReplies {
				continue
			}
			// followers-only reply: reply target を follow していない follower
			// は drop (isReplyToFollower の場合は follower 自身が target なので
			// escape)。followsReplyTarget が nil (= query 失敗 / 該当無し) の
			// 場合は保守的に skip gate せず push 継続。
			if isFollowersOnlyReply && !isReplyToFollower && followsReplyTarget != nil {
				if !followsReplyTarget[f.FollowerID] {
					continue
				}
			}
			h.pushWithLimit(ctx, HomeTimelineName(f.FollowerID), n.ID, homeCap)
			if h.publisher != nil {
				h.publisher.PublishNote("homeTimeline:"+f.FollowerID, n, author)
			}
		}
		if len(rows) < pageSize {
			return
		}
		offset += pageSize
	}
}

// fanoutToUserLists pushes the note to all user lists that contain the author.
//
// followers visibility note の場合は per-list owner が author を follow している
// 場合のみ push する (#1465)。user-list は owner が任意のユーザーを member に
// 追加できる (フォロー関係不要) ので、何も gate しないと「list owner が author
// を follow していない」状態でも followers note を REST `notes/user-list-timeline`
// (#1442 で REST 側は SQL/handler で塞いだ) と WS `userList` channel の両方から
// 取得できてしまう。REST handler の visibility filter は post-fetch で済むが、
// WS channel は pubsub 受信時点でフィルタを持たないため push 段で gate しないと
// realtime に漏れる。本人 (`ownerID == author.ID`) は CanSeeNote の short-circuit
// と同じく無条件 pass。public / home は従来どおり全 list に push する。
func (h *FanoutHook) fanoutToUserLists(ctx context.Context, n *model.Note, author *model.User, listCap int) {
	// followers 以外 (public / home) は per-list owner の follow check 不要なので
	// 旧経路 (ListIDsByMember + 全 list へ push) のままで済ます。これにより
	// 通常 note の hot path に lookup を増やさない。
	if n.Visibility == model.NoteVisibilitySpecified {
		// upstream: リスト所有者が author 本人か visibleUserIds に含まれる場合だけ
		// そのリストへ push する。他人宛ての DM を他人のリストに流さない。
		owners, err := h.userListRepo.ListIDsAndOwnersByMember(author.ID)
		if err != nil {
			slog.Warn("fanoutToUserLists: list+owner lookup failed", "err", err, "author", author.ID)
			return
		}
		for listID, ownerID := range owners {
			if ownerID != author.ID && !slices.Contains([]string(n.VisibleUserIDs), ownerID) {
				continue
			}
			h.pushWithLimit(ctx, UserListTimelineName(listID), n.ID, listCap)
			// 他の分岐と同じく WS へも流す。push だけだと userList channel に
			// live note が届かず、再読込するまで DM が出てこない。
			h.publishNote("userListTimeline:"+listID, n, author)
		}
		return
	}
	if n.Visibility != model.NoteVisibilityFollowers {
		listIDs, err := h.userListRepo.ListIDsByMember(author.ID)
		if err != nil {
			slog.Warn("fanoutToUserLists: list lookup failed", "err", err, "author", author.ID)
			return
		}
		for _, listID := range listIDs {
			h.pushWithLimit(ctx, UserListTimelineName(listID), n.ID, listCap)
			h.publishNote("userListTimeline:"+listID, n, author)
		}
		return
	}

	// followers visibility: list owner ごとに author を follow しているかを check。
	// 自分自身 (owner == author) も pass する (CanSeeNote と同じ短絡)。
	owners, err := h.userListRepo.ListIDsAndOwnersByMember(author.ID)
	if err != nil {
		slog.Warn("fanoutToUserLists: list+owner lookup failed", "err", err, "author", author.ID)
		return
	}
	if len(owners) == 0 {
		return
	}
	// followingRepo 未配線時は fail-closed に倒す (= followers visibility note は
	// 本人 list へのみ push し、他 owner の list には push しない)。
	// CanSeeNote の semantics と一致させる。
	if h.followingRepo == nil {
		for listID, ownerID := range owners {
			if ownerID != author.ID {
				continue
			}
			h.pushWithLimit(ctx, UserListTimelineName(listID), n.ID, listCap)
			h.publishNote("userListTimeline:"+listID, n, author)
		}
		return
	}
	// distinct な non-self owner を 1 query で「author を follow しているか」
	// 判定する (#1144 batch method の前例に揃える)。owner ごとに `Exists` を
	// ループすると、author が多数の異なる owner 所有 list の member の場合に
	// N+1 になり、同一 owner が複数 list を持つ場合は重複呼び出しになる
	// (#1468 review)。
	distinctOwners := make(map[string]struct{}, len(owners))
	for _, ownerID := range owners {
		if ownerID == author.ID {
			continue
		}
		distinctOwners[ownerID] = struct{}{}
	}
	followingOwners := make(map[string]struct{}, len(distinctOwners))
	if len(distinctOwners) > 0 {
		candidates := make([]string, 0, len(distinctOwners))
		for ownerID := range distinctOwners {
			candidates = append(candidates, ownerID)
		}
		// rows where followerID IN owners AND followeeID = author → returns the
		// subset of owners that follow author. err 時は fail-closed: 本人 list
		// 以外は push しない (= 旧 per-owner Exists 失敗時より厳しめだが、
		// "誰が author を follow しているか" 全く分からない状況で部分 push する
		// より安全寄り)。
		filtered, ferr := h.followingRepo.FilterFollowingsToAnchor(author.ID, candidates)
		if ferr != nil {
			slog.Warn("fanoutToUserLists: batch follow check failed",
				"err", ferr, "author", author.ID, "owners", len(candidates))
			for listID, ownerID := range owners {
				if ownerID != author.ID {
					continue
				}
				h.pushWithLimit(ctx, UserListTimelineName(listID), n.ID, listCap)
				h.publishNote("userListTimeline:"+listID, n, author)
			}
			return
		}
		for _, ownerID := range filtered {
			followingOwners[ownerID] = struct{}{}
		}
	}
	for listID, ownerID := range owners {
		if ownerID != author.ID {
			if _, ok := followingOwners[ownerID]; !ok {
				continue
			}
		}
		h.pushWithLimit(ctx, UserListTimelineName(listID), n.ID, listCap)
		h.publishNote("userListTimeline:"+listID, n, author)
	}
}

// channelFollowerPageSize は channel follower fanout の cursor batch サイズ。
// #320 の channel unread fanout (channel_service.go) と同値に揃える。
const channelFollowerPageSize = 500

// fanoutToChannelFollowers pushes a channel note ID onto every channel
// follower's home timeline Redis list (#1686, upstream pushToTl の channelId
// 分岐)。cursor-based pagination で全 follower を走査するため popular channel
// でも O(followers / pageSize) 回のラウンドトリップで完了する。push 先は home
// list のみ (live WS 配信は channel:<id> topic が担当するため home WS への
// per-follower publish は行わない)。reply gating は適用しない (upstream の
// channel 分岐も follower 全員へ push する)。
func (h *FanoutHook) fanoutToChannelFollowers(ctx context.Context, channelID, noteID string, homeCap int) {
	cursor := ""
	for {
		ids, next, err := h.channelFollowerRepo.ListFollowerIDsPage(channelID, cursor, channelFollowerPageSize)
		if err != nil {
			slog.Warn("fanoutToChannelFollowers: list channel followers failed", "err", err, "channel", channelID)
			return
		}
		if len(ids) == 0 {
			return
		}
		for _, fid := range ids {
			h.pushWithLimit(ctx, HomeTimelineName(fid), noteID, homeCap)
		}
		if next == "" {
			return
		}
		cursor = next
	}
}

// removeFromChannelFollowerHomes removes a channel note ID from every channel
// follower's home timeline list, mirroring fanoutToChannelFollowers for
// OnNoteDeleted (#1686).
func (h *FanoutHook) removeFromChannelFollowerHomes(ctx context.Context, channelID, noteID string) {
	cursor := ""
	for {
		ids, next, err := h.channelFollowerRepo.ListFollowerIDsPage(channelID, cursor, channelFollowerPageSize)
		if err != nil {
			slog.Warn("removeFromChannelFollowerHomes: list channel followers failed", "err", err, "channel", channelID)
			return
		}
		if len(ids) == 0 {
			return
		}
		for _, fid := range ids {
			h.removeBestEffort(ctx, HomeTimelineName(fid), noteID)
		}
		if next == "" {
			return
		}
		cursor = next
	}
}

// fanoutEnabled reports whether meta.enableFanoutTimeline is on. Provider 未配線
// なら従来どおり有効扱い。
func (h *FanoutHook) fanoutEnabled() bool {
	if h.fanoutToggle == nil {
		return true
	}
	return h.fanoutToggle.FanoutTimelineEnabled()
}

// pushWithLimit wraps Push with error logging and an explicit cap.
func (h *FanoutHook) pushWithLimit(ctx context.Context, name Name, id string, maxLen int) {
	if !fanoutEnabledFrom(ctx) {
		return
	}
	if err := h.fanout.Push(ctx, name, id, maxLen); err != nil {
		slog.Warn("timeline push failed", "name", string(name), "id", id, "err", err)
	}
}

// RemoveNoteID drops a note ID from the timeline lists it could have been
// pushed to, without needing the full note row (#2332).
//
// ephemeral note が DB 行に置き換わったときに旧 ID を除くために使う。残すと
// hydrate で ephemeral 側が拾われ続けて二重表示になる。OnNoteDeleted と違い
// *model.Note を持たない (Redis から既に落ちている) ので、fan-out 先の判定に
// 必要な最小限 (著者 / visibility / host) だけを受け取る。
//
// リレー由来の投稿はローカルにフォロワーが居ない前提なので、follower の home
// までは辿らない。著者の userTimeline と global / local だけを掃除する。
func (h *FanoutHook) RemoveNoteID(noteID string, author *model.User, visibility string, host *string) {
	if h == nil || h.fanout == nil || noteID == "" || author == nil {
		return
	}
	ctx := context.Background()
	h.removeBestEffort(ctx, UserTimelineName(author.ID), noteID)
	if visibility == string(model.NoteVisibilityPublic) {
		h.removeBestEffort(ctx, GlobalTimeline, noteID)
		if host == nil || *host == "" {
			h.removeBestEffort(ctx, LocalTimeline, noteID)
		}
	}
}

// OnNoteDeleted purges the deleted note ID from every Redis timeline list
// it could have been pushed to by OnNoteCreated (#379)。inbound Delete も
// ローカル削除も同じ経路を通って届くので、ここ 1 か所で fan-out 先全てを
// 掃除する。失敗はベストエフォート (DB は既に消えているので部分残留しても
// 後段の note hydrate で missing として落ちる)。
func (h *FanoutHook) OnNoteDeleted(n *model.Note, author *model.User) {
	if n == nil || author == nil {
		return
	}
	ctx := context.Background()

	// 1. ユーザータイムライン
	h.removeBestEffort(ctx, UserTimelineName(author.ID), n.ID)

	// 2-5. OnNoteCreated と対称に、channel note か否かで掃除先を分ける (#1686)。
	if n.ChannelID != nil && *n.ChannelID != "" {
		// channel note は channel follower の home にのみ push されているので
		// そこから掃除する (author home / user-follower / LTL / GTL / userList
		// には push していない)。
		if h.channelFollowerRepo != nil {
			h.removeFromChannelFollowerHomes(ctx, *n.ChannelID, n.ID)
		}
	} else {
		// 2. ホームタイムライン: 投稿者本人 + フォロワー全員
		h.removeBestEffort(ctx, HomeTimelineName(author.ID), n.ID)
		if h.followingRepo != nil && shouldFanoutToFollowers(n) {
			h.removeFromFollowerHomes(ctx, author.ID, n.ID)
		}

		// 3. ローカルタイムライン
		if author.Host == nil && n.Visibility == model.NoteVisibilityPublic {
			h.removeBestEffort(ctx, LocalTimeline, n.ID)
		}

		// 4. グローバルタイムライン
		if n.Visibility == model.NoteVisibilityPublic {
			h.removeBestEffort(ctx, GlobalTimeline, n.ID)
		}

		// 5. ユーザーリストタイムライン
		if h.userListRepo != nil && shouldFanoutToFollowers(n) {
			h.removeFromUserLists(ctx, author.ID, n.ID)
		}
	}
}

// removeBestEffort wraps Remove with error logging.
func (h *FanoutHook) removeBestEffort(ctx context.Context, name Name, noteID string) {
	if err := h.fanout.Remove(ctx, name, noteID); err != nil {
		slog.Warn("timeline remove failed", "name", string(name), "id", noteID, "err", err)
	}
}

// removeFromFollowerHomes mirrors fanoutToFollowersAndStream: page through
// followers and LREM the note ID from each follower's home timeline list.
func (h *FanoutHook) removeFromFollowerHomes(ctx context.Context, authorID, noteID string) {
	const pageSize = 200
	offset := 0
	for {
		rows, err := h.followingRepo.ListFollowers(authorID, pageSize, offset)
		if err != nil {
			slog.Warn("removeFromFollowerHomes: list followers failed", "err", err, "author", authorID)
			return
		}
		if len(rows) == 0 {
			return
		}
		for _, f := range rows {
			h.removeBestEffort(ctx, HomeTimelineName(f.FollowerID), noteID)
		}
		if len(rows) < pageSize {
			return
		}
		offset += pageSize
	}
}

// removeFromUserLists mirrors fanoutToUserLists: query the lists that contain
// the author at delete time and LREM the note ID from each.
func (h *FanoutHook) removeFromUserLists(ctx context.Context, authorID, noteID string) {
	listIDs, err := h.userListRepo.ListIDsByMember(authorID)
	if err != nil {
		slog.Warn("removeFromUserLists: list lookup failed", "err", err, "author", authorID)
		return
	}
	for _, listID := range listIDs {
		h.removeBestEffort(ctx, UserListTimelineName(listID), noteID)
	}
}

// selfIsSpecifiedTarget reports whether the note is a specified-visibility note
// that lists the author itself as a recipient.
//
// upstream NoteCreateService の「自分自身のHTL」条件
// `note.visibility !== 'specified' || !note.visibleUserIds.some(v => v === user.id)`
// の否定。宛先に自分が入っているダイレクトは、宛先としての配送で届くので
// 自分の HTL には積まない。
func selfIsSpecifiedTarget(n *model.Note, authorID string) bool {
	if n.Visibility != model.NoteVisibilitySpecified {
		return false
	}
	for _, id := range n.VisibleUserIDs {
		if id == authorID {
			return true
		}
	}
	return false
}

// isReplyToOther reports whether the note is a reply to someone other than its
// own author. upstream misc/is-reply.ts の isReply(note) (viewer 指定なし) と
// 同じ条件。
func isReplyToOther(n *model.Note) bool {
	return n.ReplyID != nil && n.ReplyUserID != nil && *n.ReplyUserID != n.UserID
}
