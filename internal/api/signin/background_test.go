package signin

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/bgwork"
	"github.com/elythia-network/elythia/internal/misc/id"
	miscsmtp "github.com/elythia-network/elythia/internal/misc/smtp"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

type countingLoginNotifier struct{ n atomic.Int32 }

func (c *countingLoginNotifier) OnLogin(string) { c.n.Add(1) }

// stoppedBackgroundHandler returns a handler whose record, notifier and mail
// are all wired, on a group that has already stopped.
func stoppedBackgroundHandler(t *testing.T) (*Handler, *testutil.MockSigninRepository, *countingLoginNotifier, *atomic.Int32, *bgwork.Group) {
	t.Helper()
	repo := testutil.NewMockUserRepository()
	tok := "Tk-1"
	repo.Users["u1"] = &model.User{ID: "u1", Token: &tok}
	email := "u1@example.test"
	repo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Email: &email, EmailVerified: true, UsePasswordLessLogin: true}
	g := bgwork.New()
	h := NewHandler(repo, g, nil, nil)
	signins := testutil.NewMockSigninRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	h.SetSigninRepo(signins, idGen)
	notifier := &countingLoginNotifier{}
	h.SetLoginNotifier(notifier)
	var mails atomic.Int32
	h.SetEmailSender("https://example.test", func(string, miscsmtp.Message) { mails.Add(1) })
	require.Zero(t, g.Stop(context.Background()))
	return h, signins, notifier, &mails, g
}

// The record, the login notification and the mail of a signin start in the
// group of the server generation (#3661): after the group has stopped, none
// of them runs.
func TestSignin_WorkAfterTheResponseRunsInTheBackgroundGroup(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		h, signins, notifier, mails, g := stoppedBackgroundHandler(t)
		h.RecordSuccessfulSignin("u1", "192.0.2.1", http.Header{})
		time.Sleep(100 * time.Millisecond)
		assert.Zero(t, signins.Len(), "世代を止めた後にサインインを記録した")
		assert.Zero(t, notifier.n.Load(), "世代を止めた後にログインを通知した")
		assert.Zero(t, mails.Load(), "世代を止めた後にメールを送った")
		assert.EqualValues(t, 3, g.Dropped())
	})
	t.Run("failure", func(t *testing.T) {
		h, signins, _, _, g := stoppedBackgroundHandler(t)
		require.NoError(t, h.fail(newCtx(), &model.User{ID: "u1"}, http.StatusForbidden, "x"))
		time.Sleep(100 * time.Millisecond)
		assert.Zero(t, signins.Len(), "世代を止めた後に失敗を記録した")
		assert.EqualValues(t, 1, g.Dropped())
	})
	t.Run("passkey", func(t *testing.T) {
		h, signins, _, _, g := stoppedBackgroundHandler(t)
		tok := "Tk-1"
		require.NoError(t, h.finishPasskeySignin(newCtx(), &model.User{ID: "u1", Token: &tok}, nil))
		time.Sleep(100 * time.Millisecond)
		assert.Zero(t, signins.Len(), "世代を止めた後にパスキーのサインインを記録した")
		assert.EqualValues(t, 1, g.Dropped())
	})
}
