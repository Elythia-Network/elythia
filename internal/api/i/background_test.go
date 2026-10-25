package i

import (
	"context"
	"errors"
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

type countingRoundTripper struct{ n atomic.Int32 }

func (c *countingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, errors.New("no network in tests")
}

func stoppedGroup(t *testing.T, h *Handler) *bgwork.Group {
	t.Helper()
	g := bgwork.New()
	h.background = g
	require.Zero(t, g.Stop(context.Background()))
	return g
}

// The rel=me check of i/update starts in the group of the server generation
// (#3661): after the group has stopped, it does not run.
func TestUpdate_VerifyLinksRunsInTheBackgroundGroup(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := updateUser(repo)
	rt := &countingRoundTripper{}
	h.SetVerifyLinkClient(&http.Client{Transport: rt})
	h.SetServerURL("https://local.example")
	g := stoppedGroup(t, h)

	rec := post(h.Update, `{"fields":[{"name":"site","value":"https://remote.example/me"}]}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	time.Sleep(100 * time.Millisecond)
	assert.Zero(t, rt.n.Load(), "世代を止めた後に rel=me を確かめに行った")
	assert.EqualValues(t, 1, g.Dropped())
}

// The verification mail of i/update-email starts in the group of the server
// generation (#3661).
func TestUpdateEmail_MailRunsInTheBackgroundGroup(t *testing.T) {
	h, repo := newExtraHandler(t)
	pwd := hashPassword("secret")
	repo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Password: &pwd}
	var sent atomic.Int32
	h.SetEmailSender(func(string, miscsmtp.Message) { sent.Add(1) })
	g := stoppedGroup(t, h)

	rec := postExtra(h.UpdateEmail, `{"password":"secret","email":"new@example.com"}`, stubUser)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	time.Sleep(100 * time.Millisecond)
	assert.Zero(t, sent.Load(), "世代を止めた後に確認メールを送った")
	assert.EqualValues(t, 1, g.Dropped())
}
