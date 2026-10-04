package federation

import (
	"testing"

	"github.com/shiroha-a/mk/internal/activitypub"
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
	}
	for _, tc := range cases {
		t.Run(tc.uri, func(t *testing.T) {
			assert.Equal(t, tc.want, r.localUserIDFromAPID(tc.uri))
		})
	}
	t.Run("no URL builder", func(t *testing.T) {
		assert.Empty(t, (&Resolver{}).localUserIDFromAPID("https://example.com/users/bob"))
		assert.Empty(t, (&Resolver{}).ExtractLocalUserID("https://example.com/users/bob"))
	})
}
