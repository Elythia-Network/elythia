package emojiapplication_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/emojiapplication"
	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/model"
)

type recordingNotifier struct {
	inputs []notification.CreateInput
	err    error
}

func (r *recordingNotifier) Create(_ context.Context, in notification.CreateInput) (*notification.Notification, error) {
	r.inputs = append(r.inputs, in)
	if r.err != nil {
		return nil, r.err
	}
	return &notification.Notification{}, nil
}

type stubReviewerLister struct {
	users     []*model.User
	err       error
	askedKeys []string
}

func (s *stubReviewerLister) GetUsersWithPolicy(policyKey string) ([]*model.User, error) {
	s.askedKeys = append(s.askedKeys, policyKey)
	return s.users, s.err
}

func TestReceivedNotifier_NotifiesEveryReviewer(t *testing.T) {
	inner := &recordingNotifier{}
	lister := &stubReviewerLister{users: []*model.User{{ID: "r1"}, {ID: "r2"}}}
	n := emojiapplication.NewReceivedNotifier(inner, lister)
	require.NotNil(t, n)

	app := &model.EmojiApplication{ID: "a1", UserID: "applicant", Name: "sushi"}
	require.NoError(t, n.NotifyEmojiApplicationReceived(context.Background(), app))

	require.Len(t, inner.inputs, 2)
	// **宛先は canManageCustomEmojis を持つ人。** モデレーターではない。
	assert.Equal(t, []string{role.PolicyCanManageCustomEmojis}, lister.askedKeys)
	for i, want := range []string{"r1", "r2"} {
		assert.Equal(t, want, inner.inputs[i].NotifieeID)
		assert.Equal(t, "applicant", inner.inputs[i].NotifierID)
		assert.Equal(t, notification.TypeEmojiApplicationReceived, inner.inputs[i].Type)
		assert.Equal(t, "a1", inner.inputs[i].Extra["applicationId"])
	}
}

// **本文は載せない。** 名前・ライセンス・画像は読み出し時に引き直す。
func TestReceivedNotifier_CarriesOnlyTheApplicationID(t *testing.T) {
	inner := &recordingNotifier{}
	n := emojiapplication.NewReceivedNotifier(inner, &stubReviewerLister{users: []*model.User{{ID: "r1"}}})
	app := &model.EmojiApplication{ID: "a1", UserID: "u1", Name: "sushi", License: "CC0"}
	require.NoError(t, n.NotifyEmojiApplicationReceived(context.Background(), app))

	require.Len(t, inner.inputs, 1)
	assert.Equal(t, map[string]any{"applicationId": "a1"}, inner.inputs[0].Extra)
}

// 1 人が ErrSelfNotification で弾かれても、他の審査者への通知は続ける。
func TestReceivedNotifier_ContinuesAfterSelfNotification(t *testing.T) {
	inner := &recordingNotifier{err: notification.ErrSelfNotification}
	n := emojiapplication.NewReceivedNotifier(inner, &stubReviewerLister{users: []*model.User{{ID: "r1"}, {ID: "r2"}}})
	require.NoError(t, n.NotifyEmojiApplicationReceived(context.Background(),
		&model.EmojiApplication{ID: "a1", UserID: "r1"}))
	assert.Len(t, inner.inputs, 2)
}

func TestReceivedNotifier_ListerErrorIsReturned(t *testing.T) {
	inner := &recordingNotifier{}
	n := emojiapplication.NewReceivedNotifier(inner, &stubReviewerLister{err: errors.New("db down")})
	assert.Error(t, n.NotifyEmojiApplicationReceived(context.Background(),
		&model.EmojiApplication{ID: "a1"}))
	assert.Empty(t, inner.inputs)
}

func TestReceivedNotifier_NilSafe(t *testing.T) {
	assert.Nil(t, emojiapplication.NewReceivedNotifier(nil, &stubReviewerLister{}))
	assert.Nil(t, emojiapplication.NewReceivedNotifier(&recordingNotifier{}, nil))

	inner := &recordingNotifier{}
	n := emojiapplication.NewReceivedNotifier(inner, &stubReviewerLister{users: []*model.User{nil, {ID: "r1"}}})
	require.NoError(t, n.NotifyEmojiApplicationReceived(context.Background(), nil))
	assert.Empty(t, inner.inputs, "app が nil なら何も送らない")
	require.NoError(t, n.NotifyEmojiApplicationReceived(context.Background(), &model.EmojiApplication{ID: "a1"}))
	assert.Len(t, inner.inputs, 1, "一覧に nil が混ざっても落ちない")
}
