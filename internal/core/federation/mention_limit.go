package federation

import (
	corenote "github.com/shiroha-a/mk/internal/core/note"
	"github.com/shiroha-a/mk/internal/model"
)

// RolePolicyProvider resolves a user's effective role policies. It has the
// same method set as role.PolicyProvider; *role.Service implements it.
type RolePolicyProvider interface {
	GetUserPolicies(userID string) map[string]any
}

// SetRolePolicyProvider wires the role policy lookup used for the mention
// limit of inbound notes (#3330). nil keeps corenote.DefaultMentionLimit.
func (r *Resolver) SetRolePolicyProvider(p RolePolicyProvider) {
	r.rolePolicyProvider = p
}

// HasRolePolicyProvider reports whether the role policy lookup was wired
// (critical wiring check).
func (r *Resolver) HasRolePolicyProvider() bool { return r.rolePolicyProvider != nil }

// mentionLimitFor returns the `mentionLimit` policy of the note author userID.
// 本家 NoteCreateService.create は投稿者がリモートでも
// roleService.getUserPolicies(user.id).mentionLimit で判定する。
func (r *Resolver) mentionLimitFor(userID string) int {
	if r.rolePolicyProvider == nil {
		return corenote.DefaultMentionLimit
	}
	return corenote.MentionLimitFor(r.rolePolicyProvider, userID)
}

// exceedsRemoteMentionLimit reports whether a remote note with the given
// resolved mentions and raw Mention tag hrefs is over limit.
//
// Mirrors upstream NoteCreateService.create for AP notes: the count is the
// larger of the distinct resolved users (`mentionedUsers` = the mentions, the
// reply target's author unless it is the note's author, and for a specified
// note its recipients) and the number of distinct Mention hrefs
// (`apMentionRawCount`, #17576), and the note is rejected when that count is
// positive and above limit. limit is called only when the count is positive,
// so a note without mentions never looks the author's policies up. n must
// carry ReplyUserID, Visibility and VisibleUserIDs.
func exceedsRemoteMentionLimit(n *model.Note, mentions, tagHrefs []string, limit func() int) bool {
	rawTagSet := make(map[string]struct{}, len(tagHrefs))
	for _, h := range tagHrefs {
		rawTagSet[h] = struct{}{}
	}
	mentionedUsers := make(map[string]struct{}, len(mentions))
	for _, id := range mentions {
		mentionedUsers[id] = struct{}{}
	}
	if n.ReplyUserID != nil && *n.ReplyUserID != "" && *n.ReplyUserID != n.UserID {
		mentionedUsers[*n.ReplyUserID] = struct{}{}
	}
	if n.Visibility == model.NoteVisibilitySpecified {
		for _, id := range n.VisibleUserIDs {
			mentionedUsers[id] = struct{}{}
		}
	}
	effective := max(len(mentionedUsers), len(rawTagSet))
	// 本家の条件は `effectiveMentionCount > 0 && effectiveMentionCount > mentionLimit`。
	// 以前は `limit > 0` を条件にしていたので、mentionLimit=0 (メンション禁止) の
	// ロールが当たっていても判定ごと飛ばしていた (ローカルの経路の checkMentionLimit
	// と同じ直し方)。
	//
	// 上限は数が 1 以上のときだけ引く (本家の短絡評価と同じ)。メンションの無い
	// note のたびに role policy を引くと、role.Service の利用者ごとのキャッシュに
	// 受信したノートの著者が際限なく積もる。
	return effective > 0 && effective > limit()
}

// exceedsMentionLimit is exceedsRemoteMentionLimit with the author's
// mentionLimit policy, looked up only when the note has mentions.
func (r *Resolver) exceedsMentionLimit(n *model.Note, mentions, tagHrefs []string) bool {
	return exceedsRemoteMentionLimit(n, mentions, tagHrefs, r.lazyMentionLimit(n.UserID))
}

// lazyMentionLimit returns a function that looks the mentionLimit policy of
// userID up on its first call and returns the same value afterwards.
//
// 取り込みの 1 回の中では、未知の actor の取得の上限と、取り込みの上限判定で
// 同じ値を使い、policy を 1 回だけ引く。呼ばれなければ引かない。
func (r *Resolver) lazyMentionLimit(userID string) func() int {
	loaded, limit := false, 0
	return func() int {
		if !loaded {
			limit, loaded = r.mentionLimitFor(userID), true
		}
		return limit
	}
}
