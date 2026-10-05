package notesfilter

import "github.com/elythia-network/elythia/internal/model"

// HasSuspendedAuthor reports whether the note itself, its reply target, or its
// renote target was written by a suspended user.
//
// 取得経路が見ているのと**同じ 3 author** を対象にする
// (repository.applyTimelineFilter の NOT EXISTS 3 本 /
// core/timeline.ApplyFilter の suspended-user filter)。ここだけ対象が狭いと、
// 配信では通ったのに再取得で消える (またはその逆) という食い違いが出る。
//
// relation が未取得 (nil) の author は suspended ではないものとして扱う。
// SQL 側も `"renoteUserId" IS NULL OR NOT EXISTS (...)` で user 行が無ければ
// 通すので、削除済みユーザーの扱いが両経路で揃う。
func HasSuspendedAuthor(n *model.Note, author *model.User) bool {
	if n == nil {
		return false
	}
	if author != nil && author.IsSuspended {
		return true
	}
	return suspendedAuthorOf(n) || suspendedAuthorOf(n.Reply) || suspendedAuthorOf(n.Renote)
}

// suspendedAuthorOf reports whether the note carries a preloaded suspended author.
func suspendedAuthorOf(n *model.Note) bool {
	return n != nil && n.User != nil && n.User.IsSuspended
}
