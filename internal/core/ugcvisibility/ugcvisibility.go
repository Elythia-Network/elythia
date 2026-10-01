// Package ugcvisibility implements meta.ugcVisibilityForVisitor, the policy
// that limits which user-generated content anonymous visitors can read.
//
// The policy only applies to visitors (no signed-in user). Callers check the
// viewer themselves and consult these helpers only when there is none.
package ugcvisibility

// Policy values of meta.ugcVisibilityForVisitor.
const (
	All   = "all"
	Local = "local"
	None  = "none"
)

// HidesAll reports whether a visitor gets no content at all under policy.
// Mirrors upstream's `ugcVisibilityForVisitor === 'none'` checks.
func HidesAll(policy string) bool {
	return policy == None
}

// HidesNote reports whether a visitor must not see a note whose author is on
// userHost (nil for a local author) under policy. Mirrors upstream
// QueryService.generateUgcVisibilityQueryForVisitor (`none` → nothing,
// `local` → `note.userHost IS NULL`).
//
// 未知の値 (空文字を含む) は upstream と同じく `all` として扱う。設定を引けない
// ときに `local` へ倒すのは、値を渡す側 (server.metaUGCVisibility) の責務。
func HidesNote(policy string, userHost *string) bool {
	return policy == None || (policy == Local && userHost != nil)
}
