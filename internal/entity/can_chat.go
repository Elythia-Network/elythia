package entity

// CanChatLookup resolves a user's effective canChat status by consulting
// role policies. Mirrors upstream Misskey TS `UserEntityService.pack`:
//
//	canChat: this.roleService.getUserPolicies(user.id)
//	  .then(r => r.chatAvailability === 'available')
//
// Returns ok=false when the lookup isn't wired (router not configured), or
// when policy data isn't available for this user; the caller then falls
// back to the default true (matches DefaultPolicies' "available").
type CanChatLookup interface {
	LookupCanChat(userID string) (canChat bool, ok bool)
}

// resolveCanChat returns the canChat value for userID via the Packer's
// lookup, falling back to true (= upstream default `chatAvailability ===
// "available"`) when lookup is unwired or returns ok=false.
//
// upstream の lookup は async だが mk-go の role.Service は in-memory
// cache (#761) を持つ pure sync function なので、entity 層から直接
// 呼んで OK (DB 負荷は加わらない)。
func (pkr *Packer) resolveCanChat(userID string) bool {
	l := pkr.get().canChat
	if l == nil {
		return true
	}
	val, ok := l.LookupCanChat(userID)
	if !ok {
		return true
	}
	return val
}
