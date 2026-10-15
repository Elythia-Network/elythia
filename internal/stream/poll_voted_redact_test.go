package stream

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pollVotedBodyFor subscribes viewer to note n1, delivers one pollVoted event
// and returns the inner body the connection received.
func pollVotedBodyFor(t *testing.T, viewer *model.User, inner string) map[string]any {
	t.Helper()
	fc := newFakeConn()
	conn := NewConnection("test", viewer, fc)
	go conn.Start()
	t.Cleanup(conn.Close)
	bus := newStubBus()
	d := NewDispatcher(conn, nil, bus)
	d.SetNoteVisibilityChecker(&stubNoteVisibility{defaultAllow: true})
	d.HandleClientMessage("s", json.RawMessage(`{"id":"n1"}`))

	bus.deliver("noteStream:n1", []byte(inner))

	require.Eventually(t, func() bool { return fc.writeCount() >= 1 }, time.Second, 5*time.Millisecond)
	fc.mu.Lock()
	raw := fc.writes[0]
	fc.mu.Unlock()
	var outer struct {
		Body struct {
			Type string         `json:"type"`
			Body map[string]any `json:"body"`
		} `json:"body"`
	}
	require.NoError(t, json.Unmarshal(raw, &outer))
	require.Equal(t, "pollVoted", outer.Body.Type)
	return outer.Body.Body
}

func TestDispatcher_PollVoted_VoterIDOnlyForVoter(t *testing.T) {
	const inner = `{"type":"pollVoted","body":{"choice":1,"userId":"voter","voteId":"v1"}}`
	cases := []struct {
		name       string
		viewer     *model.User
		wantUserID string
	}{
		{name: "voter sees own id", viewer: &model.User{ID: "voter"}, wantUserID: "voter"},
		{name: "other user sees vote id", viewer: &model.User{ID: "other"}, wantUserID: "v1"},
		{name: "anonymous sees vote id", viewer: nil, wantUserID: "v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := pollVotedBodyFor(t, tc.viewer, inner)
			assert.Equal(t, tc.wantUserID, body["userId"])
			assert.EqualValues(t, 1, body["choice"])
			_, hasVoteID := body["voteId"]
			assert.False(t, hasVoteID, "voteId must not reach the client as its own field")
		})
	}
}

func TestDispatcher_PollVoted_WithoutVoteIDHidesVoter(t *testing.T) {
	// voteId を持たない publisher (更新の途中の旧版) でも投票者を流さない。
	body := pollVotedBodyFor(t, &model.User{ID: "other"},
		`{"type":"pollVoted","body":{"choice":0,"userId":"voter"}}`)
	assert.Equal(t, "", body["userId"])
}

func TestDispatcher_PollVoted_MalformedBodyDropped(t *testing.T) {
	cases := []struct {
		name  string
		inner string
	}{
		{name: "body not object", inner: `{"type":"pollVoted","body":"x"}`},
		{name: "choice missing", inner: `{"type":"pollVoted","body":{"userId":"voter","voteId":"v1"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeConn()
			conn := NewConnection("test", &model.User{ID: "other"}, fc)
			go conn.Start()
			defer conn.Close()
			bus := newStubBus()
			d := NewDispatcher(conn, nil, bus)
			d.SetNoteVisibilityChecker(&stubNoteVisibility{defaultAllow: true})
			d.HandleClientMessage("s", json.RawMessage(`{"id":"n1"}`))

			bus.deliver("noteStream:n1", []byte(tc.inner))
			time.Sleep(50 * time.Millisecond)
			assert.Zero(t, fc.writeCount())
		})
	}
}
