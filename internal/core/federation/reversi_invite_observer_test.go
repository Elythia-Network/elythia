package federation_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 連合で届いた招待を、作った対局の ID と一緒に観測者へ知らせる (#3942)。
// 再送の Invite は同じ対局に畳むので、知らせるのは最初の 1 回だけ。

type reversiInvitedCall struct{ invitee, inviter, game string }

type stubReversiInviteObserver struct{ calls []reversiInvitedCall }

func (s *stubReversiInviteObserver) ReversiInvited(_ context.Context, inviteeID, inviterID, gameID string) {
	s.calls = append(s.calls, reversiInvitedCall{inviteeID, inviterID, gameID})
}

func TestReversiInbox_Invite_TellsObserverOnce(t *testing.T) {
	b := newReversiProcessor(t)
	registerLocalBob(t, b.userRepo)
	obs := &stubReversiInviteObserver{}
	b.processor.SetReversiInviteObserver(obs)

	send := func(sessionID string) {
		body := []byte(`{
			"type": "Invite",
			"actor": "https://remote.example/users/alice",
			"to": "https://example.com/users/bob",
			"object": {
				"type": "Game",
				"game_type_uuid": "1c086295-25e3-4b82-b31e-3e3959906312",
				"game_state": {"game_session_id": "` + sessionID + `"}
			}
		}`)
		require.NoError(t, b.processor.Process(body))
	}
	send("sess-obs-1")
	send("sess-obs-2")

	g := b.gameRepo.findGameBySession(t, b.fedCache, "sess-obs-2")
	require.NotNil(t, g)
	require.Len(t, obs.calls, 1, "再送の Invite では知らせない")
	assert.Equal(t, "bob", obs.calls[0].invitee)
	assert.Equal(t, g.User1ID, obs.calls[0].inviter)
	assert.Equal(t, g.ID, obs.calls[0].game)
}
