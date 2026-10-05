package channels

import (
	"testing"

	"github.com/elythia-network/elythia/internal/stream"
	"github.com/stretchr/testify/assert"
)

func set(ids ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		m[id] = struct{}{}
	}
	return m
}

func TestNoteMutedOrBlocked(t *testing.T) {
	t.Run("nil snapshot is fail-open", func(t *testing.T) {
		assert.False(t, noteMutedOrBlocked([]byte(`{"userId":"author"}`), nil))
	})
	t.Run("parse error is fail-open", func(t *testing.T) {
		snap := &stream.MuteBlockSnapshot{Muting: set("author")}
		assert.False(t, noteMutedOrBlocked([]byte(`{not json`), snap))
	})
	t.Run("clean note passes", func(t *testing.T) {
		snap := &stream.MuteBlockSnapshot{
			Muting:         set("x"),
			BlockingMe:     set("y"),
			MutedInstances: set("evil.example"),
		}
		assert.False(t, noteMutedOrBlocked([]byte(`{"userId":"author","user":{"host":"good.example"}}`), snap))
	})
	t.Run("instance-mute on top-level author", func(t *testing.T) {
		snap := &stream.MuteBlockSnapshot{MutedInstances: set("evil.example")}
		assert.True(t, noteMutedOrBlocked([]byte(`{"userId":"a","user":{"host":"evil.example"}}`), snap))
	})
	t.Run("instance-mute on reply author", func(t *testing.T) {
		snap := &stream.MuteBlockSnapshot{MutedInstances: set("evil.example")}
		p := []byte(`{"userId":"a","user":{"host":"good.example"},"reply":{"userId":"b","user":{"host":"evil.example"}}}`)
		assert.True(t, noteMutedOrBlocked(p, snap))
	})
	t.Run("instance-mute on renote author", func(t *testing.T) {
		snap := &stream.MuteBlockSnapshot{MutedInstances: set("evil.example")}
		p := []byte(`{"userId":"a","user":{"host":"good.example"},"renote":{"userId":"b","user":{"host":"evil.example"}}}`)
		assert.True(t, noteMutedOrBlocked(p, snap))
	})
	t.Run("user-mute on author", func(t *testing.T) {
		snap := &stream.MuteBlockSnapshot{Muting: set("author")}
		assert.True(t, noteMutedOrBlocked([]byte(`{"userId":"author"}`), snap))
	})
	t.Run("block (blocking me) on renote author", func(t *testing.T) {
		snap := &stream.MuteBlockSnapshot{BlockingMe: set("blocker")}
		p := []byte(`{"userId":"a","renote":{"userId":"blocker"}}`)
		assert.True(t, noteMutedOrBlocked(p, snap))
	})
	t.Run("renote-mute drops pure renote", func(t *testing.T) {
		snap := &stream.MuteBlockSnapshot{RenoteMuting: set("renoter")}
		p := []byte(`{"userId":"renoter","renoteId":"r1","renote":{"userId":"orig"}}`)
		assert.True(t, noteMutedOrBlocked(p, snap))
	})
	t.Run("renote-mute does NOT drop quote (has text)", func(t *testing.T) {
		snap := &stream.MuteBlockSnapshot{RenoteMuting: set("renoter")}
		p := []byte(`{"userId":"renoter","renoteId":"r1","text":"quote!","renote":{"userId":"orig"}}`)
		assert.False(t, noteMutedOrBlocked(p, snap))
	})
	t.Run("channel-mute on own channel", func(t *testing.T) {
		snap := &stream.MuteBlockSnapshot{MutingChannels: set("ch1")}
		assert.True(t, noteMutedOrBlocked([]byte(`{"userId":"a","channelId":"ch1"}`), snap))
	})
	t.Run("channel-mute on renote channel", func(t *testing.T) {
		snap := &stream.MuteBlockSnapshot{MutingChannels: set("ch2")}
		p := []byte(`{"userId":"a","channelId":"ch1","renote":{"userId":"b","channelId":"ch2"}}`)
		assert.True(t, noteMutedOrBlocked(p, snap))
	})
}

func TestUserRelated(t *testing.T) {
	t.Run("empty set false", func(t *testing.T) {
		assert.False(t, userRelated(muteBlockProbe{UserID: "a"}, nil))
	})
	t.Run("reply author equal to top author is not double-checked", func(t *testing.T) {
		// reply.userId == userId のとき、reply branch では評価しない (top で評価済)。
		p := muteBlockProbe{UserID: "a", Reply: &muteBlockEmbed{UserID: "a"}}
		assert.False(t, userRelated(p, set("b")))
	})
}

func TestIsPureRenotePayload(t *testing.T) {
	assert.False(t, isPureRenotePayload(muteBlockProbe{}), "no renoteId")
	assert.True(t, isPureRenotePayload(muteBlockProbe{RenoteID: ptr("r1")}), "bare renote")
	assert.False(t, isPureRenotePayload(muteBlockProbe{RenoteID: ptr("r1"), Text: ptr("hi")}), "text => quote")
	assert.False(t, isPureRenotePayload(muteBlockProbe{RenoteID: ptr("r1"), CW: ptr("cw")}), "cw => quote")
	assert.False(t, isPureRenotePayload(muteBlockProbe{RenoteID: ptr("r1"), ReplyID: ptr("p1")}), "replyId => quote")
	assert.False(t, isPureRenotePayload(muteBlockProbe{RenoteID: ptr("r1"), Poll: []byte(`{}`)}), "poll => quote")
	assert.False(t, isPureRenotePayload(muteBlockProbe{RenoteID: ptr("r1"), FileIDs: []string{"f1"}}), "files => quote")
	assert.True(t, isPureRenotePayload(muteBlockProbe{RenoteID: ptr("r1"), Poll: []byte(`null`)}), "poll=null still pure")
}

func TestNotificationFromMutedInstance(t *testing.T) {
	t.Run("nil snapshot false", func(t *testing.T) {
		assert.False(t, notificationFromMutedInstance([]byte(`{"user":{"host":"evil.example"}}`), nil))
	})
	t.Run("empty muted set false", func(t *testing.T) {
		snap := &stream.MuteBlockSnapshot{}
		assert.False(t, notificationFromMutedInstance([]byte(`{"user":{"host":"evil.example"}}`), snap))
	})
	t.Run("muted instance drops", func(t *testing.T) {
		snap := &stream.MuteBlockSnapshot{MutedInstances: set("evil.example")}
		assert.True(t, notificationFromMutedInstance([]byte(`{"user":{"host":"evil.example"}}`), snap))
	})
	t.Run("local notifier (no host) passes", func(t *testing.T) {
		snap := &stream.MuteBlockSnapshot{MutedInstances: set("evil.example")}
		assert.False(t, notificationFromMutedInstance([]byte(`{"user":{"id":"local"}}`), snap))
	})
	t.Run("no user field passes", func(t *testing.T) {
		snap := &stream.MuteBlockSnapshot{MutedInstances: set("evil.example")}
		assert.False(t, notificationFromMutedInstance([]byte(`{"id":"n1"}`), snap))
	})
	t.Run("parse error passes", func(t *testing.T) {
		snap := &stream.MuteBlockSnapshot{MutedInstances: set("evil.example")}
		assert.False(t, notificationFromMutedInstance([]byte(`{not json`), snap))
	})
}

func ptr[T any](v T) *T { return &v }
