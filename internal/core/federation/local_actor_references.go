package federation

import (
	"errors"
	"sort"
	"strings"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/localfederation"
	"github.com/elythia-network/elythia/internal/model"
)

// LocalActorReferenceRecorder records an externally visible reference to a
// local actor and reports whether it remained externally available.
type LocalActorReferenceRecorder func(user *model.User, reason string) (bool, error)

// FilterLocalActorReferences returns a copy of user with local alsoKnownAs and
// movedTo references omitted when their target is missing, moderated, or not
// externally available. Lookup and observation failures abort the operation.
// All targets are resolved before any reference is recorded.
func FilterLocalActorReferences(
	user *model.User,
	urls *activitypub.URLBuilder,
	resolve func(string) (*model.User, error),
	observer activitypub.LocalFederationObserver,
	reason string,
	record LocalActorReferenceRecorder,
) (*model.User, error) {
	if user == nil || urls == nil || observer == nil {
		return user, nil
	}

	copyUser := *user
	localIDs := make(map[string]struct{})
	collect := func(value *string, split bool) {
		if value == nil {
			return
		}
		values := []string{*value}
		if split {
			values = strings.Split(*value, ",")
		}
		for _, uri := range values {
			if id := urls.LocalUserIDFromURI(strings.TrimSpace(uri)); id != "" {
				localIDs[id] = struct{}{}
			}
		}
	}
	collect(user.AlsoKnownAs, true)
	collect(user.MovedToURI, false)

	ids := make([]string, 0, len(localIDs))
	for id := range localIDs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	targets := make(map[string]*model.User, len(ids))
	for _, id := range ids {
		target, err := resolve(id)
		if err != nil {
			return nil, err
		}
		targets[id] = target
	}

	keep := make(map[string]bool, len(ids))
	for _, id := range ids {
		target := targets[id]
		keep[id] = target != nil && target.IsLocal() && !target.IsSuspended && !target.IsDeleted && observer.IsExternallyAvailable(target)
	}
	for _, id := range ids {
		if !keep[id] {
			continue
		}
		target := targets[id]
		if target.FirstExternalReferenceAt != nil {
			continue
		}
		var available bool
		var err error
		if record != nil {
			available, err = record(target, reason)
		} else {
			_, err = observer.ObserveExternalReference(target.ID, reason, "")
			available = err == nil
		}
		if errors.Is(err, localfederation.ErrFederationDisabled) {
			keep[id] = false
			continue
		}
		if err != nil {
			return nil, err
		}
		keep[id] = available
	}

	filterURI := func(uri string) string {
		id := urls.LocalUserIDFromURI(uri)
		if id != "" && !keep[id] {
			return ""
		}
		return uri
	}
	if user.AlsoKnownAs != nil {
		aliases := strings.Split(*user.AlsoKnownAs, ",")
		kept := make([]string, 0, len(aliases))
		for _, uri := range aliases {
			uri = strings.TrimSpace(uri)
			if uri == "" {
				continue
			}
			if filtered := filterURI(uri); filtered != "" {
				kept = append(kept, filtered)
			}
		}
		copyUser.AlsoKnownAs = nil
		if len(kept) != 0 {
			aliases := strings.Join(kept, ",")
			copyUser.AlsoKnownAs = &aliases
		}
	}
	if user.MovedToURI != nil {
		filtered := filterURI(*user.MovedToURI)
		copyUser.MovedToURI = nil
		if filtered != "" {
			copyUser.MovedToURI = &filtered
		}
	}
	return &copyUser, nil
}
