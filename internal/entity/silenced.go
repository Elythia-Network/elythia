package entity

// SilencedLookup resolves a user's `isSilenced` state from role policies.
// Mirrors upstream UserEntityService.pack which sets
// `isSilenced: !(await roleService.getUserPolicies(user.id)).canPublicNote`.
//
// Implemented structurally by role.Service.IsSilenced(userID) bool, so the
// router can wire it directly without an adapter (keeps layering: entity never
// imports core/role).
type SilencedLookup interface {
	IsSilenced(userID string) bool
}

// resolveSilenced returns isSilenced for userID via the Packer's lookup,
// falling back to false when the lookup is unwired.
func (pkr *Packer) resolveSilenced(userID string) bool {
	l := pkr.get().silenced
	if l == nil {
		return false
	}
	return l.IsSilenced(userID)
}
