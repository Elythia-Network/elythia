package actors

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNamesAndURIs(t *testing.T) {
	assert.Equal(t, "bench-sender-7", Name(7))
	assert.Equal(t, "https://faker/users/bench-sender-7", URI("faker", 7))
	assert.Equal(t, "https://faker/users/bench-sender-7#main-key", KeyID("faker", 7))
}

func TestNumber(t *testing.T) {
	for _, tc := range []struct {
		name string
		want int
		ok   bool
	}{
		{"bench-sender-0", 0, true},
		{"bench-sender-999", 999, true},
		{"bench-sender-99999", 99999, true},
		{"bench-sender-100000", 0, false},
		{"bench-sender-007", 0, false},
		{"bench-sender--1", 0, false},
		{"bench-sender-", 0, false},
		{"bench-sender-x", 0, false},
		{"bench-sender", 0, false},
		{"other-1", 0, false},
	} {
		n, ok := Number(tc.name)
		assert.Equal(t, tc.ok, ok, tc.name)
		assert.Equal(t, tc.want, n, tc.name)
	}
}

func TestForSpreadsActivitiesOverActors(t *testing.T) {
	counts := map[int]int{}
	for i := range 10000 {
		counts[For(i, 1000)]++
	}
	require.Len(t, counts, 1000)
	for n, c := range counts {
		assert.Equal(t, 10, c, "actor %d", n)
	}
	assert.Equal(t, 0, For(5, 1))
	assert.Equal(t, 0, For(5, 0), "a count below 1 means one actor")
}

func TestDocument(t *testing.T) {
	var doc map[string]any
	require.NoError(t, json.Unmarshal(Document("faker", 3, "PEM"), &doc))
	assert.Equal(t, "https://faker/users/bench-sender-3", doc["id"])
	assert.Equal(t, "bench-sender-3", doc["preferredUsername"])
	assert.Equal(t, "https://faker/users/bench-sender-3/inbox", doc["inbox"])
	key := doc["publicKey"].(map[string]any)
	assert.Equal(t, "https://faker/users/bench-sender-3#main-key", key["id"])
	assert.Equal(t, "https://faker/users/bench-sender-3", key["owner"])
	assert.Equal(t, "PEM", key["publicKeyPem"])
}

// get serves one request through the routes the faker registers.
func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	Routes(mux, "faker", "PEM")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHandlerServesEveryActorDocument(t *testing.T) {
	for _, n := range []int{0, 3, 999} {
		rec := get(t, "/users/"+Name(n))
		require.Equal(t, http.StatusOK, rec.Code, "actor %d", n)
		assert.Equal(t, "application/activity+json", rec.Header().Get("Content-Type"))
		var doc map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
		assert.Equal(t, URI("faker", n), doc["id"])
	}
	for _, path := range []string{"/users/bench-sender", "/users/bench-sender-007", "/users/other", "/users/" + Name(Max)} {
		assert.Equal(t, http.StatusNotFound, get(t, path).Code, path)
	}
}

func TestHandlerAnswersWebFinger(t *testing.T) {
	rec := get(t, "/.well-known/webfinger?resource=acct:bench-sender-5@faker")
	require.Equal(t, http.StatusOK, rec.Code)
	var jrd struct {
		Subject string `json:"subject"`
		Links   []struct {
			Href string `json:"href"`
		} `json:"links"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &jrd))
	assert.Equal(t, "acct:bench-sender-5@faker", jrd.Subject)
	require.Len(t, jrd.Links, 1)
	assert.Equal(t, URI("faker", 5), jrd.Links[0].Href)

	for _, q := range []string{"bench-sender-5@faker", "acct:bench-sender-5@other", "acct:someone@faker"} {
		assert.Equal(t, http.StatusNotFound, get(t, "/.well-known/webfinger?resource="+q).Code, q)
	}
}
