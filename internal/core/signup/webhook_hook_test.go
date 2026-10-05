package signup_test

import (
	"sync/atomic"
	"testing"

	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingSignupWebhook struct {
	count int32
}

func (r *recordingSignupWebhook) OnUserCreated(_ *model.User) {
	atomic.AddInt32(&r.count, 1)
}

// SetWebhookHook 経由で注入されたフックが Signup 成功時に呼ばれる。
func TestSignupService_WebhookHookFires(t *testing.T) {
	svc, _, _ := newTestService(t)
	hook := &recordingSignupWebhook{}
	svc.SetWebhookHook(hook)

	_, err := svc.Signup("webhook_user", "password123", false, signup.UsernamePolicyPublic)
	require.NoError(t, err)

	assert.Equal(t, int32(1), atomic.LoadInt32(&hook.count))
}
