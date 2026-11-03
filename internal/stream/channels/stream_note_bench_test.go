package channels

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
)

// benchNotePayload returns a packed local note shaped like what the publisher
// emits for a plain text post (#3986)。renote を渡すと純リノートになる。
func benchNotePayload(tb testing.TB, renote *entity.NoteEntity) []byte {
	tb.Helper()
	name := "Author"
	text := "今日は良い天気ですね。散歩に行ってきます。 #walk https://example.com/some/path?with=query and a bit more text to look like a real post"
	badges := []any{}
	pairs := []string{}
	n := entity.NoteEntity{
		ID:        "a1b2c3d4e5f6g7h8",
		CreatedAt: "2026-11-03T12:34:56.789Z",
		UserID:    "author0000000001",
		User: entity.UserLite{
			ID: "author0000000001", Name: &name, Username: "author",
			AvatarURL:         "https://go.example/identicon/author",
			AvatarDecorations: []entity.AvatarDecorationItem{},
			Emojis:            map[string]string{},
			OnlineStatus:      "online",
			BadgeRoles:        &badges,
		},
		Text:                     &text,
		Visibility:               "public",
		Reactions:                []byte(`{}`),
		ReactionEmojis:           map[string]string{},
		FileIDs:                  []string{},
		Files:                    []any{},
		Tags:                     []string{"walk"},
		ReactionAndUserPairCache: &pairs,
	}
	if renote != nil {
		n.Text = nil
		n.Tags = nil
		n.RenoteID = &renote.ID
		n.Renote = renote
	}
	b, err := json.Marshal(&n)
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// benchViewerCtx is a signed-in viewer following 300 users with a few mutes,
// the shape the 2026-11-03 audit measured (300 homeTimeline connections)。
func benchViewerCtx(rules []byte) *stubContext {
	follow := make(map[string]bool, 300)
	for i := range 300 {
		follow[fmt.Sprintf("followee%08d", i)] = i%2 == 0
	}
	follow["author0000000001"] = false
	ctx := newCtx(&model.User{ID: "viewer0000000001"})
	ctx.followingSnap = follow
	ctx.hardMuteRules = rules
	ctx.muteBlockSnap = &stream.MuteBlockSnapshot{
		Muting:     map[string]struct{}{"muted1": {}, "muted2": {}, "muted3": {}},
		BlockingMe: map[string]struct{}{},
	}
	return ctx
}

func benchmarkDeliver(b *testing.B, newCh func(stream.ChannelContext) stream.Channel, params string, rules []byte, payload []byte) {
	ctx := benchViewerCtx(rules)
	ch := newCh(ctx)
	if err := ch.Init(json.RawMessage(params)); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ch.OnRedisEvent(payload)
		// 送った分を捨てて、スライスの伸びを測らないようにする。
		ctx.sentType = ctx.sentType[:0]
		ctx.sentBody = ctx.sentBody[:0]
	}
}

// BenchmarkTimelineDeliver measures one delivery of one note to one
// connection (#3986)。
func BenchmarkTimelineDeliver(b *testing.B) {
	plain := benchNotePayload(b, nil)
	var reacted entity.NoteEntity
	if err := json.Unmarshal(benchNotePayload(b, nil), &reacted); err != nil {
		b.Fatal(err)
	}
	reacted.ID = "renotetarget0001"
	reacted.Reactions = []byte(`{"👍":1}`)
	reacted.ReactionAndUserPairCache = &[]string{"viewer0000000001/👍"}
	renoteMine := benchNotePayload(b, &reacted)
	cases := []struct {
		name    string
		newCh   func(stream.ChannelContext) stream.Channel
		params  string
		rules   []byte
		payload []byte
	}{
		{"home/plain", NewHomeTimeline, `{}`, nil, plain},
		{"home/hardmute", NewHomeTimeline, `{}`, []byte(`[["forbidden"]]`), plain},
		{"home/renote-myreaction", NewHomeTimeline, `{}`, nil, renoteMine},
		{"local/plain", NewLocalTimeline, `{"withReplies":false}`, nil, plain},
		{"hybrid/plain", NewHybridTimeline, `{}`, nil, plain},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			benchmarkDeliver(b, tc.newCh, tc.params, tc.rules, tc.payload)
		})
	}
}
