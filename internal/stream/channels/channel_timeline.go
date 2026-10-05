package channels

import (
	"encoding/json"

	"github.com/elythia-network/elythia/internal/stream"
)

// ChannelTimelineChannel forwards notes posted to a specific channel.
type ChannelTimelineChannel struct {
	ctx       stream.ChannelContext
	topic     string
	channelID string
	filter    noteFilter
}

// NewChannelTimeline returns a channel factory for "channel".
func NewChannelTimeline(ctx stream.ChannelContext) stream.Channel {
	return &ChannelTimelineChannel{ctx: ctx}
}

func (c *ChannelTimelineChannel) Init(params json.RawMessage) error {
	var p struct {
		ChannelID string `json:"channelId"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	// TS本家のchannel.tsはchannelId欠如時にreturn (init自体は成功) するためerrorは返さない
	if p.ChannelID == "" {
		return nil
	}
	c.filter = parseNoteFilter(params)
	c.channelID = p.ChannelID
	c.topic = "channel:" + p.ChannelID
	c.ctx.Subscribe(c.topic)
	return nil
}

func (c *ChannelTimelineChannel) OnRedisEvent(payload []byte) {
	// channelId 一致を確認 (defense-in-depth)。topic 購読で基本保証されるが、
	// fanout が channel:<note.channelId> へ publish するので payload 側でも照合。
	if noteChannelID(payload) != c.channelID {
		return
	}
	// anon viewer + 著者 requireSigninToViewContents は note を丸ごと drop する
	// (#1549, upstream channel.ts の note/renote/reply 3 連 gate)。hideEmbeds の
	// blank 化より前に弾いて「何も送らない」upstream 挙動に揃える。
	if anonRequireSigninDrop(payload, viewerIDFromCtx(c.ctx)) {
		return
	}
	// 未ログインの viewer には meta.ugcVisibilityForVisitor を適用する
	// (upstream NoteStreamingHidingService.filter)。
	if anonUGCVisibilityDrop(c.ctx, payload, viewerIDFromCtx(c.ctx)) {
		return
	}
	// per-subscriber 可視性 gate (#1549, fail-closed)。channel note は通常
	// public だが、共通ゲートを通して非可視/壊れた payload を drop する。
	if !streamNoteVisibleForViewer(payload, viewerIDFromCtx(c.ctx), c.ctx.FollowingSnapshot()) {
		return
	}
	if !c.filter.shouldEmit(payload, c.ctx.HardMuteRules(), viewerIDFromCtx(c.ctx)) {
		return
	}
	// channel timeline は upstream channel.ts の override に倣い、視聴中 channel
	// 自身の mute は無視する (直接見ている channel の note は流す)。他 channel への
	// renote の channel-mute のみ適用する (#1812)。
	if noteMutedOrBlockedInChannel(payload, c.ctx.MuteBlockSnapshot(), c.channelID) {
		return
	}
	payload = hideEmbeds(c.ctx, payload)
	// pure renote の renote.myReaction を viewer 毎に inject (#2058)。
	payload = injectRenoteMyReaction(payload, viewerIDFromCtx(c.ctx))
	_ = c.ctx.Send("note", json.RawMessage(payload))
}

func (c *ChannelTimelineChannel) OnClientMessage(string, json.RawMessage) {}

func (c *ChannelTimelineChannel) Dispose() {
	if c.topic != "" {
		c.ctx.Unsubscribe(c.topic)
	}
}
