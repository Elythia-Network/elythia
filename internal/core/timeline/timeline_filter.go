package timeline

import (
	"strings"

	"github.com/elythia-network/elythia/internal/model"
)

// idInSet reports whether the (nilable) id is present in set.
func idInSet(set map[string]struct{}, id *string) bool {
	if id == nil {
		return false
	}
	_, ok := set[*id]
	return ok
}

// hostInSet reports whether the (nilable) host (case-insensitive) is in set.
// set のキーは lowercase 前提。
func hostInSet(set map[string]struct{}, host *string) bool {
	if host == nil {
		return false
	}
	_, ok := set[strings.ToLower(*host)]
	return ok
}

// TimelineFilter holds filtering options for timeline queries.
// *bool フィールドは nil のときデフォルト値として扱う。
type TimelineFilter struct {
	WithFiles             bool     // trueならファイル付きノートのみ
	WithRenotes           *bool    // nil=true。falseでpure renote除外
	WithReplies           *bool    // nil=false。local/hybridのみ
	IncludeMyRenotes      *bool    // nil=true。home/hybridのみ
	IncludeRenotedMyNotes *bool    // nil=true。home/hybridのみ
	IncludeLocalRenotes   *bool    // nil=true。home/hybridのみ
	AllowPartial          bool     // trueならRedis結果が不足でもDBフォールバックしない
	MutedChannelIDs       []string // 指定があれば channelId が一致するノートを除外
	// MutedUserIDs は viewer が mute した user の note を除外する filter
	// 用 (#874)。nil なら filter 無効、空 slice なら filter 有効だが除外
	// 対象なし。renote の場合は renoteUserId も check する (= upstream
	// Misskey TS の muting JOIN と同 semantics)。
	//
	// この in-memory ApplyFilter は Redis fanout 経路 (cached note ID から
	// 解決した note slice) で動作する。DB fallback path では同じ
	// MutedUserIDs が toDBFilter 経由で SQL の NOT IN として push-down
	// されるので、両経路で同 semantics に保たれる (#892)。
	MutedUserIDs []string
	// FollowingIDs は viewer がフォローしている user の集合。
	//
	// upstream の HTL は取得側の noteFilter で「返信先が followers 限定の投稿で、
	// その投稿者を自分がフォローしていない (かつ自分でもない)」ノートを弾く。
	// fanout 側にも同等のガードはあるが、DB fallback 経路には効かないので
	// 取得側でも同じ判定を通す。nil なら判定をスキップする (未配線)。
	FollowingIDs map[string]struct{}
	// RenoteMutedUserIDs は viewer が renote-mute した user の **pure renote**
	// だけを除外する filter (#903)。MutedUserIDs と異なり:
	//   - 投稿者の plain note (= text あり / file 付き / quote renote 含む) は
	//     **そのまま表示**
	//   - pure renote (= text なし / file なし / renoteId あり) のみ skip
	// upstream Misskey TS の generateMutedUserRelatedRenotesQuery と同 semantics。
	RenoteMutedUserIDs []string
	// BlockerIDs は viewer を block している user の id (被block、#1681)。
	// note/reply/renote のいずれかの author が含まれる note を除外する
	// (upstream generateBlockedUserQueryForNotes)。
	BlockerIDs []string
	// MutedInstances は viewer が mute した instance host (#1681、lowercase)。
	// note/reply/renote のいずれかの author host が一致する note を除外する。
	MutedInstances []string
	// FollowedChannelIDs は viewer がフォローしている channel の id (mute 済は
	// 除く、#1686)。home timeline でのみ使う。in-memory ApplyFilter では参照
	// しない (Redis の home list は fanout が channel-follower の home へ push
	// するため既に正しい)。DB fallback の ListHomeTimeline が
	// `channelId IN (...)` で followed channel の note を home に含める。
	FollowedChannelIDs []string
	// LocalUsersOnly drops notes whose own author is remote. The handler sets
	// it for anonymous visitors under meta.ugcVisibilityForVisitor=local
	// (upstream FanoutTimelineEndpointService / generateUgcVisibilityQueryForVisitor).
	// Reply / renote targets attached to a local note are kept, as upstream does.
	LocalUsersOnly bool
}

// boolDefault returns *b if non-nil, else def.
func boolDefault(b *bool, def bool) bool {
	if b != nil {
		return *b
	}
	return def
}

// isPureRenote returns true if the note is a pure renote (renote with no
// text/cw/files/poll/reply)。upstream isPureRenote (= !isQuote) と揃える。quote
// (text/cw/files/poll/replyId のいずれかを伴う renote) は pure renote ではない (#1886)。
func isPureRenote(n *model.Note) bool {
	return n.RenoteID != nil &&
		(n.Text == nil || *n.Text == "") &&
		(n.CW == nil || *n.CW == "") &&
		!n.HasPoll &&
		len(n.FileIDs) == 0 &&
		(n.ReplyID == nil || *n.ReplyID == "")
}

// userSuspended reports whether the (nilable) preloaded user is suspended.
// relation が未取得 (nil) なら suspended ではないものとして扱う (SQL 側の
// `replyUser.id IS NULL` 分岐と同じく pass-through)。
func userSuspended(u *model.User) bool {
	return u != nil && u.IsSuspended
}

// ApplyFilter filters notes in-memory according to the given TimelineFilter.
// viewerID is the currently authenticated user's ID (empty string if anonymous).
func ApplyFilter(notes []*model.Note, viewerID string, f TimelineFilter) []*model.Note {
	withRenotes := boolDefault(f.WithRenotes, true)
	// withReplies は in-memory filter では参照しない (#1047)。upstream Misskey
	// TS の Home TL Redis cache 経路 (`fanoutTimelineEndpointService.timeline`)
	// と同じく、cache に乗っている note は filter なしで pass-through する。
	// reply の表示制御は fanout 側で `following.withReplies` を見て push を
	// 制御するため、cache に乗る note 集合自体が既に正しい。
	// DB fallback (= cache miss) では repo.applyTimelineFilter で
	// `replyUserId = note.userId` (self-thread のみ残す) upstream 互換 filter
	// が走る。
	includeMyRenotes := boolDefault(f.IncludeMyRenotes, true)
	includeRenotedMyNotes := boolDefault(f.IncludeRenotedMyNotes, true)
	includeLocalRenotes := boolDefault(f.IncludeLocalRenotes, true)

	// 返信先が followers 限定なら、その投稿者をフォローしているか (または自分の
	// 投稿か) を確認する。upstream timeline.ts の noteFilter と同じ判定。
	hideFollowersOnlyReply := func(n *model.Note) bool {
		if f.FollowingIDs == nil || n.Reply == nil {
			return false
		}
		if n.Reply.Visibility != model.NoteVisibilityFollowers {
			return false
		}
		if n.Reply.UserID == viewerID {
			return false
		}
		_, following := f.FollowingIDs[n.Reply.UserID]
		return !following
	}

	var mutedChannels map[string]struct{}
	if len(f.MutedChannelIDs) > 0 {
		mutedChannels = make(map[string]struct{}, len(f.MutedChannelIDs))
		for _, id := range f.MutedChannelIDs {
			mutedChannels[id] = struct{}{}
		}
	}

	var mutedUsers map[string]struct{}
	if len(f.MutedUserIDs) > 0 {
		mutedUsers = make(map[string]struct{}, len(f.MutedUserIDs))
		for _, id := range f.MutedUserIDs {
			mutedUsers[id] = struct{}{}
		}
	}

	var renoteMutedUsers map[string]struct{}
	if len(f.RenoteMutedUserIDs) > 0 {
		renoteMutedUsers = make(map[string]struct{}, len(f.RenoteMutedUserIDs))
		for _, id := range f.RenoteMutedUserIDs {
			renoteMutedUsers[id] = struct{}{}
		}
	}

	// 被block (#1681): viewer を block している user が note/reply/renote の
	// author なら除外。
	var blockers map[string]struct{}
	if len(f.BlockerIDs) > 0 {
		blockers = make(map[string]struct{}, len(f.BlockerIDs))
		for _, id := range f.BlockerIDs {
			blockers[id] = struct{}{}
		}
	}
	// instance-mute (#1681): viewer が mute した instance の note/reply/renote
	// author host を除外。host は lowercase で突合する。
	var mutedInstances map[string]struct{}
	if len(f.MutedInstances) > 0 {
		mutedInstances = make(map[string]struct{}, len(f.MutedInstances))
		for _, h := range f.MutedInstances {
			mutedInstances[strings.ToLower(h)] = struct{}{}
		}
	}

	out := make([]*model.Note, 0, len(notes))
	for _, n := range notes {
		if f.LocalUsersOnly && n.UserHost != nil {
			continue
		}
		if f.WithFiles && len(n.FileIDs) == 0 {
			continue
		}
		if hideFollowersOnlyReply(n) {
			continue
		}
		if !withRenotes && isPureRenote(n) {
			continue
		}
		if mutedChannels != nil && n.ChannelID != nil {
			if _, muted := mutedChannels[*n.ChannelID]; muted {
				continue
			}
		}
		// suspended-user filter (#1686): note/reply/renote のいずれかの author が
		// suspended なら除外する (upstream generateSuspendedUserQueryForNote)。
		// FindManyByIDsWithUser が note/sub-note の User relation を配線するため、
		// reply/renote 先の author suspended も判定できる。SQL 側の
		// applyTimelineFilter にも同等の NOT EXISTS があり 2 経路で一致する。
		//
		// **relation が引けないときの限界 (#2624)**: sub-note が nil だと判定を
		// 飛ばす。SQL 側は `renoteUserId` 列を直接見るので、「renote 先の note
		// だけ削除され、その author は suspended のまま残っている」ケースでだけ
		// 結果が食い違う。両者を厳密に揃えるには viewer ごとの timeline read
		// (hot path) で suspended 判定用のクエリを 1 本増やす必要があり、この
		// 稀なケースには見合わない。
		//
		// なお streaming 側は publish 時に同じ 3 author を見て止める
		// (internal/stream の suspended-author gate)。**Redis の list には積んだ
		// うえで配信だけを落とす**設計なので、凍結を解除すればこのフィルタが
		// 判断して普通に出るようになる。
		if userSuspended(n.User) ||
			(n.Reply != nil && userSuspended(n.Reply.User)) ||
			(n.Renote != nil && userSuspended(n.Renote.User)) {
			continue
		}
		// user mute filter: 投稿者が muted user なら除外。renote / reply の
		// author も check する (= upstream generateMutedUserQueryForNotes は
		// note/reply/renote の 3 author を見る、#874 / reply は #1681 で追加)。
		if mutedUsers != nil {
			if _, muted := mutedUsers[n.UserID]; muted {
				continue
			}
			if n.RenoteUserID != nil {
				if _, muted := mutedUsers[*n.RenoteUserID]; muted {
					continue
				}
			}
			if n.ReplyUserID != nil {
				if _, muted := mutedUsers[*n.ReplyUserID]; muted {
					continue
				}
			}
		}
		// 被block (#1681): note/reply/renote のいずれかの author が viewer を
		// block していれば除外。
		if blockers != nil && (idInSet(blockers, &n.UserID) || idInSet(blockers, n.ReplyUserID) || idInSet(blockers, n.RenoteUserID)) {
			continue
		}
		// instance-mute (#1681): note/reply/renote のいずれかの author host が
		// muted instance なら除外。
		if mutedInstances != nil && (hostInSet(mutedInstances, n.UserHost) || hostInSet(mutedInstances, n.ReplyUserHost) || hostInSet(mutedInstances, n.RenoteUserHost)) {
			continue
		}
		// renote-mute filter: 投稿者が renote-muted で **かつ** pure renote
		// の場合のみ除外する (= upstream の generateMutedUserRelatedRenotesQuery
		// と同 semantics、#903)。投稿者の plain note / quote renote はそのまま
		// 通す。renote 元の userId は対象外 (= renote-mute は renoter 視点)。
		if renoteMutedUsers != nil && isPureRenote(n) {
			if _, muted := renoteMutedUsers[n.UserID]; muted {
				continue
			}
		}
		// withReplies=false 時の Redis cache 経路は filter なし pass-through
		// (#1047)。upstream Misskey TS の Home TL と同じく、cache に乗る note
		// 集合は fanout 側で既に正しい (= follower の following.withReplies
		// 設定で push 制御済) ので、in-memory で再 filter しない。
		if isPureRenote(n) {
			// includeMyRenotes=false: 自分がした pure renote を除外
			if !includeMyRenotes && viewerID != "" && n.UserID == viewerID {
				continue
			}
			// includeRenotedMyNotes=false: 自分のノートの pure renote を除外
			if !includeRenotedMyNotes && viewerID != "" && n.RenoteUserID != nil && *n.RenoteUserID == viewerID {
				continue
			}
			// includeLocalRenotes=false: ローカルユーザーの pure renote を除外
			if !includeLocalRenotes && n.RenoteUserHost == nil {
				continue
			}
		}
		out = append(out, n)
	}
	return out
}
