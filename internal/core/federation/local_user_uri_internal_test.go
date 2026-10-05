package federation

import (
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/stretchr/testify/assert"
)

// #3330: localUserIDFromAPID reads a local user URI like upstream
// ApDbResolverService.getUserFromApId (parseUri): the segment right after
// `users`, ignoring the rest.
func TestLocalUserIDFromAPID(t *testing.T) {
	r := &Resolver{urls: activitypub.NewURLBuilder("https://example.com")}
	cases := []struct {
		uri  string
		want string
	}{
		{"https://example.com/users/bob", "bob"},
		{"https://example.com/users/bob/followers", "bob"},
		{"https://example.com/users/bob/", "bob"},
		{"https://example.com/notes/bob", ""},
		{"https://remote.example/users/bob", ""},
		// 本家の parseUri はホストで判定し、scheme は見ない。
		{"http://example.com/users/bob", "bob"},
		{"https://EXAMPLE.com/users/bob", "bob"},
		{"https://example.com/users", ""},
		{"https://example.com/users/", ""},
		{"https://example.com/", ""},
		// pathname はエスケープされたまま割る。
		{"https://example.com/users/a%2Fb/x", "a%2Fb"},
		{"::bad", ""},
	}
	for _, tc := range cases {
		t.Run(tc.uri, func(t *testing.T) {
			assert.Equal(t, tc.want, r.localUserIDFromAPID(tc.uri))
		})
	}
	t.Run("no URL builder", func(t *testing.T) {
		assert.Empty(t, (&Resolver{}).localUserIDFromAPID("https://example.com/users/bob"))
		assert.Empty(t, (&Resolver{}).ExtractLocalUserID("https://example.com/users/bob"))
		assert.Empty(t, (&Resolver{}).flagTargetUserID("https://example.com/users/bob"))
	})
}
