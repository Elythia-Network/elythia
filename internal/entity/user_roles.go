package entity

import (
	"sort"

	"github.com/elythia-network/elythia/internal/model"
)

// UserRolesLookup resolves the roles currently assigned to a user. Mirrors
// upstream Misskey TS `RoleService.getUserRoles`:
//
//	this.roleService.getUserRoles(user.id)
//	  .then(roles => roles.filter(role => role.isPublic).sort(...).map(...))
//
// Returns nil when the lookup isn't wired (router not configured) or the
// user has no role assignments; callers then render an empty `roles` /
// `badgeRoles` array (= upstream behavior for users with no roles).
type UserRolesLookup interface {
	LookupUserRoles(userID string) []*model.Role
}

// SetUserRolesLookup wires the role lookup used by PackUserLite and
// PackUserDetailed to populate the `roles` / `badgeRoles` fields. Pass nil
// to clear (used in tests).
//
// It changes only the default Packer (#3788); a Packer built by NewPacker
// keeps its own dependencies.
func SetUserRolesLookup(l UserRolesLookup) {
	updateDefaultPacker(func(p *Packer) { p.userRoles = l })
}

// SetShowRemoteBadgesLookup wires the meta.showRoleBadgesOfRemoteUsers
// accessor used by packBadgeRoles (#1653)。Pass nil to clear (used in tests).
//
// It changes only the default Packer (#3788); a Packer built by NewPacker
// keeps its own dependencies.
func SetShowRemoteBadgesLookup(fn func() bool) {
	updateDefaultPacker(func(p *Packer) { p.showRemoteBadges = fn })
}

// resolveShowRemoteBadges reports whether remote users' badge roles should be
// exposed, falling back to false (= local-only) when the lookup is unwired.
func (pkr *Packer) resolveShowRemoteBadges() bool {
	fn := pkr.get().showRemoteBadges
	if fn == nil {
		return false
	}
	return fn()
}

// SetInstanceIconURLLookup wires the meta.iconUrl accessor used by IdenticonURL
// for the local-system-account avatarUrl special case (#1781)。Pass nil to clear
// (used in tests).
//
// It changes only the default Packer (#3788); a Packer built by NewPacker
// keeps its own dependencies.
func SetInstanceIconURLLookup(fn func() string) {
	updateDefaultPacker(func(p *Packer) { p.instanceIconURL = fn })
}

// resolveInstanceIconURL returns meta.iconUrl, or "" when the lookup is unwired
// or meta.iconUrl is unset.
func (pkr *Packer) resolveInstanceIconURL() string {
	fn := pkr.get().instanceIconURL
	if fn == nil {
		return ""
	}
	return fn()
}

// resolveUserRolesSorted returns the user's assigned roles sorted by
// displayOrder descending (= upstream sort key). Returns a fresh slice so
// callers can iterate / filter freely without mutating the role service's
// per-user cache (#761).
//
// nil is returned when the lookup isn't wired or the user has no roles.
func (pkr *Packer) resolveUserRolesSorted(userID string) []*model.Role {
	l := pkr.get().userRoles
	if l == nil {
		return nil
	}
	src := l.LookupUserRoles(userID)
	if len(src) == 0 {
		return nil
	}
	// 共有 cache を mutate しないよう copy してから sort する。upstream
	// は b.displayOrder - a.displayOrder (= descending) でソートしている。
	out := make([]*model.Role, len(src))
	copy(out, src)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].DisplayOrder > out[j].DisplayOrder
	})
	return out
}

// packBadgeRoles returns the user's badge roles ready for JSON serialisation
// as the `badgeRoles` field of a UserLite. Filter mirrors upstream
// `r.asBadge && r.isPublic`.
//
// #2106 L11/L53 (documented limitation): upstream は `r.isPublic || iAmModerator` で
// filter し moderator は非公開 badge も見られるが、mk-go は PackUserLite に viewer context
// が無いため常に public のみで filter する。viewer/iAmModerator を全 pack 経路に thread する
// 大改修が必要なため現状は documented limitation として維持する (一般ユーザー視点の出力は
// upstream と一致、moderator が他者の非公開 badge を見る軽微なケースのみ乖離)。
//
// upstream の `(meta.showRoleBadgesOfRemoteUsers || user.host == null)` 条件を
// 再現する (#1653)。local user (host==nil) は常に pack し、remote user は
// meta.showRoleBadgesOfRemoteUsers が on のときだけ pack する。
//
// 戻り値で「省略 (nil)」と「空配列 present (&[]any{})」を区別する:
//   - local user: 常に non-nil (badge が無くても &[]any{} = JSON `[]`)
//   - remote + flag off: nil (= upstream undefined、JSON では omitempty で省略)
//   - remote + flag on: badge を pack した non-nil slice
//
// BadgeRoles は *[]any + omitempty なので nil → 省略、non-nil → present。
func (pkr *Packer) packBadgeRoles(u *model.User) *[]any {
	if u == nil {
		return nil
	}
	if u.Host != nil && !pkr.resolveShowRemoteBadges() {
		// remote user は flag off なら badge を出さない (upstream: undefined)
		return nil
	}
	out := []any{}
	roles := pkr.resolveUserRolesSorted(u.ID)
	for _, r := range roles {
		if r == nil || !r.AsBadge || !r.IsPublic {
			continue
		}
		out = append(out, map[string]any{
			"name": r.Name,
			// role icon は admin 設定だが remote URL を指せるため、frontend が
			// <img src> へ直接載せる前に proxy 経由へ書き換えて IP 漏洩を防ぐ
			// (#1529)。local は no-op。
			"iconUrl":      pkr.ProxyMediaURLPtr(r.IconURL),
			"displayOrder": r.DisplayOrder,
		})
	}
	return &out
}

// packPublicRoles returns the user's public roles ready for JSON
// serialisation as the `roles` field of a UserDetailed. Filter mirrors
// upstream `roles.filter(role => role.isPublic)`.
//
// Always returns a non-nil slice ([]any{}) so the JSON output is `[]` not
// `null`.
func (pkr *Packer) packPublicRoles(u *model.User) []any {
	out := []any{}
	if u == nil {
		return out
	}
	roles := pkr.resolveUserRolesSorted(u.ID)
	for _, r := range roles {
		if r == nil || !r.IsPublic {
			continue
		}
		out = append(out, map[string]any{
			"id":              r.ID,
			"name":            r.Name,
			"color":           r.Color,
			"iconUrl":         pkr.ProxyMediaURLPtr(r.IconURL),
			"description":     r.Description,
			"isModerator":     r.IsModerator,
			"isAdministrator": r.IsAdministrator,
			"displayOrder":    r.DisplayOrder,
		})
	}
	return out
}
