package channels

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/elythia-network/elythia/internal/core/ugcvisibility"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countPayloadDecodes installs a decode counter for the rest of the test.
func countPayloadDecodes(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	hook := func() { n.Add(1) }
	prev := payloadDecodeHook.Swap(&hook)
	t.Cleanup(func() { payloadDecodeHook.Store(prev) })
	return &n
}

// streamNoteChannelKind builds one timeline channel wired directly (no Init),
// paired with the per-gate pipeline the channel ran before #3986.
type streamNoteChannelKind struct {
	name   string
	build  func(ctx *stubContext, f noteFilter) stream.Channel
	legacy func(ch stream.Channel, payload []byte)
}

// legacyForViewer is the pre-#3986 tail shared by every timeline channel.
func legacyForViewer(ctx stream.ChannelContext, payload []byte, viewerID string) {
	payload = hideEmbeds(ctx, payload)
	payload = injectRenoteMyReaction(payload, viewerID)
	_ = ctx.Send("note", json.RawMessage(payload))
}

func legacyTimeline(c stream.ChannelContext, filter noteFilter, payload []byte, signinGate bool, mode replyGateMode, param bool) {
	viewerID := viewerIDFromCtx(c)
	if signinGate && anonRequireSigninDrop(payload, viewerID) {
		return
	}
	if anonUGCVisibilityDrop(c, payload, viewerID) {
		return
	}
	if !filter.shouldEmit(payload, c.HardMuteRules(), viewerID) {
		return
	}
	if noteMutedOrBlocked(payload, c.MuteBlockSnapshot()) {
		return
	}
	if !replyShouldEmit(payload, viewerID, c.FollowingSnapshot(), param, mode) {
		return
	}
	legacyForViewer(c, payload, viewerID)
}

func streamNoteChannelKinds() []streamNoteChannelKind {
	explorable := mockExplorable{public: map[string]bool{"r1": true}, explorable: map[string]bool{"r1": true}}
	return []streamNoteChannelKind{
		{
			name:  "localTimeline",
			build: func(ctx *stubContext, f noteFilter) stream.Channel { return &LocalTimelineChannel{ctx: ctx, filter: f} },
			legacy: func(ch stream.Channel, p []byte) {
				c := ch.(*LocalTimelineChannel)
				legacyTimeline(c.ctx, c.filter, p, true, replyGateLocal, c.filter.WithReplies)
			},
		},
		{
			name: "globalTimeline",
			build: func(ctx *stubContext, f noteFilter) stream.Channel {
				return &GlobalTimelineChannel{ctx: ctx, filter: f}
			},
			legacy: func(ch stream.Channel, p []byte) {
				c := ch.(*GlobalTimelineChannel)
				legacyTimeline(c.ctx, c.filter, p, true, replyGateGlobal, c.filter.WithReplies)
			},
		},
		{
			name:  "homeTimeline",
			build: func(ctx *stubContext, f noteFilter) stream.Channel { return &HomeTimelineChannel{ctx: ctx, filter: f} },
			legacy: func(ch stream.Channel, p []byte) {
				c := ch.(*HomeTimelineChannel)
				legacyTimeline(c.ctx, c.filter, p, false, replyGateHome, false)
			},
		},
		{
			name: "hybridTimeline",
			build: func(ctx *stubContext, f noteFilter) stream.Channel {
				return &HybridTimelineChannel{ctx: ctx, filter: f}
			},
			legacy: func(ch stream.Channel, p []byte) {
				c := ch.(*HybridTimelineChannel)
				legacyTimeline(c.ctx, c.filter, p, true, replyGateHybrid, c.filter.WithReplies)
			},
		},
		{
			name:  "antenna",
			build: func(ctx *stubContext, f noteFilter) stream.Channel { return &AntennaChannel{ctx: ctx, filter: f} },
			legacy: func(ch stream.Channel, p []byte) {
				c := ch.(*AntennaChannel)
				if !streamNoteVisibleForViewer(p, viewerIDFromCtx(c.ctx), c.ctx.FollowingSnapshot()) {
					return
				}
				if anonUGCVisibilityDrop(c.ctx, p, viewerIDFromCtx(c.ctx)) {
					return
				}
				if !c.filter.shouldEmit(p, c.ctx.HardMuteRules(), viewerIDFromCtx(c.ctx)) {
					return
				}
				if noteMutedOrBlocked(p, c.ctx.MuteBlockSnapshot()) {
					return
				}
				legacyForViewer(c.ctx, p, viewerIDFromCtx(c.ctx))
			},
		},
		{
			name: "channel",
			build: func(ctx *stubContext, f noteFilter) stream.Channel {
				return &ChannelTimelineChannel{ctx: ctx, channelID: "ch1", filter: f}
			},
			legacy: func(ch stream.Channel, p []byte) {
				c := ch.(*ChannelTimelineChannel)
				if noteChannelID(p) != c.channelID {
					return
				}
				if anonRequireSigninDrop(p, viewerIDFromCtx(c.ctx)) {
					return
				}
				if anonUGCVisibilityDrop(c.ctx, p, viewerIDFromCtx(c.ctx)) {
					return
				}
				if !streamNoteVisibleForViewer(p, viewerIDFromCtx(c.ctx), c.ctx.FollowingSnapshot()) {
					return
				}
				if !c.filter.shouldEmit(p, c.ctx.HardMuteRules(), viewerIDFromCtx(c.ctx)) {
					return
				}
				if noteMutedOrBlockedInChannel(p, c.ctx.MuteBlockSnapshot(), c.channelID) {
					return
				}
				legacyForViewer(c.ctx, p, viewerIDFromCtx(c.ctx))
			},
		},
		{
			name: "roleTimeline",
			build: func(ctx *stubContext, f noteFilter) stream.Channel {
				return &RoleTimelineChannel{ctx: ctx, explorable: explorable, roleID: "r1", filter: f}
			},
			legacy: func(ch stream.Channel, p []byte) {
				c := ch.(*RoleTimelineChannel)
				if c.explorable == nil || !c.explorable.IsPublicExplorable(c.roleID) {
					return
				}
				if noteVisibility(p) != string(model.NoteVisibilityPublic) {
					return
				}
				if anonRequireSigninDrop(p, viewerIDFromCtx(c.ctx)) {
					return
				}
				if anonUGCVisibilityDrop(c.ctx, p, viewerIDFromCtx(c.ctx)) {
					return
				}
				if !c.filter.shouldEmit(p, c.ctx.HardMuteRules(), viewerIDFromCtx(c.ctx)) {
					return
				}
				if noteMutedOrBlocked(p, c.ctx.MuteBlockSnapshot()) {
					return
				}
				legacyForViewer(c.ctx, p, viewerIDFromCtx(c.ctx))
			},
		},
		{
			name: "hashtag",
			build: func(ctx *stubContext, f noteFilter) stream.Channel {
				return &HashtagChannel{ctx: ctx, q: [][]string{{"walk"}}, filter: f, seen: map[string]struct{}{}}
			},
			legacy: func(ch stream.Channel, p []byte) {
				c := ch.(*HashtagChannel)
				if !c.matchesQuery(p) {
					return
				}
				if id := streamNoteID(p); id != "" {
					if _, dup := c.seen[id]; dup {
						return
					}
					c.seen[id] = struct{}{}
				}
				if anonRequireSigninDrop(p, viewerIDFromCtx(c.ctx)) {
					return
				}
				if anonUGCVisibilityDrop(c.ctx, p, viewerIDFromCtx(c.ctx)) {
					return
				}
				if !streamNoteVisibleForViewer(p, viewerIDFromCtx(c.ctx), c.ctx.FollowingSnapshot()) {
					return
				}
				if !c.filter.shouldEmit(p, c.ctx.HardMuteRules(), viewerIDFromCtx(c.ctx)) {
					return
				}
				if noteMutedOrBlocked(p, c.ctx.MuteBlockSnapshot()) {
					return
				}
				legacyForViewer(c.ctx, p, viewerIDFromCtx(c.ctx))
			},
		},
		{
			name: "userList",
			build: func(ctx *stubContext, f noteFilter) stream.Channel {
				return &UserListChannel{ctx: ctx, filter: f, withRepliesByUser: map[string]bool{"author1": true, "author2": false}}
			},
			legacy: func(ch stream.Channel, p []byte) {
				c := ch.(*UserListChannel)
				var env struct {
					Type string          `json:"type"`
					Body json.RawMessage `json:"body"`
				}
				if json.Unmarshal(p, &env) == nil && (env.Type == "userAdded" || env.Type == "userRemoved" || env.Type == "userUpdated") {
					c.applyMembershipEvent(env.Type, env.Body)
					if env.Type != "userUpdated" {
						_ = c.ctx.Send(env.Type, env.Body)
					}
					return
				}
				viewerID := viewerIDFromCtx(c.ctx)
				if anonUGCVisibilityDrop(c.ctx, p, viewerID) {
					return
				}
				if !c.filter.shouldEmit(p, c.ctx.HardMuteRules(), viewerID) {
					return
				}
				if noteMutedOrBlocked(p, c.ctx.MuteBlockSnapshot()) {
					return
				}
				if !userListVisibilityShouldEmit(p, viewerID, c.ctx.FollowingSnapshot()) {
					return
				}
				if c.withRepliesByUser != nil {
					var author struct {
						UserID string `json:"userId"`
					}
					_ = json.Unmarshal(p, &author)
					if !userListReplyShouldEmit(p, viewerID, c.withRepliesByUser[author.UserID], c.ctx.FollowingSnapshot()) {
						return
					}
				}
				legacyForViewer(c.ctx, p, viewerID)
			},
		},
	}
}

// streamNoteCorpus is a set of payloads that between them reach every gate's
// pass and drop branches, plus payloads that do not decode as a note.
func streamNoteCorpus() map[string]string {
	user := func(id string, extra string) string {
		return fmt.Sprintf(`{"id":%q,"username":%q,"host":null,"avatarUrl":"https://go.example/a"%s}`, id, id, extra)
	}
	note := func(id, userID, u, rest string) string {
		return fmt.Sprintf(`{"id":%q,"createdAt":"2026-01-01T00:00:00.000Z","userId":%q,"user":%s,"visibility":"public","text":"hello #walk","cw":null,"renoteId":null,"replyId":null,"fileIds":[],"files":[],"tags":["walk"],"reactions":{},"reactionAndUserPairCache":[]%s}`, id, userID, u, rest)
	}
	a1 := user("author1", "")
	a2 := user("author2", "")
	a3 := user("author3", "")
	a4 := user("author4", "")
	a5 := user("author5", "")
	remote := `{"id":"remote1","username":"r","host":"remote.example","avatarUrl":"x"}`
	signin := user("author6", `,"requireSigninToViewContents":true`)
	foBefore := user("author7", `,"makeNotesFollowersOnlyBefore":1800000000`)
	hiddenBefore := user("author8", `,"makeNotesHiddenBefore":1800000000`)
	foBeforeUnfollowed := user("author9", `,"makeNotesFollowersOnlyBefore":1800000000`)
	stranger := user("stranger", "")
	pure := func(id, userID, u, target string) string {
		return fmt.Sprintf(`{"id":%q,"createdAt":"2026-01-01T00:00:00.000Z","userId":%q,"user":%s,"visibility":"public","text":null,"cw":null,"renoteId":"t1","replyId":null,"fileIds":[],"files":[],"tags":["walk"],"reactions":{},"renote":%s}`, id, userID, u, target)
	}
	followersBy := func(id, userID, u string) string {
		return strings.Replace(note(id, userID, u, ""), `"visibility":"public"`, `"visibility":"followers"`, 1)
	}
	reacted := note("t1", "author2", a2, ``)
	reacted = strings.Replace(reacted, `"reactions":{},"reactionAndUserPairCache":[]`, `"reactions":{"👍":1},"reactionAndUserPairCache":["viewer/👍"]`, 1)
	truncated := strings.Replace(reacted, `{"👍":1}`, `{"👍":5}`, 1)
	return map[string]string{
		"plain":                note("n1", "author1", a1, ""),
		"plainOther":           note("n2", "author2", a2, ""),
		"reply-to-viewer":      note("n3", "author2", a2, `,"replyId":"r1","reply":{"id":"r1","userId":"viewer","visibility":"public"}`),
		"reply-followers":      note("n4", "author1", a1, `,"replyId":"r1","reply":`+followersBy("r1", "stranger", stranger)),
		"reply-self-thread":    note("n5", "author2", a2, `,"replyId":"r1","reply":{"id":"r1","userId":"author2","visibility":"public"}`),
		"reply-other":          note("n6", "author2", a2, `,"replyId":"r1","reply":{"id":"r1","userId":"stranger","visibility":"public"}`),
		"reply-mention":        note("n7", "author2", a2, `,"mentions":["viewer"],"replyId":"r1","reply":{"id":"r1","userId":"stranger","visibility":"public"}`),
		"reply-specified-dest": note("n8", "author2", a2, `,"visibility":"specified","visibleUserIds":["viewer"],"mentions":["viewer"],"replyId":"r1","reply":{"id":"r1","userId":"stranger","visibility":"public"}`),
		"reply-no-embed":       note("n9", "author2", a2, `,"replyId":"r1"`),
		"renote-myreaction":    pure("n10", "author1", a1, reacted),
		"renote-truncated":     pure("n11", "author1", a1, truncated),
		"renote-of-followers-reply": pure("n12", "author1", a1,
			note("t1", "author2", a2, `,"replyId":"r2","reply":`+followersBy("r2", "stranger", stranger))),
		"renote-hidden-embed":   pure("n13", "author1", a1, followersBy("t1", "stranger", stranger)),
		"renote-depth2":         pure("n14", "author1", a1, note("t1", "author2", a2, `,"renoteId":"t2","renote":`+followersBy("t2", "stranger", stranger))),
		"renote-by-renotemuted": pure("n15", "author5", a5, note("t1", "author2", a2, "")),
		"renote-of-muted":       pure("n16", "author1", a1, note("t1", "author3", a3, "")),
		"renote-muted-channel":  pure("n17", "author1", a1, note("t1", "author2", a2, `,"channelId":"ch2"`)),
		"renote-signin":         pure("n18", "author1", a1, note("t1", "author6", signin, "")),
		"reply-signin":          note("n44", "author1", a1, `,"replyId":"r1","reply":`+note("r1", "author6", signin, "")),
		"quote":                 note("n19", "author1", a1, `,"renoteId":"t1","renote":`+reacted),
		"files":                 note("n20", "author1", a1, `,"fileIds":["f1"],"files":[{"id":"f1"}]`),
		"poll":                  note("n21", "author1", a1, `,"poll":{"choices":[]}`),
		"specified":             note("n22", "author2", a2, `,"visibility":"specified","visibleUserIds":["viewer"]`),
		"specified-other":       note("n23", "author2", a2, `,"visibility":"specified","visibleUserIds":["someone"]`),
		"followers-followee":    followersBy("n24", "author1", a1),
		"followers-stranger":    followersBy("n25", "stranger", stranger),
		"in-channel":            note("n26", "author1", a1, `,"channelId":"ch1"`),
		"in-muted-channel":      note("n27", "author1", a1, `,"channelId":"ch2"`),
		"remote-muted":          note("n28", "remote1", remote, ""),
		"blocked-by":            note("n29", "author4", a4, ""),
		"muted-author":          note("n30", "author3", a3, ""),
		"signin-author":         note("n31", "author6", signin, ""),
		"followers-only-before": note("n32", "author7", foBefore, ""),
		"hidden-before":         note("n33", "author8", hiddenBefore, ""),
		"followers-only-before-reply-to-viewer": note("n42", "author9", foBeforeUnfollowed,
			`,"replyId":"r1","reply":{"id":"r1","userId":"viewer","visibility":"public"}`),
		"followers-only-before-reply-other": note("n43", "author9", foBeforeUnfollowed,
			`,"replyId":"r1","reply":{"id":"r1","userId":"stranger","visibility":"public"}`),
		"hardmuted":           strings.Replace(note("n34", "author1", a1, ""), "hello", "forbidden", 1),
		"hardmuted-in-renote": pure("n35", "author1", a1, strings.Replace(note("t1", "author2", a2, ""), "hello", "forbidden", 1)),
		"no-tags":             strings.Replace(note("n36", "author1", a1, ""), `"tags":["walk"],`, "", 1),
		"user-null":           `{"id":"n37","userId":"author1","user":null,"visibility":"public","text":"hi","tags":["walk"]}`,
		"no-user":             `{"id":"n38","userId":"author1","visibility":"public","text":"hi","tags":["walk"]}`,
		"json-null":           `null`,
		"malformed":           `{"id":"n39",`,
		"type-mismatch":       `{"id":"n40","userId":"author1","user":{"id":"author1","host":null},"visibility":"public","text":"hi","tags":["walk"],"fileIds":"oops"}`,
		"type-mismatch-reply": `{"id":"n41","userId":"author2","visibility":"public","replyId":"r1","reply":{"userId":"viewer","visibility":7},"tags":["walk"]}`,
		"array":               `[1,2]`,
		"membership-added":    `{"type":"userAdded","body":{"id":"author2"}}`,
		"membership-updated":  `{"type":"userUpdated","body":{"id":"author2","withReplies":true}}`,
		"other-envelope":      `{"type":"somethingElse","body":{"id":"x"},"userId":"author1","visibility":"public","tags":["walk"]}`,
	}
}

type streamNoteViewer struct {
	name  string
	build func() *stubContext
}

func streamNoteViewers() []streamNoteViewer {
	signedIn := func(full bool) func() *stubContext {
		return func() *stubContext {
			ctx := newCtx(&model.User{ID: "viewer"})
			if full {
				ctx.followingSnap = map[string]bool{"author1": true, "author2": false, "author7": false}
				ctx.hardMuteRules = []byte(`[["forbidden"]]`)
				ctx.muteBlockSnap = &stream.MuteBlockSnapshot{
					Muting:         map[string]struct{}{"author3": {}},
					BlockingMe:     map[string]struct{}{"author4": {}},
					RenoteMuting:   map[string]struct{}{"author5": {}},
					MutedInstances: map[string]struct{}{"remote.example": {}},
					MutingChannels: map[string]struct{}{"ch2": {}},
				}
			}
			return ctx
		}
	}
	anon := func(policy string) func() *stubContext {
		return func() *stubContext {
			ctx := newCtx(nil)
			ctx.ugcPolicy = policy
			return ctx
		}
	}
	return []streamNoteViewer{
		{"anon", anon("")},
		{"anon-ugc-local", anon(string(ugcvisibility.Local))},
		{"anon-ugc-none", anon("none")},
		{"viewer-bare", signedIn(false)},
		{"viewer-full", signedIn(true)},
	}
}

func streamNoteFilters() map[string]noteFilter {
	return map[string]noteFilter{
		"default":     defaultNoteFilter(),
		"noRenotes":   {WithRenotes: false},
		"withFiles":   {WithRenotes: true, WithFiles: true},
		"withReplies": {WithRenotes: true, WithReplies: true},
	}
}

func sentAsStrings(ctx *stubContext) []string {
	out := make([]string, 0, len(ctx.sentBody))
	for i, b := range ctx.sentBody {
		raw, _ := json.Marshal(b)
		out = append(out, ctx.sentType[i]+" "+string(raw))
	}
	return out
}

// TestStreamNoteDeliveryMatchesPerGateDecode pins #3986: decoding the payload
// once per delivery must not change any gate's result. Every channel kind gets
// every payload under every viewer and connect filter, and must send exactly
// what the pre-#3986 per-gate pipeline sends.
func TestStreamNoteDeliveryMatchesPerGateDecode(t *testing.T) {
	corpus := streamNoteCorpus()
	sent, dropped, rewritten := 0, 0, 0
	for _, kind := range streamNoteChannelKinds() {
		for _, viewer := range streamNoteViewers() {
			for fname, filter := range streamNoteFilters() {
				for pname, payload := range corpus {
					gotCtx, wantCtx := viewer.build(), viewer.build()
					kind.build(gotCtx, filter).OnRedisEvent([]byte(payload))
					kind.legacy(kind.build(wantCtx, filter), []byte(payload))
					want := sentAsStrings(wantCtx)
					sent += len(want)
					if len(want) == 0 {
						dropped++
					}
					for _, b := range wantCtx.sentBody {
						if raw, ok := b.(json.RawMessage); ok && string(raw) != payload {
							rewritten++
						}
					}
					assert.Equal(t, want, sentAsStrings(gotCtx), "%s / %s / %s / %s", kind.name, viewer.name, fname, pname)
				}
			}
		}
	}
	// コーパスが何も送らない形に崩れると、比べても何も確かめていないことになる。
	t.Logf("sent=%d dropped=%d rewritten=%d", sent, dropped, rewritten)
	require.Greater(t, sent, 500, "the corpus must reach the send path")
	require.Greater(t, dropped, 500, "the corpus must reach the drop branches")
	require.Greater(t, rewritten, 50, "the corpus must reach the rewrite path")
}

// TestStreamNoteDeliveryDecodesPayloadOnce pins #3986: one delivery decodes
// the payload once, whatever gates run, and re-decodes only on a connection
// whose bytes must be rewritten.
func TestStreamNoteDeliveryDecodesPayloadOnce(t *testing.T) {
	corpus := streamNoteCorpus()
	cases := []struct {
		payload string
		want    int64
		sends   int
	}{
		// 全部の判定を通って送る。書き換えは無い。
		{"plain", 1, 1},
		// 途中の判定で落ちる。
		{"muted-author", 1, 0},
		// myReaction を足すために、送る前に1回だけ読み直す。
		{"renote-myreaction", 2, 1},
	}
	for _, kind := range streamNoteChannelKinds() {
		for _, tc := range cases {
			t.Run(kind.name+"/"+tc.payload, func(t *testing.T) {
				ctx := streamNoteViewers()[4].build()
				ch := kind.build(ctx, defaultNoteFilter())
				if kind.name == "hashtag" || kind.name == "channel" {
					// hashtag は tag、channel は channelId が合わないと最初に落ちる。
					// 他の判定まで届く payload を使う。
					tc.payload = strings.Replace(corpus[tc.payload], `"tags":["walk"]`, `"tags":["walk"],"channelId":"ch1"`, 1)
				} else {
					tc.payload = corpus[tc.payload]
				}
				decodes := countPayloadDecodes(t)
				ch.OnRedisEvent([]byte(tc.payload))
				assert.Equal(t, tc.want, decodes.Load(), "payload decodes per delivery")
				assert.Len(t, ctx.sentBody, tc.sends)
			})
		}
	}
}
