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
	// payload は配信1回につき1回だけ解析し、各判定はその結果を読む (#3986)。
	note := newNoteView(payload)
	// channelId 一致を確認 (defense-in-depth)。topic 購読で基本保証されるが、
	// fanout が channel:<note.channelId> へ publish するので payload 側でも照合。
	if note.channelID() != c.channelID {
		return
	}
	// anon viewer + 著者 requireSigninToViewContents は note を丸ごと drop する
	// (#1549, upstream channel.ts の note/renote/reply 3 連 gate)。hideEmbeds の
	// blank 化より前に弾いて「何も送らない」upstream 挙動に揃える。
	if note.anonRequireSigninDrop(viewerIDFromCtx(c.ctx)) {
		return
	}
	// 未ログインの viewer には meta.ugcVisibilityForVisitor を適用する
	// (upstream NoteStreamingHidingService.filter)。
	if note.anonUGCVisibilityDrop(c.ctx, viewerIDFromCtx(c.ctx)) {
		return
	}
	// per-subscriber 可視性 gate (#1549, fail-closed)。channel note は通常
	// public だが、共通ゲートを通して非可視/壊れた payload を drop する。
	if !note.visibleForViewer(viewerIDFromCtx(c.ctx), c.ctx.FollowingSnapshot()) {
		return
	}
	if !note.shouldEmit(&c.filter, c.ctx.HardMuteRules(), viewerIDFromCtx(c.ctx)) {
		return
	}
	// channel timeline は upstream channel.ts の override に倣い、視聴中 channel
	// 自身の mute は無視する (直接見ている channel の note は流す)。他 channel への
	// renote の channel-mute のみ適用する (#1812)。
	if note.mutedOrBlockedInChannel(c.ctx.MuteBlockSnapshot(), c.channelID) {
		return
	}
	// 埋め込みを隠し、pure renote の renote.myReaction を viewer 毎に inject する
	// (#1536 / #2058)。書き換えが要らなければ元の bytes を送る。
	_ = c.ctx.Send("note", json.RawMessage(note.forViewer(c.ctx, viewerIDFromCtx(c.ctx))))
}

func (c *ChannelTimelineChannel) OnClientMessage(string, json.RawMessage) {}

func (c *ChannelTimelineChannel) Dispose() {
	if c.topic != "" {
		c.ctx.Unsubscribe(c.topic)
	}
}
