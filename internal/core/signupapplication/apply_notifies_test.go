package signupapplication

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

type countingReceivedNotifier struct {
	apps []*model.SignupApplication
	err  error
}

func (c *countingReceivedNotifier) NotifySignupApplicationReceived(_ context.Context, app *model.SignupApplication) error {
	c.apps = append(c.apps, app)
	return c.err
}

// **申請の受付が通知を発火すること自体を固定する (#2987)。** notifier 単体の
// テストだけだと、`Apply` から呼ばれなくなっても両方緑のまま通る (絵文字側の
// `create_notifies_test.go` と同じ理由)。
func TestApply_NotifiesModerators(t *testing.T) {
	svc, _ := newService(t)
	notifier := &countingReceivedNotifier{}
	svc.SetReceivedNotifier(notifier)

	app, _, err := svc.Apply(testAnswers())
	require.NoError(t, err)
	require.Len(t, notifier.apps, 1, "申請を受けてもモデレーターに通知が飛んでいない")
	assert.Equal(t, app.ID, notifier.apps[0].ID)
}

// **通知の失敗で申請を失わせない。**
func TestApply_SucceedsWhenNotificationFails(t *testing.T) {
	svc, _ := newService(t)
	svc.SetReceivedNotifier(&countingReceivedNotifier{err: errors.New("redis down")})

	app, code, err := svc.Apply(nil)
	require.NoError(t, err)
	assert.NotEmpty(t, app.ID)
	assert.NotEmpty(t, code)
}

func TestApply_WithoutReceivedNotifier(t *testing.T) {
	svc, _ := newService(t)
	_, _, err := svc.Apply(nil)
	require.NoError(t, err)
}
