package channels

import (
	"encoding/json"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
)

// AntennaOwnerLookup resolves an antenna by id for the channel-side ownership
// gate. repository.AntennaRepository satisfies it (FindByID).
type AntennaOwnerLookup interface {
	FindByID(id string) (*model.Antenna, error)
}

// AntennaFactory builds AntennaChannels carrying an ownership lookup so Init can
// reject subscriptions to antennas the connecting user does not own (#1569).
type AntennaFactory struct {
	owners AntennaOwnerLookup
}

// NewAntennaFactory constructs an AntennaFactory. owners is used by Init to
// verify the antenna belongs to the connecting user. A nil owners fails closed
// (every subscription rejected).
func NewAntennaFactory(owners AntennaOwnerLookup) *AntennaFactory {
	return &AntennaFactory{owners: owners}
}

// New implements stream.ChannelFactory.
func (f *AntennaFactory) New(ctx stream.ChannelContext) stream.Channel {
	return &AntennaChannel{ctx: ctx, owners: f.owners}
}

// AntennaChannel forwards notes captured by a user-defined antenna.
type AntennaChannel struct {
	ctx    stream.ChannelContext
	owners AntennaOwnerLookup
	topic  string
	filter noteFilter
}

func (c *AntennaChannel) Init(params json.RawMessage) error {
	// 認証必須
	user, ok := c.ctx.User().(*model.User)
	if !ok || user == nil {
		return stream.ErrInvalidParams
	}
	var p struct {
		AntennaID string `json:"antennaId"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	if p.AntennaID == "" {
		return stream.ErrInvalidParams
	}
	// Ownership gate (#1569): REST antennas/notes (Service.Show) と同じく、接続中の
	// user が所有する antenna でなければ購読を拒否する。antenna stream は push 段で
	// owner 可視性 (matchNote -> CanSeeNote, #1464) でのみ gate されるため、これが
	// 無いと任意の認証 user が他人の antennaTimeline:<id> を購読し、owner には
	// 見えるが自分には見えない followers / specified note を top-level で受信できる
	// (cross-user IDOR)。本家 AntennaChannel.init() の antenna.userId === user.id
	// 検証に相当。owners 未配線時は fail-closed で購読拒否 (#1444 / #1460 と同方針)。
	if c.owners == nil {
		return stream.ErrInvalidParams
	}
	a, err := c.owners.FindByID(p.AntennaID)
	if err != nil || a == nil || a.UserID != user.ID {
		return stream.ErrInvalidParams
	}
	// 所有者ゲート通過後に他チャンネル同様 connect param の filter を読む
	// (withRenotes / withFiles)。hardmute / 純リノート / file filter は
	// OnRedisEvent の shouldEmit で適用する。
	c.filter = parseNoteFilter(params)
	c.topic = "antennaTimeline:" + p.AntennaID
	c.ctx.Subscribe(c.topic)
	return nil
}

func (c *AntennaChannel) OnRedisEvent(payload []byte) {
	// payload は配信1回につき1回だけ解析し、各判定はその結果を読む (#3986)。
	note := newNoteView(payload)
	// per-subscriber 可視性 re-filter (#1573 課題2 / #1549)。push 時点の owner
	// CanSeeNote (#1464) は subscriber == owner を前提に gate するが、push 後に
	// 著者が followers 化 / owner が unfollow した stale-narrowing はカバー
	// しない。fail-closed: 非可視 / 壊れた payload を drop する。
	if !note.visibleForViewer(viewerIDFromCtx(c.ctx), c.ctx.FollowingSnapshot()) {
		return
	}
	// 未ログインの viewer には meta.ugcVisibilityForVisitor を適用する
	// (upstream NoteStreamingHidingService.filter)。
	if note.anonUGCVisibilityDrop(c.ctx, viewerIDFromCtx(c.ctx)) {
		return
	}
	if !note.shouldEmit(&c.filter, c.ctx.HardMuteRules(), viewerIDFromCtx(c.ctx)) {
		return
	}
	if note.mutedOrBlocked(c.ctx.MuteBlockSnapshot()) {
		return
	}
	// embedded renote/reply の per-viewer 可視性 gate (#1536)。絶対に消さない:
	// 消すと non-visible な引用/返信元が top-level antenna note 経由で漏れる
	// IDOR を再 open する。
	// 埋め込みを隠し、pure renote の renote.myReaction を viewer 毎に inject する
	// (#1536 / #2058)。書き換えが要らなければ元の bytes を送る。
	_ = c.ctx.Send("note", json.RawMessage(note.forViewer(c.ctx, viewerIDFromCtx(c.ctx))))
}

func (c *AntennaChannel) OnClientMessage(string, json.RawMessage) {}

// RequiredPermission implements stream.PermittedChannel.
func (c *AntennaChannel) RequiredPermission() string { return "read:account" }

func (c *AntennaChannel) Dispose() {
	if c.topic != "" {
		c.ctx.Unsubscribe(c.topic)
	}
}
