package resetpassword

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/bgwork"
	miscsmtp "github.com/elythia-network/elythia/internal/misc/smtp"
	"github.com/elythia-network/elythia/internal/model"
)

// The reset mail starts in the group of the server generation (#3661): after
// the group has stopped, it is not sent.
func TestRequestReset_MailRunsInTheBackgroundGroup(t *testing.T) {
	h, userRepo, _ := newTestHandler()
	email := "test@example.com"
	userRepo.users["u1"] = &model.User{ID: "u1", Username: "testuser", UsernameLower: "testuser"}
	userRepo.profiles["u1"] = &model.UserProfile{UserID: "u1", Email: &email, EmailVerified: true}
	var sent atomic.Int32
	h.SetEmailSender(func(string, miscsmtp.Message) { sent.Add(1) })
	g := bgwork.New()
	h.SetBackground(g)
	assert.True(t, h.HasBackground())
	require.Zero(t, g.Stop(context.Background()))

	rec := post(h.RequestReset, `{"username":"testuser","email":"test@example.com"}`)
	require.Equal(t, http.StatusNoContent, rec.Code)
	time.Sleep(100 * time.Millisecond)
	assert.Zero(t, sent.Load(), "世代を止めた後にリセットのメールを送った")
	assert.EqualValues(t, 1, g.Dropped())
}
