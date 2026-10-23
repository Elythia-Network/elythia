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

// SetSilencedLookup wires the role-policy lookup used by PackUserDetailed to
// derive isSilenced. Pass nil to clear (used in tests).
//
// It changes only the default Packer (#3788); a Packer built by NewPacker
// keeps its own dependencies.
func SetSilencedLookup(l SilencedLookup) {
	updateDefaultPacker(func(p *Packer) { p.silenced = l })
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
