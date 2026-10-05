package entity

import (
	"sort"
	"sync"

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

// userRolesLookup is process-wide so PackUserLite / PackUserDetailed can
// resolve roles without changing their signature. Router wires this once at
// startup via SetUserRolesLookup. nil disables enrichment, which keeps
// existing tests that don't bother wiring the lookup green (= empty roles
// / badgeRoles array).
var (
	userRolesLookupMu sync.RWMutex
	userRolesLookup   UserRolesLookup
)

// SetUserRolesLookup wires the role lookup used by PackUserLite and
// PackUserDetailed to populate the `roles` / `badgeRoles` fields. Pass nil
// to clear (used in tests).
func SetUserRolesLookup(l UserRolesLookup) {
	userRolesLookupMu.Lock()
	userRolesLookup = l
	userRolesLookupMu.Unlock()
}

// showRemoteBadgesLookup resolves meta.showRoleBadgesOfRemoteUsers so
// packBadgeRoles can decide whether a remote user's badge roles are exposed
// (upstream `meta.showRoleBadgesOfRemoteUsers || user.host == null`、#1653)。
// Router wires this once at startup with a closure that reads the cached meta.
// nil leaves it false, which keeps the conservative local-only default for
// tests that don't wire it.
var (
	showRemoteBadgesMu     sync.RWMutex
	showRemoteBadgesLookup func() bool
)

// SetShowRemoteBadgesLookup wires the meta.showRoleBadgesOfRemoteUsers
// accessor used by packBadgeRoles (#1653)。Pass nil to clear (used in tests).
func SetShowRemoteBadgesLookup(fn func() bool) {
	showRemoteBadgesMu.Lock()
	showRemoteBadgesLookup = fn
	showRemoteBadgesMu.Unlock()
}

// resolveShowRemoteBadges reports whether remote users' badge roles should be
// exposed, falling back to false (= local-only) when the lookup is unwired.
func resolveShowRemoteBadges() bool {
	showRemoteBadgesMu.RLock()
	fn := showRemoteBadgesLookup
	showRemoteBadgesMu.RUnlock()
	if fn == nil {
		return false
	}
	return fn()
}

// instanceIconURLLookup resolves meta.iconUrl so IdenticonURL can apply
// upstream's local-system-account special case: a local user whose username
// contains '.' (relay.actor / instance.actor / proxy.actor) uses the instance
// icon as its avatarUrl instead of the identicon route (upstream
// getIdenticonUrl, UserEntityService.ts:386-391、#1781)。Router wires this once
// at startup with a closure reading the cached meta. nil / 未配線時は "" を返し
// identicon route に fallback する。
var (
	instanceIconURLMu     sync.RWMutex
	instanceIconURLLookup func() string
)

// SetInstanceIconURLLookup wires the meta.iconUrl accessor used by IdenticonURL
// for the local-system-account avatarUrl special case (#1781)。Pass nil to clear
// (used in tests).
func SetInstanceIconURLLookup(fn func() string) {
	instanceIconURLMu.Lock()
	instanceIconURLLookup = fn
	instanceIconURLMu.Unlock()
}

// resolveInstanceIconURL returns meta.iconUrl, or "" when the lookup is unwired
// or meta.iconUrl is unset.
func resolveInstanceIconURL() string {
	instanceIconURLMu.RLock()
	fn := instanceIconURLLookup
	instanceIconURLMu.RUnlock()
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
func resolveUserRolesSorted(userID string) []*model.Role {
	userRolesLookupMu.RLock()
	l := userRolesLookup
	userRolesLookupMu.RUnlock()
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
func packBadgeRoles(u *model.User) *[]any {
	if u == nil {
		return nil
	}
	if u.Host != nil && !resolveShowRemoteBadges() {
		// remote user は flag off なら badge を出さない (upstream: undefined)
		return nil
	}
	out := []any{}
	roles := resolveUserRolesSorted(u.ID)
	for _, r := range roles {
		if r == nil || !r.AsBadge || !r.IsPublic {
			continue
		}
		out = append(out, map[string]any{
			"name": r.Name,
			// role icon は admin 設定だが remote URL を指せるため、frontend が
			// <img src> へ直接載せる前に proxy 経由へ書き換えて IP 漏洩を防ぐ
			// (#1529)。local は no-op。
			"iconUrl":      ProxyMediaURLPtr(r.IconURL),
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
func packPublicRoles(u *model.User) []any {
	out := []any{}
	if u == nil {
		return out
	}
	roles := resolveUserRolesSorted(u.ID)
	for _, r := range roles {
		if r == nil || !r.IsPublic {
			continue
		}
		out = append(out, map[string]any{
			"id":              r.ID,
			"name":            r.Name,
			"color":           r.Color,
			"iconUrl":         ProxyMediaURLPtr(r.IconURL),
			"description":     r.Description,
			"isModerator":     r.IsModerator,
			"isAdministrator": r.IsAdministrator,
			"displayOrder":    r.DisplayOrder,
		})
	}
	return out
}
