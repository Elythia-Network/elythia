package pages

import (
	"context"

	"github.com/elythia-network/elythia/internal/api/meself"
	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
)

// UserPacker packs target as UserDetailed seen by viewer, the way upstream
// UserEntityService.pack(target, viewer) does. *userpack.Packer satisfies it.
type UserPacker interface {
	DetailedNotMe(ctx context.Context, target, viewer *model.User) (entity.UserDetailed, bool)
}

// SetUserPacker wires the packer of the user embedded in the page-push
// pageEvent (#3330).
func (h *Handler) SetUserPacker(p UserPacker) {
	h.userPacker = p
}

// HasUserPacker reports whether the user packer was wired.
//
// 未配線だと page-push の pageEvent の利用者に、持ち主から見た関係・カウント・
// ピン留め・移行先が載らない。起動時検査に使う。
func (h *Handler) HasUserPacker() bool { return h.userPacker != nil }

// packPusher packs the page-push caller as upstream
// pack(me.id, {id: page.userId}, {schema: 'UserDetailed'}) does: the page
// owner is the viewer. When the caller owns the page the result is MeDetailed
// (isMe). It returns false when the caller's profile cannot be loaded.
//
// 本家は閲覧者を持ち主にするので、関係 (isFollowing など)・カウントのゲート・
// 持ち主がモデレーターのときの項目・ピン留め・移行先は持ち主から見た値になる。
// 自分のページなら isMe なので MeDetailed になる (#3330)。
func (h *Handler) packPusher(ctx context.Context, pusher *coreuser.UserWithProfile, ownerID string) (any, bool) {
	if h.userPacker == nil {
		// 配線が外れた構成では素の UserDetailed (packer の既定で非公開のカウントは伏せる)。
		return entity.PackUserDetailed(pusher.User, pusher.Profile, h.idGen), true
	}
	owner := pusher.User
	if ownerID != pusher.User.ID {
		// 本家は {id: page.userId} だけを閲覧者に渡す。持ち主の行が引けないときも
		// ID だけで組む。
		owner = &model.User{ID: ownerID}
		if ob, err := h.userSource.ShowByID(ownerID); err == nil && ob != nil && ob.User != nil {
			owner = ob.User
		}
	}
	d, ok := h.userPacker.DetailedNotMe(ctx, pusher.User, owner)
	if !ok {
		return nil, false
	}
	if owner.ID == pusher.User.ID {
		return meself.PackMe(ctx, d, pusher.User, pusher.Profile), true
	}
	return d, true
}
