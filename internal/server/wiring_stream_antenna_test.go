package server

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/elythia-network/elythia/internal/testutil"
)

// antennaTestContext is the minimum stream.ChannelContext the antenna channel
// reads: the viewer, and what it sends.
type antennaTestContext struct {
	user *model.User
	sent []any
}

func (c *antennaTestContext) ID() string                { return "conn" }
func (c *antennaTestContext) HasPermission(string) bool { return true }
func (c *antennaTestContext) User() any                 { return c.user }
func (c *antennaTestContext) Send(_ string, body any) error {
	c.sent = append(c.sent, body)
	return nil
}
func (c *antennaTestContext) Subscribe(string)                             {}
func (c *antennaTestContext) Unsubscribe(string)                           {}
func (c *antennaTestContext) HardMuteRules() []byte                        { return nil }
func (c *antennaTestContext) FollowingSnapshot() map[string]bool           { return nil }
func (c *antennaTestContext) MuteBlockSnapshot() *stream.MuteBlockSnapshot { return nil }
func (c *antennaTestContext) UserPolicies() map[string]any                 { return nil }
func (c *antennaTestContext) UGCVisibilityForVisitor() string              { return "all" }

// TestBuiltinStreamChannelsGiveTheAntennaThePollVotes: the antenna factory is
// built with the poll vote repository, so the channel marks the viewer's votes
// (#4164). Without it the channel would silently send every poll as unvoted.
func TestBuiltinStreamChannelsGiveTheAntennaThePollVotes(t *testing.T) {
	antennas := testutil.NewMockAntennaRepository()
	antennas.Antennas["a1"] = &model.Antenna{ID: "a1", UserID: "alice"}
	votes := testutil.NewMockPollVoteRepository()
	require.NoError(t, votes.Create(&model.PollVote{ID: "v1", UserID: "alice", NoteID: "n1", Choice: 1}))

	d := allStreamChannelDeps()
	d.antennas = antennas
	d.pollVotes = votes
	var factory stream.ChannelFactory
	for _, c := range builtinStreamChannels(d) {
		if c.name == "antenna" {
			factory = c.factory
		}
	}
	require.NotNil(t, factory, "antenna の factory が無い")

	ctx := &antennaTestContext{user: &model.User{ID: "alice"}}
	ch := factory(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"antennaId":"a1"}`)))
	ch.OnRedisEvent([]byte(`{"id":"n1","userId":"author","visibility":"public","poll":{"multiple":false,"expiresAt":null,"choices":[{"text":"a","votes":0,"isVoted":false},{"text":"b","votes":1,"isVoted":false}]}}`))
	require.Len(t, ctx.sent, 1)
	var got struct {
		Poll struct {
			Choices []struct {
				IsVoted bool `json:"isVoted"`
			} `json:"choices"`
		} `json:"poll"`
	}
	require.NoError(t, json.Unmarshal(ctx.sent[0].(json.RawMessage), &got))
	require.Len(t, got.Poll.Choices, 2)
	assert.False(t, got.Poll.Choices[0].IsVoted)
	assert.True(t, got.Poll.Choices[1].IsVoted)
}
