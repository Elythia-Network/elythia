package activitypub

import "github.com/elythia-network/elythia/internal/model"

// LocalFederationObserver provides the account-level availability check and
// irreversible first-reference recording used at ActivityPub boundaries.
type LocalFederationObserver interface {
	IsExternallyAvailable(user *model.User) bool
	ObserveExternalReference(userID, reason, host string) (bool, error)
}
