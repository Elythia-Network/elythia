package admin_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/bgwork"
)

// admin/send-email sends in the group of the server generation (#3661):
// after the group has stopped, it does not start the send.
func TestSendEmail_RunsInTheBackgroundGroup(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	g := bgwork.New()
	h.SetBackgroundForTest(g)
	require.Zero(t, g.Stop(context.Background()))

	assert.Equal(t, http.StatusNoContent, doPost(h.SendEmail, `{"to":"a@example.com","subject":"s","text":"t"}`, adminUser).Code)
	assert.EqualValues(t, 1, g.Dropped(), "世代を止めた後に送信を始めた")
}
