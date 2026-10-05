package plugin_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/plugin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatusErrorUnkeyedLiteralSourceCompatibility(t *testing.T) {
	var err error = &plugin.StatusError{http.StatusTeapot, "legacy message"}
	var statusErr *plugin.StatusError
	require.True(t, errors.As(err, &statusErr))
	assert.Equal(t, http.StatusTeapot, statusErr.Status)
	assert.Equal(t, "legacy message", statusErr.Message)
}
