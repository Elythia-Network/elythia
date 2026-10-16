package notesfilter

import "github.com/elythia-network/elythia/internal/model"

// IsUserRelated ports upstream misc/is-user-related.ts. It reports whether the
// note's author, reply-target author, or renote-target author is in set.
//
// With ignoreAuthor the author itself is not checked. The reply / renote
// target author is checked only when it differs from the note's author, in
// both modes (a self-reply or self-renote never matches through the target).
// Only depth 1 is inspected: the renote target's own reply / renote is not.
func IsUserRelated(n *model.Note, set map[string]struct{}, ignoreAuthor bool) bool {
	if n == nil || len(set) == 0 {
		return false
	}
	if !ignoreAuthor {
		if _, hit := set[n.UserID]; hit {
			return true
		}
	}
	if uid := relatedUserID(n.ReplyUserID, n.Reply); uid != "" && uid != n.UserID {
		if _, hit := set[uid]; hit {
			return true
		}
	}
	if uid := relatedUserID(n.RenoteUserID, n.Renote); uid != "" && uid != n.UserID {
		if _, hit := set[uid]; hit {
			return true
		}
	}
	return false
}

// relatedUserID mirrors upstream `note.replyUserId ?? note.reply?.userId`.
func relatedUserID(col *string, embed *model.Note) string {
	if col != nil {
		return *col
	}
	if embed != nil {
		return embed.UserID
	}
	return ""
}

// DropUserRelated returns the notes for which IsUserRelated(n, set,
// ignoreAuthor) is false. An empty set returns notes unchanged.
func DropUserRelated(notes []*model.Note, set map[string]struct{}, ignoreAuthor bool) []*model.Note {
	if len(notes) == 0 || len(set) == 0 {
		return notes
	}
	out := make([]*model.Note, 0, len(notes))
	for _, n := range notes {
		if n == nil || IsUserRelated(n, set, ignoreAuthor) {
			continue
		}
		out = append(out, n)
	}
	return out
}
