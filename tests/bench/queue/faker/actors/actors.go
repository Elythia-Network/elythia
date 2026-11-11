// Package actors names the faker's sending actors for the queue bench (#4138).
//
// The faker sends inbound activities from many actors instead of one, so that
// the bench measures how fast a receiver processes them rather than how long
// writes to one actor's row wait for each other. Actor n is
// `https://<host>/users/bench-sender-<n>`, and every actor shares one key pair.
package actors

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

const (
	// PathPrefix is the path of actor n without n.
	PathPrefix = "/users/bench-sender-"
	// NamePrefix is the username of actor n without n.
	NamePrefix = "bench-sender-"
	// KeyIDFragment follows an actor URI to form its key id.
	KeyIDFragment = "#main-key"
	// Max bounds the actor numbers.
	Max = 100000
)

// Name returns the username of actor n.
func Name(n int) string {
	return NamePrefix + strconv.Itoa(n)
}

// URI returns the URI of actor n on host.
func URI(host string, n int) string {
	return "https://" + host + PathPrefix + strconv.Itoa(n)
}

// KeyID returns the key id of actor n on host.
func KeyID(host string, n int) string {
	return URI(host, n) + KeyIDFragment
}

// Number reads n out of a username "bench-sender-<n>". ok is false for any
// other name, including a non-canonical number such as "007".
func Number(name string) (n int, ok bool) {
	rest, found := strings.CutPrefix(name, NamePrefix)
	if !found {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 || n >= Max || strconv.Itoa(n) != rest {
		return 0, false
	}
	return n, true
}

// For returns the actor that sends activity index when count actors share the
// activities. A count below 1 means one actor.
func For(index, count int) int {
	if count < 1 {
		return 0
	}
	return index % count
}

// Document renders the ActivityPub actor document of actor n.
func Document(host string, n int, publicKeyPem string) []byte {
	uri := URI(host, n)
	// 文字列だけの map なので Marshal は失敗しない
	body, _ := json.Marshal(map[string]any{
		"@context": []any{
			"https://www.w3.org/ns/activitystreams",
			"https://w3id.org/security/v1",
		},
		"type":              "Person",
		"id":                uri,
		"preferredUsername": Name(n),
		"name":              "Queue Bench Sender " + strconv.Itoa(n),
		"inbox":             uri + "/inbox",
		"outbox":            uri + "/outbox",
		"publicKey": map[string]any{
			"id":           uri + KeyIDFragment,
			"owner":        uri,
			"publicKeyPem": publicKeyPem,
		},
	})
	return body
}

// Routes registers the actor documents under /users/ and their WebFinger
// lookups for host on mux. The path must end in a slash: ServeMux matches a
// pattern without one only exactly, so "/users/bench-sender-" would never
// reach the handler and every actor would be a 404 (#4138).
func Routes(mux *http.ServeMux, host, publicKeyPem string) {
	mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
		n, ok := Number(strings.TrimPrefix(r.URL.Path, "/users/"))
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/activity+json")
		_, _ = w.Write(Document(host, n, publicKeyPem))
	})
	mux.HandleFunc("/.well-known/webfinger", func(w http.ResponseWriter, r *http.Request) {
		resource := r.URL.Query().Get("resource")
		acct, isAcct := strings.CutPrefix(resource, "acct:")
		name, onHost := strings.CutSuffix(acct, "@"+host)
		n, ok := Number(name)
		if !isAcct || !onHost || !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/jrd+json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"subject": resource,
			"links": []map[string]any{
				{"rel": "self", "type": "application/activity+json", "href": URI(host, n)},
			},
		})
	})
}
