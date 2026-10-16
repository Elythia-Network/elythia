package channels

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNoteChannels_ApplyHardMute pins that every note-carrying channel drops a
// note hidden by the viewer's hardMutedWords, including a pure renote judged by
// the renoted note (#787 / #3369). The clean control proves the payload would
// otherwise be emitted on that channel.
func TestNoteChannels_ApplyHardMute(t *testing.T) {
	type channelCase struct {
		name   string
		make   func(ctx *stubContext) stream.Channel
		params string
	}
	cases := []channelCase{
		{"localTimeline", func(ctx *stubContext) stream.Channel { return NewLocalTimeline(ctx) }, `{"withRenotes":true}`},
		{"globalTimeline", func(ctx *stubContext) stream.Channel { return NewGlobalTimeline(ctx) }, `{"withRenotes":true}`},
		{"homeTimeline", func(ctx *stubContext) stream.Channel { return NewHomeTimeline(ctx) }, `{"withRenotes":true}`},
		{"hybridTimeline", func(ctx *stubContext) stream.Channel { return NewHybridTimeline(ctx) }, `{"withRenotes":true}`},
		{"hashtag", func(ctx *stubContext) stream.Channel { return NewHashtag(ctx) }, `{"q":[["golang"]]}`},
		{"channel", func(ctx *stubContext) stream.Channel { return NewChannelTimeline(ctx) }, `{"channelId":"ch1"}`},
		{"roleTimeline", func(ctx *stubContext) stream.Channel { return newRoleCh(ctx, true) }, `{"roleId":"r1"}`},
		{"userList", func(ctx *stubContext) stream.Channel { return newUserListCh(ctx, "viewer") }, `{"listId":"l1"}`},
		{"antenna", func(ctx *stubContext) stream.Channel { return newAntennaCh(ctx, "viewer") }, `{"antennaId":"a1"}`},
	}
	const tmpl = `{"id":"n1","userId":"booster","visibility":"public","tags":["golang"],"channelId":"ch1","renoteId":"r1","renote":{"id":"r1","userId":"author","visibility":"public","text":%q}}`
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := func(text string) int {
				ctx := newCtx(&model.User{ID: "viewer"})
				ctx.hardMuteRules = []byte(`[["muted"]]`)
				ctx.followingSnap = map[string]bool{"booster": true, "author": true}
				ch := tc.make(ctx)
				require.NoError(t, ch.Init(json.RawMessage(tc.params)))
				ch.OnRedisEvent([]byte(fmt.Sprintf(tmpl, text)))
				return len(ctx.sentType)
			}
			require.Equal(t, 1, run("clean"), "control: the clean pure renote must be emitted")
			assert.Equal(t, 0, run("a muted word"), "a pure renote of a hard-muted note must be dropped")
		})
	}
}
