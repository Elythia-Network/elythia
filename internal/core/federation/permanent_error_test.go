package federation

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	corereaction "github.com/elythia-network/elythia/internal/core/reaction"
	"github.com/stretchr/testify/assert"
)

func TestIsPermanentSkipError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"status 404", &activitypub.StatusError{StatusCode: 404, Status: "404 Not Found"}, true},
		{"status 401", &activitypub.StatusError{StatusCode: 401, Status: "401 Unauthorized"}, true},
		{"status 403", &activitypub.StatusError{StatusCode: 403, Status: "403 Forbidden"}, true},
		{"status 410", &activitypub.StatusError{StatusCode: 410, Status: "410 Gone"}, true},
		{"status 500", &activitypub.StatusError{StatusCode: 500, Status: "500 Internal Server Error"}, false},
		{"status 502", &activitypub.StatusError{StatusCode: 502, Status: "502 Bad Gateway"}, false},
		{"status 503", &activitypub.StatusError{StatusCode: 503, Status: "503 Service Unavailable"}, false},
		{"status 530", &activitypub.StatusError{StatusCode: 530, Status: "530"}, false},
		{"ErrInvalidActor", ErrInvalidActor, true},
		{"ErrInvalidNote", ErrInvalidNote, true},
		{"ErrInvalidActor wrapped", fmt.Errorf("resolve: %w", ErrInvalidActor), true},
		{"ErrInvalidNote wrapped", fmt.Errorf("ingest: %w", ErrInvalidNote), true},
		{"ErrNoteNotVisible", corereaction.ErrNoteNotVisible, true},
		{"ErrNoteNotVisible wrapped", fmt.Errorf("create: %w", corereaction.ErrNoteNotVisible), true},
		{"ErrHostNotAllowed", ErrHostNotAllowed, true},
		{"ErrHostNotAllowed wrapped", fmt.Errorf("resolve: %w", ErrHostNotAllowed), true},
		{"status 404 wrapped", fmt.Errorf("fetch %s: %w", "url", &activitypub.StatusError{StatusCode: 404, Status: "404"}), true},
		{"status 500 wrapped", fmt.Errorf("fetch: %w", &activitypub.StatusError{StatusCode: 500, Status: "500"}), false},
		{"generic error", errors.New("dial failed"), false},
		{"net.OpError (transient)", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isPermanentSkipError(tt.err)
			assert.Equal(t, tt.want, got, "err=%v", tt.err)
		})
	}
}
