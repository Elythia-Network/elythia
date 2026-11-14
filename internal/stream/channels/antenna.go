package channels

import (
	"bytes"
	"encoding/json"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/stream"
)

// AntennaOwnerLookup resolves an antenna by id for the channel-side ownership
// gate. repository.AntennaRepository satisfies it (FindByID).
type AntennaOwnerLookup interface {
	FindByID(id string) (*model.Antenna, error)
}

// AntennaPollVoteLookup returns the viewer's poll votes grouped by note id
// (repository.PollVoteRepository.FindByUserAndNoteIDs).
type AntennaPollVoteLookup interface {
	FindByUserAndNoteIDs(userID string, noteIDs []string) (map[string][]int, error)
}

// AntennaFactory builds AntennaChannels carrying an ownership lookup so Init can
// reject subscriptions to antennas the connecting user does not own (#1569).
type AntennaFactory struct {
	owners    AntennaOwnerLookup
	pollVotes AntennaPollVoteLookup
}

// NewAntennaFactory constructs an AntennaFactory. owners is used by Init to
// verify the antenna belongs to the connecting user. A nil owners fails closed
// (every subscription rejected).
func NewAntennaFactory(owners AntennaOwnerLookup, pollVotes AntennaPollVoteLookup) *AntennaFactory {
	return &AntennaFactory{owners: owners, pollVotes: pollVotes}
}

// New implements stream.ChannelFactory.
func (f *AntennaFactory) New(ctx stream.ChannelContext) stream.Channel {
	return &AntennaChannel{ctx: ctx, owners: f.owners, pollVotes: f.pollVotes}
}

// AntennaChannel forwards notes captured by a user-defined antenna.
type AntennaChannel struct {
	ctx       stream.ChannelContext
	owners    AntennaOwnerLookup
	pollVotes AntennaPollVoteLookup
	topic     string
	filter    noteFilter
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
	out := note.forViewer(c.ctx, viewerIDFromCtx(c.ctx))
	_ = c.ctx.Send("note", json.RawMessage(c.withMyPollVotes(note, out, viewerIDFromCtx(c.ctx))))
}

// withMyPollVotes marks poll.choices[i].isVoted for the viewer's votes on the
// note and on its renote / reply, followed recursively, and returns the bytes to
// send. It returns out unchanged when none of them has a poll or the viewer has
// not voted.
//
// アンテナに入ったノートは、アンテナの持ち主ではなく投稿者の視点で pack されて
// 配られるので、投票済みのアンケートが未投票として届いていた。本家 2026.11.0
// (#18056) と同じく、アンテナのチャンネルで閲覧者の投票を埋める (#4164。本家も
// timeline のチャンネルでは埋めていない)。アンケートが無ければ DB を引かない。
func (c *AntennaChannel) withMyPollVotes(note noteView, out []byte, viewerID string) []byte {
	if c.pollVotes == nil || viewerID == "" || note.n == nil {
		return out
	}
	ids := collectPollNoteIDs(note.n, nil, 0)
	if len(ids) == 0 {
		return out
	}
	votes, err := c.pollVotes.FindByUserAndNoteIDs(viewerID, ids)
	if err != nil || len(votes) == 0 {
		return out
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	var root map[string]any
	if dec.Decode(&root) != nil {
		return out
	}
	if !markVotedTree(root, votes, 0) {
		return out
	}
	b, err := json.Marshal(root)
	if err != nil {
		return out
	}
	return b
}

// maxPollEmbedDepth bounds how deep withMyPollVotes follows renote / reply.
// 本家の withMyPollVotes は renote / reply を再帰的にたどる (#4164)。配られる
// ノートの埋め込みは publisher が数段に限るが、壊れた payload で深く潜らないよう
// 上限を置く。
const maxPollEmbedDepth = 8

// collectPollNoteIDs appends the ids of n and its renote / reply (recursively)
// that carry a poll.
func collectPollNoteIDs(n *streamNote, ids []string, depth int) []string {
	if n == nil || depth > maxPollEmbedDepth {
		return ids
	}
	if n.ID != "" && hasPoll(n.Poll) {
		ids = append(ids, n.ID)
	}
	ids = collectPollNoteIDs(n.Renote, ids, depth+1)
	return collectPollNoteIDs(n.Reply, ids, depth+1)
}

// markVotedTree applies markVoted to note and its renote / reply
// (recursively), and reports whether it changed anything.
func markVotedTree(note map[string]any, votes map[string][]int, depth int) bool {
	if depth > maxPollEmbedDepth {
		return false
	}
	changed := markVoted(note, votes)
	for _, key := range []string{"renote", "reply"} {
		if embed, ok := note[key].(map[string]any); ok && markVotedTree(embed, votes, depth+1) {
			changed = true
		}
	}
	return changed
}

// hasPoll reports whether a raw `poll` field holds a poll.
func hasPoll(raw json.RawMessage) bool {
	s := bytes.TrimSpace(raw)
	return len(s) > 0 && !bytes.Equal(s, []byte("null"))
}

// markVoted sets isVoted on the choices of note's poll that votes records for
// note's id, and reports whether it changed anything.
func markVoted(note map[string]any, votes map[string][]int) bool {
	id, _ := note["id"].(string)
	choices := votes[id]
	if len(choices) == 0 {
		return false
	}
	poll, ok := note["poll"].(map[string]any)
	if !ok {
		return false
	}
	list, ok := poll["choices"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, idx := range choices {
		if idx < 0 || idx >= len(list) {
			continue
		}
		if choice, ok := list[idx].(map[string]any); ok && choice["isVoted"] != true {
			choice["isVoted"] = true
			changed = true
		}
	}
	return changed
}

func (c *AntennaChannel) OnClientMessage(string, json.RawMessage) {}

// RequiredPermission implements stream.PermittedChannel.
func (c *AntennaChannel) RequiredPermission() string { return "read:account" }

func (c *AntennaChannel) Dispose() {
	if c.topic != "" {
		c.ctx.Unsubscribe(c.topic)
	}
}
