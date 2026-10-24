package signup_test

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	apisignup "github.com/elythia-network/elythia/internal/api/signup"
	"github.com/elythia-network/elythia/internal/bgwork"
	coresignup "github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/misc/id"
	miscsmtp "github.com/elythia-network/elythia/internal/misc/smtp"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// The confirmation mail starts in the group of the server generation
// (#3661): after the group has stopped, it is not sent.
func TestSignup_MailRunsInTheBackgroundGroup(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	metaRepo := testutil.NewMockMetaRepository()
	metaRepo.Meta = &model.Meta{ID: "x", EmailRequiredForSignup: true}
	pendingRepo := testutil.NewMockUserPendingRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coresignup.NewService(userRepo, metaRepo, idGen)
	svc.SetUserPendingRepo(pendingRepo)
	g := bgwork.New()
	h := apisignup.NewHandler(svc, metaRepo, idGen, g, nil, nil)
	var sent atomic.Int32
	h.SetEmailSender("https://example.test", func(string, miscsmtp.Message) { sent.Add(1) })
	require.Zero(t, g.Stop(context.Background()))

	rec := doPost(h.Signup, `{"username":"alice","password":"pass1234","emailAddress":"alice@example.com"}`)
	require.Equal(t, http.StatusNoContent, rec.Code)
	time.Sleep(100 * time.Millisecond)
	assert.Zero(t, sent.Load(), "世代を止めた後に確認メールを送った")
	assert.EqualValues(t, 1, g.Dropped())
}
