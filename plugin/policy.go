package plugin

import (
	"context"
	"fmt"
)

// ActiveRoleAssignment is one active manual role assignment paired with the
// role it grants.
//
// **conditional role は含まれない。** 条件つきロールには assignment row が
// 無いので、plugin が見えるのは手動で割り当てられた active なロールだけになる。
// [EffectivePolicyRequest.RoleIDs] は conditional を含むので、plugin は
// 「何の権限があるか」と「どの assignment に紐づくか」を別々に読む。
type ActiveRoleAssignment struct {
	// RoleID is the role the assignment grants. RoleIDs に必ず含まれる。
	RoleID string
	// AssignmentID is the native `role_assignment.id`。
	//
	// **不透明な ID として扱うこと。** plugin はこれを Plugin storage の行と
	// 対応させるだけで、内容を解釈しない。unassign → re-assign で別 ID に
	// なるので、assignment に紐づく状態は復活しない。
	AssignmentID string
}

// EffectivePolicyRequest is the input to an effective policy resolver.
type EffectivePolicyRequest struct {
	// UserID is empty when the user is anonymous.
	UserID string
	// RoleIDs are active role IDs, sorted without duplicates. Anonymous
	// requests receive a non-nil empty slice.
	RoleIDs []string
	// ActiveAssignments are the user's active manual role assignments, sorted
	// by RoleID. Each role appears at most once.
	//
	// - expired / deleted assignments are excluded
	// - conditional roles are never included (they have no assignment)
	// - anonymous requests receive a non-nil empty slice
	//
	// **RoleIDs を置き換えない。** 既存providerはこのsliceだけを見ている。
	// 追加contributionだけ返す実装には RoleIDs があれば十分。
	ActiveAssignments []ActiveRoleAssignment
}

// EffectivePolicyContribution is one policy key's contribution.
type EffectivePolicyContribution struct {
	Key        string
	Priority   int
	UseDefault bool
	Value      any
	// Order distinguishes multiple **additive** contributions for the same key and
	// controls their deterministic provider-local order. Each (Key, Order) pair must
	// be unique within one resolver result.
	//
	// **A replacement cannot use Order.** A contribution that sets
	// [EffectivePolicyContribution.ReplaceRoleID] must leave Order at 0, and
	// replacements of the same key are told apart by ReplaceRoleID instead: each
	// (Key, ReplaceRoleID) pair must be unique.
	Order int
	// ReplaceRoleID turns this contribution into a **replacement** of one active
	// manual role's native contribution for Key, instead of an additional
	// contribution.
	//
	// - "" (the default) keeps the existing additive behaviour.
	// - non-empty names a role that must appear in
	//   [EffectivePolicyRequest.ActiveAssignments]. Conditional roles are never
	//   there, so they cannot be replaced.
	//
	// **置換は 1 対 1。** 対象 role/key の native contribution だけを差し替え、priority は
	// **元 native entry の宣言値を引き継ぐ**。他 role の contribution と通常の
	// priority / type 集約はそのまま行う。
	//
	// **Priority と Order は 0 のまま渡すこと。** 置き換える native entry が持った priority
	// を引き継ぐので、plugin が選んでよいと二重定義になる。host は 0 以外を malformed
	// として provider 全体を失敗扱いにする。
	//
	// 同じ role/key を複数 provider が置換した場合は**競合**となり、host はその pair だけを
	// native contribution に戻す。**fallback は pair 単位で、他の role の contribution と
	// provider の通常 contribution はそのまま結果に残る**（結果が丸ごと native に戻ることは
	// ない）。checked 解決と unchecked 解決は**同じ結果**を返し、違うのは競合を error として
	// 報告するかどうかだけ。報告するのは checked 側だけで、競合ごとに固定の sentinel を返す。
	ReplaceRoleID string
}

// EffectivePolicyResolver computes effective policy contributions for a user.
type EffectivePolicyResolver func(context.Context, EffectivePolicyRequest) ([]EffectivePolicyContribution, error)

// EffectivePolicyRegistration declares an effective-policy provider.
type EffectivePolicyRegistration struct {
	Keys    []string
	Resolve EffectivePolicyResolver
}

// Validate reports whether the registration is usable.
func (r EffectivePolicyRegistration) Validate() error {
	if r.Resolve == nil {
		return fmt.Errorf("plugin: EffectivePolicies の Resolve が nil です")
	}
	if len(r.Keys) == 0 {
		return fmt.Errorf("plugin: EffectivePolicies の Keys が空です")
	}
	seen := make(map[string]bool, len(r.Keys))
	for _, key := range r.Keys {
		if key == "" {
			return fmt.Errorf("plugin: EffectivePolicies の Keys に空のキーが含まれています")
		}
		if seen[key] {
			return fmt.Errorf("plugin: EffectivePolicies の Keys に重複があります (%q)", key)
		}
		seen[key] = true
	}
	return nil
}

// EffectivePolicyInvalidator drops cached policy inputs and successful
// provider output after committed state changes.
type EffectivePolicyInvalidator interface {
	InvalidateUser(context.Context, string) error
	// InvalidateRole is intentionally broader than one role because conditional
	// role holders cannot be enumerated from assignments.
	InvalidateRole(context.Context, string) error
}
