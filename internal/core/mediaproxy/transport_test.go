package mediaproxy

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestService_SetTransportKeepsClientTimeout(t *testing.T) {
	s := testService(nil)
	timeout := s.httpClient.Timeout
	rt := NewSSRFSafeTransport(testAllowedCIDRs)

	s.SetTransport(rt)

	assert.Same(t, http.RoundTripper(rt), s.httpClient.Transport)
	assert.Equal(t, timeout, s.httpClient.Timeout)
	assert.Positive(t, s.httpClient.Timeout)
}
