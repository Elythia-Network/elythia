package role

// PolicyProvider returns a user's effective role policies (defaults, meta
// overrides, and assigned roles). *Service implements it; tests and narrow
// adapters inject stubs without importing the full service surface.
//
// userID == "" yields base (anonymous) policies, mirroring upstream
// getUserPolicies(null). Callers gate features on individual keys
// (ltlAvailable, pinLimit, rateLimitFactor, etc.).
type PolicyProvider interface {
	GetUserPolicies(userID string) map[string]any
}
