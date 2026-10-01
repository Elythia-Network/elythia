package webpush

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsValidEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		endpoint string
		want     bool
	}{
		{name: "https FCM style", endpoint: "https://fcm.googleapis.com/fcm/send/abc:def", want: true},
		{name: "https with port and query", endpoint: "https://push.example:8443/p/1?x=y", want: true},
		{name: "uppercase scheme is normalized", endpoint: "HTTPS://push.example/1", want: true},
		{name: "http is rejected", endpoint: "http://push.example/1", want: false},
		{name: "other scheme is rejected", endpoint: "ftp://push.example/1", want: false},
		{name: "relative path is rejected", endpoint: "/push/1", want: false},
		{name: "unparsable is rejected", endpoint: "https://push.example/%zz", want: false},
		{name: "username is rejected", endpoint: "https://user@push.example/1", want: false},
		{name: "username and password are rejected", endpoint: "https://user:pass@push.example/1", want: false},
		{name: "password only is rejected", endpoint: "https://:pass@push.example/1", want: false},
		{name: "empty userinfo is rejected", endpoint: "https://@push.example/1", want: false},
		{name: "opaque form is rejected", endpoint: "https:push.example/1", want: false},
		{name: "empty host is rejected", endpoint: "https:///1", want: false},
		{name: "port only host is rejected", endpoint: "https://:443/1", want: false},
		{name: "empty string is rejected", endpoint: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsValidEndpoint(tc.endpoint))
		})
	}
}
