// Package channels contains the per-name Channel implementations registered
// with stream.Registry. 各 Channel は決まった Redis topic を購読し、受信した
// payload を `note` envelope としてクライアントに転送する。
package channels

import (
	"encoding/json"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
)

// timelinePolicyAllows reports whether the connection may subscribe to a
// policy-gated timeline. nil policies (provider 未配線、test/旧挙動) → allow
// (fail-open)。それ以外は named policy が true でなければ不許可 (upstream
// `if (!policies.ltlAvailable) return` 相当、#1942)。
func timelinePolicyAllows(ctx stream.ChannelContext, key string) bool {
	policies := ctx.UserPolicies()
	if policies == nil {
		return true
	}
	v, ok := policies[key].(bool)
	return ok && v
}

// LocalTimelineChannel forwards local-public/home notes.
type LocalTimelineChannel struct {
	ctx    stream.ChannelContext
	filter noteFilter
}

// NewLocalTimeline は ChannelFactory の形をした生成関数。Registry に
// "localTimeline" として登録する。
func NewLocalTimeline(ctx stream.ChannelContext) stream.Channel {
	return &LocalTimelineChannel{ctx: ctx}
}

// Init subscribes to the local-timeline pubsub topic, gated by ltlAvailable.
func (c *LocalTimelineChannel) Init(params json.RawMessage) error {
	c.filter = parseNoteFilter(params)
	// upstream local-timeline.ts init(): ltlAvailable=false なら subscribe せず
	// return (接続は成立するが note は流れない、#1942)。
	if !timelinePolicyAllows(c.ctx, "ltlAvailable") {
		return nil
	}
	c.ctx.Subscribe("localTimeline")
	return nil
}

// OnRedisEvent forwards a JSON-encoded note payload as a `note` event.
// reply の表示可否は upstream `local-timeline.ts` に揃え (#1063)、認証済み
// viewer に対してのみ followee.withReplies / connect param withReplies /
// 3 escape hatch を OR で評価する。
func (c *LocalTimelineChannel) OnRedisEvent(payload []byte) {
	viewerID := viewerIDFromCtx(c.ctx)
	// anon viewer + note/renote/reply 著者が requireSigninToViewContents なら note を
	// 丸ごと drop (#1942、upstream local-timeline.ts の 3 連 gate)。hideEmbeds の
	// blank 化より前に弾いて「何も送らない」upstream 挙動に揃える。
	if anonRequireSigninDrop(payload, viewerID) {
		return
	}
	// 未ログインの viewer には meta.ugcVisibilityForVisitor を適用する
	// (upstream NoteStreamingHidingService.filter)。
	if anonUGCVisibilityDrop(c.ctx, payload, viewerID) {
		return
	}
	if !c.filter.shouldEmit(payload, c.ctx.HardMuteRules(), viewerID) {
		return
	}
	// live note も user-mute / block / instance-mute / renote-mute / channel-mute で
	// filter する (upstream channel.ts isNoteMutedOrBlocked、#1812)。snapshot は
	// connection 確立時にロード済 (未配線/anon は nil で fail-open)。
	if noteMutedOrBlocked(payload, c.ctx.MuteBlockSnapshot()) {
		return
	}
	if !replyShouldEmit(payload, viewerID, c.ctx.FollowingSnapshot(), c.filter.WithReplies, replyGateLocal) {
		return
	}
	payload = hideEmbeds(c.ctx, payload)
	// pure renote の renote.myReaction を viewer 毎に in-memory 算出して inject (#2058)。
	payload = injectRenoteMyReaction(payload, viewerID)
	_ = c.ctx.Send("note", json.RawMessage(payload))
}

// OnClientMessage is unused for the local timeline channel.
func (c *LocalTimelineChannel) OnClientMessage(string, json.RawMessage) {}

// Dispose unsubscribes from the topic.
func (c *LocalTimelineChannel) Dispose() {
	c.ctx.Unsubscribe("localTimeline")
}

// GlobalTimelineChannel forwards every public note across the federation.
type GlobalTimelineChannel struct {
	ctx    stream.ChannelContext
	filter noteFilter
}

// NewGlobalTimeline returns a channel factory for "globalTimeline".
func NewGlobalTimeline(ctx stream.ChannelContext) stream.Channel {
	return &GlobalTimelineChannel{ctx: ctx}
}

func (c *GlobalTimelineChannel) Init(params json.RawMessage) error {
	c.filter = parseNoteFilter(params)
	// upstream global-timeline.ts init(): gtlAvailable=false なら subscribe しない (#1942)。
	if !timelinePolicyAllows(c.ctx, "gtlAvailable") {
		return nil
	}
	c.ctx.Subscribe("globalTimeline")
	return nil
}

// OnRedisEvent forwards a JSON-encoded note payload as a `note` event.
// upstream `global-timeline.ts` は reply に関する gate を一切持たない (#1063)
// ので、ここでも replyShouldEmit を呼ばずに pass-through 相当の挙動になる。
func (c *GlobalTimelineChannel) OnRedisEvent(payload []byte) {
	viewerID := viewerIDFromCtx(c.ctx)
	// anon viewer + note/renote/reply 著者が requireSigninToViewContents なら drop
	// (#1942、upstream global-timeline.ts の 3 連 gate)。hideEmbeds より前に弾く。
	if anonRequireSigninDrop(payload, viewerID) {
		return
	}
	// 未ログインの viewer には meta.ugcVisibilityForVisitor を適用する
	// (upstream NoteStreamingHidingService.filter)。
	if anonUGCVisibilityDrop(c.ctx, payload, viewerID) {
		return
	}
	if !c.filter.shouldEmit(payload, c.ctx.HardMuteRules(), viewerID) {
		return
	}
	// live note も user-mute / block / instance-mute / renote-mute / channel-mute で
	// filter する (upstream channel.ts isNoteMutedOrBlocked、#1812)。snapshot は
	// connection 確立時にロード済 (未配線/anon は nil で fail-open)。
	if noteMutedOrBlocked(payload, c.ctx.MuteBlockSnapshot()) {
		return
	}
	if !replyShouldEmit(payload, viewerID, c.ctx.FollowingSnapshot(), c.filter.WithReplies, replyGateGlobal) {
		return
	}
	payload = hideEmbeds(c.ctx, payload)
	// pure renote の renote.myReaction を viewer 毎に in-memory 算出して inject (#2058)。
	payload = injectRenoteMyReaction(payload, viewerID)
	_ = c.ctx.Send("note", json.RawMessage(payload))
}
func (c *GlobalTimelineChannel) OnClientMessage(string, json.RawMessage) {}
func (c *GlobalTimelineChannel) Dispose() {
	c.ctx.Unsubscribe("globalTimeline")
}

// HomeTimelineChannel forwards notes addressed to the authenticated user. 認証
// 済みでない場合は subscribe しない (channel は接続するだけで何も送らない)。
type HomeTimelineChannel struct {
	ctx    stream.ChannelContext
	topic  string
	filter noteFilter
}

// NewHomeTimeline returns a channel factory for "homeTimeline".
func NewHomeTimeline(ctx stream.ChannelContext) stream.Channel {
	return &HomeTimelineChannel{ctx: ctx}
}

func (c *HomeTimelineChannel) Init(params json.RawMessage) error {
	c.filter = parseNoteFilter(params)
	user, ok := c.ctx.User().(*model.User)
	if !ok || user == nil {
		// TS本家はrequireCredential=trueでConnection側で弾くが、
		// Go側は現状 silent no-op としている。Phase 3ではシグネチャ変更に
		// とどめ、認証拒否はPermittedChannel等別系で扱う
		return nil
	}
	c.topic = "homeTimeline:" + user.ID
	c.ctx.Subscribe(c.topic)
	return nil
}

// OnRedisEvent forwards a JSON-encoded note payload as a `note` event.
// reply 表示可否は upstream `home-timeline.ts` 互換で、followee の
// withReplies snapshot + 3 escape hatch (isMe / replyToMe / selfThread)
// で決める (#1063)。upstream には connect param withReplies が無い ので、
// replyShouldEmit の paramWithReplies には false を渡す。
func (c *HomeTimelineChannel) OnRedisEvent(payload []byte) {
	viewerID := viewerIDFromCtx(c.ctx)
	// 未ログインの viewer には meta.ugcVisibilityForVisitor を適用する
	// (upstream NoteStreamingHidingService.filter)。
	if anonUGCVisibilityDrop(c.ctx, payload, viewerID) {
		return
	}
	if !c.filter.shouldEmit(payload, c.ctx.HardMuteRules(), viewerID) {
		return
	}
	// live note も user-mute / block / instance-mute / renote-mute / channel-mute で
	// filter する (upstream channel.ts isNoteMutedOrBlocked、#1812)。snapshot は
	// connection 確立時にロード済 (未配線/anon は nil で fail-open)。
	if noteMutedOrBlocked(payload, c.ctx.MuteBlockSnapshot()) {
		return
	}
	if !replyShouldEmit(payload, viewerID, c.ctx.FollowingSnapshot(), false, replyGateHome) {
		return
	}
	payload = hideEmbeds(c.ctx, payload)
	// pure renote の renote.myReaction を viewer 毎に in-memory 算出して inject (#2058)。
	payload = injectRenoteMyReaction(payload, viewerID)
	_ = c.ctx.Send("note", json.RawMessage(payload))
}

func (c *HomeTimelineChannel) OnClientMessage(string, json.RawMessage) {}

// RequiredPermission implements stream.PermittedChannel.
func (c *HomeTimelineChannel) RequiredPermission() string { return "read:account" }

func (c *HomeTimelineChannel) Dispose() {
	if c.topic != "" {
		c.ctx.Unsubscribe(c.topic)
	}
}

// HybridTimelineChannel combines homeTimeline (for the authenticated user) and
// localTimeline (for everyone). 通常の Misskey クライアントが使う "ソーシャル
// タイムライン" がこれに相当する。
type HybridTimelineChannel struct {
	ctx       stream.ChannelContext
	homeTopic string
	filter    noteFilter
}

// NewHybridTimeline returns a channel factory for "hybridTimeline".
func NewHybridTimeline(ctx stream.ChannelContext) stream.Channel {
	return &HybridTimelineChannel{ctx: ctx}
}

func (c *HybridTimelineChannel) Init(params json.RawMessage) error {
	c.filter = parseNoteFilter(params)
	// upstream hybrid-timeline.ts init(): ltlAvailable=false なら subscribe しない
	// (local / home 両方、#1942)。
	if !timelinePolicyAllows(c.ctx, "ltlAvailable") {
		return nil
	}
	c.ctx.Subscribe("localTimeline")
	if user, ok := c.ctx.User().(*model.User); ok && user != nil {
		c.homeTopic = "homeTimeline:" + user.ID
		c.ctx.Subscribe(c.homeTopic)
	}
	return nil
}

// OnRedisEvent forwards a JSON-encoded note payload as a `note` event.
// upstream `hybrid-timeline.ts` の reply gate は homeTimeline と同じ形だが、
// connect param `withReplies` を followee.withReplies と OR して評価する点
// が違う (#1063)。
func (c *HybridTimelineChannel) OnRedisEvent(payload []byte) {
	viewerID := viewerIDFromCtx(c.ctx)
	// anon viewer + requireSigninToViewContents は note を丸ごと drop (#1942)。
	// upstream hybrid-timeline.ts は requireCredential=true で anon が到達しない
	// 前提のため明示 gate を持たないが、mk-go は cookie/anon が hybrid の
	// localTimeline topic を購読できるため、LTL/GTL と同じく blank shell を送らず
	// drop して揃える (requireCredential 非適用の root cause は別 issue)。
	if anonRequireSigninDrop(payload, viewerID) {
		return
	}
	// 未ログインの viewer には meta.ugcVisibilityForVisitor を適用する
	// (upstream NoteStreamingHidingService.filter)。
	if anonUGCVisibilityDrop(c.ctx, payload, viewerID) {
		return
	}
	if !c.filter.shouldEmit(payload, c.ctx.HardMuteRules(), viewerID) {
		return
	}
	// live note も user-mute / block / instance-mute / renote-mute / channel-mute で
	// filter する (upstream channel.ts isNoteMutedOrBlocked、#1812)。snapshot は
	// connection 確立時にロード済 (未配線/anon は nil で fail-open)。
	if noteMutedOrBlocked(payload, c.ctx.MuteBlockSnapshot()) {
		return
	}
	if !replyShouldEmit(payload, viewerID, c.ctx.FollowingSnapshot(), c.filter.WithReplies, replyGateHybrid) {
		return
	}
	payload = hideEmbeds(c.ctx, payload)
	// pure renote の renote.myReaction を viewer 毎に in-memory 算出して inject (#2058)。
	payload = injectRenoteMyReaction(payload, viewerID)
	_ = c.ctx.Send("note", json.RawMessage(payload))
}

func (c *HybridTimelineChannel) OnClientMessage(string, json.RawMessage) {}

// RequiredPermission implements stream.PermittedChannel.
func (c *HybridTimelineChannel) RequiredPermission() string { return "read:account" }

func (c *HybridTimelineChannel) Dispose() {
	c.ctx.Unsubscribe("localTimeline")
	if c.homeTopic != "" {
		c.ctx.Unsubscribe(c.homeTopic)
	}
}
