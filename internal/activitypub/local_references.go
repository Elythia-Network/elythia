package activitypub

import (
	"encoding/json"
	"sort"
	"strings"
)

// semanticAPReferenceFields are ActivityPub fields whose values identify an
// actor, object, or embedded object. Arbitrary text fields (including content,
// summary, and profile properties) are deliberately never traversed.
var semanticAPReferenceFields = map[string]struct{}{
	"id": {}, "@id": {}, "actor": {}, "object": {}, "attributedTo": {},
	"to": {}, "cc": {}, "bto": {}, "bcc": {}, "audience": {}, "inReplyTo": {},
	"quoteUrl": {}, "_misskey_quote": {}, "quote": {}, "quoteUri": {}, "quoteAuthorization": {},
	"tag": {}, "href": {}, "alsoKnownAs": {}, "movedTo": {}, "target": {}, "instrument": {}, "result": {},
	"interactingObject": {}, "interactionTarget": {}, "items": {}, "orderedItems": {},
}

// ExtractLocalAPReferences finds canonical local actor and note URIs in
// ActivityPub reference fields only. It performs no lookups or federation
// state changes. IDs are unique and sorted for deterministic validation.
func ExtractLocalAPReferences(baseURL string, serialized string) (userIDs []string, noteIDs []string) {
	var document any
	if json.Unmarshal([]byte(serialized), &document) != nil {
		return nil, nil
	}
	users, notes := map[string]struct{}{}, map[string]struct{}{}
	var visit func(any)
	visit = func(value any) {
		switch value := value.(type) {
		case string:
			if id := canonicalLocalReferenceID(baseURL, "/users/", value); id != "" {
				users[id] = struct{}{}
			}
			if id := canonicalLocalReferenceID(baseURL, "/notes/", value); id != "" {
				notes[id] = struct{}{}
			}
		case []any:
			for _, item := range value {
				visit(item)
			}
		case map[string]any:
			for key, item := range value {
				if _, semantic := semanticAPReferenceFields[key]; semantic {
					visit(item)
				}
			}
		}
	}
	visit(document)
	for id := range users {
		userIDs = append(userIDs, id)
	}
	for id := range notes {
		noteIDs = append(noteIDs, id)
	}
	sort.Strings(userIDs)
	sort.Strings(noteIDs)
	return userIDs, noteIDs
}

func canonicalLocalReferenceID(baseURL, path, uri string) string {
	if baseURL == "" || strings.Contains(uri, "?") {
		return ""
	}
	// AP actor keys and quote request IDs may append a fragment to the same
	// canonical path. Query strings and path suffixes are never accepted.
	uri, _, _ = strings.Cut(uri, "#")
	prefix := baseURL + path
	if !strings.HasPrefix(uri, prefix) {
		return ""
	}
	id := strings.TrimPrefix(uri, prefix)
	if path == "/notes/" {
		parts := strings.Split(id, "/")
		switch {
		case len(parts) == 1:
			// Canonical Note URI.
		case len(parts) == 2 && parts[1] == "activity":
			id = parts[0]
		case len(parts) == 3 && parts[1] == "quote-authorizations" && parts[2] != "" && !strings.ContainsAny(parts[2], "?#%"):
			id = parts[0]
		default:
			return ""
		}
	}
	if id == "" || strings.ContainsAny(id, "/?#%") {
		return ""
	}
	return id
}
