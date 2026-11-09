package identicon

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestURL(t *testing.T) {
	tests := []struct {
		name, base, username, host, want string
	}{
		{"local user", "https://mkgo.local", "alice", "mkgo.local", "https://mkgo.local/identicon/alice@mkgo.local"},
		{"lowercases the username", "https://mkgo.local", "Alice", "mkgo.local", "https://mkgo.local/identicon/alice@mkgo.local"},
		{"remote user", "https://mkgo.local", "bob", "remote.example", "https://mkgo.local/identicon/bob@remote.example"},
		{"trailing slash of the base", "https://mkgo.local/", "alice", "mkgo.local", "https://mkgo.local/identicon/alice@mkgo.local"},
		{"host with a port", "http://127.0.0.1:3000", "alice", "127.0.0.1:3000", "http://127.0.0.1:3000/identicon/alice@127.0.0.1:3000"},
		{"no base gives a relative URL", "", "alice", "mkgo.local", "/identicon/alice@mkgo.local"},
		{"no host leaves out the host", "", "alice", "", "/identicon/alice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, URL(tt.base, tt.username, tt.host))
		})
	}
}
