package channels

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/stream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubContext implements stream.ChannelContext for tests.
type stubContext struct {
	mu            sync.Mutex
	id            string
	user          any
	subs          []string
	unsubs        []string
	sentType      []string
	sentBody      []any
	sendError     error
	hardMuteRules []byte
	followingSnap map[string]bool
	followingUpd  []followingUpdate
	muteBlockSnap *stream.MuteBlockSnapshot
	policies      map[string]any
	ugcPolicy     string
}

// followingUpdate records a call to UpdateFollowingSnapshot for assertions.
type followingUpdate struct {
	id        string
	following bool
}

func (s *stubContext) ID() string { return s.id }
func (s *stubContext) User() any  { return s.user }
func (s *stubContext) Send(msgType string, body any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sentType = append(s.sentType, msgType)
	s.sentBody = append(s.sentBody, body)
	return s.sendError
}
func (s *stubContext) Subscribe(topic string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = append(s.subs, topic)
}
func (s *stubContext) Unsubscribe(topic string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unsubs = append(s.unsubs, topic)
}
func (s *stubContext) HardMuteRules() []byte              { return s.hardMuteRules }
func (s *stubContext) FollowingSnapshot() map[string]bool { return s.followingSnap }
func (s *stubContext) MuteBlockSnapshot() *stream.MuteBlockSnapshot {
	return s.muteBlockSnap
}
func (s *stubContext) UserPolicies() map[string]any    { return s.policies }
func (s *stubContext) UGCVisibilityForVisitor() string { return s.ugcPolicy }
func (s *stubContext) UpdateFollowingSnapshot(followeeID string, following bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.followingUpd = append(s.followingUpd, followingUpdate{followeeID, following})
}

func newCtx(user any) *stubContext {
	return &stubContext{id: "ch1", user: user}
}

// channels conformance: each constructor returns a stream.Channel.
var (
	_ stream.Channel = (*LocalTimelineChannel)(nil)
	_ stream.Channel = (*GlobalTimelineChannel)(nil)
	_ stream.Channel = (*HomeTimelineChannel)(nil)
	_ stream.Channel = (*HybridTimelineChannel)(nil)
	_ stream.Channel = (*NotificationsChannel)(nil)
	_ stream.Channel = (*MainChannel)(nil)
)

// #1812: timeline channel は mute/block snapshot に該当する live note を drop する。
func TestLocalTimeline_DropsMutedAndBlockedAndInstance(t *testing.T) {
	// user-mute: muted author の note を落とす。
	muted := newCtx(&model.User{ID: "viewer"})
	muted.muteBlockSnap = &stream.MuteBlockSnapshot{Muting: map[string]struct{}{"bad": {}}}
	chM := NewLocalTimeline(muted)
	require.NoError(t, chM.Init(nil))
	chM.OnRedisEvent([]byte(`{"id":"n1","userId":"bad"}`))
	assert.Empty(t, muted.sentType, "muted author の note は drop する")
	chM.OnRedisEvent([]byte(`{"id":"n2","userId":"ok"}`))
	assert.Len(t, muted.sentType, 1, "非 mute author は通す")

	// block (blockingMe): 自分を block している author の note を落とす。
	blk := newCtx(&model.User{ID: "viewer"})
	blk.muteBlockSnap = &stream.MuteBlockSnapshot{BlockingMe: map[string]struct{}{"enemy": {}}}
	chB := NewLocalTimeline(blk)
	require.NoError(t, chB.Init(nil))
	chB.OnRedisEvent([]byte(`{"id":"n3","userId":"enemy"}`))
	assert.Empty(t, blk.sentType, "blockingMe author の note は drop する")

	// instance-mute: muted host の author の note を落とす。
	inst := newCtx(&model.User{ID: "viewer"})
	inst.muteBlockSnap = &stream.MuteBlockSnapshot{MutedInstances: map[string]struct{}{"bad.example": {}}}
	chI := NewLocalTimeline(inst)
	require.NoError(t, chI.Init(nil))
	chI.OnRedisEvent([]byte(`{"id":"n4","userId":"r","user":{"host":"bad.example"}}`))
	assert.Empty(t, inst.sentType, "muted instance の note は drop する")
}

func TestLocalTimeline_Lifecycle(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewLocalTimeline(ctx)
	require.NoError(t, ch.Init(nil))
	assert.Equal(t, []string{"localTimeline"}, ctx.subs)

	ch.OnRedisEvent([]byte(`{"id":"n1"}`))
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "note", ctx.sentType[0])
	body, ok := ctx.sentBody[0].(json.RawMessage)
	require.True(t, ok)
	assert.Contains(t, string(body), "n1")

	ch.OnClientMessage("ignored", json.RawMessage(`{}`))
	ch.Dispose()
	assert.Equal(t, []string{"localTimeline"}, ctx.unsubs)
}

// #1942 F1: ltlAvailable=false の connection は localTimeline を subscribe しない
// (= 接続は成立するが note が流れない、upstream init() return 相当)。nil policies
// (provider 未配線) は fail-open で subscribe する (TestLocalTimeline_Lifecycle で担保)。
func TestLocalTimeline_PolicyGate(t *testing.T) {
	disabled := newCtx(&model.User{ID: "v"})
	disabled.policies = map[string]any{"ltlAvailable": false}
	require.NoError(t, NewLocalTimeline(disabled).Init(nil))
	assert.Empty(t, disabled.subs, "ltlAvailable=false は subscribe しない")

	enabled := newCtx(&model.User{ID: "v"})
	enabled.policies = map[string]any{"ltlAvailable": true}
	require.NoError(t, NewLocalTimeline(enabled).Init(nil))
	assert.Equal(t, []string{"localTimeline"}, enabled.subs, "ltlAvailable=true は subscribe する")
}

// #1942 F1: gtlAvailable gate。
func TestGlobalTimeline_PolicyGate(t *testing.T) {
	disabled := newCtx(&model.User{ID: "v"})
	disabled.policies = map[string]any{"gtlAvailable": false}
	require.NoError(t, NewGlobalTimeline(disabled).Init(nil))
	assert.Empty(t, disabled.subs)

	enabled := newCtx(&model.User{ID: "v"})
	enabled.policies = map[string]any{"gtlAvailable": true}
	require.NoError(t, NewGlobalTimeline(enabled).Init(nil))
	assert.Equal(t, []string{"globalTimeline"}, enabled.subs)
}

// #1942 F1: hybrid は ltlAvailable=false なら local も home も subscribe しない。
func TestHybridTimeline_PolicyGate(t *testing.T) {
	disabled := newCtx(&model.User{ID: "v"})
	disabled.policies = map[string]any{"ltlAvailable": false}
	require.NoError(t, NewHybridTimeline(disabled).Init(nil))
	assert.Empty(t, disabled.subs, "ltlAvailable=false は local も home も subscribe しない")
}

// #1942 F2: anon viewer + note/renote/reply 著者 requireSigninToViewContents の note は
// 丸ごと drop する (blank shell を送らない)。
func TestLocalTimeline_AnonRequireSigninDrop(t *testing.T) {
	anon := newCtx(nil) // viewerID ""
	ch := NewLocalTimeline(anon)
	require.NoError(t, ch.Init(nil))
	ch.OnRedisEvent([]byte(`{"id":"n1","user":{"requireSigninToViewContents":true}}`))
	assert.Empty(t, anon.sentType, "anon に requireSignin note を送らない")
	ch.OnRedisEvent([]byte(`{"id":"n2","user":{"requireSigninToViewContents":false}}`))
	assert.Len(t, anon.sentType, 1, "通常 note は通す")

	// authed viewer には requireSignin note も流れる (gate は anon 限定)。
	authed := newCtx(&model.User{ID: "v"})
	ca := NewLocalTimeline(authed)
	require.NoError(t, ca.Init(nil))
	ca.OnRedisEvent([]byte(`{"id":"n3","user":{"requireSigninToViewContents":true}}`))
	assert.Len(t, authed.sentType, 1, "authed viewer には流れる")
}

// #1942 F2: GTL は reply 著者の requireSignin も anon に対して drop する。
func TestGlobalTimeline_AnonRequireSigninDrop(t *testing.T) {
	anon := newCtx(nil)
	ch := NewGlobalTimeline(anon)
	require.NoError(t, ch.Init(nil))
	ch.OnRedisEvent([]byte(`{"id":"n1","reply":{"user":{"requireSigninToViewContents":true}}}`))
	assert.Empty(t, anon.sentType, "anon に reply 著者 requireSignin の note を送らない")
}

// #1942 F2: mk-go は anon が hybrid を購読できる (upstream は requireCredential)
// ため hybrid も anon に requireSignin note を blank shell で送らず drop する。
func TestHybridTimeline_AnonRequireSigninDrop(t *testing.T) {
	anon := newCtx(nil)
	ch := NewHybridTimeline(anon)
	require.NoError(t, ch.Init(nil))
	ch.OnRedisEvent([]byte(`{"id":"n1","user":{"requireSigninToViewContents":true}}`))
	assert.Empty(t, anon.sentType, "anon hybrid に requireSignin note を送らない")
}

func TestGlobalTimeline_Lifecycle(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewGlobalTimeline(ctx)
	require.NoError(t, ch.Init(nil))
	assert.Equal(t, []string{"globalTimeline"}, ctx.subs)

	ch.OnRedisEvent([]byte(`{"id":"g1"}`))
	require.Len(t, ctx.sentType, 1)

	ch.OnClientMessage("ignored", json.RawMessage(`{}`))
	ch.Dispose()
	assert.Equal(t, []string{"globalTimeline"}, ctx.unsubs)
}

func TestHomeTimeline_AuthenticatedSubscribesPerUser(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewHomeTimeline(ctx)
	require.NoError(t, ch.Init(nil))
	assert.Equal(t, []string{"homeTimeline:alice"}, ctx.subs)

	ch.OnRedisEvent([]byte(`{"id":"h1"}`))
	require.Len(t, ctx.sentType, 1)

	ch.OnClientMessage("ignored", json.RawMessage(`{}`))
	ch.Dispose()
	assert.Equal(t, []string{"homeTimeline:alice"}, ctx.unsubs)
}

func TestHomeTimeline_AnonymousIsNoOp(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewHomeTimeline(ctx)
	require.NoError(t, ch.Init(nil))
	assert.Empty(t, ctx.subs)

	ch.Dispose()
	assert.Empty(t, ctx.unsubs)
}

func TestHomeTimeline_NilUserPointer(t *testing.T) {
	ctx := newCtx((*model.User)(nil))
	ch := NewHomeTimeline(ctx)
	require.NoError(t, ch.Init(nil))
	assert.Empty(t, ctx.subs)
}

func TestHybridTimeline_AuthenticatedSubscribesBoth(t *testing.T) {
	ctx := newCtx(&model.User{ID: "alice"})
	ch := NewHybridTimeline(ctx)
	require.NoError(t, ch.Init(nil))
	assert.ElementsMatch(t, []string{"localTimeline", "homeTimeline:alice"}, ctx.subs)

	ch.OnRedisEvent([]byte(`{"id":"h1"}`))
	require.Len(t, ctx.sentType, 1)

	ch.OnClientMessage("ignored", json.RawMessage(`{}`))
	ch.Dispose()
	assert.ElementsMatch(t, []string{"localTimeline", "homeTimeline:alice"}, ctx.unsubs)
}

func TestHybridTimeline_AnonymousSubscribesLocalOnly(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewHybridTimeline(ctx)
	require.NoError(t, ch.Init(nil))
	assert.Equal(t, []string{"localTimeline"}, ctx.subs)

	ch.Dispose()
	assert.Equal(t, []string{"localTimeline"}, ctx.unsubs)
}

// #1063: 自分自身が投稿した reply が HomeTimeline channel を経由して
// WebSocket subscriber に push される (= isMe escape hatch)。upstream
// `home-timeline.ts:46-69` の `isMe` 経路を Go 側で再現するもの。drift
// regression guard。
func TestHomeTimeline_SelfReplyEmitsViaWebSocket(t *testing.T) {
	ctx := newCtx(&model.User{ID: "viewer"})
	ch := NewHomeTimeline(ctx)
	require.NoError(t, ch.Init(nil))
	// viewer が他人 reply target に投稿した reply。snapshot は空でも pass する。
	payload := []byte(`{"userId":"viewer","replyId":"p1","reply":{"userId":"someone","visibility":"public"}}`)
	ch.OnRedisEvent(payload)
	require.Len(t, ctx.sentType, 1)
	assert.Equal(t, "note", ctx.sentType[0])
}

// #1063: 他人 → 他人 reply (3 escape hatch どれも該当しない) は default で
// drop される。upstream `home-timeline.ts:69` の default branch。
func TestHomeTimeline_ForeignReplyDroppedWithoutWithReplies(t *testing.T) {
	ctx := newCtx(&model.User{ID: "viewer"})
	ctx.followingSnap = map[string]bool{"author": false}
	ch := NewHomeTimeline(ctx)
	require.NoError(t, ch.Init(nil))
	payload := []byte(`{"userId":"author","replyId":"p1","reply":{"userId":"target","visibility":"public"}}`)
	ch.OnRedisEvent(payload)
	assert.Empty(t, ctx.sentType)
}

// #1063: 自分宛 reply (reply.userId === me) は author を follow していなくても
// pass する (replyToMe escape hatch)。
func TestHomeTimeline_ReplyToMeAllowed(t *testing.T) {
	ctx := newCtx(&model.User{ID: "viewer"})
	ch := NewHomeTimeline(ctx)
	require.NoError(t, ch.Init(nil))
	payload := []byte(`{"userId":"author","replyId":"p1","reply":{"userId":"viewer","visibility":"public"}}`)
	ch.OnRedisEvent(payload)
	require.Len(t, ctx.sentType, 1)
}

// #1063: self-thread (author が自分自身に返信) は snapshot 無しでも pass。
func TestHomeTimeline_SelfThreadAllowed(t *testing.T) {
	ctx := newCtx(&model.User{ID: "viewer"})
	ch := NewHomeTimeline(ctx)
	require.NoError(t, ch.Init(nil))
	payload := []byte(`{"userId":"author","replyId":"p1","reply":{"userId":"author","visibility":"public"}}`)
	ch.OnRedisEvent(payload)
	require.Len(t, ctx.sentType, 1)
}

// #1063: followee.withReplies=true なら他人宛 reply も pass する。
func TestHomeTimeline_FolloweeWithRepliesAllowsForeignReply(t *testing.T) {
	ctx := newCtx(&model.User{ID: "viewer"})
	ctx.followingSnap = map[string]bool{"author": true}
	ch := NewHomeTimeline(ctx)
	require.NoError(t, ch.Init(nil))
	payload := []byte(`{"userId":"author","replyId":"p1","reply":{"userId":"target","visibility":"public"}}`)
	ch.OnRedisEvent(payload)
	require.Len(t, ctx.sentType, 1)
}

// #1063: hybrid channel は connect param withReplies=true で他人宛 reply も
// pass する (snapshot 不要)。
func TestHybridTimeline_ConnectParamWithRepliesAllowsForeignReply(t *testing.T) {
	ctx := newCtx(&model.User{ID: "viewer"})
	ch := NewHybridTimeline(ctx)
	require.NoError(t, ch.Init(json.RawMessage(`{"withReplies":true}`)))
	payload := []byte(`{"userId":"author","replyId":"p1","reply":{"userId":"target","visibility":"public"}}`)
	ch.OnRedisEvent(payload)
	require.Len(t, ctx.sentType, 1)
}

// #1063 follow-up: anonymous viewer の hybrid channel でも reply は
// pass-through する (upstream の `this.user` ガードを mk-go の legacy anonymous
// hybrid 経路にも反映)。旧来 `WithReplies=true → 全 pass` の挙動と整合させる
// ためのリグレッション guard。
func TestHybridTimeline_AnonymousReplyPassthrough(t *testing.T) {
	ctx := newCtx(nil)
	ch := NewHybridTimeline(ctx)
	require.NoError(t, ch.Init(nil))
	payload := []byte(`{"userId":"author","replyId":"p1","reply":{"userId":"target","visibility":"public"}}`)
	ch.OnRedisEvent(payload)
	require.Len(t, ctx.sentType, 1)
}
