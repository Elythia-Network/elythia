package signupapplication_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/core/signupapplication"
	"github.com/elythia-network/elythia/internal/model"
)

type recordingNotifier struct {
	inputs []notification.CreateInput
}

func (r *recordingNotifier) Create(_ context.Context, in notification.CreateInput) (*notification.Notification, error) {
	r.inputs = append(r.inputs, in)
	return &notification.Notification{}, nil
}

type stubModeratorLister struct {
	users []*model.User
	err   error
}

func (s *stubModeratorLister) GetModerators() ([]*model.User, error) { return s.users, s.err }

func TestReceivedNotifier_NotifiesEveryModerator(t *testing.T) {
	inner := &recordingNotifier{}
	n := signupapplication.NewReceivedNotifier(inner, &stubModeratorLister{
		users: []*model.User{{ID: "m1"}, {ID: "m2"}},
	})
	require.NotNil(t, n)

	require.NoError(t, n.NotifySignupApplicationReceived(context.Background(),
		&model.SignupApplication{ID: "s1"}))

	require.Len(t, inner.inputs, 2)
	for i, want := range []string{"m1", "m2"} {
		assert.Equal(t, want, inner.inputs[i].NotifieeID)
		assert.Equal(t, notification.TypeSignupApplicationReceived, inner.inputs[i].Type)
		// **notifier は空。** 申請者はまだアカウントを持っていない。
		assert.Empty(t, inner.inputs[i].NotifierID)
		// **回答は載せない。** 氏名や連絡先が入りうる。
		assert.Equal(t, map[string]any{"applicationId": "s1"}, inner.inputs[i].Extra)
	}
}

func TestReceivedNotifier_ListerErrorIsReturned(t *testing.T) {
	inner := &recordingNotifier{}
	n := signupapplication.NewReceivedNotifier(inner, &stubModeratorLister{err: errors.New("db down")})
	assert.Error(t, n.NotifySignupApplicationReceived(context.Background(),
		&model.SignupApplication{ID: "s1"}))
	assert.Empty(t, inner.inputs)
}

func TestReceivedNotifier_NilSafe(t *testing.T) {
	assert.Nil(t, signupapplication.NewReceivedNotifier(nil, &stubModeratorLister{}))
	assert.Nil(t, signupapplication.NewReceivedNotifier(&recordingNotifier{}, nil))

	inner := &recordingNotifier{}
	n := signupapplication.NewReceivedNotifier(inner, &stubModeratorLister{users: []*model.User{nil, {ID: "m1"}}})
	require.NoError(t, n.NotifySignupApplicationReceived(context.Background(), nil))
	assert.Empty(t, inner.inputs)
	require.NoError(t, n.NotifySignupApplicationReceived(context.Background(), &model.SignupApplication{ID: "s1"}))
	assert.Len(t, inner.inputs, 1)
}
