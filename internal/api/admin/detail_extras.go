package admin

import (
	"context"

	"github.com/shiroha-a/mk/internal/core/userpack"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/model"
)

// SetDetailExtras wires the filler of pinnedNotes / pinnedPage / movedTo /
// alsoKnownAs for admin/accounts/find-by-email and admin/update-proxy-account
// (#3330).
func (h *Handler) SetDetailExtras(x userpack.DetailExtras) {
	h.detailExtras = x
}

// HasDetailExtras reports whether the detail extras filler was wired.
//
// 未配線だと admin/accounts/find-by-email と admin/update-proxy-account の利用者の
// pinnedNotes などが空、movedTo / alsoKnownAs が null のまま返る。起動時検査に使う。
func (h *Handler) HasDetailExtras() bool { return h.detailExtras != nil }

// SetListPacker wires the packer of the users embedded in admin/show-users,
// admin/roles/users, admin/abuse-user-reports and admin/show-moderation-logs,
// and of the instance / emojis of admin/accounts/find-by-email (#3330).
func (h *Handler) SetListPacker(p userpack.ListPacker) {
	h.listPacker = p
}

// HasListPacker reports whether the list packer was wired.
//
// 未配線だと admin の一覧の利用者にピン留め・移行先・instance・絵文字が載らず、
// モデレーター向けの項目や関係も欠ける。起動時検査に使う。
func (h *Handler) HasListPacker() bool { return h.listPacker != nil }

// packUsersMany packs users as upstream packMany(users, viewer, {schema:
// 'UserDetailed'}), in order. viewer is nil for upstream packMany(users, null).
//
// 配線が外れた構成では素の UserDetailed に倒す。閲覧者がいれば (admin の一覧は
// モデレーター以上しか呼べない) カウントを見せ、匿名ならゲートを通したままにする。
func (h *Handler) packUsersMany(ctx context.Context, viewer *model.User, users []*model.User, profiles map[string]*model.UserProfile) []entity.UserDetailed {
	if h.listPacker != nil {
		return h.listPacker.DetailedMany(ctx, viewer, users, profiles)
	}
	out := make([]entity.UserDetailed, len(users))
	for i, u := range users {
		out[i] = entity.PackUserDetailed(u, profiles[u.ID], h.idGen)
		entity.GateCountVisibility(&out[i], false, viewer != nil, false)
	}
	return out
}

// packUserAnonymous packs one user as upstream pack(user, null, {schema:
// 'UserDetailedNotMe'}): no moderator fields, counts gated as for an anonymous
// viewer, pins shown to the anonymous viewer (single pack fetches them).
//
// 本家 find-by-email は閲覧者を null で pack するので、呼んだ管理者には
// フォロワー限定のカウントも moderationNote も見えない (#3330)。
func (h *Handler) packUserAnonymous(ctx context.Context, u *model.User, profile *model.UserProfile) entity.UserDetailed {
	d := entity.PackUserDetailed(u, profile, h.idGen)
	entity.GateCountVisibility(&d, false, false, false)
	if h.listPacker != nil {
		h.listPacker.FillLites([]*model.User{u}, []*entity.UserLite{&d.UserLite})
	}
	if h.detailExtras != nil {
		h.detailExtras.FillDetailedExtras(ctx, nil, u, profile, &d)
	}
	return d
}
