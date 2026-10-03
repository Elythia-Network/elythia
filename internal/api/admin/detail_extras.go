package admin

import (
	"github.com/shiroha-a/mk/internal/core/userpack"
)

// SetDetailExtras wires the filler of pinnedNotes / pinnedPage / movedTo /
// alsoKnownAs for admin/accounts/find-by-email (#3330).
func (h *Handler) SetDetailExtras(x userpack.DetailExtras) {
	h.detailExtras = x
}

// HasDetailExtras reports whether the detail extras filler was wired.
//
// 未配線だと admin/accounts/find-by-email の利用者の pinnedNotes などが空、movedTo /
// alsoKnownAs が null のまま返る。起動時検査に使う。
func (h *Handler) HasDetailExtras() bool { return h.detailExtras != nil }
