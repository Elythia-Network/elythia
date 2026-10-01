package channels

import (
	"encoding/json"
	"errors"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/stream"
)

// RoleExplorableChecker reports whether a role's timeline is publicly
// streamable (isPublic && isExplorable). Implemented by core/role.Service.
type RoleExplorableChecker interface {
	IsPublicExplorable(roleID string) bool
}

// errRoleTimelineNotAvailable is returned from Init when the role is not both
// public and explorable, so the dispatcher drops the connect request.
var errRoleTimelineNotAvailable = errors.New("roleTimeline: role is not public and explorable")

// RoleTimelineFactory builds RoleTimelineChannels carrying an isPublic &&
// isExplorable checker so Init and OnRedisEvent can gate hidden roles (#1549).
type RoleTimelineFactory struct {
	explorable RoleExplorableChecker
}

// NewRoleTimelineFactory constructs a RoleTimelineFactory.
func NewRoleTimelineFactory(explorable RoleExplorableChecker) *RoleTimelineFactory {
	return &RoleTimelineFactory{explorable: explorable}
}

// New implements stream.ChannelFactory.
func (f *RoleTimelineFactory) New(ctx stream.ChannelContext) stream.Channel {
	return &RoleTimelineChannel{ctx: ctx, explorable: f.explorable}
}

// RoleTimelineChannel forwards notes from users with a specific role.
type RoleTimelineChannel struct {
	ctx        stream.ChannelContext
	explorable RoleExplorableChecker
	topic      string
	roleID     string
	filter     noteFilter
}

func (c *RoleTimelineChannel) Init(params json.RawMessage) error {
	var p struct {
		RoleID string `json:"roleId"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	// 本家 2026.10.0 の role-timeline.ts は roleId が無ければ init で false を返し、
	// 接続を受け付けない (以前は何もせず成功していた)。空文字も、該当するロールが
	// 無いので同じく受け付けない。
	if p.RoleID == "" {
		return errRoleTimelineNotAvailable
	}
	// 本家 2026.10.0 の role-timeline.ts は init でも isPublic && isExplorable を
	// 確かめ、満たさなければ接続を受け付けない (#17987)。mk-go では Init が error を
	// 返すと dispatcher が channel を巻き戻し、connected も送らない。checker 未配線は
	// fail-closed。
	if c.explorable == nil || !c.explorable.IsPublicExplorable(p.RoleID) {
		return errRoleTimelineNotAvailable
	}
	c.filter = parseNoteFilter(params)
	c.roleID = p.RoleID
	c.topic = "roleTimeline:" + p.RoleID
	c.ctx.Subscribe(c.topic)
	return nil
}

func (c *RoleTimelineChannel) OnRedisEvent(payload []byte) {
	// 本家 role-timeline.ts: isPublic && isExplorable な role かつ visibility==public
	// のみ emit。両フラグは runtime 可変 (接続後に非公開へ変わりうる) なので
	// per-event でも check する (publish 側では gate しない)。checker 未配線は
	// fail-closed。
	if c.explorable == nil || !c.explorable.IsPublicExplorable(c.roleID) {
		return
	}
	if noteVisibility(payload) != string(model.NoteVisibilityPublic) {
		return
	}
	// anon viewer + 著者 requireSigninToViewContents は note を丸ごと drop する
	// (upstream role-timeline.ts:53-55 の note/renote/reply 3 連 gate、channel /
	// hashtag と同じ。role-timeline にだけ移植が漏れていた、#1780)。
	if anonRequireSigninDrop(payload, viewerIDFromCtx(c.ctx)) {
		return
	}
	// 未ログインの viewer には meta.ugcVisibilityForVisitor を適用する
	// (upstream NoteStreamingHidingService.filter)。
	if anonUGCVisibilityDrop(c.ctx, payload, viewerIDFromCtx(c.ctx)) {
		return
	}
	if !c.filter.shouldEmit(payload, c.ctx.HardMuteRules(), viewerIDFromCtx(c.ctx)) {
		return
	}
	if noteMutedOrBlocked(payload, c.ctx.MuteBlockSnapshot()) {
		return
	}
	payload = hideEmbeds(c.ctx, payload)
	// pure renote の renote.myReaction を viewer 毎に inject (#2058)。
	payload = injectRenoteMyReaction(payload, viewerIDFromCtx(c.ctx))
	_ = c.ctx.Send("note", json.RawMessage(payload))
}

func (c *RoleTimelineChannel) OnClientMessage(string, json.RawMessage) {}

func (c *RoleTimelineChannel) Dispose() {
	if c.topic != "" {
		c.ctx.Unsubscribe(c.topic)
	}
}
