package admin

import (
	"context"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
)

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
		out[i] = h.entityPacker.PackUserDetailed(u, profiles[u.ID], h.idGen)
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
	d := h.entityPacker.PackUserDetailed(u, profile, h.idGen)
	entity.GateCountVisibility(&d, false, false, false)
	if h.listPacker != nil {
		h.listPacker.FillLites([]*model.User{u}, []*entity.UserLite{&d.UserLite})
	}
	if h.detailExtras != nil {
		h.detailExtras.FillDetailedExtras(ctx, nil, u, profile, &d)
	}
	return d
}
