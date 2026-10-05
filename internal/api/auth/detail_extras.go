package auth

import (
	"github.com/elythia-network/elythia/internal/core/userpack"
)

// SetDetailExtras wires the filler of pinnedNotes / pinnedPage / movedTo /
// alsoKnownAs for auth/session/userkey と miauth の check (#3330).
func (h *Handler) SetDetailExtras(x userpack.DetailExtras) {
	h.detailExtras = x
}

// HasDetailExtras reports whether the detail extras filler was wired.
//
// 未配線だと auth/session/userkey と miauth の check の利用者の pinnedNotes などが空、movedTo /
// alsoKnownAs が null のまま返る。起動時検査に使う。
func (h *Handler) HasDetailExtras() bool { return h.detailExtras != nil }
